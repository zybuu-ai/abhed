#!/usr/bin/env bash
# Runs one headless Abhed task and keeps its evidence. The run's exit code is
# written to GITHUB_OUTPUT, not returned, so the evidence is uploaded before
# the job fails on it.
#
# The agent runs under `env -i`: it is given PATH, a home, the locale, the
# model key's variable and the names in ABHED_ENV_PASSTHROUGH, and nothing
# else of the runner's environment, so no GitHub token reaches it. Its home
# and temp folder are fresh, outside the runner's temp folder, so a command
# the sandbox lets write its temp folder cannot reach the runner's file
# commands, the evidence or the installed binary.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# fd 3 is the step's stderr, which a redirected command's failure still reaches.
exec 3>&2
fail() { echo "::error::abhed: $*" >&3; exit 2; }

bin="${ABHED_BIN:?}"
ws="$(cd "${ABHED_WORKSPACE:-.}" && pwd)"
ev="${ABHED_EVIDENCE_DIR:?}"
mkdir -p "$ev"
chmod 700 "$ev"

. "$here/digest.sh"
# The binary and its helpers are checked before every run of them: what the
# agent's run could have reached must not decide what runs after it.
want_sha="${ABHED_BIN_SHA256:-$(digest "$bin")}"
check_bin() { [ "$(digest "$bin")" = "$want_sha" ] || fail "$bin or a helper beside it changed since it was installed; nothing more is run"; }
check_bin

# A token actions/checkout left in the checkout's git configuration is
# readable by the agent; refuse to run beside it unless told otherwise.
if [ "${ABHED_ALLOW_PERSISTED_CREDENTIALS:-false}" != "true" ] && git -C "$ws" rev-parse --git-dir >/dev/null 2>&1; then
  if git -C "$ws" config --includes --get-regexp '^http\..*\.extraheader$' >/dev/null 2>&1; then
    fail "the checkout keeps a GitHub token in its git configuration, where the agent could read it. Check out with persist-credentials: false (or set allow-persisted-credentials: true to accept it)"
  fi
fi

# The run's TMPDIR and home, never under the runner's temp folder, which
# holds the file commands (GITHUB_ENV, GITHUB_PATH), the evidence and the
# binary. On Linux, bubblewrap gives each command a private /tmp and binds
# nothing else there, so TMPDIR is /tmp. On macOS it is the account's own
# temp folder (getconf DARWIN_USER_TEMP_DIR), as for any local run: the
# process sandbox lets a command write it, and macOS's mktemp writes there
# whatever TMPDIR says, so any other folder breaks mktemp. The home, which
# holds the run's record, is a fresh folder under the runner user's home,
# removed on every way out of this script.
if [ "$(uname -s)" = Darwin ]; then
  tmpdir="$(getconf DARWIN_USER_TEMP_DIR 2>/dev/null || true)"
  tmpdir="${tmpdir%/}"
  [ -n "$tmpdir" ] && [ -d "$tmpdir" ] || fail "the account's temp folder (getconf DARWIN_USER_TEMP_DIR) is not available"
else
  tmpdir=/tmp
fi
home="$HOME"
made_home=""
cleanup() { [ -n "$made_home" ] && rm -rf "$made_home" 2>/dev/null; return 0; }
trap cleanup EXIT
if [ "${ABHED_ISOLATE_HOME:-true}" = "true" ]; then
  mkdir -p "$HOME"
  made_home="$(mktemp -d "$HOME/.abhed-action.XXXXXXXX")"
  home="$made_home"
  chmod 700 "$home"
fi
if [ -n "${RUNNER_TEMP:-}" ]; then
  rt="$(cd "$RUNNER_TEMP" && pwd -P)"
  for d in "$tmpdir" "$home"; do
    case "$(cd "$d" && pwd -P)/" in "$rt/"*) fail "the run's folders would be inside RUNNER_TEMP" ;; esac
  done
fi

