#!/usr/bin/env bash
set -euo pipefail

STAGED_BIN="/var/lib/ghostbook/staged/ghostbook.staged"
STAGED_META="/var/lib/ghostbook/staged/ghostbook.staged.meta"
LIVE_BIN="/usr/local/bin/ghostbook"
PREV_BIN="/usr/local/bin/ghostbook.prev"
DB_PATH="/var/lib/ghostbook/data/book_studio.db"
SERVICE_USER="${GHOSTBOOK_USER:-ghostbook}"
IDLE_WINDOW_SECONDS="${IDLE_WINDOW_SECONDS:-900}"
FORCE="${1:-}"

if [ ! -f "$STAGED_BIN" ]; then
  echo "[ghostbook-upgrade] No staged binary queued at $STAGED_BIN; nothing to do."
  exit 0
fi

STAGED_SHA=$(sha256sum "$STAGED_BIN" | awk '{print $1}')
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
  if [ "${RECENT_CALLS:-1}" -gt 0 ]; then
    echo "[ghostbook-upgrade] Deferring upgrade: $RECENT_CALLS MCP/ConnectRPC tool call(s) recorded within last ${IDLE_WINDOW_SECONDS}s."
    exit 0
  fi
fi

echo "[ghostbook-upgrade] Idle window confirmed. Applying staged binary ($STAGED_SHA)..."
if [ -f "$LIVE_BIN" ]; then
  cp -f "$LIVE_BIN" "$PREV_BIN"
fi

install -m 0755 -o root -g root "$STAGED_BIN" "$LIVE_BIN"
systemctl restart ghostbook.service

HEALTH_OK=0
for _ in $(seq 1 15); do
  if curl -sf http://127.0.0.1:8090/healthz >/dev/null 2>&1; then
    HEALTH_OK=1
    break
  fi
  sleep 1
done

if [ "$HEALTH_OK" -ne 1 ]; then
  QUARANTINE_BIN="${STAGED_BIN}.failed.${STAGED_SHA:0:12}"
  mv -f "$STAGED_BIN" "$QUARANTINE_BIN" 2>/dev/null || rm -f "$STAGED_BIN"
  rm -f "$STAGED_META"
  echo "[ghostbook-upgrade] ERROR: /healthz failed after restart; quarantined binary at $QUARANTINE_BIN and rolling back to $PREV_BIN." >&2
  if [ -f "$PREV_BIN" ]; then
    install -m 0755 -o root -g root "$PREV_BIN" "$LIVE_BIN"
    systemctl restart ghostbook.service
    for _ in $(seq 1 10); do
      if curl -sf http://127.0.0.1:8090/healthz >/dev/null 2>&1; then
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
echo "[ghostbook-upgrade] Promoted staged binary ($STAGED_SHA) and verified /healthz."
