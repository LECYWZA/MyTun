#!/usr/bin/env bash
# Build release binaries for every desktop platform and pack them with UPX.
#
# UPX is optional: without it the binaries are simply left uncompressed. The
# packed binaries are verified by running the local one before we call it a day,
# because packing a Go binary can (rarely) produce something that will not start.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DIST="$ROOT/dist"
# NOTE: upx itself reads the UPX environment variable as extra options, so our
# override must use a different name.
UPX_BIN="${MYTUN_UPX:-$(command -v upx || true)}"
mkdir -p "$DIST"

echo "== clean =="
rm -f "$DIST"/mytun-linux-amd64 "$DIST"/mytun-linux-arm64 "$DIST"/mytun-windows-amd64.exe "$DIST"/mytun-darwin-arm64

echo "== frontend =="
if [ ! -d webui/node_modules ]; then
	echo "  installing frontend dependencies"
	( cd webui && if [ -f package-lock.json ]; then npm ci >/dev/null; else npm install >/dev/null; fi )
fi
( cd webui && npm run build >/dev/null )

build() {
	local goos="$1" goarch="$2" out="$3"
	echo "== build $goos/$goarch =="
	GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w" -o "$DIST/$out" ./cmd/mytun
}

build linux amd64 mytun-linux-amd64
build linux arm64 mytun-linux-arm64
build windows amd64 mytun-windows-amd64.exe
build darwin arm64 mytun-darwin-arm64

if [ -n "$UPX_BIN" ] && [ -x "$UPX_BIN" ]; then
	echo "== pack with upx ($("$UPX_BIN" --version | head -1)) =="
	# macOS binaries are left alone: upx refuses them unless --force-macos is
	# passed, and that mode is still experimental.
	for f in mytun-linux-amd64 mytun-linux-arm64 mytun-windows-amd64.exe; do
		# --best is the safe choice; --lzma squeezes harder but is slower and
		# occasionally upsets loaders, so keep it off by default.
		"$UPX_BIN" --best -q "$DIST/$f" >/dev/null
	done
else
	echo "== upx not found, keeping binaries unpacked =="
fi

echo "== smoke test (linux/amd64) =="
TMP="$(mktemp -d)"
"$DIST/mytun-linux-amd64" --data "$TMP" --host 127.0.0.1 --port 18899 >"$TMP/out.log" 2>&1 &
PID=$!
for _ in $(seq 1 20); do
	sleep 0.5
	if curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://127.0.0.1:18899/api/health" | grep -q 200; then
		echo "   ok: console responds"
		break
	fi
done
CODE=$(curl -s --noproxy '*' -o /dev/null -w '%{http_code}' "http://127.0.0.1:18899/api/health" || true)
kill "$PID" 2>/dev/null || true
wait "$PID" 2>/dev/null || true
rm -rf "$TMP"
if [ "$CODE" != "200" ]; then
	echo "!! packed binary failed its smoke test (health=$CODE)" >&2
	exit 1
fi

echo "== sizes =="
ls -lh "$DIST"/mytun-* | awk '{print "  "$9": "$5}'
( cd "$DIST" && sha256sum mytun-linux-amd64 mytun-linux-arm64 mytun-windows-amd64.exe mytun-darwin-arm64 > SHA256SUMS )
echo "== done =="
