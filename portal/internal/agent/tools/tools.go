// Package tools is the A3.2 agent tool registry (docs/AGENT_PLAN_A3.md
// §6): the eight tools the model may call during a run, executed
// against ONE sandbox through the sandboxd client (exec v2 + files
// API). The sandbox id and org id never come from the model — they
// are wired once, when the registry is built, and no tool accepts a
// sandbox parameter (AGENT_PLAN_A3 §3 principle 1).
//
// Tool paths are relative to the project directory (/home/sandbox/
// workspace/app when it exists, else /home/sandbox/workspace —
// decision §13-2). Paths are normalised here and again by the files
// API; "..", absolute paths and empty paths are rejected before any
// call leaves the Portal.
//
// Tool failures are DATA for the model: an exit code, a blocked path
// or an ambiguous edit is returned as the tool result so the run
// continues. Only an unreachable sandboxd fails the run (the loop
// counts consecutive transport errors, plan §6 "sandbox_unavailable").
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// Limits from plan §6. The LLM-facing cap of run_command output is
// 16 KiB (head 8 KiB + tail 8 KiB); sandboxd itself caps the exec
// output at 256 KiB.
const (
	listMaxEntries    = 2000
	listMaxDepth      = 4
	readMaxLines      = 2000
	readLineMaxRunes  = 2000
	readMaxBytes      = 128 << 10 // text handed to the LLM per read_file
	writeMaxBytes     = 1 << 20
	searchMaxHits     = 200
	searchLineRunes   = 300
	commandMaxTimeout = 600 // seconds, plan §6
	commandDefaultTO  = 120
	outputLLMBytes    = 16 << 10
	outputHalfBytes   = 8 << 10
	previewMaxSeconds = 10
)

// ErrBlockedPath is returned (as a tool result, not a run failure)
// when a path argument is absolute, contains ".." or is otherwise
// unusable. Plan §6: blocked BEFORE calling sandboxd.
var ErrBlockedPath = errors.New("tools: path blocked")

// Result is what one tool execution hands back: OK=false is a normal,
// model-visible failure (bad path, ambiguous edit, non-zero exit);
// err != nil is an infrastructure failure the loop may escalate on.
type Result struct {
	OK        bool
	Text      string // the tool_result content the LLM sees
	ExitCode  int    // -1 = not applicable (files API tools)
	TimedOut  bool
	Truncated bool // Text was trimmed to fit the LLM budget
	ErrCode   string
}

// Registry executes the eight tools against one sandbox.
type Registry struct {
	sbx        *sandboxd.Client
	sandboxID  string
	projectDir string // absolute in-container dir, e.g. /home/sandbox/workspace/app
	relPrefix  string // files-API path prefix relative to /home/sandbox, e.g. "workspace/app"
	hasRg      bool
	previewURL string // http://127.0.0.1:<first public port> ("" when portless)
	log        *slog.Logger
}

// New builds the registry for one run. hasRg switches `search` to the
// grep fallback (plan §7 step 2: "thiếu rg thì search dùng grep").
func New(sbx *sandboxd.Client, sandboxID, projectDir, relPrefix string, hasRg bool, ports []int, log *slog.Logger) *Registry {
	r := &Registry{
		sbx:        sbx,
		sandboxID:  sandboxID,
		projectDir: projectDir,
		relPrefix:  relPrefix,
		hasRg:      hasRg,
		log:        log,
	}
	if len(ports) > 0 && ports[0] > 0 {
		r.previewURL = fmt.Sprintf("http://127.0.0.1:%d", ports[0])
	}
	return r
}

// ProjectDir returns the absolute project directory of the run.
func (r *Registry) ProjectDir() string { return r.projectDir }

// Names lists the tool names in registry order.
func Names() []string {
	return []string{"list_files", "read_file", "write_file", "edit_file",
		"search", "run_command", "check_preview", "finish"}
}

