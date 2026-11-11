package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// Console dashboard views: GET /v1/overview and GET /v1/monitoring. Both are
// read-only aggregations of data the API already exposes elsewhere, shaped
// for the console's Overview and Monitoring pages so the page needs one call
// instead of N.

// --- GET /v1/overview -----------------------------------------------

type overviewCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Stopped  int `json:"stopped"`
	Creating int `json:"creating"`
	Error    int `json:"error"`
}

type overviewUsers struct {
	Total     int `json:"total"`
	Admins    int `json:"admins"`
	Operators int `json:"operators"`
	Viewers   int `json:"viewers"`
	Disabled  int `json:"disabled"`
}

type overviewAudit struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type overviewNet struct {
	Total   int `json:"total"`
	Blocked int `json:"blocked"`
}

type overviewResp struct {
	Sandboxes     overviewCounts  `json:"sandboxes"`
	Users         *overviewUsers  `json:"users"` // admins only (user list is an admin route)
	PortsExposed  int             `json:"ports_exposed"`
	ApprovalsOpen int             `json:"approvals_open"`
	NetEvents24h  overviewNet     `json:"net_events_24h"`
	Recent        []v1Sandbox     `json:"recent"`
	Audit         []overviewAudit `json:"audit"` // admins only (audit is an admin route); [] otherwise
}

const overviewRecent = 8
const overviewAuditTail = 10

func (s *Server) v1Overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.Store.List(ctx)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	all := make([]v1Sandbox, 0, len(rows))
	for _, sb := range rows {
		all = append(all, s.v1SandboxFromRow(r, sb))
	}
	// M7b — sandboxes running on workers count too.
	remote, _ := s.remoteSandboxes(r)
	for _, raw := range remote {
		var v v1Sandbox
		if json.Unmarshal(raw, &v) == nil && v.ID != "" {
			all = append(all, v)
		}
	}

	out := overviewResp{Recent: []v1Sandbox{}, Audit: []overviewAudit{}}
	for _, sb := range all {
		out.Sandboxes.Total++
		switch sb.Status {
		case "running":
			out.Sandboxes.Running++
		case "stopped":
			out.Sandboxes.Stopped++
		case "creating", "pending", "starting":
			out.Sandboxes.Creating++
		case "error":
			out.Sandboxes.Error++
		}
		out.PortsExposed += len(sb.Ports)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
	if len(all) > overviewRecent {
		all = all[:overviewRecent]
	}
	out.Recent = append(out.Recent, all...)

	if pending, err := s.Store.ListApprovals(ctx, "pending", 200); err == nil {
		out.ApprovalsOpen = len(pending)
	}
	if s.NetLog != nil {
		out.NetEvents24h.Total, out.NetEvents24h.Blocked = s.NetLog.CountSince(time.Now().Add(-24 * time.Hour))
	}

	if auth.ActorFrom(ctx).Role == auth.RoleAdmin {
		if users, err := s.Store.ListUsers(ctx); err == nil {
			u := &overviewUsers{}
			for _, x := range users {
				u.Total++
				switch x.Role {
				case auth.RoleAdmin:
					u.Admins++
				case auth.RoleOperator:
					u.Operators++
				case auth.RoleViewer:
					u.Viewers++
				}
				if x.Disabled {
					u.Disabled++
				}
			}
			out.Users = u
		}
		if entries, err := s.Store.ListAudit(ctx, 0, overviewAuditTail, "", ""); err == nil {
			for _, e := range entries {
				actor := e.ActorName
				if actor == "" {
					actor = e.ActorKind
				}
				out.Audit = append(out.Audit, overviewAudit{
					At: time.Unix(e.At, 0).UTC().Format(time.RFC3339), Actor: actor,
					Action: e.Action, Target: e.Target, Detail: e.Detail,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /v1/monitoring ---------------------------------------------

type monSandbox struct {
	ID            string  `json:"id"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemUsageBytes int64   `json:"mem_usage_bytes"`
	MemLimitBytes int64   `json:"mem_limit_bytes"`
	PIDs          int     `json:"pids"`
}

type monFleet struct {
	Total      int     `json:"total"`
	Running    int     `json:"running"`
	AvgCPU     float64 `json:"avg_cpu"`
	MemUsedMB  int64   `json:"mem_used_mb"`
	MemLimitMB int64   `json:"mem_limit_mb"`
}

type monPoint struct {
	T     string  `json:"t"`
	CPU   float64 `json:"cpu"`
	MemMB int64   `json:"mem_mb"`
}

type monResp struct {
	Fleet     monFleet     `json:"fleet"`
	Sandboxes []monSandbox `json:"sandboxes"`
	History   []monPoint   `json:"history"`
}

// monHistory keeps the fleet samples taken by /v1/monitoring calls: at most
// one per monStep, for monWindow. It only fills while someone is looking
// (the console polls), so no background sampler costs anything when idle.
type monHistory struct {
	mu     sync.Mutex
	points []monPoint
	last   time.Time
}

const (
	monStep   = 10 * time.Second
	monWindow = 8 * time.Minute
)

var fleetHistory monHistory

func (h *monHistory) add(now time.Time, p monPoint) []monPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now.Sub(h.last) >= monStep {
		h.points = append(h.points, p)
		h.last = now
	}
	cut := now.Add(-monWindow).UTC().Format(time.RFC3339)
	i := 0
	for i < len(h.points) && h.points[i].T < cut {
		i++
	}
	h.points = h.points[i:]
	return append([]monPoint{}, h.points...)
}

func (s *Server) v1Monitoring(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.List(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := monResp{Sandboxes: []monSandbox{}}
	out.Fleet.Total = len(rows)
	var running []*store.Sandbox
	for _, sb := range rows {
		if sb.Status == "running" {
			running = append(running, sb)
		}
	}
	out.Fleet.Running = len(running)

	// Sample every running sandbox in parallel (docker stats takes ~1s each).
	if s.Docker != nil && len(running) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rr := r.WithContext(ctx)
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, sb := range running {
			wg.Add(1)
			go func(sb *store.Sandbox) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				m, err := s.sampleMetrics(rr, sb)
				if err != nil || m.Status != "running" {
					return
				}
				mu.Lock()
				out.Sandboxes = append(out.Sandboxes, monSandbox{
					ID: sb.ID, CPUPercent: m.Stats.CPUPercent, MemUsageBytes: m.Stats.MemUsageBytes,
					MemLimitBytes: m.Stats.MemLimitBytes, PIDs: m.Stats.PIDs,
				})
				mu.Unlock()
			}(sb)
		}
		wg.Wait()
	}
	sort.Slice(out.Sandboxes, func(i, j int) bool { return out.Sandboxes[i].ID < out.Sandboxes[j].ID })

	var cpu float64
	var used, limit int64
	for _, x := range out.Sandboxes {
		cpu += x.CPUPercent
		used += x.MemUsageBytes
		limit += x.MemLimitBytes
	}
	if n := len(out.Sandboxes); n > 0 {
		out.Fleet.AvgCPU = float64(int(cpu/float64(n)*10+0.5)) / 10
	}
	out.Fleet.MemUsedMB = used >> 20
	out.Fleet.MemLimitMB = limit >> 20

	now := time.Now()
	out.History = fleetHistory.add(now, monPoint{T: now.UTC().Format(time.RFC3339), CPU: out.Fleet.AvgCPU, MemMB: out.Fleet.MemUsedMB})
	writeJSON(w, http.StatusOK, out)
}
