package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wjkoh/ghostbook/internal/store"
)

var testMailConfig = store.MailConfig{
	BookTitle:  "Ghostbook",
	From:       "Ghostbook <ghostbook@example.com>",
	ReplyTo:    "Alice Chen <alice@example.com>",
	Recipients: []string{"Alice Chen <alice@example.com>", "Ops Bot <ops@example.com>"},
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "store_unit_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	if err := st.SyncConfiguredAuthors(context.Background(), store.DefaultDemoAuthors); err != nil {
		_ = st.Close()
		t.Fatalf("sync demo authors: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpen_InvalidPathAndEmpty(t *testing.T) {
	if _, err := store.Open("   "); err == nil {
		t.Fatalf("expected error for empty dbPath")
	}
	if _, err := store.Open("/nonexistent-root-dir-xyz/impossible/book.db"); err == nil {
		t.Fatalf("expected error for unwritable directory")
	}
}

func TestLoadAuthorsFile_And_UpsertAuthor(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "authors.json")
	if err := os.WriteFile(p, []byte(`[{"id":"dana","display_name":"Dana Lin","email":"dana@example.com","role_badge":"Editor","aliases":["dana.lin@example.com"]}]`), 0o600); err != nil {
		t.Fatalf("write authors.json: %v", err)
	}
	loaded, err := store.LoadAuthorsFile(p)
	if err != nil || len(loaded) != 1 || loaded[0].ID != "dana" {
		t.Fatalf("LoadAuthorsFile: %+v (err=%v)", loaded, err)
	}
	st := openTestStore(t)
	created, err := st.UpsertAuthor(ctx, loaded[0])
	if err != nil || len(created.SecretKey) != 16 {
		t.Fatalf("UpsertAuthor: %+v (err=%v)", created, err)
	}
	// Re-upserting must preserve existing secret key
	updated, err := st.UpsertAuthor(ctx, store.AuthorConfig{
		ID:          "dana",
		DisplayName: "Dana Lin-Updated",
		Email:       "dana@example.com",
		Badge:       "Lead Editor",
	})
	if err != nil || updated.SecretKey != created.SecretKey || updated.DisplayName != "Dana Lin-Updated" {
		t.Fatalf("UpsertAuthor preserve key: %+v (err=%v)", updated, err)
	}
}

