#!/usr/bin/env bash
# Build the Community Edition release artefacts: one archive per platform
# holding the binary, the licence, the README and the changelog, the
# microVM driver's two binaries (abhed-microvm and abhed-guest) for Linux,
# plus a checksum file. What the release workflow runs; runs the same on a
# laptop.
#
#   scripts/build-release.sh [-v VERSION] [-o OUTDIR] [-p PLATFORMS] [-m MICROVM_PLATFORMS]
set -euo pipefail
VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
OUTDIR="dist"
PLATFORMS="linux/amd64 linux/arm64 linux/s390x darwin/arm64 darwin/amd64"
# The microVM driver runs on Linux only. The guest kernel and root
# filesystem are not shipped: build-rootfs.sh's output is not reproducible
# (mkfs.ext4 stamps a fresh UUID and times), so operators build their own.
MICROVM_PLATFORMS="linux/amd64 linux/arm64 linux/s390x"
while getopts "v:o:p:m:h" opt; do
  case $opt in
    v) VERSION="$OPTARG" ;;
    o) OUTDIR="$OPTARG" ;;
    p) PLATFORMS="$OPTARG" ;;
    m) MICROVM_PLATFORMS="$OPTARG" ;;
    h) sed -n '2,9p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"

echo "Abhed ${VERSION}"
echo "  embedding documentation"
python3 "$ROOT/scripts/docsite/build.py" --embed-only >/dev/null

for platform in $PLATFORMS; do
  os="${platform%/*}"; arch="${platform#*/}"
  name="abhed-${VERSION}-${os}-${arch}"
  dir="$STAGE/$name"
  mkdir -p "$dir"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$dir/abhed" "$ROOT/cmd/abhed"
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$ROOT/CHANGELOG.md" "$dir/"
  tar -czf "$OUTDIR/$name.tar.gz" -C "$STAGE" "$name"
  # The bare binary too, for a one-line install with curl.
  cp "$dir/abhed" "$OUTDIR/$name"
  echo "  $name ($(du -h "$OUTDIR/$name.tar.gz" | cut -f1))"
done

# Bare binaries, beside the archives: abhed-microvm runs as root on the host,
# abhed-guest goes into the root filesystem build-rootfs.sh -g makes.
for platform in $MICROVM_PLATFORMS; do
  os="${platform%/*}"; arch="${platform#*/}"
  for cmd in abhed-microvm abhed-guest; do
    name="${cmd}-${VERSION}-${os}-${arch}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
      -trimpath -ldflags "-s -w" -o "$OUTDIR/$name" "$ROOT/cmd/$cmd"
    echo "  $name"
  done
done

( cd "$OUTDIR" && (command -v sha256sum >/dev/null && sha256sum abhed-* || shasum -a 256 abhed-*) > SHA256SUMS )
echo "  SHA256SUMS written"
echo "Verify a download with: sha256sum -c SHA256SUMS --ignore-missing"
