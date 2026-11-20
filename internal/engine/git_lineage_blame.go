package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func computeCommitTreePatch(c *object.Commit) (string, error) {
	curTree, err := c.Tree()
	if err != nil {
		return "", err
	}
	if c.NumParents() == 0 {
		patch, err := (&object.Tree{}).Patch(curTree)
		if err != nil {
			return "", err
		}
		return patch.String(), nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return "", err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return "", err
	}
	patch, err := parentTree.Patch(curTree)
	if err != nil {
		return "", err
	}
	return patch.String(), nil
}

func computeBetweenCommitsPatch(baseCommit, targetCommit *object.Commit) (string, error) {
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return "", err
	}
	targetTree, err := targetCommit.Tree()
	if err != nil {
		return "", err
	}
	patch, err := baseTree.Patch(targetTree)
	if err != nil {
		return "", err
	}
	return patch.String(), nil
}

func (e *BookEngine) computeCommitDiffLocked(commitSHA, slug, baseCommitSHA string) (*GetCommitDiffOutput, error) {
	c, err := e.resolveCommitByPrefixLocked(commitSHA)
	if err != nil {
		return nil, err
	}
	var patchStr string
	if strings.TrimSpace(baseCommitSHA) != "" {
		baseCommit, baseErr := e.resolveCommitByPrefixLocked(baseCommitSHA)
		if baseErr != nil {
			return nil, baseErr
		}
		patchStr, err = computeBetweenCommitsPatch(baseCommit, c)
	} else {
		patchStr, err = computeCommitTreePatch(c)
	}
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(slug) != "" {
		cleanSlug, _, err := NormalizeChapterOrMetaSlug(slug)
		if err != nil {
			return nil, err
		}
		targetPath := srcRelPathForSlug(cleanSlug)
		patchStr = filterUnifiedDiffByPath(patchStr, targetPath)
	}

	return &GetCommitDiffOutput{
		CommitSHA:     shortSHA(c.Hash.String()),
		AuthorName:    c.Author.Name,
		TimestampISO:  c.Author.When.UTC().Format(time.RFC3339),
		CommitMessage: strings.TrimSpace(c.Message),
		UnifiedDiff:   patchStr,
	}, nil
}