func TestAuthenticate_SecretKeyEnforcementAndRevocation_UT_SSR_01(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// 1. Empty token or secretless bare emails/aliases -> strictly ErrUnauthorized
	for _, bare := range []string{
		"   ",
		"alice@example.com",
		"ALICE@EXAMPLE.COM",
		"bob@example.com",
		"carol@example.com",
		"carol",
		"ghostbook-token-alice",
		"stranger@example.com",
	} {
		if _, err := st.Authenticate(ctx, bare); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for secretless token %q, got %v", bare, err)
		}
	}

	authors, err := st.ListAuthors(ctx)
	if err != nil || len(authors) != 3 {
		t.Fatalf("ListAuthors: %+v (err=%v)", authors, err)
	}
	byID := map[string]store.Author{}
	for _, a := range authors {
		byID[a.ID] = a
	}

	// 2. Valid <email>:<secret>, <id>:<secret>, and <alias>:<secret> succeed (case-insensitive)
	cases := []struct {
		token      string
		expectedID string
	}{
		{byID["alice"].AuthIdentifier(), "alice"},
		{strings.ToUpper(byID["alice"].AuthIdentifier()), "alice"},
		{"alice:" + byID["alice"].SecretKey, "alice"},
		{"ghostbook-token-alice:" + byID["alice"].SecretKey, "alice"},
		{byID["bob"].AuthIdentifier(), "bob"},
		{"bob:" + byID["bob"].SecretKey, "bob"},
		{"ghostbook-token-bob:" + byID["bob"].SecretKey, "bob"},
		{byID["carol"].AuthIdentifier(), "carol"},
		{"carol:" + byID["carol"].SecretKey, "carol"},
		{"ghostbook-token-carol:" + byID["carol"].SecretKey, "carol"},
	}
	for _, tc := range cases {
		a, err := st.Authenticate(ctx, tc.token)
		if err != nil {
			t.Fatalf("Authenticate(%q) failed: %v", tc.token, err)
		}
		if a.ID != tc.expectedID {
			t.Fatalf("Authenticate(%q) got ID %q, want %q", tc.token, a.ID, tc.expectedID)
		}
	}

	// 3. Revoke non-existent token -> ErrNotFound
	if err := st.RevokeToken(ctx, "nonexistent-token-xyz"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound when revoking unknown token, got %v", err)
	}

	// 4. Revoke Bob's <email>:<secret> token -> Authenticate and ResolveAuthorName return ErrTokenRevoked across all aliases & whitespace variants
	bobCred := byID["bob"].AuthIdentifier()
	if err := st.RevokeToken(ctx, bobCred); err != nil {
		t.Fatalf("revoke %s: %v", bobCred, err)
	}
	for _, revokedVariant := range []string{
		bobCred,
		byID["bob"].GitEmail + ": " + byID["bob"].SecretKey,
		"bob:" + byID["bob"].SecretKey,
		"ghostbook-token-bob:" + byID["bob"].SecretKey,
	} {
		if _, err := st.Authenticate(ctx, revokedVariant); !errors.Is(err, store.ErrTokenRevoked) {
			t.Fatalf("expected ErrTokenRevoked on Authenticate(%q), got %v", revokedVariant, err)
		}
	}
	if _, err := st.ResolveAuthorName(ctx, bobCred); !errors.Is(err, store.ErrTokenRevoked) {
		t.Fatalf("expected ErrTokenRevoked on ResolveAuthorName for revoked credential, got %v", err)
	}
	// Verify Author JSON serialization omits SecretKey
	marshaledAuthor, err := json.Marshal(byID["alice"])
	if err != nil {
		t.Fatalf("json.Marshal(author): %v", err)
	}
	if strings.Contains(string(marshaledAuthor), byID["alice"].SecretKey) || strings.Contains(string(marshaledAuthor), "secret_key") {
		t.Fatalf("expected Author JSON to omit secret_key, got %s", string(marshaledAuthor))
	}
}

func TestResolveAuthorName_And_ListAuthors(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	authors, err := st.ListAuthors(ctx)
	if err != nil {
		t.Fatalf("ListAuthors: %v", err)
	}
	if len(authors) != 3 {
		t.Fatalf("expected 3 demo authors, got %d", len(authors))
	}

	if _, err := st.ResolveAuthorName(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for empty name, got %v", err)
	}
	if _, err := st.ResolveAuthorName(ctx, "Unknown Person"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for Unknown Person, got %v", err)
	}

	byDisplay, err := st.ResolveAuthorName(ctx, "Bob Rivera")
	if err != nil || byDisplay.ID != "bob" {
		t.Fatalf("ResolveAuthorName('Bob Rivera') = %+v, err=%v", byDisplay, err)
	}

	byAlias, err := st.ResolveAuthorName(ctx, "ghostbook-token-carol")
	if err != nil || byAlias.ID != "carol" {
		t.Fatalf("ResolveAuthorName alias = %+v, err=%v", byAlias, err)
	}
}

