// Package agent — loop.go: one claimed run executed end to end.
//
// A3.1 is the frame without real tools (plan §11): the loop calls the
// LLM once (StreamTools with no tools registered), streams text
// deltas as message events, and finishes with a done event. The
// sandbox prep, checkpoint and the 8-tool registry arrive in A3.2 —
// the loop's control flow (claim → status events → stream → done,
// with timeout/cancel/token handling) is already the one the tools
// plug into.
package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
)

// SystemPrompt is the go:embed'd agent system prompt (prompt/system.md).
// Decision §2-1: ported from the upstream prompt, trimmed; the full
// workspace context blocks arrive with the tools in A3.2.
//
//go:embed prompt/system.md
var SystemPrompt string

// LLMSource resolves the LLM client for one run. The worker wires it
// to llmconfig (per-org connections) with an env fallback; tests wire
// it to a fake server directly. The returned client's key is
// plaintext and must never be logged.
type LLMSource interface {
	Client(ctx context.Context, run Run) (*llm.Client, error)
}

// ConfigSource resolves clients through model_connections: the run's
// pinned connection when it has one, else the org's effective
// connection (org default → platform default). An error here surfaces
// as failed/llm_error — a run cannot execute without a model.
type ConfigSource struct {
	Pool *pgxpool.Pool
	// Fallback is the env-based Chat MVP client (PORTAL_DEFAULT_LLM_*),
	// used when no DB connection is configured. nil = DB only.
	Fallback *llm.Client
}

// Client implements LLMSource.
func (c ConfigSource) Client(ctx context.Context, run Run) (*llm.Client, error) {
	k, err := llmconfig.ResolveForRun(ctx, c.Pool, run.OrgID, run.ConnectionID)
	if errors.Is(err, llmconfig.ErrNotFound) {
		if c.Fallback != nil && run.ConnectionID == "" {
			fb := *c.Fallback // copy: per-run model mutation below
			fb.Model = run.Model
			return &fb, nil
		}
		return nil, llmconfig.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &llm.Client{
		BaseURL: k.BaseURL,
		APIKey:  k.APIKey, // plaintext; never logged
		Model:   run.Model,
		Flavor:  k.Flavor,
	}, nil
}

// deltaFlusher streams message deltas as message{delta} events,
// batching by the plan's budgets (250 ms or 512 bytes) so one long
// reply does not become one event per token. Losing a message delta
// must not fail the run — the final text still lands in the done
// event — so append failures only set failed and stop flushing.
type deltaFlusher struct {
	store *Store
	orgID string
	runID string
	ctx   context.Context

	buf    strings.Builder
	failed bool
	mu     sync.Mutex
}

const (
	deltaFlushBytes  = 512
	deltaFlushPeriod = 250 * time.Millisecond
)

func (d *deltaFlusher) add(chunk string) {
	d.mu.Lock()
	d.buf.WriteString(chunk)
	d.mu.Unlock()
}

func (d *deltaFlusher) pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Len()
}

// flush emits the buffered text as one message event.
func (d *deltaFlusher) flush() {
	d.mu.Lock()
	pending := d.buf.String()
	d.buf.Reset()
	d.mu.Unlock()
	if pending == "" || d.failed || d.ctx == nil {
		return
	}
	if _, err := d.store.AppendEvent(d.ctx, d.orgID, d.runID, "message", map[string]string{"delta": pending}); err != nil {
		d.failed = true
	}
}

// start flushes on the period ticker until ctx is done.
func (d *deltaFlusher) start(ctx context.Context) {
	d.ctx = ctx
	go func() {
		t := time.NewTicker(deltaFlushPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				d.flush()
				return
			case <-t.C:
				d.flush()
			}
		}
	}()
}

// executeRunResult reports the terminal outcome of one run attempt.
type executeRunResult struct {
	Status       string
	ErrorCode    string
	Summary      string
	Steps        int
	InputTokens  int64
	OutputTokens int64
	Result       json.RawMessage
	// Err is the internal cause for the log; never leaves the worker.
	Err error
}

// executeRun runs one claimed run to its terminal state: it writes
// the final row + done event and returns the outcome. It never
// returns a non-nil error for expected run failures — those are the
// result (failed/…); a returned error means the final write itself
// failed (the sweeper will reap the lease).
func (w *Worker) executeRun(runCtx context.Context, claimed ClaimedRun) (executeRunResult, error) {
	run := claimed.Run
	orgID := run.OrgID

	// status{phase:"running"} — the claim set the row; this event tells
	// the SSE followers (the claim's UPDATE does not NOTIFY).
	if _, err := w.Store.AppendEvent(runCtx, orgID, run.ID, "status", map[string]any{
		"phase": "running",
	}); err != nil {
		return executeRunResult{}, err
	}

	res := w.loop(runCtx, claimed)

	// Terminal write + done event in one tx. A3.1 has no checkpoint or
	// build yet: files_changed is empty and build_status is skipped
	// (plan §9.3 contract).
	if res.Result == nil {
		raw, _ := json.Marshal(runResultJSON{
			FilesChanged: []any{},
			BuildStatus:  "skipped",
			Steps:        res.Steps,
			InputTokens:  res.InputTokens,
			OutputTokens: res.OutputTokens,
		})
		res.Result = raw
	}
	final := FinalState{
		Status:       res.Status,
		ErrorCode:    res.ErrorCode,
		Result:       res.Result,
		Steps:        res.Steps,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
		DoneData: map[string]any{
			"status": res.Status,
			"result": json.RawMessage(res.Result),
		},
	}
	if res.ErrorCode != "" {
		final.DoneData["error_code"] = res.ErrorCode
	}
	// The terminal write must land even when the run context was
	// cancelled (cancel_requested / shutdown) — the process is still
	// alive, only the run's context is done.
	finalCtx := context.WithoutCancel(runCtx)
	if err := w.Store.SetFinal(finalCtx, orgID, run.ID, final); err != nil {
		return res, err
	}

	if res.Summary != "" {
		if err := w.Store.SaveSummary(finalCtx, orgID, run.ID,
			run.ConversationID, run.CreatedByUser, res.Summary); err != nil {
			w.Log.Warn("agent summary not saved to chat", "run_id", run.ID, "error", err.Error())
		}
	}
	return res, nil
}

