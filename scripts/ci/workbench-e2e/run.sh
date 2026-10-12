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

home="$WORK/home" ws="$WORK/ws"
rm -rf "$home" "$ws"
mkdir -p "$home/.abhed" "$ws"
cat > "$home/.abhed/config.json" <<EOF
{"model":{"default":"fake","providers":{"fake":{"type":"openai-compatible","base_url":"http://127.0.0.1:${MODEL_PORT}/v1","model":"workbench-e2e","context_window":32768}}},
 "auth":{"mode":"local","allow_signup":true,"users_file":"$home/.abhed/users.json"},
 "sandbox":{"min_tier":"none"}}
EOF

node "$WORK/harness/fake-model.mjs" "$MODEL_PORT" > "$WORK/model.log" 2>&1 &
pids+=($!)
env -i PATH="$PATH" HOME="$home" TMPDIR="${TMPDIR:-/tmp}" LANG=C.UTF-8 \
  "$WORK/abhed" -C "$ws" serve -addr "127.0.0.1:${SERVE_PORT}" > "$WORK/serve.log" 2>&1 &
pids+=($!)

for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:${SERVE_PORT}/v1/health" >/dev/null 2>&1 && break
  sleep 0.2
done
if ! curl -fsS "http://127.0.0.1:${SERVE_PORT}/v1/health" >/dev/null; then
  echo "abhed serve did not come up:"; cat "$WORK/serve.log"; exit 1
fi

status=0
(cd "$WORK/harness" && node checks.mjs "http://127.0.0.1:${SERVE_PORT}" "$ws") || status=$?
if [ "$status" -ne 0 ]; then
  echo "--- serve log"; tail -n 50 "$WORK/serve.log"
fi
exit "$status"
