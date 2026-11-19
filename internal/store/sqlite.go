package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrUnauthorized = errors.New("unauthorized or missing author token")
	ErrTokenRevoked = errors.New("author token has been revoked")
	ErrNotFound     = errors.New("record not found")
)

type Author struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name"`
	GitEmail        string `json:"git_email"`
	RoleBadge       string `json:"role_badge"`
	SecretKey       string `json:"-"`
	InviteSentAt    string `json:"invite_sent_at,omitempty"`
	InviteMessageID string `json:"invite_message_id,omitempty"`
}

func (a Author) AuthIdentifier() string {
	if strings.TrimSpace(a.SecretKey) != "" {
		return a.GitEmail + ":" + a.SecretKey
	}
	return a.GitEmail
}

type SearchHit struct {
	Slug      string  `json:"slug"`
	Title     string  `json:"title"`
	Snippet   string  `json:"snippet"`
	BM25Score float64 `json:"bm25_score"`
}

type ChapterFTSDoc struct {
	Slug         string
	Title        string
	Headings     string
	BodyMarkdown string
}

type AuditEntry struct {
	ID           int64  `json:"id"`
	TimestampISO string `json:"timestamp_iso"`
	AuthorID     string `json:"author_id"`
	Protocol     string `json:"protocol"`
	UserAgent    string `json:"user_agent"`
	ToolName     string `json:"tool_name"`
	ChapterSlug  string `json:"chapter_slug"`
	GitCommitSHA string `json:"git_commit_sha"`
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message"`
	DurationMs   int64  `json:"duration_ms"`
}

const TimestampMillisLayout = "2006-01-02T15:04:05.000Z"

type FeedbackRecord struct {
	ID              int64  `json:"id"`
	CreatedAtISO    string `json:"created_at_iso"`
	AuthorID        string `json:"author_id"`
	AuthorEmail     string `json:"author_email"`
	UserAgent       string `json:"user_agent"`
	Kind            string `json:"kind"`
	Summary         string `json:"summary"`
	Details         string `json:"details"`
	ChapterSlug     string `json:"chapter_slug"`
	RevisionSHA     string `json:"revision_sha"`
	RecentContext   string `json:"recent_context"`
	ResendMessageID string `json:"resend_message_id"`
	Status          string `json:"status"`
}

type MCPFrictionWindowStats struct {
	FailedCalls120s   int
	RecentFailures    []AuditEntry
	WriteCalls180s    int
	DistinctWriteSlug int
	RecentWriteSlugs  []string
}

type TimelineEntryRecord struct {
	ID                int64    `json:"id"`
	StartRevision     int32    `json:"start_revision"`
	EndRevision       int32    `json:"end_revision"`
	StartCommitSHA    string   `json:"start_commit_sha"`
	EndCommitSHA      string   `json:"end_commit_sha"`
	TimestampISO      string   `json:"timestamp_iso"`
	StartTimestampISO string   `json:"start_timestamp_iso"`
	CommitCount       int      `json:"commit_count"`
	Revisions         []int32  `json:"revisions"`
	Title             string   `json:"title"`
	Summary           string   `json:"summary"`
	ChapterSlugs      []string `json:"chapter_slugs"`
	Authors           []string `json:"authors,omitempty"`
	CreatedAtISO      string   `json:"created_at_iso"`
}

type TimelineStateRecord struct {
	LastHeadSHA      string `json:"last_head_sha"`
	LastHeadRevision int32  `json:"last_head_revision"`
	ModelName        string `json:"model_name"`
	UpdatedAtISO     string `json:"updated_at_iso"`
}

const lastUsedUpdateInterval = 5 * time.Minute

type Store struct {
	db       *sql.DB
	mu       sync.Mutex
	lastUsed map[string]time.Time
}

