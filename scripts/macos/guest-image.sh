#!/usr/bin/env bash
# Fetch and build the arm64 guest for the microvm driver on a Mac: a pinned
# guest kernel and a pinned Alpine minirootfs, each checked against its
# SHA-256, and a root filesystem with abhed-guest built by
# scripts/microvm/build-rootfs.sh. That script needs Linux and root, so it
# runs in a throwaway Linux container (podman or docker).
#
#   scripts/macos/guest-image.sh -o <folder> [-e podman|docker]
#
# Writes <folder>/vmlinux-arm64 and <folder>/rootfs-arm64.ext4.
set -euo pipefail

# The kernel is the arm64 guest kernel Firecracker's CI publishes: an
# uncompressed Image, as Virtualization.framework needs on arm64, with virtio
# PCI, block, console and vsock built in, so no initrd or modules are needed.
KERNEL_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/aarch64/vmlinux-6.1.155
KERNEL_SHA256=e3544b10603acbf3db492cb52e000d22ba202cb4b63b9add027565683e11c591
ALPINE_URL=https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/aarch64/alpine-minirootfs-3.24.2-aarch64.tar.gz
ALPINE_SHA256=9bf70a7f18ea44094cbb5f70c58f9af129c8214745743db0e68e5502cc2ce773
# Only for mkfs.ext4 and bash; nothing from it goes into the root filesystem.
BUILDER=docker.io/library/alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

OUT="" ENGINE=""
while getopts "o:e:h" opt; do
  case $opt in
    o) OUT="$OPTARG" ;;
    e) ENGINE="$OPTARG" ;;
    h) sed -n '2,10p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
[ -n "$OUT" ] || { sed -n '2,10p' "$0"; exit 2; }
if [ -z "$ENGINE" ]; then
  if command -v podman >/dev/null; then ENGINE=podman; elif command -v docker >/dev/null; then ENGINE=docker; fi
fi
[ -n "$ENGINE" ] || { echo "building the root filesystem needs podman or docker" >&2; exit 1; }
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

fetch() {
  local url=$1 sum=$2 dst=$3
  if [ ! -f "$dst" ] || ! echo "$sum  $dst" | shasum -a 256 -c - >/dev/null 2>&1; then
    curl -fsSL -o "$dst.part" "$url"
    echo "$sum  $dst.part" | shasum -a 256 -c - >/dev/null || { echo "checksum mismatch for $url" >&2; rm -f "$dst.part"; exit 1; }
    mv "$dst.part" "$dst"
  fi
}
fetch "$KERNEL_URL" "$KERNEL_SHA256" "$OUT/vmlinux-arm64"
fetch "$ALPINE_URL" "$ALPINE_SHA256" "$OUT/alpine-minirootfs-aarch64.tar.gz"
chmod 0644 "$OUT/vmlinux-arm64"

(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o "$OUT/abhed-guest" ./cmd/abhed-guest)
"$ENGINE" run --rm --platform linux/arm64 \
  -v "$ROOT/scripts/microvm:/s:ro" -v "$OUT:/out" "$BUILDER" \
  sh -c 'apk add -q --no-cache bash e2fsprogs && bash /s/build-rootfs.sh -b /out/alpine-minirootfs-aarch64.tar.gz -g /out/abhed-guest -a arm64 -s 256M -o /out/rootfs-arm64.ext4' >/dev/null
rm -f "$OUT/abhed-guest"
echo "$OUT/vmlinux-arm64"
echo "$OUT/rootfs-arm64.ext4"