func TestFTS5_EnglishUnicodeAndSanitization(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.IndexChapterFTS(ctx, "01-architecture", "Chapter 1: System Architecture", "SQLite FTS5 Git", "Ghostbook uses embedded SQLite and 협업 문서 엔진 indexing."); err != nil {
		t.Fatalf("IndexChapterFTS: %v", err)
	}
	if err := st.IndexChapterFTS(ctx, "01-architecture", "Chapter 1: System Architecture Updated", "SQLite FTS5 Git", "Ghostbook uses embedded SQLite and 협업 문서 엔진 indexing."); err != nil {
		t.Fatalf("IndexChapterFTS update: %v", err)
	}

	// 1. Search English phrase with punctuation/operators that would break raw FTS5 syntax
	hits, err := st.SearchFTS(ctx, `"embedded SQLite" (Ghostbook) -+/*`, 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("SearchFTS English failed: hits=%+v err=%v", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, "<mark>") {
		t.Fatalf("expected highlighted snippet, got %q", hits[0].Snippet)
	}

	// 2. Search Korean Unicode tokens
	krHits, err := st.SearchFTS(ctx, "협업 문서", 5)
	if err != nil || len(krHits) != 1 {
		t.Fatalf("SearchFTS Unicode failed: hits=%+v err=%v", krHits, err)
	}

	// 3. Pure punctuation query -> empty slice, nil error
	emptyHits, err := st.SearchFTS(ctx, "   !@#$%^&*()   ", 5)
	if err != nil || len(emptyHits) != 0 {
		t.Fatalf("expected empty hits for symbol-only query, got %+v (err=%v)", emptyHits, err)
	}
}

func TestActivityAuditLog_LogAndFilter(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.LogActivity(ctx, store.AuditEntry{
		AuthorID:     "bob",
		Protocol:     "mcp",
		UserAgent:    "Claude-Desktop/1.0",
		ToolName:     "patch_chapter",
		ChapterSlug:  "01-architecture",
		GitCommitSHA: "1234567",
		Status:       "ok",
		DurationMs:   42,
	}); err != nil {
		t.Fatalf("LogActivity bob: %v", err)
	}

	if err := st.LogActivity(ctx, store.AuditEntry{
		TimestampISO: "2026-11-08T12:00:00Z",
		AuthorID:     "carol",
		Protocol:     "connectrpc",
		UserAgent:    "ChatGPT/1.0",
		ToolName:     "ReadChapter",
		ChapterSlug:  "02-agent-workflow",
		Status:       "ok",
		DurationMs:   11,
	}); err != nil {
		t.Fatalf("LogActivity carol: %v", err)
	}

	all, err := st.ListActivity(ctx, "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("expected 2 audit entries, got %+v (err=%v)", all, err)
	}

	bobOnly, err := st.ListActivity(ctx, "bob", 10)
	if err != nil || len(bobOnly) != 1 || bobOnly[0].AuthorID != "bob" {
		t.Fatalf("expected 1 audit entry for bob, got %+v (err=%v)", bobOnly, err)
	}
}

func TestAuthorSecretKeyAndIdempotentInvites(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "secret_key_test.db")

	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	if err := st1.SyncConfiguredAuthors(ctx, store.DefaultDemoAuthors); err != nil {
		t.Fatalf("SyncConfiguredAuthors st1: %v", err)
	}
	authors1, err := st1.ListAuthors(ctx)
	if err != nil || len(authors1) != 3 {
		t.Fatalf("expected 3 demo authors, got %+v (err=%v)", authors1, err)
	}
	keysByID := make(map[string]string)
	for _, a := range authors1 {
		if len(a.SecretKey) != 16 {
			t.Fatalf("expected 16-char secret_key for %s, got %q", a.ID, a.SecretKey)
		}
		keysByID[a.ID] = a.SecretKey

		authed, err := st1.Authenticate(ctx, a.AuthIdentifier())
		if err != nil || authed.ID != a.ID {
			t.Fatalf("expected Authenticate(%q) to succeed for %s, got %+v (err=%v)", a.AuthIdentifier(), a.ID, authed, err)
		}
		if _, err := st1.Authenticate(ctx, a.GitEmail+":wrongsecretkey00"); !errors.Is(err, store.ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized for wrong secret key on %s, got %v", a.GitEmail, err)
		}
	}
	_ = st1.Close()

	// Re-open database and sync authors again -> secret_key values must be preserved
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open st2: %v", err)
	}
	defer func() { _ = st2.Close() }()
	if err := st2.SyncConfiguredAuthors(ctx, store.DefaultDemoAuthors); err != nil {
		t.Fatalf("SyncConfiguredAuthors st2: %v", err)
	}

	authors2, err := st2.ListAuthors(ctx)
	if err != nil {
		t.Fatalf("ListAuthors st2: %v", err)
	}
	for _, a := range authors2 {
		if a.SecretKey != keysByID[a.ID] {
			t.Fatalf("secret_key changed across restart for %s: %q != %q", a.ID, a.SecretKey, keysByID[a.ID])
		}
	}

	// Verify BuildAuthorInviteEmail copy and SendPendingAuthorInvites idempotency
	var received []store.InviteEmail
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer re_test_key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var msg store.InviteEmail
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, msg)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test_123"}`))
	}))
	defer srv.Close()

	sent1, err := st2.SendPendingAuthorInvites(ctx, "https://docs.example.com", "re_test_key", srv.URL, testMailConfig)
	if err != nil || sent1 != 3 {
		t.Fatalf("expected 3 invites sent on first run, got sent=%d err=%v", sent1, err)
	}
	if len(received) != 3 {
		t.Fatalf("expected 3 HTTP requests to Resend mock, got %d", len(received))
	}
	for _, msg := range received {
		if msg.From != testMailConfig.From {
			t.Fatalf("unexpected From: %q", msg.From)
		}
		if !strings.Contains(msg.Text, "/guide") || !strings.Contains(msg.Text, "/mcp") {
			t.Fatalf("unexpected email body:\n%s", msg.Text)
		}
	}

	sent2, err := st2.SendPendingAuthorInvites(ctx, "https://docs.example.com", "re_test_key", srv.URL, testMailConfig)
	if err != nil || sent2 != 0 {
		t.Fatalf("expected 0 invites sent on second run, got sent=%d err=%v", sent2, err)
	}
}

