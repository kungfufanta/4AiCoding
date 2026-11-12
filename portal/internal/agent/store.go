// Package agent runs the coding agent loop inside the Portal:
// a durable run queue in Postgres (claimed with FOR UPDATE SKIP
// LOCKED + lease), the LLM loop (tool calling lands in A3.2) and the
// event log the Chat page streams via SSE (docs/AGENT_PLAN_A3.md).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// Run statuses (migration 0010 CHECK constraint).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Terminal statuses — no worker will ever touch them again.
func Terminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

// Error codes (plan §4 error_code column + §7 flow).
const (
	ErrWorkerLost         = "worker_lost"
	ErrShutdown           = "worker_shutdown"
	ErrTimeout            = "timeout"
	ErrMaxSteps           = "max_steps"
	ErrTokenBudget        = "token_budget"
	ErrLLM                = "llm_error"
	ErrSandboxUnavailable = "sandbox_unavailable"
	ErrInternal           = "internal"
)

// Store is the durable state of agent runs, events and LLM history.
// Every tenant-scoped method runs under db.WithOrg so Row Level
// Security (migration 0010) backstops the SQL; the queue operations
// (ClaimNext/SweepLost) go through the SECURITY DEFINER functions,
// the only cross-org paths.
type Store struct {
	Pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// Sentinels the HTTP layer maps to status codes.
var (
	// ErrNotFound — the run/chat/sandbox does not exist or belongs to
	// another org (RLS hides it). Answers 404, never 403.
	ErrNotFound = errors.New("agent: not found")
	// ErrRunInProgress — the sandbox already has a queued/running run
	// (partial unique index agent_runs_one_active_per_sandbox). 409.
	ErrRunInProgress = errors.New("agent: run in progress for this sandbox")
	// ErrNotCancellable — the run already finished. 409.
	ErrNotCancellable = errors.New("agent: run is not running")
	// ErrIdempotencyKeyReuse — the Idempotency-Key was used for a
	// different sandbox. 422.
	ErrIdempotencyKeyReuse = errors.New("agent: idempotency key reuse")
	// ErrSandboxLocked — the chat already has runs, its sandbox can no
	// longer change (decision §13-3). 409.
	ErrSandboxLocked = errors.New("agent: chat sandbox is locked")
	// ErrNotLatestRun — revert targets the newest run of the sandbox
	// only (plan §8); the run is older. 409 not_latest_run.
	ErrNotLatestRun = errors.New("agent: run is not the latest of its sandbox")
	// ErrAlreadyReverted — reverted_at is already set. 409 already_reverted.
	ErrAlreadyReverted = errors.New("agent: run already reverted")
	// ErrNoCheckpoint — the run has checkpoint_ref NULL (image without
	// git, or checkpoint failed): nothing to restore from. 409 no_checkpoint.
	ErrNoCheckpoint = errors.New("agent: run has no checkpoint")
)

// Run is one agent task (row of agent_runs).
type Run struct {
	ID              string
	OrgID           string
	SandboxID       string
	ConversationID  string // "" when created via /v1
	CreatedByUser   string // "" when created via /v1
	CreatedByKey    string // "" when created via the BFF
	Prompt          string
	ConnectionID    string // "" = resolve the org default at claim time
	Model           string
	MaxSteps        int
	TimeoutS        int
	MaxTokens       int
	Status          string
	ErrorCode       string
	CheckpointRef   string
	Result          json.RawMessage
	Steps           int
	InputTokens     int64
	OutputTokens    int64
	CancelRequested bool
	RevertedAt      time.Time // epoch = not reverted
	IdempotencyKey  string
	CreatedAt       time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
}

const runColumns = `id::text, org_id::text, sandbox_id, COALESCE(conversation_id::text,''),
        COALESCE(created_by_user::text,''), COALESCE(created_by_key,''), prompt,
        COALESCE(connection_id::text,''), model, max_steps, timeout_s, max_tokens,
        status, COALESCE(error_code,''), COALESCE(checkpoint_ref,''), COALESCE(result::text,'{}'),
        steps, input_tokens, output_tokens, cancel_requested, COALESCE(reverted_at, 'epoch'), COALESCE(idempotency_key,''),
        created_at, COALESCE(started_at, 'epoch'), COALESCE(finished_at, 'epoch')`

// runScanFields returns the scan destinations matching runColumns —
// shared by scanRun and PrepareRevert.
func runScanFields(r *Run) []any {
	return []any{&r.ID, &r.OrgID, &r.SandboxID, &r.ConversationID,
		&r.CreatedByUser, &r.CreatedByKey, &r.Prompt,
		&r.ConnectionID, &r.Model, &r.MaxSteps, &r.TimeoutS, &r.MaxTokens,
		&r.Status, &r.ErrorCode, &r.CheckpointRef, &r.Result,
		&r.Steps, &r.InputTokens, &r.OutputTokens, &r.CancelRequested, &r.RevertedAt, &r.IdempotencyKey,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt}
}

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(runScanFields(&r)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	return r, nil
}

// CreateIn is the validated input of CreateRun.
type CreateIn struct {
	OrgID          string
	SandboxID      string
	ConversationID string
	CreatedByUser  string
	CreatedByKey   string
	Prompt         string
	// ConnectionID pins the model connection resolved at create time;
	// empty = the worker resolves the org default at claim time.
	ConnectionID   string
	Model          string
	MaxSteps       int
	TimeoutS       int
	MaxTokens      int
	IdempotencyKey string
}

// CreateRun inserts a queued run (the sandbox must be visible in this
// org — a foreign sandbox reads as not found) plus its first event
// status{phase:"queued"}, in one transaction. Replay with the same
// Idempotency-Key returns the original run (replayed=true) instead of
// a second run; a concurrent second active run on the same sandbox is
// ErrRunInProgress (the partial unique index is the real gate).
func (s *Store) CreateRun(ctx context.Context, in CreateIn) (run Run, replayed bool, err error) {
	// Plan §2-3 defaults; the DB CHECKs floor timeout_s at 30 s.
	if in.TimeoutS <= 0 {
		in.TimeoutS = 900
	}
	if in.MaxTokens <= 0 {
		in.MaxTokens = 300000
	}
	if in.MaxSteps <= 0 {
		in.MaxSteps = 30
	}
	err = db.WithOrg(ctx, s.Pool, in.OrgID, func(tx db.Tx) error {
		// Idempotency first: a replay must return the original run
		// even while it is still queued (the per-sandbox unique index
		// would otherwise win the 23505 race below).
		if in.IdempotencyKey != "" {
			prev, err := scanRun(tx.QueryRow(ctx,
				`SELECT `+runColumns+` FROM agent_runs WHERE org_id = $1::uuid AND idempotency_key = $2`,
				in.OrgID, in.IdempotencyKey))
			if err == nil {
				if prev.SandboxID != in.SandboxID {
					return ErrIdempotencyKeyReuse
				}
				run = prev
				replayed = true
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		// The sandbox must exist in this org (RLS hides foreign rows).
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM sandboxes WHERE id = $1`, in.SandboxID).Scan(nil); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// A conversation must exist in this org AND (RLS on
		// chat_conversations also pins the owner) belong to the caller.
		if in.ConversationID != "" {
			if err := tx.QueryRow(ctx,
				`SELECT 1 FROM chat_conversations WHERE id = $1::uuid`, in.ConversationID).Scan(nil); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
		}
		err := tx.QueryRow(ctx, `
                        INSERT INTO agent_runs
                                (org_id, sandbox_id, conversation_id, created_by_user, created_by_key,
                                 prompt, connection_id, model, max_steps, timeout_s, max_tokens, idempotency_key, status)
                        VALUES ($1::uuid, $2, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid, NULLIF($5,''),
                                $6, NULLIF($12,'')::uuid, $7, $8, $9, $10, NULLIF($11,''), 'queued')
                        RETURNING `+runColumns,
			in.OrgID, in.SandboxID, in.ConversationID, in.CreatedByUser, in.CreatedByKey,
			in.Prompt, in.Model, in.MaxSteps, in.TimeoutS, in.MaxTokens, in.IdempotencyKey,
			in.ConnectionID).
			Scan(runScanFields(&run)...)
		if err != nil {
			return createRunErr(ctx, tx, in, err, &run, &replayed)
		}
		_, err = appendEventTx(ctx, tx, run.ID, in.OrgID, "status", map[string]any{"phase": "queued"})
		return err
	})
	return run, replayed, err
}

// createRunErr translates the INSERT errors of CreateRun: unique
// violations on the per-sandbox index → ErrRunInProgress, on the
// idempotency index → return the original run (same sandbox) or
// ErrIdempotencyKeyReuse (different sandbox).
func createRunErr(ctx context.Context, tx db.Tx, in CreateIn, err error, run *Run, replayed *bool) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	switch pgErr.ConstraintName {
	case "agent_runs_one_active_per_sandbox":
		return ErrRunInProgress
	case "agent_runs_idem":
		prev, e := scanRun(tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM agent_runs WHERE org_id = $1::uuid AND idempotency_key = $2`,
			in.OrgID, in.IdempotencyKey))
		if e != nil {
			return e
		}
		if prev.SandboxID != in.SandboxID {
			return ErrIdempotencyKeyReuse
		}
		*run = prev
		*replayed = true
		return nil
	default:
		return err
	}
}

// GetRun reads one run. A run of another org is indistinguishable
// from a missing one (RLS) — both answer ErrNotFound.
func (s *Store) GetRun(ctx context.Context, orgID, runID string) (Run, error) {
	var r Run
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		var e error
		r, e = scanRun(tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM agent_runs WHERE id = $1::uuid`, runID))
		return e
	})
	return r, err
}

// ListRunsByChat lists the runs of one conversation, newest first.
func (s *Store) ListRunsByChat(ctx context.Context, orgID, chatID string) ([]Run, error) {
	out := []Run{}
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM chat_conversations WHERE id = $1::uuid`, chatID).Scan(nil); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return listRuns(ctx, tx, `WHERE conversation_id = $1::uuid ORDER BY created_at DESC LIMIT 200`, chatID, &out)
	})
	return out, err
}

