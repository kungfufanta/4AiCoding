// gitops.go — the GitOps side of node capacity changes. The console never
// changes a node directly: an admin's request becomes a pull request that
// edits the node's Helm values file in the deploy repo. A human reviews and
// merges it, and the repo's deploy workflow applies it (helm upgrade). Git
// is the audit trail; reverting the PR is the rollback.
//
// The control plane only needs a GitHub token scoped to the one deploy
// repo with Contents + Pull requests write. It holds no kubeconfig and no
// hypervisor credentials.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// GitOpsConfig is the deploy repo the capacity change requests target.
type GitOpsConfig struct {
	// Repo is "owner/name" on GitHub.
	Repo string
	// Token is a GitHub token with Contents + Pull requests write on Repo
	// only. Empty = preview only (the console shows the edit to commit by
	// hand); reads of a public repo still work without it.
	Token string
	// Base is the branch PRs target (default "main").
	Base string
	// APIURL is the GitHub API base (default https://api.github.com).
	APIURL string
	// NodeFiles maps a node id to the Helm values file holding its
	// `capacity:` block, e.g. {"k3s-1": "deploy/k3s-1-values.yaml"}.
	NodeFiles map[string]string
	// Max is the hard ceiling of a request, whatever the host reports.
	Max store.Capacity
	// HTTP is the client (default: 15 s timeout).
	HTTP *http.Client
}

