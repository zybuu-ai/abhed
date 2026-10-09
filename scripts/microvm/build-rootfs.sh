#!/usr/bin/env bash
# Build an ext4 root filesystem for the microvm driver: a base tree (for
# example an Alpine minirootfs tarball whose checksum you have checked) with
# abhed-guest installed as /sbin/abhed-guest and an empty /workspace. Run as
# root so the tree keeps root ownership. Linux only; needs mkfs.ext4, and Go
# unless -g names an abhed-guest already built.
#
#   sudo scripts/microvm/build-rootfs.sh -b base.tar.gz -o rootfs.ext4 [-a amd64|arm64] [-s 512M] [-g abhed-guest]
set -euo pipefail
BASE="" OUT="" ARCH="amd64" SIZE="512M" GUEST=""
while getopts "b:o:a:s:g:h" opt; do
  case $opt in
    b) BASE="$OPTARG" ;;
    o) OUT="$OPTARG" ;;
    a) ARCH="$OPTARG" ;;
    s) SIZE="$OPTARG" ;;
    g) GUEST="$OPTARG" ;;
    h) sed -n '2,8p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
[ -n "$BASE" ] && [ -n "$OUT" ] || { sed -n '2,8p' "$0"; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root, so the tree is owned by root" >&2; exit 1; }
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TREE="$(mktemp -d)"
trap 'rm -rf "$TREE"' EXIT

tar -xzf "$BASE" -C "$TREE"
# /bin/sh is often a link to busybox, absolute to the guest's root.
[ -L "$TREE/bin/sh" ] || [ -x "$TREE/bin/sh" ] || { echo "the base tree has no /bin/sh" >&2; exit 1; }
if [ -n "$GUEST" ]; then
  cp "$GUEST" "$TREE/sbin/abhed-guest"
else
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "-s -w" \
    -o "$TREE/sbin/abhed-guest" ./cmd/abhed-guest)
fi
chown 0:0 "$TREE/sbin/abhed-guest"
chmod 0755 "$TREE/sbin/abhed-guest"
mkdir -p "$TREE/workspace" "$TREE/proc" "$TREE/sys" "$TREE/dev" "$TREE/tmp" "$TREE/run"
rm -f "$OUT"
mkfs.ext4 -q -F -L rootfs -d "$TREE" "$OUT" "$SIZE"
chmod 0644 "$OUT"
echo "$OUT"
