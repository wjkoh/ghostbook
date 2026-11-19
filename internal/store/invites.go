package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"
)

const (
	DefaultResendEndpoint = "https://api.resend.com/emails"
	DefaultBookTitle      = "Ghostbook"
)

// MailConfig configures outgoing operational alerts and co-author onboarding invites via Resend.
type MailConfig struct {
	BookTitle  string
	From       string
	ReplyTo    string
	Recipients []string
}

type InviteEmail struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	ReplyTo string   `json:"reply_to,omitempty"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

func firstName(displayName string) string {
	parts := strings.Fields(strings.TrimSpace(displayName))
	if len(parts) == 0 {
		return "there"
	}
	return parts[0]
}

// BuildAuthorInviteEmail constructs the minimal plain-text onboarding email
// pointing the co-author to their personalized /u/<email>:<secret-key>/guide and /mcp endpoints.
func BuildAuthorInviteEmail(baseURL string, author Author, mailCfg ...MailConfig) InviteEmail {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://127.0.0.1:8090"
	}
	var cfg MailConfig
	if len(mailCfg) > 0 {
		cfg = mailCfg[0]
	}
	bookTitle := strings.TrimSpace(cfg.BookTitle)
	if bookTitle == "" {
		bookTitle = DefaultBookTitle
	}
	fromAddr := strings.TrimSpace(cfg.From)
	replyTo := strings.TrimSpace(cfg.ReplyTo)

	authIdent := author.AuthIdentifier()
	guideURL := fmt.Sprintf("%s/u/%s/guide", base, authIdent)
	mcpURL := fmt.Sprintf("%s/u/%s/mcp", base, authIdent)

	body := fmt.Sprintf(`Hi %s,

%s is live and designed to be read on the web while authored strictly through your AI agent (Claude, ChatGPT, Cursor, or any MCP client).

Open your personalized setup guide below to connect your AI agent in one click:

%s

Your private MCP server URL is:

%s

Please keep these links private, as they authenticate all edits and Git commits under your name.
`, firstName(author.DisplayName), bookTitle, guideURL, mcpURL)

	toFormatted := formatMailboxHeader(author.DisplayName, author.GitEmail)

	return InviteEmail{
		From:    fromAddr,
		To:      []string{toFormatted},
		ReplyTo: replyTo,
		Subject: bookTitle + " — Connect Your AI Agent",
		Text:    body,
	}
}

func formatMailboxHeader(displayName, email string) string {
	cleanEmail := strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(email))
	cleanName := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(displayName))
	if cleanName == "" {
		return cleanEmail
	}
	addr := &mail.Address{Name: cleanName, Address: cleanEmail}
	return addr.String()
}

type resendSendResponse struct {
	ID string `json:"id"`
}

// SendPendingAuthorInvites checks SQLite for any active co-authors whose invite_sent_at is empty,
// dispatches their personalized invite email via Resend, and records invite_sent_at and invite_message_id.
func (s *Store) SendPendingAuthorInvites(ctx context.Context, baseURL, resendAPIKey, resendEndpoint string, mailCfg ...MailConfig) (int, error) {
	apiKey := strings.TrimSpace(resendAPIKey)
	if apiKey == "" {
		return 0, fmt.Errorf("resend API key is empty")
	}
	var cfg MailConfig
	if len(mailCfg) > 0 {
		cfg = mailCfg[0]
	}
	if strings.TrimSpace(cfg.From) == "" {
		return 0, fmt.Errorf("mail_from is required to send author invites")
	}
	endpoint := strings.TrimRight(strings.TrimSpace(resendEndpoint), "/")
	if endpoint == "" || endpoint == strings.TrimRight(DefaultResendEndpoint, "/") {
		if testing.Testing() {
			return 0, fmt.Errorf("refusing to call production Resend API during go test without mock endpoint")
		}
		if endpoint == "" {
			endpoint = DefaultResendEndpoint
		}
	}

	pending, err := s.ListAuthorsPendingInvite(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pending author invites: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	sentCount := 0
	var errs []error

	for _, author := range pending {
		msg := BuildAuthorInviteEmail(baseURL, author, cfg)
		payload, err := json.Marshal(msg)
		if err != nil {
			errs = append(errs, fmt.Errorf("marshal invite for %s: %w", author.GitEmail, err))
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			errs = append(errs, fmt.Errorf("create request for %s: %w", author.GitEmail, err))
			continue
		}
		idempSum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(baseURL), "/") + ":" + author.ID + ":" + author.SecretKey))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "ghostbook-invite-"+hex.EncodeToString(idempSum[:12]))

		resp, err := httpClient.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("send invite to %s: %w", author.GitEmail, err))
			continue
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errs = append(errs, fmt.Errorf("resend API error for %s (HTTP %d): %s", author.GitEmail, resp.StatusCode, strings.TrimSpace(string(respBody))))
			continue
		}

		var parsed resendSendResponse
		_ = json.Unmarshal(respBody, &parsed)
		if err := s.MarkAuthorInviteSent(ctx, author.ID, parsed.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
			errs = append(errs, fmt.Errorf("mark invite sent for %s: %w", author.GitEmail, err))
			continue
		}
		sentCount++
	}

	return sentCount, errors.Join(errs...)
}
