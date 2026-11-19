package engine

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/wjkoh/ghostbook/internal/renderer"
	"github.com/wjkoh/ghostbook/internal/store"
)

const (
	DefaultBriefingDebounce         = 10 * time.Minute
	DefaultBriefingSelfHealInterval = 15 * time.Minute
	maxBootstrapTimelineCommits     = 90
	maxPerCommitDiffRunes           = 360
	maxDisplayedOpenCallouts        = 6
	briefingAlertCooldown           = 1 * time.Hour
)

var inlineCalloutBlockquoteRe = regexp.MustCompile(`(?i)^>\s*(?:\*\*)?\[(TODO|Comment)\b([^\]]*)\](?:\*\*)?\s*:?\s*(.+)$`)

type revisionCommitMeta struct {
	RevisionNumber int32
	ShortSHA       string
	TimestampISO   string
	AuthorName     string
}

func countChapterInlineCallouts(md string) int32 {
	var count int32
	inFence := false
	for _, rawLine := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if isMarkdownCodeFence(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(trimmed, ">") {
			continue
		}
		upper := strings.ToUpper(trimmed)
		if strings.Contains(upper, "YYYY-MM-DD") || strings.Contains(upper, "CO-AUTHOR NAME") || strings.Contains(upper, "NAME · YYYY") {
			continue
		}
		if m := inlineCalloutBlockquoteRe.FindStringSubmatch(trimmed); len(m) == 4 && strings.TrimSpace(m[3]) != "" {
			count++
		}
	}
	return count
}

func extractOpenCallouts(chapters []*renderer.ChapterView) []renderer.OpenCalloutView {
	var out []renderer.OpenCalloutView
	seen := make(map[string]bool)

	for _, ch := range chapters {
		if ch == nil {
			continue
		}
		chapLabel := renderer.FormatBriefingChapterChip(ch.Title)
		inFence := false

		for _, rawLine := range strings.Split(ch.Markdown, "\n") {
			trimmed := strings.TrimSpace(rawLine)
			if isMarkdownCodeFence(trimmed) {
				inFence = !inFence
				continue
			}
			if inFence || !strings.HasPrefix(trimmed, ">") {
				continue
			}
			upper := strings.ToUpper(trimmed)
			if strings.Contains(upper, "YYYY-MM-DD") || strings.Contains(upper, "CO-AUTHOR NAME") || strings.Contains(upper, "NAME · YYYY") {
				continue
			}
			m := inlineCalloutBlockquoteRe.FindStringSubmatch(trimmed)
			if len(m) != 4 {
				continue
			}
			kind := "Comment"
			if strings.EqualFold(m[1], "TODO") {
				kind = "TODO"
			}
			meta := strings.TrimSpace(strings.Trim(m[2], "·:- "))
			body := strings.ReplaceAll(strings.TrimSpace(m[3]), "**", "")
			body = strings.ReplaceAll(body, `\$`, "$")
			body = strings.ReplaceAll(body, `\~`, "~")
			body = truncateUTF8(strings.Join(strings.Fields(body), " "), 160)
			if body == "" {
				continue
			}
			dedupKey := ch.Slug + "|" + kind + "|" + body
			if seen[dedupKey] {
				continue
			}
			seen[dedupKey] = true
			out = append(out, renderer.OpenCalloutView{
				Kind:        kind,
				AuthorLabel: truncateUTF8(meta, 72),
				ChapterSlug: ch.Slug,
				ChapterName: chapLabel,
				Excerpt:     body,
			})
			if len(out) >= maxDisplayedOpenCallouts {
				return out
			}
		}
	}
	return out
}

