// The public sandbox API (PORTAL_PLAN 5, 6/S3): Bearer API-key auth,
// org-scoped through the `sandboxes` ownership index, idempotent
// creates and execs. Sandbox ids outside the caller's org answer 404
// (plan 5: never 403 — do not reveal existence).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/quota"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// withAPIKey authenticates a Bearer API key, enforces one scope and
// hands the handler the verified key. 401 on missing/invalid keys,
// 403 when the key's scopes exclude the route.
func (s *Server) withAPIKey(w http.ResponseWriter, r *http.Request, scope string, h func(w http.ResponseWriter, r *http.Request, key apikeys.Key)) {
	if s.Keys == nil {
		writeErr(w, http.StatusServiceUnavailable, "keys_unavailable", "API keys are not configured")
		return
	}
	presented := bearerToken(r)
	if presented == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "API key required (Authorization: Bearer sbx_live_…)")
		return
	}
	key, err := s.Keys.Verify(r.Context(), presented)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked API key")
		return
	}
	if !apikeys.ScopeValid(scope) || !apikeys.ScopesSubset([]string{scope}, key.Scopes) {
		writeErr(w, http.StatusForbidden, "scope_exceeded", "this API key lacks the "+scope+" scope")
		return
	}
	h(w, r, key)
}

// keyOwnsSandbox reports whether the key may touch sandbox id: it must
// be in the key's org AND, for a project-pinned key, in that project —
// the same boundary create and list already enforce. Anything else is
// the same 404 as an unknown id.
func (s *Server) keyOwnsSandbox(ctx context.Context, key apikeys.Key, id string) bool {
	projectID, ok := s.sandboxOwned(ctx, key.OrgID, id)
	if !ok {
		return false
	}
	return key.ProjectID == "" || projectID == key.ProjectID
}

// handleV1CreateSandbox — POST /v1/sandboxes.
func (s *Server) handleV1CreateSandbox(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:create", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		// P5 rate limit: token bucket per key, refill rate from the
		// org's plan. Refusal is 429 rate_limited with Retry-After.
		if s.Limiter != nil {
			plan, _ := quota.LoadPlan(r.Context(), s.Pool, key.OrgID)
			rps := plan.RateLimitRPS
			if rps <= 0 {
				rps = 2 // free plan default; LoadPlan never returns 0 for known plans
			}
			if ok, retry := s.Limiter.Take(key.KeyID, rps, 1); !ok {
				secs := int(retry.Round(time.Second).Seconds())
				if secs < 1 {
					secs = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(secs))
				writeErr(w, http.StatusTooManyRequests, "rate_limited",
					"too many sandbox:create requests; retry after the backoff")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		var body createSandboxBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		if !body.validate(w) {
			return
		}
		projectID := body.ProjectID
		if key.ProjectID != "" {
			if projectID != "" && projectID != key.ProjectID {
				writeErr(w, http.StatusForbidden, "forbidden",
					"this API key is pinned to one project and cannot create sandboxes elsewhere")
				return
			}
			projectID = key.ProjectID
		}
		if projectID != "" && !s.projectInOrg(r.Context(), key.OrgID, projectID) {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown project for this org")
			return
		}

		raw, _ := json.Marshal(body)
		idem := requestHash(http.MethodPost, "/v1/sandboxes", raw)
		status, payload, err := s.idempotent(r.Context(), key.OrgID, r.Header.Get("Idempotency-Key"), idem,
			func() (int, []byte, error) {
				// P5: admission inside the idempotent closure, so a
				// replay returns the stored 201 even at the ceiling
				// and the lock spans create + index insert.
				release, err := s.admitRun(r.Context(), key.OrgID)
				if err != nil {
					return 0, nil, err
				}
				defer release()
				sb, err := s.gatewayCreate(r.Context(), key.OrgID, projectID, "api_key", key.KeyID, body)
				if err != nil {
					return 0, nil, err
				}
				out, _ := json.Marshal(sandboxJSON{Sandbox: sb, ProjectID: projectID})
				return http.StatusCreated, out, nil
			})
		if errors.Is(err, errKeyReuse) {
			writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
				"this Idempotency-Key was already used with a different request body")
			return
		}
		if errors.Is(err, errIDConflict) {
			writeErr(w, http.StatusConflict, "idempotency_in_progress",
				"an identical request with this Idempotency-Key is still in progress")
			return
		}
		if s.writeAdmitErr(w, key.OrgID, err) {
			return
		}
		if err != nil {
			s.writeSandboxdErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	})
}