// Definitions returns the JSON Schemas sent to the model (plan §6).
func Definitions() []llm.Tool {
	return []llm.Tool{
		{
			Name: "list_files",
			Description: "List the workspace tree. Paths are relative to the project " +
				"directory. node_modules/.git/dist and similar generated dirs are excluded.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":  {"type": "string", "description": "Subdirectory to list (\".\" = project root)"},
    "depth": {"type": "integer", "description": "1-4 (default 4)"}
  },
  "required": []
}`),
		},
		{
			Name: "read_file",
			Description: "Read a text file with line numbers. Binary files are reported " +
				"as \"binary, N bytes\" without content.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":   {"type": "string"},
    "offset": {"type": "integer", "description": "First line to show (1-based, default 1)"},
    "limit":  {"type": "integer", "description": "Max lines to show, <= 2000 (default 2000)"}
  },
  "required": ["path"]
}`),
		},
		{
			Name: "write_file",
			Description: "Create or overwrite a file (<= 1 MiB). Parent directories are " +
				"created automatically. Prefer edit_file for existing files.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string"},
    "content": {"type": "string"}
  },
  "required": ["path", "content"]
}`),
		},
		{
			Name: "edit_file",
			Description: "Replace an exact substring in a text file. The old text must appear " +
				"exactly once unless replace_all is true — otherwise nothing changes " +
				"and the error tells you how many occurrences were found.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":        {"type": "string"},
    "old":         {"type": "string"},
    "new":         {"type": "string"},
    "replace_all": {"type": "boolean"}
  },
  "required": ["path", "old", "new"]
}`),
		},
		{
			Name: "search",
			Description: "Search file contents (ripgrep). Returns up to 200 matches as " +
				"path:line: text.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string"},
    "path":    {"type": "string", "description": "Subdirectory to search (default project root)"},
    "glob":    {"type": "string", "description": "Filename filter, e.g. \"*.go\""}
  },
  "required": ["pattern"]
}`),
		},
		{
			Name: "run_command",
			Description: "Run a shell command (bash -lc) in the sandbox. Output is capped " +
				"(16 KiB head/tail mix). Use for builds, git, curl, process checks.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":   {"type": "string"},
    "cwd":       {"type": "string", "description": "Subdirectory to run in (default project root)"},
    "timeout_s": {"type": "integer", "description": "1-600 (default 120)"}
  },
  "required": ["command"]
}`),
		},
		{
			Name: "check_preview",
			Description: "Request the sandbox's own preview port to verify the app responds " +
				"(curl -sS --max-time 10). Reports the HTTP status code.",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "URL path (default \"/\")"},
    "port": {"type": "integer", "description": "Local port (default the sandbox's first public port)"}
  },
  "required": []
}`),
		},
		{
			Name: "finish",
			Description: "End the run. Call it when the task is done, with a short factual " +
				"summary of what changed and what you verified (<= 4000 characters).",
			Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {"type": "string"}
  },
  "required": ["summary"]
}`),
		},
	}
}

// toolArgs decodes the call arguments into out.
func toolArgs(call llm.ToolCall, out any) error {
	args := string(call.Arguments)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %v", err)
	}
	return nil
}

// Exec runs one tool call. The returned error is reserved for
// infrastructure trouble (sandboxd unreachable); every model-level
// mistake comes back as Result{OK:false}.
func (r *Registry) Exec(ctx context.Context, call llm.ToolCall) (Result, error) {
	switch call.Name {
	case "list_files":
		return r.listFiles(ctx, call)
	case "read_file":
		return r.readFile(ctx, call)
	case "write_file":
		return r.writeFile(ctx, call)
	case "edit_file":
		return r.editFile(ctx, call)
	case "search":
		return r.search(ctx, call)
	case "run_command":
		return r.runCommand(ctx, call)
	case "check_preview":
		return r.checkPreview(ctx, call)
	case "finish":
		var a struct {
			Summary string `json:"summary"`
		}
		if err := toolArgs(call, &a); err != nil {
			return badRequest(err.Error()), nil
		}
		summary := strings.TrimSpace(a.Summary)
		if summary == "" {
			return badRequest("finish requires a non-empty summary"), nil
		}
		if len([]rune(summary)) > 4000 {
			summary = string([]rune(summary)[:4000])
		}
		return Result{OK: true, Text: summary, ExitCode: -1}, nil
	default:
		return badRequest("unknown tool " + call.Name), nil
	}
}

