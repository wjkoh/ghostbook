package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

func setupTestEngine(t *testing.T) (*engine.BookEngine, *store.Store) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_studio.db")
	repoDir := filepath.Join(tmpDir, "repo")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	if err := st.SyncConfiguredAuthors(context.Background(), store.DefaultDemoAuthors); err != nil {
		t.Fatalf("sync demo authors: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := engine.NewBookEngine(repoDir, st, "https://docs.example.com")
	if err != nil {
		t.Fatalf("new book engine: %v", err)
	}
	return eng, st
}

// UT-01: TestPatchChapter_ExactMatchAndGitCommit
func TestPatchChapter_ExactMatchAndGitCommit(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	samRef, _ := st.ResolveAuthorName(ctx, "alice")
	alice, err := st.Authenticate(ctx, samRef.AuthIdentifier())
	if err != nil {
		t.Fatalf("authenticate Alice: %v", err)
	}
	ctx = engine.WithAuthor(ctx, alice)

	chBefore, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "01-system-architecture"})
	if err != nil {
		t.Fatalf("read chapter: %v", err)
	}

	oldPhrase := "Ghostbook bundles pure-Go"
	newPhrase := "Ghostbook bundles pure-Go (San Francisco & New York)"
	patchOut, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       oldPhrase,
		NewText:       newPhrase,
		CommitMessage: "Add SF and NY office context for pure-Go Git",
	})
	if err != nil {
		t.Fatalf("patch chapter failed: %v", err)
	}
	if patchOut.NewRevisionSHA == "" || patchOut.NewRevisionSHA == chBefore.RevisionSHA {
		t.Fatalf("expected new revision SHA, got %q (before %q)", patchOut.NewRevisionSHA, chBefore.RevisionSHA)
	}
	if !strings.Contains(patchOut.CommittedBy, "Alice Chen") || !strings.Contains(patchOut.CommittedBy, "alice@example.com") {
		t.Fatalf("unexpected committed_by signature: %q", patchOut.CommittedBy)
	}
	if !strings.Contains(patchOut.DiffPreview, "+"+newPhrase) {
		t.Fatalf("expected diff preview to include added line, got:\n%s", patchOut.DiffPreview)
	}

	diffOut, err := eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		CommitSHA: patchOut.NewRevisionSHA,
		Slug:      "01-system-architecture",
	})
	if err != nil {
		t.Fatalf("get commit diff: %v", err)
	}
	if diffOut.AuthorName != "Alice Chen" {
		t.Fatalf("expected commit author Alice Chen, got %q", diffOut.AuthorName)
	}
	if !strings.Contains(diffOut.UnifiedDiff, newPhrase) {
		t.Fatalf("expected unified diff to contain %q, got:\n%s", newPhrase, diffOut.UnifiedDiff)
	}
}

// UT-02: TestPatchChapter_MissingOrAmbiguousTarget
func TestPatchChapter_MissingOrAmbiguousTarget(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	before, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"})
	if err != nil {
		t.Fatalf("read chapter: %v", err)
	}

	// 1. Missing target text
	_, err = eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "THIS_SENTENCE_DOES_NOT_EXIST_ANYWHERE_12345",
		NewText:       "Replacement",
		CommitMessage: "Should fail missing target",
	})
	if !errors.Is(err, engine.ErrPatchNotFound) {
		t.Fatalf("expected ErrPatchNotFound, got: %v", err)
	}

	// 2. Ambiguous target text ("Bucket" appears multiple times)
	_, err = eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "MCP",
		NewText:       "RPC",
		CommitMessage: "Should fail ambiguous target",
	})
	if !errors.Is(err, engine.ErrPatchAmbiguous) {
		t.Fatalf("expected ErrPatchAmbiguous, got: %v", err)
	}

	// Verify zero file corruption
	after, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"})
	if err != nil {
		t.Fatalf("read chapter after failed patches: %v", err)
	}
	if after.ContentMarkdown != before.ContentMarkdown || after.RevisionSHA != before.RevisionSHA {
		t.Fatalf("chapter content or revision mutated despite rejected patches")
	}
}

// UT-03: TestPatchChapter_OptimisticRevisionConflict
func TestPatchChapter_OptimisticRevisionConflict(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	_, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "03-editorial-lineage",
		OldText:             "Five Killer Product Features",
		NewText:             "Five Institutional Product Features",
		CommitMessage:       "Test stale revision lock",
		ExpectedRevisionSHA: "deadbee",
	})
	if !errors.Is(err, engine.ErrRevisionConflict) {
		t.Fatalf("expected ErrRevisionConflict, got: %v", err)
	}
}

// UT-04: TestCreateChapter_UpdatesSummaryMd
func TestCreateChapter_UpdatesSummaryMd(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	carolRef, _ := st.ResolveAuthorName(ctx, "carol")
	carol, err := st.Authenticate(ctx, carolRef.AuthIdentifier())
	if err != nil {
		t.Fatalf("authenticate Carol: %v", err)
	}
	ctx = engine.WithAuthor(ctx, carol)

	createOut, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            "05-security-model",
		Title:           "Chapter 5: Security & Auth Model",
		InitialMarkdown: "This chapter details Foreign Grantor Trust structuring and Korea-US treaty optimization for Dual-Nexus founders.",
		CommitMessage:   "Create Chapter 5 on Korea-US tax treaty mechanics",
	})
	if err != nil {
		t.Fatalf("create chapter: %v", err)
	}
	if createOut.NewRevisionSHA == "" {
		t.Fatalf("expected non-empty revision SHA")
	}

	toc, err := eng.GetTableOfContents(ctx, &engine.GetTableOfContentsInput{})
	if err != nil {
		t.Fatalf("get toc: %v", err)
	}
	if !strings.Contains(toc.SummaryMarkdown, "(05-security-model.md)") {
		t.Fatalf("expected SUMMARY.md to include new chapter link, got:\n%s", toc.SummaryMarkdown)
	}

	searchOut, err := eng.SearchBook(ctx, &engine.SearchBookInput{Query: "Grantor"})
	if err != nil {
		t.Fatalf("search newly created chapter: %v", err)
	}
	if len(searchOut.Matches) == 0 || searchOut.Matches[0].Slug != "05-security-model" {
		t.Fatalf("expected FTS5 search to find new chapter immediately, got: %+v", searchOut.Matches)
	}
}

