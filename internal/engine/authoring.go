package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

var headingPrefixLintRe = regexp.MustCompile(`(?i)^(?:(?:chapter|section|appendix)\s+[a-z0-9]+(?:\.[a-z0-9]+)*\s*[:.\-—]\s*|(?:introduction|preface|foreword|prologue|epilogue)\s*:\s*|\d+(?:\.\d+)*\s*[:.\-—]\s*)`)

// LintChapterMarkdown performs non-blocking editorial checks against STYLE_GUIDE.md:
// - Rule 1: Headings (H1/H2/H3) should be concise (<= 42 characters after prefix).
// - Rule 5: Suppress em dashes (—) in narrative prose.
// - Optional Rule 6: Flag any configured banned words.
func LintChapterMarkdown(slug, md string, bannedWordsOpt ...[]string) []string {
	if strings.EqualFold(slug, "STYLE_GUIDE") || strings.EqualFold(slug, "SUMMARY") {
		return nil
	}
	var bannedWords []string
	if len(bannedWordsOpt) > 0 {
		for _, w := range bannedWordsOpt[0] {
			if clean := strings.ToLower(strings.TrimSpace(w)); clean != "" {
				bannedWords = append(bannedWords, clean)
			}
		}
	}
	var warnings []string
	inFence := false
	emDashReported := false
	bannedReported := make(map[string]bool)

	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if isMarkdownCodeFence(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence || trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			rawTitle := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			coreTitle := strings.TrimSpace(headingPrefixLintRe.ReplaceAllString(rawTitle, ""))
			if utf8.RuneCountInString(coreTitle) > 42 {
				warnings = append(warnings, fmt.Sprintf("Rule 1 (Concise Headings <= 42 chars): heading %q is %d chars after prefix", rawTitle, utf8.RuneCountInString(coreTitle)))
			}
		}
		for _, bw := range bannedWords {
			if !bannedReported[bw] && strings.Contains(strings.ToLower(trimmed), bw) {
				bannedReported[bw] = true
				warnings = append(warnings, fmt.Sprintf("Rule 6 (Terminology): avoid the configured banned term %q", bw))
			}
		}
		if !emDashReported && !strings.HasPrefix(trimmed, "|") && strings.Contains(trimmed, "—") {
			emDashReported = true
			warnings = append(warnings, "Rule 5 (Punctuation Discipline): avoid em dashes (—) in narrative prose")
		}
	}
	return warnings
}

func buildInlineDiffPreview(slug string, hunks []ChapterPatchHunk) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--- a/src/%s.md\n+++ b/src/%s.md\n", slug, slug)
	for _, h := range hunks {
		fmt.Fprintf(&buf, "@@ surgical patch @@\n")
		for _, line := range strings.Split(h.OldText, "\n") {
			fmt.Fprintf(&buf, "-%s\n", line)
		}
		for _, line := range strings.Split(h.NewText, "\n") {
			fmt.Fprintf(&buf, "+%s\n", line)
		}
	}
	return buf.String()
}

func (e *BookEngine) verifyOptimisticRevisionLocked(relPath, expectedRaw, currentContent string) (string, error) {
	lastCommit, err := e.latestCommitForFileLocked(relPath)
	if err != nil {
		return "", err
	}
	headCommit, err := e.headCommitLocked()
	if err != nil {
		return "", err
	}
	currentFileSHA := lastCommit.Hash.String()
	currentHeadSHA := headCommit.Hash.String()

	expected := strings.TrimSpace(strings.ToLower(expectedRaw))
	if expected == "" {
		return currentFileSHA, nil
	}
	if _, isRevNum := parseRevisionNumber(expected); isRevNum && strings.HasPrefix(expected, "r") {
		if resolved, rErr := e.resolveCommitByPrefixLocked(expected); rErr == nil {
			resolvedSHA := resolved.Hash.String()
			if resolvedSHA == currentFileSHA || resolvedSHA == currentHeadSHA {
				return currentFileSHA, nil
			}
		}
	}
	if len(expected) >= 4 && (strings.HasPrefix(strings.ToLower(currentFileSHA), expected) ||
		strings.HasPrefix(strings.ToLower(currentHeadSHA), expected)) {
		return currentFileSHA, nil
	}
	if _, isRevNum := parseRevisionNumber(expected); isRevNum {
		if resolved, rErr := e.resolveCommitByPrefixLocked(expected); rErr == nil {
			resolvedSHA := resolved.Hash.String()
			if resolvedSHA == currentFileSHA || resolvedSHA == currentHeadSHA {
				return currentFileSHA, nil
			}
		}
	}
	return "", fmt.Errorf("%w: expected %s but chapter is at revision %s (latest text: %s)",
		ErrRevisionConflict, expected, shortSHA(currentFileSHA), truncateUTF8(currentContent, 400))
}

