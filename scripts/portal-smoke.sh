#!/usr/bin/env bash
# Smoke-check a live Portal: landing → SSO redirect → API auth gate.
# Usage: scripts/portal-smoke.sh https://chat.devhub.io.vn
# Needs no secret. Exit 1 if any hard check fails; warnings don't fail.
# When $GITHUB_STEP_SUMMARY is set, a Markdown table is appended there.
set -euo pipefail

base="${1:-https://chat.devhub.io.vn}"
base="${base%/}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
rows=()

record() { # status name detail
  rows+=("| $1 | $2 | $3 |")
  printf '%-5s %-28s %s\n' "$1" "$2" "$3"
  if [ "$1" = "FAIL" ]; then fail=1; fi
}

get() { # path outfile -> prints http code
  curl -sS -m 20 -o "$2" -w '%{http_code}' "$base$1" || echo 000
}

code="$(get / "$tmp/root")"
if [ "$code" = 200 ] && grep -q '<div id="root"' "$tmp/root"; then
  record OK "GET /" "200, SPA root present"
else
  record FAIL "GET /" "http $code or no <div id=\"root\">"
fi

for p in /healthz /readyz; do
  code="$(get "$p" "$tmp/h")"
  if [ "$code" = 200 ]; then record OK "GET $p" "200"; else record FAIL "GET $p" "http $code"; fi
done

code="$(get /auth/methods "$tmp/m")"
if [ "$code" != 200 ]; then
  record FAIL "GET /auth/methods" "http $code"
elif jq -e '.sso == true' "$tmp/m" >/dev/null 2>&1; then
  record OK "GET /auth/methods" "$(jq -c . "$tmp/m")"
else
  record WARN "GET /auth/methods" "sso is not true: $(head -c 200 "$tmp/m")"
fi

loc="$(curl -sS -m 20 -o /dev/null -w '%{http_code} %{redirect_url}' "$base/auth/login" || echo '000 ')"
lcode="${loc%% *}"
lurl="${loc#* }"
if [ "$lcode" = 302 ] && [[ "$lurl" == https://auth.devhub.io.vn/* ]]; then
  record OK "GET /auth/login" "302 → auth.devhub.io.vn"
  # Follow the IdP redirects; a stale client_id shows Errors.App.NotFound (PR #76).
  idp_code="$(curl -sS -m 30 -L -o "$tmp/idp" -w '%{http_code}' "$lurl" || echo 000)"
  if grep -q 'Errors.App.NotFound' "$tmp/idp"; then
    record FAIL "Zitadel login page" "Errors.App.NotFound (stale OIDC client_id: run configure-zitadel-oidc)"
  elif [ "$idp_code" = 200 ]; then
    record OK "Zitadel login page" "200"
  else
    record FAIL "Zitadel login page" "http $idp_code"
  fi
else
  record FAIL "GET /auth/login" "want 302 to auth.devhub.io.vn, got $lcode ${lurl:0:80}"
fi

code="$(get /api/me "$tmp/me")"
if [ "$code" = 401 ]; then record OK "GET /api/me" "401 (auth gate)"; else record FAIL "GET /api/me" "want 401, got $code"; fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Portal smoke: $base"
    echo
    echo "| Result | Check | Detail |"
    echo "|---|---|---|"
    printf '%s\n' "${rows[@]}"
  } >> "$GITHUB_STEP_SUMMARY"
fi
exit "$fail"
