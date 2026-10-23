#!/usr/bin/env bash
# Build the MyTun Android APK.
#
# The Go core is packaged as jniLibs/<abi>/libmytuncore.so for two reasons:
#   1. Android only grants execute permission to native libs; a binary copied
#      into the app's private dir dies with "execute_no_trans" under SELinux.
#   2. Android is built with GOOS=android + cgo (NDK) so the C library path is
#      used. The pure-Go SQLite driver issues lstat (syscall 6) on amd64, which
#      Android's seccomp policy rejects with SIGSYS.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SKILL_SDKS="${MYTUN_ANDROID_SDKS:-/data/dsh/.dsh/skills/flutter-android-build}"
GOHOME="${MYTUN_GOHOME:-/data/dsh/go-sdk/go}"
GRADLE_BIN="${MYTUN_GRADLE:-$SKILL_SDKS/gradle-home/wrapper/dists/gradle-8.10.2-bin/e0thjr3we83usdufs66z371ne/gradle-8.10.2/bin/gradle}"
NDK="${MYTUN_NDK:-$SKILL_SDKS/sdks/android-sdk/ndk/26.3.11579264}"
TOOLCHAIN="$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin"

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

JNI="$ROOT/android/app/src/main/jniLibs"
mkdir -p "$JNI"/{arm64-v8a,armeabi-v7a,x86_64}

echo "== cross-compiling core for android (cgo + NDK) =="
cd "$ROOT"
build_one() {
  local abi="$1" cc="$2" goarch="$3"
  echo "  -> $abi"
  CGO_ENABLED=1 GOOS=android GOARCH="$goarch" \
    CC="$TOOLCHAIN/$cc" CXX="$TOOLCHAIN/$cc++" \
    go build -ldflags "-s -w" -o "$JNI/$abi/libmytuncore.so" ./cmd/mytun
}
build_one arm64-v8a    aarch64-linux-android21-clang          arm64
build_one armeabi-v7a  armv7a-linux-androideabi21-clang       arm
build_one x86_64       x86_64-linux-android21-clang           amd64
ls -lh "$JNI"/*/libmytuncore.so

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
mkdir -p "$ROOT/dist"
cp "$APK" "$ROOT/dist/mytun-android.apk"
echo "APK: $ROOT/dist/mytun-android.apk"
