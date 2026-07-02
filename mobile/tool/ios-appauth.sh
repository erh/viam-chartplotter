#!/usr/bin/env bash
# Register flutter_appauth's OAuth redirect scheme in the iOS app so the system
# routes the post-login callback (<scheme>:/oauthredirect) back into the app.
# `flutter create .` regenerates ios/Runner/Info.plist from a template with no
# CFBundleURLTypes, so without this the OAuth flow launches Safari and never
# returns. Mirrors tool/ci-android-appauth.sh (which does the Android side).
#
# macOS only (needs PlistBuddy + Xcode's generated ios/); a no-op elsewhere or
# before `flutter create .`. Idempotent. Run from the app dir (mobile/).
set -euo pipefail

SCHEME="${1:-com.viam.chartplotter}"
PB=/usr/libexec/PlistBuddy
PLIST="ios/Runner/Info.plist"

[ -f "$PLIST" ] || { echo "no $PLIST; skipping iOS URL scheme" >&2; exit 0; }
[ -x "$PB" ] || { echo "PlistBuddy not found; skipping (non-macOS?)" >&2; exit 0; }

# Already registered? (idempotent)
if "$PB" -c "Print :CFBundleURLTypes" "$PLIST" 2>/dev/null | grep -q "$SCHEME"; then
  echo "iOS URL scheme $SCHEME already present in $PLIST"
  exit 0
fi

# Ensure the CFBundleURLTypes array exists, then append a new URL-type entry at
# the next index (count of existing dict entries).
"$PB" -c "Print :CFBundleURLTypes" "$PLIST" >/dev/null 2>&1 \
  || "$PB" -c "Add :CFBundleURLTypes array" "$PLIST"

idx=$("$PB" -c "Print :CFBundleURLTypes" "$PLIST" 2>/dev/null | grep -c "Dict {")

"$PB" -c "Add :CFBundleURLTypes:$idx dict" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleTypeRole string Editor" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLName string $SCHEME" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLSchemes array" "$PLIST"
"$PB" -c "Add :CFBundleURLTypes:$idx:CFBundleURLSchemes:0 string $SCHEME" "$PLIST"

echo "registered iOS URL scheme $SCHEME in $PLIST"
