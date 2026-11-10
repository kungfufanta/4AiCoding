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
GW="$(docker network inspect -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' "$NET")"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' \
  -d "{\"cmd\":[\"bash\",\"-c\",\"exec 3<>/dev/tcp/$GW/$PORT && printf 'GET /v1/sandboxes HTTP/1.0\\\\r\\\\n\\\\r\\\\n' >&3 && head -1 <&3\"]}")"
echo "$OUT" | jget stdout | grep -q " 403" || fail "sandbox reached the API (want 403): $OUT"
log "api source guard ✓"

# 4. Exec runs inside the sandbox and sees the requested env; non-zero exit is reported.
log "exec"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","echo $E2E_MARK"]}')"
[ "$(echo "$OUT" | jget stdout | tr -d '\n')" = "from-env" ] || fail "exec output/env mismatch: $OUT"
OUT="$(curl -s -m30 -XPOST "$API/v1/sandboxes/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["sh","-c","exit 7"]}')"
[ "$(echo "$OUT" | jget exit_code)" = "7" ] || fail "exec exit code not reported: $OUT"
log "exec ✓"

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
