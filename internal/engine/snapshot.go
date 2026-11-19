package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/wjkoh/ghostbook/internal/renderer"
	"github.com/wjkoh/ghostbook/internal/store"
)

type commitHistoryIndex struct {
	headCommit    *object.Commit
	headRevNum    int32
	revMap        map[string]int32
	latestByPath  map[string]*object.Commit
	recentCommits []CommitEntryItem
}

// buildCommitHistoryIndexLocked walks the first-parent commit chain once in O(N) time,
// recording the latest commit for every modified path and the most recent commits list.
func (e *BookEngine) buildCommitHistoryIndexLocked(recentLimit int) (*commitHistoryIndex, error) {
	chronological, revMap, err := e.orderedCommitsLocked()
	if err != nil {
		return nil, err
	}
	if len(chronological) == 0 {
		return nil, fmt.Errorf("git repository has no commits")
	}

	head := chronological[len(chronological)-1]
	headRev := revMap[head.Hash.String()]

	latestByPath := make(map[string]*object.Commit, 32)
	capHint := recentLimit
	if capHint <= 0 || capHint > len(chronological) {
		capHint = len(chronological)
	}
	recent := make([]CommitEntryItem, 0, capHint)

	for i := len(chronological) - 1; i >= 0; i-- {
		c := chronological[i]
		files, cached := e.commitFilesCache[c.Hash]
		if !cached {
			files, _ = commitModifiedFiles(c)
			if e.commitFilesCache != nil {
				e.commitFilesCache[c.Hash] = files
			}
		}
		for _, p := range files {
			if _, seen := latestByPath[p]; !seen {
				latestByPath[p] = c
			}
		}
		if recentLimit <= 0 || len(recent) < recentLimit {
			fullSHA := c.Hash.String()
			recent = append(recent, CommitEntryItem{
				CommitSHA:      shortSHA(fullSHA),
				FullSHA:        fullSHA,
				RevisionNumber: revMap[fullSHA],
				AuthorName:     c.Author.Name,
				AuthorEmail:    c.Author.Email,
				TimestampISO:   c.Author.When.UTC().Format(time.RFC3339),
				CommitMessage:  strings.TrimSpace(c.Message),
				FilesChanged:   files,
			})
		}
	}

	return &commitHistoryIndex{
		headCommit:    head,
		headRevNum:    headRev,
		revMap:        revMap,
		latestByPath:  latestByPath,
		recentCommits: recent,
	}, nil
}

func (e *BookEngine) buildChapterViewLocked(
	item renderer.SummaryEntry,
	histIdx *commitHistoryIndex,
) (*renderer.ChapterView, store.ChapterFTSDoc, error) {
	slug := item.Slug
	relPath := filepath.ToSlash(filepath.Join("src", slug+".md"))
	rawBytes, err := e.readSrcFileSafe(slug + ".md")
	if err != nil {
		return nil, store.ChapterFTSDoc{}, err
	}
	mdText := string(rawBytes)
	renderedHTML, title, headings, wordCount, err := renderer.RenderMarkdownToHTML(mdText)
	if err != nil {
		return nil, store.ChapterFTSDoc{}, err
	}
	if title == "" {
		if item.SummaryTitle != "" {
			title = item.SummaryTitle
		} else {
			title = slug
		}
	}

	lastCommit := histIdx.latestByPath[relPath]
	if lastCommit == nil {
		lastCommit = histIdx.headCommit
	}
	lastSHA := shortSHA(lastCommit.Hash.String())
	revNum := histIdx.revMap[lastCommit.Hash.String()]
	if revNum <= 0 {
		revNum = 1
	}

	liveURL := fmt.Sprintf("%s/chapters/%s", e.baseURL, slug)
	permalinkURL := fmt.Sprintf("%s/chapters/%s/rev/%s", e.baseURL, slug, lastSHA)
	calloutCount := countChapterInlineCallouts(mdText)

	ftsDoc := store.ChapterFTSDoc{
		Slug:         slug,
		Title:        title,
		Headings:     strings.Join(headings, " "),
		BodyMarkdown: mdText,
	}

	cv := &renderer.ChapterView{
		Slug:                 slug,
		NumberLabel:          item.NumberLabel,
		PartTitle:            item.PartTitle,
		StartsNewPart:        item.StartsNewPart,
		Depth:                item.Depth,
		ParentSlug:           item.ParentSlug,
		Title:                title,
		Markdown:             mdText,
		RenderedHTML:         renderedHTML,
		WordCount:            wordCount,
		OpenCommentCount:     calloutCount,
		LastCommitSHA:        lastSHA,
		RevisionNumber:       revNum,
		LastAuthor:           lastCommit.Author.Name,
		LastUpdatedISO:       lastCommit.Author.When.UTC().Format(time.RFC3339),
		PermalinkURL:         permalinkURL,
		LifecycleStatus:      "current",
		IsLatest:             true,
		LatestRevisionSHA:    lastSHA,
		LatestRevisionNumber: revNum,
		LatestLiveURL:        liveURL,
	}
	return cv, ftsDoc, nil
}

