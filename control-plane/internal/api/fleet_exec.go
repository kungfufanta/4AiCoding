// fleet_exec.go — sandboxes executed on workers (M7b).
//
// A worker in executor mode is a full sandboxd on its own machine (its own
// Docker, store and edge). It registers with an API token; the control plane
// stores that token encrypted and uses it to call the worker's /v1 API.
//
// The control plane stays the single front door:
//   - POST /v1/sandboxes picks a node (pickNode). A worker placement is
//     created on that worker and recorded in remote_sandboxes.
//   - every /v1/sandboxes/{id}/... request for a remote sandbox is forwarded
//     to its worker (remoteRoute), after the control plane's own approval
//     gates ran; the worker is told which approval was cleared
//     (FleetGrantHeader) and enforces the policy it receives on heartbeat.
//   - GET /v1/sandboxes merges the sandboxes of every executor worker.
//
// Authorisation (role, scopes, key binding, CSRF) already happened in the
// auth middleware on the control plane before any of this runs.
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
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// --- node credentials -------------------------------------------------------

func (s *Server) sealToken(tok string) (string, error) {
	if s.Secrets == nil {
		return "", errors.New("secrets encryption is not configured")
	}
	ct, nonce, err := s.Secrets.Seal([]byte(tok))
	if err != nil {
		return "", err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Server) openToken(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if s.Secrets == nil {
		return "", errors.New("secrets encryption is not configured")
	}
	parts := strings.Split(enc, ":")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", errors.New("unknown node token format")
	}
	nonce, err1 := base64.StdEncoding.DecodeString(parts[1])
	ct, err2 := base64.StdEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return "", errors.New("corrupt node token")
	}
	pt, err := s.Secrets.Open(ct, nonce)
	return string(pt), err
}

func (s *Server) storeNodeToken(ctx context.Context, nodeID, tok string) error {
	enc, err := s.sealToken(tok)
	if err != nil {
		return err
	}
	return s.Store.SetNodeAPIToken(ctx, nodeID, enc)
}

// checkNodeToken enforces the token binding on register: once a node has
// an API token, only the same token may re-register that node id.
func (s *Server) checkNodeToken(w http.ResponseWriter, r *http.Request, nodeID, presented string) bool {
	enc, err := s.Store.NodeAPIToken(r.Context(), nodeID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && enc == "") {
		return true
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return false
	}
	have, err := s.openToken(enc)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "node token: "+err.Error())
		return false
	}
	if presented == "" || !auth.TokenEqual(presented, have) {
		writeV1Err(w, http.StatusConflict, "conflict",
			"node "+nodeID+" is bound to another worker credential; an admin can reset it with DELETE /v1/nodes/"+nodeID)
		return false
	}
	return true
}

// execNode is a worker that can run sandboxes for the control plane.
type execNode struct {
	ID    string
	Base  *url.URL
	Token string
	Node  *store.Node
}

// execNodeFor resolves a node id to a callable executor (address + token).
func (s *Server) execNodeFor(ctx context.Context, n *store.Node) (*execNode, error) {
	if n.Role != store.RoleWorker {
		return nil, fmt.Errorf("node %s is not a worker", n.NodeID)
	}
	enc, err := s.Store.NodeAPIToken(ctx, n.NodeID)
	if err != nil {
		return nil, err
	}
	tok, err := s.openToken(enc)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, fmt.Errorf("node %s runs no sandboxes (no API token registered)", n.NodeID)
	}
	u, err := url.Parse(n.Address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("node %s has no usable address (%q)", n.NodeID, n.Address)
	}
	return &execNode{ID: n.NodeID, Base: u, Token: tok, Node: n}, nil
}

// executors returns every online worker that can run sandboxes.
func (s *Server) executors(ctx context.Context) ([]*execNode, error) {
	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []*execNode
	for _, n := range nodes {
		if n.Role != store.RoleWorker || fleet.Status(n, now, s.nodeLease()) != fleet.StatusOnline {
			continue
		}
		if en, err := s.execNodeFor(ctx, n); err == nil {
			out = append(out, en)
		}
	}
	return out, nil
}