// AuthorConfig defines a co-author loaded from an authors JSON file or CLI command.
type AuthorConfig struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Email       string   `json:"email"`
	Badge       string   `json:"role_badge,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// DefaultDemoAuthors provides neutral sample authors for unit tests and local demos.
var DefaultDemoAuthors = []AuthorConfig{
	{
		ID:          "alice",
		DisplayName: "Alice Chen",
		Email:       "alice@example.com",
		Badge:       "Co-Author · Systems",
		Aliases:     []string{"alice@example.com", "alice", "ghostbook-token-alice"},
	},
	{
		ID:          "bob",
		DisplayName: "Bob Rivera",
		Email:       "bob@example.com",
		Badge:       "Co-Author · Product",
		Aliases:     []string{"bob@example.com", "bob", "ghostbook-token-bob"},
	},
	{
		ID:          "carol",
		DisplayName: "Carol Park",
		Email:       "carol@example.com",
		Badge:       "Co-Author · Research",
		Aliases:     []string{"carol@example.com", "carol", "ghostbook-token-carol"},
	},
}

// LoadAuthorsFile parses a JSON array of AuthorConfig entries from disk.
func LoadAuthorsFile(path string) ([]AuthorConfig, error) {
	raw, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("read authors file: %w", err)
	}
	var authors []AuthorConfig
	if err := json.Unmarshal(raw, &authors); err != nil {
		return nil, fmt.Errorf("parse authors file: %w", err)
	}
	return authors, nil
}

func Open(dbPath string) (*Store, error) {
	cleanPath := strings.TrimSpace(dbPath)
	if cleanPath == "" {
		return nil, fmt.Errorf("open sqlite: empty dbPath")
	}
	escapedPath := (&url.URL{Path: cleanPath}).EscapedPath()
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(ON)", escapedPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{
		db:       db,
		lastUsed: make(map[string]time.Time),
	}
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func HashToken(raw string) string {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func canonicalizeCredential(raw string) (aliasPart, secretPart, canonical string, ok bool) {
	cleaned := strings.TrimSpace(strings.ToLower(raw))
	colonIdx := strings.IndexByte(cleaned, ':')
	if colonIdx <= 0 {
		return "", "", "", false
	}
	aliasPart = strings.TrimSpace(cleaned[:colonIdx])
	secretPart = strings.TrimSpace(cleaned[colonIdx+1:])
	if aliasPart == "" || secretPart == "" || strings.ContainsAny(secretPart, " \t\r\n:") {
		return "", "", "", false
	}
	return aliasPart, secretPart, aliasPart + ":" + secretPart, true
}

func generateSecretKey() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Store) initSchema() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("exec schema.sql: %w", err)
	}

	// Idempotent column & index migrations for existing SQLite databases
	for _, stmt := range []string{
		`ALTER TABLE authors ADD COLUMN secret_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE authors ADD COLUMN invite_sent_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE authors ADD COLUMN invite_message_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_audit_author_id_desc ON agent_audit_log(author_id, id DESC)`,
	} {
		_, _ = s.db.Exec(stmt)
	}

	// Revoke any legacy secretless bare-email or bare-ID tokens from earlier schema versions
	if _, err := s.db.Exec(`UPDATE auth_tokens SET revoked = 1 WHERE instr(raw_identifier, ':') = 0`); err != nil {
		return fmt.Errorf("revoke legacy secretless tokens: %w", err)
	}
	// Scrub any legacy plaintext :<secret_key> values previously stored in auth_tokens.raw_identifier
	if _, err := s.db.Exec(`
		UPDATE auth_tokens
		SET raw_identifier = substr(raw_identifier, 1, instr(raw_identifier, ':') - 1) || ':redacted'
		WHERE instr(raw_identifier, ':') > 0 AND raw_identifier NOT LIKE '%:redacted'
	`); err != nil {
		return fmt.Errorf("redact legacy auth_tokens raw_identifier: %w", err)
	}
	return nil
}

// UpsertAuthor registers or updates a single co-author, generating a random 16-hex secret_key
// if the author is new while preserving any existing secret_key in SQLite.
func (s *Store) UpsertAuthor(ctx context.Context, cfg AuthorConfig) (*Author, error) {
	id := strings.ToLower(strings.TrimSpace(cfg.ID))
	displayName := strings.TrimSpace(cfg.DisplayName)
	email := strings.ToLower(strings.TrimSpace(cfg.Email))
	badge := strings.TrimSpace(cfg.Badge)
	if badge == "" {
		badge = "Co-Author"
	}
	if id == "" || displayName == "" || email == "" {
		return nil, fmt.Errorf("author id, display_name, and email are required")
	}
	if strings.ContainsAny(id, " :\t\r\n/") || strings.ContainsAny(email, " :\t\r\n/") {
		return nil, fmt.Errorf("author id and email must not contain spaces, colons, or slashes")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.upsertAuthorLocked(ctx, AuthorConfig{
		ID:          id,
		DisplayName: displayName,
		Email:       email,
		Badge:       badge,
		Aliases:     cfg.Aliases,
	})
}

// SyncConfiguredAuthors upserts every author from a configuration list without rotating existing secret keys.
func (s *Store) SyncConfiguredAuthors(ctx context.Context, authors []AuthorConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, cfg := range authors {
		id := strings.ToLower(strings.TrimSpace(cfg.ID))
		displayName := strings.TrimSpace(cfg.DisplayName)
		email := strings.ToLower(strings.TrimSpace(cfg.Email))
		badge := strings.TrimSpace(cfg.Badge)
		if badge == "" {
			badge = "Co-Author"
		}
		if id == "" || displayName == "" || email == "" {
			return fmt.Errorf("invalid author config %+v: id, display_name, and email are required", cfg)
		}
		if _, err := s.upsertAuthorLocked(ctx, AuthorConfig{
			ID:          id,
			DisplayName: displayName,
			Email:       email,
			Badge:       badge,
			Aliases:     cfg.Aliases,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) upsertAuthorLocked(ctx context.Context, a AuthorConfig) (*Author, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO authors (id, display_name, git_email, role_badge)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name = excluded.display_name,
			git_email = excluded.git_email,
			role_badge = excluded.role_badge
	`, a.ID, a.DisplayName, a.Email, a.Badge)
	if err != nil {
		return nil, fmt.Errorf("upsert author %s: %w", a.ID, err)
	}

	var secretKey string
	if err := s.db.QueryRowContext(ctx, `SELECT secret_key FROM authors WHERE id = ?`, a.ID).Scan(&secretKey); err != nil {
		return nil, fmt.Errorf("query secret_key for %s: %w", a.ID, err)
	}
	if strings.TrimSpace(secretKey) == "" {
		generated, err := generateSecretKey()
		if err != nil {
			return nil, fmt.Errorf("generate secret_key for %s: %w", a.ID, err)
		}
		secretKey = generated
		if _, err := s.db.ExecContext(ctx, `UPDATE authors SET secret_key = ? WHERE id = ?`, secretKey, a.ID); err != nil {
			return nil, fmt.Errorf("update secret_key for %s: %w", a.ID, err)
		}
	}

	seenAliases := map[string]bool{}
	addAlias := func(raw string) {
		clean := strings.TrimSpace(strings.ToLower(raw))
		if clean != "" && !strings.Contains(clean, ":") {
			seenAliases[clean] = true
		}
	}
	addAlias(a.Email)
	addAlias(a.ID)
	for _, alias := range a.Aliases {
		addAlias(alias)
	}

	for alias := range seenAliases {
		fullCred := alias + ":" + strings.ToLower(secretKey)
		h := HashToken(fullCred)
		redactedLabel := alias + ":redacted"
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO auth_tokens (token_hash, raw_identifier, author_id, client_label, revoked)
			VALUES (?, ?, ?, ?, 0)
			ON CONFLICT(token_hash) DO UPDATE SET
				raw_identifier = excluded.raw_identifier
		`, h, redactedLabel, a.ID, a.DisplayName+" ("+a.ID+")")
		if err != nil {
			return nil, fmt.Errorf("seed token %s: %w", redactedLabel, err)
		}
	}

	return s.queryAuthorByIDLocked(ctx, a.ID)
}

func (s *Store) queryAuthorByIDLocked(ctx context.Context, authorID string) (*Author, error) {
	var author Author
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, git_email, role_badge, secret_key, invite_sent_at, invite_message_id
		FROM authors WHERE id = ?
	`, authorID).Scan(&author.ID, &author.DisplayName, &author.GitEmail, &author.RoleBadge, &author.SecretKey, &author.InviteSentAt, &author.InviteMessageID)
	if err != nil {
		return nil, err
	}
	return &author, nil
}

