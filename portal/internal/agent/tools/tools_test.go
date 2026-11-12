// tools_test.go — the A3.2 tool registry against the sandboxtest fake
// (plan §6 limits): path blocking BEFORE any sandboxd call, the
// write→read→edit round trip, list depth, grep/rg parsing, run_command
// truncation + timeout, and the write_file event preview that never
// carries content.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent/sandboxtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newRegistry builds a registry against a fresh fake sandboxd rooted
// at a temp dir.
func newRegistry(t *testing.T) (*Registry, *sandboxtest.Server, string) {
	t.Helper()
	root := t.TempDir()
	// The project directory must exist: exec maps cwd into it.
	if err := os.MkdirAll(filepath.Join(root, "workspace/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	sbx := sandboxtest.New(root)
	t.Cleanup(sbx.Close)
	reg := New(sbx.Client(), sandboxtest.DefaultID,
		"/home/sandbox/workspace/app", "workspace/app", false, []int{3000}, testLogger())
	return reg, sbx, root
}

func mustExec(t *testing.T, reg *Registry, name, args string) Result {
	t.Helper()
	res, err := reg.Exec(context.Background(), llm.ToolCall{ID: "t1", Name: name, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s exec error: %v", name, err)
	}
	return res
}

// TestBlockedPaths — "..", absolute and Windows-style paths are
// rejected before sandboxd sees anything (plan §6, acceptance: path
// `../../etc/passwd` and `/etc/passwd` chặn trước khi gọi sandboxd).
func TestBlockedPaths(t *testing.T) {
	reg, sbx, _ := newRegistry(t)
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "a/../../b", `sub\file.txt`, "~root/x"} {
		res := mustExec(t, reg, "write_file", fmt.Sprintf(`{"path":%q,"content":"x"}`, p))
		if res.OK || res.ErrCode != "blocked_path" {
			t.Fatalf("write_file %q = ok=%v err=%v, want blocked", p, res.OK, res.Text)
		}
		res = mustExec(t, reg, "read_file", fmt.Sprintf(`{"path":%q}`, p))
		if res.OK {
			t.Fatalf("read_file %q = ok, want blocked", p)
		}
	}
	if len(sbx.Execs) != 0 {
		t.Fatalf("blocked paths reached sandboxd exec: %+v", sbx.Execs)
	}
	entries, _, err := List(context.Background(), sbx.Client(), sandboxtest.DefaultID, "workspace/app", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("blocked writes landed in the workspace: %+v", entries)
	}
}

// TestWriteReadEditRoundtrip — the core file flow with plan §6 caps.
func TestWriteReadEditRoundtrip(t *testing.T) {
	reg, _, root := newRegistry(t)

	res := mustExec(t, reg, "write_file", `{"path":"src/hello.txt","content":"line one\nline two\nline three\n"}`)
	if !res.OK || !strings.Contains(res.Text, "wrote src/hello.txt") {
		t.Fatalf("write_file = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(root, "workspace/app/src/hello.txt")); err != nil {
		t.Fatalf("file not on the fake root: %v", err)
	}

	// read_file with line numbers + totals.
	res = mustExec(t, reg, "read_file", `{"path":"src/hello.txt","offset":2,"limit":1}`)
	if !strings.Contains(res.Text, "     2| line two") || !strings.Contains(res.Text, "3 lines total; showing 2..2") {
		t.Fatalf("read_file paging = %q", res.Text)
	}
	// offset past the end.
	res = mustExec(t, reg, "read_file", `{"path":"src/hello.txt","offset":99}`)
	if !res.OK || !strings.Contains(res.Text, "past the end") {
		t.Fatalf("read_file offset past end = %+v", res)
	}

	// edit_file: unique old → replaced.
	res = mustExec(t, reg, "edit_file", `{"path":"src/hello.txt","old":"line two","new":"LINE 2"}`)
	if !res.OK || !strings.Contains(res.Text, "1 replacement") {
		t.Fatalf("edit_file unique = %+v", res)
	}
	res = mustExec(t, reg, "read_file", `{"path":"src/hello.txt"}`)
	if !strings.Contains(res.Text, "LINE 2") {
		t.Fatalf("edit not applied: %q", res.Text)
	}

	// Ambiguous old: refused, nothing changed.
	mustExec(t, reg, "write_file", `{"path":"dup.txt","content":"word word\n"}`)
	res = mustExec(t, reg, "edit_file", `{"path":"dup.txt","old":"word","new":"X"}`)
	if res.OK || !strings.Contains(res.Text, "appears 2 times") || res.ErrCode != "old_not_unique" {
		t.Fatalf("edit_file ambiguous = %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "workspace/app/dup.txt"))
	if string(data) != "word word\n" {
		t.Fatalf("ambiguous edit changed the file: %q", data)
	}
	// replace_all flips every occurrence.
	res = mustExec(t, reg, "edit_file", `{"path":"dup.txt","old":"word","new":"X","replace_all":true}`)
	if !res.OK || !strings.Contains(res.Text, "2 replacement") {
		t.Fatalf("edit_file replace_all = %+v", res)
	}
	// old not found.
	res = mustExec(t, reg, "edit_file", `{"path":"dup.txt","old":"nope","new":"X"}`)
	if res.OK || res.ErrCode != "old_not_found" {
		t.Fatalf("edit_file missing old = %+v", res)
	}

	// Binary file: reported, not shown.
	mustExec(t, reg, "write_file", `{"path":"blob.bin","content":"a\u0000b"}`)
	res = mustExec(t, reg, "read_file", `{"path":"blob.bin"}`)
	if !res.OK || !strings.Contains(res.Text, "binary") {
		t.Fatalf("read_file binary = %+v", res)
	}
}

// TestListFilesDepth — the tree respects depth, excludes generated
// dirs, and shows a placeholder for pruned directories.
func TestListFilesDepth(t *testing.T) {
	reg, _, root := newRegistry(t)
	for _, p := range []string{
		"README.md", "src/main.go", "src/util/util.go",
		"src/deep/a/b/c.txt", "node_modules/pkg/index.js",
	} {
		full := filepath.Join(root, "workspace/app", filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := mustExec(t, reg, "list_files", `{"depth":2}`)
	// depth 2: project root + one level below — src/main.go shows,
	// src/util shows as a pruned placeholder, node_modules is gone.
	if !strings.Contains(res.Text, "src/") || !strings.Contains(res.Text, "README.md") ||
		!strings.Contains(res.Text, "main.go") || !strings.Contains(res.Text, "util/…") ||
		!strings.Contains(res.Text, "deep/…") {
		t.Fatalf("list depth 2 = %q", res.Text)
	}
	if strings.Contains(res.Text, "util.go") || strings.Contains(res.Text, "c.txt") {
		t.Fatalf("depth 2 shows deeper files: %q", res.Text)
	}
	if strings.Contains(res.Text, "node_modules") {
		t.Fatalf("node_modules leaked into the listing: %q", res.Text)
	}
	res = mustExec(t, reg, "list_files", `{"path":"src","depth":1}`)
	if !strings.Contains(res.Text, "main.go") || !strings.Contains(res.Text, "util/…") ||
		strings.Contains(res.Text, "util.go") {
		t.Fatalf("list src depth 1 = %q", res.Text)
	}
}

// TestRunCommandTruncation — 5 MiB of output reaches the model as
// 16 KiB head+tail (plan §6), and the event preview is smaller.
func TestRunCommandTruncation(t *testing.T) {
	reg, _, _ := newRegistry(t)
	res := mustExec(t, reg, "run_command",
		`{"command":"head -c 5000000 /dev/zero | tr '\\0' 'a'"}`)
	if res.OK || res.ExitCode == 0 {
		// pipeline exit code: head exits 0; keep going either way.
		_ = res
	}
	if res.Truncated != true {
		t.Fatalf("big output not marked truncated: %+v", res)
	}
	if len(res.Text) > 20<<10 {
		t.Fatalf("LLM-facing output too large: %d bytes", len(res.Text))
	}
	if !strings.Contains(res.Text, "bytes hidden") {
		t.Fatalf("truncation marker missing: %.200q", res.Text)
	}
}

// TestRunCommandTimeout — a sleep beyond timeout_s reports timed_out
// quickly (acceptance: `sleep 999` with timeout_s 2 → timed_out ≤ 4 s).
func TestRunCommandTimeout(t *testing.T) {
	reg, _, _ := newRegistry(t)
	start := time.Now()
	res := mustExec(t, reg, "run_command", `{"command":"sleep 999","timeout_s":1}`)
	elapsed := time.Since(start)
	if res.OK || !res.TimedOut {
		t.Fatalf("sleep 999 = %+v, want timed_out", res)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("timeout took %s, want <= 4s", elapsed)
	}
	if !strings.Contains(res.Text, "timed_out=true") {
		t.Fatalf("result text missing timed_out marker: %q", res.Text)
	}
}

// TestSearchGrepFallback — search works without rg (plan §6 grep
// fallback); the rg JSON parser is tested separately.
func TestSearchGrepFallback(t *testing.T) {
	reg, _, root := newRegistry(t)
	full := filepath.Join(root, "workspace/app")
	if err := os.MkdirAll(filepath.Join(full, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(full, "main.go"), []byte("package main\n// TODO fix\n"), 0o644)
	os.WriteFile(filepath.Join(full, "pkg", "a.go"), []byte("package pkg\n// TODO later\n"), 0o644)

	res := mustExec(t, reg, "search", `{"pattern":"TODO"}`)
	if !res.OK || !strings.Contains(res.Text, "main.go:2: // TODO fix") ||
		!strings.Contains(res.Text, filepath.Join("pkg", "a.go")+":2: // TODO later") {
		t.Fatalf("search = %+v", res)
	}
	res = mustExec(t, reg, "search", `{"pattern":"absent-needle"}`)
	if !res.OK || res.Text != "no matches" {
		t.Fatalf("empty search = %+v", res)
	}
}

// TestParseSearchRGJSON — the rg --json event parser.
func TestParseSearchRGJSON(t *testing.T) {
	out := `{"type":"begin","data":{"path":{"text":"a.go"}}}
{"type":"match","data":{"path":{"text":"a.go"},"line_number":3,"lines":{"text":"func X() {}\n"}}}
junk not json
{"type":"match","data":{"path":{"text":"b.go"},"line_number":10,"lines":{"text":"y"}}}
{"type":"end","data":{}}
`
	hits := parseSearch(out, true)
	if len(hits) != 2 || hits[0].Path != "a.go" || hits[0].Line != 3 ||
		hits[0].Text != "func X() {}" || hits[1].Path != "b.go" {
		t.Fatalf("parseSearch rg = %+v", hits)
	}
	hits = parseSearch("main.go:12: hello\nx/y.go:3: world\nbadline\n", false)
	if len(hits) != 2 || hits[0].Line != 12 || hits[1].Path != "x/y.go" {
		t.Fatalf("parseSearch grep = %+v", hits)
	}
}

// TestPreviewArgs — write_file events carry path+bytes only
// (plan §9.3: no full content on the wire).
func TestPreviewArgs(t *testing.T) {
	raw := PreviewArgs(llm.ToolCall{Name: "write_file",
		Arguments: json.RawMessage(`{"path":"secret.txt","content":"SUPERSECRETCONTENT"}`)})
	s := string(raw)
	if strings.Contains(s, "SUPERSECRETCONTENT") || strings.Contains(s, "content") {
		t.Fatalf("write_file preview leaks content: %s", s)
	}
	if !strings.Contains(s, `"bytes":18`) || !strings.Contains(s, "secret.txt") {
		t.Fatalf("write_file preview = %s", s)
	}
	big := `{"path":"a.txt","content":"` + strings.Repeat("x", 4000) + `"}`
	raw = PreviewArgs(llm.ToolCall{Name: "edit_file", Arguments: json.RawMessage(big)})
	if len(raw) > 2200 || !strings.Contains(string(raw), "args_preview") {
		t.Fatalf("oversized args not cut: %d bytes", len(raw))
	}
}

// TestCheckPreviewNoPort — portless sandboxes answer a clear error.
func TestCheckPreviewNoPort(t *testing.T) {
	root := t.TempDir()
	sbx := sandboxtest.New(root)
	t.Cleanup(sbx.Close)
	reg := New(sbx.Client(), sandboxtest.DefaultID,
		"/home/sandbox/workspace", "workspace", false, nil, testLogger())
	res := mustExec(t, reg, "check_preview", `{}`)
	if res.OK || res.ErrCode != "no_preview_port" {
		t.Fatalf("check_preview portless = %+v", res)
	}
}

// TestFinishCapsSummary — finish truncates at 4000 runes.
func TestFinishCapsSummary(t *testing.T) {
	reg, _, _ := newRegistry(t)
	long := strings.Repeat("x", 5000)
	res := mustExec(t, reg, "finish", fmt.Sprintf(`{"summary":%q}`, long))
	if !res.OK || len([]rune(res.Text)) != 4000 {
		t.Fatalf("finish summary = %d runes, ok=%v", len([]rune(res.Text)), res.OK)
	}
	res = mustExec(t, reg, "finish", `{"summary":""}`)
	if res.OK {
		t.Fatal("empty finish accepted")
	}
}

// TestUnknownToolAndBadArgs — contract violations come back as data.
func TestUnknownToolAndBadArgs(t *testing.T) {
	reg, _, _ := newRegistry(t)
	res := mustExec(t, reg, "no_such_tool", `{}`)
	if res.OK || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("unknown tool = %+v", res)
	}
	res = mustExec(t, reg, "read_file", `{"path":123}`)
	if res.OK || !strings.Contains(res.Text, "invalid arguments") {
		t.Fatalf("bad args = %+v", res)
	}
	res = mustExec(t, reg, "write_file", `{"path":"x.txt","content":"a","bogus":1}`)
	if res.OK || !strings.Contains(res.Text, "invalid arguments") {
		t.Fatalf("unknown field = %+v", res)
	}
}

// TestFilesAPIClient — the exported List/Read/Write helpers the
// workspace builder reuses.
func TestFilesAPIClient(t *testing.T) {
	newRegistry(t) // ensure helper compiles against the fake
	root := t.TempDir()
	sbx := sandboxtest.New(root)
	t.Cleanup(sbx.Close)
	client := sbx.Client()
	ctx := context.Background()

	if err := Write(ctx, client, sandboxtest.DefaultID, "workspace/AGENTS.md", []byte("# agents"), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, size, err := Read(ctx, client, sandboxtest.DefaultID, "workspace/AGENTS.md")
	if err != nil || string(data) != "# agents" || size != 8 {
		t.Fatalf("read: %v %q %d", err, data, size)
	}
	entries, _, err := List(ctx, client, sandboxtest.DefaultID, "workspace", 10)
	if err != nil || len(entries) != 1 || entries[0].Path != "workspace/AGENTS.md" {
		t.Fatalf("list: %v %+v", err, entries)
	}
	var api *sandboxd.APIError
	_, _, err = Read(ctx, client, sandboxtest.DefaultID, "workspace/missing.md")
	if err == nil || !asAPI(err, &api) || api.Status != 404 {
		t.Fatalf("missing file = %v, want sandboxd 404", err)
	}
}

func asAPI(err error, target **sandboxd.APIError) bool {
	if e, ok := err.(*sandboxd.APIError); ok {
		*target = e
		return true
	}
	return false
}