// UT-05: TestInlineMarkdownCallouts_And_RegressionGuards
func TestInlineMarkdownCallouts_And_RegressionGuards(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	alice, _ := st.ResolveAuthorName(ctx, "alice")
	ctxAlice := engine.WithAuthor(ctx, alice)

	_, err := eng.PatchChapter(ctxAlice, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "## 1. Surgical Patching",
		NewText:       "## 1. Surgical Patching\n\n> **[Comment · Alice Chen · 2026-11-18]:** Add a 200 bps SOFR stress row here.",
		CommitMessage: "Add inline comment callout in chapter 2",
	})
	if err != nil {
		t.Fatalf("PatchChapter inline callout: %v", err)
	}

	toc, err := eng.GetTableOfContents(ctx, nil)
	if err != nil {
		t.Fatalf("GetTableOfContents: %v", err)
	}
	var ch2Count int32
	for _, ch := range toc.Chapters {
		if ch.Slug == "02-agent-workflow" {
			ch2Count = ch.OpenCommentCount
		}
	}
	if ch2Count != 1 {
		t.Fatalf("expected OpenCommentCount=1 from inline Markdown callout, got %d", ch2Count)
	}

	// Short/symbol multi-token query ("S & P", "$ 5") must NOT match every line of every chapter
	shortSearch, err := eng.SearchBook(ctx, &engine.SearchBookInput{Query: "S & P"})
	if err != nil {
		t.Fatalf("SearchBook short tokens: %v", err)
	}
	if len(shortSearch.Matches) != 0 {
		t.Fatalf("expected 0 matches for absent short-token query 'S & P', got %d", len(shortSearch.Matches))
	}

	// GetCommitDiff with Slug filter on a commit that did NOT touch that chapter must return empty UnifiedDiff
	diffOther, err := eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		CommitSHA: "HEAD",
		Slug:      "01-system-architecture",
	})
	if err != nil {
		t.Fatalf("GetCommitDiff filtered: %v", err)
	}
	if diffOther.UnifiedDiff != "" {
		t.Fatalf("expected empty UnifiedDiff when filtered slug was untouched in HEAD, got:\n%s", diffOther.UnifiedDiff)
	}

	// ValidateSlug must reject meta/route collisions ("summary", "style-guide", >120 chars)
	for _, bad := range []string{"summary", "style-guide", "llms", "openapi", strings.Repeat("a", 121)} {
		if _, vErr := engine.ValidateSlug(bad); !errors.Is(vErr, engine.ErrInvalidSlug) {
			t.Fatalf("expected ErrInvalidSlug for %q, got %v", bad, vErr)
		}
	}

	// Prefix-sharing slug rename must NOT corrupt longer sibling links or /rev/ permalinks
	if _, err := eng.CreateChapter(ctxAlice, &engine.CreateChapterInput{
		Slug:            "01-mfo",
		Title:           "Chapter 1A: Short Prefix",
		InitialMarkdown: "# Chapter 1A: Short Prefix\n\nBody.\n",
	}); err != nil {
		t.Fatalf("CreateChapter 01-mfo: %v", err)
	}
	if _, err := eng.PatchChapter(ctxAlice, &engine.PatchChapterInput{
		Slug:          "04-deployment-ops",
		OldText:       "## 2. Remote Git Sync",
		NewText:       "## 2. Remote Git Sync\n\nSee [Short](/chapters/01-mfo), [Long](/chapters/01-system-architecture), and [Pinned](/chapters/01-mfo/rev/r1).",
		CommitMessage: "Add cross-chapter prefix links",
	}); err != nil {
		t.Fatalf("PatchChapter cross-links: %v", err)
	}
	if _, err := eng.RenameChapter(ctxAlice, &engine.RenameChapterInput{
		OldSlug: "01-mfo",
		NewSlug: "01-family-office",
	}); err != nil {
		t.Fatalf("RenameChapter 01-mfo -> 01-family-office: %v", err)
	}
	ch4After, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{Slug: "04-deployment-ops"})
	if err != nil {
		t.Fatalf("ReadChapter ch4: %v", err)
	}
	if !strings.Contains(ch4After.ContentMarkdown, "(/chapters/01-family-office)") ||
		!strings.Contains(ch4After.ContentMarkdown, "(/chapters/01-system-architecture)") ||
		!strings.Contains(ch4After.ContentMarkdown, "(/chapters/01-mfo/rev/r1)") {
		t.Fatalf("unexpected link rewrite result in ch4:\n%s", ch4After.ContentMarkdown)
	}
}

// UT-06: TestSearchBook_FTS5BM25
func TestSearchBook_FTS5BM25(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	res, err := eng.SearchBook(ctx, &engine.SearchBookInput{
		Query: "Quantitative Notation",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("search book: %v", err)
	}
	if len(res.Matches) == 0 {
		t.Fatalf("expected FTS5 match for Quantitative Notation")
	}
	if res.Matches[0].Slug != "02-agent-workflow" {
		t.Fatalf("expected top match in 02-agent-workflow, got %q", res.Matches[0].Slug)
	}
	if !strings.Contains(res.Matches[0].Snippet, "<mark>") {
		t.Fatalf("expected <mark> highlighted snippet, got %q", res.Matches[0].Snippet)
	}
}

// UT-07: TestGitBlameAndRevert_RoundTrip
func TestGitBlameAndRevert_RoundTrip(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	carol, _ := st.ResolveAuthorName(ctx, "carol")
	ctxCarol := engine.WithAuthor(ctx, carol)

	orig, err := eng.ReadChapter(ctxCarol, &engine.ReadChapterInput{Slug: "04-deployment-ops"})
	if err != nil {
		t.Fatalf("read chapter: %v", err)
	}

	_, err = eng.PatchChapter(ctxCarol, &engine.PatchChapterInput{
		Slug:          "04-deployment-ops",
		OldText:       "# Chapter 4: Single-Binary Deployment",
		NewText:       "# Chapter 4: Single-Binary Deployment (Phase 1 Wedge)",
		CommitMessage: "Clarify Phase 1 Wedge in title",
	})
	if err != nil {
		t.Fatalf("patch chapter: %v", err)
	}

	blameOut, err := eng.GetChapterBlame(ctxCarol, &engine.GetChapterBlameInput{Slug: "04-deployment-ops"})
	if err != nil {
		t.Fatalf("get chapter blame: %v", err)
	}
	if len(blameOut.Sections) == 0 {
		t.Fatalf("expected non-empty blame sections")
	}
	if blameOut.Sections[0].LastAuthor != "Carol Park" {
		t.Fatalf("expected first section blame author Carol Park, got %q", blameOut.Sections[0].LastAuthor)
	}

	revOut, err := eng.RevertChapterRevision(ctxCarol, &engine.RevertChapterRevisionInput{
		Slug:            "04-deployment-ops",
		TargetCommitSHA: orig.RevisionSHA,
		Reason:          "Revert title change for clean test",
	})
	if err != nil {
		t.Fatalf("revert chapter revision: %v", err)
	}
	if revOut.NewRevisionSHA == "" {
		t.Fatalf("expected new forward commit SHA on revert")
	}

	restored, err := eng.ReadChapter(ctxCarol, &engine.ReadChapterInput{Slug: "04-deployment-ops"})
	if err != nil {
		t.Fatalf("read restored chapter: %v", err)
	}
	if restored.ContentMarkdown != orig.ContentMarkdown {
		t.Fatalf("expected restored markdown to match original exactly")
	}
}

// UT-08: TestSlugPathTraversalRejection
func TestSlugPathTraversalRejection(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	maliciousSlugs := []string{
		"../etc/passwd",
		"../../secret",
		"01-mfo/../../evil",
		"/etc/hosts",
		"UPPERCASE_SLUG",
		"slug with spaces",
		"",
	}
	for _, bad := range maliciousSlugs {
		_, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: bad})
		if !errors.Is(err, engine.ErrInvalidSlug) {
			t.Fatalf("expected ErrInvalidSlug for %q, got: %v", bad, err)
		}
	}
}

