#!/usr/bin/env bash
# End-to-end lifecycle test for sandboxd against a real Docker daemon.
# Covers the /v1 lifecycle: create -> list -> exec -> logs -> stop -> start ->
# restart -> delete, the container hardening flags (non-root user, no docker
# socket, only declared env), per-sandbox image/command/resources, preview
# port changes, metrics and volumes, input validation, the API source guard
# (a sandbox cannot reach the control plane), and the legacy /sandbox create
# path. No credentials needed.
#
# Inputs (env):
#   SANDBOXD_IMAGE  sandbox image tag (e.g. built from scripts/e2e/Dockerfile)
#   SANDBOXD_BIN    path to a built sandboxd binary
#
# Run locally:
#   docker build -t sandboxd-e2e:local scripts/e2e
#   (cd control-plane && go build -o /tmp/sandboxd ./cmd/sandboxd)
#   sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=/tmp/sandboxd bash scripts/e2e.sh
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  # Like production: sandboxd must be able to delete workspace files that the
  # sandbox user (uid 1000) wrote. Run with sudo.
  echo "❌ scripts/e2e.sh must run as root (sudo), like sandboxd in production" >&2
  exit 2
fi

IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DD="$(mktemp -d)"
NET="sbxe2e-$$"
PORT="${SANDBOXD_E2E_PORT:-9191}"
API="http://127.0.0.1:$PORT"
SBX_PID=""
declare -a IDS=()

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  [ -s "$DD/last.body" ] && { echo "---- last response body ----"; cat "$DD/last.body"; echo; }
  echo "---- sandboxd log (tail) ----"; tail -40 "$DD/sandboxd.log" 2>/dev/null || true
  exit 1
}

cleanup() {
  for id in "${IDS[@]:-}"; do
    [ -n "$id" ] && curl -s -m30 -o /dev/null -XPOST "$API/sandbox/$id/purge" >/dev/null 2>&1 || true
  done
  [ -n "$SBX_PID" ] && kill "$SBX_PID" >/dev/null 2>&1 || true
  [ -n "${WWW_PID:-}" ] && kill "$WWW_PID" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=sandboxd.managed=true" --filter "network=$NET" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  [ -n "${ALT_IMG:-}" ] && docker rmi "$ALT_IMG" >/dev/null 2>&1 || true
  rm -rf "$DD" 2>/dev/null || true
}
trap cleanup EXIT

http() { curl -s -m"${3:-30}" -o "$DD/last.body" -w '%{http_code}' -X"$1" "$API$2"; }
status() { curl -s -m5 "$API/v1/sandboxes/$1" | grep -oE '"status":"[a-z]+"' | head -1 | cut -d'"' -f4; }
cstate() { docker inspect -f '{{.State.Status}}' "s-$1" 2>/dev/null; }
wait_status() { # id want tries
  for _ in $(seq 1 "$3"); do [ "$(status "$1")" = "$2" ] && return 0; sleep 1; done; return 1
}

# Start sandboxd. Auth is disabled here: auth behaviour is covered by Go unit
# tests; this script exercises the Docker lifecycle only.
mkdir -p "$DD/log" "$DD/library"
docker network create "$NET" >/dev/null || fail "docker network create"
# A second tag of the same image stands in for an allowlisted custom image
# (no registry pull needed).
ALT_IMG="sandboxd-e2e-alt:local"
docker tag "$IMG" "$ALT_IMG" || fail "docker tag"
# Listen on all interfaces so the source-guard test below can try to reach the
# API from inside a sandbox (via the network gateway, i.e. the host).
# PLAN §8 #5: the egress proxy listens on the host side of the sandbox network.
GW="$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' "$NET")"
EPORT="${SANDBOXD_E2E_EGRESS_PORT:-9192}"
SANDBOXD_EGRESS_PROXY_ADDR="0.0.0.0:$EPORT" SANDBOXD_EGRESS_PROXY_URL="http://$GW:$EPORT" \
SANDBOXD_ADDR="0.0.0.0:$PORT" SANDBOXD_IMAGE_ALLOWLIST="$ALT_IMG" SANDBOXD_MAX_MEMORY_MB=1024 SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
  SANDBOXD_LOG_DIR="$DD/log" SANDBOXD_NETWORK="$NET" SANDBOXD_USERNS=host \
  SANDBOXD_LIBRARY_DIR="$DD/library" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
  SANDBOXD_DB="$DD/sandboxd.db" SANDBOXD_IDLE_THRESHOLD_SECONDS=3600 \
  SANDBOXD_API_AUTH_DISABLED=true \
  "$BIN" >"$DD/sandboxd.log" 2>&1 &
