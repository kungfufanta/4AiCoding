#!/usr/bin/env bash
# End-to-end test of sandboxes executed on a worker (PLAN M7b), with two real
# sandboxd daemons on the runner's Docker: a control plane and a worker in
# executor mode (SANDBOXD_CONTROL_PLANE_URL). Auth is on for both.
#
#   - the worker joins with its own API token; its API refuses every other
#     credential
#   - a create on the control plane lands on the worker (more free memory),
#     the container exists, and exec / logs / list / get go through the
#     control plane
#   - the control plane's policy reaches the worker (keep-alive allowed
#     only after the policy says so)
#   - deleting a keep-alive sandbox on the worker is parked for approval on
#     the control plane, approved by an admin, then forwarded
#   - a drained worker gets no new sandboxes; the node can then be removed
#
#   sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=/tmp/sandboxd bash scripts/e2e-fleet-exec.sh
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "❌ scripts/e2e-fleet-exec.sh must run as root (sudo), like sandboxd in production" >&2
  exit 2
fi

IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DD="$(mktemp -d)"
NET="sbxexec-$$"
CP_PORT="${SANDBOXD_E2E_CP_PORT:-9301}"
W_PORT="${SANDBOXD_E2E_W_PORT:-9302}"
CP="http://127.0.0.1:$CP_PORT"
WK="http://127.0.0.1:$W_PORT"
TOKEN="sk_e2e_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
WTOKEN="wk_e2e_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
ADMIN_PW="e2e-admin-pass-1"
CP_PID=""
W_PID=""
declare -a IDS=()

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  [ -s "$DD/body" ] && { echo "---- last response body ----"; cat "$DD/body"; echo; }
  echo "---- control plane log (tail) ----"; tail -25 "$DD/cp/sandboxd.log" 2>/dev/null || true
  echo "---- worker log (tail) ----"; tail -25 "$DD/w1/sandboxd.log" 2>/dev/null || true
  exit 1
}
cleanup() {
  for id in "${IDS[@]:-}"; do
    [ -n "$id" ] && curl -s -m30 -o /dev/null -H "Authorization: Bearer $TOKEN" -XDELETE "$CP/v1/sandboxes/$id" || true
  done
  for p in "$W_PID" "$CP_PID"; do [ -n "$p" ] && kill "$p" >/dev/null 2>&1 || true; done
  sleep 1
  docker ps -aq --filter "label=sandboxd.managed=true" --filter "network=$NET" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$DD" 2>/dev/null || true
}
trap cleanup EXIT

