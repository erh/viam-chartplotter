#!/usr/bin/env bash
# Inject flutter_appauth's required `appAuthRedirectScheme` manifest placeholder
# into the app-level Gradle file that `flutter create .` regenerated.
#
# The two Gradle DSLs need DIFFERENT syntax:
#   - Kotlin DSL (build.gradle.kts, Flutter >=3.29 default): index-SET with '='
#       manifestPlaceholders["appAuthRedirectScheme"] = "<scheme>"
#   - Groovy   (build.gradle, older Flutter): append with '+=' so we don't
#     clobber Flutter's own applicationName placeholder
#       manifestPlaceholders += [appAuthRedirectScheme: "<scheme>"]
#
# Uses awk (not sed) so it behaves identically on GNU (Linux/CI) and BSD
# (macOS) — BSD sed rejects the `0,/re/` address and `\n` replacements this
# needs. Idempotent (skips if already present). Run from the app dir (mobile/).
set -euo pipefail

SCHEME="${1:-com.viam.chartplotter}"
KTS="android/app/build.gradle.kts"
GROOVY="android/app/build.gradle"

# Insert `text` on its own line right after the first `defaultConfig {`.
insert_after_default_config() {
  file="$1"; text="$2"
  if awk -v ins="$text" '
      !done && $0 ~ /defaultConfig[[:space:]]*[{]/ { print; print ins; done=1; next }
      { print }
      END { if (!done) exit 3 }
    ' "$file" > "$file.tmp"; then
    mv "$file.tmp" "$file"
  else
    rm -f "$file.tmp"
    echo "defaultConfig block not found in $file" >&2
    return 1
  fi
}

if [ -f "$KTS" ]; then
  grep -q appAuthRedirectScheme "$KTS" || insert_after_default_config "$KTS" \
    '        manifestPlaceholders["appAuthRedirectScheme"] = "'"$SCHEME"'"'
  echo "configured appAuthRedirectScheme in $KTS"
elif [ -f "$GROOVY" ]; then
  grep -q appAuthRedirectScheme "$GROOVY" || insert_after_default_config "$GROOVY" \
    '        manifestPlaceholders += [appAuthRedirectScheme: "'"$SCHEME"'"]'
  echo "configured appAuthRedirectScheme in $GROOVY"
else
  echo "no android/app/build.gradle{.kts} found; nothing to configure" >&2
  exit 1
fi