// fleetUsage is the per-node usage for /v1/nodes and /v1/capacity: local
// placements, plus the number of sandboxes the control plane placed on
// each worker.
func (s *Server) fleetUsage(ctx context.Context) (map[string]store.Usage, error) {
	usage, err := s.Store.NodeUsage(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.Store.RemoteSandboxCounts(ctx)
	if err != nil {
		return nil, err
	}
	for id, n := range counts {
		u := usage[id]
		u.Sandboxes += n
		usage[id] = u
	}
	return usage, nil
}

// --- placement ----------------------------------------------------------------

// pickNode chooses where a new sandbox runs: the schedulable node with the
// most free memory, the local node winning ties. It returns nil for the
// local node (and when the fleet has no executor workers, so all-in-one
// installs behave exactly as before).
func (s *Server) pickNode(ctx context.Context) (*execNode, error) {
	workers, err := s.executors(ctx)
	if err != nil || len(workers) == 0 {
		return nil, err
	}
	free := func(n *store.Node, used store.Capacity) int {
		if n.Capacity.MemoryMB == 0 {
			return 0
		}
		return n.Capacity.MemoryMB - used.MemoryMB
	}
	var best *execNode
	bestFree := -1 << 31
	if local, err := s.localNode(ctx); err == nil && local != nil && local.DrainState == store.DrainActive {
		usage, _ := s.Store.NodeUsage(ctx)
		u := usage[local.NodeID]
		bestFree = free(local, store.Capacity{MemoryMB: u.MemoryMB})
	}
	localOK := bestFree != -1<<31
	for _, w := range workers {
		if w.Node.DrainState != store.DrainActive {
			continue
		}
		if f := free(w.Node, w.Node.Used); f > bestFree {
			best, bestFree = w, f
		}
	}
	if best == nil && !localOK {
		return nil, nil // nothing schedulable: the local create path answers 503
	}
	return best, nil
}

func (s *Server) localNode(ctx context.Context) (*store.Node, error) {
	nodes, err := s.Store.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Role == store.RoleLocal {
			return n, nil
		}
	}
	return nil, nil
}

// --- calling a worker ---------------------------------------------------------

var fleetHTTP = &http.Client{Timeout: 5 * time.Minute}

// nodeDo calls a worker's API as the control plane.
func (s *Server) nodeDo(ctx context.Context, en *execNode, method, path string, body []byte, hdr http.Header) (int, []byte, error) {
	u := *en.Base
	ref, err := url.Parse(path)
	if err != nil {
		return 0, nil, err
	}
	target := u.ResolveReference(ref)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+en.Token)
	resp, err := fleetHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, raw, err
}

func writeNodeUnreachable(w http.ResponseWriter, nodeID string, err error) {
	w.Header().Set("Retry-After", "10")
	writeV1Err(w, http.StatusBadGateway, "node_unreachable", "node "+nodeID+": "+err.Error())
}

// --- create on a worker ---------------------------------------------------------

// createRemote creates the sandbox on a worker. grant names the approval the
// control plane already cleared for this request ("" = none needed).
func (s *Server) createRemote(w http.ResponseWriter, r *http.Request, en *execNode, req v1CreateReq, grant string) {
	if req.Network != nil && len(req.Network.Ports) > 0 {
		// v1CreateSandbox already folded network.ports into ports.
		nw := *req.Network
		nw.Ports = nil
		req.Network = &nw
	}
	body, err := json.Marshal(req)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	hdr := http.Header{}
	if grant != "" {
		hdr.Set(FleetGrantHeader, grant)
	}
	code, resp, err := s.nodeDo(r.Context(), en, http.MethodPost, "/v1/sandboxes", body, hdr)
	if err != nil {
		writeNodeUnreachable(w, en.ID, err)
		return
	}
	if code == http.StatusCreated {
		var sb struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(resp, &sb); err != nil || sb.ID == "" {
			writeV1Err(w, http.StatusBadGateway, "node_unreachable", "node "+en.ID+" returned no sandbox id")
			return
		}
		actor := auth.ActorFrom(r.Context())
		if err := s.Store.AddRemoteSandbox(r.Context(), sb.ID, en.ID, actor.Kind+":"+actor.Name); err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", "record placement: "+err.Error())
			return
		}
		s.auditAction(r, audit.Entry{Action: "sandbox.create", Target: sb.ID,
			Detail: map[string]any{"node": en.ID, "visibility": req.Visibility}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(resp)
}

// --- routing /v1/sandboxes/{id}/... ------------------------------------------

// remoteRoute serves a sandbox-scoped route locally, or forwards it to the
// worker that runs the sandbox.
func (s *Server) remoteRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Store == nil {
			h(w, r)
			return
		}
		rs, err := s.Store.RemoteSandboxFor(r.Context(), r.PathValue("id"))
		if err != nil {
			h(w, r) // not remote (or store error: the local handler reports it)
			return
		}
		s.forwardRemote(w, r, rs)
	}
}

