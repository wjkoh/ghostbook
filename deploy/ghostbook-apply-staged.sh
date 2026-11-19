#!/usr/bin/env bash
set -euo pipefail

# /var/lib/ghostbook/staged MUST be owned by root:root (mode 0700) so the unprivileged
# ghostbook service user cannot write or replace staged binaries promoted by root.
STAGED_DIR="/var/lib/ghostbook/staged"
STAGED_BIN="${STAGED_DIR}/ghostbook.staged"
STAGED_META="${STAGED_DIR}/ghostbook.staged.meta"
LIVE_BIN="/usr/local/bin/ghostbook"
PREV_BIN="/usr/local/bin/ghostbook.prev"
DB_PATH="/var/lib/ghostbook/data/book_studio.db"
SERVICE_USER="${GHOSTBOOK_USER:-ghostbook}"
HEALTHZ_URL="${HEALTHZ_URL:-http://127.0.0.1:8090/healthz}"
IDLE_WINDOW_SECONDS="${IDLE_WINDOW_SECONDS:-900}"
LOCK_FILE="/run/ghostbook-upgrade.lock"
FORCE="${1:-}"

if ! [[ "$IDLE_WINDOW_SECONDS" =~ ^[0-9]+$ ]]; then
  echo "[ghostbook-upgrade] ERROR: IDLE_WINDOW_SECONDS must be a non-negative integer, got '$IDLE_WINDOW_SECONDS'." >&2
  exit 1
fi

exec 9>"$LOCK_FILE"
if ! flock -n 9; then
  echo "[ghostbook-upgrade] Another upgrade process is already running; exiting."
  exit 0
fi

if [ ! -e "$STAGED_BIN" ]; then
  echo "[ghostbook-upgrade] No staged binary queued at $STAGED_BIN; nothing to do."
  exit 0
fi

# Verify staged binary is a regular file (never a symlink) owned by root
if [ -L "$STAGED_BIN" ] || [ ! -f "$STAGED_BIN" ]; then
  echo "[ghostbook-upgrade] ERROR: $STAGED_BIN must be a regular file (symlinks rejected)." >&2
  exit 1
fi
STAGED_UID=$(stat -c '%u' "$STAGED_BIN")
if [ "$STAGED_UID" -ne 0 ]; then
  echo "[ghostbook-upgrade] ERROR: $STAGED_BIN must be owned by root (uid 0), got uid $STAGED_UID." >&2
  exit 1
fi

# Copy to a root-owned temp file to close any TOCTOU window between checksum and install
TMP_BIN=$(mktemp /run/ghostbook.promote.XXXXXX)
trap 'rm -f "$TMP_BIN"' EXIT
cp -- "$STAGED_BIN" "$TMP_BIN"
chmod 0755 "$TMP_BIN"

STAGED_SHA=$(sha256sum "$TMP_BIN" | awk '{print $1}')

if [ -f "$STAGED_META" ] && [ ! -L "$STAGED_META" ] && [ "$(stat -c '%u' "$STAGED_META")" -eq 0 ]; then
  META_SHA=$(awk '/^[0-9a-f]{64}[[:space:]]/ {print $1; exit}' "$STAGED_META" || true)
  if [ -n "$META_SHA" ] && [ "$META_SHA" != "$STAGED_SHA" ]; then
    echo "[ghostbook-upgrade] ERROR: staged binary SHA-256 ($STAGED_SHA) does not match metadata ($META_SHA)." >&2
    exit 1
  fi
fi

LIVE_SHA=""
if [ -f "$LIVE_BIN" ]; then
  LIVE_SHA=$(sha256sum "$LIVE_BIN" | awk '{print $1}')
fi

if [ "$STAGED_SHA" = "$LIVE_SHA" ]; then
  echo "[ghostbook-upgrade] Staged binary SHA-256 ($STAGED_SHA) already matches live binary; clearing queue without restart."
  rm -f "$STAGED_BIN" "$STAGED_META"
  exit 0
fi

if [ "$FORCE" != "--force" ] && [ -f "$DB_PATH" ]; then
  if ! RECENT_CALLS=$(runuser -u "$SERVICE_USER" -- sqlite3 -readonly -cmd ".timeout 5000" "$DB_PATH" "SELECT COUNT(*) FROM agent_audit_log WHERE timestamp_iso >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-${IDLE_WINDOW_SECONDS} seconds');" 2>/dev/null); then
    echo "[ghostbook-upgrade] Deferring upgrade: SQLite audit check busy or unavailable (failing closed)."
    exit 0
  fi
  RECENT_CALLS=$(echo "$RECENT_CALLS" | tail -n 1 | tr -d '[:space:]')
  if ! [[ "$RECENT_CALLS" =~ ^[0-9]+$ ]]; then
    echo "[ghostbook-upgrade] Deferring upgrade: unexpected SQLite audit result '$RECENT_CALLS' (failing closed)."
    exit 0
  fi
  if [ "$RECENT_CALLS" -gt 0 ]; then
    echo "[ghostbook-upgrade] Deferring upgrade: $RECENT_CALLS MCP/ConnectRPC tool call(s) recorded within last ${IDLE_WINDOW_SECONDS}s."
    exit 0
  fi
fi

echo "[ghostbook-upgrade] Idle window confirmed. Applying staged binary ($STAGED_SHA)..."
if [ -f "$LIVE_BIN" ]; then
  cp -f "$LIVE_BIN" "$PREV_BIN"
fi

install -m 0755 -o root -g root "$TMP_BIN" "$LIVE_BIN"
systemctl restart ghostbook.service

HEALTH_OK=0
for _ in $(seq 1 15); do
  if curl -sf "$HEALTHZ_URL" >/dev/null 2>&1; then
    HEALTH_OK=1
    break
  fi
  sleep 1
done

if [ "$HEALTH_OK" -ne 1 ]; then
  CURRENT_STAGED_SHA=$(sha256sum "$STAGED_BIN" 2>/dev/null | awk '{print $1}' || true)
  if [ "$CURRENT_STAGED_SHA" = "$STAGED_SHA" ]; then
    QUARANTINE_BIN="${STAGED_BIN}.failed.${STAGED_SHA:0:12}"
    mv -f "$STAGED_BIN" "$QUARANTINE_BIN" 2>/dev/null || rm -f "$STAGED_BIN"
    rm -f "$STAGED_META"
    echo "[ghostbook-upgrade] ERROR: $HEALTHZ_URL failed after restart; quarantined binary at $QUARANTINE_BIN and rolling back to $PREV_BIN." >&2
  else
    echo "[ghostbook-upgrade] ERROR: $HEALTHZ_URL failed after restart; rolling back to $PREV_BIN (preserving newer staged binary)." >&2
  fi
  if [ -f "$PREV_BIN" ]; then
    install -m 0755 -o root -g root "$PREV_BIN" "$LIVE_BIN"
    systemctl restart ghostbook.service
    for _ in $(seq 1 10); do
      if curl -sf "$HEALTHZ_URL" >/dev/null 2>&1; then
        echo "[ghostbook-upgrade] Rollback to $PREV_BIN healthy." >&2
        break
      fi
      sleep 1
    done
  fi
  exit 1
fi

CURRENT_STAGED_SHA=$(sha256sum "$STAGED_BIN" 2>/dev/null | awk '{print $1}' || true)
if [ "$CURRENT_STAGED_SHA" = "$STAGED_SHA" ]; then
  rm -f "$STAGED_BIN" "$STAGED_META"
fi
echo "[ghostbook-upgrade] Promoted staged binary ($STAGED_SHA) and verified $HEALTHZ_URL."
