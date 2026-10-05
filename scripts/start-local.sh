#!/usr/bin/env bash
# Start the local Abhed stack: Postgres -> Ollama -> Abhed server.
# Safe to re-run: every step is idempotent and skips what is already up.
#
#   ./scripts/start-local.sh          # start everything
#   ./scripts/start-local.sh --stop   # stop Abhed and Ollama (leaves Postgres)

set -uo pipefail

# Everything lives inside this repo: /tmp is purged by macOS, which is what
# silently destroyed the old /tmp/abhed-test workspace.
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE="$REPO/.abhed-workspace"
LOGDIR="$REPO/.abhed-workspace/logs"
ADDR=:8420
PORT=8420
PGDATA=/opt/homebrew/var/postgresql@16

mkdir -p "$LOGDIR"

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mok\033[0m   %s\n' "$*"; }
warn() { printf '  \033[33mwarn\033[0m %s\n' "$*"; }
die()  { printf '  \033[31mfail\033[0m %s\n' "$*"; exit 1; }

# Each process this script starts has its PID written here, and --stop ends
# only those PIDs: a kill by name also ended processes it never started.
pidfile() { echo "$LOGDIR/$1.pid"; }
stop_started() { # name, the command word the PID must still be running
  local f pid
  f="$(pidfile "$1")"
  pid="$(cat "$f" 2>/dev/null)"
  if [[ "$pid" =~ ^[0-9]+$ ]] && ps -p "$pid" -o args= 2>/dev/null | grep -q "$2"; then
    kill "$pid" && ok "$1 stopped (pid $pid)"
  else
    warn "$1 was not started by this script, or has ended"
  fi
  rm -f "$f"
}

if [[ "${1:-}" == "--stop" ]]; then
  say "Stopping Abhed stack"
  stop_started abhed "abhed serve"
  stop_started ollama "ollama serve"
  echo "  (Postgres left running: brew services stop postgresql@16)"
  exit 0
fi

# 1. Postgres ---------------------------------------------------------------
say "1/4  Postgres"
if pg_isready -q; then
  ok "already accepting connections"
else
  # A crash or a killed postmaster can leave a stale lock that blocks startup.
  # Only remove it when no postmaster is actually running.
  if [[ -f "$PGDATA/postmaster.pid" ]] && ! pgrep -qf "[p]ostgres -D"; then
    warn "stale postmaster.pid found (no postgres running) - removing"
    rm -f "$PGDATA/postmaster.pid"
  fi
  brew services restart postgresql@16 >/dev/null 2>&1
  for _ in $(seq 1 15); do pg_isready -q && break; sleep 1; done
  pg_isready -q && ok "started" || die "did not come up - tail /opt/homebrew/var/log/postgresql@16.log"
fi

# 2. Ollama -----------------------------------------------------------------
say "2/4  Ollama"
if curl -sf --max-time 3 http://127.0.0.1:11434/api/version >/dev/null; then
  ok "already listening on :11434"
else
  # Flash attention + a q8 KV cache are what let a 26B model hold a long
  # context in 36 GB. Do not drop them.
  OLLAMA_FLASH_ATTENTION=1 OLLAMA_KV_CACHE_TYPE=q8_0 \
     nohup ollama serve > "$LOGDIR/ollama.log" 2>&1 &
  echo $! > "$(pidfile ollama)"
  disown
  for _ in $(seq 1 20); do
    curl -sf --max-time 2 http://127.0.0.1:11434/api/version >/dev/null && break
    sleep 1
  done
  curl -sf --max-time 3 http://127.0.0.1:11434/api/version >/dev/null \
    && ok "started (log: $LOGDIR/ollama.log)" || die "did not come up - tail $LOGDIR/ollama.log"
fi

# 3. Workspace --------------------------------------------------------------
# The workspace is committed-adjacent (gitignored) inside the repo, so it
# survives reboots. A missing config here means it was deleted by hand.
say "3/4  Workspace $WORKSPACE"
if [[ -f "$WORKSPACE/.abhed/config.json" ]]; then
  ok "config present"
else
  warn "no .abhed/config.json - scaffolding a new one"
  mkdir -p "$WORKSPACE"
  ( cd "$WORKSPACE" && abhed init >/dev/null 2>&1 )
  [[ -f "$WORKSPACE/.abhed/config.json" ]] \
    && ok "scaffold written - re-add storage.dsn, model and skills settings" \
    || die "abhed init did not produce a config"
fi

# 4. Abhed server -----------------------------------------------------------
say "4/4  Abhed server"
if lsof -nP -iTCP:$PORT -sTCP:LISTEN >/dev/null 2>&1; then
  if pgrep -qf "abhed serve"; then
    ok "already listening on $ADDR"
  else
    die "port $PORT is taken by something that is not abhed - lsof -nP -iTCP:$PORT -sTCP:LISTEN"
  fi
else
  ( cd "$WORKSPACE" && exec nohup abhed serve -addr "$ADDR" > "$LOGDIR/abhed-serve.log" 2>&1 ) &
  echo $! > "$(pidfile abhed)"
  disown
  for _ in $(seq 1 20); do
    curl -sf --max-time 2 "http://127.0.0.1:$PORT/v1/health" >/dev/null && break
    sleep 1
  done
  curl -sf --max-time 3 "http://127.0.0.1:$PORT/v1/health" >/dev/null \
    && ok "started (log: $LOGDIR/abhed-serve.log)" || die "did not come up - tail $LOGDIR/abhed-serve.log"
fi

say "Health"
curl -s "http://127.0.0.1:$PORT/v1/health"; echo
echo
echo "  Web UI   http://localhost:$PORT"
echo "  Verify   cd $WORKSPACE && abhed doctor"
echo "  Stop     $0 --stop"
