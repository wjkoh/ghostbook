package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	DefaultAlertCooldown = 15 * time.Minute
)

// AlertNotifier dispatches rate-limited operational alert emails via Resend
// whenever an unexpected daemon error, panic, or Git remote sync failure occurs.
type AlertNotifier struct {
	apiKey     string
	endpoint   string
	baseURL    string
	bookTitle  string
	fromAddr   string
	replyTo    string
	recipients []string
	cooldown   time.Duration
	httpClient *http.Client

	mu       sync.Mutex
	lastSent map[string]time.Time
	inFlight map[string]bool
	wg       sync.WaitGroup
}

func NewAlertNotifier(apiKey, endpoint, baseURL string, cooldown time.Duration, mailCfg ...MailConfig) *AlertNotifier {
	ep := strings.TrimSpace(endpoint)
	key := strings.TrimSpace(apiKey)
	if ep == "" || ep == DefaultResendEndpoint {
		if testing.Testing() {
			// Never dispatch real production Resend emails during `go test` when no mock endpoint is provided.
			key = ""
		}
		if ep == "" {
			ep = DefaultResendEndpoint
		}
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	if cooldown <= 0 {
		cooldown = DefaultAlertCooldown
	}
	var cfg MailConfig
	if len(mailCfg) > 0 {
		cfg = mailCfg[0]
	}
	bookTitle := strings.TrimSpace(cfg.BookTitle)
	if bookTitle == "" {
		bookTitle = DefaultBookTitle
	}
	var recipients []string
	for _, r := range cfg.Recipients {
		if clean := strings.TrimSpace(r); clean != "" {
			recipients = append(recipients, clean)
		}
	}
	return &AlertNotifier{
		apiKey:     key,
		endpoint:   ep,
		baseURL:    base,
		bookTitle:  bookTitle,
		fromAddr:   strings.TrimSpace(cfg.From),
		replyTo:    strings.TrimSpace(cfg.ReplyTo),
		recipients: recipients,
		cooldown:   cooldown,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		lastSent:   make(map[string]time.Time),
		inFlight:   make(map[string]bool),
	}
}

func (n *AlertNotifier) Enabled() bool {
	return n != nil && n.apiKey != "" && n.fromAddr != "" && len(n.recipients) > 0
}

func (n *AlertNotifier) Wait() {
	if n != nil {
		n.wg.Wait()
	}
}

func (n *AlertNotifier) IsInCooldown(category string, customCooldown time.Duration) bool {
	if n == nil {
		return false
	}
	key := strings.TrimSpace(category)
	if key == "" {
		key = "general"
	}
	cd := customCooldown
	if cd <= 0 {
		cd = n.cooldown
	}
	now := time.Now().UTC()
	n.mu.Lock()
	defer n.mu.Unlock()
	if prev, ok := n.lastSent[key]; ok && now.Sub(prev) < cd {
		return true
	}
	return false
}

// NotifyError sends a rate-limited plain-text alert email for the given category/key.
// Returns true if an email was dispatched, or false if skipped (disabled, within cooldown, or failed).
func (n *AlertNotifier) NotifyError(ctx context.Context, category, summary, details string) bool {
	if n == nil {
		return false
	}
	return n.NotifyWithCooldown(ctx, category, n.cooldown, summary, details)
}

// NotifyWithCooldown sends a rate-limited plain-text alert email using customCooldown (or default cooldown if <= 0).
func (n *AlertNotifier) NotifyWithCooldown(ctx context.Context, category string, customCooldown time.Duration, summary, details string) bool {
	if !n.Enabled() {
		return false
	}
	key := strings.TrimSpace(category)
	if key == "" {
		key = "general"
	}
	cd := customCooldown
	if cd <= 0 {
		cd = n.cooldown
	}

	now := time.Now().UTC()
	n.mu.Lock()
	if n.inFlight[key] {
		n.mu.Unlock()
		return false
	}
	if prev, ok := n.lastSent[key]; ok && now.Sub(prev) < cd {
		n.mu.Unlock()
		return false
	}
	n.inFlight[key] = true
	n.mu.Unlock()

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "ghostbook-host"
	}

	subject := fmt.Sprintf("[%s Alert] %s: %s", n.bookTitle, key, truncateAlertSubject(summary, 72))
	body := fmt.Sprintf(`%s Daemon Alert

Timestamp (UTC): %s
Host:            %s
Base URL:        %s
Category:        %s
Summary:         %s

Details:
%s
`, n.bookTitle, now.Format(time.RFC3339), hostname, n.baseURL, key, sanitizeHeaderText(summary), strings.TrimSpace(details))

	_, ok := n.postEmail(ctx, InviteEmail{
		From:    n.fromAddr,
		To:      n.recipients,
		ReplyTo: n.replyTo,
		Subject: subject,
		Text:    body,
	}, key)

	n.mu.Lock()
	delete(n.inFlight, key)
	if ok {
		n.lastSent[key] = time.Now().UTC()
	}
	n.mu.Unlock()
	return ok
}

