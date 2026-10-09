#!/usr/bin/env bash
# Sign abhed-vmhelper for distribution: Developer ID signature with the
# hardened runtime, a secure timestamp and the virtualization entitlement,
# then notarization by Apple and a stapled ticket. A bare executable cannot
# hold a stapled ticket, so the helper is shipped in a disk image, which is
# signed, notarized and stapled; Apple's record covers the helper inside it.
#
# Needs, from the environment:
#   ABHED_SIGN_IDENTITY    the "Developer ID Application: <name> (<TEAMID>)"
#                          certificate in the keychain, or its SHA-1 hash
#   ABHED_NOTARY_PROFILE   a notarytool keychain profile, made once with
#                          xcrun notarytool store-credentials <profile> \
#                            --apple-id <id> --team-id <TEAMID> --password <app-specific password>
#   ABHED_SIGN_KEYCHAIN    optional: the keychain holding the identity
#
#   scripts/macos/sign-vmhelper.sh <built abhed-vmhelper> <output folder>
#
# Writes <output folder>/abhed-vmhelper and abhed-vmhelper.dmg, and prints the
# team id to put in the driver's helper.team_id.
set -euo pipefail
[ $# -eq 2 ] || { sed -n '2,20p' "$0"; exit 2; }
IN=$1 OUTDIR=$2
: "${ABHED_SIGN_IDENTITY:?set ABHED_SIGN_IDENTITY to a Developer ID Application identity}"
: "${ABHED_NOTARY_PROFILE:?set ABHED_NOTARY_PROFILE to a notarytool keychain profile}"
[ "$ABHED_SIGN_IDENTITY" != "-" ] || { echo "an ad-hoc signature cannot be notarized; use scripts/macos/build-vmhelper.sh for local use" >&2; exit 2; }
[ "$(uname -s)" = Darwin ] || { echo "signing runs only on macOS" >&2; exit 1; }
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENT="$ROOT/macos/abhed-vmhelper/abhed-vmhelper.entitlements"
KC=()
[ -z "${ABHED_SIGN_KEYCHAIN:-}" ] || KC=(--keychain "$ABHED_SIGN_KEYCHAIN")

mkdir -p "$OUTDIR"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir "$STAGE/abhed-vmhelper"
BIN="$STAGE/abhed-vmhelper/abhed-vmhelper"
install -m 0755 "$IN" "$BIN"

codesign --force --timestamp --options runtime --identifier com.zybuu.abhed.vmhelper \
  --entitlements "$ENT" --sign "$ABHED_SIGN_IDENTITY" ${KC[@]+"${KC[@]}"} "$BIN"
TEAM="$(codesign -d --verbose=2 "$BIN" 2>&1 | sed -n 's/^TeamIdentifier=//p')"
[ -n "$TEAM" ] && [ "$TEAM" != "not set" ] || { echo "the signature carries no team id: is this a Developer ID identity?" >&2; exit 1; }
REQ="identifier \"com.zybuu.abhed.vmhelper\" and anchor apple generic and certificate leaf[subject.OU] = \"$TEAM\""
codesign --verify --strict -R="$REQ" "$BIN"
codesign -d --entitlements - --xml "$BIN" | grep -q 'com.apple.security.virtualization</key><true/>' \
  || { echo "the signature lacks the virtualization entitlement" >&2; exit 1; }

DMG="$STAGE/abhed-vmhelper.dmg"
hdiutil create -quiet -fs HFS+ -format UDZO -volname abhed-vmhelper -srcfolder "$STAGE/abhed-vmhelper" "$DMG"
codesign --force --timestamp --sign "$ABHED_SIGN_IDENTITY" ${KC[@]+"${KC[@]}"} "$DMG"
xcrun notarytool submit "$DMG" --keychain-profile "$ABHED_NOTARY_PROFILE" --wait
xcrun stapler staple "$DMG"
xcrun stapler validate "$DMG"
spctl --assess --type open --context context:primary-signature -v "$DMG"

install -m 0755 "$BIN" "$OUTDIR/abhed-vmhelper"
install -m 0644 "$DMG" "$OUTDIR/abhed-vmhelper.dmg"
echo "signed and notarized; set \"helper\": {\"team_id\": \"$TEAM\"} in the driver's settings"