// gatewayCreate is the one create path both the public API and the
// dashboard BFF run through: sandboxd create (tenant-tagged), index
// row, audit.
func (s *Server) gatewayCreate(ctx context.Context, orgID, projectID, actorType, actorID string, body createSandboxBody) (sandboxd.Sandbox, error) {
	in := sandboxd.CreateInput{
		OrgID:       orgID,
		ProjectID:   projectID,
		UserID:      actorID,
		Image:       body.Image,
		Command:     body.Command,
		Env:         body.Env,
		CPUs:        body.Resources.CPUs,
		MemoryMB:    body.Resources.MemoryMB,
		Ports:       body.Ports,
		EgressMode:  body.Egress.Mode,
		EgressAllow: body.Egress.Allow,
		RepoURL:     body.Workspace.RepoURL,
		Branch:      body.Workspace.Branch,
	}
	sb, err := s.Sandboxd.Create(ctx, in)
	if err != nil {
		return sandboxd.Sandbox{}, err
	}
	// Index row + audit in one tx; the create itself is committed on
	// sandboxd's side already, so a failed index write must not fail
	// the caller's 201 — log it and let read-through sync heal.
	var createdByKey, createdByUser any
	if actorType == "api_key" {
		createdByKey = actorID
	} else {
		createdByUser = actorID
	}
	if err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		if _, err := tx.Exec(ctx, `
                        INSERT INTO sandboxes
                                (id, org_id, project_id, created_by_key, created_by_user, status, image,
                                 vcpu, memory_mb, ports, created_at, updated_at, started_at)
                        VALUES ($1, $2::uuid, NULLIF($3,'')::uuid, $4, $5::uuid, $6, $7, $8, $9, $10, $11, now(),
                                -- P5: a created sandbox is running, so its billing window opens now
                                CASE WHEN $6 IN ('running','created','starting','pending') THEN now() END)`,
			sb.ID, orgID, projectID, createdByKey, createdByUser,
			sb.Status, sb.Image, sb.Resources.CPUs, sb.Resources.MemoryMB, sb.Ports,
			parseRFC3339OrNow(sb.CreatedAt),
		); err != nil {
			return err
		}
		meta, _ := json.Marshal(map[string]any{
			"image": sb.Image, "project_id": projectID, "actor_kind": actorType,
		})
		return tenants.Audit(ctx, tx, actorType, actorID, "sandbox.create", sb.ID, string(meta))
	}); err != nil {
		s.Log.Error("sandbox index insert failed", "sandbox_id", sb.ID, "error", err.Error())
	}
	return sb, nil
}

// handleV1ListSandboxes — GET /v1/sandboxes.
func (s *Server) handleV1ListSandboxes(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:read", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		s.listSandboxesCommon(w, r, key.OrgID, key.ProjectID)
	})
}

// handleV1GetSandbox — GET /v1/sandboxes/{id}.
func (s *Server) handleV1GetSandbox(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:read", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		if !s.keyOwnsSandbox(r.Context(), key, r.PathValue("id")) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		s.getSandboxCommon(w, r, key.OrgID, r.PathValue("id"))
	})
}

