#!/usr/bin/env bash
# End-to-end test of the access-control layer against a real sandboxd and
# Docker daemon, with auth ON (scripts/e2e.sh runs with auth off and covers
# the lifecycle). Covers: no credential -> 401, bootstrap admin login and
# session cookie, CSRF on cookie requests, user roles (viewer cannot write),
# API keys with role / sandbox binding / scopes, keep-alive gated by policy,
# the approvals flow (202 -> admin approves -> single-use retry), and the
# audit hash chain.
#
# Inputs (env): SANDBOXD_IMAGE, SANDBOXD_BIN (as for scripts/e2e.sh).
#   sudo SANDBOXD_IMAGE=sandboxd-e2e:local SANDBOXD_BIN=/tmp/sandboxd bash scripts/e2e-auth.sh
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "❌ scripts/e2e-auth.sh must run as root (sudo), like sandboxd in production" >&2
  exit 2
fi

IMG="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
BIN="${SANDBOXD_BIN:?set SANDBOXD_BIN}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
DD="$(mktemp -d)"
NET="sbxauth-$$"
PORT="${SANDBOXD_E2E_AUTH_PORT:-9291}"
API="http://127.0.0.1:$PORT"
TOKEN="sk_e2e_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
ADMIN_PW="e2e-admin-pass-1"
SBX_PID=""
declare -a IDS=()

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  [ -s "$DD/body" ] && { echo "---- last response body ----"; cat "$DD/body"; echo; }
  echo "---- sandboxd log (tail) ----"; tail -40 "$DD/sandboxd.log" 2>/dev/null || true
  exit 1
}
cleanup() {
  for id in "${IDS[@]:-}"; do
    [ -n "$id" ] && curl -s -m30 -o /dev/null -H "Authorization: Bearer $TOKEN" -XPOST "$API/sandbox/$id/purge" || true
  done
  [ -n "$SBX_PID" ] && kill "$SBX_PID" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=sandboxd.managed=true" --filter "network=$NET" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$DD" 2>/dev/null || true
}
trap cleanup EXIT