func toTimelineMilestoneViews(records []store.TimelineEntryRecord, bySlug map[string]*renderer.ChapterView) []renderer.TimelineMilestoneView {
	if len(records) == 0 {
		return nil
	}
	out := make([]renderer.TimelineMilestoneView, 0, len(records))
	for _, rec := range records {
		var links []renderer.BriefingChapterLink
		for _, slug := range rec.ChapterSlugs {
			if ch, ok := bySlug[slug]; ok && ch != nil {
				links = append(links, renderer.BriefingChapterLink{
					Slug:  slug,
					Label: renderer.FormatBriefingChapterChip(ch.Title),
				})
			}
		}
		out = append(out, renderer.TimelineMilestoneView{
			ID:                rec.ID,
			StartRevision:     rec.StartRevision,
			EndRevision:       rec.EndRevision,
			StartCommitSHA:    rec.StartCommitSHA,
			EndCommitSHA:      rec.EndCommitSHA,
			TimestampISO:      rec.TimestampISO,
			StartTimestampISO: rec.StartTimestampISO,
			CommitCount:       rec.CommitCount,
			Title:             rec.Title,
			Summary:           rec.Summary,
			Chapters:          links,
			Authors:           strings.Join(rec.Authors, ", "),
		})
	}
	return out
}

func computePendingTimelineWindow(headRev int32, state *store.TimelineStateRecord) (pendingCount int, startRev, endRev int32) {
	if state == nil || headRev <= state.LastHeadRevision {
		return 0, 0, 0
	}
	return int(headRev - state.LastHeadRevision), state.LastHeadRevision + 1, headRev
}

func extractSrcSlugFromDiffHeader(headerLine string) string {
	fields := strings.Fields(headerLine)
	for _, prefix := range []string{"b/src/", "a/src/", "src/"} {
		for _, token := range fields {
			if strings.HasPrefix(token, prefix) && strings.HasSuffix(token, ".md") {
				return strings.TrimSuffix(strings.TrimPrefix(token, prefix), ".md")
			}
		}
	}
	return ""
}

func splitUnifiedDiffBlocks(fullPatch string) []string {
	var blocks []string
	var cur strings.Builder
	inBlock := false
	for _, line := range strings.Split(fullPatch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if inBlock {
				blocks = append(blocks, cur.String())
				cur.Reset()
			}
			inBlock = true
		}
		if inBlock {
			if cur.Len() > 0 {
				cur.WriteByte('\n')
			}
			cur.WriteString(line)
		}
	}
	if inBlock && cur.Len() > 0 {
		blocks = append(blocks, cur.String())
	}
	return blocks
}

func summarizeCommitPatchForPrompt(fullPatch string, activeBySlug map[string]*renderer.ChapterView) ([]string, string) {
	var slugs []string
	seenSlug := make(map[string]bool)
	var diffLines []string

	for _, rawChunk := range splitUnifiedDiffBlocks(fullPatch) {
		if strings.TrimSpace(rawChunk) == "" {
			continue
		}
		lines := strings.Split(strings.TrimRight(rawChunk, "\n"), "\n")
		slug := extractSrcSlugFromDiffHeader(lines[0])
		if slug == "" || strings.EqualFold(slug, "STYLE_GUIDE") || strings.EqualFold(slug, "BOOK_CONFIG") {
			continue
		}
		if (strings.HasPrefix(slug, "draft-") || strings.HasPrefix(slug, "archive-")) && strings.Contains(rawChunk, "\n+++ /dev/null") {
			continue
		}
		if _, isActive := activeBySlug[slug]; isActive && !seenSlug[slug] {
			seenSlug[slug] = true
			slugs = append(slugs, slug)
		}
		if slug == "SUMMARY" {
			continue
		}
		diffLines = append(diffLines, "["+slug+"]")
		addedForFile := 0
		for _, ln := range lines[1:] {
			if strings.HasPrefix(ln, "--- ") || strings.HasPrefix(ln, "+++ ") || strings.HasPrefix(ln, "@@") {
				continue
			}
			if strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") {
				trimmed := strings.TrimSpace(ln)
				if len(trimmed) <= 1 {
					continue
				}
				diffLines = append(diffLines, truncateUTF8(ln, 140))
				addedForFile++
				if addedForFile >= 8 {
					break
				}
			}
		}
	}

	joined := strings.Join(diffLines, "\n")
	return slugs, truncateUTF8(joined, maxPerCommitDiffRunes)
}