SBX_PID=$!
for _ in $(seq 1 60); do curl -s -m2 "$API/healthz" 2>/dev/null | grep -q ok && break; sleep 0.5; done
curl -s -m3 "$API/healthz" | grep -q ok || fail "sandboxd never became healthy"
log "sandboxd healthy"

jget() { # json field from stdin (top-level string/number)
  python3 -c 'import json,sys; d=json.load(sys.stdin); v=d
for k in sys.argv[1].split("."): v=v[int(k)] if k.isdigit() else v.get(k)
print("" if v is None else v)' "$1"
}

# 1. Create via /v1 -> container running.
log "create (/v1) -> container running"
RESP="$(curl -s -m120 -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"ports":[3000],"env":{"E2E_MARK":"from-env"}}')"
ID="$(echo "$RESP" | jget id)"
[ -n "$ID" ] || fail "create returned no id: $RESP"
IDS+=("$ID")
[ "$(echo "$RESP" | jget status)" = "running" ] || fail "create status is not running: $RESP"
[ "$(echo "$RESP" | jget preview.port)" = "3000" ] || fail "create response has no preview for port 3000: $RESP"
[ "$(cstate "$ID")" = "running" ] || fail "container s-$ID is '$(cstate "$ID")', want running"
docker inspect -f '{{index .Config.Labels "sandboxd.managed"}}' "s-$ID" | grep -q true || fail "container missing sandboxd.managed label"
log "create ✓ ($ID)"

# 2. List includes it; status filter works.
log "list"
curl -s -m10 "$API/v1/sandboxes" | grep -q "\"$ID\"" || fail "list does not include $ID"
curl -s -m10 "$API/v1/sandboxes?status=stopped" | grep -q "\"$ID\"" && fail "status=stopped returned a running sandbox"
log "list ✓"

# 3. Hardening flags are applied.
log "container hardening"
[ "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "s-$ID")" = "true" ] || fail "rootfs is not read-only"
docker inspect -f '{{.HostConfig.CapDrop}}' "s-$ID" | grep -q ALL || fail "capabilities not dropped"
docker inspect -f '{{.HostConfig.SecurityOpt}}' "s-$ID" | grep -q no-new-privileges || fail "no-new-privileges missing"
docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "s-$ID" | grep -q docker.sock && fail "docker.sock is mounted into the sandbox"
[ "$(docker inspect -f '{{len .Mounts}}' "s-$ID")" = "1" ] || fail "sandbox has mounts besides the workspace: $(docker inspect -f '{{json .Mounts}}' "s-$ID")"
[ "$(docker inspect -f '{{.Config.User}}' "s-$ID")" = "1000:1000" ] || fail "container user is not 1000:1000"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["id","-u"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "1000" ] || fail "sandbox process is not uid 1000: $OUT"
# Only the declared env reaches the sandbox, never the control plane's.
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","env | grep -c ^SANDBOXD_ || true"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "0" ] || fail "control-plane env leaked into the sandbox: $OUT"
log "hardening ✓"

# 3b. The sandbox cannot reach the control plane (PLAN §8 #13): a request from
# inside it to the host's API port is refused by the source guard.
log "api source guard"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' \
  -d "{\"cmd\":[\"bash\",\"-c\",\"exec 3<>/dev/tcp/$GW/$PORT && printf 'GET /v1/sandboxes HTTP/1.0\\\\r\\\\n\\\\r\\\\n' >&3 && head -1 <&3\"]}")"
echo "$OUT" | jget stdout | grep -q " 403" || fail "sandbox reached the API (want 403): $OUT"
log "api source guard ✓"