func applyChapterPatchText(slug, currentContent, oldText, newText, currentFileSHA string) (string, string, error) {
	if oldText == "*" {
		if strings.TrimSpace(newText) == "" {
			return "", "", fmt.Errorf("%w: new_text must not be empty when replacing full chapter", ErrInvalidArgument)
		}
		if !strings.HasSuffix(newText, "\n") {
			newText += "\n"
		}
		return currentContent, newText, nil
	}
	occurrences := strings.Count(currentContent, oldText)
	if occurrences == 0 {
		return "", "", fmt.Errorf("%w (current revision %s; excerpt: %s)",
			ErrPatchNotFound, shortSHA(currentFileSHA), truncateUTF8(currentContent, 400))
	}
	if occurrences > 1 {
		return "", "", fmt.Errorf("%w: matched %d occurrences in %s", ErrPatchAmbiguous, occurrences, slug)
	}
	return oldText, strings.Replace(currentContent, oldText, newText, 1), nil
}

func collectPatchHunks(in *PatchChapterInput) ([]ChapterPatchHunk, error) {
	var hunks []ChapterPatchHunk
	if strings.TrimSpace(in.OldText) != "" {
		hunks = append(hunks, ChapterPatchHunk{
			OldText: in.OldText,
			NewText: in.NewText,
		})
	}
	for _, h := range in.Patches {
		if strings.TrimSpace(h.OldText) == "" {
			return nil, fmt.Errorf("%w: old_text in patches must not be empty", ErrInvalidArgument)
		}
		hunks = append(hunks, h)
	}
	if len(hunks) == 0 {
		return nil, fmt.Errorf("%w: old_text (or patches array) must not be empty", ErrInvalidArgument)
	}
	return hunks, nil
}