func (e *BookEngine) collectUnsummarizedTimelineBatchLocked(
	state *store.TimelineStateRecord,
	priorEntries []store.TimelineEntryRecord,
) (*TimelineClusterInput, string, int32, map[int32]revisionCommitMeta, error) {
	chronological, revMap, err := e.orderedCommitsLocked()
	if err != nil {
		return nil, "", 0, nil, err
	}
	if len(chronological) < 2 {
		return nil, "", 0, nil, nil
	}

	headCommit := chronological[len(chronological)-1]
	headFull := headCommit.Hash.String()
	headSHA := shortSHA(headFull)
	headRev := revMap[headFull]

	if state != nil && (state.LastHeadSHA == headSHA || headRev <= state.LastHeadRevision) {
		return nil, headSHA, headRev, nil, nil
	}

	startIdx := 1
	endIdx := len(chronological)
	if state == nil {
		if len(chronological)-maxBootstrapTimelineCommits > startIdx {
			startIdx = len(chronological) - maxBootstrapTimelineCommits
		}
	} else {
		found := -1
		for i := 1; i < len(chronological); i++ {
			r := revMap[chronological[i].Hash.String()]
			if r > state.LastHeadRevision {
				found = i
				break
			}
		}
		if found < 1 {
			return nil, headSHA, headRev, nil, nil
		}
		startIdx = found
		if endIdx-startIdx > maxBootstrapTimelineCommits {
			endIdx = startIdx + maxBootstrapTimelineCommits
		}
	}

	batchEndCommit := chronological[endIdx-1]
	batchEndFull := batchEndCommit.Hash.String()
	batchEndSHA := shortSHA(batchEndFull)
	batchEndRev := revMap[batchEndFull]

	snap := e.CurrentSnapshot()
	var validChapters []BriefingChapterRef
	var activeBySlug map[string]*renderer.ChapterView
	if snap != nil {
		activeBySlug = snap.BySlug
		for _, ch := range snap.Chapters {
			validChapters = append(validChapters, BriefingChapterRef{
				Slug:  ch.Slug,
				Title: ch.Title,
			})
		}
	}

	var commits []TimelineBatchCommit
	revLookup := make(map[int32]revisionCommitMeta, endIdx-startIdx)

	for i := startIdx; i < endIdx; i++ {
		c := chronological[i]
		cFull := c.Hash.String()
		rNum := revMap[cFull]
		sSHA := shortSHA(cFull)
		tsISO := c.Author.When.UTC().Format(time.RFC3339)

		patchStr, _ := computeCommitTreePatch(c)
		touchedSlugs, compactDiff := summarizeCommitPatchForPrompt(patchStr, activeBySlug)
		msg := truncateUTF8(strings.Join(strings.Fields(c.Message), " "), 180)

		commits = append(commits, TimelineBatchCommit{
			RevisionNumber: rNum,
			ShortSHA:       sSHA,
			TimestampISO:   tsISO,
			AuthorName:     c.Author.Name,
			CommitMessage:  msg,
			ChapterSlugs:   touchedSlugs,
			CompactDiff:    compactDiff,
		})
		revLookup[rNum] = revisionCommitMeta{
			RevisionNumber: rNum,
			ShortSHA:       sSHA,
			TimestampISO:   tsISO,
			AuthorName:     c.Author.Name,
		}
	}
	if len(commits) == 0 {
		return nil, headSHA, headRev, nil, nil
	}

	var priorTitles []string
	for _, p := range priorEntries {
		priorTitles = append(priorTitles, fmt.Sprintf("r%d..r%d: %s", p.StartRevision, p.EndRevision, p.Title))
	}

	return &TimelineClusterInput{
		MinRevision:          commits[0].RevisionNumber,
		MaxRevision:          commits[len(commits)-1].RevisionNumber,
		ValidChapters:        validChapters,
		PriorPublishedTitles: priorTitles,
		EditorialGuidance:    e.TimelinePromptGuidance(),
		Commits:              commits,
	}, batchEndSHA, batchEndRev, revLookup, nil
}

func (e *BookEngine) SetBriefSynthesizer(s *BriefSynthesizer) {
	e.briefSynth.Store(s)
}

func (e *BookEngine) BriefSynthesizer() *BriefSynthesizer {
	return e.briefSynth.Load()
}

func (e *BookEngine) triggerBriefingRefresh() {
	if e.briefCh == nil {
		return
	}
	select {
	case e.briefCh <- struct{}{}:
	default:
	}
}