// UT-09: TestEngine_NilInputsAndValidationEdgeCases
func TestEngine_NilInputsAndValidationEdgeCases(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	if _, err := engine.NewBookEngine(t.TempDir(), nil, ""); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for nil store, got %v", err)
	}
	if eng.Store() != st {
		t.Fatalf("expected Store() to return underlying store")
	}
	if eng.BaseURL() != "https://docs.example.com" {
		t.Fatalf("unexpected BaseURL: %q", eng.BaseURL())
	}
	if !strings.Contains(eng.StyleGuide(), "Strunk & White Standard") {
		t.Fatalf("expected StyleGuide() to return editorial guidelines")
	}

	// Nil input checks
	if _, err := eng.ReadChapter(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on ReadChapter(nil), got %v", err)
	}
	if _, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "99-does-not-exist"}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on missing chapter, got %v", err)
	}
	if out, err := eng.SearchBook(ctx, nil); err != nil || len(out.Matches) != 0 {
		t.Fatalf("expected empty SearchBook on nil input, got %+v err=%v", out, err)
	}
	if _, err := eng.PatchChapter(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on PatchChapter(nil), got %v", err)
	}
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{Slug: "01-system-architecture", OldText: "   "}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on empty OldText, got %v", err)
	}
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{Slug: "99-missing", OldText: "abc"}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on missing chapter patch, got %v", err)
	}
	if _, err := eng.CreateChapter(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on CreateChapter(nil), got %v", err)
	}
	if _, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{Slug: "07-empty-title", Title: "   "}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on empty title, got %v", err)
	}
	// Duplicate chapter creation must fail with ErrInvalidArgument rather than overwriting
	if _, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{Slug: "01-system-architecture", Title: "Duplicate"}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when creating duplicate chapter slug, got %v", err)
	}

	if _, err := eng.ListRecentChanges(ctx, nil); err != nil {
		t.Fatalf("expected ListRecentChanges(nil) to succeed, got %v", err)
	}
	if _, err := eng.GetCommitDiff(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on GetCommitDiff(nil), got %v", err)
	}
	if _, err := eng.GetChapterBlame(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on GetChapterBlame(nil), got %v", err)
	}
	if _, err := eng.RevertChapterRevision(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on RevertChapterRevision(nil), got %v", err)
	}
	if _, err := eng.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{Slug: "01-system-architecture", TargetCommitSHA: ""}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on empty TargetCommitSHA, got %v", err)
	}
	if _, err := eng.ListAgentActivity(ctx, nil); err != nil {
		t.Fatalf("expected ListAgentActivity(nil) to succeed, got %v", err)
	}
}

