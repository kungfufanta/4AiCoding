// checkpoint_test.go — git-level correctness of the run checkpoint
// (plan §8, plus the two A3.3 fixes from the #69 review):
//
//  1. files_changed must carry REAL paths: a project file named
//     "tên file.txt" must not come out as git's C-quoted
//     "t\303\252n file.txt" — revert deletes by these paths and the
//     UI shows them.
//  2. A project without .gitignore must not drag build output
//     (node_modules/, .next/, dist/...) into every checkpoint: the
//     defaults land in .git/info/exclude (BRAIN.md-style), and the
//     user's own .gitignore always wins when present.
//
// The fake sandboxd (sandboxtest) executes the scripts with REAL git
// and REAL bash on the host — the same way the #69 review ran them.
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent/sandboxtest"
)

// gitSandbox starts the host-mapped fake sandboxd with a committed
// git repo at the default project dir (/home/sandbox/workspace →
// root/workspace). All git plumbing runs with a hermetic env: no
// host git config leaks in — core.quotePath included, so the
// C-quoting bug the -z fix addresses is really exercised.
func gitSandbox(t *testing.T) (*sandboxtest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	sbx := sandboxtest.New(root)
	project := filepath.Join(root, "workspace")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, project, "init", "-q")
	git(t, project, "config", "user.name", "t")
	git(t, project, "config", "user.email", "t@example.invalid")
	if err := os.WriteFile(filepath.Join(project, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, project, "add", "-A")
	git(t, project, "commit", "-qm", "init")
	return sbx, project
}