# 3c. Egress goes through the proxy, deny-by-default (PLAN §8 #5). A tiny HTTP
# server on the gateway stands in for "the internet" (no real network needed).
log "egress proxy"
sbsh() { # id script -> stdout
  python3 -c 'import json,sys; print(json.dumps({"cmd":["bash","-c",sys.argv[1]]}))' "$2" |
    curl -s -m30 -XPOST "$API/v1/sandboxes/$1/exec" -H 'content-type: application/json' -d @- | jget stdout
}
via_proxy() { # id host:port -> first response line of a proxied GET
  sbsh "$1" "exec 3<>/dev/tcp/$GW/$EPORT && printf 'GET http://$2/ HTTP/1.1\\r\\nHost: $2\\r\\nConnection: close\\r\\n\\r\\n' >&3 && head -1 <&3"
}
connect_proxy() { # id host:port -> first response line of a CONNECT
  sbsh "$1" "exec 3<>/dev/tcp/$GW/$EPORT && printf 'CONNECT $2 HTTP/1.1\\r\\nHost: $2\\r\\n\\r\\n' >&3 && head -1 <&3"
}
mkdir -p "$DD/www" && echo egress-ok > "$DD/www/index.html"
python3 -m http.server 18099 --bind "$GW" --directory "$DD/www" >/dev/null 2>&1 &
WWW_PID=$!
sleep 1
sbsh "$ID" 'echo $HTTPS_PROXY' | grep -q "http://$GW:$EPORT" || fail "HTTPS_PROXY not injected into the sandbox"
via_proxy "$ID" "$GW:18099" | grep -q " 403" || fail "empty allowlist must deny egress"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/network/egress" -H 'content-type: application/json' -d "{\"allow\":[\"$GW\"]}")"
[ "$code" = "403" ] || fail "egress outside the strict policy returned $code, want 403"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/policy/egress" -H 'content-type: application/json' -d "{\"mode\":\"strict\",\"allow\":[\"$GW\",\"169.254.169.254\"]}")"
[ "$code" = "200" ] || fail "policy egress returned $code"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/network/egress" -H 'content-type: application/json' -d "{\"allow\":[\"$GW\",\"169.254.169.254\"]}")"
[ "$code" = "200" ] || fail "sandbox egress returned $code"
via_proxy "$ID" "$GW:18099" | grep -q " 200" || fail "allowlisted destination denied: $(via_proxy "$ID" "$GW:18099")"
connect_proxy "$ID" "$GW:18099" | grep -q " 200" || fail "allowlisted CONNECT denied"
connect_proxy "$ID" "169.254.169.254:80" | grep -q " 403" || fail "cloud metadata must stay blocked even when allowlisted"
via_proxy "$ID" "example.com:80" | grep -q " 403" || fail "non-allowlisted host must be denied"

# Network modes + block list + live activity.
put_egress() { # body -> http code
  curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/network/egress" -H 'content-type: application/json' -d "$1"
}
code="$(put_egress "{\"deny\":[\"$GW\"]}")"; [ "$code" = "200" ] || fail "deny list returned $code"
via_proxy "$ID" "$GW:18099" | grep -q " 403" || fail "block list must win over the allow list"
code="$(put_egress '{"mode":"none","deny":[]}')"; [ "$code" = "200" ] || fail "mode none returned $code"
via_proxy "$ID" "$GW:18099" | grep -q " 403" || fail "mode none must deny everything"
code="$(put_egress '{"mode":"open"}')"; [ "$code" = "403" ] || fail "mode open under a strict policy returned $code, want 403"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/policy/egress" -H 'content-type: application/json' -d "{\"mode\":\"permissive\",\"allow\":[]}")"
[ "$code" = "200" ] || fail "policy permissive returned $code"
code="$(put_egress '{"mode":"open"}')"; [ "$code" = "200" ] || fail "mode open returned $code: $(cat "$DD/last.body")"
via_proxy "$ID" "$GW:18099" | grep -q " 200" || fail "open mode denied a destination the allow list names"
connect_proxy "$ID" "169.254.169.254:80" | grep -q " 403" || fail "cloud metadata must stay blocked in open mode"
code="$(put_egress "{\"deny\":[\"$GW/32\"]}")"; [ "$code" = "200" ] || fail "CIDR deny returned $code"
via_proxy "$ID" "$GW:18099" | grep -q " 403" || fail "CIDR block list must deny in open mode"
act="$(curl -s -m10 "$API/v1/sandboxes/$ID/network/activity")"
echo "$act" | python3 -c '
import json, sys
a = json.load(sys.stdin); gw = sys.argv[1]
h = {x["host"]: x for x in a["hosts"]}
assert a["recording"], "activity is not recording"
assert gw in h and h[gw]["allowed"] >= 1 and h[gw]["denied"] >= 1, h.get(gw)
assert any(e["host"] == gw and not e["allowed"] for e in a["events"]), "no denied event"
assert a["rules"]["mode"] == "open", a["rules"]
' "$GW" || fail "network activity is wrong: $act"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPUT "$API/v1/policy/egress" -H 'content-type: application/json' -d "{\"mode\":\"strict\",\"allow\":[\"$GW\",\"169.254.169.254\"]}")"
[ "$code" = "200" ] || fail "policy back to strict returned $code"
code="$(put_egress '{"mode":"allowlist","deny":[]}')"; [ "$code" = "200" ] || fail "back to allowlist returned $code"
kill "$WWW_PID" 2>/dev/null || true
log "egress proxy ✓"