func (s *Server) forwardRemote(w http.ResponseWriter, r *http.Request, rs *store.RemoteSandbox) {
	n, err := s.Store.GetNode(r.Context(), rs.NodeID)
	if err != nil {
		writeNodeUnreachable(w, rs.NodeID, errors.New("node is not in the fleet"))
		return
	}
	en, err := s.execNodeFor(r.Context(), n)
	if err != nil {
		writeNodeUnreachable(w, rs.NodeID, err)
		return
	}
	id := rs.SandboxID
	grant := ""
	var body []byte
	switch {
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+id:
		// Same gate as v1DeleteSandbox: deleting a keep-alive sandbox may
		// need an approval. The keep-alive flag lives on the worker.
		code, raw, err := s.nodeDo(r.Context(), en, http.MethodGet, "/v1/sandboxes/"+id, nil, nil)
		if err != nil {
			writeNodeUnreachable(w, en.ID, err)
			return
		}
		var sb v1Sandbox
		if code == http.StatusOK && json.Unmarshal(raw, &sb) == nil && sb.Lifecycle.KeepAlive {
			pol, perr := s.policy(r.Context())
			if perr != nil {
				writeV1Err(w, http.StatusInternalServerError, "internal", perr.Error())
				return
			}
			if pol.RequiresApproval(policy.ApproveDeleteKeepAlive) {
				if s.requestApproval(w, r, policy.ApproveDeleteKeepAlive, "sandbox:"+id, nil) {
					return
				}
				grant = policy.ApproveDeleteKeepAlive
			}
		}
	case r.Method == http.MethodPut && r.URL.Path == "/v1/sandboxes/"+id+"/network/egress":
		// Same gate as v1PutEgress, decided here with the control plane's
		// policy on the worker's current rules; the worker applies exactly
		// the normalised result.
		req, ok := decodeEgressReq(w, r)
		if !ok {
			return
		}
		code, raw, err := s.nodeDo(r.Context(), en, http.MethodGet, "/v1/sandboxes/"+id+"/network/egress", nil, nil)
		if err != nil {
			writeNodeUnreachable(w, en.ID, err)
			return
		}
		if code != http.StatusOK {
			relayV1Error(w, code, raw)
			return
		}
		var cur policy.EgressRules
		if err := json.Unmarshal(raw, &cur); err != nil {
			writeV1Err(w, http.StatusBadGateway, "node_unreachable", "node "+en.ID+" returned unreadable egress rules")
			return
		}
		p, err := s.policy(r.Context())
		if err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		d := egressDecide(p, req.merge(cur))
		if !d.Allow {
			if d.NeedsApproval == "" {
				writePolicyErr(w, &policyError{d: d})
				return
			}
			if s.requestApproval(w, r, d.NeedsApproval, "sandbox:"+id+":egress", egressApprovalPayload(d)) {
				return
			}
			grant = d.NeedsApproval
		}
		body, _ = json.Marshal(egressApprovalPayload(d))
	}

	sw := &statusCapture{ResponseWriter: w, status: http.StatusOK}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(en.Base)
			pr.Out.URL.Path = r.URL.Path
			pr.Out.URL.RawPath = r.URL.RawPath
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = en.Base.Host
			// The worker sees the control plane, never the caller's
			// credentials.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del(auth.CSRFHeader)
			pr.Out.Header.Del(ApprovalHeader)
			pr.Out.Header.Del(FleetGrantHeader)
			pr.Out.Header.Set("Authorization", "Bearer "+en.Token)
			if grant != "" {
				pr.Out.Header.Set(FleetGrantHeader, grant)
			}
			pr.SetXForwarded()
			if body != nil {
				pr.Out.Body = io.NopCloser(bytes.NewReader(body))
				pr.Out.ContentLength = int64(len(body))
				pr.Out.Header.Set("Content-Type", "application/json")
			}
		},
		FlushInterval: -1, // SSE: logs?follow, metrics/stream
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeNodeUnreachable(w, en.ID, err)
		},
	}
	proxy.ServeHTTP(sw, r)

	if sw.status >= 200 && sw.status < 300 && r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+id {
		if err := s.Store.DeleteRemoteSandbox(r.Context(), id); err != nil {
			s.Log.Warn("fleet: forget remote sandbox failed", "sandbox_id", id, "err", err.Error())
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.auditAction(r, audit.Entry{Action: "sandbox.forward", Target: id, Detail: map[string]any{
			"node": en.ID, "method": r.Method, "path": r.URL.Path, "status": sw.status, "grant": grant,
		}})
	}
}