func filterUnifiedDiffByPath(fullPatch, targetPath string) string {
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
	var kept []string
	for _, blk := range blocks {
		firstLine := strings.SplitN(blk, "\n", 2)[0]
		if strings.Contains(firstLine, "a/"+targetPath+" ") || strings.HasSuffix(firstLine, "b/"+targetPath) {
			kept = append(kept, blk)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n")
}

func readFileAtCommit(c *object.Commit, relPath string) (string, bool) {
	if c == nil {
		return "", false
	}
	f, err := c.File(relPath)
	if err != nil {
		return "", false
	}
	content, err := f.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

func isMarkdownCodeFence(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func extractMarkdownH1(md string) string {
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if isMarkdownCodeFence(trimmed) {
			inFence = !inFence
			continue
		}
		if !inFence && strings.HasPrefix(trimmed, "# ") {
			return cleanHeadingTitle(trimmed)
		}
	}
	return ""
}

func extractReplacedByHint(msg string) string {
	lower := strings.ToLower(msg)
	const marker = "[replaced-by:"
	idx := strings.Index(lower, marker)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(marker):]
	endIdx := strings.IndexByte(rest, ']')
	if endIdx < 0 {
		return ""
	}
	slug, err := ValidateSlug(strings.TrimSpace(rest[:endIdx]))
	if err != nil {
		return ""
	}
	return slug
}

// detectAddedChapterSlugInCommit checks the parent-to-commit tree diff to see if
// removedSlug was renamed or if a single new src/<slug>.md file was added in commit c.
func detectAddedChapterSlugInCommit(c *object.Commit, _ string) string {
	if c == nil || c.NumParents() == 0 {
		return ""
	}
	parent, err := c.Parent(0)
	if err != nil || parent == nil {
		return ""
	}
	parentTree, err := parent.Tree()
	if err != nil || parentTree == nil {
		return ""
	}
	curTree, err := c.Tree()
	if err != nil || curTree == nil {
		return ""
	}
	changes, err := parentTree.Diff(curTree)
	if err != nil {
		return ""
	}
	var added []string
	for _, ch := range changes {
		if strings.HasPrefix(ch.To.Name, "src/") && strings.HasSuffix(ch.To.Name, ".md") {
			if _, pErr := parent.File(ch.To.Name); pErr != nil {
				base := strings.TrimSuffix(strings.TrimPrefix(ch.To.Name, "src/"), ".md")
				if base != "SUMMARY" && base != "STYLE_GUIDE" {
					if clean, vErr := ValidateSlug(base); vErr == nil {
						added = append(added, clean)
					}
				}
			}
		}
	}
	if len(added) == 1 {
		return added[0]
	}
	return ""
}

// followSlugLineageToHead walks chronological commits from startIdx forward to find
// what happened when relPath ("src/<slug>.md") was removed or renamed, prioritizing
// explicit [replaced-by: slug] commit hints over tree-diff heuristics and following
// multi-hop renames/replacements all the way to HEAD.
func followSlugLineageToHead(chronological []*object.Commit, startIdx int, initialSlug string) (
	status string,
	finalSlug string,
	finalTitle string,
	eventCommit *object.Commit,
) {
	if len(chronological) == 0 {
		return "", "", "", nil
	}
	curSlug := initialSlug
	for i := startIdx + 1; i < len(chronological); i++ {
		c := chronological[i]
		rel := "src/" + curSlug + ".md"
		if _, exists := readFileAtCommit(c, rel); exists {
			continue
		}
		if hint := extractReplacedByHint(c.Message); hint != "" {
			status = "deleted"
			curSlug = hint
			finalSlug = hint
			eventCommit = c
			continue
		}
		if addedSlug := detectAddedChapterSlugInCommit(c, curSlug); addedSlug != "" {
			if status != "deleted" {
				status = "moved"
			}
			curSlug = addedSlug
			eventCommit = c
			continue
		}
		status = "deleted"
		finalSlug = ""
		eventCommit = c
		break
	}
	head := chronological[len(chronological)-1]
	if status == "moved" || (status == "deleted" && finalSlug != "") {
		if _, existsAtHead := readFileAtCommit(head, "src/"+curSlug+".md"); existsAtHead {
			finalSlug = curSlug
		} else {
			status = "deleted"
			finalSlug = ""
		}
	}
	if finalSlug != "" {
		if md, ok := readFileAtCommit(head, "src/"+finalSlug+".md"); ok {
			finalTitle = extractMarkdownH1(md)
		}
	}
	return status, finalSlug, finalTitle, eventCommit
}

func (e *BookEngine) resolveTargetCommitForPermalinkLocked(
	chronological []*object.Commit,
	relPath, revSpec string,
) (*object.Commit, int, string, error) {
	if len(chronological) == 0 {
		return nil, -1, "", fmt.Errorf("%w: repository has no commits", ErrNotFound)
	}
	headCommit := chronological[len(chronological)-1]
	targetCommit := headCommit
	targetIdx := len(chronological) - 1
	foundInChain := true

	if strings.TrimSpace(revSpec) != "" {
		resolved, err := e.resolveCommitByPrefixLocked(revSpec)
		if err != nil {
			return nil, -1, "", err
		}
		targetCommit = resolved
		foundInChain = false
		for i, c := range chronological {
			if c.Hash == targetCommit.Hash {
				targetIdx = i
				foundInChain = true
				break
			}
		}
	}

	if md, ok := readFileAtCommit(targetCommit, relPath); ok {
		if !foundInChain {
			return targetCommit, -1, md, nil
		}
		return targetCommit, targetIdx, md, nil
	}
	if foundInChain {
		for i := targetIdx; i >= 0; i-- {
			if md, ok := readFileAtCommit(chronological[i], relPath); ok {
				return chronological[i], i, md, nil
			}
		}
	}
	return nil, -1, "", fmt.Errorf("%w: %s", ErrNotFound, strings.TrimSuffix(strings.TrimPrefix(relPath, "src/"), ".md"))
}

func populateHeadExistingLineage(
	out *ReadChapterOutput,
	chronological []*object.Commit,
	revMap map[string]int32,
	targetIdx int,
	chapCommitAtTarget *object.Commit,
	relPath, contentAtTarget, titleAtTarget, contentAtHead string,
) {
	var commitsBehind int32
	latestChapCommit := chapCommitAtTarget
	var titleChangeCommit *object.Commit
	prevTitle := titleAtTarget

	for i := targetIdx + 1; i < len(chronological); i++ {
		c := chronological[i]
		files, _ := commitModifiedFiles(c)
		if containsString(files, relPath) {
			commitsBehind++
			latestChapCommit = c
			if md, ok := readFileAtCommit(c, relPath); ok {
				t := extractMarkdownH1(md)
				if t != "" && t != prevTitle && titleChangeCommit == nil {
					titleChangeCommit = c
					prevTitle = t
				}
			}
		}
	}

	out.LatestRevisionSHA = shortSHA(latestChapCommit.Hash.String())
	out.LatestRevisionNumber = revMap[latestChapCommit.Hash.String()]
	headTitle := extractMarkdownH1(contentAtHead)

	if contentAtTarget == contentAtHead && commitsBehind == 0 {
		out.LifecycleStatus = "current"
		out.IsLatest = true
		out.CommitsBehind = 0
		return
	}

	out.IsLatest = false
	if commitsBehind == 0 {
		commitsBehind = 1
	}
	out.CommitsBehind = commitsBehind
	if headTitle != "" && headTitle != titleAtTarget {
		out.LifecycleStatus = "renamed"
		out.MovedToSlug = out.Slug
		out.MovedToTitle = headTitle
		ev := latestChapCommit
		if titleChangeCommit != nil {
			ev = titleChangeCommit
		}
		out.LifecycleCommitSHA = shortSHA(ev.Hash.String())
		out.LifecycleAuthor = ev.Author.Name
		out.LifecycleTimestampISO = ev.Author.When.UTC().Format(time.RFC3339)
		out.LifecycleMessage = strings.TrimSpace(ev.Message)
		return
	}

	out.LifecycleStatus = "outdated"
	out.LifecycleCommitSHA = shortSHA(latestChapCommit.Hash.String())
	out.LifecycleAuthor = latestChapCommit.Author.Name
	out.LifecycleTimestampISO = latestChapCommit.Author.When.UTC().Format(time.RFC3339)
	out.LifecycleMessage = strings.TrimSpace(latestChapCommit.Message)
}

func (e *BookEngine) resolveChapterPermalinkLocked(slug, revSpec string) (*ReadChapterOutput, error) {
	cleanSlug, _, err := NormalizeChapterOrMetaSlug(slug)
	if err != nil {
		return nil, err
	}
	chronological, revMap, err := e.orderedCommitsLocked()
	if err != nil || len(chronological) == 0 {
		return nil, fmt.Errorf("git commit history unavailable: %w", err)
	}
	headCommit := chronological[len(chronological)-1]
	headRevNum := revMap[headCommit.Hash.String()]
	relPath := srcRelPathForSlug(cleanSlug)

	targetCommit, targetIdx, contentAtTarget, err := e.resolveTargetCommitForPermalinkLocked(chronological, relPath, revSpec)
	if err != nil {
		return nil, err
	}

	chapCommitAtTarget := targetCommit
	for i := targetIdx; i >= 0; i-- {
		files, _ := commitModifiedFiles(chronological[i])
		if containsString(files, relPath) {
			chapCommitAtTarget = chronological[i]
			break
		}
	}

	targetSHA := shortSHA(targetCommit.Hash.String())
	targetRevNum := revMap[targetCommit.Hash.String()]
	if strings.TrimSpace(revSpec) == "" {
		targetSHA = shortSHA(chapCommitAtTarget.Hash.String())
		targetRevNum = revMap[chapCommitAtTarget.Hash.String()]
	}
	titleAtTarget := extractMarkdownH1(contentAtTarget)
	if cleanSlug == "BOOK_CONFIG" {
		titleAtTarget = "Book Configuration"
	} else if titleAtTarget == "" {
		titleAtTarget = cleanSlug
	}

	liveURL := fmt.Sprintf("%s/chapters/%s", e.baseURL, cleanSlug)
	permalinkURL := fmt.Sprintf("%s/chapters/%s/rev/%s", e.baseURL, cleanSlug, targetSHA)
	switch cleanSlug {
	case "SUMMARY", "STYLE_GUIDE":
		liveURL = fmt.Sprintf("%s/%s.md", e.baseURL, cleanSlug)
		permalinkURL = fmt.Sprintf("%s/rev/%s/%s.md", e.baseURL, targetSHA, cleanSlug)
	case "BOOK_CONFIG":
		liveURL = fmt.Sprintf("%s/book.json", e.baseURL)
		permalinkURL = fmt.Sprintf("%s/rev/%s/book.json", e.baseURL, targetSHA)
	}
	out := &ReadChapterOutput{
		Slug:                 cleanSlug,
		Title:                titleAtTarget,
		ContentMarkdown:      contentAtTarget,
		RevisionSHA:          targetSHA,
		RevisionNumber:       targetRevNum,
		LastAuthor:           chapCommitAtTarget.Author.Name,
		LastUpdatedISO:       chapCommitAtTarget.Author.When.UTC().Format(time.RFC3339),
		LiveURL:              liveURL,
		PermalinkURL:         permalinkURL,
		LatestRevisionSHA:    shortSHA(headCommit.Hash.String()),
		LatestRevisionNumber: headRevNum,
		LatestLiveURL:        liveURL,
	}

	if contentAtHead, existsAtHead := readFileAtCommit(headCommit, relPath); existsAtHead {
		populateHeadExistingLineage(out, chronological, revMap, targetIdx, chapCommitAtTarget, relPath, contentAtTarget, titleAtTarget, contentAtHead)
		return out, nil
	}

	status, finalSlug, finalTitle, eventCommit := followSlugLineageToHead(chronological, targetIdx, cleanSlug)
	out.IsLatest = false
	out.LifecycleStatus = status
	out.MovedToSlug = finalSlug
	out.MovedToTitle = finalTitle
	if finalSlug != "" {
		out.LatestLiveURL = fmt.Sprintf("%s/chapters/%s", e.baseURL, finalSlug)
	}
	if eventCommit != nil {
		out.LifecycleCommitSHA = shortSHA(eventCommit.Hash.String())
		out.LifecycleAuthor = eventCommit.Author.Name
		out.LifecycleTimestampISO = eventCommit.Author.When.UTC().Format(time.RFC3339)
		out.LifecycleMessage = strings.TrimSpace(eventCommit.Message)
		out.CommitsBehind = int32(len(chronological) - 1 - targetIdx)
	}
	return out, nil
}

func isMarkdownHeadingLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "# ") ||
		strings.HasPrefix(trimmed, "## ") ||
		strings.HasPrefix(trimmed, "### ")
}

