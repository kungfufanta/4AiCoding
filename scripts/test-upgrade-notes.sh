#!/usr/bin/env bash
#
# Exercises upgrade.sh's release-notes path without network or Docker: a curl
# stub on PATH serves canned GitHub API responses, and the script runs inside a
# throwaway git checkout. Covers --check (breaking: yes/no), the non-interactive
# apply path (prints, never prompts), and the terminal prompt (via script(1)).
#
#   scripts/test-upgrade-notes.sh

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
fail() {
  printf '  FAIL: %s\n' "$*" >&2
  # On failure, re-run the last invocation traced so CI logs show the exact line.
  if [ -f "$T/last-args" ]; then
    printf '  --- trace of: upgrade.sh %s ---\n' "$(cat "$T/last-args")" >&2
    # shellcheck disable=SC2046
    ( cd "$T/repo" && bash -x ./upgrade.sh $(cat "$T/last-args") </dev/null 2>&1 | tail -40 ) >&2 || true
  fi
  exit 1
}
pass=0

# ── fake checkout + curl stub ────────────────────────────────────────
mkdir -p "$T/repo/control-plane" "$T/bin" "$T/data"
cp "$ROOT/upgrade.sh" "$T/repo/"
touch "$T/repo/docker-compose.yml"
( cd "$T/repo" && git init -q && git -c user.name=t -c user.email=t@t commit -q --allow-empty -m init \
    && git tag v0.1.0 )
cat > "$T/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Serve $STUB_DIR/<last path element>.json for GitHub API URLs; fail otherwise.
url=""; for a in "$@"; do case "$a" in http*) url="$a";; esac; done
case "$url" in
  *"/releases/latest") cat "$STUB_DIR/latest.json" ;;
  *"/releases/tags/"*) f="$STUB_DIR/tag-${url##*/}.json"; [ -f "$f" ] && cat "$f" || exit 22 ;;
  *) exit 7 ;;
esac
STUB
chmod +x "$T/bin/curl"
# Docker stub: the notes path must be testable where no daemon exists (macOS CI).
# `docker info` / `docker compose version` succeed; anything else is a no-op.
printf '#!/usr/bin/env bash\nexit 0\n' > "$T/bin/docker"; chmod +x "$T/bin/docker"
export PATH="$T/bin:$PATH" STUB_DIR="$T/stubs" SANDBOXD_DATA_DIR="$T/data"
mkdir -p "$STUB_DIR"

notes_breaking='## What'"'"'s Changed\n* Faster builds by @x in https://github.com/tastyeffectco/sandboxd/pull/1\n\n## Breaking changes\n* SANDBOXD_PORT was renamed to SANDBOXD_API_BIND\n\n## Other\n* misc'
notes_plain='## What'"'"'s Changed\n* Just fixes'
release_json() { printf '{"tag_name":"%s","html_url":"https://github.com/tastyeffectco/sandboxd/releases/tag/%s","published_at":"2026-08-01T00:00:00Z","body":"%s"}\n' "$1" "$1" "$2"; }
release_json v0.2.0 "$notes_breaking" > "$STUB_DIR/latest.json"
release_json v0.2.0 "$notes_breaking" > "$STUB_DIR/tag-v0.2.0.json"
release_json v0.3.0 "$notes_plain"    > "$STUB_DIR/tag-v0.3.0.json"

run() { printf '%s' "$*" > "$T/last-args"; ( cd "$T/repo" && bash ./upgrade.sh "$@" 2>&1 ) || true; }

# ── --check ──────────────────────────────────────────────────────────
out="$(run --check </dev/null)"
grep -q 'an update may be available: v0.2.0' <<<"$out" || fail "--check: update line missing:\n$out"
grep -q 'breaking changes: yes' <<<"$out" || fail "--check: expected 'breaking changes: yes':\n$out"
grep -q 'What.s new' <<<"$out" && fail "--check must not print the notes body"
pass=$((pass+1))

release_json v0.3.0 "$notes_plain" > "$STUB_DIR/latest.json"
out="$(run --check </dev/null)"
grep -q 'breaking changes: no' <<<"$out" || fail "--check: expected 'breaking changes: no':\n$out"
pass=$((pass+1))