// statusCapture records the status a proxied response was written with,
// keeping Flush (SSE) working.
type statusCapture struct {
	http.ResponseWriter
	status int
}

func (c *statusCapture) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *statusCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// --- listing --------------------------------------------------------------------

// remoteSandboxes collects the sandboxes of every executor worker (query
// string passed through). Unreachable nodes are reported, not fatal.
func (s *Server) remoteSandboxes(r *http.Request) (out []json.RawMessage, unreachable []string) {
	nodes, err := s.executors(r.Context())
	if err != nil || len(nodes) == 0 {
		return nil, nil
	}
	path := "/v1/sandboxes"
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, en := range nodes {
		wg.Add(1)
		go func(en *execNode) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			code, raw, err := s.nodeDo(ctx, en, http.MethodGet, path, nil, nil)
			var resp struct {
				Sandboxes []json.RawMessage `json:"sandboxes"`
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil || code != http.StatusOK || json.Unmarshal(raw, &resp) != nil {
				unreachable = append(unreachable, en.ID)
				return
			}
			out = append(out, resp.Sandboxes...)
		}(en)
	}
	wg.Wait()
	return out, unreachable
}

// --- executor side -------------------------------------------------------------

// ApplyFleetPolicy stores the control plane's policy on an executor worker
// (sent with every heartbeat reply). It reports whether anything changed.
func (s *Server) ApplyFleetPolicy(ctx context.Context, raw json.RawMessage) (bool, error) {
	var p policy.Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		return false, err
	}
	cur, err := s.policy(ctx)
	if err == nil {
		a, _ := json.Marshal(cur)
		b, _ := json.Marshal(p)
		if bytes.Equal(a, b) {
			return false, nil
		}
	}
	if _, err := s.savePolicy(ctx, p); err != nil {
		return false, err
	}
	return true, nil
}

// --- DELETE /v1/nodes/{id} -------------------------------------------------------

// v1DeleteNode forgets a worker: an operator resets a machine that lost its
// state dir (new API token) or retires one. Refused while the control plane
// still routes sandboxes to it.
func (s *Server) v1DeleteNode(w http.ResponseWriter, r *http.Request) {
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
	if n.Role == store.RoleLocal {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "the control plane's own node cannot be removed")
		return
	}
	counts, err := s.Store.RemoteSandboxCounts(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if c := counts[id]; c > 0 {
		writeV1Err(w, http.StatusConflict, "conflict",
			fmt.Sprintf("node %s still runs %d sandbox(es); delete them first", id, c))
		return
	}
	if err := s.Store.DeleteNode(r.Context(), id); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "node.delete", Target: id})
	w.WriteHeader(http.StatusNoContent)
}