# req <who> <method> <path> [json] [extra curl args...] -> prints HTTP code,
# body in $DD/body. who: none | token | key:<k> | jar:<name> (cookie + CSRF) |
# jar-nocsrf:<name>.
req() {
  local who="$1" m="$2" p="$3" body="${4:-}"
  shift 4 2>/dev/null || shift $#
  local -a a=(-s -m60 -o "$DD/body" -w '%{http_code}' -X "$m")
  case "$who" in
    token) a+=(-H "Authorization: Bearer $TOKEN") ;;
    key:*) a+=(-H "Authorization: Bearer ${who#key:}") ;;
    jar:*) a+=(-b "$DD/${who#jar:}.jar" -c "$DD/${who#jar:}.jar" -H "X-Sandboxd-CSRF: 1") ;;
    jar-nocsrf:*) a+=(-b "$DD/${who#jar-nocsrf:}.jar") ;;
  esac
  [ -n "$body" ] && a+=(-H 'content-type: application/json' -d "$body")
  curl "${a[@]}" "$@" "$API$p"
}
expect() { # <want-code> <who> <method> <path> [json] [extra...]
  local want="$1"
  shift
  local got
  got="$(req "$@")"
  [ "$got" = "$want" ] || fail "$2 $3 as $1: got $got, want $want"
}
jget() { python3 -c 'import json,sys; d=json.load(open(sys.argv[2])); v=d
for k in sys.argv[1].split("."): v=v[int(k)] if k.isdigit() else v.get(k)
print("" if v is None else v)' "$1" "$DD/body"; }

mkdir -p "$DD/log" "$DD/library"
docker network create "$NET" >/dev/null || fail "docker network create"
SANDBOXD_ADDR="127.0.0.1:$PORT" SANDBOXD_IMAGE="$IMG" SANDBOXD_DATA_DIR="$DD" \
  SANDBOXD_LOG_DIR="$DD/log" SANDBOXD_NETWORK="$NET" SANDBOXD_USERNS=host \
  SANDBOXD_LIBRARY_DIR="$DD/library" SANDBOXD_MIGRATIONS="$HERE/control-plane/migrations" \
  SANDBOXD_DB="$DD/sandboxd.db" SANDBOXD_API_AUTH_DISABLED=false \
  SANDBOXD_API_TOKENS="default=$TOKEN" SANDBOXD_BOOTSTRAP_ADMIN="admin:$ADMIN_PW" \
  "$BIN" >"$DD/sandboxd.log" 2>&1 &
SBX_PID=$!
for _ in $(seq 1 60); do curl -s -m2 "$API/healthz" 2>/dev/null | grep -q ok && break; sleep 0.5; done
curl -s -m3 "$API/healthz" | grep -q ok || fail "sandboxd never became healthy"
log "sandboxd healthy (auth on)"

# 1. No credential, bad credential.
log "unauthenticated access"
expect 401 none GET /v1/sandboxes
expect 401 key:sk_wrong GET /v1/sandboxes
expect 200 none GET /v1/auth/status
grep -q '"setup_needed":false' "$DD/body" || fail "bootstrap admin was not created"
expect 409 none POST /v1/auth/setup '{"username":"evil","password":"evil-pass-1"}'
log "unauthenticated ✓"

# 2. Login (bootstrap admin), session cookie, CSRF.
log "login + CSRF"
expect 401 jar:admin POST /v1/auth/login '{"username":"admin","password":"wrong-password"}'
expect 204 jar:admin POST /v1/auth/login "{\"username\":\"admin\",\"password\":\"$ADMIN_PW\"}"
grep -q sandboxd_session "$DD/admin.jar" 2>/dev/null || grep -qi session "$DD/admin.jar" || fail "no session cookie set"
expect 200 jar:admin GET /v1/auth/me
[ "$(jget role)" = "admin" ] && [ "$(jget kind)" = "user" ] || fail "me: $(cat "$DD/body")"
expect 403 jar-nocsrf:admin POST /v1/sandboxes '{}'
grep -q "X-Sandboxd-CSRF" "$DD/body" || fail "CSRF denial should name the header"
log "login + CSRF ✓"

# 3. Roles: a viewer can read but not write.
log "roles"
expect 201 jar:admin POST /v1/users '{"username":"vic","password":"viewer-pass-1","role":"viewer"}'
VIC_ID="$(jget id)"
expect 409 token POST /v1/users '{"username":"VIC","password":"viewer-pass-1","role":"viewer"}'
expect 204 jar:vic POST /v1/auth/login '{"username":"vic","password":"viewer-pass-1"}'
expect 200 jar:vic GET /v1/sandboxes
expect 403 jar:vic POST /v1/sandboxes '{}'
expect 403 jar:vic GET /v1/users
expect 403 jar:vic PUT /v1/policy/lifecycle '{"allow_keep_alive":true}'
# Disabling the user kills their session at once.
expect 200 jar:admin PATCH "/v1/users/$VIC_ID" '{"disabled":true}'
expect 401 jar:vic GET /v1/sandboxes
log "roles ✓"

# 4. Create a sandbox as the admin (cookie + CSRF).
log "create as admin"
expect 201 jar:admin POST /v1/sandboxes '{"ports":[3000]}' || true
SID="$(jget id)"
[ -n "$SID" ] || fail "create returned no id"
IDS+=("$SID")

# 5. API keys: role cap, sandbox binding, scopes, revoke.
log "api keys"
expect 403 token POST /v1/api-keys '{"name":"by-token"}'  # keys are minted by a console user
expect 201 jar:admin POST /v1/api-keys "{\"name\":\"bound\",\"role\":\"operator\",\"sandbox_id\":\"$SID\",\"ttl_seconds\":600}"
BOUND="$(jget key)"; BOUND_ID="$(jget id)"
[ -n "$(jget expires_at)" ] || fail "ttl not applied"
expect 200 "key:$BOUND" POST "/v1/sandboxes/$SID/exec" '{"cmd":["true"]}'
expect 403 "key:$BOUND" GET /v1/sandboxes
expect 403 "key:$BOUND" PUT /v1/policy/lifecycle '{"allow_keep_alive":true}'
expect 201 jar:admin POST /v1/api-keys '{"name":"reader","role":"viewer","scopes":["sandboxes:read"]}'
READER="$(jget key)"
expect 200 "key:$READER" GET "/v1/sandboxes/$SID"
expect 403 "key:$READER" POST "/v1/sandboxes/$SID/exec" '{"cmd":["true"]}'
# 5b. Scope sandboxes:files (AGENT_PLAN A0) gates the workspace files API.
expect 201 jar:admin POST /v1/api-keys '{"name":"execer","role":"operator","scopes":["sandboxes:exec"]}'
EXECER="$(jget key)"
expect 200 "key:$EXECER" POST "/v1/sandboxes/$SID/exec" '{"cmd":["true"]}'
expect 403 "key:$EXECER" GET "/v1/sandboxes/$SID/files"
expect 403 "key:$EXECER" PUT "/v1/sandboxes/$SID/files/content?path=x.txt"
expect 201 jar:admin POST /v1/api-keys '{"name":"nocap","role":"operator","scopes":[]}'
NOCAP="$(jget key)"
# A key with no scopes may do whatever its role allows; the PUT also
# materialises the workspace dir so the listings below have something to show.
expect 200 "key:$NOCAP" PUT "/v1/sandboxes/$SID/files/content?path=scope-e2e.txt" 'hello'
expect 200 "key:$NOCAP" GET "/v1/sandboxes/$SID/files"
expect 200 "key:$NOCAP" POST "/v1/sandboxes/$SID/exec" '{"cmd":["true"]}'
expect 201 jar:admin POST /v1/api-keys '{"name":"filer","role":"operator","scopes":["sandboxes:files"]}'
FILER="$(jget key)"
expect 200 "key:$FILER" GET "/v1/sandboxes/$SID/files"
expect 200 "key:$FILER" GET "/v1/sandboxes/$SID/files/content?path=scope-e2e.txt"
expect 403 "key:$FILER" POST "/v1/sandboxes/$SID/exec" '{"cmd":["true"]}'
expect 204 "key:$FILER" DELETE "/v1/sandboxes/$SID/files?path=scope-e2e.txt"
log "files scope ✓"

expect 204 jar:admin DELETE "/v1/api-keys/$BOUND_ID"
expect 401 "key:$BOUND" GET "/v1/sandboxes/$SID"
log "api keys ✓"

# 6. Keep-alive is denied until the policy allows it.
log "keep-alive policy"
expect 403 token POST "/v1/sandboxes/$SID/keep-alive"
grep -q policy_denied "$DD/body" || fail "keep-alive denial should be policy_denied"
expect 200 token PUT /v1/policy/lifecycle '{"allow_keep_alive":true,"max_idle_timeout_secs":3600}'
expect 200 token POST "/v1/sandboxes/$SID/keep-alive"
[ "$(jget keep_alive)" = "True" ] || fail "keep-alive not on: $(cat "$DD/body")"
expect 200 token PATCH "/v1/sandboxes/$SID/lifecycle" '{"idle_timeout_secs":999999}'
[ "$(jget idle_timeout_secs)" = "3600" ] || fail "idle timeout not clamped: $(cat "$DD/body")"
log "keep-alive ✓"

# 7. Approvals: deleting a keep-alive sandbox needs an admin's approval.
log "approvals"
expect 200 token PUT /v1/policy/approvals '["sandbox.delete_keep_alive"]'
expect 202 token DELETE "/v1/sandboxes/$SID"
AID="$(jget approval_id)"
[ -n "$AID" ] || fail "no approval id"
docker inspect "s-$SID" >/dev/null 2>&1 || fail "sandbox deleted before approval"
expect 403 token DELETE "/v1/sandboxes/$SID" "" -H "X-Sandboxd-Approval: $AID"   # still pending
expect 403 token POST "/v1/approvals/$AID/approve" '{}'                        # API keys never approve
expect 200 jar:admin POST "/v1/approvals/$AID/approve" '{"reason":"e2e"}'
expect 403 token DELETE "/v1/sandboxes/$SID" "" -H "X-Sandboxd-Approval: bogus"
expect 204 token DELETE "/v1/sandboxes/$SID" "" -H "X-Sandboxd-Approval: $AID"
docker inspect "s-$SID" >/dev/null 2>&1 && fail "container still exists after approved delete"
IDS=()
expect 200 token GET "/v1/approvals/$AID"
[ "$(jget status)" = "used" ] || fail "approval not consumed: $(cat "$DD/body")"
log "approvals ✓"

# 8. Audit: actions are recorded and the hash chain verifies.
log "audit"
expect 403 "key:$READER" GET /v1/audit
expect 200 token GET "/v1/audit?limit=200"
for a in auth.login auth.login_failed sandbox.create approval.requested approval.approved approval.used policy.update; do
  grep -q "\"action\":\"$a\"" "$DD/body" || fail "audit is missing $a"
done
expect 200 token GET /v1/audit/verify
[ "$(jget ok)" = "True" ] || fail "audit chain broken: $(cat "$DD/body")"
log "audit ✓"

# 9. Logout revokes the session.
log "logout"
expect 204 jar:admin POST /v1/auth/logout
expect 401 jar:admin GET /v1/sandboxes
log "logout ✓"

echo "✅ AUTH E2E PASSED"