// git runs one git command with a hermetic environment (no global or
// system config, no user identity).
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// checkpointTree lists the paths of a checkpoint commit. Only ASCII
// names are asserted through it: ls-tree C-quotes non-ASCII paths no
// matter what our fix does.
func checkpointTree(t *testing.T, project, ref string) []string {
	t.Helper()
	cmd := exec.Command("git", "ls-tree", "-r", "--name-only", ref)
	cmd.Dir = project
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ls-tree %s: %v", ref, err)
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

// TestFilesChangedKeepsUnicodeAndSpaceNames runs checkpoint → mutate →
// files_changed with filenames git would C-quote without -z
// (Vietnamese diacritics AND plain spaces) and asserts every row
// carries the real path — never a quoted or escaped one.
func TestFilesChangedKeepsUnicodeAndSpaceNames(t *testing.T) {
	sbx, project := gitSandbox(t)
	client := sbx.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ref, err := Checkpoint(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", "run-uni-1")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	created := map[string]string{
		"tên file.txt":  "xin chào\n", // diacritics AND a space
		"nhị phân.txt":  "dấu tiếng Việt\n",
		"with space.md": "# heading\n", // spaces only
	}
	for name, body := range created {
		if err := os.WriteFile(filepath.Join(project, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changes, err := FilesChanged(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", ref)
	if err != nil {
		t.Fatalf("files_changed: %v", err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Status
	}
	for name := range created {
		if got[name] != "A" {
			t.Errorf("path %q: got status %q (want A); rows: %v", name, got[name], changes)
		}
	}
	if got["base.txt"] != "M" {
		t.Errorf("base.txt: got %q (want M)", got["base.txt"])
	}
	for _, c := range changes {
		if strings.HasPrefix(c.Path, "\"") || strings.Contains(c.Path, "\\3") {
			t.Errorf("C-quoted path leaked into files_changed: %q", c.Path)
		}
	}
}

// TestFilesChangedRenameKeepsNewPath — rename detection under -z: the
// row keeps status R and the NEW path (the path revert must keep and
// the UI must show).
func TestFilesChangedRenameKeepsNewPath(t *testing.T) {
	sbx, project := gitSandbox(t)
	client := sbx.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := os.WriteFile(filepath.Join(project, "old name.txt"), []byte("renamed later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := Checkpoint(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", "run-ren-1")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := os.Rename(filepath.Join(project, "old name.txt"), filepath.Join(project, "tên mới.txt")); err != nil {
		t.Fatal(err)
	}

	changes, err := FilesChanged(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", ref)
	if err != nil {
		t.Fatalf("files_changed: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want exactly the rename row, got %v", changes)
	}
	c := changes[0]
	if c.Status != "R" {
		t.Errorf("status = %q (want R)", c.Status)
	}
	if c.Path != "tên mới.txt" {
		t.Errorf("rename row kept path %q (want the NEW path %q)", c.Path, "tên mới.txt")
	}
}

// TestCheckpointExcludesBuildOutputWithoutGitignore: no .gitignore →
// the checkpoint tree must not contain node_modules/.next/dist (slow
// "git add -A", disk bloat), and .git/info/exclude must carry the
// defaults next to BRAIN.md.
func TestCheckpointExcludesBuildOutputWithoutGitignore(t *testing.T) {
	sbx, project := gitSandbox(t)
	client := sbx.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, p := range []string{
		filepath.Join(project, "node_modules", "left-pad", "index.js"),
		filepath.Join(project, ".next", "cache", "pack.json"),
		filepath.Join(project, "dist", "bundle.js"),
		filepath.Join(project, "src", "main.go"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ref, err := Checkpoint(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", "run-exc-1")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	tree := checkpointTree(t, project, ref)
	for _, banned := range []string{"node_modules/", ".next/", "dist/"} {
		for _, path := range tree {
			if strings.HasPrefix(path, banned) {
				t.Errorf("checkpoint tree contains build output %q (want excluded)", path)
			}
		}
	}
	if !slices.Contains(tree, "src/main.go") {
		t.Errorf("checkpoint tree lost real source src/main.go (got %v)", tree)
	}

	exc, err := os.ReadFile(filepath.Join(project, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BRAIN.md\n", "node_modules/\n", ".next/\n", "dist/\n"} {
		if !strings.Contains(string(exc), want) {
			t.Errorf(".git/info/exclude missing %q; file:\n%s", want, exc)
		}
	}
}

// TestCheckpointKeepsUserGitignore: when the user HAS a .gitignore,
// no defaults are added — the user's rules (and only them) apply.
func TestCheckpointKeepsUserGitignore(t *testing.T) {
	sbx, project := gitSandbox(t)
	client := sbx.Client()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// node_modules stays visible: the user chose no default excludes.
	for _, p := range []string{
		filepath.Join(project, "node_modules", "x.js"),
		filepath.Join(project, "secret.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ref, err := Checkpoint(ctx, client, sandboxtest.DefaultID, "/home/sandbox/workspace", "run-exc-2")
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	tree := checkpointTree(t, project, ref)
	if !slices.Contains(tree, "node_modules/x.js") {
		t.Errorf("user's own .gitignore wins: node_modules/x.js missing from %v", tree)
	}
	if slices.Contains(tree, "secret.txt") {
		t.Errorf("user's own .gitignore rule not applied: secret.txt in %v", tree)
	}

	exc, err := os.ReadFile(filepath.Join(project, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exc), "node_modules/") {
		t.Errorf("defaults leaked into .git/info/exclude despite .gitignore:\n%s", exc)
	}
	if !strings.Contains(string(exc), "BRAIN.md") {
		t.Errorf("BRAIN.md exclude missing:\n%s", exc)
	}
}

func TestParseFilesChangedZ(t *testing.T) {
	t.Run("unicode and spaces stay verbatim", func(t *testing.T) {
		rows := parseFilesChangedZ("A\x00tên file.txt\x00M\x00base.txt\x00")
		want := []FileChange{
			{Path: "tên file.txt", Status: "A"},
			{Path: "base.txt", Status: "M"},
		}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("got %v, want %v", rows, want)
		}
	})
	t.Run("rename keeps the NEW path", func(t *testing.T) {
		rows := parseFilesChangedZ("R100\x00old name.txt\x00tên mới.txt\x00")
		want := []FileChange{{Path: "tên mới.txt", Status: "R"}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("got %v, want %v", rows, want)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		rows := parseFilesChangedZ("D\x00gone.txt\x00")
		if len(rows) != 1 || rows[0].Path != "gone.txt" || rows[0].Status != "D" {
			t.Errorf("got %v", rows)
		}
	})
	t.Run("empty output", func(t *testing.T) {
		if rows := parseFilesChangedZ(""); len(rows) != 0 {
			t.Errorf("got %v", rows)
		}
	})
	t.Run("truncated tail dropped", func(t *testing.T) {
		// MaxOutputBytes cut the stream mid-record: the dangling
		// "M\0par" must not become a row with a half path.
		rows := parseFilesChangedZ("A\x00p1.txt\x00M\x00par")
		want := []FileChange{{Path: "p1.txt", Status: "A"}}
		if !reflect.DeepEqual(rows, want) {
			t.Errorf("got %v, want %v", rows, want)
		}
	})
	t.Run("truncated rename dropped", func(t *testing.T) {
		// "R100\0old\0" without the NEW path must not produce a row.
		rows := parseFilesChangedZ("R100\x00old.txt\x00")
		if len(rows) != 0 {
			t.Errorf("got %v, want no rows", rows)
		}
	})
	t.Run("capped at 500", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 600; i++ {
			fmt.Fprintf(&b, "A\x00f%d.txt\x00", i)
		}
		rows := parseFilesChangedZ(b.String())
		if len(rows) != filesChangedMaxEntries {
			t.Errorf("got %d rows, want %d", len(rows), filesChangedMaxEntries)
		}
		if rows[len(rows)-1].Path != "f499.txt" {
			t.Errorf("capped rows lost order: last is %q", rows[len(rows)-1].Path)
		}
	})
}
