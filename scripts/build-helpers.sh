#!/usr/bin/env bash
# Build abhed-helper, the small Linux helper an engine binds read-only into a
# container where its own binary cannot run (the egress relay and the debug
# bridge), for each Linux architecture, into OUTDIR as
# abhed-helper-linux-<arch>. Prints on stdout the -ldflags -X arguments that
# pin each helper's sha256 into an engine build; any build that ships the
# helpers beside its engine (the release, the desktop app, the enterprise
# binary) passes them, or the engine does not use the helper. The build is
# reproducible: no cgo, -trimpath and an empty build ID.
#
#   scripts/build-helpers.sh [-o OUTDIR] [-a "amd64 arm64 s390x"]
set -euo pipefail
OUTDIR="dist/helpers"
ARCHES="amd64 arm64 s390x"
while getopts "o:a:h" opt; do
  case $opt in
    o) OUTDIR="$OPTARG" ;;
    a) ARCHES="$OPTARG" ;;
    h) sed -n '2,11p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$OUTDIR"
flags=()
for arch in $ARCHES; do
  out="$OUTDIR/abhed-helper-linux-$arch"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
    -trimpath -ldflags "-s -w -buildid=" -o "$out" ./cmd/abhed-helper)
  if command -v sha256sum >/dev/null; then sum="$(sha256sum "$out")"; else sum="$(shasum -a 256 "$out")"; fi
  case $arch in amd64) v=AMD64 ;; arm64) v=ARM64 ;; s390x) v=S390X ;; *) echo "no pin for linux/$arch" >&2; exit 2 ;; esac
  flags+=("-X github.com/zybuu-ai/abhed/internal/sandbox.helperSHA256Linux$v=${sum%% *}")
  echo "  abhed-helper-linux-$arch" >&2
done
echo "${flags[*]}"