func cleanHeadingTitle(trimmed string) string {
	h := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
	h = strings.ReplaceAll(h, `\$`, `$`)
	h = strings.ReplaceAll(h, `\~`, `~`)
	return h
}

func extractBlameAuthor(line *git.Line) string {
	if line.AuthorName != "" {
		return line.AuthorName
	}
	return line.Author
}

func (e *BookEngine) computeChapterBlameLocked(slug string) (*GetChapterBlameOutput, error) {
	cleanSlug, _, err := NormalizeChapterOrMetaSlug(slug)
	if err != nil {
		return nil, err
	}
	relPath := srcRelPathForSlug(cleanSlug)
	head, err := e.headCommitLocked()
	if err != nil {
		return nil, err
	}

	blameRes, err := git.Blame(head, relPath)
	if err != nil {
		return nil, fmt.Errorf("%w: blame %s: %v", ErrNotFound, cleanSlug, err)
	}
	_, revMap, _ := e.orderedCommitsLocked()

	var sections []BlameSectionItem
	curHeading := "Chapter Introduction"
	curStart := 1
	var latestSHA, latestAuthor string
	var latestRev int32
	var latestWhen time.Time
	inCodeFence := false

	flushSection := func(endLine int) {
		if endLine < curStart || latestSHA == "" {
			return
		}
		sections = append(sections, BlameSectionItem{
			Heading:        curHeading,
			LastCommitSHA:  shortSHA(latestSHA),
			LastAuthor:     latestAuthor,
			LastUpdatedISO: latestWhen.UTC().Format(time.RFC3339),
			StartLine:      int32(curStart),
			EndLine:        int32(endLine),
		})
	}

	for idx, line := range blameRes.Lines {
		lineNum := idx + 1
		lineHash := line.Hash.String()
		lineRev := revMap[lineHash]
		trimmed := strings.TrimSpace(line.Text)
		if isMarkdownCodeFence(trimmed) {
			inCodeFence = !inCodeFence
		}
		if !inCodeFence && isMarkdownHeadingLine(trimmed) {
			if lineNum > 1 {
				flushSection(lineNum - 1)
			}
			curHeading = cleanHeadingTitle(trimmed)
			curStart = lineNum
			latestSHA = lineHash
			latestAuthor = extractBlameAuthor(line)
			latestRev = lineRev
			latestWhen = line.Date
			continue
		}
		if latestSHA == "" || lineRev > latestRev || (lineRev == latestRev && line.Date.After(latestWhen)) {
			latestSHA = lineHash
			latestAuthor = extractBlameAuthor(line)
			latestRev = lineRev
			latestWhen = line.Date
		}
	}
	flushSection(len(blameRes.Lines))

	return &GetChapterBlameOutput{
		Slug:     cleanSlug,
		Sections: sections,
	}, nil
}