// UT-10: TestCreateChapter_ParentSlugAndInitialBodyVariants
func TestCreateChapter_ParentSlugAndInitialBodyVariants(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	// 1. Create sub-chapter with ParentSlug and empty InitialMarkdown
	out1, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:       "02a-obpi-floor-math",
		Title:      "Section 2A: OBPI Floor Calibration",
		ParentSlug: "02-agent-workflow",
		AuthorName: "Alice Chen",
	})
	if err != nil {
		t.Fatalf("CreateChapter with ParentSlug: %v", err)
	}
	toc, err := eng.GetTableOfContents(ctx, nil)
	if err != nil {
		t.Fatalf("GetTableOfContents: %v", err)
	}
	if !strings.Contains(toc.SummaryMarkdown, "  - [Section 2A: OBPI Floor Calibration](02a-obpi-floor-math.md)") {
		t.Fatalf("expected indented entry under parent in SUMMARY.md, got:\n%s", toc.SummaryMarkdown)
	}

	// 1b. Create a second sub-chapter under the same ParentSlug and verify ordering + PartTitle grouping
	_, err = eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:       "02b-tail-hedging",
		Title:      "Section 2B: Tail Risk Hedging",
		ParentSlug: "02-agent-workflow",
		AuthorName: "Carol Park",
	})
	if err != nil {
		t.Fatalf("CreateChapter second sub-chapter: %v", err)
	}

	// 1c. Create a chapter under an existing PartTitle and under a brand-new PartTitle
	_, err = eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:       "02c-bucket1-compounding",
		Title:      "Chapter 2C: Snapshot Caching",
		PartTitle:  "Part I",
		AuthorName: "Bob Rivera",
	})
	if err != nil {
		t.Fatalf("CreateChapter with existing PartTitle: %v", err)
	}
	_, err = eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:       "08-custody-mpc-rail",
		Title:      "Chapter 8: Institutional Custody & MPC Rail",
		PartTitle:  "Part III — Custody, Execution & Governance",
		AuthorName: "Bob Rivera",
	})
	if err != nil {
		t.Fatalf("CreateChapter with new PartTitle: %v", err)
	}

	tocAfterParts, err := eng.GetTableOfContents(ctx, nil)
	if err != nil {
		t.Fatalf("GetTableOfContents after PartTitle: %v", err)
	}
	if !strings.Contains(tocAfterParts.SummaryMarkdown, "# Part III — Custody, Execution & Governance") {
		t.Fatalf("expected new Part III header in SUMMARY.md, got:\n%s", tocAfterParts.SummaryMarkdown)
	}
	foundSub := false
	foundPart3 := false
	for _, ch := range tocAfterParts.Chapters {
		if ch.Slug == "02b-tail-hedging" && ch.Depth == 1 && ch.ParentSlug == "02-agent-workflow" && ch.NumberLabel == "2.2" {
			foundSub = true
		}
		if ch.Slug == "08-custody-mpc-rail" && ch.PartTitle == "Part III — Custody, Execution & Governance" && ch.Depth == 0 {
			foundPart3 = true
		}
	}
	if !foundSub || !foundPart3 {
		t.Fatalf("expected hierarchical sub-chapter (2.2) and Part III metadata in TOC, got: %+v", tocAfterParts.Chapters)
	}

	// 2. Create chapter whose InitialMarkdown already starts with "# "
	_, err = eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            "07-custom-h1",
		Title:           "Chapter 7: Custom H1",
		InitialMarkdown: "# Chapter 7: Custom H1\n\n```bash\n# Bash comment inside code fence\necho ok\n```\n\n## Section 7.1",
	})
	if err != nil {
		t.Fatalf("CreateChapter with existing H1: %v", err)
	}

	blame, err := eng.GetChapterBlame(ctx, &engine.GetChapterBlameInput{Slug: "07-custom-h1"})
	if err != nil {
		t.Fatalf("GetChapterBlame: %v", err)
	}
	// Should have 2 blame sections ("Chapter 7: Custom H1" and "Section 7.1"), NOT 3 (bash comment ignored)
	if len(blame.Sections) != 2 {
		t.Fatalf("expected 2 blame sections (bash comment inside fence ignored), got %+v", blame.Sections)
	}

	// Test GetCommitDiff on initial commit (0 parents) and full 40-char SHA
	changes, err := eng.ListRecentChanges(ctx, &engine.ListRecentChangesInput{Limit: 50})
	if err != nil || len(changes.Commits) == 0 {
		t.Fatalf("ListRecentChanges: %v", err)
	}
	initialCommit := changes.Commits[len(changes.Commits)-1]
	diffInit, err := eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		CommitSHA: initialCommit.FullSHA,
		Slug:      "00-preface",
	})
	if err != nil || diffInit.UnifiedDiff == "" {
		t.Fatalf("GetCommitDiff on initial commit failed: err=%v diff=%q", err, diffInit)
	}
	_ = out1
}

// UT-11: TestReopenExistingRepo_And_PeriodicSyncLoop
func TestReopenExistingRepo_And_PeriodicSyncLoop(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "reopen.db")
	repoDir := filepath.Join(tmpDir, "repo")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	eng1, err := engine.NewBookEngine(repoDir, st, "")
	if err != nil {
		t.Fatalf("first NewBookEngine: %v", err)
	}

	// Write an extra unindexed .md file in src/ to test extra-chapter discovery on reopen
	extraPath := filepath.Join(repoDir, "src", "90-appendix-extra.md")
	if err := os.WriteFile(extraPath, []byte("# Appendix Extra\n\nExtra content.\n"), 0o644); err != nil {
		t.Fatalf("write extra md: %v", err)
	}

	eng2, err := engine.NewBookEngine(repoDir, st, "")
	if err != nil {
		t.Fatalf("second NewBookEngine on existing repo: %v", err)
	}
	if _, err := eng2.ReadChapter(context.Background(), &engine.ReadChapterInput{Slug: "90-appendix-extra"}); err != nil {
		t.Fatalf("expected extra .md file to be discovered on reopen, got %v", err)
	}

	// Configure a bare remote and exercise StartPeriodicGitHubSync + triggerSync + GITHUB_PAT env
	bareDir := filepath.Join(tmpDir, "bare.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}
	if err := eng1.ConfigureRemote(bareDir); err != nil {
		t.Fatalf("ConfigureRemote: %v", err)
	}

	t.Setenv("GITHUB_PAT", "dummy-pat-for-auth-branch")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		eng1.StartPeriodicGitHubSync(ctx, 40*time.Millisecond)
		close(done)
	}()
	time.Sleep(90 * time.Millisecond)
	cancel()
	<-done
}