# 4. Exec runs inside the sandbox and sees the requested env; non-zero exit is reported.
log "exec"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","echo $E2E_MARK"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "from-env" ] || fail "exec output/env mismatch: $OUT"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","exit 7"]}')"
[ "$(echo "$OUT" | jget exit_code)" = "7" ] || fail "exec exit code not reported: $OUT"
log "exec ✓"

# 4b. Exec v2 (AGENT_PLAN A0): in-container timeout kills the whole process
# group, the output bound keeps control-plane RAM bounded, ?stream=ndjson
# streams live, and a bare {cmd} request behaves exactly as before.
log "exec v2"
T0="$(date +%s%3N)"
OUT="$(curl -s -m10 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' \
  -d '{"cmd":["sleep","999"],"timeout_s":2,"max_output_bytes":1024}')"
T1="$(date +%s%3N)"
[ "$(echo "$OUT" | jget timed_out)" = "True" ] || fail "exec v2 timed_out: $OUT"
[ "$(echo "$OUT" | jget exit_code)" = "124" ] || fail "exec v2 exit_code: $OUT"
[ $((T1 - T0)) -le 6000 ] || fail "exec v2 timeout answered after $((T1 - T0))ms (want <= ~3s)"
docker top "s-$ID" 2>/dev/null | grep -q "sleep 999" && fail "sleep 999 survived the timeout in the container"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' \
  -d '{"cmd":["sh","-c","yes A | head -c 50000000"],"timeout_s":60,"max_output_bytes":4096}')"
[ "$(echo "$OUT" | jget truncated)" = "True" ] || fail "50 MB output not truncated: $(echo "$OUT" | head -c 200)"
[ "$(echo "$OUT" | jget exit_code)" = "0" ] || fail "truncated run exit: $OUT"
[ "$(echo "$OUT" | wc -c)" -lt 30000 ] || fail "truncated response still huge: $(echo "$OUT" | wc -c) bytes"
OUT="$(curl -s -m10 -XPOST "$API/v1/sandboxes/$ID/exec?stream=ndjson" -H 'content-type: application/json' \
  -d '{"cmd":["sh","-c","echo nd-hello"],"timeout_s":10}')"
echo "$OUT" | grep -q '"stream":"stdout"' || fail "ndjson missing stdout lines: $OUT"
echo "$OUT" | tail -1 | grep -q '"exit_code":0' || fail "ndjson last line not the exit status: $OUT"
ND_DUR="$(echo "$OUT" | tail -1 | python3 -c 'import json,sys; print(json.loads(sys.stdin.read())["duration_ms"] >= 0)')"
[ "$ND_DUR" = "True" ] || fail "ndjson duration_ms missing"
for BODY in '{"cmd":["ls"],"cwd":"/etc"}' '{"cmd":["ls"],"env":{"PATH":"/x"}}' '{"cmd":["ls"],"timeout_s":0}' '{"cmd":["ls"],"max_output_bytes":999999999}' ; do
  code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d "$BODY")"
  [ "$code" = "400" ] || fail "exec v2 validation ($BODY) returned $code, want 400"