# Offline: --check output unchanged, no "breaking changes" line.
out="$(STUB_DIR=/nonexistent run --check </dev/null)"
grep -q "couldn't reach GitHub" <<<"$out" || fail "offline --check:\n$out"
grep -q 'breaking changes' <<<"$out" && fail "offline --check printed breaking status"
pass=$((pass+1))

# ── apply, non-TTY (the console-driven upgrader): print, never prompt ──
# The run stops at "git fetch origin" (no remote) — after the notes step.
out="$(run v0.2.0 </dev/null)"
grep -q 'BREAKING CHANGES in v0.2.0' <<<"$out" || fail "non-tty: breaking header missing:\n$out"
grep -q 'SANDBOXD_PORT was renamed' <<<"$out" || fail "non-tty: breaking body missing:\n$out"
grep -q "What's new in v0.2.0" <<<"$out" || fail "non-tty: what's new missing:\n$out"
grep -q 'Faster builds' <<<"$out" || fail "non-tty: notes body missing:\n$out"
grep -q 'Continue with the upgrade' <<<"$out" && fail "non-tty must never prompt:\n$out"
grep -q '1/4 · Backing up' <<<"$out" || fail "non-tty: did not proceed past the notes:\n$out"
# Breaking section is printed BEFORE the what's-new section.
[ "$(grep -n 'BREAKING CHANGES' <<<"$out" | cut -d: -f1 | head -1)" -lt "$(grep -n "What's new" <<<"$out" | cut -d: -f1 | head -1)" ] \
  || fail "breaking section must come first"
pass=$((pass+1))

# No breaking section: notes only.
out="$(run v0.3.0 </dev/null)"
grep -q 'BREAKING' <<<"$out" && fail "v0.3.0 has no breaking section:\n$out"
grep -q 'Just fixes' <<<"$out" || fail "v0.3.0 notes missing:\n$out"
pass=$((pass+1))

# Unknown tag / offline: silent, still proceeds.
out="$(run v9.9.9 </dev/null)"
grep -q "What's new" <<<"$out" && fail "unknown tag must skip notes silently:\n$out"
grep -q '1/4 · Backing up' <<<"$out" || fail "unknown tag: did not proceed:\n$out"
pass=$((pass+1))

# ── apply, TTY: prompt on breaking changes; --yes skips it ───────────
# script(1) flags differ between util-linux and BSD; only the former is used.
if script --version >/dev/null 2>&1; then
  tty_run() { ( cd "$T/repo" && script -qec "bash ./upgrade.sh $1" /dev/null ) 2>&1 || true; }
  out="$(printf 'n\n' | tty_run v0.2.0)"
  grep -q 'Continue with the upgrade' <<<"$out" || fail "tty: prompt missing:\n$out"
  grep -q 'upgrade cancelled' <<<"$out" || fail "tty: 'n' should cancel:\n$out"
  grep -q '1/4 · Backing up' <<<"$out" && fail "tty: cancelled run must not proceed"
  pass=$((pass+1))

  out="$(printf 'y\n' | tty_run v0.2.0)"
  grep -q '1/4 · Backing up' <<<"$out" || fail "tty: 'y' should proceed:\n$out"
  pass=$((pass+1))

  out="$(printf 'n\n' | tty_run 'v0.2.0 --yes')"
  grep -q 'Continue with the upgrade' <<<"$out" && fail "tty --yes must not prompt:\n$out"
  grep -q 'BREAKING CHANGES' <<<"$out" || fail "tty --yes still prints breaking changes:\n$out"
  grep -q '1/4 · Backing up' <<<"$out" || fail "tty --yes should proceed:\n$out"
  pass=$((pass+1))

  out="$(printf 'n\n' | tty_run v0.3.0)"
  grep -q 'Continue with the upgrade' <<<"$out" && fail "tty: no prompt without breaking changes:\n$out"
  pass=$((pass+1))
else
  echo "  (script(1) not found — skipping the TTY prompt cases)"
fi

echo "  ok: $pass upgrade.sh notes cases passed"
