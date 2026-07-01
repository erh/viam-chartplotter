#!/usr/bin/env bash
# Grant the macOS desktop build outgoing-network access. The macOS App Sandbox
# blocks client connections by default and Flutter's generated entitlements
# don't include it, so tile/weather fetches and the Viam connection fail with
# "Operation not permitted (errno 1)". Add com.apple.security.network.client to
# both the debug and release entitlements.
#
# macOS only; a no-op when the entitlements aren't present (Linux, or before
# `flutter create .` has generated the macOS runner) — Android/iOS don't need
# it. Idempotent. Run from the app dir (mobile/).
set -euo pipefail

PB=/usr/libexec/PlistBuddy
KEY="com.apple.security.network.client"

for f in macos/Runner/DebugProfile.entitlements macos/Runner/Release.entitlements; do
  [ -f "$f" ] || continue
  if [ ! -x "$PB" ]; then
    echo "PlistBuddy not found; skipping $f" >&2
    continue
  fi
  # Set succeeds if the key exists; otherwise Add creates it (idempotent).
  "$PB" -c "Set :$KEY true" "$f" 2>/dev/null \
    || "$PB" -c "Add :$KEY bool true" "$f"
  echo "granted $KEY in $f"
done
