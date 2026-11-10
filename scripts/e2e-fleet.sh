#!/usr/bin/env bash
# End-to-end fleet test for sandboxd (M6): the control plane's node/fleet
# surface driven by REAL worker processes.
#
#   - the control plane boots with a worker token and its own role='local'
#     node appears in /v1/nodes
#   - the /internal door is closed without the worker token
#   - two workers (two `sandboxd worker` processes, as on two machines)
#     register and heartbeat; their capacity lands in /v1/nodes and
#     /v1/capacity
#   - a created sandbox is recorded as placed on the local node
#   - draining the local node blocks sandbox creation (503); undraining
#     re-opens it
#   - a drained worker learns its state from the heartbeat response
#   - a worker that stops heartbeating goes offline after the lease
#
# Inputs (env):
#   SANDBOXD_IMAGE  sandbox image tag (e.g. built from scripts/e2e/Dockerfile)
#   SANDBOXD_BIN    path to a built sandboxd binary
#
# Run locally:
#   docker build -t sandboxd-e2e:local scripts/e2e
#   (cd control-plane && go build -o /tmp/sandboxd ./cmd/sandboxd)
#   sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=/tmp/sandboxd bash scripts/e2e-fleet.sh
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "❌ scripts/e2e-fleet.sh must run as root (sudo), like sandboxd in production" >&2
  exit 2
fi

IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DD="$(mktemp -d)"
PORT="${SANDBOXD_E2E_PORT:-9195}"
API="http://127.0.0.1:$PORT"
TOKEN="fleet-e2e-worker-token"
LEASE=6            # seconds a worker stays online after its last heartbeat
BEAT=2             # worker heartbeat period for the test
CP_PID=""
W1_PID=""
W2_PID=""
SBX_ID=""

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  [ -s "$DD/last.body" ] && { echo "---- last response body ----"; cat "$DD/last.body"; echo; }
  echo "---- sandboxd log (tail) ----"; tail -30 "$DD/sandboxd.log" 2>/dev/null || true
  echo "---- worker logs (tail) ----"
  tail -10 "$DD/w1.log" 2>/dev/null || true
  tail -10 "$DD/w2.log" 2>/dev/null || true
  exit 1
}