func (e *BookEngine) attachTimelineToCurrentSnapshot(records []store.TimelineEntryRecord, state *store.TimelineStateRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cur := e.snapshot.Load()
	if cur == nil {
		return
	}
	pendingCount, pStart, pEnd := computePendingTimelineWindow(cur.HeadRevisionNumber, state)
	updated := *cur
	updated.TimelineEntries = toTimelineMilestoneViews(records, cur.BySlug)
	updated.PendingTimelineCommits = pendingCount
	updated.PendingStartRevision = pStart
	updated.PendingEndRevision = pEnd
	e.snapshot.Store(&updated)
}

// SynthesizeBriefingIfNeeded checks whether HEAD has unsummarized commits above
// editorial_timeline_state.last_head_revision; if so, clusters strictly those new commits via
// Gemini, appends zero or more immutable milestones to SQLite, advances the watermark, and
// atomically updates the active in-memory snapshot.
func (e *BookEngine) SynthesizeBriefingIfNeeded(ctx context.Context) ([]store.TimelineEntryRecord, error) {
	synth := e.BriefSynthesizer()
	if synth == nil || !synth.Enabled() {
		return nil, nil
	}

	e.briefMu.Lock()
	defer e.briefMu.Unlock()

	// Snapshot the target HEAD revision once before looping so commits arriving mid-synthesis
	// wait cleanly for the next debounced session pass instead of bypassing the debounce window.
	var targetHeadRev int32
	if snap := e.CurrentSnapshot(); snap != nil {
		targetHeadRev = snap.HeadRevisionNumber
	}

	var latestEntries []store.TimelineEntryRecord
	for pass := 0; pass < 16; pass++ {
		start := time.Now()
		state, err := e.store.GetEditorialTimelineState(ctx)
		if err != nil {
			return latestEntries, err
		}
		if targetHeadRev > 0 && state != nil && state.LastHeadRevision >= targetHeadRev {
			allExisting, lErr := e.store.ListEditorialTimelineEntries(ctx, 0)
			if lErr != nil {
				return latestEntries, lErr
			}
			e.attachTimelineToCurrentSnapshot(allExisting, state)
			return allExisting, nil
		}
		priorTop, _ := e.store.ListEditorialTimelineEntries(ctx, 2)

		e.mu.RLock()
		batchIn, batchEndSHA, batchEndRev, revLookup, err := e.collectUnsummarizedTimelineBatchLocked(state, priorTop)
		e.mu.RUnlock()
		if err != nil {
			return latestEntries, err
		}
		if batchIn == nil {
			if state != nil {
				allExisting, lErr := e.store.ListEditorialTimelineEntries(ctx, 0)
				if lErr != nil {
					return latestEntries, lErr
				}
				e.attachTimelineToCurrentSnapshot(allExisting, state)
				return allExisting, nil
			}
			return latestEntries, nil
		}

		res, err := synth.SynthesizeTimeline(ctx, *batchIn)
		if err != nil {
			return latestEntries, err
		}

		newRecords := make([]store.TimelineEntryRecord, 0, len(res.Milestones))
		for _, m := range res.Milestones {
			startRev := m.Revisions[0]
			endRev := m.Revisions[len(m.Revisions)-1]
			startMeta := revLookup[startRev]
			endMeta := revLookup[endRev]

			newRecords = append(newRecords, store.TimelineEntryRecord{
				StartRevision:     startRev,
				EndRevision:       endRev,
				StartCommitSHA:    startMeta.ShortSHA,
				EndCommitSHA:      endMeta.ShortSHA,
				TimestampISO:      endMeta.TimestampISO,
				StartTimestampISO: startMeta.TimestampISO,
				CommitCount:       len(m.Revisions),
				Revisions:         m.Revisions,
				Title:             m.Title,
				Summary:           m.Summary,
				ChapterSlugs:      m.ChapterSlugs,
				Authors:           m.Authors,
			})
		}

		nextState := store.TimelineStateRecord{
			LastHeadSHA:      batchEndSHA,
			LastHeadRevision: batchEndRev,
			ModelName:        res.ModelName,
		}
		allEntries, err := e.store.AppendEditorialTimelineEntries(ctx, newRecords, nextState)
		if err != nil {
			return latestEntries, err
		}
		latestEntries = allEntries

		savedState, _ := e.store.GetEditorialTimelineState(ctx)
		if savedState == nil {
			savedState = &nextState
		}
		e.attachTimelineToCurrentSnapshot(allEntries, savedState)

		slog.Info("editorial_timeline_appended",
			"batch_range", fmt.Sprintf("r%d..r%d", batchIn.MinRevision, batchIn.MaxRevision),
			"commits_reviewed", len(batchIn.Commits),
			"milestones_appended", len(newRecords),
			"total_milestones", len(allEntries),
			"model", res.ModelName,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		if targetHeadRev > 0 && batchEndRev >= targetHeadRev {
			break
		}
	}
	return latestEntries, nil
}

func (e *BookEngine) runBriefingPassWithAlert(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	if _, err := e.SynthesizeBriefingIfNeeded(callCtx); err != nil && ctx.Err() == nil {
		slog.Warn("editorial_briefing_failed", "error", err.Error())
		if alerter := e.AlertNotifier(); alerter != nil {
			alerter.NotifyWithCooldown(ctx, "editorial_briefing", briefingAlertCooldown, "Front-page AI timeline synthesis failed", err.Error())
		}
	}
}

func (e *BookEngine) startupOrRemainingDebounceDelay(ctx context.Context, debounce time.Duration) (shouldSchedule bool, delay time.Duration) {
	state, err := e.store.GetEditorialTimelineState(ctx)
	if err != nil {
		return true, 0
	}
	// Empty timeline database on cold start: bootstrap immediately.
	if state == nil {
		return true, 0
	}

	e.mu.RLock()
	headCommit, hErr := e.headCommitLocked()
	var headRev int32
	if hErr == nil {
		_, revMap, oErr := e.orderedCommitsLocked()
		if oErr == nil {
			headRev = revMap[headCommit.Hash.String()]
		}
	}
	e.mu.RUnlock()

	if hErr != nil || headRev <= state.LastHeadRevision {
		return false, 0
	}
	age := time.Since(headCommit.Author.When.UTC())
	if age >= debounce {
		return true, 0
	}
	rem := debounce - age
	if rem < 0 {
		rem = 0
	}
	if rem > debounce {
		rem = debounce
	}
	return true, rem
}

// StartBriefingSynthesizer runs an immediate cold-start bootstrap when the timeline is empty,
// coalesces ongoing editing sessions via a 10-minute quiet-period debounce before appending
// immutable milestones, and runs a 15-minute idle local-SQLite self-healing check.
func (e *BookEngine) StartBriefingSynthesizer(ctx context.Context, debounce time.Duration) {
	e.StartBriefingSynthesizerWithRetry(ctx, debounce, DefaultBriefingSelfHealInterval)
}

func (e *BookEngine) StartBriefingSynthesizerWithRetry(ctx context.Context, debounce, selfHealInterval time.Duration) {
	if s := e.BriefSynthesizer(); s == nil || !s.Enabled() {
		return
	}
	if debounce <= 0 {
		debounce = DefaultBriefingDebounce
	}
	if selfHealInterval <= 0 {
		selfHealInterval = DefaultBriefingSelfHealInterval
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
	defer stopTimer()

	armTimer := func(d time.Duration) {
		stopTimer()
		if timer == nil {
			timer = time.NewTimer(d)
		} else {
			timer.Reset(d)
		}
		timerC = timer.C
	}

	if shouldRun, waitDur := e.startupOrRemainingDebounceDelay(ctx, debounce); shouldRun {
		if waitDur <= 0 {
			e.runBriefingPassWithAlert(ctx)
		} else {
			armTimer(waitDur)
		}
	}

	healTicker := time.NewTicker(selfHealInterval)
	defer healTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.briefCh:
			armTimer(debounce)
		case <-timerC:
			timerC = nil
			e.runBriefingPassWithAlert(ctx)
		case <-healTicker.C:
			if timerC == nil {
				e.runBriefingPassWithAlert(ctx)
			}
		}
	}
}