// GitOpsFromEnv reads SANDBOXD_GITOPS_* (see docs/NODE_RESOURCES.md).
// Returns nil when no node is mapped to a values file.
func GitOpsFromEnv() *GitOpsConfig {
	files := parseNodeFiles(os.Getenv("SANDBOXD_GITOPS_NODE_FILES"))
	if len(files) == 0 {
		return nil
	}
	g := &GitOpsConfig{
		Repo:      strings.TrimSpace(os.Getenv("SANDBOXD_GITOPS_REPO")),
		Token:     strings.TrimSpace(os.Getenv("SANDBOXD_GITOPS_TOKEN")),
		Base:      strings.TrimSpace(os.Getenv("SANDBOXD_GITOPS_BASE")),
		APIURL:    strings.TrimSpace(os.Getenv("SANDBOXD_GITOPS_API_URL")),
		NodeFiles: files,
	}
	if v, err := strconv.ParseFloat(os.Getenv("SANDBOXD_GITOPS_MAX_CPUS"), 64); err == nil && v > 0 {
		g.Max.CPUs = v
	}
	if v, err := strconv.Atoi(os.Getenv("SANDBOXD_GITOPS_MAX_MEMORY_MB")); err == nil && v > 0 {
		g.Max.MemoryMB = v
	}
	if v, err := strconv.Atoi(os.Getenv("SANDBOXD_GITOPS_MAX_DISK_MB")); err == nil && v > 0 {
		g.Max.DiskMB = v
	}
	return g
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var valuesPathRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+\.ya?ml$`)

// parseNodeFiles reads "node=path,node=path". Entries with a bad node id
// or path (absolute, "..") are dropped.
func parseNodeFiles(v string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		node, path, ok := strings.Cut(strings.TrimSpace(part), "=")
		node, path = strings.TrimSpace(node), strings.TrimSpace(path)
		if !ok || !store.ValidNodeID(node) || !valuesPathRe.MatchString(path) ||
			strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			continue
		}
		out[node] = path
	}
	return out
}

func (g *GitOpsConfig) fileFor(nodeID string) string {
	if g == nil {
		return ""
	}
	return g.NodeFiles[nodeID]
}

// canOpenPR reports whether PRs can be opened (not just previewed).
func (g *GitOpsConfig) canOpenPR() bool {
	return g != nil && g.Token != "" && repoRe.MatchString(g.Repo)
}

func (g *GitOpsConfig) base() string {
	if g.Base == "" {
		return "main"
	}
	return g.Base
}

func (g *GitOpsConfig) max() store.Capacity {
	m := g.Max
	if m.CPUs <= 0 {
		m.CPUs = 256
	}
	if m.MemoryMB <= 0 {
		m.MemoryMB = 1 << 20 // 1 TiB
	}
	if m.DiskMB <= 0 {
		m.DiskMB = 64 << 20 // 64 TiB
	}
	return m
}

// --- values file edit -----------------------------------------------------

type valuesCapacity struct {
	Capacity struct {
		CPUs     float64 `yaml:"cpus"`
		MemoryMB int     `yaml:"memoryMB"`
		DiskMB   int     `yaml:"diskMB"`
	} `yaml:"capacity"`
}

func readValuesCapacity(src []byte) (store.Capacity, error) {
	var v valuesCapacity
	if err := yaml.Unmarshal(src, &v); err != nil {
		return store.Capacity{}, fmt.Errorf("parse values file: %w", err)
	}
	return store.Capacity{CPUs: v.Capacity.CPUs, MemoryMB: v.Capacity.MemoryMB, DiskMB: v.Capacity.DiskMB}, nil
}

var (
	capacityHeadRe = regexp.MustCompile(`^capacity:\s*(#.*)?$`)
	capacityKeyRe  = regexp.MustCompile(`^(\s+)(cpus|memoryMB|diskMB):(\s*)([^#\s][^#]*?|)(\s*#.*)?$`)
)

// setValuesCapacity rewrites the top-level `capacity:` block of a Helm
// values file, line by line so comments and layout survive. It verifies
// the result: the capacity parses to want and nothing else changed.
func setValuesCapacity(src []byte, want store.Capacity) ([]byte, error) {
	text := string(src)
	trailingNL := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	vals := map[string]string{
		"cpus":     strconv.FormatFloat(want.CPUs, 'f', -1, 64),
		"memoryMB": strconv.Itoa(want.MemoryMB),
		"diskMB":   strconv.Itoa(want.DiskMB),
	}
	order := []string{"cpus", "memoryMB", "diskMB"}

	head := -1
	for i, l := range lines {
		if capacityHeadRe.MatchString(l) {
			head = i
			break
		}
	}
	if head < 0 {
		block := []string{"", "capacity:"}
		for _, k := range order {
			block = append(block, "  "+k+": "+vals[k])
		}
		lines = append(lines, block...)
	} else {
		end := head + 1
		for end < len(lines) {
			l := lines[end]
			if strings.TrimSpace(l) != "" && l[0] != ' ' && l[0] != '\t' {
				break
			}
			end++
		}
		indent := "  "
		seen := map[string]bool{}
		lastKey := head
		for i := head + 1; i < end; i++ {
			m := capacityKeyRe.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			if !seen[m[2]] && len(seen) == 0 {
				indent = m[1]
			}
			sep := m[3]
			if sep == "" {
				sep = " "
			}
			lines[i] = m[1] + m[2] + ":" + sep + vals[m[2]] + m[5]
			seen[m[2]] = true
			lastKey = i
		}
		var missing []string
		for _, k := range order {
			if !seen[k] {
				missing = append(missing, indent+k+": "+vals[k])
			}
		}
		if len(missing) > 0 {
			rest := append([]string{}, lines[lastKey+1:]...)
			lines = append(append(lines[:lastKey+1], missing...), rest...)
		}
	}
	out := strings.Join(lines, "\n")
	if trailingNL || head < 0 {
		out += "\n"
	}

	// Verify: capacity is what we asked for and the rest is untouched.
	got, err := readValuesCapacity([]byte(out))
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("values edit verification failed: got %+v, want %+v", got, want)
	}
	var before, after map[string]any
	if err := yaml.Unmarshal(src, &before); err != nil {
		return nil, fmt.Errorf("parse values file: %w", err)
	}
	if err := yaml.Unmarshal([]byte(out), &after); err != nil {
		return nil, fmt.Errorf("parse edited values file: %w", err)
	}
	delete(before, "capacity")
	delete(after, "capacity")
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if !bytes.Equal(b1, b2) {
		return nil, errors.New("values edit verification failed: keys outside capacity changed")
	}
	return []byte(out), nil
}