func (e *BookEngine) PatchChapter(ctx context.Context, in *PatchChapterInput) (*PatchChapterOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	slug, isMeta, err := NormalizeChapterOrMetaSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	hunks, err := collectPatchHunks(in)
	if err != nil {
		return nil, err
	}
	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("Update %s", slug)
	}
	author := e.resolveEffectiveAuthor(ctx, in.AuthorName)

	e.mu.Lock()
	defer e.mu.Unlock()

	relPath := filepath.ToSlash(filepath.Join("src", slug+".md"))
	fullPath := filepath.Join(e.repoDir, "src", slug+".md")
	rawBytes, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, slug)
		}
		return nil, err
	}
	currentContent := string(rawBytes)

	currentFileSHA, err := e.verifyOptimisticRevisionLocked(relPath, in.ExpectedRevisionSHA, currentContent)
	if err != nil {
		return nil, err
	}

	updatedContent := currentContent
	appliedHunks := make([]ChapterPatchHunk, 0, len(hunks))
	for _, h := range hunks {
		diffOld, nextContent, pErr := applyChapterPatchText(slug, updatedContent, h.OldText, h.NewText, currentFileSHA)
		if pErr != nil {
			return nil, pErr
		}
		updatedContent = nextContent
		appliedHunks = append(appliedHunks, ChapterPatchHunk{OldText: diffOld, NewText: h.NewText})
	}
	if updatedContent == currentContent {
		return nil, fmt.Errorf("%w: patch produced no changes in %s", ErrInvalidArgument, slug)
	}
	if err := os.WriteFile(fullPath, []byte(updatedContent), 0o644); err != nil {
		return nil, err
	}

	commitPaths := []string{relPath}
	if !isMeta {
		oldH1 := extractMarkdownH1(currentContent)
		newH1 := extractMarkdownH1(updatedContent)
		if newH1 != "" && newH1 != oldH1 {
			summaryFull := filepath.Join(e.repoDir, "src", "SUMMARY.md")
			if rawSummary, sumErr := os.ReadFile(summaryFull); sumErr == nil {
				updatedSummary := rewriteSummaryForRename(string(rawSummary), slug, slug, newH1)
				if updatedSummary != string(rawSummary) {
					if err := os.WriteFile(summaryFull, []byte(updatedSummary), 0o644); err != nil {
						e.resetWorktreeLocked()
						return nil, err
					}
					commitPaths = append(commitPaths, "src/SUMMARY.md")
				}
			}
		}
	}

	newFullSHA, err := e.commitPathsLocked(author, commitMsg, commitPaths...)
	if err != nil {
		e.resetWorktreeLocked()
		return nil, err
	}
	if err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	e.triggerSync()
	e.triggerBriefingRefresh()

	shortNewSHA := shortSHA(newFullSHA)
	slog.Info("chapter_patched",
		"slug", slug,
		"author", author.ID,
		"prev_sha", shortSHA(currentFileSHA),
		"new_sha", shortNewSHA,
		"commit_msg", commitMsg,
	)
	return &PatchChapterOutput{
		Slug:           slug,
		NewRevisionSHA: shortNewSHA,
		CommittedBy:    fmt.Sprintf("%s <%s>", author.DisplayName, author.GitEmail),
		DiffPreview:    buildInlineDiffPreview(slug, appliedHunks),
		LiveChapterURL: fmt.Sprintf("%s/chapters/%s", e.baseURL, slug),
		StyleWarnings:  LintChapterMarkdown(slug, updatedContent, e.BannedWords()),
	}, nil
}

func (e *BookEngine) CreateChapter(ctx context.Context, in *CreateChapterInput) (*CreateChapterOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	slug, err := ValidateSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidArgument)
	}
	body := formatInitialChapterBody(title, in.InitialMarkdown)
	author := e.resolveEffectiveAuthor(ctx, in.AuthorName)

	e.mu.Lock()
	defer e.mu.Unlock()

	relChapter := filepath.ToSlash(filepath.Join("src", slug+".md"))
	fullChapter := filepath.Join(e.repoDir, "src", slug+".md")
	if _, err := os.Stat(fullChapter); err == nil {
		return nil, fmt.Errorf("%w: chapter %s already exists", ErrInvalidArgument, slug)
	}
	if err := os.WriteFile(fullChapter, []byte(body), 0o644); err != nil {
		return nil, err
	}

	summaryFull := filepath.Join(e.repoDir, "src", "SUMMARY.md")
	rawSummary, _ := os.ReadFile(summaryFull)
	updatedSummary := insertSummaryEntry(string(rawSummary), title, slug, in.ParentSlug, in.PartTitle)
	if updatedSummary != string(rawSummary) {
		if err := os.WriteFile(summaryFull, []byte(updatedSummary), 0o644); err != nil {
			e.resetWorktreeLocked(fullChapter)
			return nil, err
		}
	}

	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("Create chapter %s (%s)", slug, title)
	}

	newFullSHA, err := e.commitPathsLocked(author, commitMsg, relChapter, "src/SUMMARY.md")
	if err != nil {
		e.resetWorktreeLocked(fullChapter)
		return nil, err
	}
	if err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	e.triggerSync()
	e.triggerBriefingRefresh()

	shortNewSHA := shortSHA(newFullSHA)
	slog.Info("chapter_created",
		"slug", slug,
		"title", title,
		"author", author.ID,
		"new_sha", shortNewSHA,
	)
	return &CreateChapterOutput{
		Slug:           slug,
		NewRevisionSHA: shortNewSHA,
		LiveChapterURL: fmt.Sprintf("%s/chapters/%s", e.baseURL, slug),
		StyleWarnings:  LintChapterMarkdown(slug, body, e.BannedWords()),
	}, nil
}

