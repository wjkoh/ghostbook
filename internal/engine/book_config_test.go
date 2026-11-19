package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

func TestBookConfig_ParseValidateAndPrecedence_UT_CFG_01_02(t *testing.T) {
	base := engine.EngineConfig{
		BookTitle:         "Server Default Title",
		BookKicker:        "Server Default Kicker",
		HidePoweredBy:     true,
		BannedWords:       []string{"legacy"},
		ExtraLLMsMarkdown: "- [Server Link](https://docs.example.com/server)",
	}

	// Omitted file inherits all server defaults
	effDefault, warn := engine.ResolveEffectiveBookConfig(base, nil)
	if warn != "" || effDefault.FromRepoFile {
		t.Fatalf("unexpected default resolve: eff=%+v warn=%q", effDefault, warn)
	}
	if effDefault.BookTitle != "Server Default Title" || effDefault.BookKicker != "Server Default Kicker" ||
		!effDefault.HidePoweredBy || effDefault.Lint.MaxHeadingChars != engine.DefaultMaxHeadingChars ||
		!effDefault.Lint.ForbidEmDash || len(effDefault.Lint.BannedWords) != 1 || effDefault.Lint.BannedWords[0] != "legacy" {
		t.Fatalf("unexpected default effective config: %+v", effDefault)
	}

	// Explicit false, 0, empty slice [], and empty string "" override server defaults cleanly
	overrideJSON := []byte(`{
		"title": "Repo Custom Book",
		"hide_powered_by": false,
		"lint": {
			"max_heading_chars": 0,
			"forbid_em_dash": false,
			"banned_words": []
		},
		"timeline": {
			"prompt_guidance": "Use concrete distributed systems terms."
		},
		"extra_llms_markdown": ""
	}`)
	eff, warn := engine.ResolveEffectiveBookConfig(base, overrideJSON)
	if warn != "" || !eff.FromRepoFile {
		t.Fatalf("unexpected override resolve: eff=%+v warn=%q", eff, warn)
	}
	if eff.BookTitle != "Repo Custom Book" || eff.BookKicker != "Server Default Kicker" {
		t.Fatalf("expected title overridden and omitted kicker inherited: %+v", eff)
	}
	if eff.HidePoweredBy || eff.Lint.MaxHeadingChars != 0 || eff.Lint.ForbidEmDash || len(eff.Lint.BannedWords) != 0 {
		t.Fatalf("expected explicit zero/false/[] overrides to win: %+v", eff)
	}
	if eff.TimelineGuidance != "Use concrete distributed systems terms." || eff.ExtraLLMsMarkdown != "" {
		t.Fatalf("unexpected timeline/extra_llms override: %+v", eff)
	}

	// UT-CFG-02: Strict validation rejections
	tooBig := []byte(`{"extra_llms_markdown":"` + strings.Repeat("a", engine.MaxBookConfigBytes+10) + `"}`)
	tooManyBanned := make([]string, 0, engine.MaxBannedWordsCount+1)
	for i := 0; i <= engine.MaxBannedWordsCount; i++ {
		tooManyBanned = append(tooManyBanned, strings.Repeat("w", 4)+string(rune('a'+(i%26)))+string(rune('0'+(i/26))))
	}
	tooManyBannedJSON, _ := json.Marshal(map[string]any{"lint": map[string]any{"banned_words": tooManyBanned}})

	badPayloads := []struct {
		name string
		raw  []byte
	}{
		{"unknown_key", []byte(`{"unknown_field": true}`)},
		{"malformed_json", []byte(`{"title": "Unclosed`)},
		{"not_object", []byte(`["array"]`)},
		{"trailing_tokens", []byte(`{"title":"Ok"}{"extra":1}`)},
		{"oversize", tooBig},
		{"negative_heading", []byte(`{"lint":{"max_heading_chars":-1}}`)},
		{"excessive_heading", []byte(`{"lint":{"max_heading_chars":201}}`)},
		{"multiline_title", []byte(`{"title":"Line1\nLine2"}`)},
		{"multiline_kicker", []byte(`{"kicker":"Line1\r\nLine2"}`)},
		{"too_many_banned", tooManyBannedJSON},
	}
	for _, tc := range badPayloads {
		if _, err := engine.ParseAndValidateBookConfig(tc.raw); !errors.Is(err, engine.ErrInvalidArgument) {
			t.Fatalf("case %s: expected ErrInvalidArgument, got %v", tc.name, err)
		}
	}
}

