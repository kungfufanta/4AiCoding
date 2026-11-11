// v1_node_change.go — capacity change requests (admin only, roles.go).
//
// POST /v1/nodes/{id}/change-requests {cpus, memory_mb, disk_mb, reason,
// dry_run} edits the `capacity:` block of the node's Helm values file:
// dry_run returns the preview; otherwise a branch + commit + pull request
// is opened on the deploy repo (gitops.go). Nothing is applied until a
// human merges the PR and the deploy workflow runs helm upgrade.
//
// GET /v1/nodes/{id}/change-requests lists the node's recent PRs.
//
// Guard rails: a request cannot advertise more than the host the node runs
// on actually has (its latest telemetry), nor more than the configured
// ceilings. Growing the machine (VM vCPU/RAM/disk) comes first; the change
// request only makes the advertised capacity follow.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/hosttelemetry"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

type nodeChangeReq struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	DiskMB   int     `json:"disk_mb"`
	Reason   string  `json:"reason,omitempty"`
	DryRun   bool    `json:"dry_run,omitempty"`
}

type nodeChangePR struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"` // open | closed | merged
	Branch    string `json:"branch"`
	CreatedAt string `json:"created_at,omitempty"`
	MergedAt  string `json:"merged_at,omitempty"`
}

type nodeChangeResp struct {
	NodeID   string         `json:"node_id"`
	DryRun   bool           `json:"dry_run"`
	Repo     string         `json:"repo,omitempty"`
	Base     string         `json:"base"`
	File     string         `json:"file"`
	Current  v1NodeCapacity `json:"current"`
	Proposed v1NodeCapacity `json:"proposed"`
	Diff     []string       `json:"diff"`
	Content  string         `json:"content,omitempty"`
	// CanOpenPR is false when no GitOps token is configured: the preview
	// is then the change to commit by hand.
	CanOpenPR bool          `json:"can_open_pr"`
	PR        *nodeChangePR `json:"pr,omitempty"`
}

// validateNodeChange checks a request against the ceilings and the host's
// real resources (telemetry). Returns a user-facing message, "" when ok.
func validateNodeChange(req nodeChangeReq, max store.Capacity, tel *hosttelemetry.Snapshot) string {
	switch {
	case req.CPUs <= 0 || math.IsNaN(req.CPUs) || math.IsInf(req.CPUs, 0):
		return "cpus must be > 0"
	case math.Round(req.CPUs*100) != req.CPUs*100:
		return "cpus has at most 2 decimals"
	case req.MemoryMB < 256:
		return "memory_mb must be at least 256"
	case req.DiskMB < 0:
		return "disk_mb must be >= 0 (0 = unreported)"
	case req.CPUs > max.CPUs:
		return fmt.Sprintf("cpus above the ceiling %g", max.CPUs)
	case req.MemoryMB > max.MemoryMB:
		return fmt.Sprintf("memory_mb above the ceiling %d", max.MemoryMB)
	case req.DiskMB > max.DiskMB:
		return fmt.Sprintf("disk_mb above the ceiling %d", max.DiskMB)
	case len(req.Reason) > 1000:
		return "reason too long (max 1000 chars)"
	}
	if tel != nil {
		if tel.CPU.Cores > 0 && req.CPUs > float64(tel.CPU.Cores) {
			return fmt.Sprintf("cpus %g is more than the host has (%d cores): grow the machine first", req.CPUs, tel.CPU.Cores)
		}
		if tel.Memory.TotalMB > 0 && int64(req.MemoryMB) > tel.Memory.TotalMB {
			return fmt.Sprintf("memory_mb %d is more than the host has (%d MiB): grow the machine first", req.MemoryMB, tel.Memory.TotalMB)
		}
		if tel.Disk.TotalMB > 0 && int64(req.DiskMB) > tel.Disk.TotalMB {
			return fmt.Sprintf("disk_mb %d is more than the host disk (%d MiB): grow the disk first", req.DiskMB, tel.Disk.TotalMB)
		}
	}
	return ""
}