// ListRunsBySandbox lists the runs of one sandbox, newest first.
func (s *Store) ListRunsBySandbox(ctx context.Context, orgID, sandboxID string) ([]Run, error) {
	out := []Run{}
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM sandboxes WHERE id = $1`, sandboxID).Scan(nil); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return listRuns(ctx, tx, `WHERE sandbox_id = $1 ORDER BY created_at DESC LIMIT 200`, sandboxID, &out)
	})
	return out, err
}

func listRuns(ctx context.Context, tx db.Tx, suffix, arg string, out *[]Run) error {
	rows, err := tx.Query(ctx, `SELECT `+runColumns+` FROM agent_runs `+suffix, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return err
		}
		*out = append(*out, r)
	}
	return rows.Err()
}

// RequestCancel flags a queued/running run for cancellation and wakes
// its worker via NOTIFY agent_cancel (the worker also polls
// cancel_requested once per second as the fallback, decision §2-8).
// An already-finished run answers ErrNotCancellable; an unknown or
// foreign one ErrNotFound.
func (s *Store) RequestCancel(ctx context.Context, orgID, runID string) (Run, error) {
	var r Run
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(ctx, `
                        UPDATE agent_runs SET cancel_requested = true
                        WHERE id = $1::uuid AND status IN ('queued','running')
                          AND cancel_requested = false`, runID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			// NOTIFY in the same transaction: the worker's listener
			// fires the moment this commits.
			if _, err := tx.Exec(ctx, `SELECT pg_notify('agent_cancel', $1)`, runID); err != nil {
				return err
			}
		}
		r, err = scanRun(tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM agent_runs WHERE id = $1::uuid`, runID))
		if err != nil {
			return err
		}
		if Terminal(r.Status) {
			return ErrNotCancellable
		}
		return nil
	})
	return r, err
}