// DispatchAsyncAlert checks cooldown synchronously and, if eligible, dispatches NotifyWithCooldown
// in a tracked background goroutine detached from request cancellation.
func (n *AlertNotifier) DispatchAsyncAlert(ctx context.Context, category string, customCooldown time.Duration, summary, details string) {
	if !n.Enabled() || n.IsInCooldown(category, customCooldown) {
		return
	}
	bgCtx := context.WithoutCancel(ctx)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.NotifyWithCooldown(bgCtx, category, customCooldown, summary, details)
	}()
}

// SendFeedbackNotification sends an immediate (non-cooldown-suppressed) email for an explicit
// platform_feedback submission, setting Reply-To to the submitting co-author and returning the Resend message ID.
func (n *AlertNotifier) SendFeedbackNotification(ctx context.Context, rec FeedbackRecord, author *Author) (string, bool) {
	if !n.Enabled() {
		return "", false
	}

	displayName := strings.TrimSpace(rec.AuthorID)
	replyTo := n.replyTo
	if author != nil {
		if dn := sanitizeHeaderText(author.DisplayName); dn != "" {
			displayName = dn
		}
		if email := sanitizeHeaderText(author.GitEmail); email != "" {
			replyTo = formatMailboxHeader(author.DisplayName, email)
		}
	}
	if displayName == "" {
		displayName = "Co-Author"
	}

	subject := fmt.Sprintf("[%s Feedback #%d] %s from %s: %s",
		n.bookTitle,
		rec.ID,
		sanitizeHeaderText(rec.Kind),
		displayName,
		truncateAlertSubject(rec.Summary, 80),
	)

	slugLabel := sanitizeHeaderText(rec.ChapterSlug)
	if slugLabel == "" {
		slugLabel = "(none)"
	}
	revLabel := sanitizeHeaderText(rec.RevisionSHA)
	if revLabel == "" {
		revLabel = "(none)"
	}
	uaLabel := sanitizeHeaderText(rec.UserAgent)
	if uaLabel == "" {
		uaLabel = "(unknown)"
	}
	detailsBlock := strings.TrimSpace(rec.Details)
	if detailsBlock == "" {
		detailsBlock = "(no additional details provided)"
	}
	contextBlock := strings.TrimSpace(rec.RecentContext)
	if contextBlock == "" {
		contextBlock = "(no recent audit log entries)"
	}

	body := fmt.Sprintf(`%s Platform Feedback (#%d)

Feedback ID:     %d
Timestamp (UTC): %s
Kind:            %s
Submitted By:    %s (%s)
User-Agent:      %s
Chapter Slug:    %s
Revision SHA:    %s
Base URL:        %s

Summary:
%s

Details:
%s

Recent Author MCP / Studio Context:
%s
`,
		n.bookTitle,
		rec.ID,
		rec.ID,
		rec.CreatedAtISO,
		rec.Kind,
		displayName,
		sanitizeHeaderText(rec.AuthorEmail),
		uaLabel,
		slugLabel,
		revLabel,
		n.baseURL,
		sanitizeHeaderText(rec.Summary),
		detailsBlock,
		contextBlock,
	)

	return n.postEmail(ctx, InviteEmail{
		From:    n.fromAddr,
		To:      n.recipients,
		ReplyTo: replyTo,
		Subject: subject,
		Text:    body,
	}, fmt.Sprintf("platform_feedback_%d", rec.ID))
}

func (n *AlertNotifier) postEmail(ctx context.Context, msg InviteEmail, logCategory string) (string, bool) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return "", false
	}

	sendCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, n.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+n.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		slog.Warn("alert_email_send_failed", "category", logCategory, "error", err.Error())
		return "", false
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("alert_email_http_error", "category", logCategory, "status", resp.StatusCode, "body", strings.TrimSpace(string(respBody)))
		return "", false
	}

	var parsed resendSendResponse
	_ = json.Unmarshal(respBody, &parsed)
	slog.Info("alert_email_dispatched", "category", logCategory, "message_id", parsed.ID, "recipients", len(n.recipients))
	return strings.TrimSpace(parsed.ID), true
}

func sanitizeHeaderText(s string) string {
	clean := strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return strings.Join(strings.Fields(strings.TrimSpace(clean)), " ")
}

func truncateAlertSubject(s string, maxRunes int) string {
	clean := sanitizeHeaderText(s)
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	return string(runes[:maxRunes]) + "..."
}