envs=("PATH=$PATH" "HOME=$home" "LANG=${LANG:-C.UTF-8}" "TMPDIR=$tmpdir" "CI=true")
[ -n "${USER:-}" ] && envs+=("USER=$USER")
[ -n "${ABHED_KEY_ENV:-}" ] && envs+=("$ABHED_KEY_ENV=${ABHED_ACTION_MODEL_KEY:-}")
for name in ${ABHED_ENV_PASSTHROUGH//,/ }; do
  [[ "$name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail "env-passthrough: $name is not a variable name"
  case "$name" in
    GITHUB_TOKEN|GH_TOKEN|ACTIONS_*|*_REQUEST_TOKEN|*RUNTIME_TOKEN|RUNNER_TEMP|GITHUB_ENV|GITHUB_PATH|GITHUB_OUTPUT|GITHUB_STEP_SUMMARY|GITHUB_STATE)
      fail "env-passthrough: $name is a GitHub credential or file command and is never passed to the agent" ;;
  esac
  [ -n "${!name+x}" ] && envs+=("$name=${!name}")
done
abhed() { check_bin; env -i "${envs[@]}" "$bin" "$@"; }
# A flag this version lacks is found before the run, not as a failed run.
has() { check_bin; env -i "${envs[@]}" "$bin" "$@" -version >/dev/null 2>&1; }
version() { abhed -version 2>&1 | head -1; }

args=(-C "$ws" -p -no-stdin -output-format stream-json)
[ -n "${ABHED_STDIN_FILE:-}" ] && args=(-C "$ws" -p -output-format stream-json)
[ -n "${ABHED_PERMISSION_MODE:-}" ] && args+=(-mode "$ABHED_PERMISSION_MODE")
[ -n "${ABHED_VERIFY:-}" ] && args+=(-verify "$ABHED_VERIFY")
[ -n "${ABHED_SETTINGS:-}" ] && args+=(-settings "$ABHED_SETTINGS")
[ -n "${ABHED_MAX_TURNS:-}" ] && args+=(-max-turns "$ABHED_MAX_TURNS")
if [ -n "${ABHED_MAX_BUDGET_USD:-}" ]; then
  has -max-budget-usd 1 || fail "max-budget-usd needs Abhed 1.3.0 or later; this is $(version). Set abhed-version and abhed-sha256, or use max-turns"
  args+=(-max-budget-usd "$ABHED_MAX_BUDGET_USD")
fi
if [ -n "${ABHED_TOOLS:-}" ] || [ "${ABHED_BARE:-false}" = "true" ]; then
  if has -tools read -bare; then
    [ -n "${ABHED_TOOLS:-}" ] && args+=(-tools "$ABHED_TOOLS")
    [ "${ABHED_BARE:-false}" = "true" ] && args+=(-bare)
  elif [ -n "${ABHED_TOOLS_FALLBACK_DENY:-}" ]; then
    # Before 1.3.0 there is no -tools or -bare: the tools are narrowed by
    # deny rules instead, which plan mode backs, and the repository's memory
    # files are read.
    echo "::warning::abhed: $(version) has no -tools or -bare; the review is narrowed by deny rules instead, and the repository's ABHED.md is read. Abhed 1.3.0 narrows it fully."
    args+=(-deny "$ABHED_TOOLS_FALLBACK_DENY")
  else
    fail "-tools and -bare need Abhed 1.3.0 or later; this is $(version)"
  fi
fi
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

version > "$ev/abhed-version.txt" || true
code=0
if [ -n "${ABHED_STDIN_FILE:-}" ]; then
  abhed "${args[@]}" < "$ABHED_STDIN_FILE" > "$ev/run.jsonl" 2> "$ev/stderr.log" || code=$?
else
  abhed "${args[@]}" < /dev/null > "$ev/run.jsonl" 2> "$ev/stderr.log" || code=$?
fi
# stderr holds Abhed's notes (each refusal and what would allow it), never the key.
cat "$ev/stderr.log" >&2 || true

sid="$(python3 "$here/abhed_action.py" result --run "$ev/run.jsonl" --evidence "$ev" --exit-code "$code" --print-session)"
if [ -n "$sid" ]; then
  # The record, as recorded and chained, and a transcript to read. The copy
  # verifies with `abhed record verify record.jsonl` anywhere.
  abhed -C "$ws" record export "$sid" -o "$ev/record.jsonl" > /dev/null 2>> "$ev/export.log" || echo "::warning::abhed: the record could not be exported; see export.log"
  abhed -C "$ws" record export "$sid" -format html -o "$ev/transcript.html" > /dev/null 2>> "$ev/export.log" || true
  if [ -f "$ev/record.jsonl" ]; then
    abhed record verify "$ev/record.jsonl" > "$ev/record-verify.txt" 2>&1 || echo "::warning::abhed: the exported record did not verify; see record-verify.txt"
  fi
fi
echo "abhed exited $code"