// UT-12: TestChapterPermalinkLineage_OutdatedRenamedMovedDeletedAndRestored
func TestChapterPermalinkLineage_OutdatedRenamedMovedDeletedAndRestored(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	alice, _ := st.ResolveAuthorName(ctx, "alice")
	ctxAlice := engine.WithAuthor(ctx, alice)

	// 1. Read chapter at r1 (current HEAD)
	r1Read, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug:     "03-editorial-lineage",
		Revision: "r1",
	})
	if err != nil {
		t.Fatalf("ReadChapter r1: %v", err)
	}
	if !r1Read.IsLatest || r1Read.LifecycleStatus != "current" || r1Read.RevisionNumber != 1 {
		t.Fatalf("expected r1 to be current/latest, got %+v", r1Read)
	}

	// 2. Patch chapter -> r1 permalink becomes "outdated" (1 commit behind)
	patchOut, err := eng.PatchChapter(ctxAlice, &engine.PatchChapterInput{
		Slug:          "03-editorial-lineage",
		OldText:       "## 2. Semantic Timeline Clustering",
		NewText:       "## 2. Semantic Editorial Timeline Clustering",
		CommitMessage: "Update section 2 heading in chapter 3",
	})
	if err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}

	r1AfterPatch, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug:     "03-editorial-lineage",
		Revision: "r1",
	})
	if err != nil {
		t.Fatalf("ReadChapter r1 after patch: %v", err)
	}
	if r1AfterPatch.IsLatest || r1AfterPatch.LifecycleStatus != "outdated" || r1AfterPatch.CommitsBehind != 1 {
		t.Fatalf("expected r1 to be outdated with 1 commit behind, got %+v", r1AfterPatch)
	}
	if r1AfterPatch.LatestRevisionSHA != patchOut.NewRevisionSHA {
		t.Fatalf("expected LatestRevisionSHA=%s, got %s", patchOut.NewRevisionSHA, r1AfterPatch.LatestRevisionSHA)
	}

	cvOutdated, err := eng.RenderChapterAtRevision(ctxAlice, "03-editorial-lineage", "r1", true)
	if err != nil || cvOutdated.DiffToLatest == "" {
		t.Fatalf("expected non-empty DiffToLatest on outdated permalink view, err=%v", err)
	}

	// 3. In-place title rename (same slug, new H1 title) -> r1 permalink becomes "renamed"
	renameTitleOut, err := eng.RenameChapter(ctxAlice, &engine.RenameChapterInput{
		OldSlug:       "03-editorial-lineage",
		NewTitle:      "Chapter 3: The Five Institutional Pillars of Ghostbook",
		CommitMessage: "Rename chapter 3 H1 title in place",
	})
	if err != nil {
		t.Fatalf("RenameChapter (title in place): %v", err)
	}
	r1AfterTitleRename, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug:     "03-editorial-lineage",
		Revision: "r1",
	})
	if err != nil {
		t.Fatalf("ReadChapter r1 after title rename: %v", err)
	}
	if r1AfterTitleRename.LifecycleStatus != "renamed" || r1AfterTitleRename.MovedToTitle != renameTitleOut.NewTitle {
		t.Fatalf("expected LifecycleStatus=renamed and MovedToTitle=%q, got %+v", renameTitleOut.NewTitle, r1AfterTitleRename)
	}

	// 4. Move/rename slug (03-editorial-lineage -> 03-five-institutional-pillars) -> old slug becomes "moved"
	moveSlugOut, err := eng.RenameChapter(ctxAlice, &engine.RenameChapterInput{
		OldSlug:       "03-editorial-lineage",
		NewSlug:       "03-five-institutional-pillars",
		NewTitle:      "Chapter 3: The Five Institutional Pillars",
		CommitMessage: "Move chapter 3 slug to 03-five-institutional-pillars",
	})
	if err != nil {
		t.Fatalf("RenameChapter (slug move): %v", err)
	}

	// Even without specifying Revision, reading the old slug resolves its archived snapshot and reports "moved"!
	oldSlugRead, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug: "03-editorial-lineage",
	})
	if err != nil {
		t.Fatalf("ReadChapter on moved old slug: %v", err)
	}
	if oldSlugRead.LifecycleStatus != "moved" || oldSlugRead.MovedToSlug != "03-five-institutional-pillars" {
		t.Fatalf("expected moved status pointing to 03-five-institutional-pillars, got %+v", oldSlugRead)
	}
	if oldSlugRead.LifecycleCommitSHA != moveSlugOut.NewRevisionSHA {
		t.Fatalf("expected LifecycleCommitSHA=%s, got %s", moveSlugOut.NewRevisionSHA, oldSlugRead.LifecycleCommitSHA)
	}

	// 5. Delete chapter with replaced_by_slug -> becomes "deleted"
	delOut, err := eng.DeleteChapter(ctxAlice, &engine.DeleteChapterInput{
		Slug:           "03-five-institutional-pillars",
		Reason:         "Consolidated into Chapter 2",
		ReplacedBySlug: "02-agent-workflow",
	})
	if err != nil {
		t.Fatalf("DeleteChapter: %v", err)
	}
	if delOut.ArchivedPermalinkURL == "" {
		t.Fatalf("expected non-empty ArchivedPermalinkURL")
	}

	deletedRead, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug: "03-five-institutional-pillars",
	})
	if err != nil {
		t.Fatalf("ReadChapter on deleted slug: %v", err)
	}
	if deletedRead.LifecycleStatus != "deleted" || deletedRead.MovedToSlug != "02-agent-workflow" {
		t.Fatalf("expected deleted status with replacement 02-agent-workflow, got %+v", deletedRead)
	}

	// 6. Restore the deleted chapter via RevertChapterRevision using its pre-delete revision SHA
	_, err = eng.RevertChapterRevision(ctxAlice, &engine.RevertChapterRevisionInput{
		Slug:            "03-five-institutional-pillars",
		TargetCommitSHA: moveSlugOut.NewRevisionSHA,
		Reason:          "Restore deleted chapter back to active book",
	})
	if err != nil {
		t.Fatalf("RevertChapterRevision on deleted chapter: %v", err)
	}
	restoredRead, err := eng.ReadChapter(ctxAlice, &engine.ReadChapterInput{
		Slug: "03-five-institutional-pillars",
	})
	if err != nil || !restoredRead.IsLatest || restoredRead.LifecycleStatus != "current" {
		t.Fatalf("expected restored chapter to be active and current, got %+v (err=%v)", restoredRead, err)
	}
}

// RT-03: TestRT03_FailClosedCloneOnUnreachableRemote_And_IdempotentSync
func TestRT03_FailClosedCloneOnUnreachableRemote_And_IdempotentSync(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "rt03_primary.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	defer func() { _ = st1.Close() }()

	// Configured unreachable remote on empty repoDir MUST fail closed instead of seeding fake test chapters
	badRepoDir := filepath.Join(tmpDir, "bad-clone-repo")
	if _, err := engine.NewBookEngine(badRepoDir, st1, "https://docs.example.com", "http://127.0.0.1:1/unreachable.git"); err == nil {
		t.Fatalf("expected NewBookEngine to fail closed when configured remote clone fails")
	}

	repoDir := filepath.Join(tmpDir, "repo")
	eng1, err := engine.NewBookEngine(repoDir, st1, "https://docs.example.com")
	if err != nil {
		t.Fatalf("NewBookEngine eng1: %v", err)
	}
	ctx := context.Background()
	tocBefore, err := eng1.GetTableOfContents(ctx, nil)
	if err != nil || tocBefore.HeadRevisionNumber != 1 {
		t.Fatalf("expected initial HeadRevisionNumber=1, got %+v (err=%v)", tocBefore, err)
	}
	if err := eng1.SyncWithRemote(ctx); err != nil {
		t.Fatalf("SyncWithRemote: %v", err)
	}
	tocAfter, err := eng1.GetTableOfContents(ctx, nil)
	if err != nil || tocAfter.HeadRevisionNumber != 1 || tocAfter.HeadCommitSHA != tocBefore.HeadCommitSHA {
		t.Fatalf("expected HeadRevisionNumber to remain 1, got %+v (err=%v)", tocAfter, err)
	}
}