func badRequest(msg string) Result {
	return Result{OK: false, Text: msg, ExitCode: -1, ErrCode: "bad_tool_args"}
}

// resolve validates a tool path and returns (relative-to-/home/sandbox,
// absolute-in-container). Blocked before any sandboxd call (plan §6).
func (r *Registry) resolve(p string, forWrite bool) (string, string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "."
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") ||
		strings.HasPrefix(p, "~") || hasDotDot(p) {
		return "", "", fmt.Errorf("%w: %q (paths are relative to the project directory)",
			ErrBlockedPath, p)
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", fmt.Errorf("%w: %q", ErrBlockedPath, p)
	}
	if forWrite && clean == "." {
		return "", "", fmt.Errorf("%w: refusing to overwrite the whole project directory", ErrBlockedPath)
	}
	rel := clean
	if r.relPrefix != "" && r.relPrefix != "." {
		rel = r.relPrefix + "/" + clean
	}
	abs := path.Join(r.projectDir, clean)
	return rel, abs, nil
}

func hasDotDot(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// --- sandboxd files helpers (shared with the workspace builder) -------

// listResp is the files-API listing envelope.
type listResp struct {
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// Entry is one row of a files-API listing.
type Entry struct {
	Path string `json:"path"` // relative to /home/sandbox
	Type string `json:"type"` // "file" | "dir"
	Size int64  `json:"size,omitempty"`
}

// List walks one directory of the sandbox through the files API.
// relPath is relative to /home/sandbox (e.g. "workspace/app").
func List(ctx context.Context, sbx *sandboxd.Client, sandboxID, relPath string, maxEntries int) ([]Entry, bool, error) {
	if maxEntries <= 0 {
		maxEntries = listMaxEntries
	}
	q := url.Values{}
	q.Set("path", relPath)
	q.Set("recursive", "true")
	q.Set("max_entries", fmt.Sprint(maxEntries))
	var out listResp
	if err := sbx.RawDecode(ctx, "GET",
		"/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files?"+q.Encode(), nil, &out); err != nil {
		return nil, false, err
	}
	return out.Entries, out.Truncated, nil
}

// readResp is the files-API content envelope.
type readResp struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Encoding  string `json:"encoding"` // utf-8 | base64
	Data      string `json:"data"`
	Truncated bool   `json:"truncated"`
}

// Read returns the raw bytes of one file (relPath relative to
// /home/sandbox). The files API caps a read at 2 MiB.
func Read(ctx context.Context, sbx *sandboxd.Client, sandboxID, relPath string) ([]byte, int64, error) {
	q := url.Values{}
	q.Set("path", relPath)
	var out readResp
	if err := sbx.RawDecode(ctx, "GET",
		"/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/content?"+q.Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	if out.Encoding == "base64" {
		raw, err := base64.StdEncoding.DecodeString(out.Data)
		if err != nil {
			return nil, out.Size, fmt.Errorf("decode base64 body: %w", err)
		}
		return raw, out.Size, nil
	}
	return []byte(out.Data), out.Size, nil
}

// Write puts bytes at relPath, optionally gated on the sha256 the
// caller read (if_match — the files API refuses a stale overwrite).
func Write(ctx context.Context, sbx *sandboxd.Client, sandboxID, relPath string, data []byte, ifMatch string) error {
	q := url.Values{}
	q.Set("path", relPath)
	if ifMatch != "" {
		q.Set("if_match", ifMatch)
	}
	_, err := sbx.Raw(ctx, "PUT",
		"/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/content?"+q.Encode(), data,
		"application/octet-stream")
	return err
}

// --- the tools ---------------------------------------------------------

func (r *Registry) listFiles(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Path  string `json:"path"`
		Depth *int   `json:"depth"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	rel, _, err := r.resolve(a.Path, false)
	if err != nil {
		return blocked(err), nil
	}
	depth := listMaxDepth
	if a.Depth != nil {
		depth = *a.Depth
		if depth < 1 {
			depth = 1
		}
		if depth > listMaxDepth {
			depth = listMaxDepth
		}
	}
	entries, truncated, err := List(ctx, r.sbx, r.sandboxID, rel, listMaxEntries)
	if err != nil {
		return Result{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	var b strings.Builder
	fmt.Fprintf(&b, "listing %s (depth %d)", r.display(a.Path), depth)
	if truncated {
		b.WriteString(" — TRUNCATED at 2000 entries")
	}
	b.WriteString("\n")
	shown := 0
	omitted := false
	for i, e := range entries {
		if leaf(e.Path) == ".next" || leaf(e.Path) == ".venv" || leaf(e.Path) == ".vite" {
			continue
		}
		d := depthOf(e.Path, rel)
		if d > depth {
			continue
		}
		if shown >= listMaxEntries {
			omitted = true
			break
		}
		indent := strings.Repeat("  ", max(d-1, 0))
		if e.Type == "dir" && d == depth && hasChildren(entries, i, e.Path) {
			// The tree continues below the cut: one placeholder line.
			fmt.Fprintf(&b, "%s%s/…\n", indent, leaf(e.Path))
		} else if e.Type == "dir" {
			fmt.Fprintf(&b, "%s%s/\n", indent, leaf(e.Path))
		} else {
			fmt.Fprintf(&b, "%s%s (%s)\n", indent, leaf(e.Path), humanBytes(e.Size))
		}
		shown++
	}
	if omitted {
		b.WriteString("… (more entries omitted)\n")
	}
	if shown == 0 && !truncated {
		b.WriteString("(empty)\n")
	}
	return Result{OK: true, Text: b.String(), ExitCode: -1}, nil
}

// hasChildren reports whether any entry after i lives under dirPath.
func hasChildren(entries []Entry, i int, dirPath string) bool {
	if i+1 >= len(entries) {
		return false
	}
	next := entries[i+1].Path
	return next == dirPath || strings.HasPrefix(next, dirPath+"/")
}

// display shows the path the MODEL asked for (not the prefixed one).
func (r *Registry) display(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return "."
	}
	return path.Clean(p)
}

func depthOf(entryPath, base string) int {
	rel := strings.TrimPrefix(entryPath, base)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return 1
	}
	return 1 + strings.Count(rel, "/")
}

func leaf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

func parentOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[:i]
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	}
}

func blocked(err error) Result {
	return Result{OK: false, Text: err.Error(), ExitCode: -1, ErrCode: "blocked_path"}
}

func (r *Registry) readFile(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	rel, _, err := r.resolve(a.Path, false)
	if err != nil {
		return blocked(err), nil
	}
	data, size, err := Read(ctx, r.sbx, r.sandboxID, rel)
	if err != nil {
		return Result{}, err
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return Result{
			OK:       true,
			Text:     fmt.Sprintf("%s is binary, %d bytes — not shown", r.display(a.Path), size),
			ExitCode: -1,
		}, nil
	}
	lines := splitLines(data)
	first := a.Offset
	if first < 1 {
		first = 1
	}
	if first > len(lines) {
		return Result{OK: true, ExitCode: -1, Text: fmt.Sprintf(
			"%s has %d lines; offset %d is past the end", r.display(a.Path), len(lines), a.Offset)}, nil
	}
	limit := a.Limit
	if limit <= 0 {
		limit = readMaxLines
	}
	if limit > readMaxLines {
		limit = readMaxLines
	}
	last := min(first+limit-1, len(lines))

	var b strings.Builder
	for n := first; n <= last; n++ {
		fmt.Fprintf(&b, "%6d| %s\n", n, truncateRunes(lines[n-1], readLineMaxRunes))
		if b.Len() > readMaxBytes {
			b.WriteString("… [read_file output truncated at 128 KiB]\n")
			last = n
			break
		}
	}
	fmt.Fprintf(&b, "(%d lines total; showing %d..%d)", len(lines), first, last)
	if last < len(lines) && a.Limit <= 0 {
		b.WriteString(" — use offset to read more")
	}
	return Result{OK: true, Text: b.String(), ExitCode: -1}, nil
}

// splitLines splits without keeping a trailing empty element for the
// final newline.
func splitLines(data []byte) []string {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func (r *Registry) writeFile(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	rel, _, err := r.resolve(a.Path, true)
	if err != nil {
		return blocked(err), nil
	}
	data := []byte(a.Content)
	if len(data) > writeMaxBytes {
		return Result{OK: false, ExitCode: -1, ErrCode: "too_large",
			Text: fmt.Sprintf("write_file content is %d bytes, the limit is 1 MiB", len(data))}, nil
	}
	if err := Write(ctx, r.sbx, r.sandboxID, rel, data, ""); err != nil {
		return r.filesErr(err)
	}
	return Result{OK: true, ExitCode: -1,
		Text: fmt.Sprintf("wrote %s (%d bytes)", r.display(a.Path), len(data))}, nil
}

// filesErr distinguishes a model-visible files-API error (missing
// file, if_match race) from a sandboxd transport failure. Call it as
// `return r.filesErr(err)` from the two-value returns.
func (r *Registry) filesErr(err error) (Result, error) {
	var api *sandboxd.APIError
	if errors.As(err, &api) {
		return Result{OK: false, ExitCode: -1, ErrCode: "files_error",
			Text: "files API: " + api.Message}, nil
	}
	return Result{}, err
}

func (r *Registry) editFile(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old"`
		New        string `json:"new"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	rel, _, err := r.resolve(a.Path, true)
	if err != nil {
		return blocked(err), nil
	}
	if a.Old == "" {
		return badRequest("edit_file: old must not be empty"), nil
	}
	data, _, err := Read(ctx, r.sbx, r.sandboxID, rel)
	if err != nil {
		return r.filesErr(err)
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return Result{OK: false, ExitCode: -1, ErrCode: "binary_file",
			Text: fmt.Sprintf("%s is binary; edit_file works on text files only", r.display(a.Path))}, nil
	}
	content := string(data)
	count := strings.Count(content, a.Old)
	if count == 0 {
		return Result{OK: false, ExitCode: -1, ErrCode: "old_not_found",
			Text: fmt.Sprintf("old text not found in %s — nothing changed", r.display(a.Path))}, nil
	}
	if count > 1 && !a.ReplaceAll {
		return Result{OK: false, ExitCode: -1, ErrCode: "old_not_unique",
			Text: fmt.Sprintf("old text appears %d times in %s; pass replace_all=true or give more context — nothing changed",
				count, r.display(a.Path))}, nil
	}
	if a.ReplaceAll {
		content = strings.ReplaceAll(content, a.Old, a.New)
	} else {
		content = strings.Replace(content, a.Old, a.New, 1)
	}
	sum := sha256.Sum256(data)
	if err := Write(ctx, r.sbx, r.sandboxID, rel, []byte(content), hex.EncodeToString(sum[:])); err != nil {
		return r.filesErr(err)
	}
	replaced := 1
	if a.ReplaceAll {
		replaced = count
	}
	return Result{OK: true, ExitCode: -1,
		Text: fmt.Sprintf("edited %s: %d replacement(s)", r.display(a.Path), replaced)}, nil
}

func (r *Registry) search(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return badRequest("search requires a pattern"), nil
	}
	_, abs, err := r.resolve(a.Path, false)
	if err != nil {
		return blocked(err), nil
	}
	searchPath := abs
	if a.Path == "" || a.Path == "." {
		searchPath = "."
	}
	var cmd []string
	if r.hasRg {
		cmd = []string{"rg", "--json", "-m", fmt.Sprint(searchMaxHits), "--no-messages"}
		if a.Glob != "" {
			cmd = append(cmd, "-g", a.Glob)
		}
		cmd = append(cmd, "--", a.Pattern, searchPath)
	} else {
		// grep fallback (plan §6): -H prints the file, -n the line.
		cmd = []string{"grep", "-rnH", "--binary-files=without-match", "-m", fmt.Sprint(searchMaxHits)}
		if a.Glob != "" {
			cmd = append(cmd, "--include", a.Glob)
		}
		cmd = append(cmd, "--", a.Pattern, searchPath)
	}
	res, err := r.exec(ctx, cmd, r.projectDir, nil, 60, 1<<20)
	if err != nil {
		return Result{}, err
	}
	hits := parseSearch(res.Stdout, r.hasRg)
	if len(hits) == 0 {
		if res.ExitCode == 2 && res.Stderr != "" {
			return Result{OK: false, ExitCode: res.ExitCode, ErrCode: "search_error",
				Text: "search failed: " + firstLine(res.Stderr)}, nil
		}
		return Result{OK: true, ExitCode: res.ExitCode, Text: "no matches"}, nil
	}
	var b strings.Builder
	for i, h := range hits {
		if i >= searchMaxHits {
			b.WriteString("… (more matches omitted)\n")
			break
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", h.Path, h.Line, truncateRunes(h.Text, searchLineRunes))
	}
	return Result{OK: true, ExitCode: res.ExitCode, Text: b.String()}, nil
}

type searchHit struct {
	Path string
	Line int
	Text string
}

// parseSearch reads rg --json events (and plain grep output) into
// hits. Unknown lines are ignored.
func parseSearch(out string, rg bool) []searchHit {
	var hits []searchHit
	if !rg {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			// path:line:text — the path may contain colons; the first
			// two fields split it.
			p, n, text, ok := cutNumber(line)
			if !ok {
				continue
			}
			hits = append(hits, searchHit{Path: p, Line: n, Text: text})
		}
		return hits
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue // rg emits one JSON event per line; skip junk
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path struct {
					Text string `json:"text"`
				} `json:"path"`
				LineNumber int `json:"line_number"`
				Lines      struct {
					Text string `json:"text"`
				} `json:"lines"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // tolerate keep-alive junk
		}
		if ev.Type != "match" {
			continue
		}
		hits = append(hits, searchHit{
			Path: ev.Data.Path.Text,
			Line: ev.Data.LineNumber,
			Text: strings.TrimRight(ev.Data.Lines.Text, "\n\r"),
		})
	}
	return hits
}

