#!/usr/bin/env bash
# Configure the macOS desktop build. `flutter create` regenerates macos/ from a
# bare, heavily-sandboxed template, so we re-apply everything the app needs:
#
#  Entitlements (macos/Runner/*.entitlements):
#   - com.apple.security.network.client : outgoing connections (tiles, weather,
#     Viam cloud). The App Sandbox blocks these by default → "Operation not
#     permitted (errno 1)".
#   - keychain-access-groups : flutter_secure_storage keeps the Viam session in
#     the keychain; a sandboxed app without this fails with errSecMissingEntitlement
#     (-34018), which crashes the app on launch ("Failed to foreground app").
#
#  Info.plist (macos/Runner/Info.plist):
#   - NSLocalNetworkUsageDescription + NSBonjourServices (_rpc._tcp) : macOS 15+
#     local-network privacy prompt for the mDNS `.local` connection the Viam SDK
#     uses to reach the machine on the LAN (Bonsoir browses `_rpc._tcp`).
#
# Writes are skipped when the value is already present so we don't churn the file
# mtime (a changed *.entitlements mtime makes Xcode fail incremental builds with
# "Entitlements file was modified during the build").
#
# macOS only; a no-op when the files/PlistBuddy aren't present (Linux/CI, or
# before `flutter create`). Idempotent. Run from the app dir (mobile/).
set -euo pipefail

PB=/usr/libexec/PlistBuddy
[ -x "$PB" ] || { echo "PlistBuddy not found; skipping macOS config" >&2; exit 0; }

# Idempotent grant of a boolean entitlement.
grant_bool() { # file key
  local f="$1" key="$2"
  [ -f "$f" ] || return 0
  [ "$("$PB" -c "Print :$key" "$f" 2>/dev/null)" = "true" ] && return 0
  "$PB" -c "Set :$key true" "$f" 2>/dev/null || "$PB" -c "Add :$key bool true" "$f"
  echo "granted $key in $f"
}

# Idempotent keychain-access-group. The $(AppIdentifierPrefix)/$(CFBundleIdentifier)
# build variables are substituted by Xcode at sign time, so this tracks whatever
# team/bundle id the build uses.
grant_keychain() { # file
  local f="$1" grp='$(AppIdentifierPrefix)$(CFBundleIdentifier)'
  [ -f "$f" ] || return 0
  "$PB" -c "Print :keychain-access-groups" "$f" 2>/dev/null | grep -q 'CFBundleIdentifier' && return 0
  "$PB" -c "Print :keychain-access-groups" "$f" >/dev/null 2>&1 \
    || "$PB" -c "Add :keychain-access-groups array" "$f"
  "$PB" -c "Add :keychain-access-groups: string $grp" "$f"
  echo "granted keychain-access-groups in $f"
}

for f in macos/Runner/DebugProfile.entitlements macos/Runner/Release.entitlements; do
  grant_bool "$f" "com.apple.security.network.client"
  grant_keychain "$f"
done

# --- Info.plist: local network / mDNS --------------------------------------
PLIST=macos/Runner/Info.plist
if [ -f "$PLIST" ]; then
  if [ -z "$("$PB" -c "Print :NSLocalNetworkUsageDescription" "$PLIST" 2>/dev/null)" ]; then
    "$PB" -c "Add :NSLocalNetworkUsageDescription string Connects to your Viam machine on the local network." "$PLIST"
  fi
  if ! "$PB" -c "Print :NSBonjourServices" "$PLIST" 2>/dev/null | grep -q "_rpc._tcp"; then
    "$PB" -c "Print :NSBonjourServices" "$PLIST" >/dev/null 2>&1 \
      || "$PB" -c "Add :NSBonjourServices array" "$PLIST"
    "$PB" -c "Add :NSBonjourServices: string _rpc._tcp" "$PLIST"
  fi
  echo "ensured local-network keys in $PLIST"
fi