done
OUT="$(curl -s -m10 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","echo legacy-ok"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "legacy-ok" ] || fail "legacy {cmd} broke: $OUT"
echo "$OUT" | grep -q '"timed_out":false' || fail "legacy response lost the timed_out field"
log "exec v2 ✓"

# 4c. Files API (AGENT_PLAN A0): write/read roundtrip with sha256, traversal
# and symlink escapes refused, listing exclusions, if_match, move, delete.
log "files api"
CONTENT="portal-a0-hello"
SHA="$(printf '%s' "$CONTENT" | curl -s -m10 -XPUT "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt" --data-binary @- | jget sha256)"
[ "$SHA" = "$(printf '%s' "$CONTENT" | sha256sum | cut -d' ' -f1)" ] || fail "files sha256 mismatch: $SHA"
OUT="$(curl -s -m10 "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt")"
[ "$(echo "$OUT" | jget encoding)" = "utf-8" ] || fail "read encoding: $OUT"
[ "$(echo "$OUT" | jget data)" = "$CONTENT" ] || fail "read data: $OUT"
printf '\x00\x01\x02binary' > "$DD/blob.bin"
BIN_SHA="$(sha256sum "$DD/blob.bin" | cut -d' ' -f1)"
BIN_RESP="$(curl -s -m10 -XPUT "$API/v1/sandboxes/$ID/files/content?path=a0%2Fblob.bin" --data-binary @"$DD/blob.bin")"
[ "$(echo "$BIN_RESP" | jget sha256)" = "$BIN_SHA" ] || fail "binary write sha256: $BIN_RESP"
OUT="$(curl -s -m10 "$API/v1/sandboxes/$ID/files/content?path=a0%2Fblob.bin")"
[ "$(echo "$OUT" | jget encoding)" = "base64" ] || fail "binary encoding: $OUT"
OUT="$(curl -s -m10 -o /dev/null -w '%{http_code}' "$API/v1/sandboxes/$ID/files/content?path=..%2F..%2Fetc%2Fpasswd")"
[ "$OUT" != "200" ] || fail "traversal read leaked /etc/passwd"
curl -s -m10 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["ln","-s","/etc/passwd","/home/sandbox/leak"]}' >/dev/null
OUT="$(curl -s -m10 -o /dev/null -w '%{http_code}' "$API/v1/sandboxes/$ID/files/content?path=leak")"
[ "$OUT" != "200" ] || fail "symlink read leaked /etc/passwd"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/files/content?path=leak" --data-binary PWNED)"
[ "$code" = "400" ] || fail "symlink overwrite returned $code, want 400"
curl -s -m10 -XPUT "$API/v1/sandboxes/$ID/files/content?path=node_modules%2Fx.js" --data-binary 'js' >/dev/null
OUT="$(curl -s -m10 "$API/v1/sandboxes/$ID/files?recursive=true")"
echo "$OUT" | grep -q '"a0/hello.txt"' || fail "listing missing a0/hello.txt: $(echo "$OUT" | head -c 300)"
echo "$OUT" | grep -q 'node_modules' && fail "listing included node_modules"
curl -s -m10 -XPUT "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt" --data-binary "$CONTENT" >/dev/null
GOOD_SHA="$SHA"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt&if_match=0000000000000000000000000000000000000000000000000000000000000000" --data-binary X)"
[ "$code" = "409" ] || fail "stale if_match returned $code, want 409"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPUT "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt&if_match=$GOOD_SHA" --data-binary "$CONTENT")"
[ "$code" = "200" ] || fail "matching if_match returned $code, want 200"
OUT="$(curl -s -m10 -XPOST "$API/v1/sandboxes/$ID/files/move" -H 'content-type: application/json' -d '{"from":"a0/hello.txt","to":"a0/renamed.txt"}')"
echo "$OUT" | grep -q '"to":"a0/renamed.txt"' || fail "move: $OUT"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' "$API/v1/sandboxes/$ID/files/content?path=a0%2Fhello.txt")"
[ "$code" = "404" ] || fail "old path still readable after move: $code"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XDELETE "$API/v1/sandboxes/$ID/files?path=a0%2Frenamed.txt")"
[ "$code" = "204" ] || fail "delete file returned $code, want 204"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XDELETE "$API/v1/sandboxes/$ID/files?path=a0%2Fblob.bin")"
[ "$code" = "204" ] || fail "delete blob returned $code, want 204"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XDELETE "$API/v1/sandboxes/$ID/files?path=a0")"
[ "$code" = "204" ] || fail "delete empty dir returned $code, want 204"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XDELETE "$API/v1/sandboxes/$ID/files?path=a0")"
[ "$code" = "404" ] || fail "delete missing returned $code, want 404"
log "files api ✓"