func (s *Server) v1CreateNodeChange(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.Store.GetNode(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	g := s.GitOps
	path := g.fileFor(n.NodeID)
	if path == "" {
		writeV1Err(w, http.StatusConflict, "gitops_not_configured",
			"no values file is mapped to this node (SANDBOXD_GITOPS_NODE_FILES)")
		return
	}
	var req nodeChangeReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	var tel *hosttelemetry.Snapshot
	if len(n.Telemetry) > 0 {
		tel, _ = decodeTelemetry(n.Telemetry)
	}
	if msg := validateNodeChange(req, g.max(), tel); msg != "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	if !g.canOpenPR() && !req.DryRun {
		writeV1Err(w, http.StatusConflict, "gitops_not_configured",
			"no GitOps token is configured (SANDBOXD_GITOPS_TOKEN): preview with dry_run and commit the change by hand")
		return
	}
	if !repoRe.MatchString(g.Repo) {
		writeV1Err(w, http.StatusConflict, "gitops_not_configured", "SANDBOXD_GITOPS_REPO must be owner/name")
		return
	}

	src, fileSHA, err := g.getFile(r.Context(), path, g.base())
	if err != nil {
		writeV1Err(w, http.StatusBadGateway, "upstream", "read "+path+": "+err.Error())
		return
	}
	cur, err := readValuesCapacity(src)
	if err != nil {
		writeV1Err(w, http.StatusUnprocessableEntity, "invalid_values", err.Error())
		return
	}
	want := store.Capacity{CPUs: req.CPUs, MemoryMB: req.MemoryMB, DiskMB: req.DiskMB}
	if cur == want {
		writeV1Err(w, http.StatusConflict, "no_change", "the values file already has this capacity")
		return
	}
	out, err := setValuesCapacity(src, want)
	if err != nil {
		writeV1Err(w, http.StatusUnprocessableEntity, "invalid_values", err.Error())
		return
	}
	resp := nodeChangeResp{
		NodeID:    n.NodeID,
		DryRun:    req.DryRun,
		Repo:      g.Repo,
		Base:      g.base(),
		File:      path,
		Current:   v1NodeCapacity{CPUs: cur.CPUs, MemoryMB: cur.MemoryMB, DiskMB: cur.DiskMB},
		Proposed:  v1NodeCapacity{CPUs: want.CPUs, MemoryMB: want.MemoryMB, DiskMB: want.DiskMB},
		Diff:      lineDiff(src, out),
		Content:   string(out),
		CanOpenPR: g.canOpenPR(),
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	actor := auth.ActorFrom(r.Context())
	branch := fmt.Sprintf("capacity/%s-%d", n.NodeID, time.Now().Unix())
	title := fmt.Sprintf("capacity(%s): %g CPU, %d MiB RAM, %d MiB disk", n.NodeID, want.CPUs, want.MemoryMB, want.DiskMB)
	body := changePRBody(n, path, cur, want, req.Reason, actor.Name, tel)
	msg := title + "\n\nRequested by " + actor.Name + " from the sandboxd console."
	pr, err := g.openPR(r.Context(), branch, path, fileSHA, msg, out, title, body)
	if err != nil {
		writeV1Err(w, http.StatusBadGateway, "upstream", err.Error())
		return
	}
	resp.PR = toChangePR(*pr)
	s.auditAction(r, audit.Entry{Action: "node.change_request", Target: n.NodeID, Detail: map[string]any{
		"file": path, "pr": pr.Number, "url": pr.HTMLURL,
		"cpus": want.CPUs, "memory_mb": want.MemoryMB, "disk_mb": want.DiskMB, "reason": req.Reason,
	}})
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) v1ListNodeChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	g := s.GitOps
	out := map[string]any{
		"node_id":     id,
		"file":        g.fileFor(id),
		"can_open_pr": g.canOpenPR(),
		"requests":    []nodeChangePR{},
	}
	if g == nil || g.fileFor(id) == "" || !repoRe.MatchString(g.Repo) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out["repo"] = g.Repo
	out["base"] = g.base()
	// The values file's current capacity prefills the console form.
	if src, _, err := g.getFile(r.Context(), g.fileFor(id), g.base()); err == nil {
		if cur, err := readValuesCapacity(src); err == nil {
			out["current"] = v1NodeCapacity{CPUs: cur.CPUs, MemoryMB: cur.MemoryMB, DiskMB: cur.DiskMB}
		}
	}
	prs, err := g.listPRs(r.Context(), "capacity/"+id+"-")
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	list := make([]nodeChangePR, 0, len(prs))
	for _, p := range prs {
		list = append(list, *toChangePR(p))
	}
	out["requests"] = list
	writeJSON(w, http.StatusOK, out)
}

func toChangePR(p ghPR) *nodeChangePR {
	state := p.State
	if p.MergedAt != "" {
		state = "merged"
	}
	return &nodeChangePR{Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: state,
		Branch: p.Head.Ref, CreatedAt: p.CreatedAt, MergedAt: p.MergedAt}
}

func changePRBody(n *store.Node, path string, cur, want store.Capacity, reason, actor string, tel *hosttelemetry.Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Capacity change request for node `%s`, opened from the sandboxd console by **%s**.\n\n", n.NodeID, mdEscape(actor))
	if reason != "" {
		fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(mdEscape(reason), "\n", "\n> "))
	}
	fmt.Fprintf(&b, "File: `%s`\n\n", path)
	b.WriteString("| | current | requested |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| CPUs | %g | %g |\n", cur.CPUs, want.CPUs)
	fmt.Fprintf(&b, "| Memory (MiB) | %d | %d |\n", cur.MemoryMB, want.MemoryMB)
	fmt.Fprintf(&b, "| Disk (MiB) | %d | %d |\n\n", cur.DiskMB, want.DiskMB)
	if tel != nil {
		fmt.Fprintf(&b, "Host at request time (%s): %d cores (%.1f%% busy), %d/%d MiB RAM used, %d/%d MiB disk used.\n\n",
			tel.CollectedAt.UTC().Format(time.RFC3339), tel.CPU.Cores, tel.CPU.UsagePercent,
			tel.Memory.UsedMB, tel.Memory.TotalMB, tel.Disk.UsedMB, tel.Disk.TotalMB)
	}
	b.WriteString("This only changes the capacity the worker advertises to the scheduler. It cannot add physical CPU/RAM/disk: grow the machine first.\n\n")
	b.WriteString("Merging applies it through the deploy workflow (helm upgrade). Revert this PR to roll back.\n")
	return b.String()
}

// mdEscape keeps user text from injecting markdown links/mentions/HTML.
func mdEscape(s string) string {
	r := strings.NewReplacer("<", "&lt;", ">", "&gt;", "@", "@​", "[", "\\[", "]", "\\]", "`", "'")
	return r.Replace(s)
}
