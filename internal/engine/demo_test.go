package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

func TestSeedDemoBook_PopulatesRichLineageTimelineAndIdempotency(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "book_studio.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	eng, err := engine.NewBookEngineWithConfig(filepath.Join(tmp, "repo"), st, "http://127.0.0.1:8090", engine.EngineConfig{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	res, err := engine.SeedDemoBook(ctx, eng)
	if err != nil {
		t.Fatalf("SeedDemoBook: %v", err)
	}
	if !res.Seeded {
		t.Fatalf("expected Seeded=true on fresh engine")
	}
	if len(res.Authors) != 3 {
		t.Fatalf("expected 3 demo authors, got %d", len(res.Authors))
	}
	if res.HeadRevision < 10 {
		t.Fatalf("expected >= 10 demo revisions, got r%d", res.HeadRevision)
	}

	snap := eng.CurrentSnapshot()
	if snap.BookTitle != "Atlas Systems Handbook" {
		t.Fatalf("expected book.json title 'Atlas Systems Handbook', got %q", snap.BookTitle)
	}
	if !snap.BookConfig.FromRepoFile {
		t.Fatalf("expected BookConfig.FromRepoFile=true")
	}
	if len(snap.TimelineEntries) != 4 {
		t.Fatalf("expected 4 demo timeline milestones, got %d", len(snap.TimelineEntries))
	}
	if len(snap.OpenCallouts) < 3 {
		t.Fatalf("expected >= 3 open callouts on demo timeline, got %d", len(snap.OpenCallouts))
	}
	if len(snap.RecentActivity) < 10 {
		t.Fatalf("expected >= 10 seeded audit entries, got %d", len(snap.RecentActivity))
	}
	if _, ok := snap.BySlug["03-lineage-and-audit"]; !ok {
		t.Fatalf("expected renamed chapter 03-lineage-and-audit in snapshot")
	}
	if ch, ok := snap.BySlug["01a-failover-runbook"]; !ok || ch.Depth != 1 {
		t.Fatalf("expected nested chapter 01a-failover-runbook at depth 1, got ok=%v ch=%+v", ok, ch)
	}

	// Verify lint cleanliness across all active demo chapters
	for _, ch := range snap.Chapters {
		if warns := engine.LintChapterMarkdownWithConfig(ch.Slug, ch.Markdown, eng.LintConfig()); len(warns) > 0 {
			t.Fatalf("demo chapter %s failed style lint: %v", ch.Slug, warns)
		}
	}

	// Verify outdated permalink, renamed redirect, and deleted draft lineage all resolve cleanly
	outdated, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: res.OutdatedChapterSlug, Revision: res.OutdatedRevision})
	if err != nil || outdated.LifecycleStatus != "outdated" {
		t.Fatalf("expected outdated lifecycle for %s@%s, got status=%q err=%v", res.OutdatedChapterSlug, res.OutdatedRevision, outdated.LifecycleStatus, err)
	}
	moved, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: res.RenamedFromSlug})
	if err != nil || moved.LifecycleStatus != "moved" || moved.MovedToSlug != res.RenamedToSlug {
		t.Fatalf("expected moved lifecycle for %s -> %s, got %+v err=%v", res.RenamedFromSlug, res.RenamedToSlug, moved, err)
	}
	deleted, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: res.RetiredDraftSlug})
	if err != nil || deleted.LifecycleStatus != "deleted" || deleted.MovedToSlug != "02-agent-workflow" {
		t.Fatalf("expected deleted lifecycle for %s -> 02-agent-workflow, got %+v err=%v", res.RetiredDraftSlug, deleted, err)
	}

	// Verify multi-author Git blame on Chapter 1
	blame, err := eng.GetChapterBlame(ctx, &engine.GetChapterBlameInput{Slug: "01-system-architecture"})
	if err != nil {
		t.Fatalf("GetChapterBlame: %v", err)
	}
	authorsSeen := map[string]bool{}
	for _, sec := range blame.Sections {
		authorsSeen[sec.LastAuthor] = true
	}
	if !authorsSeen["Alice Chen"] || !authorsSeen["Bob Rivera"] {
		t.Fatalf("expected multi-author blame (Alice Chen & Bob Rivera) on Chapter 1, got %v", authorsSeen)
	}

	// Verify idempotency when calling SeedDemoBook a second time on the same engine
	res2, err := engine.SeedDemoBook(ctx, eng)
	if err != nil {
		t.Fatalf("second SeedDemoBook: %v", err)
	}
	if res2.Seeded {
		t.Fatalf("expected Seeded=false when resuming already-seeded demo repo")
	}
	if res2.HeadRevision != res.HeadRevision || !strings.EqualFold(res2.HeadSHA, res.HeadSHA) {
		t.Fatalf("expected head revision unchanged on re-seed: got r%d (%s), want r%d (%s)", res2.HeadRevision, res2.HeadSHA, res.HeadRevision, res.HeadSHA)
	}
}

func TestComputeChapterBlame_SameSecondSequentialCommits(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "book_studio.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SyncConfiguredAuthors(ctx, store.DefaultDemoAuthors); err != nil {
		t.Fatalf("sync authors: %v", err)
	}
	alice, _ := st.ResolveAuthorName(ctx, "alice")
	bob, _ := st.ResolveAuthorName(ctx, "bob")

	eng, err := engine.NewBookEngineWithConfig(filepath.Join(tmp, "repo"), st, "http://127.0.0.1:8090", engine.EngineConfig{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	// Perform two rapid back-to-back patches in the same wall-clock second by different authors.
	if _, err := eng.PatchChapter(engine.WithAuthor(ctx, alice), &engine.PatchChapterInput{
		Slug:    "01-system-architecture",
		OldText: "Ghostbook bundles pure-Go Git storage",
		NewText: "Ghostbook bundles embedded pure-Go Git storage",
	}); err != nil {
		t.Fatalf("alice patch: %v", err)
	}
	if _, err := eng.PatchChapter(engine.WithAuthor(ctx, bob), &engine.PatchChapterInput{
		Slug:    "01-system-architecture",
		OldText: "Full-text indexing combines SQLite FTS5 BM25 ranking",
		NewText: "Full-text indexing combines embedded SQLite FTS5 BM25 ranking",
	}); err != nil {
		t.Fatalf("bob patch: %v", err)
	}

	blame, err := eng.GetChapterBlame(ctx, &engine.GetChapterBlameInput{Slug: "01-system-architecture"})
	if err != nil {
		t.Fatalf("GetChapterBlame: %v", err)
	}
	sectionOwner := map[string]string{}
	for _, s := range blame.Sections {
		sectionOwner[s.Heading] = s.LastAuthor
	}
	if sectionOwner["1. Core Components"] != "Alice Chen" {
		t.Fatalf("expected section 1 owned by Alice Chen, got %+v", sectionOwner)
	}
	if sectionOwner["2. Search & Indexing"] != "Bob Rivera" {
		t.Fatalf("expected section 2 owned by Bob Rivera, got %+v", sectionOwner)
	}
}