func TestBatchFTS_And_BackupToFile(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	docs := []store.ChapterFTSDoc{
		{
			Slug:         "01-architecture",
			Title:        "Chapter 1: System Architecture",
			Headings:     "Storage Engine",
			BodyMarkdown: "Pure-Go Git commits paired with SQLite WAL full-text search.",
		},
		{
			Slug:         "02-agent-workflow",
			Title:        "Chapter 2: Agent Workflow",
			Headings:     "Surgical Patching",
			BodyMarkdown: "Optimistic locking with expected_revision_sha prevents lost updates.",
		},
	}
	if err := st.ReplaceAllChaptersFTS(ctx, docs); err != nil {
		t.Fatalf("ReplaceAllChaptersFTS: %v", err)
	}

	hits, err := st.SearchFTS(ctx, "expected_revision_sha", 5)
	if err != nil || len(hits) != 1 || hits[0].Slug != "02-agent-workflow" {
		t.Fatalf("expected 02-agent-workflow hit after ReplaceAllChaptersFTS, got %+v (err=%v)", hits, err)
	}

	if err := st.PruneStaleChapterFTS(ctx, []string{"01-architecture", "02-agent-workflow"}); err != nil {
		t.Fatalf("PruneStaleChapterFTS active: %v", err)
	}
	if err := st.DeleteChapterFTS(ctx, "02-agent-workflow"); err != nil {
		t.Fatalf("DeleteChapterFTS: %v", err)
	}
	if err := st.PruneStaleChapterFTS(ctx, nil); err != nil {
		t.Fatalf("PruneStaleChapterFTS empty: %v", err)
	}
	if err := st.ReplaceAllChaptersFTS(ctx, nil); err != nil {
		t.Fatalf("ReplaceAllChaptersFTS empty: %v", err)
	}

	stEmpty := openTestStore(t)
	if err := stEmpty.IndexChapterFTS(ctx, "01-architecture", "Chapter 1", "Section 1", "Ghostbook backup check"); err != nil {
		t.Fatalf("IndexChapterFTS: %v", err)
	}
	if err := stEmpty.BackupToFile(ctx, "   "); err == nil {
		t.Fatalf("expected error for empty backup destination path")
	}
	backupPath := filepath.Join(t.TempDir(), "backup_snapshot.db")
	if err := stEmpty.BackupToFile(ctx, backupPath); err != nil {
		t.Fatalf("BackupToFile: %v", err)
	}
	stRestored, err := store.Open(backupPath)
	if err != nil {
		t.Fatalf("Open backup DB: %v", err)
	}
	defer func() { _ = stRestored.Close() }()
	restoredHits, err := stRestored.SearchFTS(ctx, "Ghostbook backup", 5, "01-architecture")
	if err != nil || len(restoredHits) != 1 {
		t.Fatalf("expected 1 FTS hit in backed-up SQLite DB, got %+v (err=%v)", restoredHits, err)
	}
}

