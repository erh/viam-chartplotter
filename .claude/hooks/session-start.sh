#!/bin/bash
# SessionStart hook for Claude Code on the web.
#
# Provisions the Flutter toolchain so web sessions can run `flutter analyze`
# / `flutter test` / build the mobile app in mobile/. It is GATED on the
# mobile/ directory existing, so it is a no-op on branches/checkouts that
# don't contain the Flutter app (e.g. main before the mobile PR lands).
set -euo pipefail

# Only meaningful in the remote (Claude Code on the web) environment; locally
# the developer already has their own toolchain.
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

PROJECT_DIR="${CLAUDE_PROJECT_DIR:-$(pwd)}"

# No Flutter app checked out → nothing to provision.
if [ ! -d "$PROJECT_DIR/mobile" ]; then
  exit 0
fi

# Single source of truth for the Flutter version: mobile/.fvmrc (the same file
# CI reads via subosito/flutter-action's flutter-version-file). Falls back to a
# default if the file is missing or unparseable.
FLUTTER_VERSION="$(sed -n 's/.*"flutter"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$PROJECT_DIR/mobile/.fvmrc" 2>/dev/null)"
FLUTTER_VERSION="${FLUTTER_VERSION:-3.29.3}"
FLUTTER_DIR="$HOME/flutter"

# Install Flutter (idempotent: skip the clone if it's already there so
# re-runs and the container cache are fast).
if [ ! -x "$FLUTTER_DIR/bin/flutter" ]; then
  echo "Installing Flutter $FLUTTER_VERSION…"
  git clone --depth 1 --branch "$FLUTTER_VERSION" \
    https://github.com/flutter/flutter.git "$FLUTTER_DIR"
fi

# Put flutter/dart on PATH for this hook and persist it for the session.
export PATH="$FLUTTER_DIR/bin:$PATH"
if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  echo "export PATH=\"$FLUTTER_DIR/bin:\$PATH\"" >> "$CLAUDE_ENV_FILE"
fi

# Warm the tool cache and resolve the app's dependencies so `flutter analyze`
# is ready the moment the session starts. pub get needs only pubspec.yaml, not
# the (gitignored) platform folders.
flutter --version
(cd "$PROJECT_DIR/mobile" && flutter pub get)