func linkChapterPagination(chapters []*renderer.ChapterView) {
	for i, ch := range chapters {
		if i > 0 {
			ch.PrevSlug = chapters[i-1].Slug
			ch.PrevTitle = chapters[i-1].Title
		}
		if i+1 < len(chapters) {
			ch.NextSlug = chapters[i+1].Slug
			ch.NextTitle = chapters[i+1].Title
		}
	}
}

func toCommitSummaryViews(commits []CommitEntryItem) []renderer.CommitSummaryView {
	out := make([]renderer.CommitSummaryView, 0, len(commits))
	for _, c := range commits {
		out = append(out, renderer.CommitSummaryView{
			ShortSHA:       c.CommitSHA,
			FullSHA:        c.FullSHA,
			RevisionNumber: c.RevisionNumber,
			AuthorName:     c.AuthorName,
			AuthorEmail:    c.AuthorEmail,
			TimestampISO:   c.TimestampISO,
			CommitMessage:  c.CommitMessage,
			FilesChanged:   c.FilesChanged,
		})
	}
	return out
}

func (e *BookEngine) rebuildSnapshotAndFTSLocked(ctx context.Context) error {
	start := time.Now()
	summaryEntries, summaryText, err := e.parseOrderedChapterEntriesLocked()
	if err != nil {
		return err
	}

	histIdx, err := e.buildCommitHistoryIndexLocked(0)
	if err != nil {
		return err
	}

	chapters := make([]*renderer.ChapterView, 0, len(summaryEntries))
	bySlug := make(map[string]*renderer.ChapterView, len(summaryEntries))
	ftsDocs := make([]store.ChapterFTSDoc, 0, len(summaryEntries))
	var totalWords int32

	for _, item := range summaryEntries {
		cv, ftsDoc, err := e.buildChapterViewLocked(item, histIdx)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		chapters = append(chapters, cv)
		bySlug[item.Slug] = cv
		ftsDocs = append(ftsDocs, ftsDoc)
		totalWords += cv.WordCount
	}

	if err := e.store.ReplaceAllChaptersFTS(ctx, ftsDocs); err != nil {
		return fmt.Errorf("replace chapters fts: %w", err)
	}
	linkChapterPagination(chapters)

	recentAct, actErr := e.store.ListActivity(ctx, "", 30)
	if actErr != nil {
		slog.Warn("snapshot_list_activity_failed", "error", actErr)
	}
	timelineRecords, tlErr := e.store.ListEditorialTimelineEntries(ctx, 0)
	if tlErr != nil {
		slog.Warn("snapshot_list_timeline_failed", "error", tlErr)
	}
	timelineState, stErr := e.store.GetEditorialTimelineState(ctx)
	if stErr != nil {
		slog.Warn("snapshot_get_timeline_state_failed", "error", stErr)
	}
	pendingCount, pStart, pEnd := computePendingTimelineWindow(histIdx.headRevNum, timelineState)
	openCallouts := extractOpenCallouts(chapters)
	headSHA := shortSHA(histIdx.headCommit.Hash.String())

	var authorViews []renderer.AuthorProfileView
	if dbAuthors, aErr := e.store.ListAuthors(ctx); aErr == nil {
		for _, a := range dbAuthors {
			authorViews = append(authorViews, renderer.AuthorProfileView{
				ID:          a.ID,
				DisplayName: a.DisplayName,
				Email:       a.GitEmail,
				RoleBadge:   a.RoleBadge,
			})
		}
	} else {
		slog.Warn("snapshot_list_authors_failed", "error", aErr)
	}

	e.snapshot.Store(&renderer.RenderedBook{
		BookTitle:              e.BookTitle(),
		BookKicker:             e.BookKicker(),
		MCPName:                e.MCPName(),
		HidePoweredBy:          e.cfg.HidePoweredBy,
		Authors:                authorViews,
		HeadCommitSHA:          headSHA,
		HeadRevisionNumber:     histIdx.headRevNum,
		SummaryMarkdown:        summaryText,
		Chapters:               chapters,
		BySlug:                 bySlug,
		RecentCommits:          toCommitSummaryViews(histIdx.recentCommits),
		RecentActivity:         recentAct,
		TimelineEntries:        toTimelineMilestoneViews(timelineRecords, bySlug),
		PendingTimelineCommits: pendingCount,
		PendingStartRevision:   pStart,
		PendingEndRevision:     pEnd,
		OpenCallouts:           openCallouts,
	})
	slog.Info("snapshot_rebuilt",
		"head_sha", headSHA,
		"revision", fmt.Sprintf("r%d", histIdx.headRevNum),
		"chapters", len(chapters),
		"total_words", totalWords,
		"open_callouts", len(openCallouts),
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}
