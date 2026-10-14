#!/usr/bin/env bash
# Run the workbench's browser checks against a freshly built abhed serve, with
# a scripted model, a throwaway HOME and workspace, and nothing outside the
# machine. Needs Go, node and python3; Playwright's Chromium is installed into
# the work folder unless PLAYWRIGHT_BROWSERS_PATH names one.
#
#   scripts/ci/workbench-e2e/run.sh [WORKDIR]
#
# MODEL_PORT and SERVE_PORT choose the loopback ports (8851 and 8852);
# WITH_DEPS=1 also installs Chromium's system libraries (CI, with sudo).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
WORK="${1:-$(mktemp -d)}"
mkdir -p "$WORK"
WORK="$(cd "$WORK" && pwd)"
MODEL_PORT="${MODEL_PORT:-8851}"
SERVE_PORT="${SERVE_PORT:-8852}"
export PLAYWRIGHT_BROWSERS_PATH="${PLAYWRIGHT_BROWSERS_PATH:-$WORK/browsers}"

pids=()
# shellcheck disable=SC2317 # run by the trap
cleanup() {
  for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done
  for p in ${pids[@]+"${pids[@]}"}; do wait "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

echo "building abhed"
(cd "$ROOT" && python3 scripts/docsite/build.py --embed-only >/dev/null && go build -o "$WORK/abhed" ./cmd/abhed)

echo "installing the pinned Playwright and its Chromium"
rm -rf "$WORK/harness" && mkdir -p "$WORK/harness"
cp "$HERE/package.json" "$HERE/package-lock.json" "$HERE"/*.mjs "$WORK/harness/"
(cd "$WORK/harness" && npm ci --no-audit --no-fund --silent && npx --no-install playwright install ${WITH_DEPS:+--with-deps} chromium >/dev/null)

node "$WORK/harness/fake-model.mjs" "$MODEL_PORT" > "$WORK/model.log" 2>&1 &
pids+=($!)

# Each scenario gets a fresh HOME, workspace and server: per-session is the
# default, where a session's commands run in a sandbox of its own; shared sets
# server.shared_sessions, where they run in the one workspace.
status=0
for scenario in per-session shared; do
  echo "--- scenario: $scenario"
  home="$WORK/$scenario/home" ws="$WORK/$scenario/ws"
  rm -rf "${WORK:?}/$scenario"
  mkdir -p "$home/.abhed" "$ws"
  shared=false
  if [ "$scenario" = shared ]; then shared=true; fi
  # A user_prompt_submit hook that refuses messages saying "forbidden".
  cat > "$WORK/$scenario/gate.sh" <<'GATE'
#!/bin/bash
while IFS= read -r line; do
  case "$line" in
    *'"event":"user_prompt_submit"'*forbidden*) echo '{"block":true,"reason":"that topic is off limits"}' ;;
    *) echo '{}' ;;
  esac
done
GATE
  cat > "$home/.abhed/config.json" <<EOF
{"model":{"default":"fake","providers":{"fake":{"type":"openai-compatible","base_url":"http://127.0.0.1:${MODEL_PORT}/v1","model":"workbench-e2e","context_window":32768}}},
 "auth":{"mode":"local","allow_signup":true,"users_file":"$home/.abhed/users.json"},
 "server":{"shared_sessions":$shared},
 "extensions":[{"name":"gate","command":"bash","args":["$WORK/$scenario/gate.sh"],"events":["user_prompt_submit"]}],
 "sandbox":{"min_tier":"none"}}
EOF

  env -i PATH="$PATH" HOME="$home" TMPDIR="${TMPDIR:-/tmp}" LANG=C.UTF-8 \
    "$WORK/abhed" -C "$ws" serve -addr "127.0.0.1:${SERVE_PORT}" > "$WORK/$scenario/serve.log" 2>&1 &
  serve=$!
  pids+=("$serve")

  for _ in $(seq 1 100); do
    curl -fsS "http://127.0.0.1:${SERVE_PORT}/v1/health" >/dev/null 2>&1 && break
    sleep 0.2
  done
  if ! curl -fsS "http://127.0.0.1:${SERVE_PORT}/v1/health" >/dev/null; then
    echo "abhed serve did not come up:"; cat "$WORK/$scenario/serve.log"; exit 1
  fi

  rc=0
  (cd "$WORK/harness" && node checks.mjs "http://127.0.0.1:${SERVE_PORT}" "$ws" "$scenario") || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "--- serve log ($scenario)"; tail -n 50 "$WORK/$scenario/serve.log"
    status=1
  fi
  kill "$serve" 2>/dev/null || true
  wait "$serve" 2>/dev/null || true
done
exit "$status"