// Event is one SSE frame (row of agent_events).
type Event struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

// eventCap is the plan's 256 KiB ceiling per event payload.
const eventCap = 256 << 10

// appendEventTx allocates seq = max+1 and inserts. The worker is the
// single writer per run, so the max+1 window cannot race with itself.
func appendEventTx(ctx context.Context, tx db.Tx, runID, orgID, typ string, data any) (int, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshal event %s: %w", typ, err)
	}
	if len(raw) > eventCap {
		raw, _ = json.Marshal(map[string]any{"truncated": true, "bytes": len(raw)})
	}
	var seq int
	err = tx.QueryRow(ctx, `
                INSERT INTO agent_events (run_id, seq, org_id, type, data)
                VALUES ($1::uuid,
                        (SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_events WHERE run_id = $1::uuid),
                        $2::uuid, $3, $4::jsonb)
                RETURNING seq`,
		runID, orgID, typ, raw).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("append event %s: %w", typ, err)
	}
	return seq, nil
}

// AppendEvent writes one event org-scoped.
func (s *Store) AppendEvent(ctx context.Context, orgID, runID, typ string, data any) (int, error) {
	var seq int
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		var e error
		seq, e = appendEventTx(ctx, tx, runID, orgID, typ, data)
		return e
	})
	return seq, err
}