func (s *Store) isAuthorCredentialRevokedLocked(ctx context.Context, author *Author) bool {
	if author == nil || strings.TrimSpace(author.SecretKey) == "" {
		return true
	}
	primaryHash := HashToken(strings.ToLower(author.GitEmail) + ":" + strings.ToLower(author.SecretKey))
	var revoked int
	if err := s.db.QueryRowContext(ctx, `SELECT revoked FROM auth_tokens WHERE token_hash = ?`, primaryHash).Scan(&revoked); err == nil && revoked != 0 {
		return true
	}
	return false
}

func (s *Store) Authenticate(ctx context.Context, rawTokenOrEmail string) (*Author, error) {
	emailPart, secretPart, canonical, ok := canonicalizeCredential(rawTokenOrEmail)
	if !ok {
		return nil, ErrUnauthorized
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	h := HashToken(canonical)
	var tokenAuthorID string
	var revoked int
	var author *Author

	if err := s.db.QueryRowContext(ctx, `SELECT author_id, revoked FROM auth_tokens WHERE token_hash = ?`, h).Scan(&tokenAuthorID, &revoked); err == nil {
		if revoked != 0 {
			return nil, ErrTokenRevoked
		}
		a, qErr := s.queryAuthorByIDLocked(ctx, tokenAuthorID)
		if qErr != nil {
			return nil, ErrUnauthorized
		}
		author = a
	} else {
		var resolved Author
		qErr := s.db.QueryRowContext(ctx, `
			SELECT id, display_name, git_email, role_badge, secret_key, invite_sent_at, invite_message_id
			FROM authors
			WHERE LOWER(git_email) = ? OR LOWER(id) = ?
		`, emailPart, emailPart).Scan(&resolved.ID, &resolved.DisplayName, &resolved.GitEmail, &resolved.RoleBadge, &resolved.SecretKey, &resolved.InviteSentAt, &resolved.InviteMessageID)
		if qErr != nil || resolved.SecretKey == "" {
			return nil, ErrUnauthorized
		}
		author = &resolved
	}

	expectedSecret := strings.ToLower(strings.TrimSpace(author.SecretKey))
	if expectedSecret == "" {
		return nil, ErrUnauthorized
	}
	secretSum := sha256.Sum256([]byte(secretPart))
	expectedSum := sha256.Sum256([]byte(expectedSecret))
	if subtle.ConstantTimeCompare(secretSum[:], expectedSum[:]) != 1 {
		return nil, ErrUnauthorized
	}
	if s.isAuthorCredentialRevokedLocked(ctx, author) {
		return nil, ErrTokenRevoked
	}

	now := time.Now().UTC()
	if prev, seen := s.lastUsed[h]; !seen || now.Sub(prev) >= lastUsedUpdateInterval {
		s.lastUsed[h] = now
		_, _ = s.db.ExecContext(ctx, `UPDATE auth_tokens SET last_used_at = ? WHERE token_hash = ?`,
			now.Format(time.RFC3339), h)
	}
	return author, nil
}

func (s *Store) ResolveAuthorName(ctx context.Context, nameOrEmail string) (*Author, error) {
	cleaned := strings.TrimSpace(strings.ToLower(nameOrEmail))
	if cleaned == "" {
		return nil, ErrNotFound
	}
	if colonIdx := strings.IndexByte(cleaned, ':'); colonIdx > 0 {
		a, err := s.Authenticate(ctx, cleaned)
		if err != nil {
			if errors.Is(err, ErrTokenRevoked) {
				return nil, ErrTokenRevoked
			}
			return nil, ErrNotFound
		}
		return a, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var a Author
	err := s.db.QueryRowContext(ctx, `
		SELECT id, display_name, git_email, role_badge, secret_key, invite_sent_at, invite_message_id
		FROM authors
		WHERE LOWER(display_name) = ? OR LOWER(id) = ? OR LOWER(git_email) = ?
	`, cleaned, cleaned, cleaned).Scan(&a.ID, &a.DisplayName, &a.GitEmail, &a.RoleBadge, &a.SecretKey, &a.InviteSentAt, &a.InviteMessageID)
	if err == nil {
		return &a, nil
	}

	var aliasAuthorID string
	if err := s.db.QueryRowContext(ctx, `
		SELECT author_id FROM auth_tokens
		WHERE raw_identifier = ?
		LIMIT 1
	`, cleaned+":redacted").Scan(&aliasAuthorID); err == nil {
		if fallback, ferr := s.queryAuthorByIDLocked(ctx, aliasAuthorID); ferr == nil {
			return fallback, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) ListAuthors(ctx context.Context) ([]Author, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, display_name, git_email, role_badge, secret_key, invite_sent_at, invite_message_id
		FROM authors
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Author
	for rows.Next() {
		var a Author
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.GitEmail, &a.RoleBadge, &a.SecretKey, &a.InviteSentAt, &a.InviteMessageID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) ListAuthorsPendingInvite(ctx context.Context) ([]Author, error) {
	authors, err := s.ListAuthors(ctx)
	if err != nil {
		return nil, err
	}
	var pending []Author
	for _, a := range authors {
		if strings.TrimSpace(a.InviteSentAt) == "" && strings.TrimSpace(a.SecretKey) != "" {
			pending = append(pending, a)
		}
	}
	return pending, nil
}

func (s *Store) MarkAuthorInviteSent(ctx context.Context, authorID, messageID, sentAtISO string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(sentAtISO) == "" {
		sentAtISO = time.Now().UTC().Format(time.RFC3339)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE authors
		SET invite_sent_at = ?, invite_message_id = ?
		WHERE id = ?
	`, sentAtISO, messageID, authorID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RevokeToken(ctx context.Context, rawTokenOrEmail string) error {
	h := HashToken(rawTokenOrEmail)
	if _, _, canon, ok := canonicalizeCredential(rawTokenOrEmail); ok {
		h = HashToken(canon)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var authorID string
	if err := s.db.QueryRowContext(ctx, `SELECT author_id FROM auth_tokens WHERE token_hash = ?`, h).Scan(&authorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE auth_tokens SET revoked = 1 WHERE author_id = ?`, authorID); err != nil {
		return err
	}
	return nil
}

func (s *Store) ReplaceAllChaptersFTS(ctx context.Context, docs []ChapterFTSDoc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM chapters_fts`); err != nil {
		return err
	}

	if len(docs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO chapters_fts (slug, title, headings, body_markdown)
			VALUES (?, ?, ?, ?)
		`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for _, doc := range docs {
			if _, err := stmt.ExecContext(ctx, doc.Slug, doc.Title, doc.Headings, doc.BodyMarkdown); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) BackupToFile(ctx context.Context, destPath string) error {
	destPath = strings.TrimSpace(destPath)
	if destPath == "" {
		return fmt.Errorf("backup sqlite: empty destination path")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, destPath)
	return err
}

func (s *Store) DeleteChapterFTS(ctx context.Context, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `DELETE FROM chapters_fts WHERE slug = ?`, strings.TrimSpace(slug))
	return err
}

func (s *Store) PruneStaleChapterFTS(ctx context.Context, activeSlugs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(activeSlugs) == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM chapters_fts`)
		return err
	}
	placeholders := make([]string, len(activeSlugs))
	args := make([]any, len(activeSlugs))
	for i, slug := range activeSlugs {
		placeholders[i] = "?"
		args[i] = slug
	}
	query := `DELETE FROM chapters_fts WHERE slug NOT IN (` + strings.Join(placeholders, ",") + `)`
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

func (s *Store) IndexChapterFTS(ctx context.Context, slug, title, headings, bodyMarkdown string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM chapters_fts WHERE slug = ?`, slug); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chapters_fts (slug, title, headings, body_markdown)
		VALUES (?, ?, ?, ?)
	`, slug, title, headings, bodyMarkdown); err != nil {
		return err
	}
	return tx.Commit()
}

const (
	maxFTSQueryRunes  = 512
	maxFTSQueryTokens = 24
)

func formatFTSToken(tok string) (formatted string, substantial bool) {
	runes := []rune(tok)
	if len(runes) == 0 {
		return "", false
	}
	if len(runes) == 1 && runes[0] < 128 {
		return `"` + tok + `"`, false
	}
	return `"` + tok + `"*`, true
}

func sanitizeFTSQuery(raw string) string {
	runes := []rune(raw)
	if len(runes) > maxFTSQueryRunes {
		runes = runes[:maxFTSQueryRunes]
	}
	var tokens []string
	hasSubstantial := false
	var cur strings.Builder
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			tok, sub := formatFTSToken(cur.String())
			if sub {
				hasSubstantial = true
			}
			tokens = append(tokens, tok)
			cur.Reset()
			if len(tokens) >= maxFTSQueryTokens {
				break
			}
		}
	}
	if cur.Len() > 0 && len(tokens) < maxFTSQueryTokens {
		tok, sub := formatFTSToken(cur.String())
		if sub {
			hasSubstantial = true
		}
		tokens = append(tokens, tok)
	}
	if !hasSubstantial {
		return ""
	}
	return strings.Join(tokens, " ")
}

func (s *Store) SearchFTS(ctx context.Context, rawQuery string, limit int, filterSlug ...string) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	ftsQuery := sanitizeFTSQuery(rawQuery)
	if ftsQuery == "" {
		return nil, nil
	}
	slugFilter := ""
	if len(filterSlug) > 0 {
		slugFilter = strings.TrimSpace(filterSlug[0])
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		SELECT slug, title,
		       snippet(chapters_fts, 3, '<mark>', '</mark>', ' … ', 28) AS snip,
		       -bm25(chapters_fts, 0.0, 5.0, 3.0, 1.0) AS score
		FROM chapters_fts
		WHERE chapters_fts MATCH ?
	`
	args := []any{ftsQuery}
	if slugFilter != "" {
		query += ` AND slug = ?`
		args = append(args, slugFilter)
	}
	query += `
		ORDER BY bm25(chapters_fts, 0.0, 5.0, 3.0, 1.0) ASC
		LIMIT ?
	`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.Slug, &h.Title, &h.Snippet, &h.BM25Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (s *Store) LogActivity(ctx context.Context, entry AuditEntry) error {
	if entry.TimestampISO == "" {
		entry.TimestampISO = time.Now().UTC().Format(TimestampMillisLayout)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_audit_log (
			timestamp_iso, author_id, protocol, user_agent, tool_name,
			chapter_slug, git_commit_sha, status, error_message, duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.TimestampISO, entry.AuthorID, entry.Protocol, entry.UserAgent, entry.ToolName,
		entry.ChapterSlug, entry.GitCommitSHA, entry.Status, entry.ErrorMessage, entry.DurationMs,
	)
	return err
}

func (s *Store) ListActivity(ctx context.Context, authorID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 30
	}
	if limit > 500 {
		limit = 500
	}
	authorID = strings.TrimSpace(authorID)

	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
		SELECT id, timestamp_iso, author_id, protocol, user_agent, tool_name,
		       chapter_slug, git_commit_sha, status, error_message, duration_ms
		FROM agent_audit_log
	`
	var args []any
	if authorID != "" {
		query += ` WHERE author_id = ?`
		args = append(args, authorID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(
			&e.ID, &e.TimestampISO, &e.AuthorID, &e.Protocol, &e.UserAgent, &e.ToolName,
			&e.ChapterSlug, &e.GitCommitSHA, &e.Status, &e.ErrorMessage, &e.DurationMs,
		); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CreateFeedback(ctx context.Context, rec FeedbackRecord) (*FeedbackRecord, error) {
	if rec.CreatedAtISO == "" {
		rec.CreatedAtISO = time.Now().UTC().Format(TimestampMillisLayout)
	}
	if rec.Status == "" {
		rec.Status = "open"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO platform_feedback (
			created_at, author_id, author_email, user_agent, kind,
			summary, details, chapter_slug, revision_sha, recent_context,
			resend_message_id, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		rec.CreatedAtISO, rec.AuthorID, rec.AuthorEmail, rec.UserAgent, rec.Kind,
		rec.Summary, rec.Details, rec.ChapterSlug, rec.RevisionSHA, rec.RecentContext,
		rec.ResendMessageID, rec.Status,
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	rec.ID = id
	return &rec, nil
}

func (s *Store) GetFeedback(ctx context.Context, id int64) (*FeedbackRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var r FeedbackRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT id, created_at, author_id, author_email, user_agent, kind,
		       summary, details, chapter_slug, revision_sha, recent_context,
		       resend_message_id, status
		FROM platform_feedback
		WHERE id = ?
	`, id).Scan(
		&r.ID, &r.CreatedAtISO, &r.AuthorID, &r.AuthorEmail, &r.UserAgent, &r.Kind,
		&r.Summary, &r.Details, &r.ChapterSlug, &r.RevisionSHA, &r.RecentContext,
		&r.ResendMessageID, &r.Status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) UpdateFeedbackMessageID(ctx context.Context, id int64, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		UPDATE platform_feedback
		SET resend_message_id = ?
		WHERE id = ?
	`, strings.TrimSpace(messageID), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountRecentFeedbackByAuthor(ctx context.Context, authorID string, since time.Time) (int, error) {
	cutoff := since.UTC().Format(TimestampMillisLayout)

	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM platform_feedback
		WHERE author_id = ? AND created_at >= ?
	`, strings.TrimSpace(authorID), cutoff).Scan(&count)
	return count, err
}

func (s *Store) CheckMCPFrictionWindow(ctx context.Context, authorID string, now time.Time) (*MCPFrictionWindowStats, error) {
	authorID = strings.TrimSpace(authorID)
	cutoff120 := now.UTC().Add(-120 * time.Second).Format(TimestampMillisLayout)
	cutoff180 := now.UTC().Add(-180 * time.Second).Format(TimestampMillisLayout)

	s.mu.Lock()
	defer s.mu.Unlock()

	stats := &MCPFrictionWindowStats{}

	if err := func() error {
		failRows, err := s.db.QueryContext(ctx, `
			SELECT id, timestamp_iso, author_id, protocol, user_agent, tool_name,
			       chapter_slug, git_commit_sha, status, error_message, duration_ms
			FROM agent_audit_log
			WHERE author_id = ?
			  AND protocol IN ('mcp', 'connectrpc')
			  AND tool_name NOT IN ('report_platform_feedback', 'ReportPlatformFeedback')
			  AND status IN ('conflict', 'invalid_arg', 'not_found', 'error')
			  AND timestamp_iso >= ?
			ORDER BY id DESC
		`, authorID, cutoff120)
		if err != nil {
			return err
		}
		defer func() { _ = failRows.Close() }()

		for failRows.Next() {
			var e AuditEntry
			if err := failRows.Scan(
				&e.ID, &e.TimestampISO, &e.AuthorID, &e.Protocol, &e.UserAgent, &e.ToolName,
				&e.ChapterSlug, &e.GitCommitSHA, &e.Status, &e.ErrorMessage, &e.DurationMs,
			); err != nil {
				return err
			}
			stats.FailedCalls120s++
			if len(stats.RecentFailures) < 6 {
				stats.RecentFailures = append(stats.RecentFailures, e)
			}
		}
		return failRows.Err()
	}(); err != nil {
		return nil, err
	}

	if err := func() error {
		writeRows, err := s.db.QueryContext(ctx, `
			SELECT chapter_slug
			FROM agent_audit_log
			WHERE author_id = ?
			  AND protocol IN ('mcp', 'connectrpc')
			  AND status = 'ok'
			  AND tool_name IN (
			      'patch_chapter', 'rename_chapter', 'create_chapter', 'delete_chapter', 'revert_chapter_revision',
			      'PatchChapter', 'RenameChapter', 'CreateChapter', 'DeleteChapter', 'RevertChapterRevision'
			  )
			  AND timestamp_iso >= ?
			ORDER BY id DESC
		`, authorID, cutoff180)
		if err != nil {
			return err
		}
		defer func() { _ = writeRows.Close() }()

		seenSlugs := make(map[string]struct{})
		for writeRows.Next() {
			var slug string
			if err := writeRows.Scan(&slug); err != nil {
				return err
			}
			stats.WriteCalls180s++
			slug = strings.TrimSpace(slug)
			if slug != "" {
				if _, exists := seenSlugs[slug]; !exists {
					seenSlugs[slug] = struct{}{}
					stats.RecentWriteSlugs = append(stats.RecentWriteSlugs, slug)
				}
			}
		}
		stats.DistinctWriteSlug = len(seenSlugs)
		return writeRows.Err()
	}(); err != nil {
		return nil, err
	}

	return stats, nil
}

func (s *Store) GetEditorialTimelineState(ctx context.Context) (*TimelineStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var st TimelineStateRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT last_head_sha, last_head_revision, model_name, updated_at
		FROM editorial_timeline_state
		WHERE singleton_id = 1
	`).Scan(&st.LastHeadSHA, &st.LastHeadRevision, &st.ModelName, &st.UpdatedAtISO)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// AppendEditorialTimelineEntries inserts zero or more immutable milestone rows and advances
// editorial_timeline_state inside a single SQLite transaction without ever mutating existing rows.
func (s *Store) AppendEditorialTimelineEntries(ctx context.Context, entries []TimelineEntryRecord, state TimelineStateRecord) ([]TimelineEntryRecord, error) {
	state.LastHeadSHA = strings.TrimSpace(state.LastHeadSHA)
	if state.LastHeadSHA == "" || state.LastHeadRevision <= 0 {
		return nil, fmt.Errorf("append timeline entries: valid last_head_sha and last_head_revision required")
	}
	nowISO := time.Now().UTC().Format(TimestampMillisLayout)
	if strings.TrimSpace(state.UpdatedAtISO) == "" {
		state.UpdatedAtISO = nowISO
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Insert oldest-to-newest within the batch so AUTOINCREMENT IDs increase monotonically with time.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		revs := e.Revisions
		if revs == nil {
			revs = []int32{}
		}
		slugs := e.ChapterSlugs
		if slugs == nil {
			slugs = []string{}
		}
		authors := e.Authors
		if authors == nil {
			authors = []string{}
		}
		revsJSON, err := json.Marshal(revs)
		if err != nil {
			return nil, err
		}
		slugsJSON, err := json.Marshal(slugs)
		if err != nil {
			return nil, err
		}
		authorsJSON, err := json.Marshal(authors)
		if err != nil {
			return nil, err
		}
		createdISO := strings.TrimSpace(e.CreatedAtISO)
		if createdISO == "" {
			createdISO = nowISO
		}
		commitCount := e.CommitCount
		if commitCount <= 0 {
			commitCount = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO editorial_timeline_entries (
				start_revision, end_revision, start_commit_sha, end_commit_sha,
				timestamp_iso, start_timestamp_iso, commit_count, revisions_json,
				title, summary, chapter_slugs_json, authors_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			e.StartRevision,
			e.EndRevision,
			strings.TrimSpace(e.StartCommitSHA),
			strings.TrimSpace(e.EndCommitSHA),
			strings.TrimSpace(e.TimestampISO),
			strings.TrimSpace(e.StartTimestampISO),
			commitCount,
			string(revsJSON),
			strings.TrimSpace(e.Title),
			strings.TrimSpace(e.Summary),
			string(slugsJSON),
			string(authorsJSON),
			createdISO,
		); err != nil {
			return nil, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO editorial_timeline_state (
			singleton_id, last_head_sha, last_head_revision, model_name, updated_at
		) VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			last_head_sha      = excluded.last_head_sha,
			last_head_revision = excluded.last_head_revision,
			model_name         = excluded.model_name,
			updated_at         = excluded.updated_at
	`,
		state.LastHeadSHA,
		state.LastHeadRevision,
		strings.TrimSpace(state.ModelName),
		state.UpdatedAtISO,
	); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.listEditorialTimelineEntriesLocked(ctx, 0)
}

func (s *Store) ListEditorialTimelineEntries(ctx context.Context, limit int) ([]TimelineEntryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listEditorialTimelineEntriesLocked(ctx, limit)
}

func (s *Store) listEditorialTimelineEntriesLocked(ctx context.Context, limit int) ([]TimelineEntryRecord, error) {
	query := `
		SELECT id, start_revision, end_revision, start_commit_sha, end_commit_sha,
		       timestamp_iso, start_timestamp_iso, commit_count, revisions_json,
		       title, summary, chapter_slugs_json, authors_json, created_at
		FROM editorial_timeline_entries
		ORDER BY end_revision DESC, id DESC
	`
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = s.db.QueryContext(ctx, query+` LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []TimelineEntryRecord
	for rows.Next() {
		var r TimelineEntryRecord
		var revsJSON, slugsJSON, authorsJSON string
		if err := rows.Scan(
			&r.ID,
			&r.StartRevision,
			&r.EndRevision,
			&r.StartCommitSHA,
			&r.EndCommitSHA,
			&r.TimestampISO,
			&r.StartTimestampISO,
			&r.CommitCount,
			&revsJSON,
			&r.Title,
			&r.Summary,
			&slugsJSON,
			&authorsJSON,
			&r.CreatedAtISO,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(revsJSON), &r.Revisions); err != nil {
			slog.Warn("timeline_revisions_json_unmarshal_failed", "id", r.ID, "error", err)
		}
		if err := json.Unmarshal([]byte(slugsJSON), &r.ChapterSlugs); err != nil {
			slog.Warn("timeline_slugs_json_unmarshal_failed", "id", r.ID, "error", err)
		}
		if err := json.Unmarshal([]byte(authorsJSON), &r.Authors); err != nil {
			slog.Warn("timeline_authors_json_unmarshal_failed", "id", r.ID, "error", err)
		}
		if r.Revisions == nil {
			r.Revisions = []int32{}
		}
		if r.ChapterSlugs == nil {
			r.ChapterSlugs = []string{}
		}
		if r.Authors == nil {
			r.Authors = []string{}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