func (e *BookEngine) RevertChapterRevision(ctx context.Context, in *RevertChapterRevisionInput) (*RevertChapterRevisionOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	slug, err := ValidateSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.TargetCommitSHA) == "" {
		return nil, fmt.Errorf("%w: target_commit_sha is required", ErrInvalidArgument)
	}
	author := e.resolveEffectiveAuthor(ctx, in.AuthorName)

	e.mu.Lock()
	defer e.mu.Unlock()

	targetCommit, err := e.resolveCommitByPrefixLocked(in.TargetCommitSHA)
	if err != nil {
		return nil, err
	}

	relPath := filepath.ToSlash(filepath.Join("src", slug+".md"))
	fileObj, err := targetCommit.File(relPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s at commit %s", ErrNotFound, slug, in.TargetCommitSHA)
	}
	historicalContent, err := fileObj.Contents()
	if err != nil {
		return nil, err
	}

	fullPath := filepath.Join(e.repoDir, "src", slug+".md")
	_, existedBefore := os.Stat(fullPath)
	if currentBytes, readErr := os.ReadFile(fullPath); readErr == nil && string(currentBytes) == historicalContent {
		return nil, fmt.Errorf("%w: chapter %s is already identical to revision %s", ErrInvalidArgument, slug, shortSHA(targetCommit.Hash.String()))
	}
	if err := os.WriteFile(fullPath, []byte(historicalContent), 0o644); err != nil {
		return nil, err
	}

	commitPaths := []string{relPath}
	summaryFull := filepath.Join(e.repoDir, "src", "SUMMARY.md")
	if rawSummary, sumErr := os.ReadFile(summaryFull); sumErr == nil {
		restoredTitle := extractMarkdownH1(historicalContent)
		if restoredTitle == "" {
			restoredTitle = slug
		}
		updatedSummary := insertSummaryEntry(string(rawSummary), restoredTitle, slug, "", "")
		if updatedSummary != string(rawSummary) {
			if err := os.WriteFile(summaryFull, []byte(updatedSummary), 0o644); err != nil {
				if os.IsNotExist(existedBefore) {
					e.resetWorktreeLocked(fullPath)
				} else {
					e.resetWorktreeLocked()
				}
				return nil, err
			}
			commitPaths = append(commitPaths, "src/SUMMARY.md")
		}
	}

	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = fmt.Sprintf("Restore %s to revision %s", slug, shortSHA(targetCommit.Hash.String()))
	}
	newFullSHA, err := e.commitPathsLocked(author, reason, commitPaths...)
	if err != nil {
		if os.IsNotExist(existedBefore) {
			e.resetWorktreeLocked(fullPath)
		} else {
			e.resetWorktreeLocked()
		}
		return nil, err
	}
	if err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	e.triggerSync()
	e.triggerBriefingRefresh()

	shortNewSHA := shortSHA(newFullSHA)
	slog.Info("chapter_reverted",
		"slug", slug,
		"target_rev", in.TargetCommitSHA,
		"author", author.ID,
		"new_sha", shortNewSHA,
	)
	return &RevertChapterRevisionOutput{
		Slug:           slug,
		NewRevisionSHA: shortNewSHA,
		LiveChapterURL: fmt.Sprintf("%s/chapters/%s", e.baseURL, slug),
	}, nil
}