// EventsSince returns every event with seq > since (replay always
// reads the table — plan §12 "Replay luôn đọc từ bảng, NOTIFY chỉ để
// đánh thức").
func (s *Store) EventsSince(ctx context.Context, orgID, runID string, since int) ([]Event, error) {
	out := []Event{}
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `
                        SELECT seq, type, data, created_at
                        FROM agent_events
                        WHERE run_id = $1::uuid AND seq > $2
                        ORDER BY seq`, runID, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.Seq, &e.Type, &e.Data, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ClaimedRun is what ClaimNext hands the worker: the queue columns it
// needs to execute the run.
type ClaimedRun struct {
	Run
	LeaseUntil time.Time
	WorkerID   string
}

// ClaimNext claims the oldest queued run for workerID via
// agent_claim_next (SECURITY DEFINER — the only cross-org read the
// worker does). ok=false when the queue is empty. LeaseUntil is
// computed client-side (the function does not return its now()).
func (s *Store) ClaimNext(ctx context.Context, workerID string, lease time.Duration) (ClaimedRun, bool, error) {
	rows, err := s.Pool.Query(ctx, `
                SELECT id, org_id, sandbox_id, COALESCE(conversation_id::text,''),
                       COALESCE(created_by_user::text,''), COALESCE(created_by_key,''), prompt,
                       COALESCE(connection_id::text,''), model, max_steps, timeout_s, max_tokens
                FROM agent_claim_next($1, $2)`,
		workerID, int(lease.Seconds()))
	if err != nil {
		return ClaimedRun{}, false, fmt.Errorf("claim next run: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c ClaimedRun
		c.WorkerID = workerID
		c.LeaseUntil = time.Now().Add(lease)
		if err := rows.Scan(&c.ID, &c.OrgID, &c.SandboxID, &c.ConversationID,
			&c.CreatedByUser, &c.CreatedByKey, &c.Prompt,
			&c.ConnectionID, &c.Model, &c.MaxSteps, &c.TimeoutS, &c.MaxTokens); err != nil {
			return ClaimedRun{}, false, err
		}
		return c, true, rows.Err()
	}
	return ClaimedRun{}, false, rows.Err()
}

// RenewLease extends the lease of a run this worker still owns.
// False when the run was reaped or reassigned (the loop must stop).
func (s *Store) RenewLease(ctx context.Context, orgID, runID, workerID string, lease time.Duration) (bool, error) {
	var ok bool
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(ctx, `
                        UPDATE agent_runs SET lease_until = now() + make_interval(secs => $3)
                        WHERE id = $1::uuid AND status = 'running' AND lease_owner = $2`,
			runID, workerID, int(lease.Seconds()))
		if err != nil {
			return err
		}
		ok = tag.RowsAffected() == 1
		return nil
	})
	return ok, err
}

// FinalState is the terminal write of one run: the row update and the
// final done event land in one transaction, so an SSE replay never
// shows a terminal row without its done event.
type FinalState struct {
	Status       string
	ErrorCode    string
	Result       json.RawMessage
	SandboxID    string // usage_events rows (plan §11 A3.3)
	Steps        int
	InputTokens  int64
	OutputTokens int64
	DoneData     map[string]any
}

