#!/usr/bin/env bash
# Inject flutter_appauth's required `appAuthRedirectScheme` manifest placeholder
# into the app-level Gradle file that `flutter create .` regenerated in CI.
#
# The two Gradle DSLs need DIFFERENT syntax, which is exactly what kept breaking
# when this lived inline in the workflow:
#   - Kotlin DSL (build.gradle.kts, Flutter >=3.29 default): index-SET with '='
#     -> manifestPlaceholders["appAuthRedirectScheme"] = "<scheme>"
#   - Groovy   (build.gradle, older Flutter): append with '+=' so we don't
#     clobber Flutter's own applicationName placeholder
#     -> manifestPlaceholders += [appAuthRedirectScheme: "<scheme>"]
#
# Idempotent (skips if already present). Run from the Flutter app dir (mobile/).
set -euo pipefail

SCHEME="${1:-com.viam.chartplotter}"
KTS="android/app/build.gradle.kts"
GROOVY="android/app/build.gradle"

if [ -f "$KTS" ]; then
  grep -q appAuthRedirectScheme "$KTS" || sed -i \
    "0,/defaultConfig {/s//defaultConfig {\n        manifestPlaceholders[\"appAuthRedirectScheme\"] = \"$SCHEME\"/" \
    "$KTS"
  echo "configured appAuthRedirectScheme in $KTS"
elif [ -f "$GROOVY" ]; then
  grep -q appAuthRedirectScheme "$GROOVY" || sed -i \
    "0,/defaultConfig {/s//defaultConfig {\n        manifestPlaceholders += [appAuthRedirectScheme: \"$SCHEME\"]/" \
    "$GROOVY"
  echo "configured appAuthRedirectScheme in $GROOVY"
else
  echo "no android/app/build.gradle{.kts} found; nothing to configure" >&2
  exit 1
fi