cleanup() {
  [ -n "$SBX_ID" ] && curl -s -m30 -o /dev/null -XPOST "$API/sandbox/$SBX_ID/purge" >/dev/null 2>&1 || true
  for p in "$W2_PID" "$W1_PID" "$CP_PID"; do
    [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true
  done
  docker ps -aq --filter "label=sandboxd.managed=true" | xargs -r docker rm -f >/dev/null 2>&1 || true
  rm -rf "$DD" 2>/dev/null || true
}
trap cleanup EXIT

http() { curl -s -m"${3:-30}" -o "$DD/last.body" -w '%{http_code}' -X"$1" "$API$2"; }
jget() { # json field from stdin (top-level string/number)
  python3 -c 'import json,sys; d=json.load(sys.stdin); v=d
for k in sys.argv[1].split("."): v=v[int(k)] if k.isdigit() else v.get(k)
print("" if v is None else v)' "$1"
}
nodes_json() { curl -s -m10 "$API/v1/nodes"; }
node_field() { # node_id field -> value
  nodes_json | python3 -c '
import json,sys
d=json.load(sys.stdin)
for n in d.get("nodes",[]):
    if n["node_id"]==sys.argv[1]:
        v=n
        for k in sys.argv[2].split("."):
            v=v[int(k)] if k.isdigit() else v.get(k)
        print("" if v is None else v)
        break' "$1" "$2"
}
wait_node_online() { # node_id tries
  for _ in $(seq 1 "$2"); do
    [ "$(node_field "$1" status)" = "online" ] && return 0
    sleep 1
  done
  return 1
}

# --- boot the control plane -------------------------------------------------
log "boot control plane (worker token set, short lease)"
mkdir -p "$DD/log" "$DD/workers"
SANDBOXD_ADDR="0.0.0.0:$PORT" SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
  SANDBOXD_LOG_DIR="$DD/log" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
  SANDBOXD_DB="$DD/sandboxd.db" SANDBOXD_API_AUTH_DISABLED=true \
  SANDBOXD_WORKER_TOKEN="$TOKEN" SANDBOXD_NODE_LEASE_SECONDS="$LEASE" \
  SANDBOXD_CAPACITY_CPUS=2 SANDBOXD_CAPACITY_MEMORY_MB=4096 \
  "$BIN" >"$DD/sandboxd.log" 2>&1 &
CP_PID=$!
for _ in $(seq 1 60); do curl -s -m2 "$API/healthz" 2>/dev/null | grep -q ok && break; sleep 0.5; done
curl -s -m3 "$API/healthz" | grep -q ok || fail "sandboxd never became healthy"

# 1. The local node exists, is online, and carries the configured capacity.
log "local node in /v1/nodes"
[ "$(node_field local role)" = "local" ] || fail "no role=local node: $(nodes_json)"
[ "$(node_field local status)" = "online" ] || fail "local node not online"
[ "$(node_field local capacity.cpus)" = "2" ] || fail "local capacity.cpus: $(nodes_json)"
[ "$(node_field local drain_state)" = "active" ] || fail "local drain_state: $(nodes_json)"
log "local node ✓"

# 2. /internal is closed without the (correct) worker token.
log "internal door closed without token"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPOST "$API/internal/nodes/register" -H 'content-type: application/json' -d '{"node_id":"w1"}')"
[ "$code" = "404" ] || fail "register without token = $code, want 404"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPOST "$API/internal/nodes/register" -H 'content-type: application/json' -H "Authorization: Bearer wrong" -d '{"node_id":"w1"}')"
[ "$code" = "404" ] || fail "register with a wrong token = $code, want 404"
# ...and a worker token must not open /v1.
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' "$API/v1/nodes" -H "Authorization: Bearer $TOKEN")"
[ "$code" = "403" ] || fail "worker token on /v1 = $code, want 403"
log "internal door ✓"

# 3. Two real workers register (the same binary, worker mode — the shape of
#    two machines joining the fleet).
log "two workers register + heartbeat"
"$BIN" worker --control-plane-url "$API" --worker-token "$TOKEN" \
  --node-id w1 --address "http://w1.example:9000" \
  --capacity-cpus 4 --capacity-memory-mb 8192 --heartbeat-interval-sec "$BEAT" \
  >"$DD/w1.log" 2>&1 &
W1_PID=$!
"$BIN" worker --control-plane-url "$API" --worker-token "$TOKEN" \
  --node-id w2 --capacity-cpus 2 --capacity-memory-mb 4096 --heartbeat-interval-sec "$BEAT" \
  >"$DD/w2.log" 2>&1 &
W2_PID=$!
wait_node_online w1 20 || fail "w1 never came online: $(nodes_json)"
wait_node_online w2 20 || fail "w2 never came online: $(nodes_json)"
[ "$(node_field w1 role)" = "worker" ] || fail "w1 role: $(nodes_json)"
[ "$(node_field w1 capacity.cpus)" = "4" ] || fail "w1 capacity: $(nodes_json)"
[ "$(node_field w1 capacity.memory_mb)" = "8192" ] || fail "w1 memory capacity: $(nodes_json)"
grep -q "worker registered" "$DD/w1.log" || fail "w1 log has no register line: $(cat "$DD/w1.log")"
log "workers online ✓"

# 4. Fleet capacity totals include both workers + local.
log "fleet capacity"
CAP="$(curl -s -m10 "$API/v1/capacity")"
[ "$(echo "$CAP" | jget nodes)" = "3" ] || fail "capacity nodes: $CAP"
[ "$(echo "$CAP" | jget online)" = "3" ] || fail "capacity online: $CAP"
[ "$(echo "$CAP" | jget capacity.cpus)" = "8" ] || fail "capacity cpus total (2+4+2): $CAP"
[ "$(echo "$CAP" | jget capacity.memory_mb)" = "16384" ] || fail "capacity memory total: $CAP"
log "fleet capacity ✓"

