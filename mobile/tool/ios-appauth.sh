#!/usr/bin/env bash
# Patch the iOS Info.plist that `flutter create .` regenerates from a bare
# template. Two things it's missing that this app needs:
#
#  1. OAuth redirect scheme (CFBundleURLTypes) so the system routes the
#     post-login callback (<scheme>:/oauthredirect) back into the app; without
#     it the OAuth flow launches Safari and never returns. (Android equivalent:
#     tool/ci-android-appauth.sh.)
#  2. Camera/microphone usage descriptions. viam_sdk pulls in flutter_webrtc,
#     which links AVFoundation; iOS terminates the app on launch if those APIs
#     are reachable with no usage description — the classic "white screen then
#     crash".
#
# macOS only (needs PlistBuddy + Xcode's generated ios/); a no-op elsewhere or
# before `flutter create .`. Idempotent. Run from the app dir (mobile/).
set -euo pipefail

SCHEME="${1:-com.viam.chartplotter}"
PB=/usr/libexec/PlistBuddy
PLIST="ios/Runner/Info.plist"

[ -f "$PLIST" ] || { echo "no $PLIST; skipping iOS plist setup" >&2; exit 0; }
[ -x "$PB" ] || { echo "PlistBuddy not found; skipping (non-macOS?)" >&2; exit 0; }

# Idempotent set-or-add of a string key.
set_str() {
  "$PB" -c "Set :$1 $2" "$PLIST" 2>/dev/null \
    || "$PB" -c "Add :$1 string $2" "$PLIST"
}

# --- WebRTC (viam_sdk) usage descriptions ----------------------------------
set_str NSCameraUsageDescription "Shows the boat's camera feeds."
set_str NSMicrophoneUsageDescription "Required by the Viam robot connection."
echo "ensured camera/microphone usage descriptions in $PLIST"

# --- Local network / mDNS ---------------------------------------------------
# The Viam SDK finds the machine on the LAN via Bonsoir mDNS browsing for the
# `_rpc._tcp` service (the fast on-boat direct connection). iOS silently blocks
# this unless the app declares a local-network usage description AND lists the
# Bonjour service type it browses — otherwise the `<machine>.local` connection
# fails. Add both (idempotent).
set_str NSLocalNetworkUsageDescription "Connects to your Viam machine on the local network."
if ! "$PB" -c "Print :NSBonjourServices" "$PLIST" 2>/dev/null | grep -q "_rpc._tcp"; then
  "$PB" -c "Print :NSBonjourServices" "$PLIST" >/dev/null 2>&1 \
    || "$PB" -c "Add :NSBonjourServices array" "$PLIST"
  "$PB" -c "Add :NSBonjourServices: string _rpc._tcp" "$PLIST"
fi
echo "ensured local-network / mDNS keys in $PLIST"

# --- OAuth redirect scheme --------------------------------------------------
if "$PB" -c "Print :CFBundleURLTypes" "$PLIST" 2>/dev/null | grep -q "$SCHEME"; then
  echo "iOS URL scheme $SCHEME already present in $PLIST"
  exit 0
fi

# Ensure the CFBundleURLTypes array exists, then append a new URL-type entry at
# the next index (count of existing dict entries).
"$PB" -c "Print :CFBundleURLTypes" "$PLIST" >/dev/null 2>&1 \
  || "$PB" -c "Add :CFBundleURLTypes array" "$PLIST"

# `grep -c` exits non-zero on zero matches, which would trip `set -e`; guard it.
idx=$("$PB" -c "Print :CFBundleURLTypes" "$PLIST" 2>/dev/null | grep -c "Dict {" || true)
idx=${idx:-0}

"$PB" -c "Add :CFBundleURLTypes:$idx dict" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleTypeRole string Editor" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLName string $SCHEME" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLSchemes array" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLSchemes:0 string $SCHEME" "$PLIST"

echo "registered iOS URL scheme $SCHEME in $PLIST"