// RT-04: TestRT04_OptimisticRevisionNumberLocking_And_FullChapterPatch
func TestRT04_OptimisticRevisionNumberLocking_And_FullChapterPatch(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	// 1. PatchChapter with ExpectedRevisionSHA: "r1" must succeed at r1
	patch1, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "01-system-architecture",
		OldText:             "embedded SQLite FTS5 search",
		NewText:             "embedded SQLite FTS5 BM25 search",
		CommitMessage:       "Test r1 optimistic revision lock",
		ExpectedRevisionSHA: "r1",
		AuthorName:          "Carol Park",
	})
	if err != nil {
		t.Fatalf("expected PatchChapter with ExpectedRevisionSHA='r1' to succeed at r1, got %v", err)
	}
	if patch1.NewRevisionSHA == "" {
		t.Fatalf("expected non-empty NewRevisionSHA")
	}

	// 2. PatchChapter with stale ExpectedRevisionSHA: "r1" must now fail with ErrRevisionConflict
	_, err = eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "01-system-architecture",
		OldText:             "embedded SQLite FTS5 BM25 search",
		NewText:             "SQLite FTS5",
		CommitMessage:       "Stale r1 lock must fail",
		ExpectedRevisionSHA: "r1",
	})
	if !errors.Is(err, engine.ErrRevisionConflict) {
		t.Fatalf("expected ErrRevisionConflict for stale ExpectedRevisionSHA='r1', got %v", err)
	}

	// 3. Full-chapter Markdown replacement (OldText: "*") with ExpectedRevisionSHA: "r2"
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "01-system-architecture",
		OldText:             "*",
		NewText:             "   ",
		CommitMessage:       "Empty full replacement must fail",
		ExpectedRevisionSHA: "r2",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for empty NewText on full-chapter replace, got %v", err)
	}

	fullOut, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "01-system-architecture",
		OldText:             "*",
		NewText:             "# Chapter 1: MFO Stack Teardown (Full Studio Edit)\n\nReplaced full chapter body cleanly via Studio Full-Markdown Editor.",
		CommitMessage:       "Full chapter replacement at r2",
		ExpectedRevisionSHA: "r2",
	})
	if err != nil {
		t.Fatalf("expected full-chapter PatchChapter with OldText='*' and ExpectedRevisionSHA='r2' to succeed, got %v", err)
	}
	readAfter, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "01-system-architecture"})
	if err != nil || readAfter.RevisionSHA != fullOut.NewRevisionSHA || !strings.Contains(readAfter.ContentMarkdown, "Full Studio Edit") {
		t.Fatalf("unexpected chapter after full-markdown patch: %+v (err=%v)", readAfter, err)
	}
}

func TestRemotePullAndPush_BidirectionalSync_And_StyleGuide(t *testing.T) {
	tmpDir := t.TempDir()
	bareDir := filepath.Join(tmpDir, "origin.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatalf("init bare repo: %v", err)
	}

	// Engine 1 initializes seed book and pushes to bare origin
	st1, err := store.Open(filepath.Join(tmpDir, "eng1.db"))
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	defer func() { _ = st1.Close() }()
	eng1, err := engine.NewBookEngine(filepath.Join(tmpDir, "repo1"), st1, "https://docs.example.com")
	if err != nil {
		t.Fatalf("NewBookEngine eng1: %v", err)
	}
	if eng1.Store() != st1 || eng1.BaseURL() != "https://docs.example.com" {
		t.Fatalf("unexpected Store() or BaseURL()")
	}
	if !strings.Contains(eng1.StyleGuide(), "Editorial Style Guide") {
		t.Fatalf("expected non-empty StyleGuide()")
	}
	if err := eng1.ConfigureRemote(bareDir); err != nil {
		t.Fatalf("ConfigureRemote eng1: %v", err)
	}
	ctx := context.Background()
	if err := eng1.SyncWithRemote(ctx); err != nil {
		t.Fatalf("eng1 initial SyncWithRemote: %v", err)
	}

	// Clone bareDir into repo2, open Engine 2, create a new chapter, and push to bare origin
	repo2Dir := filepath.Join(tmpDir, "repo2")
	if _, err := git.PlainClone(repo2Dir, false, &git.CloneOptions{URL: bareDir}); err != nil {
		t.Fatalf("PlainClone repo2: %v", err)
	}
	st2, err := store.Open(filepath.Join(tmpDir, "eng2.db"))
	if err != nil {
		t.Fatalf("open st2: %v", err)
	}
	defer func() { _ = st2.Close() }()
	eng2, err := engine.NewBookEngine(repo2Dir, st2, "https://docs.example.com")
	if err != nil {
		t.Fatalf("NewBookEngine eng2: %v", err)
	}
	_, err = eng2.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            "08-remote-synced-chapter",
		Title:           "Chapter 8: Remote Synced Chapter",
		InitialMarkdown: "Created on Engine 2 and pushed to origin.",
		AuthorName:      "Alice Chen",
	})
	if err != nil {
		t.Fatalf("eng2 CreateChapter: %v", err)
	}
	if err := eng2.SyncWithRemote(ctx); err != nil {
		t.Fatalf("eng2 SyncWithRemote push: %v", err)
	}

	// Now Engine 1 runs SyncWithRemote and pulls the new commit from origin
	if err := eng1.SyncWithRemote(ctx); err != nil {
		t.Fatalf("eng1 SyncWithRemote pull: %v", err)
	}
	pulledCh, err := eng1.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "08-remote-synced-chapter"})
	if err != nil || !strings.Contains(pulledCh.ContentMarkdown, "Created on Engine 2") {
		t.Fatalf("expected eng1 to have pulled 08-remote-synced-chapter from origin, got %+v (err=%v)", pulledCh, err)
	}
}

