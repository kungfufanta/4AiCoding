#!/usr/bin/env bash
# Full-stack preview: build and start the real docker-compose stack (Traefik,
# sandboxd, console) with a throwaway data dir, check the edge routes, then
# drive the console in a browser (scripts/preview/smoke.cjs). Screenshots land
# in $PREVIEW_SHOTS. Used by the PR preview workflow; runs locally too:
#
#   docker build -t sandboxd-e2e:local scripts/e2e
#   sudo SANDBOXD_IMAGE=sandboxd-e2e:local bash scripts/preview.sh
#
# Env: SANDBOXD_IMAGE (sandbox image), PREVIEW_HTTP_PORT (default 18080),
#      PREVIEW_SHOTS (default ./preview-shots), PREVIEW_KEEP=1 to leave it up,
#      PREVIEW_BUILD=0 to use already-built images.
set -uo pipefail

if [ "$(id -u)" != "0" ]; then
  echo "❌ scripts/preview.sh must run as root (sudo): sandboxd deletes uid-1000 workspace files" >&2
  exit 2
fi

HERE="$(cd "$(dirname "$0")/.." && pwd)"
cd "$HERE"
PORT="${PREVIEW_HTTP_PORT:-18080}"
SHOTS="${PREVIEW_SHOTS:-$HERE/preview-shots}"
DD="$(mktemp -d /tmp/sbxpreview.XXXXXX)"
PASS="preview-$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
TOKEN="sk_preview_$(head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export COMPOSE_PROJECT_NAME=sbxpreview
export HTTP_PORT="$PORT"
export PREVIEW_DOMAIN=localhost
export SANDBOXD_IMAGE="${SANDBOXD_IMAGE:?set SANDBOXD_IMAGE}"
export SANDBOXD_DATA_DIR="$DD/data"
export SANDBOXD_API_BIND="127.0.0.1:${PREVIEW_API_PORT:-19090}"
export SANDBOXD_BOOTSTRAP_ADMIN="admin:$PASS"
export SANDBOXD_API_TOKENS="default=$TOKEN"
# traefik/traefik.yml names the sandbox network, so keep the default name.
export SANDBOXD_NETWORK="sandboxd_net"
mkdir -p "$SANDBOXD_DATA_DIR" "$SHOTS"

log() { echo "── $*"; }
fail() {
  echo "❌ FAIL: $*"
  docker compose logs --tail 60 2>/dev/null || true
  exit 1
}
cleanup() {
  if [ "${PREVIEW_KEEP:-}" = "1" ]; then
    echo "stack left running: http://console.localhost:$PORT  (admin / $PASS)"
    return
  fi
  docker ps -aq --filter "label=sandboxd.managed=true" --filter "network=$SANDBOXD_NETWORK" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$DD"
}
trap cleanup EXIT

log "build + start the stack"
if [ "${PREVIEW_BUILD:-1}" = "1" ]; then
  docker compose build || fail "compose build"
fi
docker compose up -d --no-build || fail "compose up"

API="http://$SANDBOXD_API_BIND"
for _ in $(seq 1 90); do curl -s -m2 "$API/healthz" | grep -q ok && break; sleep 1; done
curl -s -m3 "$API/healthz" | grep -q ok || fail "sandboxd never became healthy"
for _ in $(seq 1 60); do
  [ "$(curl -s -m2 -o /dev/null -w '%{http_code}' -H "Host: console.localhost" "http://127.0.0.1:$PORT/")" = "200" ] && break
  sleep 1
done
log "stack healthy"

log "edge routes"
code="$(curl -s -m5 -o /dev/null -w '%{http_code}' -H "Host: console.localhost" "http://127.0.0.1:$PORT/")"
[ "$code" = "200" ] || fail "console via Traefik returned $code"
curl -s -m5 -H "Host: console.localhost" "http://127.0.0.1:$PORT/v1/auth/status" | grep -q '"setup_needed":false' \
  || fail "console /v1 proxy did not reach sandboxd (or bootstrap admin missing)"
code="$(curl -s -m5 -o /dev/null -w '%{http_code}' -H "Host: console.localhost" "http://127.0.0.1:$PORT/v1/sandboxes")"
[ "$code" = "401" ] || fail "console proxy without a session returned $code, want 401"
code="$(curl -s -m5 -o /dev/null -w '%{http_code}' -H "Host: api.preview.localhost" -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/v1/sandboxes")"
[ "$code" = "200" ] || fail "API via Traefik returned $code"
curl -sI -m5 -H "Host: console.localhost" "http://127.0.0.1:$PORT/" | grep -qi "content-security-policy" || fail "console is missing its CSP header"
log "edge routes ✓"

log "egress: the sandbox network has no route out"
docker network inspect -f '{{.Internal}}' "$SANDBOXD_NETWORK" | grep -q true || fail "$SANDBOXD_NETWORK is not internal"
log "egress ✓"

log "browser smoke"
BASE="http://console.localhost:$PORT" ADMIN_USER=admin ADMIN_PASS="$PASS" SHOTS="$SHOTS" \
  node "$HERE/scripts/preview/smoke.cjs" || fail "console smoke"

echo "✅ PREVIEW PASSED (screenshots in $SHOTS)"
