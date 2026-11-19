package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wjkoh/ghostbook/internal/engine"
)

func assertZeroHTMLChrome(t *testing.T, label, md string) {
	t.Helper()
	for _, forbidden := range []string{"<!DOCTYPE", "<html", "<head>", "<aside", "<div ", "<script", "<style>"} {
		if strings.Contains(strings.ToLower(md), strings.ToLower(forbidden)) {
			t.Fatalf("%s unexpectedly contains HTML chrome token %q:\n%s", label, forbidden, md)
		}
	}
}

func TestSuiteA_MarkdownExportEngine_UT_MD_ENG_01_to_08(t *testing.T) {
	ctx := context.Background()

	t.Run("UT-MD-ENG-01_AssembleLLMsIndexAtHEAD", func(t *testing.T) {
		eng, _ := setupTestEngine(t)

		// Add a nested sub-chapter under 01-system-architecture to verify hierarchical indentation
		if _, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
			Slug:            "01a-fee-compression",
			Title:           "1.1 Fee Compression Mechanics",
			ParentSlug:      "01-system-architecture",
			InitialMarkdown: "# 1.1 Fee Compression Mechanics\n\nDetailed MFO fee stack breakdown.\n",
			CommitMessage:   "Add sub-chapter 1.1",
		}); err != nil {
			t.Fatalf("CreateChapter sub-chapter: %v", err)
		}

		res, err := eng.AssembleLLMsIndex(ctx, "")
		if err != nil {
			t.Fatalf("AssembleLLMsIndex(HEAD): %v", err)
		}
		if !res.IsLatest || res.RevisionSHA == "" || res.RevisionNumber < 2 {
			t.Fatalf("unexpected export metadata: %+v", res)
		}
		assertZeroHTMLChrome(t, "llms.txt HEAD", res.Markdown)

		for _, mustContain := range []string{
			"# Ghostbook\n\n> Canonical Git-backed living book (Living Book).",
			"## Whole-Book Raw Markdown (Single-Request Ingest)",
			"https://docs.example.com/llms-full.txt",
			"https://docs.example.com/book.md",
			"## Table of Contents (Per-Chapter Raw Markdown)",
			"https://docs.example.com/chapters/01-system-architecture.md",
			"  - [1.1 Fee Compression Mechanics](https://docs.example.com/chapters/01a-fee-compression.md)",
			"## Meta Documents & Agent Protocols",
			"https://docs.example.com/SUMMARY.md",
			"https://docs.example.com/STYLE_GUIDE.md",
			"https://docs.example.com/changelog.md",
			"https://docs.example.com/guide.md",
			"https://docs.example.com/openapi.json",
			"https://docs.example.com/feed.xml",
		} {
			if !strings.Contains(res.Markdown, mustContain) {
				t.Fatalf("llms.txt missing %q:\n%s", mustContain, res.Markdown)
			}
		}
	})

	t.Run("UT-MD-ENG-02_AssembleLLMsIndexAtHistoricalRevision", func(t *testing.T) {
		eng, _ := setupTestEngine(t)

		r1Index, err := eng.AssembleLLMsIndex(ctx, "r1")
		if err != nil {
			t.Fatalf("AssembleLLMsIndex(r1 initial): %v", err)
		}
		r1SHA := r1Index.RevisionSHA

		// Create a new chapter in r2
		if _, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
			Slug:            "99-later-chapter",
			Title:           "Chapter 99: Future Expansion",
			InitialMarkdown: "# Chapter 99: Future Expansion\n\nAdded after r1.\n",
			CommitMessage:   "Add chapter 99",
		}); err != nil {
			t.Fatalf("CreateChapter: %v", err)
		}

		histIndex, err := eng.AssembleLLMsIndex(ctx, "r1")
		if err != nil {
			t.Fatalf("AssembleLLMsIndex(r1 historical): %v", err)
		}
		if histIndex.IsLatest {
			t.Fatalf("expected IsLatest=false for r1 after r2 commit")
		}
		if histIndex.RevisionSHA != r1SHA || histIndex.RevisionNumber != 1 || histIndex.HeadRevisionNumber != 2 {
			t.Fatalf("unexpected historical metadata: %+v", histIndex)
		}
		if strings.Contains(histIndex.Markdown, "99-later-chapter") {
			t.Fatalf("historical r1 llms.txt leaked r2 chapter 99-later-chapter:\n%s", histIndex.Markdown)
		}
		if !strings.Contains(histIndex.Markdown, "Historical Snapshot of Ghostbook pinned to revision r1") {
			t.Fatalf("expected historical snapshot blockquote in r1 llms.txt:\n%s", histIndex.Markdown)
		}

		if _, err := eng.AssembleLLMsIndex(ctx, "deadbeef999"); !errors.Is(err, engine.ErrNotFound) {
			t.Fatalf("expected ErrNotFound for unknown revision, got %v", err)
		}
	})

	t.Run("UT-MD-ENG-03_AssembleFullBookMarkdownAndFenceBalancing", func(t *testing.T) {
		eng, _ := setupTestEngine(t)

		// Inject an unclosed ```python code fence at the end of Chapter 1 to test fence-leak protection
		ch1, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "01-system-architecture"})
		if err != nil {
			t.Fatalf("ReadChapter ch1: %v", err)
		}
		trimmedEnd := strings.TrimRight(ch1.ContentMarkdown, "\n")
		lastLineIdx := strings.LastIndex(trimmedEnd, "\n")
		tailTarget := trimmedEnd[lastLineIdx+1:]
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       tailTarget,
			NewText:       tailTarget + "\n\n```python\ndef unclosed_example():\n    return 42",
			CommitMessage: "Append unclosed fence to chapter 1",
		}); err != nil {
			t.Fatalf("PatchChapter unclosed fence: %v", err)
		}

		fullRes, err := eng.AssembleFullBookMarkdown(ctx, "")
		if err != nil {
			t.Fatalf("AssembleFullBookMarkdown(HEAD): %v", err)
		}
		assertZeroHTMLChrome(t, "llms-full.txt HEAD", fullRes.Markdown)

		if !strings.Contains(fullRes.Markdown, "def unclosed_example():\n    return 42\n```\n") {
			t.Fatalf("expected unclosed python fence at end of chapter 1 to be auto-closed before chapter 2:\n%s", fullRes.Markdown)
		}

		idxCh1 := strings.Index(fullRes.Markdown, "**Chapter Slug:** 01-system-architecture")
		idxCh2 := strings.Index(fullRes.Markdown, "**Chapter Slug:** 02-agent-workflow")
		if idxCh1 < 0 || idxCh2 < 0 || idxCh1 >= idxCh2 {
			t.Fatalf("expected chapters in SUMMARY.md order (ch1 before ch2), got idxCh1=%d idxCh2=%d", idxCh1, idxCh2)
		}
	})

	t.Run("UT-MD-ENG-04_AssembleFullBookMarkdownHistoricalImmutability", func(t *testing.T) {
		eng, _ := setupTestEngine(t)

		r1Book, err := eng.AssembleFullBookMarkdown(ctx, "r1")
		if err != nil {
			t.Fatalf("AssembleFullBookMarkdown(r1): %v", err)
		}

		// Mutate Chapter 1 and delete Chapter 2 in subsequent commits
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       "Ghostbook bundles pure-Go",
			NewText:       "pure-Go Git MFO Platform",
			CommitMessage: "Update pure-Go Git phrasing",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}
		if _, err := eng.DeleteChapter(ctx, &engine.DeleteChapterInput{
			Slug:   "02-agent-workflow",
			Reason: "Archive chapter 2 for test",
		}); err != nil {
			t.Fatalf("DeleteChapter: %v", err)
		}

		headSnapBefore := eng.CurrentSnapshot()
		r1After, err := eng.AssembleFullBookMarkdown(ctx, "r1")
		if err != nil {
			t.Fatalf("AssembleFullBookMarkdown(r1 after edits): %v", err)
		}
		headSnapAfter := eng.CurrentSnapshot()

		if headSnapBefore != headSnapAfter {
			t.Fatalf("historical book assembly mutated CurrentSnapshot pointer")
		}
		if r1After.IsLatest || r1After.RevisionNumber != 1 {
			t.Fatalf("expected historical r1 metadata, got %+v", r1After)
		}
		if !strings.Contains(r1After.Markdown, "Ghostbook bundles pure-Go") || strings.Contains(r1After.Markdown, "pure-Go Git MFO Platform") {
			t.Fatalf("historical r1 book did not preserve exact r1 Chapter 1 text")
		}
		if !strings.Contains(r1After.Markdown, "**Chapter Slug:** 02-agent-workflow") || r1After.WordCount != r1Book.WordCount {
			t.Fatalf("historical r1 book lost deleted Chapter 2 or changed word count (%d vs %d)", r1After.WordCount, r1Book.WordCount)
		}
	})

	t.Run("UT-MD-ENG-05_AssembleChangelogMarkdown", func(t *testing.T) {
		eng, _ := setupTestEngine(t)
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       "Ghostbook bundles pure-Go",
			NewText:       "Ghostbook bundles pure-Go LLC",
			CommitMessage: "Clarify entity suffix | test pipe escaping",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}

		cl, err := eng.AssembleChangelogMarkdown(ctx)
		if err != nil {
			t.Fatalf("AssembleChangelogMarkdown: %v", err)
		}
		assertZeroHTMLChrome(t, "changelog.md", cl.Markdown)
		for _, want := range []string{
			"# Git Commit Log",
			"| Revision | Author | Timestamp (UTC) | Summary | Modified Files & Raw Markdown Permalinks |",
			"/book.md)",
			"/chapters/01-system-architecture.md)",
			`Clarify entity suffix \| test pipe escaping`,
		} {
			if !strings.Contains(cl.Markdown, want) {
				t.Fatalf("changelog.md missing %q:\n%s", want, cl.Markdown)
			}
		}
	})

	t.Run("UT-MD-ENG-06_AssembleGuideMarkdownAndStyleGuideFallback", func(t *testing.T) {
		eng, _ := setupTestEngine(t)
		gd, err := eng.AssembleGuideMarkdown(ctx)
		if err != nil {
			t.Fatalf("AssembleGuideMarkdown: %v", err)
		}
		assertZeroHTMLChrome(t, "guide.md", gd.Markdown)
		for _, want := range []string{
			"# AI Agent & Co-Author Setup Guide",
			"https://docs.example.com/llms.txt",
			"https://docs.example.com/llms-full.txt",
			"Accept: text/markdown",
			"?raw=exact",
			"/u/<email>:<secret-key>/mcp",
			"## 3. All 13 MCP Authoring & Audit Tools",
			"patch_chapter",
			"revert_chapter_revision",
			"report_platform_feedback",
		} {
			if !strings.Contains(gd.Markdown, want) {
				t.Fatalf("guide.md missing %q:\n%s", want, gd.Markdown)
			}
		}

		// Verify ReadChapter("STYLE_GUIDE") fallback succeeds even if src/STYLE_GUIDE.md is absent on disk
		sg, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "STYLE_GUIDE"})
		if err != nil || sg.ContentMarkdown == "" || sg.LifecycleStatus != "current" {
			t.Fatalf("expected STYLE_GUIDE ReadChapter fallback to succeed, got out=%+v err=%v", sg, err)
		}
	})

	t.Run("UT-MD-ENG-07_FormatPermalinkLineageMarkdownBannerAll5States", func(t *testing.T) {
		base := "https://docs.example.com"

		bCurrent := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:            "01-system-architecture",
			RevisionSHA:     "1111111",
			RevisionNumber:  1,
			LifecycleStatus: "current",
		})
		if !strings.Contains(bCurrent, "Pinned Revision Permalink (current)") || !strings.Contains(bCurrent, "/chapters/01-system-architecture.md") {
			t.Fatalf("unexpected current banner:\n%s", bCurrent)
		}

		bOutdated := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:                 "01-system-architecture",
			RevisionSHA:          "1111111",
			RevisionNumber:       1,
			CommitsBehind:        3,
			LatestRevisionNumber: 4,
			LatestRevisionSHA:    "4444444",
			LifecycleStatus:      "outdated",
		})
		if !strings.Contains(bOutdated, "Permalink Revision Notice (outdated)") || !strings.Contains(bOutdated, "?raw=exact") {
			t.Fatalf("unexpected outdated banner:\n%s", bOutdated)
		}

		bRenamed := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:               "01-system-architecture",
			RevisionSHA:        "1111111",
			RevisionNumber:     1,
			LifecycleStatus:    "renamed",
			LifecycleCommitSHA: "2222222",
			MovedToTitle:       "Chapter 1: Updated Title",
		})
		if !strings.Contains(bRenamed, "Permalink Revision Notice (renamed)") || !strings.Contains(bRenamed, "Chapter 1: Updated Title") {
			t.Fatalf("unexpected renamed banner:\n%s", bRenamed)
		}

		bMoved := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:               "01-old-slug",
			RevisionSHA:        "1111111",
			RevisionNumber:     1,
			LifecycleStatus:    "moved",
			LifecycleCommitSHA: "3333333",
			MovedToSlug:        "01-new-slug",
		})
		if !strings.Contains(bMoved, "Permalink Revision Notice (moved)") || !strings.Contains(bMoved, "/chapters/01-new-slug.md") {
			t.Fatalf("unexpected moved banner:\n%s", bMoved)
		}

		bDeletedReplaced := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:               "02-deleted",
			RevisionSHA:        "1111111",
			RevisionNumber:     1,
			LifecycleStatus:    "deleted",
			LifecycleCommitSHA: "4444444",
			LifecycleAuthor:    "Bob Rivera",
			LifecycleMessage:   "Consolidated into ch1",
			MovedToSlug:        "01-system-architecture",
		})
		if !strings.Contains(bDeletedReplaced, "Deleted Chapter Archive (deleted)") || !strings.Contains(bDeletedReplaced, "**Replacement Chapter Raw Markdown:** https://docs.example.com/chapters/01-system-architecture.md") {
			t.Fatalf("unexpected deleted+replaced banner:\n%s", bDeletedReplaced)
		}

		bDeletedNoReplace := engine.FormatPermalinkLineageMarkdownBanner(base, &engine.ReadChapterOutput{
			Slug:               "02-deleted",
			RevisionSHA:        "1111111",
			RevisionNumber:     1,
			LifecycleStatus:    "deleted",
			LifecycleCommitSHA: "4444444",
			LifecycleAuthor:    "Bob Rivera",
			LifecycleMessage:   "Deprecated",
		})
		if !strings.Contains(bDeletedNoReplace, "**Active Book Discovery Index:** https://docs.example.com/llms.txt") {
			t.Fatalf("unexpected deleted without replacement banner:\n%s", bDeletedNoReplace)
		}
	})

	t.Run("UT-MD-ENG-08_ConcurrentReadersAndWritersRaceSafety", func(t *testing.T) {
		eng, _ := setupTestEngine(t)
		var wg sync.WaitGroup

		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				for iter := 0; iter < 5; iter++ {
					switch (id + iter) % 5 {
					case 0:
						if _, err := eng.AssembleLLMsIndex(ctx, ""); err != nil {
							t.Errorf("AssembleLLMsIndex HEAD: %v", err)
						}
					case 1:
						if _, err := eng.AssembleLLMsIndex(ctx, "r1"); err != nil {
							t.Errorf("AssembleLLMsIndex r1: %v", err)
						}
					case 2:
						if _, err := eng.AssembleFullBookMarkdown(ctx, ""); err != nil {
							t.Errorf("AssembleFullBookMarkdown HEAD: %v", err)
						}
					case 3:
						if _, err := eng.AssembleFullBookMarkdown(ctx, "r1"); err != nil {
							t.Errorf("AssembleFullBookMarkdown r1: %v", err)
						}
					case 4:
						_, _ = eng.AssembleChangelogMarkdown(ctx)
						_, _ = eng.AssembleGuideMarkdown(ctx)
					}
				}
			}(g)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			phrases := []string{"Ghostbook bundles pure-Go", "pure-Go Git Step 1", "pure-Go Git Step 2", "pure-Go Git Step 3"}
			for i := 0; i < len(phrases)-1; i++ {
				if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
					Slug:          "01-system-architecture",
					OldText:       phrases[i],
					NewText:       phrases[i+1],
					CommitMessage: "Concurrent patch step",
				}); err != nil {
					t.Errorf("concurrent PatchChapter %d: %v", i, err)
					return
				}
			}
		}()

		wg.Wait()
	})
}