func TestLineageAndSummaryEdgeCases_TildeFences_And_CrossChapterLinks(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	// 1. Create a chapter without an H1 heading and with a ~~~ tilde fence containing # comment,
	// plus cross-links to 04-deployment-ops.
	_, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            "08-tilde-fence-test",
		Title:           "Chapter 8: Tilde Fence Test",
		PartTitle:       "Part IV — Appendices",
		InitialMarkdown: "# Chapter 8: Tilde Fence Test\n\n~~~python\n# Not a markdown heading\nprint('ok')\n~~~\n\nSee [Chapter 4](04-deployment-ops.md) and [Rel](./04-deployment-ops.md) and /chapters/04-deployment-ops.\n\n## Section 8.1: Details",
	})
	if err != nil {
		t.Fatalf("CreateChapter 08-tilde-fence-test: %v", err)
	}

	blame, err := eng.GetChapterBlame(ctx, &engine.GetChapterBlameInput{Slug: "08-tilde-fence-test"})
	if err != nil || len(blame.Sections) != 2 {
		t.Fatalf("expected 2 blame sections (ignoring # inside ~~~ fence), got %+v (err=%v)", blame, err)
	}

	// 2. Rename 04-deployment-ops -> 04-lockin-playbook and verify cross-chapter link rewriting in 08-tilde-fence-test
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug:  "04-deployment-ops",
		NewSlug:  "04-lockin-playbook",
		NewTitle: "Chapter 4: The Tool-First Lock-In Playbook",
	}); err != nil {
		t.Fatalf("RenameChapter 04: %v", err)
	}

	ch8, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "08-tilde-fence-test"})
	if err != nil {
		t.Fatalf("ReadChapter 08: %v", err)
	}
	if !strings.Contains(ch8.ContentMarkdown, "(04-lockin-playbook.md)") ||
		!strings.Contains(ch8.ContentMarkdown, "(./04-lockin-playbook.md)") ||
		!strings.Contains(ch8.ContentMarkdown, "/chapters/04-lockin-playbook") {
		t.Fatalf("expected cross-chapter links to be rewritten to 04-lockin-playbook, got:\n%s", ch8.ContentMarkdown)
	}

	// 3. GetCommitDiff between r1 and HEAD
	diffBetween, err := eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		BaseCommitSHA: "r1",
		CommitSHA:     "HEAD",
		Slug:          "08-tilde-fence-test",
	})
	if err != nil || diffBetween.UnifiedDiff == "" {
		t.Fatalf("GetCommitDiff between r1 and HEAD failed: %+v (err=%v)", diffBetween, err)
	}

	// 4. Error branches for RenameChapter, DeleteChapter, RevertChapterRevision, GetCommitDiff
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug: "04-lockin-playbook",
		NewSlug: "01-system-architecture",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when renaming to existing chapter slug, got %v", err)
	}
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug: "04-lockin-playbook",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when neither new_slug nor new_title is provided, got %v", err)
	}
	if _, err := eng.DeleteChapter(ctx, &engine.DeleteChapterInput{
		Slug: "nonexistent-chapter-xyz",
	}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("expected ErrNotFound when deleting nonexistent chapter, got %v", err)
	}
	if _, err := eng.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            "01-system-architecture",
		TargetCommitSHA: "HEAD",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument when reverting chapter to identical HEAD content, got %v", err)
	}
	if _, err := eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		CommitSHA:     "HEAD",
		BaseCommitSHA: "r99999",
	}); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for out-of-range BaseCommitSHA r99999, got %v", err)
	}

	// 5. Exercise no-H1 markdown chapter, unindexed SUMMARY rename, and SSH key env auth branch
	noH1Path := filepath.Join(t.TempDir(), "repo_noh1")
	stNoH1, _ := store.Open(filepath.Join(t.TempDir(), "noh1.db"))
	defer func() { _ = stNoH1.Close() }()
	engNoH1, err := engine.NewBookEngine(noH1Path, stNoH1, "")
	if err != nil {
		t.Fatalf("NewBookEngine engNoH1: %v", err)
	}
	// Overwrite a chapter file with no H1 heading and rename it
	rawNoH1 := filepath.Join(noH1Path, "src", "99-unlisted.md")
	if err := os.WriteFile(rawNoH1, []byte("~~~bash\n# inside fence\n~~~\n\nBody without H1.\n"), 0o644); err != nil {
		t.Fatalf("write 99-unlisted.md: %v", err)
	}
	if _, err := engNoH1.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug:  "99-unlisted",
		NewSlug:  "99-listed-now",
		NewTitle: "Chapter 99: Listed Now",
	}); err != nil {
		t.Fatalf("RenameChapter unlisted no-H1: %v", err)
	}
	t.Setenv("GITHUB_SSH_KEY_PATH", "/nonexistent/ssh/key")
	_ = engNoH1.SyncWithRemote(ctx)

	// 6. PatchChapter on SUMMARY, RenderChapterAtRevision live fast-path, DeleteChapter default reason, and periodic sync trigger
	if _, err := engNoH1.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:    "SUMMARY",
		OldText: "# Summary",
		NewText: "# Summary\n<!-- Master TOC -->",
	}); err != nil {
		t.Fatalf("PatchChapter on SUMMARY: %v", err)
	}
	if cv, err := engNoH1.RenderChapterAtRevision(ctx, "01-system-architecture", "", false); err != nil || cv == nil {
		t.Fatalf("RenderChapterAtRevision fast path: %v", err)
	}
	if _, err := engNoH1.DeleteChapter(ctx, &engine.DeleteChapterInput{
		Slug: "99-listed-now",
	}); err != nil {
		t.Fatalf("DeleteChapter default reason: %v", err)
	}

	// 7. Nil inputs, full-chapter wildcard replace, revert identical/no-H1, rename validation branches, and 40-char SHA lookup
	if _, err := engNoH1.PatchChapter(ctx, nil); err == nil {
		t.Fatalf("expected error for nil PatchChapter")
	}
	if _, err := engNoH1.CreateChapter(ctx, nil); err == nil {
		t.Fatalf("expected error for nil CreateChapter")
	}
	if _, err := engNoH1.RevertChapterRevision(ctx, nil); err == nil {
		t.Fatalf("expected error for nil RevertChapterRevision")
	}
	if _, err := engNoH1.RenameChapter(ctx, nil); err == nil {
		t.Fatalf("expected error for nil RenameChapter")
	}
	if _, err := engNoH1.DeleteChapter(ctx, nil); err == nil {
		t.Fatalf("expected error for nil DeleteChapter")
	}
	if _, err := engNoH1.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:    "01-system-architecture",
		OldText: "*",
		NewText: "   ",
	}); err == nil {
		t.Fatalf("expected error for empty wildcard PatchChapter")
	}
	patchOut, err := engNoH1.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:    "01-system-architecture",
		OldText: "*",
		NewText: "# Chapter 1: Replaced Full Body\n\nReplaced paragraph without trailing newline",
	})
	if err != nil {
		t.Fatalf("wildcard PatchChapter failed: %v", err)
	}
	// Revert to the current revision should fail with already identical
	if _, err := engNoH1.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            "01-system-architecture",
		TargetCommitSHA: patchOut.NewRevisionSHA,
	}); err == nil {
		t.Fatalf("expected error when reverting to already identical revision")
	}
	if _, err := engNoH1.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            "01-system-architecture",
		TargetCommitSHA: "",
	}); err == nil {
		t.Fatalf("expected error for empty target_commit_sha")
	}
	if _, err := engNoH1.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug: "01-system-architecture",
		NewSlug: "../invalid",
	}); err == nil {
		t.Fatalf("expected error for invalid new_slug")
	}
	if _, err := engNoH1.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug: "01-system-architecture",
		NewSlug: "00-preface",
	}); err == nil {
		t.Fatalf("expected error when renaming onto existing chapter")
	}
	// Rename unlisted no-H1 file without providing NewTitle (exercises effectiveTitle = newSlug)
	rawNoH1B := filepath.Join(noH1Path, "src", "98-noh1.md")
	_ = os.WriteFile(rawNoH1B, []byte("Plain body without H1.\n"), 0o644)
	renNoH1Out, err := engNoH1.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug: "98-noh1",
		NewSlug: "98-noh1-renamed",
	})
	if err != nil {
		t.Fatalf("RenameChapter no-H1 without NewTitle: %v", err)
	}
	_, _ = engNoH1.DeleteChapter(ctx, &engine.DeleteChapterInput{Slug: "98-noh1-renamed"})
	// Revert deleted no-H1 chapter (exercises restoredTitle = slug)
	if _, err := engNoH1.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            "98-noh1-renamed",
		TargetCommitSHA: renNoH1Out.NewRevisionSHA,
	}); err != nil {
		t.Fatalf("RevertChapterRevision no-H1: %v", err)
	}
	// ListRecentChanges with full 40-char SHA diff, invalid slug, and ListAgentActivity default limit
	changes, err := engNoH1.ListRecentChanges(ctx, &engine.ListRecentChangesInput{})
	if err != nil || len(changes.Commits) == 0 {
		t.Fatalf("ListRecentChanges default limit: %v", err)
	}
	if _, err := engNoH1.GetCommitDiff(ctx, &engine.GetCommitDiffInput{CommitSHA: changes.Commits[0].FullSHA}); err != nil {
		t.Fatalf("GetCommitDiff 40-char SHA: %v", err)
	}
	if _, err := engNoH1.RenderChapterAtRevision(ctx, "01-system-architecture", "r999", false); err == nil {
		t.Fatalf("expected error for out-of-range r999")
	}
	if _, err := engNoH1.ListRecentChanges(ctx, &engine.ListRecentChangesInput{Slug: "../invalid", Limit: 5}); err == nil {
		t.Fatalf("expected error for invalid slug in ListRecentChanges")
	}
	_, _ = engNoH1.ListAgentActivity(ctx, &engine.ListAgentActivityInput{Limit: 0})

	syncCtx, cancelSync := context.WithCancel(ctx)
	syncDone := make(chan struct{})
	go func() {
		engNoH1.StartPeriodicGitHubSync(syncCtx, 0)
		close(syncDone)
	}()
	_, _ = engNoH1.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:    "SUMMARY",
		OldText: "<!-- Master TOC -->",
		NewText: "<!-- Updated Master TOC -->",
	})
	time.Sleep(300 * time.Millisecond)
	cancelSync()
	<-syncDone
}

