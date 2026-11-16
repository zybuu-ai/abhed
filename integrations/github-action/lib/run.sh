#!/usr/bin/env bash
# Runs one headless Abhed task and keeps its evidence. The run's exit code is
# written to GITHUB_OUTPUT, not returned, so the evidence is uploaded before
# the job fails on it.
#
# The agent runs under `env -i`: it is given PATH, a home, the locale, the
# model key's variable and the names in ABHED_ENV_PASSTHROUGH, and nothing
# else of the runner's environment, so no GitHub token reaches it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail() { echo "::error::abhed: $*" >&2; exit 2; }

bin="${ABHED_BIN:?}"
ws="$(cd "${ABHED_WORKSPACE:-.}" && pwd)"
ev="${ABHED_EVIDENCE_DIR:?}"
mkdir -p "$ev"
chmod 700 "$ev"

# A token actions/checkout left in the checkout's git configuration is
# readable by the agent; refuse to run beside it unless told otherwise.
if [ "${ABHED_ALLOW_PERSISTED_CREDENTIALS:-false}" != "true" ] && git -C "$ws" rev-parse --git-dir >/dev/null 2>&1; then
  if git -C "$ws" config --includes --get-regexp '^http\..*\.extraheader$' >/dev/null 2>&1; then
    fail "the checkout keeps a GitHub token in its git configuration, where the agent could read it. Check out with persist-credentials: false (or set allow-persisted-credentials: true to accept it)"
  fi
fi

args=(-C "$ws" -p -no-stdin -output-format stream-json)
[ -n "${ABHED_STDIN_FILE:-}" ] && args=(-C "$ws" -p -output-format stream-json)
[ -n "${ABHED_PERMISSION_MODE:-}" ] && args+=(-mode "$ABHED_PERMISSION_MODE")
[ -n "${ABHED_VERIFY:-}" ] && args+=(-verify "$ABHED_VERIFY")
[ -n "${ABHED_SETTINGS:-}" ] && args+=(-settings "$ABHED_SETTINGS")
[ -n "${ABHED_MAX_TURNS:-}" ] && args+=(-max-turns "$ABHED_MAX_TURNS")
[ -n "${ABHED_MAX_BUDGET_USD:-}" ] && args+=(-max-budget-usd "$ABHED_MAX_BUDGET_USD")
[ -n "${ABHED_TOOLS:-}" ] && args+=(-tools "$ABHED_TOOLS")
[ "${ABHED_BARE:-false}" = "true" ] && args+=(-bare)
[ -n "${ABHED_JSON_SCHEMA:-}" ] && args+=(-json-schema "$ABHED_JSON_SCHEMA")
# One rule per line; a line may hold several, comma-separated, as -allow takes them.
while IFS= read -r rule; do
  rule="${rule#"${rule%%[![:space:]]*}"}"; rule="${rule%"${rule##*[![:space:]]}"}"
  [ -n "$rule" ] && args+=(-allow "$rule")
done <<< "${ABHED_ALLOW:-}"
while IFS= read -r rule; do
  rule="${rule#"${rule%%[![:space:]]*}"}"; rule="${rule%"${rule##*[![:space:]]}"}"
  [ -n "$rule" ] && args+=(-deny "$rule")
done <<< "${ABHED_DENY:-}"
[ -n "${ABHED_TASK:-}" ] || fail "no task given"
args+=(-- "$ABHED_TASK")

# The run's own home, so the runner's ~/.abhed (a self-hosted runner's
# configuration, say) neither loosens nor mixes with this run's record.
home="$HOME"
if [ "${ABHED_ISOLATE_HOME:-true}" = "true" ]; then
  home="${RUNNER_TEMP:-/tmp}/abhed-home"
  mkdir -p "$home"
  chmod 700 "$home"
fi

envs=("PATH=$PATH" "HOME=$home" "LANG=${LANG:-C.UTF-8}" "TMPDIR=${RUNNER_TEMP:-${TMPDIR:-/tmp}}" "CI=true")
[ -n "${USER:-}" ] && envs+=("USER=$USER")
[ -n "${ABHED_KEY_ENV:-}" ] && envs+=("$ABHED_KEY_ENV=${ABHED_ACTION_MODEL_KEY:-}")
for name in ${ABHED_ENV_PASSTHROUGH//,/ }; do
  [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail "env-passthrough: $name is not a variable name"
  case "$name" in
    GITHUB_TOKEN|GH_TOKEN|ACTIONS_*|*_REQUEST_TOKEN|*RUNTIME_TOKEN) fail "env-passthrough: $name is a GitHub credential and is never passed to the agent" ;;
  esac
  [ -n "${!name+x}" ] && envs+=("$name=${!name}")
done

"$bin" -version > "$ev/abhed-version.txt" 2>&1 || true
code=0
if [ -n "${ABHED_STDIN_FILE:-}" ]; then
  env -i "${envs[@]}" "$bin" "${args[@]}" < "$ABHED_STDIN_FILE" > "$ev/run.jsonl" 2> "$ev/stderr.log" || code=$?
else
  env -i "${envs[@]}" "$bin" "${args[@]}" < /dev/null > "$ev/run.jsonl" 2> "$ev/stderr.log" || code=$?
fi
# stderr holds Abhed's notes (each refusal and what would allow it), never the key.
cat "$ev/stderr.log" >&2 || true

sid="$(python3 "$here/abhed_action.py" result --run "$ev/run.jsonl" --evidence "$ev" --exit-code "$code" --print-session)"
if [ -n "$sid" ]; then
  # The record, as recorded and chained, and a transcript to read. The copy
  # verifies with `abhed record verify record.jsonl` anywhere.
  env -i "${envs[@]}" "$bin" -C "$ws" record export "$sid" -o "$ev/record.jsonl" > /dev/null 2>> "$ev/export.log" || echo "::warning::abhed: the record could not be exported; see export.log"
  env -i "${envs[@]}" "$bin" -C "$ws" record export "$sid" -format html -o "$ev/transcript.html" > /dev/null 2>> "$ev/export.log" || true
  if [ -f "$ev/record.jsonl" ]; then
    env -i "${envs[@]}" "$bin" record verify "$ev/record.jsonl" > "$ev/record-verify.txt" 2>&1 || echo "::warning::abhed: the exported record did not verify; see record-verify.txt"
  fi
fi
echo "abhed exited $code"
