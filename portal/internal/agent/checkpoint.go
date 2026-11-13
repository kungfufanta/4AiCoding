// checkpoint.go — the git checkpoint + files_changed of a run
// (docs/AGENT_PLAN_A3.md §8). Everything runs through sandboxd exec,
// cwd = the project directory, with the run id passed via the exec env
// (never interpolated into the command string — plan §8 "RUN_ID truyền
// qua env"). The checkpoint never touches the user's HEAD, branch or
// staging area: it uses a temporary GIT_INDEX_FILE and writes only a
// ref under refs/4aicoding/checkpoints/<run_id>.
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// checkpointScript is plan §8 verbatim, plus the .git/info/exclude
// maintenance: BRAIN.md (upstream rule: the project-memory file never
// enters a checkpoint tree) and — ONLY for a project without its own
// .gitignore — the default build-output excludes. Without them "git
// add -A" swallows node_modules/, .next/, dist/... into every
// checkpoint: slow (the 60 s exec timeout) and disk-bloating. The
// defaults go to .git/info/exclude — never into the user's
// .gitignore — and the user's own rules always win.
const checkpointScript = `set -euo pipefail
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || git init -q
mkdir -p .git/info
grep -qxF 'BRAIN.md' .git/info/exclude 2>/dev/null || echo BRAIN.md >> .git/info/exclude
if [ ! -e .gitignore ]; then
  while IFS= read -r line; do
    grep -qxF "$line" .git/info/exclude 2>/dev/null || printf '%s\n' "$line" >> .git/info/exclude
  done <<'EXCLUDES'
node_modules/
.next/
.nuxt/
.output/
dist/
build/
out/
.venv/
venv/
__pycache__/
.cache/
coverage/
.turbo/
.parcel-cache/
target/
EXCLUDES
fi
idx=$(mktemp); rm -f "$idx"; trap 'rm -f "$idx"' EXIT
export GIT_INDEX_FILE="$idx"
git add -A
tree=$(git write-tree)
p=""; headrev=$(git rev-parse -q --verify HEAD) && p="-p $headrev"
c=$(git -c user.name=4aicoding -c user.email=agent@4aicoding.local commit-tree "$tree" $p -m "checkpoint $RUN_ID")
git update-ref "refs/4aicoding/checkpoints/$RUN_ID" "$c"
echo "$c"
`

// filesChangedScript diffs the CURRENT workspace against the run's
// checkpoint ref (temp index again — the user's staging area is never
// read or written). -z makes git emit NUL-separated records and NEVER
// C-quote paths: without it "tên file.txt" comes out as
// "t\303\252n file.txt" — a path neither revert nor the UI can use.
// --no-renames is the REVERT CONTRACT (the A3.3 rename bug): revert
// deletes the status-A rows and lets `git checkout <ref> -- .`
// restore everything the tree tracks — with rename detection a
// renamed file collapses into ONE R row carrying only the NEW path,
// so the new file lands in neither list (checkout restores the old
// name, the A-list is empty) and survives the revert. Split into
// D <old> + A <new>, the same rename composes back exactly.
// The 500-entry cap moved into the parser (head -n cannot split on
// NULs); git's own output stays bounded by MaxOutputBytes.
const filesChangedScript = `set -euo pipefail
idx=$(mktemp); rm -f "$idx"; trap 'rm -f "$idx"' EXIT
export GIT_INDEX_FILE="$idx"
git add -A
git diff --cached --name-status -z --no-renames "$CHECK_REF"
`

// filesChangedMaxEntries is plan §8's cap on files_changed rows.
const filesChangedMaxEntries = 500

// FileChange is one row of files_changed (plan §9.3 result).
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status"` // A | M | D | R
}