func TestBookConfig_FailSafeAndRepair_UT_CFG_03_06(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "studio.db")
	repoDir := filepath.Join(tmp, "repo")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.UpsertAuthor(ctx, store.AuthorConfig{ID: "alice", DisplayName: "Alice Chen", Email: "alice@example.com"}); err != nil {
		t.Fatalf("UpsertAuthor: %v", err)
	}

	eng, err := engine.NewBookEngineWithConfig(repoDir, st, "https://docs.example.com", engine.EngineConfig{
		BookTitle:   "Fallback Title",
		BannedWords: []string{"synergy"},
	})
	if err != nil {
		t.Fatalf("NewBookEngineWithConfig: %v", err)
	}

	// Simulate an external git push introducing a malformed src/book.json directly into Git
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("git.PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	badJSON := "{\n  \"title\": \"Broken Book\"\n  \"kicker\": \"Missing Comma\"\n}\n"
	if err := os.WriteFile(filepath.Join(repoDir, "src", "book.json"), []byte(badJSON), 0o644); err != nil {
		t.Fatalf("write malformed book.json: %v", err)
	}
	if _, err := wt.Add("src/book.json"); err != nil {
		t.Fatalf("wt.Add: %v", err)
	}
	if _, err := wt.Commit("External push with broken book.json", &git.CommitOptions{
		Author: &object.Signature{Name: "External", Email: "ext@example.com", When: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("wt.Commit: %v", err)
	}

	// Reopening engine with malformed src/book.json MUST succeed safely and expose ConfigWarning
	eng2, err := engine.NewBookEngineWithConfig(repoDir, st, "https://docs.example.com", engine.EngineConfig{
		BookTitle:   "Fallback Title",
		BannedWords: []string{"synergy"},
	})
	if err != nil {
		t.Fatalf("expected fail-safe startup on malformed src/book.json, got %v", err)
	}
	if eng2.BookTitle() != "Fallback Title" {
		t.Fatalf("expected baseline fallback title, got %q", eng2.BookTitle())
	}
	if eng2.CurrentSnapshot().BookConfig.ConfigWarning == "" {
		t.Fatalf("expected non-empty BookConfig.ConfigWarning on malformed src/book.json")
	}

	// ReadChapter(BOOK_CONFIG) must return the verbatim corrupted bytes so an agent can repair it
	readCorrupt, err := eng2.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "BOOK_CONFIG"})
	if err != nil {
		t.Fatalf("ReadChapter(BOOK_CONFIG) on corrupt file: %v", err)
	}
	if readCorrupt.ContentMarkdown != badJSON {
		t.Fatalf("expected verbatim corrupt bytes from ReadChapter, got:\n%s", readCorrupt.ContentMarkdown)
	}

	// Invalid MCP patch must fail cleanly without changing HEAD
	headBefore := eng2.CurrentSnapshot().HeadCommitSHA
	if _, err := eng2.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "BOOK_CONFIG",
		OldText:       "*",
		NewText:       `{"still_invalid": true}`,
		CommitMessage: "Invalid unknown key",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument on invalid patch, got %v", err)
	}
	if eng2.CurrentSnapshot().HeadCommitSHA != headBefore {
		t.Fatalf("expected HEAD unchanged after rejected patch")
	}

	// Valid repair patch restores clean config and clears ConfigWarning immediately
	repaired, err := eng2.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "BOOK_CONFIG",
		ExpectedRevisionSHA: readCorrupt.RevisionSHA,
		OldText:             "\"title\": \"Broken Book\"\n  \"kicker\": \"Missing Comma\"",
		NewText:             "\"title\": \"Repaired Book\",\n  \"kicker\": \"Clean Kicker\"",
		CommitMessage:       "Repair malformed src/book.json",
	})
	if err != nil {
		t.Fatalf("repair PatchChapter(BOOK_CONFIG): %v", err)
	}
	if repaired.LiveChapterURL != "https://docs.example.com/book.json" {
		t.Fatalf("unexpected LiveChapterURL: %q", repaired.LiveChapterURL)
	}
	if eng2.BookTitle() != "Repaired Book" || eng2.BookKicker() != "Clean Kicker" {
		t.Fatalf("expected hot-reloaded title/kicker, got %q / %q", eng2.BookTitle(), eng2.BookKicker())
	}
	if eng2.CurrentSnapshot().BookConfig.ConfigWarning != "" || !eng2.CurrentSnapshot().BookConfig.FromRepoFile {
		t.Fatalf("expected ConfigWarning cleared and FromRepoFile=true, got %+v", eng2.CurrentSnapshot().BookConfig)
	}
	_ = eng
}

