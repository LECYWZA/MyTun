#!/usr/bin/env bash
# End-to-end smoke + throughput test using two Docker containers on one host.
#
# Both containers use host networking because MyTun creates listeners
# dynamically (port maps, proxy ports) and -p flags cannot cover them.
# Ports are chosen so the two instances never collide.
#
#   bash deploy/test-e2e.sh            # build image, start A/B, measure, clean up
#   KEEP=1 bash deploy/test-e2e.sh     # leave containers running afterwards
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${IMAGE:-mytun:test}"
A_PORT="${A_PORT:-8899}"
B_PORT="${B_PORT:-8900}"
TUNNEL_PORT="${TUNNEL_PORT:-33891}"
MAP_PORT="${MAP_PORT:-18080}"
PROXY_LOCAL="${PROXY_LOCAL:-11080}"
PROXY_REMOTE="${PROXY_REMOTE:-11081}"
TARGET_PORT="${TARGET_PORT:-18090}"
PW="${PW:-e2e-pass}"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
# apost <port> <cookiejar> <json-body> <path>  — sends the jar too so the
# HttpOnly session cookie set by /api/login is reused afterwards.
apost() { curl -s --noproxy '*' -b "$2" -c "$2" -X POST -H 'Content-Type: application/json' -d "$3" "http://127.0.0.1:$1$4"; }
aget() { curl -s --noproxy '*' -b "$2" "http://127.0.0.1:$1$3"; }

cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    say "keeping containers (mytun-a / mytun-b)"
    return
  fi
  say "cleanup"
  docker rm -f mytun-a mytun-b >/dev/null 2>&1 || true
  docker rm -f mytun-e2e-target >/dev/null 2>&1 || true
}
trap cleanup EXIT

say "build image"
docker build -t "$IMAGE" -f "$ROOT/Dockerfile" "$ROOT" >/dev/null

say "start a throwaway HTTP target on :$TARGET_PORT"
TMPDIR_E2E="$(mktemp -d)"
head -c 67108864 /dev/urandom > "$TMPDIR_E2E/big.bin"
echo hello-mytun > "$TMPDIR_E2E/index.html"
docker rm -f mytun-e2e-target >/dev/null 2>&1 || true
docker run -d --name mytun-e2e-target --network host -v "$TMPDIR_E2E:/srv:ro" \
  busybox:stable httpd -f -p "$TARGET_PORT" -h /srv >/dev/null

say "start A (server) and B (client)"
docker rm -f mytun-a mytun-b >/dev/null 2>&1 || true
docker run -d --name mytun-a --network host -v "$TMPDIR_E2E/a:/data" "$IMAGE" \
  --data /data --host 0.0.0.0 --port "$A_PORT" >/dev/null
sleep 1
docker run -d --name mytun-b --network host -v "$TMPDIR_E2E/b:/data" "$IMAGE" \
  --data /data --host 127.0.0.1 --port "$B_PORT" >/dev/null
sleep 4

say "configure B (client only, release default :$TUNNEL_PORT)"
apost "$B_PORT" /tmp/e2e-b.jar "{\"username\":\"admin\",\"password\":\"password\"}" /api/login >/dev/null
apost "$B_PORT" /tmp/e2e-b.jar "{\"servers\":[],\"clients\":[{\"id\":\"cl1\",\"alias\":\"B\",\"host\":\"127.0.0.1\",\"port\":$TUNNEL_PORT,\"password\":\"$PW\",\"clientId\":\"client-1\",\"sni\":\"mail.qq.com\",\"enabled\":true}],\"forwards\":[],\"proxies\":[],\"settings\":{}}" /api/config >/dev/null
sleep 1

say "configure A (server + map + proxies)"
apost "$A_PORT" /tmp/e2e-a.jar "{\"username\":\"admin\",\"password\":\"password\"}" /api/login >/dev/null
apost "$A_PORT" /tmp/e2e-a.jar "{\"servers\":[{\"id\":\"srv-a\",\"alias\":\"A\",\"bindHost\":\"0.0.0.0\",\"port\":$TUNNEL_PORT,\"password\":\"$PW\",\"commonName\":\"mail.qq.com\",\"enabled\":true}],\"clients\":[],\"forwards\":[{\"id\":\"fw1\",\"role\":\"server\",\"listenHost\":\"0.0.0.0\",\"listenPort\":$MAP_PORT,\"targetHost\":\"127.0.0.1\",\"targetPort\":$TARGET_PORT,\"targetClientId\":\"client-1\",\"enabled\":true}],\"proxies\":[{\"id\":\"px1\",\"role\":\"server\",\"listenHost\":\"0.0.0.0\",\"listenPort\":$PROXY_LOCAL,\"networkMode\":\"local\",\"enabled\":true},{\"id\":\"px2\",\"role\":\"server\",\"listenHost\":\"0.0.0.0\",\"listenPort\":$PROXY_REMOTE,\"networkMode\":\"remote\",\"targetClientId\":\"client-1\",\"enabled\":true}],\"settings\":{}}" /api/config >/dev/null
sleep 3

say "status"
aget "$A_PORT" /tmp/e2e-a.jar /api/status | head -c 400; echo

if ! aget "$A_PORT" /tmp/e2e-a.jar /api/status | grep -q '"inbound"'; then
  say "status probe failed; container logs:"
  docker logs --tail 20 mytun-a || true
  docker logs --tail 20 mytun-b || true
  exit 1
fi

say "throughput: port map ($MAP_PORT -> tunnel -> $TARGET_PORT)"
for i in 1 2 3; do
  curl -s --noproxy '*' -o /dev/null -w "  map      %{http_code} %{speed_download} B/s\n" --max-time 60 "http://127.0.0.1:$MAP_PORT/big.bin"
done

say "throughput: mixed proxy, remote egress"
for i in 1 2 3; do
  curl -s --noproxy '' -x "http://127.0.0.1:$PROXY_REMOTE" -o /dev/null -w "  proxy-r  %{http_code} %{speed_download} B/s\n" --max-time 60 "http://127.0.0.1:$TARGET_PORT/big.bin"
done

say "throughput: mixed proxy, local egress"
for i in 1 2 3; do
  curl -s --noproxy '' -x "http://127.0.0.1:$PROXY_LOCAL" -o /dev/null -w "  proxy-l  %{http_code} %{speed_download} B/s\n" --max-time 60 "http://127.0.0.1:$TARGET_PORT/big.bin"
done

say "latency: 20 sequential small requests through the tunnel"
start=$(date +%s%N)
for i in $(seq 1 20); do curl -s --noproxy '*' -o /dev/null "http://127.0.0.1:$MAP_PORT/index.html"; done
end=$(date +%s%N)
awk -v ns="$(( end - start ))" 'BEGIN{printf "  avg %.1f ms/req over 20 requests\n", (ns/1000000)/20}' 

say "memory"
docker stats --no-stream --format '  {{.Name}} {{.MemUsage}}' mytun-a mytun-b

say "done"