func validateRenameChapterInput(in *RenameChapterInput) (string, string, string, error) {
	if in == nil {
		return "", "", "", fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	oldSlug, err := ValidateSlug(in.OldSlug)
	if err != nil {
		return "", "", "", err
	}
	newSlug := oldSlug
	if strings.TrimSpace(in.NewSlug) != "" {
		newSlug, err = ValidateSlug(in.NewSlug)
		if err != nil {
			return "", "", "", err
		}
	}
	newTitle := strings.TrimSpace(in.NewTitle)
	if newSlug == oldSlug && newTitle == "" {
		return "", "", "", fmt.Errorf("%w: either a different new_slug or a non-empty new_title is required", ErrInvalidArgument)
	}
	return oldSlug, newSlug, newTitle, nil
}

func (e *BookEngine) applyChapterRenameFilesLocked(oldSlug, newSlug, newTitle string) (string, []string, []string, error) {
	oldRel := filepath.ToSlash(filepath.Join("src", oldSlug+".md"))
	oldFull := filepath.Join(e.repoDir, "src", oldSlug+".md")
	rawBytes, err := os.ReadFile(oldFull)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil, fmt.Errorf("%w: %s", ErrNotFound, oldSlug)
		}
		return "", nil, nil, err
	}
	mdContent := string(rawBytes)

	newRel := filepath.ToSlash(filepath.Join("src", newSlug+".md"))
	newFull := filepath.Join(e.repoDir, "src", newSlug+".md")
	if newSlug != oldSlug {
		if _, statErr := os.Stat(newFull); statErr == nil {
			return "", nil, nil, fmt.Errorf("%w: destination chapter %s already exists", ErrInvalidArgument, newSlug)
		}
	}

	effectiveTitle := extractMarkdownH1(mdContent)
	if newTitle != "" {
		mdContent = replaceFirstH1Heading(mdContent, newTitle)
		effectiveTitle = newTitle
	}
	if effectiveTitle == "" {
		effectiveTitle = newSlug
	}

	if err := os.WriteFile(newFull, []byte(mdContent), 0o644); err != nil {
		return "", nil, nil, err
	}
	var removePaths []string
	if newSlug != oldSlug {
		if err := os.Remove(oldFull); err != nil && !os.IsNotExist(err) {
			e.resetWorktreeLocked(newFull)
			return "", nil, nil, err
		}
		removePaths = append(removePaths, oldRel)
	}

	addPaths := []string{newRel}
	summaryFull := filepath.Join(e.repoDir, "src", "SUMMARY.md")
	if rawSummary, sumErr := os.ReadFile(summaryFull); sumErr == nil {
		updatedSummary := rewriteSummaryForRename(string(rawSummary), oldSlug, newSlug, effectiveTitle)
		if updatedSummary != string(rawSummary) {
			if err := os.WriteFile(summaryFull, []byte(updatedSummary), 0o644); err != nil {
				if newSlug != oldSlug {
					e.resetWorktreeLocked(newFull)
				} else {
					e.resetWorktreeLocked()
				}
				return "", nil, nil, err
			}
			addPaths = append(addPaths, "src/SUMMARY.md")
		}
	}
	addPaths = append(addPaths, e.rewriteCrossChapterLinksLocked(oldSlug, newSlug)...)
	return effectiveTitle, removePaths, addPaths, nil
}