// runResultJSON is the result object of a finished run (plan §9.3).
type runResultJSON struct {
	Summary      string `json:"summary"`
	FilesChanged []any  `json:"files_changed"`
	BuildStatus  string `json:"build_status"`
	PreviewOK    *bool  `json:"preview_ok"`
	Steps        int    `json:"steps"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

// loop is the A3.1 body of a run: one LLM call, no tools.
func (w *Worker) loop(runCtx context.Context, claimed ClaimedRun) executeRunResult {
	run := claimed.Run
	steps := 0

	fail := func(code string, err error) executeRunResult {
		w.Log.Warn("agent run failed", "run_id", run.ID, "error_code", code, "error", err.Error())
		return executeRunResult{
			Status:    StatusFailed,
			ErrorCode: code,
			Err:       err,
			Steps:     steps,
		}
	}

	// Run-scoped timeout (timeout_s; the DB CHECK floors 30, caps 1800).
	timeout := time.Duration(run.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 900 * time.Second
	}
	limCtx, cancel := context.WithTimeout(runCtx, timeout)
	defer cancel()

	client, err := w.LLM.Client(limCtx, run)
	if err != nil {
		return fail(ErrLLM, fmt.Errorf("resolve llm client: %w", err))
	}

	// A3.1 context: system prompt + the user prompt. History and
	// workspace blocks join in A3.2.
	msgs := []llm.Msg{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: run.Prompt},
	}

	flusher := &deltaFlusher{store: w.Store, orgID: run.OrgID, runID: run.ID}
	flusherCtx, flushStop := context.WithCancel(limCtx)
	defer flushStop()
	flusher.start(flusherCtx)

	steps = 1
	turn, err := client.StreamTools(limCtx, llm.ToolRequest{
		Messages:  msgs,
		UserID:    llmActorID(run),
		SessionID: run.ID,
		RequestID: run.ID + ":1",
	}, func(d string) error {
		if d == "" {
			return nil
		}
		flusher.add(d)
		if flusher.pending() >= deltaFlushBytes {
			flusher.flush()
		}
		return nil
	})
	// Drain the buffered tail SYNCHRONOUSLY so every message event
	// precedes the done event (the async ticker may not have fired —
	// a short stream finishes well under 250 ms).
	flusher.flush()
	flushStop()
	if err != nil {
		switch {
		case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled):
			// Someone cancelled the run (cancel_requested / shutdown).
			return w.cancelledResult(steps, turn.Usage)
		case errors.Is(runCtx.Err(), context.DeadlineExceeded) ||
			errors.Is(limCtx.Err(), context.DeadlineExceeded):
			return fail(ErrTimeout, fmt.Errorf("run exceeded %s: %w", timeout, err))
		default:
			return fail(ErrLLM, err)
		}
	}

	// No tools are registered in A3.1; a provider returning tool calls
	// anyway is a contract violation — fail loudly instead of guessing.
	if len(turn.ToolCalls) > 0 {
		return fail(ErrLLM, fmt.Errorf("model returned %d tool calls but no tools are registered", len(turn.ToolCalls)))
	}

	summary := strings.TrimSpace(turn.Text)
	if summary == "" {
		summary = "(the model returned an empty reply)"
	}
	out := runResultJSON{
		Summary:      summary,
		FilesChanged: []any{},
		BuildStatus:  "skipped",
		Steps:        steps,
		InputTokens:  int64(turn.Usage.InputTokens),
		OutputTokens: int64(turn.Usage.OutputTokens),
	}
	raw, _ := json.Marshal(out)

	// Persist the full LLM history for "continue in the same chat".
	if err := w.Store.SaveMessages(limCtx, run.OrgID, run.ID, msgs); err != nil {
		w.Log.Warn("agent history not saved", "run_id", run.ID, "error", err.Error())
	}

	return executeRunResult{
		Status:       StatusSucceeded,
		Summary:      summary,
		Steps:        steps,
		InputTokens:  int64(turn.Usage.InputTokens),
		OutputTokens: int64(turn.Usage.OutputTokens),
		Result:       raw,
	}
}

// cancelledResult finalizes a run whose context was cancelled: the
// status depends on who cancelled — cancel_requested → cancelled;
// worker shutdown → failed/worker_shutdown (plan §7 shutdown rule).
func (w *Worker) cancelledResult(steps int, usage llm.Usage) executeRunResult {
	in, out := int64(usage.InputTokens), int64(usage.OutputTokens)
	if w.shuttingDown() {
		return executeRunResult{
			Status:       StatusFailed,
			ErrorCode:    ErrShutdown,
			Steps:        steps,
			InputTokens:  in,
			OutputTokens: out,
		}
	}
	return executeRunResult{
		Status:       StatusCancelled,
		Steps:        steps,
		InputTokens:  in,
		OutputTokens: out,
	}
}

// llmActorID is the actor the LLM call is attributed to: the creator
// user when the BFF created the run, else the API key id (/v1, the
// k_ prefix marks a key on the wire — llm keeps it as-is).
func llmActorID(run Run) string {
	if run.CreatedByUser != "" {
		return run.CreatedByUser
	}
	if run.CreatedByKey != "" {
		return "k_" + run.CreatedByKey
	}
	return ""
}