func TestInviteEdgeCases_And_ResolveAuthorColonAuth(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	msg := store.BuildAuthorInviteEmail("", store.Author{
		ID:       "custom",
		GitEmail: "custom@example.com",
	})
	if !strings.Contains(msg.Text, "Hi there,") || !strings.Contains(msg.Text, "http://127.0.0.1:8090/u/custom@example.com/guide") {
		t.Fatalf("unexpected fallback invite text:\n%s", msg.Text)
	}

	authors, _ := st.ListAuthors(ctx)
	for _, a := range authors {
		resolved, err := st.ResolveAuthorName(ctx, a.AuthIdentifier())
		if err != nil || resolved.ID != a.ID {
			t.Fatalf("ResolveAuthorName(%q) failed: %+v (err=%v)", a.AuthIdentifier(), resolved, err)
		}
	}
	if _, err := st.ResolveAuthorName(ctx, "alice@example.com:wrongsecret"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for invalid secret in ResolveAuthorName, got %v", err)
	}
	if _, err := st.Authenticate(ctx, "unknown@example.com:abcdef"); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for unknown email:secret, got %v", err)
	}
	if _, err := st.Authenticate(ctx, "alice@example.com:   "); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized for empty secret after colon, got %v", err)
	}
	noSecretAuthor := store.Author{ID: "x", GitEmail: "x@example.com"}
	if noSecretAuthor.AuthIdentifier() != "x@example.com" {
		t.Fatalf("expected plain email when SecretKey is empty, got %q", noSecretAuthor.AuthIdentifier())
	}

	if err := st.MarkAuthorInviteSent(ctx, "nonexistent-author", "msg_1", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for nonexistent author in MarkAuthorInviteSent, got %v", err)
	}

	if _, err := st.SendPendingAuthorInvites(ctx, "", "   ", "", testMailConfig); err == nil {
		t.Fatalf("expected error for empty Resend API key")
	}
	if _, err := st.SendPendingAuthorInvites(ctx, "", "re_test_key", "://invalid-url", testMailConfig); err == nil {
		t.Fatalf("expected error for malformed Resend endpoint URL")
	}
	if _, err := st.SendPendingAuthorInvites(ctx, "", "re_test_key", "http://127.0.0.1:1/emails", testMailConfig); err == nil {
		t.Fatalf("expected error for unreachable Resend endpoint")
	}
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	}))
	defer errSrv.Close()
	if _, err := st.SendPendingAuthorInvites(ctx, "", "re_test_key", errSrv.URL, testMailConfig); err == nil {
		t.Fatalf("expected error when Resend returns HTTP 429")
	}

	_ = st.Close()
	if _, err := st.ListAuthors(ctx); err == nil {
		t.Fatalf("expected error on closed DB ListAuthors")
	}
	if _, err := st.SendPendingAuthorInvites(ctx, "", "re_test_key", errSrv.URL, testMailConfig); err == nil {
		t.Fatalf("expected error on closed DB SendPendingAuthorInvites")
	}
	if _, err := st.SearchFTS(ctx, "test", 5); err == nil {
		t.Fatalf("expected error on closed DB SearchFTS")
	}
	if err := st.ReplaceAllChaptersFTS(ctx, []store.ChapterFTSDoc{{Slug: "s"}}); err == nil {
		t.Fatalf("expected error on closed DB ReplaceAllChaptersFTS")
	}
}