// SetFinal writes the terminal status + done event for orgID. Unknown
// run (or foreign org) → ErrNotFound. An already-final run keeps its
// first outcome (a sweeper racing the worker must not overwrite it).
func (s *Store) SetFinal(ctx context.Context, orgID, runID string, f FinalState) error {
	// result is JSONB: an empty FinalState still writes {} (never an
	// invalid empty string).
	if len(f.Result) == 0 {
		f.Result = json.RawMessage("{}")
	}
	return db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(ctx, `
                        UPDATE agent_runs SET
                                status = $2, error_code = NULLIF($3,''), result = $4::jsonb,
                                steps = $5, input_tokens = $6, output_tokens = $7,
                                finished_at = now(), lease_owner = NULL, lease_until = NULL
                        WHERE id = $1::uuid AND status IN ('queued','running')`,
			runID, f.Status, f.ErrorCode, string(f.Result), f.Steps,
			f.InputTokens, f.OutputTokens)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var one int
			if err := tx.QueryRow(ctx,
				`SELECT 1 FROM agent_runs WHERE id = $1::uuid`, runID).Scan(&one); err != nil {
				return ErrNotFound
			}
			return nil // already final: keep the first outcome
		}
		done := f.DoneData
		if done == nil {
			done = map[string]any{"status": f.Status}
			if f.ErrorCode != "" {
				done["error_code"] = f.ErrorCode
			}
		}
		if _, err := appendEventTx(ctx, tx, runID, orgID, "done", done); err != nil {
			return err
		}
		// Usage ledger (plan §11 A3.3): one agent_tokens + one
		// agent_steps row per run, INSIDE the same transaction as the
		// terminal write — the ledger can never miss (or double-apply,
		// source_event_id UNIQUE) a run the row says finished.
		if f.SandboxID != "" {
			if _, err := tx.Exec(ctx, `
                        INSERT INTO usage_events
                                (org_id, sandbox_id, kind, quantity, unit, source_event_id, occurred_at)
                        VALUES ($1::uuid, $2, 'agent_tokens', $3, 'tokens', $4, now()),
                               ($1::uuid, $2, 'agent_steps', $5, 'steps', $6, now())
                        ON CONFLICT (source_event_id) DO NOTHING`,
				orgID, f.SandboxID, f.InputTokens+f.OutputTokens,
				"agent_tokens:"+runID, f.Steps, "agent_steps:"+runID); err != nil {
				return fmt.Errorf("insert agent usage_events: %w", err)
			}
		}
		return nil
	})
}

