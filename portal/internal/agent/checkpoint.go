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

// checkpointScript is plan §8 verbatim, plus the BRAIN.md exclude so
// the project-memory file never enters a checkpoint tree (upstream
// rule: BRAIN.md is kept out of git via .git/info/exclude).
const checkpointScript = `set -euo pipefail
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || git init -q
mkdir -p .git/info
grep -qxF 'BRAIN.md' .git/info/exclude 2>/dev/null || echo BRAIN.md >> .git/info/exclude
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
// read or written).
const filesChangedScript = `set -euo pipefail
idx=$(mktemp); rm -f "$idx"; trap 'rm -f "$idx"' EXIT
export GIT_INDEX_FILE="$idx"
git add -A
git diff --cached --name-status "$CHECK_REF" | head -n 500
`

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
	out := []FileChange{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// "A\tpath", "M\tpath", "D\tpath", "R100\told\tnew".
		status, rest, ok := strings.Cut(line, "\t")
		if !ok || status == "" {
			continue
		}
		code := strings.TrimSpace(status)
		if len(code) > 1 { // rename score, e.g. R100
			code = code[:1]
		}
		path := rest
		if i := strings.Index(rest, "\t"); i >= 0 { // rename: keep the NEW path
			path = rest[i+1:]
		}
		out = append(out, FileChange{Path: path, Status: code})
	}
	return out, nil
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n\r \t")
	if i := strings.LastIndexAny(s, "\r\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