// handleV1ExecSandbox — POST /v1/sandboxes/{id}/exec. A1 extends the
// P4 body with the v2 exec fields (timeout_s, cwd, env, stdin,
// max_output_bytes); ?stream=ndjson relays the NDJSON stream.
func (s *Server) handleV1ExecSandbox(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:exec", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB cap (stdin ≤ 1 MiB)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		// Validate the cmd array; the v2 fields are forwarded to
		// sandboxd, which re-validates them (policy.exec.max_timeout_s,
		// env overrides, cwd under /home/sandbox…).
		var body struct {
			Cmd            []string          `json:"cmd"`
			Cwd            string            `json:"cwd,omitempty"`
			Env            map[string]string `json:"env,omitempty"`
			Stdin          string            `json:"stdin,omitempty"`
			TimeoutS       *int              `json:"timeout_s,omitempty"`
			MaxOutputBytes *int64            `json:"max_output_bytes,omitempty"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || !validArgv(body.Cmd) {
			writeErr(w, http.StatusBadRequest, "bad_request",
				`body must be {"cmd": ["…", …]} (1..64 non-empty argv entries) plus optional v2 fields`)
			return
		}
		in := sandboxd.ExecInput{
			Cmd:   body.Cmd,
			Cwd:   body.Cwd,
			Env:   body.Env,
			Stdin: []byte(body.Stdin),
		}
		if body.TimeoutS != nil {
			in.TimeoutS = *body.TimeoutS
		}
		if body.MaxOutputBytes != nil {
			in.MaxOutputBytes = *body.MaxOutputBytes
		}
		// Gateway cap: clamp timeout_s to the same 120s the context
		// already allows, regardless of the caller's request. sandboxd
		// also clamps against policy.exec.max_timeout_s.
		if in.TimeoutS > maxExecSeconds {
			in.TimeoutS = maxExecSeconds
		}
		// Cap max_output_bytes at 4 MiB (sandboxd's execV2MaxOutputB).
		const maxOutputBytesCap int64 = 4 << 20
		if in.MaxOutputBytes > maxOutputBytesCap {
			in.MaxOutputBytes = maxOutputBytesCap
		}
		idem := requestHash(http.MethodPost, "/v1/sandboxes/"+id+"/exec", raw)
		// ?stream=ndjson: relay the NDJSON stream from sandboxd's exec
		// v2 surface. The idempotency store cannot cache a stream, so
		// the replay contract is "the first delivery wins; replays
		// get the same 422/409 as a buffered exec".
		if r.URL.Query().Get("stream") == "ndjson" {
			// Idempotency-Key: refuse a conflicting replay before
			// opening the stream (otherwise two concurrent streams
			// would both run).
			fresh := false
			_, _, err := s.idempotent(r.Context(), key.OrgID, r.Header.Get("Idempotency-Key"), idem,
				func() (int, []byte, error) {
					// The closure runs only once per Idempotency-Key;
					// the actual streaming happens below.
					fresh = true
					return http.StatusOK, []byte(`{}`), nil
				})
			if errors.Is(err, errKeyReuse) {
				writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
					"this Idempotency-Key was already used with a different request body")
				return
			}
			if errors.Is(err, errIDConflict) {
				writeErr(w, http.StatusConflict, "idempotency_in_progress",
					"an identical request with this Idempotency-Key is still in progress")
				return
			}
			if err != nil {
				s.writeSandboxdErr(w, err)
				return
			}
			// A stream cannot be stored, so a replayed key must not
			// run the command a second time: refuse it instead.
			if !fresh {
				writeErr(w, http.StatusConflict, "idempotency_key_used",
					"this Idempotency-Key already ran a streamed exec; a stream cannot be replayed")
				return
			}
			// Open the stream from sandboxd and relay lines.
			ctx, cancel := context.WithTimeout(r.Context(), (maxExecSeconds+10)*time.Second)
			defer cancel()
			res, err := s.Sandboxd.ExecStream(ctx, id, in)
			if err != nil {
				s.writeSandboxdErr(w, err)
				return
			}
			defer res.Body.Close()
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			if flusher != nil {
				flusher.Flush()
			}
			buf := make([]byte, 32*1024)
			for {
				n, err := res.Body.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return
					}
					if flusher != nil {
						flusher.Flush()
					}
				}
				if err != nil {
					return
				}
			}
		}
		status, payload, err := s.idempotent(r.Context(), key.OrgID, r.Header.Get("Idempotency-Key"), idem,
			func() (int, []byte, error) {
				ctx, cancel := context.WithTimeout(r.Context(), maxExecSeconds*time.Second)
				defer cancel()
				out, err := s.Sandboxd.Exec(ctx, id, in)
				if err != nil {
					return 0, nil, err
				}
				b, _ := json.Marshal(out)
				return http.StatusOK, b, nil
			})
		if errors.Is(err, errKeyReuse) {
			writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
				"this Idempotency-Key was already used with a different request body")
			return
		}
		if errors.Is(err, errIDConflict) {
			writeErr(w, http.StatusConflict, "idempotency_in_progress",
				"an identical request with this Idempotency-Key is still in progress")
			return
		}
		if err != nil {
			s.writeSandboxdErr(w, err)
			return
		}
		s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.exec", id, nil)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	})
}

// handleV1SandboxLogs — GET /v1/sandboxes/{id}/logs (backlog or SSE).
func (s *Server) handleV1SandboxLogs(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:exec", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		follow := r.URL.Query().Get("follow") == "true"
		tail := parseIntDefault(r.URL.Query().Get("tail"), 200, 10000)
		s.proxyLogs(w, r, key.OrgID, id, tail, follow)
	})
}

// handleV1SandboxAction — POST /v1/sandboxes/{id}/start|stop and
// DELETE /v1/sandboxes/{id}. Scope sandbox:create (plan 5 table).
func (s *Server) handleV1SandboxAction(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "sandbox:create", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		var action, err = r.URL.Path, error(nil)
		switch {
		case r.Method == http.MethodPost && endsWith(action, "/start"):
			release, aerr := s.admitRun(r.Context(), key.OrgID)
			if s.writeAdmitErr(w, key.OrgID, aerr) {
				return
			}
			if err = s.Sandboxd.Start(r.Context(), id); err == nil {
				s.markRunning(r.Context(), key.OrgID, id)
			}
			release()
		case r.Method == http.MethodPost:
			err = s.Sandboxd.Stop(r.Context(), id)
		default:
			err = s.Sandboxd.Delete(r.Context(), id)
		}
		if err != nil {
			s.writeSandboxdErr(w, err)
			return
		}
		switch {
		case r.Method == http.MethodDelete:
			s.dropSandboxRow(r.Context(), key.OrgID, id)
			s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.delete", id, nil)
		case endsWith(action, "/start"):
			s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.start", id, nil)
		default:
			s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.stop", id, nil)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// --- shared read paths (also used by the BFF) -----------------------

// listSandboxesCommon lists the org's sandboxes live from sandboxd and
// decorates them with the Portal-side project pointer. projectFilter
// ("") means every project.
func (s *Server) listSandboxesCommon(w http.ResponseWriter, r *http.Request, orgID, projectFilter string) {
	sandboxes, unreachable, err := s.Sandboxd.ListByWorkspace(r.Context(), orgID)
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	if projectFilter != "" {
		kept := sandboxes[:0]
		for _, sb := range sandboxes {
			if sb.External != nil && sb.External.ProjectID == projectFilter {
				kept = append(kept, sb)
			}
		}
		sandboxes = kept
	}
	projects := s.projectMap(r.Context(), orgID)
	out := make([]sandboxJSON, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, sandboxJSON{Sandbox: sb, ProjectID: projects[sb.ID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandboxes":         out,
		"unreachable_nodes": unreachable,
	})
}

// getSandboxCommon relays one sandbox detail after the ownership
// check, syncing the index row from the live view (read-through).
func (s *Server) getSandboxCommon(w http.ResponseWriter, r *http.Request, orgID, id string) {
	projectID, ok := s.sandboxOwned(r.Context(), orgID, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
		return
	}
	sb, err := s.Sandboxd.Get(r.Context(), id)
	if err != nil {
		var apiErr *sandboxd.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			// sandboxd purged it (TTL, console delete…): the index is stale.
			s.dropSandboxRow(r.Context(), orgID, id)
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		s.writeSandboxdErr(w, err)
		return
	}
	s.syncSandboxRow(r.Context(), orgID, projectID, sb)
	writeJSON(w, http.StatusOK, sandboxJSON{Sandbox: sb, ProjectID: projectID})
}

// proxyLogs relays the sandboxd log endpoint byte-for-byte (text
// backlog or SSE follow stream), flushing per chunk so SSE arrives
// live through the portal.
func (s *Server) proxyLogs(w http.ResponseWriter, r *http.Request, orgID, id string, tail int, follow bool) {
	res, err := s.Sandboxd.Logs(r.Context(), id, tail, follow)
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// --- shared lookups --------------------------------------------------

// projectInOrg reports whether the project row exists in the org
// (RLS makes foreign projects invisible → false).
func (s *Server) projectInOrg(ctx context.Context, orgID, projectID string) bool {
	var exists bool
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1::uuid)`, projectID).Scan(&exists)
	})
	return err == nil && exists
}