// SaveMessages replaces the stored LLM history of a run (called once
// per step; the history is small enough that replace-beats-merge).
func (s *Store) SaveMessages(ctx context.Context, orgID, runID string, msgs []llm.Msg) error {
	return db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM agent_messages WHERE run_id = $1::uuid`, runID); err != nil {
			return err
		}
		for i, m := range msgs {
			raw, err := json.Marshal(m)
			if err != nil {
				return fmt.Errorf("marshal message %d: %w", i, err)
			}
			if _, err := tx.Exec(ctx, `
                                INSERT INTO agent_messages (run_id, idx, org_id, role, content)
                                VALUES ($1::uuid, $2, $3::uuid, $4, $5::jsonb)`,
				runID, i, orgID, m.Role, raw); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadMessages returns the stored LLM history in order.
func (s *Store) LoadMessages(ctx context.Context, orgID, runID string) ([]llm.Msg, error) {
	out := []llm.Msg{}
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `
                        SELECT role, content FROM agent_messages
                        WHERE run_id = $1::uuid ORDER BY idx`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role string
			var raw []byte
			if err := rows.Scan(&role, &raw); err != nil {
				return err
			}
			var m llm.Msg
			if err := json.Unmarshal(raw, &m); err != nil {
				return fmt.Errorf("unmarshal stored message: %w", err)
			}
			if m.Role == "" {
				m.Role = role
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// SweptRun is one run reaped by SweepLost.
type SweptRun struct {
	ID        string
	OrgID     string
	SandboxID string
}

// SweepLost reaps every run whose lease expired more than graceSeconds
// ago: status failed/worker_lost + the final done event, atomically
// per run (the SECURITY DEFINER agent_sweep_lost). Returns the reaped
// runs so the caller can log per-org details.
func (s *Store) SweepLost(ctx context.Context, graceSeconds int) ([]SweptRun, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT reaped_id::text, reaped_org::text, reaped_sandbox FROM agent_sweep_lost($1)`, graceSeconds)
	if err != nil {
		return nil, fmt.Errorf("sweep lost runs: %w", err)
	}
	defer rows.Close()
	out := []SweptRun{}
	for rows.Next() {
		var r SweptRun
		if err := rows.Scan(&r.ID, &r.OrgID, &r.SandboxID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CancelRequested reports whether the run row currently asks for
// cancellation (the worker's poll fallback).
func (s *Store) CancelRequested(ctx context.Context, orgID, runID string) (bool, error) {
	var want bool
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(ctx, `
                        SELECT cancel_requested FROM agent_runs
                        WHERE id = $1::uuid AND status = 'running'`, runID).Scan(&want)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // finished or reaped: no cancellation to perform
	}
	return want, err
}

// SaveSummary stores the run's final text as an assistant chat message
// (plan §7 step 7) so the Chat page shows what the agent did. Runs
// without a conversation (created via /v1) skip this. The insert runs
// with app.user_id pinned to the run creator — chat_messages RLS pins
// org AND user, and the worker acts on behalf of the creator.
func (s *Store) SaveSummary(ctx context.Context, orgID, runID, conversationID, userID, summary string) error {
	if conversationID == "" || userID == "" {
		return nil
	}
	ctx = db.WithUserID(ctx, userID)
	return db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		if _, err := tx.Exec(ctx, `
                        INSERT INTO chat_messages (conversation_id, org_id, user_id, role, content, agent_run_id)
                        VALUES ($1::uuid, $2::uuid, $3::uuid, 'assistant', $4, $5::uuid)`,
			conversationID, orgID, userID, summary, runID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE chat_conversations SET updated_at = now() WHERE id = $1::uuid`, conversationID)
		return err
	})
}

// SetCheckpoint records the run's checkpoint ref (plan §7 step 3).
// Best-effort by the caller: a lost write only means a later
// files_changed query finds nothing to diff against.
func (s *Store) SetCheckpoint(ctx context.Context, orgID, runID, ref string) error {
	return db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
                        UPDATE agent_runs SET checkpoint_ref = $2 WHERE id = $1::uuid`,
			runID, ref)
		return err
	})
}

// PrevRunRow is one earlier run for the context block.
type PrevRunRow struct {
	ID      string
	Summary string
	Result  json.RawMessage
}

// RecentSummaries returns the ≤ n most recent succeeded runs of the
// same conversation (excluding this run) with their summaries — the
// "continue in the same chat" context (plan §7.4). Rows without a
// result summary are skipped.
func (s *Store) RecentSummaries(ctx context.Context, orgID, conversationID, excludeRunID string, n int) ([]PrevRunRow, error) {
	if conversationID == "" {
		return nil, nil
	}
	out := []PrevRunRow{}
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `
                        SELECT id::text, result->>'summary', COALESCE(result::text,'{}')
                        FROM agent_runs
                        WHERE conversation_id = $1::uuid AND id <> $2::uuid
                          AND status = 'succeeded' AND result ? 'summary'
                        ORDER BY created_at DESC LIMIT $3`,
			conversationID, excludeRunID, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p PrevRunRow
			if err := rows.Scan(&p.ID, &p.Summary, &p.Result); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// AuditTool writes the agent.tool audit row for one executed tool
// (plan §6: tool name, path or argv[0], exit code, duration — never
// the file content or command output). Best-effort: the run must not
// fail on an audit error.
func (s *Store) AuditTool(ctx context.Context, orgID, runID, tool, subject string, exitCode int, durationMs int64) {
	meta, _ := json.Marshal(map[string]any{
		"run_id": runID, "tool": tool, "subject": subject,
		"exit_code": exitCode, "duration_ms": durationMs,
	})
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		return tenants.Audit(ctx, tx, "system", "agent", "agent.tool", runID, string(meta))
	})
	_ = err // audited by the caller's log when it matters
}

