// agent_runs.go — A3.1 HTTP surface (docs/AGENT_PLAN_A3.md §9):
//
//	BFF (cookie, Chat page):            /v1 (API key or agent token):
//	  PATCH  /api/orgs/{org}/chats/{chat}            — attach sandbox
//	  POST   /api/orgs/{org}/chats/{chat}/agent/runs — create run
//	  GET    /api/orgs/{org}/chats/{chat}/agent/runs — list run of chat
//	  GET    /api/orgs/{org}/agent/runs/{run}        — detail
//	  GET    /api/orgs/{org}/agent/runs/{run}/events — SSE
//	  POST   /api/orgs/{org}/agent/runs/{run}/cancel — cancel
//	  POST   /api/orgs/{org}/agent/runs/{run}/revert — revert (creator/admin)
//	  POST /v1/sandboxes/{id}/agent/runs   (agent:run,  Idempotency-Key)
//	  GET  /v1/sandboxes/{id}/agent/runs   (agent:read)
//	  GET  /v1/agent/runs/{run}            (agent:read)
//	  GET  /v1/agent/runs/{run}/events     (agent:read, ?follow=false)
//	  POST /v1/agent/runs/{run}/cancel     (agent:run)
//	  POST /v1/agent/runs/{run}/revert     (agent:run)
//
// Membership (BFF) or key-org (v1) is checked first; a foreign run,
// chat or sandbox answers 404 — never 403 (no existence leak).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// agent runs limits (plan §2-3).
const (
	agentPromptMaxRunes = 32768
	agentMaxStepsCap    = 50
	agentDefaultSteps   = 30
)

// runJSON is the wire shape of one agent run.
type runJSON struct {
	ID              string          `json:"id"`
	OrgID           string          `json:"org_id"`
	SandboxID       string          `json:"sandbox_id"`
	ConversationID  string          `json:"conversation_id,omitempty"`
	Status          string          `json:"status"`
	ErrorCode       string          `json:"error_code,omitempty"`
	Model           string          `json:"model"`
	Prompt          string          `json:"prompt"`
	MaxSteps        int             `json:"max_steps"`
	TimeoutS        int             `json:"timeout_s"`
	Steps           int             `json:"steps"`
	InputTokens     int64           `json:"input_tokens"`
	OutputTokens    int64           `json:"output_tokens"`
	Result          json.RawMessage `json:"result,omitempty"`
	CancelRequested bool            `json:"cancel_requested,omitempty"`
	RevertedAt      *time.Time      `json:"reverted_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

func runToJSON(r agent.Run) runJSON {
	j := runJSON{
		ID:              r.ID,
		OrgID:           r.OrgID,
		SandboxID:       r.SandboxID,
		ConversationID:  r.ConversationID,
		Status:          r.Status,
		ErrorCode:       r.ErrorCode,
		Model:           r.Model,
		Prompt:          r.Prompt,
		MaxSteps:        r.MaxSteps,
		TimeoutS:        r.TimeoutS,
		Steps:           r.Steps,
		InputTokens:     r.InputTokens,
		OutputTokens:    r.OutputTokens,
		Result:          r.Result,
		CancelRequested: r.CancelRequested,
		CreatedAt:       r.CreatedAt,
	}
	if len(j.Result) == 2 && string(j.Result) == "{}" {
		j.Result = nil
	}
	if r.StartedAt.Unix() > 0 {
		t := r.StartedAt
		j.StartedAt = &t
	}
	if r.FinishedAt.Unix() > 0 {
		t := r.FinishedAt
		j.FinishedAt = &t
	}
	if r.RevertedAt.Unix() > 0 {
		t := r.RevertedAt
		j.RevertedAt = &t
	}
	return j
}

func agentStore(s *Server) *agent.Store { return agent.NewStore(s.Pool) }

// --- BFF (session) handlers -------------------------------------------

// handleBFFAttachSandbox — PATCH /api/orgs/{org}/chats/{chat} with
// {"sandbox_id": "..."} (decision §13-3: one chat pins one sandbox;
// it can still change while the chat has no runs). The chat owner is
// enforced by chat_conversations RLS (org + user), so a foreign
// member's UPDATE touches nothing → 404.
func (s *Server) handleBFFAttachSandbox(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	chat, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var body struct {
		SandboxID string `json:"sandbox_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `body must be {"sandbox_id": "..."}`)
		return
	}
	sbx := strings.TrimSpace(body.SandboxID)
	var updated bool
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		// The sandbox must be visible in this org (RLS hides others).
		if sbx != "" {
			if err := tx.QueryRow(r.Context(),
				`SELECT 1 FROM sandboxes WHERE id = $1`, sbx).Scan(nil); err != nil {
				return err
			}
		}
		// Sandbox may only change while the chat has no runs.
		var n int
		if err := tx.QueryRow(r.Context(), `
                        SELECT COUNT(*) FROM agent_runs WHERE conversation_id = $1::uuid`, chat).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return agent.ErrSandboxLocked
		}
		tag, err := tx.Exec(r.Context(), `
                        UPDATE chat_conversations SET sandbox_id = NULLIF($2,''), updated_at = now()
                        WHERE id = $1::uuid`, chat, sbx)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected() == 1
		return nil
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The foreign-or-unknown sandbox SELECT fired ErrNoRows.
		writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
		return
	case errors.Is(err, agent.ErrSandboxLocked):
		writeErr(w, http.StatusConflict, "sandbox_locked",
			"this chat already has agent runs; create a new chat to use another sandbox")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "could not attach sandbox")
		return
	case !updated:
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": chat, "sandbox_id": sbx})
}