# 5. A created sandbox is placed on the local node and counted there.
log "create -> placement on local"
RESP="$(curl -s -m120 -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"ports":[3000]}')"
SBX_ID="$(echo "$RESP" | jget id)"
[ -n "$SBX_ID" ] || fail "create returned no id: $RESP"
[ "$(echo "$RESP" | jget node)" = "local" ] || fail "create response node: $RESP"
[ "$(node_field local used.sandboxes)" = "1" ] || fail "local used.sandboxes: $(nodes_json)"
[ "$(node_field local used.cpus)" != "" ] || fail "local used.cpus empty"
log "placement ✓ ($SBX_ID)"

# 6. Draining the local node blocks creation; undraining re-opens it.
log "drain blocks create"
code="$(http POST /v1/nodes/local/drain 10)"
[ "$code" = "200" ] || fail "drain local = $code"
[ "$(node_field local drain_state)" = "drain" ] || fail "local not draining after drain"
code="$(curl -s -m30 -o "$DD/last.body" -w '%{http_code}' -XPOST "$API/v1/sandboxes" -H 'content-type: application/json' -d '{"ports":[3000]}')"
[ "$code" = "503" ] || fail "create on drained node = $code, want 503"
grep -q "draining" "$DD/last.body" || fail "503 body does not mention draining: $(cat "$DD/last.body")"
code="$(http POST /v1/nodes/local/undrain 10)"
[ "$code" = "200" ] || fail "undrain local = $code"
code="$(http POST /v1/sandboxes 30)"
[ "$code" = "201" ] || fail "create after undrain = $code, want 201"
log "drain block ✓"

# 7. Drain a WORKER: its heartbeat tells it so; creation is unaffected.
log "drain worker w1"
code="$(http POST /v1/nodes/w1/drain 10)"
[ "$code" = "200" ] || fail "drain w1 = $code"
for _ in $(seq 1 10); do
  grep -q "DRAINED" "$DD/w1.log" && break
  sleep 1
done
grep -q "DRAINED" "$DD/w1.log" || fail "w1 never learned it was drained: $(cat "$DD/w1.log")"
code="$(http POST /v1/sandboxes 30)"
[ "$code" = "201" ] || fail "create while a worker is drained = $code, want 201 (local still active)"
code="$(http POST /v1/nodes/w1/undrain 10)"
[ "$code" = "200" ] || fail "undrain w1 = $code"
for _ in $(seq 1 10); do
  grep -q "worker is active" "$DD/w1.log" && break
  sleep 1
done
log "worker drain ✓"

# 8. A worker that stops heartbeating goes offline after the lease.
log "worker offline after lease"
kill "$W2_PID" 2>/dev/null || true
W2_PID=""
sleep $((LEASE + BEAT + 4))
[ "$(node_field w2 status)" = "offline" ] || fail "w2 still $(node_field w2 status) after the lease, want offline"
CAP="$(curl -s -m10 "$API/v1/capacity")"
[ "$(echo "$CAP" | jget offline)" = "1" ] || fail "capacity offline count: $CAP"
log "offline ✓"

# 9. Heartbeat for an unknown node -> 404 (the worker must re-register).
log "unknown node heartbeat"
code="$(curl -s -m10 -o "$DD/last.body" -w '%{http_code}' -XPOST "$API/internal/nodes/heartbeat" -H 'content-type: application/json' -H "Authorization: Bearer $TOKEN" -d '{"node_id":"ghost"}')"
[ "$code" = "404" ] || fail "unknown node heartbeat = $code, want 404"
log "unknown node ✓"

# 10. Cleanup: delete the sandbox; its placement goes with it.
log "cleanup sandbox"
code="$(http DELETE "/v1/sandboxes/$SBX_ID" 60)"
[ "$code" = "204" ] || fail "delete = $code"
[ "$(node_field local used.sandboxes)" = "" ] || [ "$(node_field local used.sandboxes)" = "0" ] || fail "placement survived delete: $(nodes_json)"
SBX_ID=""
log "cleanup ✓"

echo "✅ FLEET E2E PASSED"
