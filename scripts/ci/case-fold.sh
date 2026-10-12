#!/usr/bin/env bash
# The case-folding path tests, on a disk that folds case (macOS). Each runs
# alone by name under the race detector, and the verdict comes from
# `go test -json`: a skipped or missing test fails, whatever its message.
#
#   scripts/ci/case-fold.sh [OUTDIR] [extra go test flags...]
set -euo pipefail
OUT="${1:-.}"; shift || true
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
mkdir -p "$OUT"
JSON="$OUT/case.json"
: > "$JSON"

# Every test in a file that exists for case folding, as an anchored -run.
tests_in() {
  echo "^($(grep -ho '^func Test[A-Za-z0-9_]*' "$@" | sed 's/^func //' | paste -sd'|' -))\$"
}

run() {
  local pkg="$1" re="$2"; shift 2
  go test -race -count=1 -timeout 30m -json "$@" -run "$re" "$pkg" >> "$JSON" || {
    jq -r 'select(.Output) | .Output' "$JSON"; exit 1
  }
}

run ./internal/tools/ "$(tests_in internal/tools/diskpath_test.go)" "$@"
run ./internal/policy/ "$(tests_in internal/policy/casefold_test.go)" "$@"
run ./internal/agent/ "$(tests_in internal/agent/casefold_test.go)" "$@"
run ./server/ 'Explorer|DiskPath|Case' "$@"
run ./internal/forge/ 'WithoutCase' "$@"

skipped="$(jq -r 'select(.Action == "skip" and .Test != null) | "\(.Package) \(.Test)"' "$JSON")"
if [ -n "$skipped" ]; then
  echo "skipped on a disk that should fold case:"
  echo "$skipped"
  jq -r 'select(.Action == "output" and .Test != null and (.Output | test("--- SKIP|_test.go"))) | .Output' "$JSON"
  exit 1
fi

for t in TestDiskPathSpellsWhatTheDiskHolds TestDiskPathFollowsARenameToAnotherCase \
         TestDiskPathSeesARenameBehindAnOldTime TestDiskPathPrefersTheNameOverAHardLink \
         TestDiskPathFoldsAsTheDiskDoes TestIdentityScanPassesOverAHardLinkedFile TestPushComparesPathsWithoutCase \
         TestPathRulesHoldForEveryCaseTheDiskOpens TestRelativeRulesHoldUnderARootNamedInAnotherCase \
         TestAgentFileToolsMeetRulesInAnyCase TestManualRefusedRecordsOnlyADenial \
         TestExplorerRulesHoldForEveryCaseTheDiskOpens TestExplorerWriteRuleRefusalsAreRecorded; do
  jq -e --arg t "$t" 'select(.Test == $t and .Action == "pass")' "$JSON" >/dev/null || {
    echo "$t did not run and pass"; exit 1
  }
done
echo "$(jq -r 'select(.Action == "pass" and .Test != null) | .Test' "$JSON" | wc -l | tr -d ' ') tests passed; every case-folding test ran"
