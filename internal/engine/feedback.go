package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wjkoh/ghostbook/internal/store"
)

const (
	maxFeedbackPerWindow    = 5
	feedbackRateLimitWindow = 10 * time.Minute
	minFeedbackSummaryRunes = 5
	maxFeedbackSummaryRunes = 200
	maxFeedbackDetailsRunes = 8000
)

func normalizeFeedbackKind(raw string) (string, error) {
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	if cleaned == "" {
		return "feature_request", nil
	}
	cleaned = strings.NewReplacer("-", "_", " ", "_").Replace(cleaned)
	switch cleaned {
	case "bug", "defect", "error", "issue":
		return "bug", nil
	case "feature_request", "feature", "enhancement", "request":
		return "feature_request", nil
	case "ux_friction", "ux", "friction", "improvement", "usability":
		return "ux_friction", nil
	default:
		return "", fmt.Errorf("%w: invalid feedback kind %q (expected bug, feature_request, or ux_friction)", ErrInvalidArgument, raw)
	}
}

func sanitizeSingleLineBounded(raw string, maxRunes int) string {
	noCRLF := strings.NewReplacer("\r", " ", "\n", " ").Replace(raw)
	collapsed := strings.Join(strings.Fields(strings.TrimSpace(noCRLF)), " ")
	if maxRunes <= 0 {
		return collapsed
	}
	runes := []rune(collapsed)
	if len(runes) <= maxRunes {
		return collapsed
	}
	return string(runes[:maxRunes])
}

func sanitizeMultilineBounded(raw string, maxRunes int) string {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	trimmed := strings.TrimSpace(normalized)
	if maxRunes <= 0 {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) <= maxRunes {
		return trimmed
	}
	return string(runes[:maxRunes])
}

func formatRecentAuditSnapshot(entries []store.AuditEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- [%s] %s/%s status=%s duration=%dms",
			e.TimestampISO, e.Protocol, e.ToolName, e.Status, e.DurationMs)
		if e.ChapterSlug != "" {
			fmt.Fprintf(&b, " slug=%q", e.ChapterSlug)
		}
		if e.GitCommitSHA != "" {
			fmt.Fprintf(&b, " rev=%s", e.GitCommitSHA)
		}
		if e.ErrorMessage != "" {
			fmt.Fprintf(&b, " error=%q", sanitizeSingleLineBounded(e.ErrorMessage, 240))
		}
	}
	return b.String()
}

func (e *BookEngine) ReportPlatformFeedback(ctx context.Context, in *ReportPlatformFeedbackInput) (*ReportPlatformFeedbackOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}

	kind, err := normalizeFeedbackKind(in.Kind)
	if err != nil {
		return nil, err
	}

	summary := sanitizeSingleLineBounded(in.Summary, maxFeedbackSummaryRunes)
	if utf8.RuneCountInString(summary) < minFeedbackSummaryRunes {
		return nil, fmt.Errorf("%w: summary must be at least %d characters", ErrInvalidArgument, minFeedbackSummaryRunes)
	}

	details := sanitizeMultilineBounded(in.Details, maxFeedbackDetailsRunes)
	chapterSlug := sanitizeSingleLineBounded(in.ChapterSlug, 128)
	revisionSHA := sanitizeSingleLineBounded(in.RevisionSHA, 64)

	author := e.resolveEffectiveAuthor(ctx, "")
	ua := sanitizeSingleLineBounded(UserAgentFromContext(ctx), 256)

	now := time.Now().UTC()
	recentCount, err := e.store.CountRecentFeedbackByAuthor(ctx, author.ID, now.Add(-feedbackRateLimitWindow))
	if err != nil {
		return nil, fmt.Errorf("check feedback rate limit: %w", err)
	}
	if recentCount >= maxFeedbackPerWindow {
		return nil, fmt.Errorf("%w: feedback rate limit exceeded (maximum %d submissions per 10 minutes)", ErrInvalidArgument, maxFeedbackPerWindow)
	}

	recentAudit, _ := e.store.ListActivity(ctx, author.ID, 5)
	recentContext := formatRecentAuditSnapshot(recentAudit)

	saved, err := e.store.CreateFeedback(ctx, store.FeedbackRecord{
		CreatedAtISO:  now.Format(store.TimestampMillisLayout),
		AuthorID:      author.ID,
		AuthorEmail:   author.GitEmail,
		UserAgent:     ua,
		Kind:          kind,
		Summary:       summary,
		Details:       details,
		ChapterSlug:   chapterSlug,
		RevisionSHA:   revisionSHA,
		RecentContext: recentContext,
		Status:        "open",
	})
	if err != nil {
		return nil, fmt.Errorf("persist platform feedback: %w", err)
	}

	emailNotified := false
	if alerter := e.AlertNotifier(); alerter != nil {
		msgID, sent := alerter.SendFeedbackNotification(ctx, *saved, author)
		if sent {
			emailNotified = true
			if msgID != "" {
				_ = e.store.UpdateFeedbackMessageID(ctx, saved.ID, msgID)
			}
		}
	}

	submittedBy := author.DisplayName
	if author.GitEmail != "" {
		if submittedBy != "" {
			submittedBy = fmt.Sprintf("%s <%s>", submittedBy, author.GitEmail)
		} else {
			submittedBy = author.GitEmail
		}
	}

	return &ReportPlatformFeedbackOutput{
		FeedbackID:    saved.ID,
		RecordedAtISO: saved.CreatedAtISO,
		SubmittedBy:   submittedBy,
		EmailNotified: emailNotified,
		Confirmation:  fmt.Sprintf("Recorded %s %s #%d from %s.", e.BookTitle(), saved.Kind, saved.ID, author.DisplayName),
	}, nil
}
