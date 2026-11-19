CREATE TABLE IF NOT EXISTS authors (
    id                TEXT PRIMARY KEY,          -- e.g. "alice", "bob", "carol"
    display_name      TEXT NOT NULL,             -- e.g. "Alice Chen"
    git_email         TEXT NOT NULL UNIQUE,      -- e.g. "alice@example.com"
    role_badge        TEXT NOT NULL DEFAULT 'Co-Author',
    secret_key        TEXT NOT NULL DEFAULT '',  -- Per-author random secret key for /u/{email}:{secret-key}/...
    revoked           INTEGER NOT NULL DEFAULT 0, -- 1 when author credentials have been revoked
    invite_sent_at    TEXT NOT NULL DEFAULT '',  -- RFC3339 timestamp when onboarding email was sent via Resend
    invite_message_id TEXT NOT NULL DEFAULT '',  -- Resend message ID for audit/idempotency
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS auth_tokens (
    token_hash      TEXT PRIMARY KEY,          -- SHA-256 hex digest of canonical <alias>:<secret_key>
    raw_identifier  TEXT NOT NULL,             -- Redacted label (<alias>:redacted)
    author_id       TEXT NOT NULL REFERENCES authors(id),
    client_label    TEXT NOT NULL,             -- e.g. "Alice Chen (alice)"
    revoked         INTEGER NOT NULL DEFAULT 0,
    last_used_at    TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- SQLite FTS5 Virtual Table for BM25 Full-Text Search across chapters
CREATE VIRTUAL TABLE IF NOT EXISTS chapters_fts USING fts5(
    slug UNINDEXED,
    title,
    headings,
    body_markdown,
    tokenize = 'porter unicode61'
);

-- Immutable Audit Trail of every MCP and ConnectRPC tool invocation (both reads and writes)
CREATE TABLE IF NOT EXISTS agent_audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp_iso   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    author_id       TEXT NOT NULL,
    protocol        TEXT NOT NULL,             -- "mcp" or "connectrpc"
    user_agent      TEXT NOT NULL,             -- Distinguishes Claude, ChatGPT, Gemini, Cursor, and CLI agents
    tool_name       TEXT NOT NULL,             -- e.g. "read_chapter", "patch_chapter"
    chapter_slug    TEXT NOT NULL DEFAULT '',
    git_commit_sha  TEXT NOT NULL DEFAULT '',  -- Resulting commit SHA if write succeeded
    status          TEXT NOT NULL,             -- "ok", "conflict", "invalid_arg", "not_found", "error"
    error_message   TEXT NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON agent_audit_log(timestamp_iso DESC);
CREATE INDEX IF NOT EXISTS idx_audit_author_id_desc ON agent_audit_log(author_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_author_timestamp ON agent_audit_log(author_id, timestamp_iso DESC);

-- Platform bug reports, UX friction notes, and feature requests submitted via report_platform_feedback
CREATE TABLE IF NOT EXISTS platform_feedback (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    author_id         TEXT NOT NULL,
    author_email      TEXT NOT NULL DEFAULT '',
    user_agent        TEXT NOT NULL DEFAULT '',
    kind              TEXT NOT NULL CHECK (kind IN ('bug', 'feature_request', 'ux_friction')),
    summary           TEXT NOT NULL,
    details           TEXT NOT NULL DEFAULT '',
    chapter_slug      TEXT NOT NULL DEFAULT '',
    revision_sha      TEXT NOT NULL DEFAULT '',
    recent_context    TEXT NOT NULL DEFAULT '',
    resend_message_id TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved'))
);

CREATE INDEX IF NOT EXISTS idx_platform_feedback_author_created
    ON platform_feedback(author_id, created_at DESC);

-- Immutable append-only AI editorial timeline entries and watermark state
CREATE TABLE IF NOT EXISTS editorial_timeline_entries (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    start_revision      INTEGER NOT NULL,
    end_revision        INTEGER NOT NULL,
    start_commit_sha    TEXT NOT NULL DEFAULT '',
    end_commit_sha      TEXT NOT NULL DEFAULT '',
    timestamp_iso       TEXT NOT NULL,
    start_timestamp_iso TEXT NOT NULL DEFAULT '',
    commit_count        INTEGER NOT NULL DEFAULT 1,
    revisions_json      TEXT NOT NULL DEFAULT '[]',
    title               TEXT NOT NULL,
    summary             TEXT NOT NULL,
    chapter_slugs_json  TEXT NOT NULL DEFAULT '[]',
    authors_json        TEXT NOT NULL DEFAULT '[]',
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_timeline_end_rev
    ON editorial_timeline_entries(end_revision DESC, id DESC);

CREATE TABLE IF NOT EXISTS editorial_timeline_state (
    singleton_id       INTEGER PRIMARY KEY CHECK (singleton_id = 1),
    last_head_sha      TEXT NOT NULL,
    last_head_revision INTEGER NOT NULL,
    model_name         TEXT NOT NULL DEFAULT '',
    updated_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
