package engine_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

func TestReportPlatformFeedback_ValidationAliasesAuditContextAndOfflineResilience(t *testing.T) {
	eng, st := setupTestEngine(t)
	baseCtx := context.Background()

	alice, err := st.ResolveAuthorName(baseCtx, "alice")
	if err != nil {
		t.Fatalf("ResolveAuthorName(alice): %v", err)
	}
	ctx := engine.WithUserAgent(engine.WithAuthor(baseCtx, alice), "Claude-User/1.0")

	// 1. Validation rejections (nil, short summary, invalid kind)
	if _, err := eng.ReportPlatformFeedback(ctx, nil); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for nil input, got %v", err)
	}
	if _, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Summary: "hi",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for short summary, got %v", err)
	}
	if _, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Kind:    "praise_only",
		Summary: "Valid summary length",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected ErrInvalidArgument for unknown kind, got %v", err)
	}

	// Seed 2 recent audit rows for alice so recent_context capture is verified
	_ = st.LogActivity(ctx, store.AuditEntry{
		AuthorID:     "alice",
		Protocol:     "mcp",
		UserAgent:    "Claude-User/1.0",
		ToolName:     "patch_chapter",
		ChapterSlug:  "04-deployment-ops",
		Status:       "conflict",
		ErrorMessage: "patch conflict: old_text not found",
		DurationMs:   14,
	})

	// 2. Mock Resend HTTP 200 -> verifies alias mapping, truncation bounds, audit snapshot, and message ID backfill
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_ok_001"}`))
	}))
	defer okSrv.Close()
	eng.SetAlertNotifier(store.NewAlertNotifier("re_test", okSrv.URL, "https://docs.example.com", 0, store.MailConfig{BookTitle: "Ghostbook", From: "Ghostbook <ghostbook@example.com>", ReplyTo: "Alice <alice@example.com>", Recipients: []string{"Alice <alice@example.com>"}}))

	longSummary := "Multi-line\r\nsummary " + strings.Repeat("x", 260)
	longDetails := strings.Repeat("d", 8500)
	out1, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Kind:        "ux",
		Summary:     longSummary,
		Details:     longDetails,
		ChapterSlug: "04-deployment-ops\r\n",
		RevisionSHA: "r186\n",
	})
	if err != nil {
		t.Fatalf("ReportPlatformFeedback #1: %v", err)
	}
	if !out1.EmailNotified || out1.FeedbackID != 1 {
		t.Fatalf("unexpected output #1: %+v", out1)
	}

	rec1, err := st.GetFeedback(ctx, out1.FeedbackID)
	if err != nil {
		t.Fatalf("GetFeedback #1: %v", err)
	}
	if rec1.Kind != "ux_friction" || rec1.ResendMessageID != "re_ok_001" {
		t.Fatalf("unexpected stored kind/message_id: %+v", rec1)
	}
	if utf8.RuneCountInString(rec1.Summary) != 200 || strings.ContainsAny(rec1.Summary, "\r\n") {
		t.Fatalf("expected 200-rune single-line summary, got len=%d", utf8.RuneCountInString(rec1.Summary))
	}
	if utf8.RuneCountInString(rec1.Details) != 8000 {
		t.Fatalf("expected 8000-rune details bound, got %d", utf8.RuneCountInString(rec1.Details))
	}
	if !strings.Contains(rec1.RecentContext, "mcp/patch_chapter") || !strings.Contains(rec1.RecentContext, "04-deployment-ops") {
		t.Fatalf("expected recent_context to include recent alice audit entry, got:\n%s", rec1.RecentContext)
	}

	// 3. Offline / Resend HTTP 500 resilience: SQLite persistence still succeeds with EmailNotified=false
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"upstream unavailable"}`, http.StatusInternalServerError)
	}))
	defer failSrv.Close()
	eng.SetAlertNotifier(store.NewAlertNotifier("re_test", failSrv.URL, "https://docs.example.com", 0))

	out2, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Kind:    "", // defaults to feature_request
		Summary: "Offline resilience feature suggestion",
	})
	if err != nil {
		t.Fatalf("expected graceful success on Resend 500, got %v", err)
	}
	if out2.EmailNotified {
		t.Fatalf("expected EmailNotified=false when Resend returns 500")
	}
	rec2, err := st.GetFeedback(ctx, out2.FeedbackID)
	if err != nil || rec2.Kind != "feature_request" || rec2.ResendMessageID != "" {
		t.Fatalf("expected SQLite row preserved with empty resend_message_id, got %+v (err=%v)", rec2, err)
	}

	// 4. Fill remaining 3 slots (total 5 in 10 minutes for alice), then assert 6th is rate-limited while carol succeeds
	for _, alias := range []string{"bug", "defect", "enhancement"} {
		if _, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
			Kind:    alias,
			Summary: "Valid feedback item for alias " + alias,
		}); err != nil {
			t.Fatalf("unexpected error for alias %q: %v", alias, err)
		}
	}
	if _, err := eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Kind:    "bug",
		Summary: "Sixth submission within 10 minutes should be rate-limited",
	}); !errors.Is(err, engine.ErrInvalidArgument) {
		t.Fatalf("expected 6th submission for alice to return ErrInvalidArgument, got %v", err)
	}

	carol, _ := st.ResolveAuthorName(baseCtx, "carol")
	carolCtx := engine.WithAuthor(baseCtx, carol)
	if _, err := eng.ReportPlatformFeedback(carolCtx, &engine.ReportPlatformFeedbackInput{
		Kind:    "feature",
		Summary: "Carol independent rate limit bucket",
	}); err != nil {
		t.Fatalf("expected carol submission to succeed independently, got %v", err)
	}
}
