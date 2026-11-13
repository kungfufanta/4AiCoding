#!/usr/bin/env bash
# a35-acceptance.sh — the A3.5 production acceptance (plan §11 A3.5
# "Xong khi"): hand the task to the agent on the REAL portal —
# real WebHub connection, real sandbox — and capture the evidence
# (run JSON + workspace listing) for the PR.
#
# Credentials ride the environment (ADMIN_USERNAME/ADMIN_PASSWORD,
# wired from the repository secrets inside the acceptance workflow);
# the script prints nothing but ids, statuses and the captured files.
#
# Flow (BFF, session + CSRF double submit):
#   login → org (first org or a fresh "A3.5 nghiệm thu") → sandbox
#   (ubuntu:24.04) → chat + attach → agent run → poll → terminal →
#   exec `ls workspace` as the files-API cross-check.
set -euo pipefail

BASE="${BASE_URL:-https://chat.devhub.io.vn}"
ADMIN_USERNAME="${ADMIN_USERNAME:-admin}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:?ADMIN_PASSWORD is required}"
PROMPT="${ACCEPT_PROMPT:-tạo file index.html hiển thị Hello rồi kiểm tra bằng curl}"
OUT="${ACCEPT_OUT:-$PWD/a35-acceptance}"
mkdir -p "$OUT"

say() { printf '[a35] %s\n' "$*"; }
fail() { printf '[a35] FAIL: %s\n' "$*" >&2; exit 1; }

jar="$OUT/cookies.txt"
CSRF=""
ensure_csrf() {
  [ -n "$CSRF" ] || CSRF=$(awk '$6=="portal_csrf" {print $7}' "$jar" | tail -n1)
  [ -n "$CSRF" ] || { printf '[a35] FAIL: no portal_csrf cookie — login did not land\n' >&2; exit 1; }
}

api() { # api METHOD PATH [BODY]
  ensure_csrf
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -b "$jar" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json'
              -o "$OUT/last-response.json" -w '%{http_code}' "$BASE$path" -X "$method")
  [ -n "$body" ] && args+=(-d "$body")
  LAST_CODE=$(curl "${args[@]}")
  LAST_BODY="$OUT/last-response.json"
}

require_code() { # require_code 201 <what>
  [ "$LAST_CODE" = "$1" ] || fail "$2 → HTTP $LAST_CODE (want $1): $(head -c 400 "$LAST_BODY")"
}

field() { jq -r "$1" "$LAST_BODY"; }

# 1. Password login (bootstrap admin; disabled → the deployment has
#    no PORTAL_ADMIN_PASSWORD and the workflow says so).
code=$(curl -s -o "$OUT/login.json" -w '%{http_code}' -c "$jar" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}" \
  "$BASE/auth/login") || fail "login request itself failed"
[ "$code" = "200" ] || fail "login → HTTP $code ($OUT/login.json)"
say "login ok (admin)"
ensure_csrf

# 2. Org: reuse the first org of the admin, else create one.
api GET /api/me
require_code 200 "GET /api/me"
org_id=$(field '.orgs[0].id // empty')
org_slug=$(field '.orgs[0].slug // empty')
if [ -z "$org_id" ] || [ "$org_id" = "null" ]; then
  say "no org on the admin — creating one"
  api POST /api/orgs '{"name":"A3.5 nghiệm thu"}'
  require_code 201 "POST /api/orgs"
  org_id=$(field '.id'); org_slug=$(field '.slug')
fi
say "org: $org_slug ($org_id)"

# 3. Sandbox (real sandboxd behind the portal).
api POST "/api/orgs/$org_id/sandboxes" '{"image":"ubuntu:24.04"}'
require_code 201 "POST sandboxes"
sbx=$(field '.id')
say "sandbox: $sbx"

# 4. Chat + attach (attach lands the chat in Agent mode).
api POST "/api/orgs/$org_id/chats" '{}'
require_code 201 "POST chats"
chat=$(field '.id')
api PATCH "/api/orgs/$org_id/chats/$chat" "{\"sandbox_id\":\"$sbx\"}"
require_code 200 "PATCH chat attach"
say "chat: $chat (sandbox attached)"

# 5. Hand the task to the agent.
api POST "/api/orgs/$org_id/chats/$chat/agent/runs" "{\"prompt\":\"$PROMPT\"}"
require_code 201 "POST agent run"
run=$(field '.id')
say "run: $run"

# 6. Poll to a terminal state (the task drives a real LLM + real
#    sandbox; the plan's run timeout is minutes, poll up to 12).
deadline=$(( $(date +%s) + 720 ))
status=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  sleep 5
  api GET "/api/orgs/$org_id/agent/runs/$run"
  status=$(field '.status')
  case "$status" in
    succeeded|failed|cancelled) break ;;
  esac
done
[ "$status" = "succeeded" ] || fail "run ended $status (want succeeded): $(head -c 400 "$LAST_BODY")"
cp "$LAST_BODY" "$OUT/run-final.json"
say "run finished: $status"

# 7. The evidence the plan asks to paste: files_changed must contain
#    A index.html.
jq '{status, error_code, files: .result.files_changed, steps: .result.steps, tokens: (.result.input_tokens + .result.output_tokens)}' \
  "$OUT/run-final.json" > "$OUT/run-summary.json"
jq -e '.files[]? | select(.path=="index.html" and .status=="A")' "$OUT/run-final.json" >/dev/null \
  || fail "files_changed has no {A, index.html}: $(jq -c '.result.files_changed' "$OUT/run-final.json")"
say "files_changed contains A index.html ✓"

# 8. Cross-check through the exec console: the file is really there.
api POST "/api/orgs/$org_id/sandboxes/$sbx/exec" '{"cmd":"cat workspace/index.html"}'
require_code 200 "exec cat index.html"
cp "$LAST_BODY" "$OUT/exec-index.html.json"
say "workspace/index.html content captured ($(jq -r '.stdout' "$OUT/exec-index.html.json" | head -c 120 | tr '\n' ' ')…)"

# 9. Everything the PR needs.
jq -n --arg base "$BASE" --arg org "$org_slug" --arg chat "$chat" --arg sbx "$sbx" --arg run "$run" \
  '{base: $base, org: $org, chat: $chat, sandbox: $sbx, run: $run}' > "$OUT/ids.json"
say "evidence in $OUT: ids.json run-final.json run-summary.json exec-index.html.json"