# 5. Logs: the main process writes nothing, so seed a line via PID 1's stdout.
log "logs"
curl -s -m30 -o /dev/null -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","echo e2e-log-line > /proc/1/fd/1"]}'
curl -s -m10 "$API/v1/sandboxes/$ID/logs?tail=50" | grep -q e2e-log-line || fail "logs missing the seeded line"
# follow=true never ends on its own; -m3 cuts it (curl exit 28), so capture first
# instead of piping (pipefail would report the timeout, not the grep).
SSE="$(curl -s -m3 -N "$API/v1/sandboxes/$ID/logs?tail=5&follow=true" 2>/dev/null || true)"
echo "$SSE" | grep -q "^data: e2e-log-line" || fail "SSE logs missing the seeded line: $SSE"
log "logs ✓"

# 5b. Preview ports change on a running sandbox: the container is rebuilt with
# the new routes and stays running; the workspace survives.
log "network ports"
curl -s -m30 -o /dev/null -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","echo keep > /home/sandbox/keep.txt"]}'
OUT="$(curl -s -m120 -XPOST "$API/v1/sandboxes/$ID/network/ports" -H 'content-type: application/json' -d '{"container":8080}')"
echo "$OUT" | grep -q '"container":8080' || fail "add port: $OUT"
wait_status "$ID" running 30 || fail "sandbox not running after a port change"
docker inspect -f '{{json .Config.Labels}}' "s-$ID" | grep -q "s-$ID-8080" || fail "no preview router for 8080 after the change"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["cat","/home/sandbox/keep.txt"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "keep" ] || fail "workspace lost across the rebuild: $OUT"
OUT="$(curl -s -m120 -XDELETE "$API/v1/sandboxes/$ID/network/ports/8080")"
echo "$OUT" | grep -q '"container":8080' && fail "delete port: $OUT"
docker inspect -f '{{json .Config.Labels}}' "s-$ID" | grep -q "s-$ID-8080" && fail "8080 router still present"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes/$ID/network/ports" -H 'content-type: application/json' -d '{"container":70000}')"
[ "$code" = "400" ] || fail "invalid port returned $code, want 400"
log "network ports ✓"

# 5c. Metrics and volumes.
log "metrics / volumes"
OUT="$(curl -s -m30 "$API/v1/sandboxes/$ID/metrics")"
[ "$(echo "$OUT" | jget status)" = "running" ] || fail "metrics status: $OUT"
[ "$(echo "$OUT" | jget stats.pids)" -ge 1 ] 2>/dev/null || fail "metrics pids: $OUT"
[ "$(echo "$OUT" | jget stats.mem_limit_bytes)" -gt 0 ] 2>/dev/null || fail "metrics mem limit: $OUT"
SSE="$(curl -s -m4 -N "$API/v1/sandboxes/$ID/metrics/stream?interval=1" 2>/dev/null || true)"
echo "$SSE" | grep -q '^data: {"sandbox_id"' || fail "metrics stream: $SSE"
OUT="$(curl -s -m30 "$API/v1/sandboxes/$ID/volumes")"
echo "$OUT" | grep -q '"destination":"/home/sandbox"' || fail "volumes: $OUT"
log "metrics / volumes ✓"

# 5d. Per-sandbox image, command and resources (clamped to the instance max).
log "custom image / command / resources"
RESP="$(curl -s -m120 -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' \
  -d "{\"image\":\"$ALT_IMG\",\"command\":[\"bash\",\"-c\",\"echo custom-cmd; exec sleep infinity\"],\"resources\":{\"cpus\":0.5,\"memory_mb\":99999}}")"
