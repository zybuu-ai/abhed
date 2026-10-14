#!/usr/bin/env bash
# Build abhed-vmhelper, the microvm driver's Virtualization.framework helper,
# and sign it ad hoc with its entitlement and the hardened runtime: enough to
# run on this Mac. For distribution, sign it with scripts/macos/sign-vmhelper.sh.
# Needs macOS 13 or later and the Xcode command-line tools.
#
#   scripts/macos/build-vmhelper.sh [-o bin/abhed-vmhelper]
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="$ROOT/bin/abhed-vmhelper"
while getopts "o:h" opt; do
  case $opt in
    o) OUT="$OPTARG" ;;
    h) sed -n '2,7p' "$0"; exit 0 ;;
    *) exit 2 ;;
  esac
done
[ "$(uname -s)" = Darwin ] || { echo "abhed-vmhelper builds only on macOS" >&2; exit 1; }
PKG="$ROOT/macos/abhed-vmhelper"
xcrun swift build --package-path "$PKG" -c release
BIN="$(xcrun swift build --package-path "$PKG" -c release --show-bin-path)/abhed-vmhelper"
mkdir -p "$(dirname "$OUT")"
install -m 0755 "$BIN" "$OUT"
codesign --force --sign - --identifier com.zybuu.abhed.vmhelper \
  --entitlements "$PKG/abhed-vmhelper.entitlements" --options runtime "$OUT"
codesign --verify --strict -R='identifier "com.zybuu.abhed.vmhelper"' "$OUT"
echo "$OUT"