func (e *BookEngine) RenameChapter(ctx context.Context, in *RenameChapterInput) (*RenameChapterOutput, error) {
	oldSlug, newSlug, newTitle, err := validateRenameChapterInput(in)
	if err != nil {
		return nil, err
	}
	author := e.resolveEffectiveAuthor(ctx, in.AuthorName)

	e.mu.Lock()
	defer e.mu.Unlock()

	effectiveTitle, removePaths, addPaths, err := e.applyChapterRenameFilesLocked(oldSlug, newSlug, newTitle)
	if err != nil {
		return nil, err
	}

	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		if newSlug != oldSlug {
			commitMsg = fmt.Sprintf("Move/rename chapter %s to %s (%s)", oldSlug, newSlug, effectiveTitle)
		} else {
			commitMsg = fmt.Sprintf("Rename chapter %s title to %s", oldSlug, effectiveTitle)
		}
	}

	newFullSHA, err := e.removeAndCommitPathsLocked(author, commitMsg, removePaths, addPaths)
	if err != nil {
		if newSlug != oldSlug {
			e.resetWorktreeLocked(filepath.Join(e.repoDir, "src", newSlug+".md"))
		} else {
			e.resetWorktreeLocked()
		}
		return nil, err
	}

	if err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	e.triggerSync()
	e.triggerBriefingRefresh()

	shortNewSHA := shortSHA(newFullSHA)
	slog.Info("chapter_renamed",
		"old_slug", oldSlug,
		"new_slug", newSlug,
		"new_title", effectiveTitle,
		"author", author.ID,
		"new_sha", shortNewSHA,
	)
	return &RenameChapterOutput{
		OldSlug:        oldSlug,
		NewSlug:        newSlug,
		NewTitle:       effectiveTitle,
		NewRevisionSHA: shortNewSHA,
		LiveChapterURL: fmt.Sprintf("%s/chapters/%s", e.baseURL, newSlug),
		PermalinkURL:   fmt.Sprintf("%s/chapters/%s/rev/%s", e.baseURL, newSlug, shortNewSHA),
	}, nil
}

func (e *BookEngine) DeleteChapter(ctx context.Context, in *DeleteChapterInput) (*DeleteChapterOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	slug, err := ValidateSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	var replacedBy string
	if strings.TrimSpace(in.ReplacedBySlug) != "" {
		replacedBy, err = ValidateSlug(in.ReplacedBySlug)
		if err != nil {
			return nil, err
		}
		if replacedBy == slug {
			return nil, fmt.Errorf("%w: replaced_by_slug cannot equal deleted slug %s", ErrInvalidArgument, slug)
		}
	}
	author := e.resolveEffectiveAuthor(ctx, in.AuthorName)

	e.mu.Lock()
	defer e.mu.Unlock()

	relPath := filepath.ToSlash(filepath.Join("src", slug+".md"))
	fullPath := filepath.Join(e.repoDir, "src", slug+".md")
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, slug)
		}
		return nil, err
	}

	lastCommitBeforeDelete, _ := e.latestCommitForFileLocked(relPath)
	archivedSHA := "HEAD"
	if lastCommitBeforeDelete != nil {
		archivedSHA = shortSHA(lastCommitBeforeDelete.Hash.String())
	}

	if err := os.Remove(fullPath); err != nil {
		return nil, err
	}
	removePaths := []string{relPath}
	var addPaths []string

	summaryFull := filepath.Join(e.repoDir, "src", "SUMMARY.md")
	if rawSummary, sumErr := os.ReadFile(summaryFull); sumErr == nil {
		updatedSummary := removeSummaryEntry(string(rawSummary), slug)
		if updatedSummary != string(rawSummary) {
			if err := os.WriteFile(summaryFull, []byte(updatedSummary), 0o644); err != nil {
				e.resetWorktreeLocked()
				return nil, err
			}
			addPaths = append(addPaths, "src/SUMMARY.md")
		}
	}

	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		reason = fmt.Sprintf("Delete chapter %s", slug)
	}
	if replacedBy != "" && !strings.Contains(strings.ToLower(reason), "[replaced-by:") {
		reason = fmt.Sprintf("%s [replaced-by: %s]", reason, replacedBy)
	}

	newFullSHA, err := e.removeAndCommitPathsLocked(author, reason, removePaths, addPaths)
	if err != nil {
		e.resetWorktreeLocked()
		return nil, err
	}
	if err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	e.triggerSync()
	e.triggerBriefingRefresh()

	shortNewSHA := shortSHA(newFullSHA)
	slog.Info("chapter_deleted",
		"slug", slug,
		"replaced_by", replacedBy,
		"archived_sha", archivedSHA,
		"author", author.ID,
		"new_sha", shortNewSHA,
	)
	return &DeleteChapterOutput{
		DeletedSlug:          slug,
		NewRevisionSHA:       shortNewSHA,
		ArchivedPermalinkURL: fmt.Sprintf("%s/chapters/%s/rev/%s", e.baseURL, slug, archivedSHA),
	}, nil
}