func TestMultiHunkPatch_AutoSummaryH1Sync_MetaRead_And_StyleWarnings(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	// 1. ReadChapter on SUMMARY and STYLE_GUIDE
	sumRead, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "SUMMARY"})
	if err != nil || !strings.Contains(sumRead.ContentMarkdown, "01-system-architecture.md") {
		t.Fatalf("ReadChapter(SUMMARY) failed: %+v (err=%v)", sumRead, err)
	}
	styleRead, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "style_guide"})
	if err != nil || !strings.Contains(styleRead.ContentMarkdown, "Concise, Single-Line Titles") {
		t.Fatalf("ReadChapter(style_guide) failed: %+v (err=%v)", styleRead, err)
	}

	// 2. Multi-hunk Patches + H1 change auto-syncing SUMMARY.md + StyleWarnings
	patchOut, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug: "01-system-architecture",
		Patches: []engine.ChapterPatchHunk{
			{
				OldText: "# Chapter 1: System Architecture",
				NewText: "# Chapter 1: Core Architecture Analysis",
			},
			{
				OldText: "## 1. Core Components",
				NewText: "## 1. An Extremely Long Verbose Section Heading That Exceeds Forty Two Characters Easily\n\nThis paragraph includes an em dash — inside prose.",
			},
		},
		CommitMessage: "Test multi-hunk atomic patch, H1 SUMMARY sync, and style warnings",
	})
	if err != nil {
		t.Fatalf("Multi-hunk PatchChapter failed: %v", err)
	}
	// Verify SUMMARY.md was automatically updated with the new H1 title
	toc, err := eng.GetTableOfContents(ctx, &engine.GetTableOfContentsInput{})
	if err != nil || !strings.Contains(toc.SummaryMarkdown, "[Chapter 1: Core Architecture Analysis](01-system-architecture.md)") {
		t.Fatalf("expected SUMMARY.md to auto-sync with new H1 title, got:\n%s", toc.SummaryMarkdown)
	}
	// Verify Rule 1 heading length + Rule 5 em dash warnings were returned
	if len(patchOut.StyleWarnings) != 2 {
		t.Fatalf("expected 2 style warnings (Rule 1, Rule 5), got %+v", patchOut.StyleWarnings)
	}
	// Verify optional configured banned-word linting (Rule 6)
	bannedWarnings := engine.LintChapterMarkdown("01-system-architecture", "# Short\n\nAvoid synergy here.", []string{"synergy"})
	if len(bannedWarnings) != 1 {
		t.Fatalf("expected 1 banned-word warning, got %+v", bannedWarnings)
	}

	// Verify empty old_text in Patches returns ErrInvalidArgument
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:    "01-system-architecture",
		Patches: []engine.ChapterPatchHunk{{OldText: "   ", NewText: "x"}},
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for empty old_text in Patches, got %v", err)
	}

	// Verify AlertNotifier integration on BookEngine
	eng.SetAlertNotifier(store.NewAlertNotifier("", "", "", 0))
	eng.NotifyAlert(ctx, "test_cat", "summary", "details")
}