// lineDiff is a minimal changed-lines view for the console preview.
func lineDiff(a, b []byte) []string {
	al := strings.Split(strings.TrimSuffix(string(a), "\n"), "\n")
	bl := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	var out []string
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && al[i] == bl[j]:
			i++
			j++
		case len(bl)-j > len(al)-i: // inserted line
			out = append(out, "+ "+bl[j])
			j++
		case len(al)-i > len(bl)-j:
			out = append(out, "- "+al[i])
			i++
		default:
			out = append(out, "- "+al[i], "+ "+bl[j])
			i++
			j++
		}
	}
	return out
}

// --- GitHub REST client -----------------------------------------------------

type ghError struct {
	Status int
	Body   string
}

func (e *ghError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300]
	}
	return fmt.Sprintf("github returned %d: %s", e.Status, b)
}

func (g *GitOpsConfig) do(ctx context.Context, method, path string, body, out any) error {
	api := g.APIURL
	if api == "" {
		api = "https://api.github.com"
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(api, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "sandboxd-gitops")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &ghError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (g *GitOpsConfig) repoPath() string {
	return "/repos/" + g.Repo
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// getFile returns a file's content and blob sha at ref.
func (g *GitOpsConfig) getFile(ctx context.Context, path, ref string) ([]byte, string, error) {
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		SHA      string `json:"sha"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/contents/"+escapePath(path)+"?ref="+url.QueryEscape(ref), nil, &out); err != nil {
		return nil, "", err
	}
	if out.Encoding != "base64" {
		return nil, "", fmt.Errorf("unexpected content encoding %q", out.Encoding)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, "", err
	}
	return b, out.SHA, nil
}

type ghPR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	MergedAt  string `json:"merged_at"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// openPR creates branch from base, commits content to path there and
// opens a PR. Returns the PR.
func (g *GitOpsConfig) openPR(ctx context.Context, branch, path, fileSHA, message string, content []byte, title, body string) (*ghPR, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/git/ref/heads/"+escapePath(g.base()), nil, &ref); err != nil {
		return nil, fmt.Errorf("read base branch: %w", err)
	}
	if err := g.do(ctx, http.MethodPost, g.repoPath()+"/git/refs", map[string]string{
		"ref": "refs/heads/" + branch, "sha": ref.Object.SHA,
	}, nil); err != nil {
		return nil, fmt.Errorf("create branch: %w", err)
	}
	if err := g.do(ctx, http.MethodPut, g.repoPath()+"/contents/"+escapePath(path), map[string]string{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"sha":     fileSHA,
		"branch":  branch,
	}, nil); err != nil {
		return nil, fmt.Errorf("commit values file: %w", err)
	}
	var pr ghPR
	if err := g.do(ctx, http.MethodPost, g.repoPath()+"/pulls", map[string]any{
		"title": title, "head": branch, "base": g.base(), "body": body,
	}, &pr); err != nil {
		return nil, fmt.Errorf("open pull request: %w", err)
	}
	return &pr, nil
}

// listPRs returns the recent PRs whose head branch starts with prefix.
func (g *GitOpsConfig) listPRs(ctx context.Context, prefix string) ([]ghPR, error) {
	var all []ghPR
	if err := g.do(ctx, http.MethodGet, g.repoPath()+"/pulls?state=all&sort=created&direction=desc&per_page=50", nil, &all); err != nil {
		return nil, err
	}
	out := []ghPR{}
	for _, p := range all {
		if strings.HasPrefix(p.Head.Ref, prefix) {
			out = append(out, p)
		}
	}
	return out, nil
}