func TestBookConfig_FirstTimeBootstrapLintAndRevert_UT_CFG_04_05_07(t *testing.T) {
	ctx := context.Background()
	eng, _ := setupTestEngine(t)

	// UT-CFG-04: LintChapterMarkdownWithConfig respects toggles and backward-compatible wrapper
	if w := engine.LintChapterMarkdownWithConfig("01-system-architecture", "# A Very Long Heading That Exceeds Forty Two Characters Easily\n\nNarrative — dash with customword.", engine.EffectiveLintConfig{
		MaxHeadingChars: 0,
		ForbidEmDash:    false,
		BannedWords:     []string{"customword"},
	}); len(w) != 1 || !strings.Contains(w[0], "customword") {
		t.Fatalf("expected only customword warning when heading/em-dash disabled, got %+v", w)
	}
	if w := engine.LintChapterMarkdownWithConfig("BOOK_CONFIG", `{"title":"Long — text"}`, engine.EffectiveLintConfig{
		MaxHeadingChars: 5,
		ForbidEmDash:    true,
		BannedWords:     []string{"title"},
	}); len(w) != 0 {
		t.Fatalf("expected BOOK_CONFIG skipped from prose linting, got %+v", w)
	}

	// UT-CFG-05: First-time ReadChapter synthesizes JSON when src/book.json is absent
	initRead, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "BOOK_CONFIG"})
	if err != nil {
		t.Fatalf("ReadChapter(BOOK_CONFIG): %v", err)
	}
	if initRead.LiveURL != "https://docs.example.com/book.json" || !strings.Contains(initRead.ContentMarkdown, `"max_heading_chars": 42`) {
		t.Fatalf("unexpected synthesized BOOK_CONFIG output: %+v", initRead)
	}

	// First-time PatchChapter creates src/book.json with HEAD optimistic lock check
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "BOOK_CONFIG",
		ExpectedRevisionSHA: "deadbeef",
		OldText:             `"forbid_em_dash": true`,
		NewText:             `"forbid_em_dash": false`,
		CommitMessage:       "Wrong lock should fail",
	}); !errors.Is(err, engine.ErrRevisionConflict) {
		t.Fatalf("expected ErrRevisionConflict on stale HEAD lock for first-time BOOK_CONFIG, got %v", err)
	}

	firstPatch, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                "BOOK_CONFIG",
		ExpectedRevisionSHA: initRead.RevisionSHA,
		Patches: []engine.ChapterPatchHunk{
			{OldText: `"title": "Ghostbook"`, NewText: `"title": "Storage Systems Guide"`},
			{OldText: `"hide_powered_by": false`, NewText: `"hide_powered_by": true`},
			{OldText: `"forbid_em_dash": true`, NewText: `"forbid_em_dash": false`},
			{OldText: `"banned_words": []`, NewText: `"banned_words": ["deprecated-v0"]`},
		},
		CommitMessage: "Create src/book.json with custom title and lint rules",
	})
	if err != nil {
		t.Fatalf("first-time PatchChapter(BOOK_CONFIG): %v", err)
	}
	if !strings.Contains(firstPatch.DiffPreview, "--- a/src/book.json\n+++ b/src/book.json") {
		t.Fatalf("expected src/book.json diff header, got:\n%s", firstPatch.DiffPreview)
	}
	if firstPatch.LiveChapterURL != "https://docs.example.com/book.json" {
		t.Fatalf("unexpected LiveChapterURL: %q", firstPatch.LiveChapterURL)
	}
	if eng.BookTitle() != "Storage Systems Guide" || !eng.HidePoweredBy() || eng.LintConfig().ForbidEmDash {
		t.Fatalf("expected immediate hot-reload after first-time patch: %+v", eng.EffectiveConfig())
	}

	// Chapter patch now honors dynamic book.json lint rules (em-dash allowed, deprecated-v0 flagged)
	chPatch, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "Goldmark SSR rendering",
		NewText:       "Goldmark SSR — deprecated-v0 rendering",
		CommitMessage: "Test dynamic book.json lint",
	})
	if err != nil {
		t.Fatalf("PatchChapter after book.json update: %v", err)
	}
	if len(chPatch.StyleWarnings) != 1 || !strings.Contains(chPatch.StyleWarnings[0], "deprecated-v0") {
		t.Fatalf("expected only deprecated-v0 warning (em-dash disabled), got %+v", chPatch.StyleWarnings)
	}

	// UT-CFG-07: DeleteChapter & RenameChapter reject BOOK_CONFIG; RevertChapterRevision works
	if _, err := eng.DeleteChapter(ctx, &engine.DeleteChapterInput{Slug: "BOOK_CONFIG"}); !errors.Is(err, engine.ErrInvalidSlug) {
		t.Fatalf("expected ErrInvalidSlug deleting BOOK_CONFIG, got %v", err)
	}
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{OldSlug: "BOOK_CONFIG", NewSlug: "99-other"}); !errors.Is(err, engine.ErrInvalidSlug) {
		t.Fatalf("expected ErrInvalidSlug renaming BOOK_CONFIG, got %v", err)
	}

	secondPatch, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "BOOK_CONFIG",
		OldText:       `"title": "Storage Systems Guide"`,
		NewText:       `"title": "Storage Systems Guide v2"`,
		CommitMessage: "Bump title to v2",
	})
	if err != nil {
		t.Fatalf("second PatchChapter(BOOK_CONFIG): %v", err)
	}
	if eng.BookTitle() != "Storage Systems Guide v2" || secondPatch.NewRevisionSHA == "" {
		t.Fatalf("expected v2 title")
	}

	revOut, err := eng.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            "BOOK_CONFIG",
		TargetCommitSHA: firstPatch.NewRevisionSHA,
		Reason:          "Revert book title back to v1",
	})
	if err != nil {
		t.Fatalf("RevertChapterRevision(BOOK_CONFIG): %v", err)
	}
	if revOut.LiveChapterURL != "https://docs.example.com/book.json" || eng.BookTitle() != "Storage Systems Guide" {
		t.Fatalf("unexpected revert outcome: out=%+v title=%q", revOut, eng.BookTitle())
	}
}