// handleBFFListAgentRuns — GET /api/orgs/{org}/chats/{chat}/agent/runs.
func (s *Server) handleBFFListAgentRuns(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	chat, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	runs, err := agentStore(s).ListRunsByChat(r.Context(), orgID, chat)
	if errors.Is(err, agent.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list runs")
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, runToJSON(run))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBFFCreateAgentRun — POST /api/orgs/{org}/chats/{chat}/agent/runs.
// The sandbox comes from the chat (decision §13-3), the connection
// from the org's effective model connection.
func (s *Server) handleBFFCreateAgentRun(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	chat, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	in, ok2 := s.parseAgentRunBody(w, r, orgID)
	if !ok2 {
		return
	}
	// The chat must exist and belong to the caller (RLS), and must
	// carry a sandbox.
	var sandboxID string
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(r.Context(), `
                        SELECT COALESCE(sandbox_id,'') FROM chat_conversations WHERE id = $1::uuid`, chat).Scan(&sandboxID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not load chat")
		return
	}
	if sandboxID == "" {
		writeErr(w, http.StatusBadRequest, "sandbox_required",
			"attach a sandbox to this chat before starting an agent run")
		return
	}
	in.SandboxID = sandboxID
	in.ConversationID = chat
	in.OrgID = orgID
	in.CreatedByUser = db.UserID(r.Context())

	s.createAgentRun(w, r, in, "user", db.UserID(r.Context()))
}

// parseAgentRunBody validates {"prompt","model","max_steps"} shared by
// the BFF and /v1 create handlers.
func (s *Server) parseAgentRunBody(w http.ResponseWriter, r *http.Request, orgID string) (agent.CreateIn, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	var body struct {
		Prompt   string `json:"prompt"`
		Model    string `json:"model"`
		MaxSteps int    `json:"max_steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `body must be {"prompt": "...", "model": "?", "max_steps": ?}`)
		return agent.CreateIn{}, false
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" || len([]rune(prompt)) > agentPromptMaxRunes {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"prompt must be 1..32768 characters")
		return agent.CreateIn{}, false
	}
	steps := body.MaxSteps
	if steps == 0 {
		steps = agentDefaultSteps
	}
	if steps < 1 || steps > agentMaxStepsCap {
		writeErr(w, http.StatusBadRequest, "bad_request", "max_steps must be between 1 and 50")
		return agent.CreateIn{}, false
	}
	connectionID, model, status, msg, ok := s.resolveRunModel(r.Context(), orgID, strings.TrimSpace(body.Model))
	if !ok {
		writeErr(w, status, msg.code, msg.text)
		return agent.CreateIn{}, false
	}
	return agent.CreateIn{
		Prompt:       prompt,
		Model:        model,
		ConnectionID: connectionID,
		MaxSteps:     steps,
	}, true
}

// modelErr carries the HTTP mapping of a model-resolution failure.
type modelErr struct {
	code string
	text string
}

// resolveRunModel picks the connection + model for a run: the org's
// effective connection and the requested model validated against the
// allowed list. A model declared supports_tools=false is ACCEPTED:
// the flag only means "the admin has not probed it (or probed it as
// tool-less)" — A3.2 review decision. The run starts in the fallback
// (JSON-in-text) tool mode instead of being blocked with 400
// model_no_tools (plan §5: the mode is decided once per run and
// recorded in a status event). Falls back to the env client when no
// DB connection exists (the worker's EnvSource mirrors this).
func (s *Server) resolveRunModel(ctx context.Context, orgID, requested string) (connectionID, model string, status int, merr modelErr, ok bool) {
	k, err := llmconfig.ResolveEffective(ctx, s.Pool, orgID)
	switch {
	case errors.Is(err, llmconfig.ErrNotFound):
		if s.LLM != nil {
			if requested != "" {
				return "", requested, 0, modelErr{}, true
			}
			return "", s.LLM.Model, 0, modelErr{}, true
		}
		return "", "", http.StatusServiceUnavailable,
			modelErr{"agent_not_configured", "no model connection is configured for this org"}, false
	case err != nil:
		return "", "", http.StatusInternalServerError,
			modelErr{"internal", "could not resolve model connection"}, false
	}
	if requested == "" {
		return k.Connection.ID, k.DefaultModel, 0, modelErr{}, true
	}
	for _, m := range k.Models {
		if m.ID == requested {
			// supports_tools=false no longer blocks the run (A3.2): the
			// worker runs it in fallback mode — see resolveRunModel.
			return k.Connection.ID, requested, 0, modelErr{}, true
		}
	}
	if len(k.Models) > 0 {
		return "", "", http.StatusBadRequest,
			modelErr{"model_not_allowed", "model " + requested + " is not in the connection's allowed list"}, false
	}
	// Open list: accept any model id.
	return k.Connection.ID, requested, 0, modelErr{}, true
}

// createAgentRun is the shared insert path: resolves the 409/503/201
// mapping, audits and answers. replayed=true (Idempotency-Key) still
// answers 201 — the body is the same run, and 201-is-a-run matches
// the sandbox create replay behaviour.
func (s *Server) createAgentRun(w http.ResponseWriter, r *http.Request, in agent.CreateIn, actorKind, actorID string) {
	store := agentStore(s)
	run, replayed, err := store.CreateRun(r.Context(), in)
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
		return
	case errors.Is(err, agent.ErrRunInProgress):
		writeErr(w, http.StatusConflict, "run_in_progress",
			"this sandbox already has an agent run in progress")
		return
	case errors.Is(err, agent.ErrIdempotencyKeyReuse):
		writeErr(w, http.StatusUnprocessableEntity, "idempotency_key_reuse",
			"this Idempotency-Key was already used with a different sandbox")
		return
	case err != nil:
		s.Log.Error("agent run create failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not create agent run")
		return
	}
	// Audit is best-effort (plan: the run creation must not fail on it).
	auditMeta, _ := json.Marshal(map[string]string{
		"run_id": run.ID, "sandbox_id": run.SandboxID, "model": run.Model,
	})
	auditCtx := context.WithoutCancel(r.Context())
	if err := db.WithOrg(auditCtx, s.Pool, in.OrgID, func(tx db.Tx) error {
		return tenants.Audit(auditCtx, tx, actorKind, actorID, "agent.run.create", run.ID, string(auditMeta))
	}); err != nil {
		s.Log.Warn("audit agent.run.create failed", slog.Any("error", err))
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusCreated, runToJSON(run))
}

// handleBFFGetAgentRun — GET /api/orgs/{org}/agent/runs/{run}.
func (s *Server) handleBFFGetAgentRun(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	run, err := agentStore(s).GetRun(r.Context(), orgID, r.PathValue("runID"))
	if errors.Is(err, agent.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not load run")
		return
	}
	writeJSON(w, http.StatusOK, runToJSON(run))
}

// handleBFFCancelAgentRun — POST /api/orgs/{org}/agent/runs/{run}/cancel.
// 202: the worker stops within ≤5 s (NOTIFY + poll fallback).
func (s *Server) handleBFFCancelAgentRun(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	s.cancelAgentRun(w, r, orgID, r.PathValue("runID"), "user", db.UserID(r.Context()))
}

func (s *Server) cancelAgentRun(w http.ResponseWriter, r *http.Request, orgID, runID, actorKind, actorID string) {
	run, err := agentStore(s).RequestCancel(r.Context(), orgID, runID)
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	case errors.Is(err, agent.ErrNotCancellable):
		writeErr(w, http.StatusConflict, "not_cancellable", "the run already finished")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "could not cancel run")
		return
	}
	auditMeta, _ := json.Marshal(map[string]string{"run_id": run.ID, "status": run.Status})
	auditCtx := context.WithoutCancel(r.Context())
	if err := db.WithOrg(auditCtx, s.Pool, orgID, func(tx db.Tx) error {
		return tenants.Audit(auditCtx, tx, actorKind, actorID, "agent.run.cancel", run.ID, string(auditMeta))
	}); err != nil {
		s.Log.Warn("audit agent.run.cancel failed", slog.Any("error", err))
	}
	writeJSON(w, http.StatusAccepted, runToJSON(run))
}

// revertAccess is who wants the revert: the BFF passes the session
// user (permission = creator or admin, plan §2-6); /v1 passes the
// key/token identity (the agent:run scope IS the permission).
type revertAccess struct {
	orgID, runID       string
	actorKind, actorID string
	isAdmin            bool
	creatorID          string // session user id, "" for /v1
}

// handleBFFRevertAgentRun — POST /api/orgs/{org}/agent/runs/{run}/revert.
func (s *Server) handleBFFRevertAgentRun(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	role, ok := s.requireMember(w, r, orgID)
	if !ok {
		return
	}
	userID := db.UserID(r.Context())
	s.revertAgentRun(w, r, revertAccess{
		orgID: orgID, runID: r.PathValue("runID"),
		actorKind: "user", actorID: userID,
		isAdmin:   role == tenants.RoleOwner || role == tenants.RoleAdmin,
		creatorID: userID,
	})
}

// revertAgentRun is the shared A3.3 revert flow (plan §8):
// validate → exec the restore in the sandbox → claim reverted_at →
// reverted event + audit. The exec runs BEFORE the claim so a failed
// restore leaves the run revertible; the claim is the atomic gate
// against a double revert racing across the two API surfaces.
func (s *Server) revertAgentRun(w http.ResponseWriter, r *http.Request, a revertAccess) {
	store := agentStore(s)
	run, err := store.PrepareRevert(r.Context(), a.orgID, a.runID)
	switch {
	case errors.Is(err, agent.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "run not found")
		return
	case errors.Is(err, agent.ErrRunInProgress):
		writeErr(w, http.StatusConflict, "run_in_progress", "the sandbox has an active run")
		return
	case errors.Is(err, agent.ErrAlreadyReverted):
		writeErr(w, http.StatusConflict, "already_reverted", "the run was already reverted")
		return
	case errors.Is(err, agent.ErrNoCheckpoint):
		writeErr(w, http.StatusConflict, "no_checkpoint", "the run has no checkpoint to restore from")
		return
	case errors.Is(err, agent.ErrNotLatestRun):
		writeErr(w, http.StatusConflict, "not_latest_run", "only the latest run of a sandbox can be reverted")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", "could not load run")
		return
	}
	// Permission (plan §2-6): the run's creator or an org admin.
	// Members get 403; a foreign org never gets here (404 above).
	if a.creatorID != "" && !a.isAdmin && run.CreatedByUser != a.creatorID {
		writeErr(w, http.StatusForbidden, "forbidden", "only the run creator or an org admin can revert")
		return
	}
	if s.Sandboxd == nil {
		writeErr(w, http.StatusServiceUnavailable, "agent_not_configured", "no sandbox gateway wired")
		return
	}

	// The run's own files_changed drives the restore (plan §8): the
	// status-A paths are deleted, the M/D/R count is reported as
	// files_restored.
	var changed []agent.FileChange
	if len(run.Result) > 0 {
		var res struct {
			FilesChanged []agent.FileChange `json:"files_changed"`
		}
		if jerr := json.Unmarshal(run.Result, &res); jerr == nil {
			changed = res.FilesChanged
		}
	}
	added := make([]string, 0, len(changed))
	restored := 0
	for _, f := range changed {
		if f.Status == "A" {
			added = append(added, f.Path)
		} else {
			restored++
		}
	}

	// Sandbox work outlives the request context being cancelled mid-
	// exec (the client may hang up while git restores big trees).
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
	defer pcancel()
	env, perr := agent.ProbeEnv(pctx, s.Sandboxd, run.SandboxID)
	if perr != nil {
		writeErr(w, http.StatusBadGateway, "sandbox_unavailable", "the sandbox is unreachable")
		return
	}
	if rerr := agent.RevertChanges(pctx, s.Sandboxd, run.SandboxID, env.ProjectDir, run.CheckpointRef, added); rerr != nil {
		writeErr(w, http.StatusBadGateway, "revert_failed", "could not restore the checkpoint")
		return
	}
	// Post-check (plan §8): files_changed vs the ref must be empty.
	remaining, ferr := agent.FilesChanged(pctx, s.Sandboxd, run.SandboxID, env.ProjectDir, run.CheckpointRef)
	if ferr != nil {
		s.Log.Warn("agent revert post-check failed", "run_id", a.runID, slog.Any("error", ferr))
		remaining = nil
	}
	if len(remaining) > 0 {
		s.Log.Warn("agent revert left files behind", "run_id", a.runID, "remaining", len(remaining))
	}

	claimed, cerr := store.ClaimRevert(pctx, a.orgID, a.runID)
	if cerr != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not mark the run reverted")
		return
	}
	if !claimed {
		writeErr(w, http.StatusConflict, "already_reverted", "the run was already reverted")
		return
	}
	if aerr := store.AppendRevertEvent(pctx, a.orgID, a.runID, restored, len(added), remaining); aerr != nil {
		s.Log.Warn("agent reverted event failed", "run_id", a.runID, slog.Any("error", aerr))
	}
	auditMeta, _ := json.Marshal(map[string]any{
		"run_id": a.runID, "sandbox_id": run.SandboxID,
		"files_restored": restored, "files_removed": len(added),
	})
	auditCtx := context.WithoutCancel(r.Context())
	if err := db.WithOrg(auditCtx, s.Pool, a.orgID, func(tx db.Tx) error {
		return tenants.Audit(auditCtx, tx, a.actorKind, a.actorID, "agent.run.revert", a.runID, string(auditMeta))
	}); err != nil {
		s.Log.Warn("audit agent.run.revert failed", slog.Any("error", err))
	}
	fresh, gerr := store.GetRun(pctx, a.orgID, a.runID)
	if gerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"reverted": true})
		return
	}
	writeJSON(w, http.StatusOK, runToJSON(fresh))
}

// handleBFFAgentRunEvents — GET /api/orgs/{org}/agent/runs/{run}/events.
func (s *Server) handleBFFAgentRunEvents(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	s.streamAgentEvents(w, r, orgID, r.PathValue("runID"))
}

// --- /v1 (API key / agent token) --------------------------------------

// agentIdentity is the verified caller of a /v1 agent route: either a
// verified API key or a verified agent token.
type agentIdentity struct {
	OrgID      string
	ProjectID  string // pinned project, "" = unrestricted
	Scopes     []string
	KeyID      string // sbx_live key id ("" for tokens)
	TokenRunID string // agent token claim agent_run_id ("" for keys)
	Kind       string // "api_key" | "agent_token"
	ActorID    string // for audit: key id or token's key id
}

// withAgentAuth authenticates the Bearer of /v1 agent routes: an
// agent token (JWT — three dot-separated segments starting "eyJ")
// when s.Tokens is wired, else an API key. Enforces one scope like
// withAPIKey. 401 invalid, 403 scope missing, 503 when the surface
// is not configured.
func (s *Server) withAgentAuth(w http.ResponseWriter, r *http.Request, scope string, h func(w http.ResponseWriter, r *http.Request, id agentIdentity)) {
	if s.Keys == nil {
		writeErr(w, http.StatusServiceUnavailable, "keys_unavailable", "API keys are not configured")
		return
	}
	presented := bearerToken(r)
	if presented == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized",
			"Bearer required (API key sbx_live_… or agent token)")
		return
	}
	if s.Tokens != nil && strings.Count(presented, ".") == 2 && strings.HasPrefix(presented, "eyJ") {
		claims, err := s.Tokens.Verify(presented)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid_agent_token", "invalid or expired agent token")
			return
		}
		if !apikeys.ScopesSubset([]string{scope}, claims.Scopes) {
			writeErr(w, http.StatusForbidden, "scope_exceeded", "this token lacks the "+scope+" scope")
			return
		}
		h(w, r, agentIdentity{
			OrgID:      claims.OrgID,
			ProjectID:  claims.ProjectID,
			Scopes:     claims.Scopes,
			TokenRunID: claims.AgentRunID,
			Kind:       "agent_token",
			ActorID:    claims.KeyID,
		})
		return
	}
	key, err := s.Keys.Verify(r.Context(), presented)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked API key")
		return
	}
	if !apikeys.ScopesSubset([]string{scope}, key.Scopes) {
		writeErr(w, http.StatusForbidden, "scope_exceeded", "this API key lacks the "+scope+" scope")
		return
	}
	h(w, r, agentIdentity{
		OrgID:     key.OrgID,
		ProjectID: key.ProjectID,
		Scopes:    key.Scopes,
		KeyID:     key.KeyID,
		Kind:      "api_key",
		ActorID:   key.KeyID,
	})
}

// idOwnsSandbox reports whether the caller may target sandbox id —
// same boundary as keyOwnsSandbox (org + project pin).
func (s *Server) idOwnsSandbox(ctx context.Context, id agentIdentity, sandboxID string) bool {
	projectID, ok := s.sandboxOwned(ctx, id.OrgID, sandboxID)
	if !ok {
		return false
	}
	return id.ProjectID == "" || projectID == id.ProjectID
}

// handleV1CreateAgentRun — POST /v1/sandboxes/{id}/agent/runs with
// Idempotency-Key support (plan §9.2).
func (s *Server) handleV1CreateAgentRun(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:run", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		sandboxID := r.PathValue("id")
		if !s.idOwnsSandbox(r.Context(), id, sandboxID) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		in, ok := s.parseAgentRunBody(w, r, id.OrgID)
		if !ok {
			return
		}
		in.OrgID = id.OrgID
		in.SandboxID = sandboxID
		in.CreatedByKey = id.KeyID
		in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		s.createAgentRun(w, r, in, id.Kind, id.ActorID)
	})
}

// handleV1ListAgentRuns — GET /v1/sandboxes/{id}/agent/runs.
func (s *Server) handleV1ListAgentRuns(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:read", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		sandboxID := r.PathValue("id")
		if !s.idOwnsSandbox(r.Context(), id, sandboxID) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		runs, err := agentStore(s).ListRunsBySandbox(r.Context(), id.OrgID, sandboxID)
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "could not list runs")
			return
		}
		out := make([]runJSON, 0, len(runs))
		for _, run := range runs {
			out = append(out, runToJSON(run))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// handleV1GetAgentRun — GET /v1/agent/runs/{run}. An agent token
// pinned to one run (claim agent_run_id) reads only that run — a
// different id answers the same 404 as a foreign run.
func (s *Server) handleV1GetAgentRun(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:read", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		runID := r.PathValue("runID")
		if id.TokenRunID != "" && id.TokenRunID != runID {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		run, err := agentStore(s).GetRun(r.Context(), id.OrgID, runID)
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "could not load run")
			return
		}
		writeJSON(w, http.StatusOK, runToJSON(run))
	})
}

// handleV1CancelAgentRun — POST /v1/agent/runs/{run}/cancel.
func (s *Server) handleV1CancelAgentRun(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:run", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		runID := r.PathValue("runID")
		if id.TokenRunID != "" && id.TokenRunID != runID {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		s.cancelAgentRun(w, r, id.OrgID, runID, id.Kind, id.ActorID)
	})
}

// handleV1RevertAgentRun — POST /v1/agent/runs/{run}/revert. The
// agent:run scope is the permission (plan §9.2); an agent token
// pinned to one run reverts only that run.
func (s *Server) handleV1RevertAgentRun(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:run", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		runID := r.PathValue("runID")
		if id.TokenRunID != "" && id.TokenRunID != runID {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		s.revertAgentRun(w, r, revertAccess{
			orgID: id.OrgID, runID: runID,
			actorKind: id.Kind, actorID: id.ActorID,
		})
	})
}

// handleV1AgentRunEvents — GET /v1/agent/runs/{run}/events
// (?follow=false returns the replay as JSON).
func (s *Server) handleV1AgentRunEvents(w http.ResponseWriter, r *http.Request) {
	s.withAgentAuth(w, r, "agent:read", func(w http.ResponseWriter, r *http.Request, id agentIdentity) {
		runID := r.PathValue("runID")
		if id.TokenRunID != "" && id.TokenRunID != runID {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		s.streamAgentEvents(w, r, id.OrgID, runID)
	})
}

// --- shared SSE ---------------------------------------------------------

// sinceSeq parses the replay cursor: the Last-Event-ID header wins,
// then ?since=; default 0 (full replay).
func sinceSeq(r *http.Request) (int, error) {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		return strconv.Atoi(strings.TrimSpace(v))
	}
	if v := r.URL.Query().Get("since"); v != "" {
		return strconv.Atoi(v)
	}
	return 0, nil
}

// streamAgentEvents replays agent_events from the cursor and, unless
// ?follow=false, keeps streaming until the done event (plan §9.1/§9.3:
// id = seq, ping every 15 s; replay always reads the table, LISTEN/
// NOTIFY only wakes it — the 1 s poll is the fallback).
func (s *Server) streamAgentEvents(w http.ResponseWriter, r *http.Request, orgID, runID string) {
	store := agentStore(s)
	if _, err := store.GetRun(r.Context(), orgID, runID); err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "run not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", "could not load run")
		return
	}
	since, err := sinceSeq(r)
	if err != nil || since < 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Last-Event-ID/since must be a non-negative integer")
		return
	}

	if r.URL.Query().Get("follow") == "false" {
		events, err := store.EventsSince(r.Context(), orgID, runID, since)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "could not read events")
			return
		}
		writeJSON(w, http.StatusOK, events)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	flush := func() { _ = rc.Flush() }
	writeFrame := func(e agent.Event) bool {
		if _, err := w.Write([]byte("id: " + strconv.Itoa(e.Seq) +
			"\nevent: " + e.Type + "\ndata: " + string(e.Data) + "\n\n")); err != nil {
			return false
		}
		flush()
		return true
	}

	var wake <-chan agent.NotifyMsg
	var stopWake func()
	if s.AgentNotify != nil {
		// Filter by run id IN the notifier (A3.2 review): an event of any
		// other run must not wake this follower into a re-read.
		wake, stopWake = s.AgentNotify.SubscribeRun(runID)
		defer stopWake()
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()

	lastSeq := since
	for {
		events, err := store.EventsSince(r.Context(), orgID, runID, lastSeq)
		if err != nil {
			if r.Context().Err() == nil {
				// Client still here: tell it the stream broke.
				_, _ = w.Write([]byte("event: error\ndata: {\"code\":\"internal\"}\n\n"))
				flush()
			}
			return
		}
		for _, e := range events {
			if !writeFrame(e) {
				return // client gone
			}
			lastSeq = e.Seq
			if e.Type == "done" {
				return // done is the last event of a run
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flush()
		case <-poll.C:
		case msg, open := <-wake:
			if !open {
				wake = nil // notifier died: polling continues
				continue
			}
			// The notifier already filtered by run id; a wake here means
			// THIS run has a new event — re-read the table.
			_ = msg.RunID
		}
	}
}