// --- revert (plan §8, §11 A3.3) -----------------------------------------

// PrepareRevert validates that runID can be reverted and returns the
// row with its checkpoint ref (plan §8: only the LATEST run of the
// sandbox, not yet reverted, terminal, with a checkpoint — and the
// sandbox must have no active run). All checks run in one RLS-scoped
// transaction; the exec + the reverted_at claim happen after it, and
// ClaimRevert remains the atomic gate against a double revert.
func (s *Store) PrepareRevert(ctx context.Context, orgID, runID string) (Run, error) {
	var r Run
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT `+runColumns+` FROM agent_runs WHERE id = $1::uuid`, runID).
			Scan(runScanFields(&r)...)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !Terminal(r.Status) {
			// An active run is also the sandbox's one active
			// run — same answer the create path gives. 409.
			return ErrRunInProgress
		}
		// The column list COALESCEs reverted_at to 'epoch' (Unix 0) —
		// the not-reverted sentinel, same style as started_at.
		if r.RevertedAt.Unix() > 0 {
			return ErrAlreadyReverted
		}
		if r.CheckpointRef == "" {
			return ErrNoCheckpoint
		}
		// No queued/running run on this sandbox while its files
		// are being rolled back.
		var active int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM agent_runs
                          WHERE sandbox_id = $1 AND status IN ('queued','running')`,
			r.SandboxID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return ErrRunInProgress
		}
		// Latest run of the sandbox (created_at DESC, id DESC —
		// the agent_runs_sandbox_recent index covers the read).
		var latest string
		if err := tx.QueryRow(ctx,
			`SELECT id::text FROM agent_runs
                          WHERE sandbox_id = $1
                          ORDER BY created_at DESC, id DESC LIMIT 1`,
			r.SandboxID).Scan(&latest); err != nil {
			return err
		}
		if latest != r.ID {
			return ErrNotLatestRun
		}
		return nil
	})
	return r, err
}

// ClaimRevert atomically sets reverted_at (plan §8). The WHERE clause
// re-checks terminal + not-reverted so two racing reverts — or a
// revert racing a second revert via the other API surface — answer
// exactly one winner. false = someone else claimed it first.
func (s *Store) ClaimRevert(ctx context.Context, orgID, runID string) (bool, error) {
	var claimed bool
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(ctx, `
                        UPDATE agent_runs SET reverted_at = now()
                        WHERE id = $1::uuid
                          AND status IN ('succeeded','failed','cancelled')
                          AND reverted_at IS NULL`, runID)
		if err != nil {
			return err
		}
		claimed = tag.RowsAffected() > 0
		return nil
	})
	return claimed, err
}

// UnclaimRevert rolls a claim back after a failed revert exec, so the
// run stays revertible.
func (s *Store) UnclaimRevert(ctx context.Context, orgID, runID string) {
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE agent_runs SET reverted_at = NULL WHERE id = $1::uuid`, runID)
		return err
	})
	if err != nil {
		// The caller logs the exec failure; a stuck claim only
		// means the run answers already_reverted — recoverable
		// by an operator, never data loss.
		_ = err
	}
}

// AppendRevertEvent records the reverted event (plan §9.3: after done;
// files_restored / files_removed are counts, files_remaining is the
// post-check list — empty when the revert fully restored the tree).
func (s *Store) AppendRevertEvent(ctx context.Context, orgID, runID string,
	filesRestored, filesRemoved int, filesRemaining []FileChange) error {
	data := map[string]any{
		"files_restored": filesRestored,
		"files_removed":  filesRemoved,
	}
	if filesRemaining == nil {
		filesRemaining = []FileChange{}
	}
	data["files_remaining"] = filesRemaining
	_, err := s.AppendEvent(ctx, orgID, runID, "reverted", data)
	return err
}
