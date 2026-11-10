#!/usr/bin/env bash
# End-to-end lifecycle test for sandboxd against a real Docker daemon.
# Covers the /v1 lifecycle: create -> list -> exec -> logs -> stop -> start ->
# restart -> delete, the container hardening flags, input validation, and the
# legacy /sandbox create path. No credentials needed.
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
SANDBOXD_ADDR="127.0.0.1:$PORT" SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
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
log "hardening ✓"

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