func TestAlertNotifier_CooldownAndErrorBranches(t *testing.T) {
	ctx := context.Background()

	var nilNotifier *store.AlertNotifier
	if nilNotifier.Enabled() || nilNotifier.NotifyError(ctx, "panic", "test", "details") {
		t.Fatalf("expected nil notifier to be disabled")
	}
	emptyNotifier := store.NewAlertNotifier("   ", "", "", 0, testMailConfig)
	if emptyNotifier.Enabled() || emptyNotifier.NotifyError(ctx, "panic", "test", "details") {
		t.Fatalf("expected empty API key notifier to be disabled")
	}

	var received []store.InviteEmail
	mockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg store.InviteEmail
		_ = json.NewDecoder(r.Body).Decode(&msg)
		received = append(received, msg)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"alert_1"}`))
	}))
	defer mockSrv.Close()

	notifier := store.NewAlertNotifier("re_alert_key", mockSrv.URL, "", 10*time.Minute, testMailConfig)
	if !notifier.Enabled() {
		t.Fatalf("expected notifier with API key and mail config to be enabled")
	}
	longSummary := strings.Repeat("unexpected panic in handler ", 5)
	if !notifier.NotifyError(ctx, "", longSummary, "stack trace line 1") {
		t.Fatalf("expected first NotifyError call to succeed")
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 alert email sent, got %d", len(received))
	}
	if len(received[0].To) != 2 || !strings.Contains(received[0].To[0], "alice@example.com") {
		t.Fatalf("unexpected recipients: %+v", received[0].To)
	}
	if notifier.NotifyError(ctx, "general", "second occurrence", "details") {
		t.Fatalf("expected second NotifyError within cooldown window to be suppressed")
	}
	if !notifier.NotifyError(ctx, "git_remote_sync", "push failed", "remote error") {
		t.Fatalf("expected different category to bypass first category cooldown")
	}

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	defer failSrv.Close()
	failNotifier := store.NewAlertNotifier("re_key", failSrv.URL, "https://docs.example.com", time.Millisecond, testMailConfig)
	if failNotifier.NotifyError(ctx, "http_500", "test failure", "details") {
		t.Fatalf("expected NotifyError to return false when Resend returns HTTP 502")
	}
	badURLNotifier := store.NewAlertNotifier("re_key", "://bad-url", "", time.Millisecond, testMailConfig)
	if badURLNotifier.NotifyError(ctx, "bad_url", "test", "details") {
		t.Fatalf("expected NotifyError to return false on malformed URL")
	}
	unreachableNotifier := store.NewAlertNotifier("re_key", "http://127.0.0.1:1/emails", "", time.Millisecond, testMailConfig)
	if unreachableNotifier.NotifyError(ctx, "unreachable", "test", "details") {
		t.Fatalf("expected NotifyError to return false on unreachable endpoint")
	}
}

func TestPlatformFeedback_CRUDRateLimitAndResendMock(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "feedback_unit_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SyncConfiguredAuthors(ctx, store.DefaultDemoAuthors); err != nil {
		t.Fatalf("sync authors: %v", err)
	}

	saved, err := st.CreateFeedback(ctx, store.FeedbackRecord{
		AuthorID:      "carol",
		AuthorEmail:   "carol@example.com",
		UserAgent:     "Claude-User/1.0",
		Kind:          "feature_request",
		Summary:       "Add multi-chapter renumbering tool",
		Details:       "Renumbering chapters 5 through 10 took 16 commits.",
		ChapterSlug:   "02-agent-workflow",
		RevisionSHA:   "r12",
		RecentContext: "- [2026-11-12T17:19:53.000Z] mcp/rename_chapter status=ok",
	})
	if err != nil {
		t.Fatalf("CreateFeedback: %v", err)
	}
	if saved.ID != 1 || saved.Status != "open" || saved.CreatedAtISO == "" {
		t.Fatalf("unexpected saved record: %+v", saved)
	}

	if err := st.UpdateFeedbackMessageID(ctx, saved.ID, "re_msg_12345"); err != nil {
		t.Fatalf("UpdateFeedbackMessageID: %v", err)
	}
	fetched, err := st.GetFeedback(ctx, saved.ID)
	if err != nil {
		t.Fatalf("GetFeedback: %v", err)
	}
	if fetched.ResendMessageID != "re_msg_12345" || fetched.Summary != "Add multi-chapter renumbering tool" {
		t.Fatalf("unexpected fetched record: %+v", fetched)
	}

	if _, err := st.CreateFeedback(ctx, store.FeedbackRecord{
		AuthorID: "carol",
		Kind:     "invalid_kind",
		Summary:  "Should fail CHECK",
	}); err == nil {
		t.Fatalf("expected SQLite CHECK error for invalid kind")
	}

	_ = st.Close()
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i := 0; i < 4; i++ {
		if _, err := st.CreateFeedback(ctx, store.FeedbackRecord{
			AuthorID: "carol",
			Kind:     "bug",
			Summary:  "Rate limit item",
		}); err != nil {
			t.Fatalf("CreateFeedback #%d: %v", i+2, err)
		}
	}
	carolCount, err := st.CountRecentFeedbackByAuthor(ctx, "carol", time.Now().UTC().Add(-10*time.Minute))
	if err != nil || carolCount != 5 {
		t.Fatalf("expected 5 recent feedback items for carol, got %d (err=%v)", carolCount, err)
	}
	bobCount, err := st.CountRecentFeedbackByAuthor(ctx, "bob", time.Now().UTC().Add(-10*time.Minute))
	if err != nil || bobCount != 0 {
		t.Fatalf("expected 0 recent feedback items for bob, got %d (err=%v)", bobCount, err)
	}

	var captured []store.InviteEmail
	mockResend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg store.InviteEmail
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		captured = append(captured, msg)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_feedback_999"}`))
	}))
	defer mockResend.Close()

	carolAuthor, err := st.ResolveAuthorName(ctx, "carol")
	if err != nil {
		t.Fatalf("ResolveAuthorName(carol): %v", err)
	}

	notifier := store.NewAlertNotifier("re_test_key", mockResend.URL, "https://docs.example.com", 15*time.Minute, testMailConfig)
	fetched.Summary = "Header\r\nInjection Attempt"
	msgID, ok := notifier.SendFeedbackNotification(ctx, *fetched, carolAuthor)
	if !ok || msgID != "re_feedback_999" {
		t.Fatalf("expected SendFeedbackNotification ok=true msgID=re_feedback_999, got ok=%v msgID=%q", ok, msgID)
	}
	msgID2, ok2 := notifier.SendFeedbackNotification(ctx, *fetched, carolAuthor)
	if !ok2 || msgID2 != "re_feedback_999" || len(captured) != 2 {
		t.Fatalf("expected second feedback notification not dropped by cooldown, got len=%d", len(captured))
	}
	gotMsg := captured[0]
	if !strings.HasPrefix(gotMsg.Subject, "[Ghostbook Feedback #1] feature_request from Carol Park: Header Injection Attempt") {
		t.Fatalf("unexpected feedback subject: %q", gotMsg.Subject)
	}
	if strings.ContainsAny(gotMsg.Subject, "\r\n") || strings.ContainsAny(gotMsg.ReplyTo, "\r\n") {
		t.Fatalf("expected CR/LF stripped from headers: subject=%q replyTo=%q", gotMsg.Subject, gotMsg.ReplyTo)
	}
	if gotMsg.ReplyTo != "Carol Park <carol@example.com>" {
		t.Fatalf("unexpected Reply-To: %q", gotMsg.ReplyTo)
	}

	guarded := store.NewAlertNotifier("re_real_looking_key", "", "https://docs.example.com", 15*time.Minute, testMailConfig)
	if guarded.Enabled() {
		t.Fatalf("expected NewAlertNotifier with empty endpoint under go test to disable live Resend key")
	}
	if id, sent := guarded.SendFeedbackNotification(ctx, *fetched, carolAuthor); sent || id != "" {
		t.Fatalf("expected disabled notifier not to send")
	}
}