CID="$(echo "$RESP" | jget id)"
[ -n "$CID" ] || fail "custom create: $RESP"
IDS+=("$CID")
[ "$(echo "$RESP" | jget image)" = "$ALT_IMG" ] || fail "custom image not reported: $RESP"
[ "$(echo "$RESP" | jget resources.memory_mb)" = "1024" ] || fail "memory not clamped to 1024: $RESP"
[ "$(docker inspect -f '{{.Config.Image}}' "s-$CID")" = "$ALT_IMG" ] || fail "container not on the custom image"
[ "$(docker inspect -f '{{.HostConfig.Memory}}' "s-$CID")" = "1073741824" ] || fail "memory limit not applied"
[ "$(docker inspect -f '{{.HostConfig.NanoCpus}}' "s-$CID")" = "500000000" ] || fail "cpu limit not applied"
sleep 1
curl -s -m10 "$API/v1/sandboxes/$CID/logs?tail=20" | grep -q custom-cmd || fail "custom command did not run"
code="$(http DELETE "/v1/sandboxes/$CID" 60)"
[ "$code" = "204" ] || fail "delete custom sandbox returned $code"
unset 'IDS[${#IDS[@]}-1]'
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"image":"busybox:latest"}')"
[ "$code" = "400" ] || fail "image outside the allowlist returned $code, want 400"
log "custom image / command / resources ✓"

# 6. Validation: bad payloads -> 400, unknown id -> 404, exec on stopped -> 409 (below).
log "validation"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"ports":"nope"}')"
[ "$code" = "400" ] || fail "bad payload returned $code, want 400"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"template":"react-standard"}')"
[ "$code" = "400" ] || fail "unknown field returned $code, want 400"
code="$(http GET /v1/sandboxes/01JZZZZZZZZZZZZZZZZZZZZZZZ 10)"
[ "$code" = "404" ] || fail "unknown id returned $code, want 404"
log "validation ✓"

# 7. Stop -> container exited; exec refused; start -> running; restart -> running.
log "stop -> start -> restart"
code="$(http POST "/v1/sandboxes/$ID/stop" 60)"
[ "$code" = "200" ] || fail "stop returned $code"
wait_status "$ID" stopped 15 || fail "status never became stopped"
[ "$(cstate "$ID")" = "exited" ] || fail "container is '$(cstate "$ID")' after stop, want exited"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["true"]}')"
[ "$code" = "409" ] || fail "exec on stopped sandbox returned $code, want 409"
code="$(http POST "/v1/sandboxes/$ID/start" 120)"
[ "$code" = "200" ] || fail "start returned $code"
wait_status "$ID" running 30 || fail "status never became running after start"
[ "$(cstate "$ID")" = "running" ] || fail "container is '$(cstate "$ID")' after start, want running"
STARTED1="$(docker inspect -f '{{.State.StartedAt}}' "s-$ID")"
code="$(http POST "/v1/sandboxes/$ID/restart" 120)"
[ "$code" = "200" ] || fail "restart returned $code"
wait_status "$ID" running 30 || fail "status never became running after restart"
[ "$(docker inspect -f '{{.State.StartedAt}}' "s-$ID")" != "$STARTED1" ] || fail "restart did not restart the container"
log "stop/start/restart ✓"

# 8. Delete -> container and row gone.
log "delete"
code="$(http DELETE "/v1/sandboxes/$ID" 60)"
[ "$code" = "204" ] || fail "delete returned $code, want 204"
docker inspect "s-$ID" >/dev/null 2>&1 && fail "container s-$ID still exists after delete"
code="$(http GET "/v1/sandboxes/$ID" 10)"
[ "$code" = "404" ] || fail "deleted sandbox returned $code, want 404"
IDS=()
log "delete ✓"

# 9. Legacy create path still works (deprecated, kept until clients move).
log "legacy POST /sandbox"
LID="$(curl -s -m120 -XPOST "$API/sandbox" -H 'content-type: application/json' -d '{}' | jget id)"
[ -n "$LID" ] || fail "legacy create returned no id"
IDS+=("$LID")
[ "$(cstate "$LID")" = "running" ] || fail "legacy sandbox not running"
code="$(http DELETE "/v1/sandboxes/$LID" 60)"
[ "$code" = "204" ] || fail "delete of legacy sandbox returned $code"
IDS=()
log "legacy ✓"

echo "✅ E2E PASSED"