# req <base> <who> <method> <path> [json] [extra curl args...] -> HTTP code;
# body in $DD/body. who: token | none | node | jar (admin cookie + CSRF).
req() {
  local base="$1" who="$2" m="$3" p="$4" body="${5:-}"
  shift 5 2>/dev/null || shift $#
  local -a a=(-s -m120 -o "$DD/body" -w '%{http_code}' -X "$m")
  case "$who" in
    token) a+=(-H "Authorization: Bearer $TOKEN") ;;
    node) a+=(-H "Authorization: Bearer $(cat "$DD/w1/state/node-api-token" 2>/dev/null)") ;;
    jar) a+=(-b "$DD/admin.jar" -c "$DD/admin.jar" -H "X-Sandboxd-CSRF: 1") ;;
  esac
  [ -n "$body" ] && a+=(-H 'content-type: application/json' -d "$body")
  curl "${a[@]}" "$@" "$base$p"
}
expect() { # <want> <base> <who> <method> <path> [json] [extra...]
  local want="$1"; shift
  local got; got="$(req "$@")"
  [ "$got" = "$want" ] || fail "$3 $4 as $2: got $got, want $want"
}
jget() { python3 -c 'import json,sys; d=json.load(open(sys.argv[2])); v=d
for k in sys.argv[1].split("."): v=v[int(k)] if k.isdigit() else v.get(k)
print("" if v is None else v)' "$1" "$DD/body"; }
node_status() {
  req "$CP" token GET /v1/nodes >/dev/null
  python3 -c 'import json,sys
for n in json.load(open(sys.argv[1]))["nodes"]:
    if n["node_id"]==sys.argv[2]: print(n["status"]+"/"+n["drain_state"])' "$DD/body" "$1"
}
wait_for() { local n="$1"; shift; for _ in $(seq 1 "$n"); do "$@" && return 0; sleep 1; done; return 1; }
healthy() { curl -s -m2 "$1/healthz" | grep -q ok; }
w1_is() { [ "$(node_status w1)" = "$1" ]; }

start() { # <name> <port> [env...]
  local name="$1" port="$2"; shift 2
  mkdir -p "$DD/$name/log" "$DD/$name/library"
  env SANDBOXD_ADDR="127.0.0.1:$port" SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD/$name" \
    SANDBOXD_LOG_DIR="$DD/$name/log" SANDBOXD_NETWORK="$NET" SANDBOXD_USERNS=host \
    SANDBOXD_LIBRARY_DIR="$DD/$name/library" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
    SANDBOXD_DB="$DD/$name/sandboxd.db" SANDBOXD_API_AUTH_DISABLED=false \
    SANDBOXD_SANDBOX_NOFILE="${SANDBOXD_SANDBOX_NOFILE:-65536}" "$@" \
    "$BIN" >"$DD/$name/sandboxd.log" 2>&1 &
}

docker network create "$NET" >/dev/null || fail "docker network create"

log "boot control plane + executor worker"
start cp "$CP_PORT" SANDBOXD_API_TOKENS="default=$TOKEN" SANDBOXD_BOOTSTRAP_ADMIN="admin:$ADMIN_PW" \
  SANDBOXD_WORKER_TOKEN="$WTOKEN" SANDBOXD_NODE_LEASE_SECONDS=8 SANDBOXD_CAPACITY_MEMORY_MB=1024
CP_PID=$!
wait_for 60 healthy "$CP" || fail "control plane never became healthy"
start w1 "$W_PORT" SANDBOXD_CONTROL_PLANE_URL="$CP" SANDBOXD_WORKER_TOKEN="$WTOKEN" \
  SANDBOXD_NODE_ID=w1 SANDBOXD_NODE_ADDRESS="$WK" SANDBOXD_CAPACITY_MEMORY_MB=8192 \
  SANDBOXD_HEARTBEAT_INTERVAL_SECONDS=2
W_PID=$!
wait_for 60 healthy "$WK" || fail "worker never became healthy"
wait_for 30 w1_is online/active || fail "w1 never joined: $(cat "$DD/body")"
log "fleet up ✓"

# 1. The worker's API only accepts the control plane's node credential.
log "worker API is closed"
expect 401 "$WK" token GET /v1/sandboxes
expect 401 "$WK" none GET /v1/sandboxes
expect 200 "$WK" node GET /v1/sandboxes
[ "$(stat -c %a "$DD/w1/state/node-api-token")" = "600" ] || fail "node token file is not 0600"
log "worker API ✓"

# 2. Create through the control plane lands on the worker.
log "create -> worker"
expect 201 "$CP" token POST /v1/sandboxes '{"ports":[3000]}'
SID="$(jget id)"; IDS+=("$SID")
[ "$(jget node)" = "w1" ] || fail "sandbox placed on '$(jget node)', want w1"
docker inspect "s-$SID" >/dev/null 2>&1 || fail "no container for $SID"
expect 401 "$CP" none GET "/v1/sandboxes/$SID"   # the auth check runs before any routing
expect 200 "$CP" token GET "/v1/sandboxes/$SID"
[ "$(jget node)" = "w1" ] && [ "$(jget status)" = "running" ] || fail "get: $(cat "$DD/body")"
log "create ✓ ($SID)"

# 3. Exec, logs and list through the control plane.
log "exec / logs / list"
expect 200 "$CP" token POST "/v1/sandboxes/$SID/exec" '{"cmd":["sh","-c","echo hello-from-w1"]}'
grep -q hello-from-w1 "$DD/body" || fail "exec output: $(cat "$DD/body")"
expect 200 "$CP" token GET "/v1/sandboxes/$SID/logs?tail=10"
expect 200 "$CP" token GET /v1/sandboxes
grep -q "$SID" "$DD/body" || fail "list misses $SID"
expect 200 "$CP" token GET /v1/nodes
log "exec / logs / list ✓"

# 4. The control plane's policy reaches the worker.
log "policy sync"
expect 403 "$CP" token POST "/v1/sandboxes/$SID/keep-alive"
expect 200 "$CP" token PUT /v1/policy/lifecycle '{"allow_keep_alive":true,"max_idle_timeout_secs":3600}'
ka_on() { [ "$(req "$CP" token POST "/v1/sandboxes/$SID/keep-alive")" = "200" ]; }
wait_for 15 ka_on || fail "keep-alive still refused after the policy change: $(cat "$DD/body")"
log "policy sync ✓"

# 4b. Network rules and live activity of a remote sandbox go through the CP.
log "remote network rules"
expect 403 "$CP" token PUT "/v1/sandboxes/$SID/network/egress" '{"mode":"open"}'
expect 200 "$CP" token PUT /v1/policy/egress '{"mode":"permissive","allow":[]}'
put_open() { [ "$(req "$CP" token PUT "/v1/sandboxes/$SID/network/egress" '{"mode":"open","deny":["tracker.example"]}')" = "200" ]; }
wait_for 15 put_open || fail "open mode still refused after the policy change: $(cat "$DD/body")"
expect 200 "$CP" token PUT "/v1/sandboxes/$SID/network/egress" '{"allow":["github.com"]}'
grep -q '"mode":"open"' "$DD/body" && grep -q 'tracker.example' "$DD/body" || fail "egress merge on the worker lost fields: $(cat "$DD/body")"
expect 200 "$CP" token GET "/v1/sandboxes/$SID/network/activity"
grep -q '"rules"' "$DD/body" || fail "no activity from the worker: $(cat "$DD/body")"
expect 200 "$CP" token PUT /v1/policy/egress '{"mode":"strict","allow":[]}'
log "remote network rules ✓"

# 5. Deleting the keep-alive sandbox needs an admin's approval (on the CP).
log "approval for a remote delete"
expect 200 "$CP" token PUT /v1/policy/approvals '["sandbox.delete_keep_alive"]'
expect 202 "$CP" token DELETE "/v1/sandboxes/$SID"
AID="$(jget approval_id)"
docker inspect "s-$SID" >/dev/null 2>&1 || fail "deleted before approval"
expect 204 "$CP" jar POST /v1/auth/login "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}"
expect 200 "$CP" jar POST "/v1/approvals/$AID/approve" '{"reason":"e2e"}'
expect 204 "$CP" token DELETE "/v1/sandboxes/$SID" "" -H "X-Sandboxd-Approval: $AID"
docker inspect "s-$SID" >/dev/null 2>&1 && fail "container still exists after the approved delete"
IDS=()
expect 404 "$CP" token GET "/v1/sandboxes/$SID"
expect 200 "$CP" token GET "/v1/audit?limit=200"
grep -q '"action":"sandbox.forward"' "$DD/body" || fail "no sandbox.forward audit row"
log "approval ✓"

# 6. A drained worker gets nothing new; the node can then be removed.
log "drain worker"
expect 200 "$CP" token PUT /v1/policy/approvals '[]'
expect 200 "$CP" token POST /v1/nodes/w1/drain
expect 201 "$CP" token POST /v1/sandboxes '{}'
LID="$(jget id)"; IDS+=("$LID")
[ "$(jget node)" = "local" ] || fail "drained worker still got a sandbox: $(cat "$DD/body")"
expect 204 "$CP" token DELETE "/v1/sandboxes/$LID"
IDS=()
expect 204 "$CP" token DELETE /v1/nodes/w1
log "drain ✓"

echo "✅ FLEET EXEC E2E PASSED"