func TestEditorialTimelinePersistence_UT_TL_01(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	initState, err := st.GetEditorialTimelineState(ctx)
	if err != nil || initState != nil {
		t.Fatalf("expected nil initial timeline state, got %+v (err=%v)", initState, err)
	}
	if _, err := st.AppendEditorialTimelineEntries(ctx, nil, store.TimelineStateRecord{}); err == nil {
		t.Fatalf("expected validation error for empty timeline state")
	}

	batch1, err := st.AppendEditorialTimelineEntries(ctx, []store.TimelineEntryRecord{
		{
			StartRevision:     2,
			EndRevision:       4,
			StartCommitSHA:    "aaa0002",
			EndCommitSHA:      "aaa0004",
			TimestampISO:      "2026-11-18T17:00:00Z",
			StartTimestampISO: "2026-11-18T16:45:00Z",
			CommitCount:       3,
			Revisions:         []int32{2, 3, 4},
			Title:             "Initial Storage Architecture",
			Summary:           "Documented pure-Go Git commits and SQLite WAL FTS5 indexing.",
			ChapterSlugs:      []string{"01-architecture"},
			Authors:           []string{"Bob Rivera"},
		},
	}, store.TimelineStateRecord{
		LastHeadSHA:      "aaa0004",
		LastHeadRevision: 4,
		ModelName:        "gemini-2.5-flash",
	})
	if err != nil || len(batch1) != 1 {
		t.Fatalf("AppendEditorialTimelineEntries batch1 failed: %+v (err=%v)", batch1, err)
	}
	firstID := batch1[0].ID

	batch2, err := st.AppendEditorialTimelineEntries(ctx, []store.TimelineEntryRecord{
		{
			StartRevision:     5,
			EndRevision:       7,
			StartCommitSHA:    "bbb0005",
			EndCommitSHA:      "bbb0007",
			TimestampISO:      "2026-11-18T18:30:00Z",
			StartTimestampISO: "2026-11-18T18:10:00Z",
			CommitCount:       3,
			Revisions:         []int32{5, 6, 7},
			Title:             "Surgical MCP Patching",
			Summary:           "Added optimistic lock examples and multi-hunk patch specs.",
			ChapterSlugs:      []string{"02-agent-workflow"},
			Authors:           []string{"Alice Chen"},
		},
	}, store.TimelineStateRecord{
		LastHeadSHA:      "bbb0007",
		LastHeadRevision: 7,
		ModelName:        "gemini-2.5-flash",
	})
	if err != nil || len(batch2) != 2 {
		t.Fatalf("AppendEditorialTimelineEntries batch2 failed: %+v (err=%v)", batch2, err)
	}
	if batch2[0].EndRevision != 7 || batch2[1].ID != firstID || batch2[1].Title != "Initial Storage Architecture" {
		t.Fatalf("expected append-only immutability on prior entry, got %+v", batch2)
	}

	batch3, err := st.AppendEditorialTimelineEntries(ctx, nil, store.TimelineStateRecord{
		LastHeadSHA:      "ccc0008",
		LastHeadRevision: 8,
		ModelName:        "gemini-2.5-flash",
	})
	if err != nil || len(batch3) != 2 {
		t.Fatalf("expected 2 entries preserved on zero-row cosmetic watermark advance, got %+v (err=%v)", batch3, err)
	}
	stAfter, err := st.GetEditorialTimelineState(ctx)
	if err != nil || stAfter == nil || stAfter.LastHeadRevision != 8 || stAfter.LastHeadSHA != "ccc0008" {
		t.Fatalf("unexpected timeline state after cosmetic batch: %+v (err=%v)", stAfter, err)
	}
}
