#!/usr/bin/env bash
# End-to-end lifecycle test for sandboxd against a real Docker daemon.
# Covers: create -> running container -> exec -> stop -> start -> delete,
# plus input validation. No in-sandbox supervisor and no credentials needed.
#
# Inputs (env):
#   SANDBOXD_IMAGE  sandbox image tag (e.g. built from scripts/e2e/Dockerfile)
#   SANDBOXD_BIN    path to a built sandboxd binary
#
# Run locally:
#   docker build -t sandboxd-e2e:local scripts/e2e
#   (cd control-plane && go build -o /tmp/sandboxd ./cmd/sandboxd)
#   SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=/tmp/sandboxd bash scripts/e2e.sh
set -uo pipefail

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

http() { curl -s -m"${3:-30}" -o /dev/null -w '%{http_code}' -X"$1" "$API$2"; }
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

# 1. Create -> container running.
log "create -> container running"
RESP="$(curl -s -m120 -XPOST "$API/sandbox" -H 'content-type: application/json' -d '{"ports":[3000]}')"
ID="$(echo "$RESP" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)"
[ -n "$ID" ] || fail "create returned no id: $RESP"
IDS+=("$ID")
[ "$(cstate "$ID")" = "running" ] || fail "container s-$ID is '$(cstate "$ID")', want running"
docker inspect -f '{{index .Config.Labels "sandboxd.managed"}}' "s-$ID" | grep -q true || fail "container missing sandboxd.managed label"
[ "$(status "$ID")" = "running" ] || fail "v1 status is '$(status "$ID")', want running"
log "create ✓ ($ID)"

# 2. Hardening flags are applied.
log "container hardening"
[ "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "s-$ID")" = "true" ] || fail "rootfs is not read-only"
docker inspect -f '{{.HostConfig.CapDrop}}' "s-$ID" | grep -q ALL || fail "capabilities not dropped"
docker inspect -f '{{.HostConfig.SecurityOpt}}' "s-$ID" | grep -q no-new-privileges || fail "no-new-privileges missing"
log "hardening ✓"

# 3. Exec runs inside the sandbox.
log "exec"
OUT="$(curl -s -m30 -XPOST "$API/sandbox/$ID/exec" -H 'content-type: application/json' -d '{"cmd":["echo","hello-e2e"]}')"
echo "$OUT" | grep -q hello-e2e || fail "exec output missing marker: $OUT"
log "exec ✓"

# 4. Invalid payload -> 400, unknown id -> 404.
log "validation"
code="$(curl -s -m10 -o /dev/null -w '%{http_code}' -XPOST "$API/sandbox" -H 'content-type: application/json' -d '{"ports":"nope"}')"
[ "$code" = "400" ] || fail "bad payload returned $code, want 400"
code="$(http GET /v1/sandboxes/01JZZZZZZZZZZZZZZZZZZZZZZZ 10)"
[ "$code" = "404" ] || fail "unknown id returned $code, want 404"
log "validation ✓"

# 5. Stop -> container exited; start -> running again.
log "stop -> start"
code="$(http POST "/v1/sandboxes/$ID/stop" 60)"
[ "$code" = "200" ] || fail "stop returned $code"
wait_status "$ID" stopped 15 || fail "status never became stopped"
[ "$(cstate "$ID")" = "exited" ] || fail "container is '$(cstate "$ID")' after stop, want exited"
code="$(http POST "/v1/sandboxes/$ID/start" 120)"
[ "$code" = "200" ] || fail "start returned $code"
wait_status "$ID" running 30 || fail "status never became running after start"
[ "$(cstate "$ID")" = "running" ] || fail "container is '$(cstate "$ID")' after start, want running"
log "stop/start ✓"

# 6. Delete -> container and row gone.
log "delete"
code="$(http DELETE "/v1/sandboxes/$ID" 60)"
[ "$code" = "204" ] || fail "delete returned $code, want 204"
docker inspect "s-$ID" >/dev/null 2>&1 && fail "container s-$ID still exists after delete"
code="$(http GET "/v1/sandboxes/$ID" 10)"
[ "$code" = "404" ] || fail "deleted sandbox returned $code, want 404"
IDS=()
log "delete ✓"

echo "✅ E2E PASSED"