// projectMap returns sandbox_id → project_id for the org's index rows.
func (s *Server) projectMap(ctx context.Context, orgID string) map[string]string {
	m := map[string]string{}
	_ = db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, COALESCE(project_id::text,'') FROM sandboxes`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, project string
			if err := rows.Scan(&id, &project); err == nil {
				m[id] = project
			}
		}
		return rows.Err()
	})
	return m
}

// --- request types ---------------------------------------------------

type createSandboxBody struct {
	Image     string            `json:"image"`
	ProjectID string            `json:"project_id"`
	Command   []string          `json:"command"`
	Env       map[string]string `json:"env"`
	Resources struct {
		CPUs     float64 `json:"cpus"`
		MemoryMB int     `json:"memory_mb"`
	} `json:"resources"`
	Ports  []int `json:"ports"`
	Egress struct {
		Mode  string   `json:"mode"`
		Allow []string `json:"allow"`
	} `json:"egress"`
	Workspace struct {
		RepoURL string `json:"repo_url"`
		Branch  string `json:"branch"`
	} `json:"workspace"`
}

var (
	imageRe  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,199}$`)
	branchRe = regexp.MustCompile(`^[^\s^~:?*\[\\]{1,100}$`)
)

// validate answers 400 itself and returns false when the body is not
// buildable. sandboxd re-validates (allowlists, clamps) — this pass
// only rejects what can never be right.
func (b *createSandboxBody) validate(w http.ResponseWriter) bool {
	switch {
	case b.Image == "" || !imageRe.MatchString(b.Image):
		writeErr(w, http.StatusBadRequest, "bad_request", "image is required (repo[:tag] form, max 200 chars)")
	case len(b.Command) > 64:
		writeErr(w, http.StatusBadRequest, "bad_request", "command must have at most 64 argv entries")
	case len(b.Command) > 0 && !validArgv(b.Command):
		writeErr(w, http.StatusBadRequest, "bad_request", "command entries must be non-empty and ≤ 1000 chars")
	case len(b.Env) > 64:
		writeErr(w, http.StatusBadRequest, "bad_request", "env must have at most 64 entries")
	case b.Resources.CPUs < 0 || b.Resources.CPUs > 16:
		writeErr(w, http.StatusBadRequest, "bad_request", "resources.cpus must be 0..16")
	case b.Resources.MemoryMB < 0 || b.Resources.MemoryMB > 65536:
		writeErr(w, http.StatusBadRequest, "bad_request", "resources.memory_mb must be 0..65536")
	case len(b.Ports) > 8:
		writeErr(w, http.StatusBadRequest, "bad_request", "at most 8 ports")
	case !validPorts(b.Ports):
		writeErr(w, http.StatusBadRequest, "bad_request", "ports must be 1..65535")
	case b.Egress.Mode != "" && b.Egress.Mode != "none" && b.Egress.Mode != "allowlist" && b.Egress.Mode != "open":
		writeErr(w, http.StatusBadRequest, "bad_request", `egress.mode must be "none", "allowlist" or "open"`)
	case len(b.Egress.Allow) > 64:
		writeErr(w, http.StatusBadRequest, "bad_request", "at most 64 egress allow entries")
	case b.Workspace.RepoURL != "" && !validRepoURL(b.Workspace.RepoURL):
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace.repo_url must be an http(s) or git URL")
	case b.Workspace.RepoURL != "" && b.Workspace.Branch != "" && !branchRe.MatchString(b.Workspace.Branch):
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace.branch looks invalid")
	default:
		return true
	}
	return false
}

func validArgv(cmd []string) bool {
	if len(cmd) == 0 || len(cmd) > 64 {
		return false
	}
	for _, c := range cmd {
		if c == "" || len(c) > 1000 {
			return false
		}
	}
	return true
}

func validPorts(ports []int) bool {
	seen := map[int]bool{}
	for _, p := range ports {
		if p < 1 || p > 65535 || seen[p] {
			return false
		}
		seen[p] = true
	}
	return true
}

func validRepoURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https" || parsed.Scheme == "git") && parsed.Host != ""
}

// endsWith is a tiny helper so the action handler can tell start from
// stop without a route per method+verb pair.
func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
