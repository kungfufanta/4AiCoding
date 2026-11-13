// build.go — the post-run checks (docs/AGENT_PLAN_A3.md §7 step 6):
// build detection (decision §2-7: AGENTS.md build block first, then
// package.json/go.mod/Cargo.toml/pyproject.toml/requirements.txt) and
// the preview probe. Build status is passed | failed | skipped; the
// tail (last 2 KiB of output) rides on the build event.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent/tools"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// Build timeouts (plan §7.6).
const (
	buildTimeoutS    = 300
	previewTimeoutS  = 20
	buildTailBytes   = 2 << 10
	contextChunkA32K = 4 << 10 // single-file read cap for the detectors
)

// BuildResult is one postcheck build attempt.
type BuildResult struct {
	Status  string `json:"status"` // passed | failed | skipped
	Command string `json:"command,omitempty"`
	Tail    string `json:"tail,omitempty"`
}

// DetectBuild reads the project manifests and returns the build
// command ("" = nothing to detect → build_status skipped).
func DetectBuild(ctx context.Context, sbx *sandboxd.Client, sandboxID, relPrefix string) (string, error) {
	// 1. AGENTS.md build block (decision §2-7: priority).
	if data, _, err := tools.Read(ctx, sbx, sandboxID, relPrefix+"/AGENTS.md"); err == nil {
		if cmd := agentsBuildCommand(string(data)); cmd != "" {
			return cmd, nil
		}
	}
	// 2. package.json with a build script.
	if data, _, err := tools.Read(ctx, sbx, sandboxID, relPrefix+"/package.json"); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(data, &pkg) == nil && strings.TrimSpace(pkg.Scripts["build"]) != "" {
			switch {
			case fileExists(ctx, sbx, sandboxID, relPrefix, "pnpm-lock.yaml"):
				return "pnpm run build", nil
			case fileExists(ctx, sbx, sandboxID, relPrefix, "yarn.lock"):
				return "yarn build", nil
			default:
				return "npm run build", nil
			}
		}
		return "", nil // package.json without a build script: not a failure
	}
	// 3-5. The other manifests.
	if fileExists(ctx, sbx, sandboxID, relPrefix, "go.mod") {
		return "go build ./...", nil
	}
	if fileExists(ctx, sbx, sandboxID, relPrefix, "Cargo.toml") {
		return "cargo build", nil
	}
	if fileExists(ctx, sbx, sandboxID, relPrefix, "pyproject.toml") {
		return "python3 -m pip wheel --no-deps -w /tmp/4aicoding-build .", nil
	}
	if fileExists(ctx, sbx, sandboxID, relPrefix, "requirements.txt") {
		return "python3 -m pip install --dry-run -r requirements.txt", nil
	}
	return "", nil
}

// agentsBuildCommand parses an AGENTS.md block:
//
//	build:
//	  npm run build
//
// (also the inline `build: <cmd>` form). The FIRST command line wins.
func agentsBuildCommand(md string) string {
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "build:") {
			if inline := strings.TrimSpace(strings.TrimPrefix(trimmed, "build:")); inline != "" {
				return inline
			}
			for _, next := range lines[i+1:] {
				nt := strings.TrimSpace(next)
				if nt == "" {
					continue
				}
				// Any line at the same or lower indent ends the block.
				if len(strings.TrimLeft(next, " \t")) == len(next) {
					break
				}
				if strings.HasPrefix(nt, "-") || strings.HasPrefix(nt, "#") {
					continue
				}
				return strings.Trim(strings.TrimSpace(next), "`\"")
			}
			return ""
		}
		// Only consider the build: key before the next heading.
		if strings.HasPrefix(trimmed, "# ") {
			continue
		}
	}
	return ""
}

func fileExists(ctx context.Context, sbx *sandboxd.Client, sandboxID, relPrefix, name string) bool {
	_, _, err := tools.Read(ctx, sbx, sandboxID, relPrefix+"/"+name)
	return err == nil
}

// RunBuild executes the detected command. A detected-then-failed
// build is a real failure (build_status failed); nothing detected is
// skipped.
func RunBuild(ctx context.Context, sbx *sandboxd.Client, sandboxID, projectDir, command string) BuildResult {
	if strings.TrimSpace(command) == "" {
		return BuildResult{Status: "skipped"}
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(buildTimeoutS+15)*time.Second)
	defer cancel()
	res, err := sbx.Exec(cctx, sandboxID, sandboxd.ExecInput{
		Cmd:            []string{"bash", "-lc", command},
		Cwd:            projectDir,
		TimeoutS:       buildTimeoutS,
		MaxOutputBytes: 64 << 10,
	})
	if err != nil {
		return BuildResult{Status: "failed", Command: command,
			Tail: "build could not run: " + err.Error()}
	}
	out := strings.TrimSpace(res.Stdout)
	if strings.TrimSpace(res.Stderr) != "" {
		if out != "" {
			out += "\n[stderr]\n"
		}
		out += strings.TrimSpace(res.Stderr)
	}
	tail := out
	if len(tail) > buildTailBytes {
		tail = "…" + tail[len(tail)-buildTailBytes:]
	}
	status := "passed"
	if res.ExitCode != 0 || res.TimedOut {
		status = "failed"
	}
	return BuildResult{Status: status, Command: command, Tail: tail}
}

// CheckPreview probes the sandbox's first public port with curl
// (plan §7.6). checked=false when the sandbox has no ports.
func CheckPreview(ctx context.Context, sbx *sandboxd.Client, sandboxID string, ports []int) (ok, checked bool) {
	if len(ports) == 0 || ports[0] <= 0 {
		return false, false
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(previewTimeoutS+15)*time.Second)
	defer cancel()
	res, err := sbx.Exec(cctx, sandboxID, sandboxd.ExecInput{
		Cmd: []string{"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
			"--max-time", "10", fmt.Sprintf("http://127.0.0.1:%d/", ports[0])},
		Cwd:            "/home/sandbox",
		TimeoutS:       previewTimeoutS,
		MaxOutputBytes: 1 << 10,
	})
	if err != nil {
		return false, true
	}
	code := strings.TrimSpace(res.Stdout)
	return res.ExitCode == 0 && (strings.HasPrefix(code, "2") || strings.HasPrefix(code, "3")), true
}