// cutNumber splits grep -nH output "path:line:text" into its three
// fields (the path itself may contain colons).
func cutNumber(s string) (path string, n int, text string, ok bool) {
	i := strings.Index(s, ":")
	if i < 0 {
		return "", 0, "", false
	}
	rest := s[i+1:]
	j := strings.Index(rest, ":")
	if j < 0 {
		return "", 0, "", false
	}
	for _, c := range rest[:j] {
		if c < '0' || c > '9' {
			return "", 0, "", false
		}
		n = n*10 + int(c-'0')
	}
	return s[:i], n, rest[j+1:], true
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func (r *Registry) runCommand(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Command  string `json:"command"`
		Cwd      string `json:"cwd"`
		TimeoutS int    `json:"timeout_s"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return badRequest("run_command requires a command"), nil
	}
	cwd := r.projectDir
	if a.Cwd != "" && a.Cwd != "." {
		_, abs, err := r.resolve(a.Cwd, false)
		if err != nil {
			return blocked(err), nil
		}
		cwd = abs
	}
	timeout := a.TimeoutS
	if timeout <= 0 {
		timeout = commandDefaultTO
	}
	if timeout > commandMaxTimeout {
		timeout = commandMaxTimeout
	}
	res, err := r.exec(ctx, []string{"bash", "-lc", a.Command}, cwd, nil, timeout, 256<<10)
	if err != nil {
		return Result{}, err
	}
	out := combineOutput(res.Stdout, res.Stderr)
	shown := out
	truncated := false
	if len(shown) > outputLLMBytes {
		shown = trimToLLM(out)
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d", res.ExitCode)
	if res.TimedOut {
		b.WriteString(" timed_out=true")
	}
	if res.Truncated {
		b.WriteString(" output_capped=true")
	}
	b.WriteString("\n")
	b.WriteString(shown)
	if truncated {
		b.WriteString("\n[output truncated for the model: full output was " +
			humanBytes(int64(len(out))) + "]")
	}
	return Result{OK: res.ExitCode == 0 && !res.TimedOut, Text: b.String(),
		ExitCode: res.ExitCode, TimedOut: res.TimedOut, Truncated: truncated}, nil
}

// combineOutput merges stdout and stderr the way a terminal shows it.
func combineOutput(stdout, stderr string) string {
	stdout = strings.TrimRight(stdout, "\n")
	stderr = strings.TrimRight(stderr, "\n")
	switch {
	case stdout == "" && stderr == "":
		return "(no output)"
	case stderr == "":
		return stdout
	case stdout == "":
		return "[stderr]\n" + stderr
	default:
		return stdout + "\n[stderr]\n" + stderr
	}
}

// trimToLLM keeps the head and tail 8 KiB of a long output (plan §6).
func trimToLLM(out string) string {
	head := out[:min(len(out), outputHalfBytes)]
	tail := out[max(0, len(out)-outputHalfBytes):]
	hidden := len(out) - len(head) - len(tail)
	return cutAtRune(head) + fmt.Sprintf("\n[... %d bytes hidden ...]\n", hidden) + cutAtRune(tail)
}

// cutAtRune backs off to the last valid UTF-8 rune boundary.
func cutAtRune(s string) string {
	for i := 0; i < 4 && i < len(s); i++ {
		if utf8.ValidString(s[:len(s)-i]) {
			return s[:len(s)-i]
		}
	}
	return s
}

func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

func (r *Registry) checkPreview(ctx context.Context, call llm.ToolCall) (Result, error) {
	var a struct {
		Path string `json:"path"`
		Port int    `json:"port"`
	}
	if err := toolArgs(call, &a); err != nil {
		return badRequest(err.Error()), nil
	}
	if r.previewURL == "" && a.Port <= 0 {
		return Result{OK: false, ExitCode: -1, ErrCode: "no_preview_port",
			Text: "the sandbox has no public port to check"}, nil
	}
	target := r.previewURL
	if a.Port > 0 {
		target = fmt.Sprintf("http://127.0.0.1:%d", a.Port)
	}
	p := a.Path
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cmd := []string{"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
		"--max-time", fmt.Sprint(previewMaxSeconds), target + p}
	res, err := r.exec(ctx, cmd, r.projectDir, nil, previewMaxSeconds+10, 4<<10)
	if err != nil {
		return Result{}, err
	}
	code := strings.TrimSpace(res.Stdout)
	ok := res.ExitCode == 0 && (strings.HasPrefix(code, "2") || strings.HasPrefix(code, "3"))
	return Result{OK: ok, ExitCode: res.ExitCode, Text: "HTTP " + code}, nil
}

// exec is the sandboxd exec v2 call every shell tool goes through.
func (r *Registry) exec(ctx context.Context, cmd []string, cwd string, env map[string]string, timeoutS int, maxOut int64) (sandboxd.ExecResult, error) {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutS+15)*time.Second)
	defer cancel()
	return r.sbx.Exec(cctx, r.sandboxID, sandboxd.ExecInput{
		Cmd:            cmd,
		Cwd:            cwd,
		Env:            env,
		TimeoutS:       timeoutS,
		MaxOutputBytes: maxOut,
	})
}

// PreviewArgs builds the tool_call event payload: write_file carries
// NO content on the wire (plan §9.3 — path + bytes only); other tools
// show their arguments cut at 2 KiB.
func PreviewArgs(call llm.ToolCall) json.RawMessage {
	if call.Name == "write_file" {
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		_ = json.Unmarshal(call.Arguments, &a)
		raw, _ := json.Marshal(map[string]any{
			"path":  a.Path,
			"bytes": len(a.Content),
		})
		return raw
	}
	raw := []byte(call.Arguments)
	const capBytes = 2 << 10
	if len(raw) > capBytes {
		out, _ := json.Marshal(map[string]any{
			"args_bytes":   len(raw),
			"args_preview": cutAtRune(string(raw[:capBytes])),
		})
		return out
	}
	return raw
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