// Checkpoint creates the run's checkpoint and returns the commit ref.
func Checkpoint(ctx context.Context, sbx *sandboxd.Client, sandboxID, projectDir, runID string) (string, error) {
	res, err := sbx.Exec(ctx, sandboxID, sandboxd.ExecInput{
		Cmd:            []string{"bash", "-c", checkpointScript},
		Cwd:            projectDir,
		Env:            map[string]string{"RUN_ID": runID},
		TimeoutS:       60,
		MaxOutputBytes: 8 << 10,
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("checkpoint failed (exit %d): %s", res.ExitCode, lastLine(res.Stderr))
	}
	ref := strings.TrimSpace(lastLine(res.Stdout))
	if ref == "" {
		return "", fmt.Errorf("checkpoint produced no ref")
	}
	return ref, nil
}

// FilesChanged lists the workspace changes vs the checkpoint ref
// (≤ 500 entries, plan §8). An unreadable repo answers an empty list
// plus the error for the warning log.
func FilesChanged(ctx context.Context, sbx *sandboxd.Client, sandboxID, projectDir, ref string) ([]FileChange, error) {
	res, err := sbx.Exec(ctx, sandboxID, sandboxd.ExecInput{
		Cmd:            []string{"bash", "-c", filesChangedScript},
		Cwd:            projectDir,
		Env:            map[string]string{"CHECK_REF": ref},
		TimeoutS:       60,
		MaxOutputBytes: 128 << 10,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("files_changed failed (exit %d): %s", res.ExitCode, lastLine(res.Stderr))
	}
	return parseFilesChangedZ(res.Stdout), nil
}

// parseFilesChangedZ parses `git diff --cached --name-status -z
// --no-renames` output. Records are NUL-terminated: "status \0 path
// \0". The rename/copy shape ("R100 \0 old \0 new \0" — NEW path wins)
// can no longer occur — the script passes --no-renames — but the
// parser keeps the three-field branch as defence: a half-parsed R
// record must never swallow the NEXT row's status as the new path.
// Because every record ends with a NUL, a stream that does not end in
// NUL was truncated mid-record (MaxOutputBytes): the partial tail is
// dropped so a half path never reaches the DB or the UI. Parsing
// stops at filesChangedMaxEntries.
func parseFilesChangedZ(out string) []FileChange {
	if out != "" && !strings.HasSuffix(out, "\x00") {
		if i := strings.LastIndexByte(out, '\x00'); i >= 0 {
			out = out[:i+1]
		} else {
			out = ""
		}
	}
	fields := strings.Split(out, "\x00")
	changes := []FileChange{}
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			break // trailing NUL / end of stream
		}
		code := status
		if len(code) > 1 { // rename score, e.g. R100
			code = code[:1]
		}
		want := 2 // status + path
		if code == "R" || code == "C" {
			want = 3 // status + old + new — keep the NEW path
		}
		if i+want > len(fields) {
			break // truncated record (belt and braces)
		}
		path := fields[i+want-1]
		if path == "" || (want == 3 && fields[i+1] == "") {
			break
		}
		changes = append(changes, FileChange{Path: path, Status: code})
		i += want
		if len(changes) >= filesChangedMaxEntries {
			break
		}
	}
	return changes
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n\r \t")
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// revertScript restores the run's checkpoint (plan §8): git checkout
// <ref> -- . puts the checkpoint content back into index + worktree
// for every path the tree tracks, then the run's own status-A paths —
// the files it created, which the ref does not track, so checkout
// leaves them — are removed from the NUL-separated list on stdin.
// Paths ride stdin (never argv, never interpolation): they may contain
// spaces, unicode, quotes. No git clean, no HEAD move, no branch or
// user-staging reset beyond what checkout <ref> -- . prescribes.
const revertScript = `set -euo pipefail
if [ -n "$(git ls-tree -r --name-only "$CHECK_REF" | head -n 1)" ]; then
  git checkout "$CHECK_REF" -- .
fi
paths=$(mktemp); rm -f "$paths"; trap 'rm -f "$paths"' EXIT
cat > "$paths"
if [ -s "$paths" ]; then
  xargs -0 -r rm -f -- < "$paths"
fi
`

// RevertChanges rolls the workspace back to the run's checkpoint ref
// (plan §8). addedPaths is the run's own files_changed subset with
// status A. The caller post-checks with FilesChanged — after a
// correct revert the diff against the ref is empty.
func RevertChanges(ctx context.Context, sbx *sandboxd.Client, sandboxID, projectDir, ref string, addedPaths []string) error {
	stdin := make([]byte, 0, 64*len(addedPaths))
	for _, p := range addedPaths {
		stdin = append(stdin, p...)
		stdin = append(stdin, 0)
	}
	res, err := sbx.Exec(ctx, sandboxID, sandboxd.ExecInput{
		Cmd:            []string{"bash", "-c", revertScript},
		Cwd:            projectDir,
		Env:            map[string]string{"CHECK_REF": ref},
		Stdin:          stdin,
		TimeoutS:       60,
		MaxOutputBytes: 8 << 10,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("revert failed (exit %d): %s", res.ExitCode, lastLine(res.Stderr))
	}
	return nil
}
