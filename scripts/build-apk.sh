#!/usr/bin/env bash
# Build the MyTun Android APK.
#
# Cross-compiles the Go core for arm64 + armv7, drops them into assets, then
# builds and signs the app. Requires the bundled toolchain from the
# flutter-android-build skill (SDK 35 / build-tools 34 / NDK 26 / JDK 17).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKILL_SDKS="${MYTUN_ANDROID_SDKS:-/data/dsh/.dsh/skills/flutter-android-build}"
GOHOME="${MYTUN_GOHOME:-/data/dsh/go-sdk/go}"
GRADLE_BIN="${MYTUN_GRADLE:-$SKILL_SDKS/gradle-home/wrapper/dists/gradle-8.10.2-bin/e0thjr3we83usdufs66z371ne/gradle-8.10.2/bin/gradle}"

export JAVA_HOME="$SKILL_SDKS/sdks/jdk17"
export ANDROID_HOME="$SKILL_SDKS/sdks/android-sdk"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export GRADLE_USER_HOME="$SKILL_SDKS/gradle-home"
export PATH="$GOHOME/bin:$JAVA_HOME/bin:$PATH"
export GOFLAGS=-mod=mod
export GOPATH="${GOPATH:-/data/dsh/gopath}"
export GOMODCACHE="${GOMODCACHE:-/data/dsh/gopath/pkg/mod}"
export GOCACHE="${GOCACHE:-/data/dsh/gopath/build-cache}"
export GRADLE_OPTS="-Xmx2g -Dorg.gradle.jvmargs=-Xmx2048m -XX:MaxMetaspaceSize=512m -Dorg.gradle.workers.max=2 -Dorg.gradle.parallel=false -Dorg.gradle.daemon=false"

ASSETS="$ROOT/android/app/src/main/assets"
mkdir -p "$ASSETS"

echo "== cross-compiling core for android =="
pushd "$ROOT" >/dev/null
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" -o "$ASSETS/mytun-core-arm64" ./cmd/mytun
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "-s -w" -o "$ASSETS/mytun-core-arm" ./cmd/mytun
popd >/dev/null
ls -lh "$ASSETS"

KEYSTORE="${MYTUN_KEYSTORE:-$ROOT/.keystore/mytun.jks}"
if [ ! -f "$KEYSTORE" ]; then
  echo "== generating keystore =="
  mkdir -p "$(dirname "$KEYSTORE")"
  "$JAVA_HOME/bin/keytool" -genkeypair -noprompt \
    -keystore "$KEYSTORE" -alias mytun -keyalg RSA -keysize 2048 -validity 10000 \
    -storepass android -keypass android \
    -dname "CN=MyTun, OU=Dev, O=LECYWZA, L=Local, ST=Local, C=CN" >/dev/null
fi
export MYTUN_KEYSTORE="$KEYSTORE" MYTUN_KS_PASS=android MYTUN_KEY_ALIAS=mytun MYTUN_KEY_PASS=android

echo "== gradle assembleRelease =="
cd "$ROOT/android"
"$GRADLE_BIN" --no-daemon -I "$SKILL_SDKS/templates/init.gradle" assembleRelease "$@"

APK="$ROOT/android/app/build/outputs/apk/release/app-release.apk"
if [ -f "$APK" ]; then
  mkdir -p "$ROOT/dist"
  cp "$APK" "$ROOT/dist/mytun-android.apk"
  ls -lh "$ROOT/dist/mytun-android.apk"
  echo "APK: $ROOT/dist/mytun-android.apk"
else
  echo "build finished but APK not found" >&2
  exit 1
fi
