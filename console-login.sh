#!/usr/bin/env bash
# Show the sandboxd web console URL + the API bootstrap key. Run it anytime:
#     ./console-login.sh
# Forgot the console password? Clear it so the console asks you to create a new one:
#     ./console-login.sh --reset-password
set -eu
cd "$(dirname "$0")"

usage() {
  cat <<'USAGE'
Usage: ./console-login.sh [--reset-password] [--help]

  (no args)          show the console URL and the API key
  --reset-password   clear the console password (uses the API key); the console
                     asks you to create a new one and every session is signed out
  --help             show this help
USAGE
}

MODE="show"
case "${1:-}" in
  "") ;;
  --reset-password) MODE="reset" ;;
  -h|--help) usage; exit 0 ;;
  *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
esac

if [ ! -f .env ]; then
  echo "No .env here — run ./install.sh first." >&2
  exit 1
fi
# shellcheck disable=SC1091
set -a; . ./.env; set +a

DOMAIN="${PREVIEW_DOMAIN:-localhost}"
PORT="${HTTP_PORT:-80}"
SUFFIX=""; [ "$PORT" != "80" ] && SUFFIX=":$PORT"
SCHEME="http"; [ "${PREVIEW_URL_SCHEME:-}" = "https" ] && SCHEME="https"
URL="${SCHEME}://${CONSOLE_HOST:-console.${DOMAIN}}${SUFFIX}"

# Prefer the stashed value; fall back to parsing SANDBOXD_API_TOKENS.
API_KEY=""
if [ -f .console-login ]; then
  API_KEY="$(sed -n 's/^api_key=//p' .console-login | head -1)"
fi
if [ -z "$API_KEY" ]; then
  API_KEY="$(printf '%s' "${SANDBOXD_API_TOKENS:-}" | tr ',' '\n' | sed -n 's/^default=//p' | head -1)"
fi

if [ "$MODE" = "reset" ]; then
  if [ -z "$API_KEY" ]; then
    echo "No API key found (.console-login or SANDBOXD_API_TOKENS in .env) — it is needed to reset the password." >&2
    exit 1
  fi
  API="http://${SANDBOXD_API_BIND:-127.0.0.1:9090}"
  if out="$(curl -fsS -X DELETE "${API}/v1/auth/password" -H "Authorization: Bearer $API_KEY" 2>&1)"; then
    echo
    echo "  Console password cleared. Open ${URL} and create a new one."
    echo "  (All console sessions were signed out.)"
    echo
    exit 0
  fi
  echo "Password reset failed: ${out}" >&2
  echo "The API must be reachable on SANDBOXD_API_BIND (${API}) and the API key must be valid." >&2
  exit 1
fi

echo
echo "  ┌─ sandboxd web console ─────────────────────────────"
printf "  │  Open       %s\n" "$URL"
printf "  │  Login      create your password on first visit\n"
printf "  │             (change it later in Settings → Security)\n"
if [ -n "$API_KEY" ]; then
  printf "  │\n"
  printf "  │  API key    %s\n" "$API_KEY"
  printf "  │             Authorization: Bearer <key>   (for scripts / the engine)\n"
fi
printf "  │\n"
printf "  │  Forgot the password?  ./console-login.sh --reset-password\n"
echo "  └────────────────────────────────────────────────────"
echo
echo "  Rotate the API key: edit SANDBOXD_API_TOKENS in .env (SIGHUP reloads),"
echo "  or mint one in the console → Settings → Security."
echo
