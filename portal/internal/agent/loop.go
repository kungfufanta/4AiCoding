// Package agent — loop.go: one claimed run executed end to end
// (docs/AGENT_PLAN_A3.md §7). A3.2 turns the A3.1 frame into the full
// agent loop:
//
//	prep sandbox → checkpoint → build context (workspace block,
//	AGENTS.md/BRAIN.md, earlier-run summaries) → step loop
//	(llm.StreamTools ⇄ tools.Registry through sandboxd) → postcheck
//	(files_changed, build, preview) → done event.
//
// The tool mode (native | fallback JSON) is decided ONCE per run
// (plan §5): a model declared supports_tools=false starts in fallback
// mode instead of being rejected, and a provider that 400s on the
// tools field flips the run to fallback at its first call.
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

	"github.com/tastyeffectco/sandboxd/portal/internal/agent/tools"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
)

// SystemPrompt is the go:embed'd agent system prompt (prompt/system.md).
// Decision §2-1: ported from the upstream agentprompt, adapted to the
// tool-based loop (no runtimed/sandbox.yaml), with the AGENTS.md /
// BRAIN.md conventions kept.
//
//go:embed prompt/system.md
var SystemPrompt string

// LLMChoice is what an LLMSource resolves for one run: the client
// plus the model metadata the tool-mode decision needs. When the
// connection's model list knows the model, ToolsKnown is true and
// SupportsTools carries the admin's probe result; a model the list
// does not know is treated as tool-capable (the fallback flip covers
// a provider that disagrees).
type LLMChoice struct {
	Client        *llm.Client
	ToolsKnown    bool
	SupportsTools bool
	ContextWindow int
}

// LLMSource resolves the LLM client for one run. The worker wires it
// to llmconfig (per-org connections) with an env fallback; tests wire
// it to a fake server directly. The returned client's key is
// plaintext and must never be logged.
type LLMSource interface {
	Client(ctx context.Context, run Run) (LLMChoice, error)
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
func (c ConfigSource) Client(ctx context.Context, run Run) (LLMChoice, error) {
	k, err := llmconfig.ResolveForRun(ctx, c.Pool, run.OrgID, run.ConnectionID)
	if errors.Is(err, llmconfig.ErrNotFound) {
		if c.Fallback != nil && run.ConnectionID == "" {
			fb := *c.Fallback // copy: per-run model mutation below
			fb.Model = run.Model
			return LLMChoice{Client: &fb, SupportsTools: true}, nil
		}
		return LLMChoice{}, llmconfig.ErrNotFound
	}
	if err != nil {
		return LLMChoice{}, err
	}
	choice := LLMChoice{Client: &llm.Client{
		BaseURL: k.BaseURL,
		APIKey:  k.APIKey, // plaintext; never logged
		Model:   run.Model,
		Flavor:  k.Flavor,
	}, SupportsTools: true}
	for _, m := range k.Models {
		if m.ID == run.Model {
			choice.ToolsKnown = true
			choice.SupportsTools = m.SupportsTools
			choice.ContextWindow = m.ContextWindow
			break
		}
	}
	return choice, nil
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

	// status{phase:"starting"} — the claim set the row; this event
	// tells the SSE followers (the claim's UPDATE does not NOTIFY).
	if _, err := w.Store.AppendEvent(runCtx, orgID, run.ID, "status", map[string]any{
		"phase": "starting",
	}); err != nil {
		return executeRunResult{}, err
	}

	res := w.loop(runCtx, claimed)

	// The loop fills a result on every terminal path; this covers a
	// panic recovery in startRun (no loop ran at all).
	if res.Result == nil {
		raw, _ := json.Marshal(runResultJSON{
			FilesChanged: []FileChange{},
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
	Summary        string       `json:"summary"`
	FilesChanged   []FileChange `json:"files_changed"`
	BuildStatus    string       `json:"build_status"`
	BuildCommand   string       `json:"build_command,omitempty"`
	PreviewOK      *bool        `json:"preview_ok,omitempty"`
	Steps          int          `json:"steps"`
	InputTokens    int64        `json:"input_tokens"`
	OutputTokens   int64        `json:"output_tokens"`
	UsageEstimated bool         `json:"usage_estimated,omitempty"`
	Warning        string       `json:"warning,omitempty"`
}

// Tool modes recorded in status events (plan §9.3).
const (
	toolModeNative   = "native"
	toolModeFallback = "fallback"
)

// outputPreviewBytes caps the tool_result event preview.
const outputPreviewBytes = 8 << 10

// loop is the A3.2 body of a run: prep, checkpoint, context, the
// step loop, postcheck.
func (w *Worker) loop(runCtx context.Context, claimed ClaimedRun) executeRunResult {
	run := claimed.Run
	orgID := run.OrgID
	steps := 0

	// Contexts that survive a cancelled runCtx: postcheck, audit and
	// the history save still run when the run's context is dead
	// (plan §7.6 "cả khi fail/cancel, nếu sandbox còn sống").
	pctx := context.WithoutCancel(runCtx)

	if w.Sandboxd == nil {
		w.Log.Warn("agent run failed", "run_id", run.ID,
			"error_code", ErrSandboxUnavailable, "error", "no sandboxd client wired")
		return executeRunResult{Status: StatusFailed, ErrorCode: ErrSandboxUnavailable,
			Err: errors.New("the agent worker has no sandboxd client wired")}
	}

	fail := func(code string, err error) executeRunResult {
		// A cancel landing during prep (Get/Start/keep-alive/probe)
		// surfaces as an exec error, but it must still end cancelled —
		// plan §7: done{status:cancelled} within the 5 s budget. The
		// run context is the truth; the error is just the symptom
		// (same guard as the step loop below). Shutdown keeps its
		// failed(shutdown) semantics via cancelledResult.
		if runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled) {
			w.Log.Warn("agent run cancelled during prep", "run_id", run.ID, "was_going_to_fail", code, "error", err.Error())
			return w.attachFilesChanged(pctx, run, w.cancelledResult(steps, llm.Usage{}))
		}
		w.Log.Warn("agent run failed", "run_id", run.ID, "error_code", code, "error", err.Error())
		res := executeRunResult{Status: StatusFailed, ErrorCode: code, Err: err, Steps: steps}
		return w.attachFilesChanged(pctx, run, res)
	}

	// Run-scoped timeout (timeout_s; the DB CHECK floors 30, caps 1800).
	timeout := time.Duration(run.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 900 * time.Second
	}
	limCtx, cancelLim := context.WithTimeout(runCtx, timeout)
	defer cancelLim()

	// --- step 2: sandbox prep (plan §7) --------------------------------
	sb, err := w.Sandboxd.Get(limCtx, run.SandboxID)
	if err != nil {
		return fail(ErrSandboxUnavailable, fmt.Errorf("sandbox %s: %w", run.SandboxID, err))
	}
	if sb.Status == "stopped" {
		if err := w.Sandboxd.Start(limCtx, run.SandboxID); err != nil {
			return fail(ErrSandboxUnavailable, fmt.Errorf("start sandbox: %w", err))
		}
	}
	// keep-alive for the duration of the run (restored afterwards);
	// a sandbox already kept alive by its owner stays that way.
	if !sb.Lifecycle.KeepAlive {
		if kerr := w.Sandboxd.KeepAlive(limCtx, run.SandboxID, true); kerr == nil {
			defer func() {
				offCtx, offCancel := context.WithTimeout(pctx, 10*time.Second)
				defer offCancel()
				_ = w.Sandboxd.KeepAlive(offCtx, run.SandboxID, false)
			}()
		} else {
			w.Log.Warn("agent keep-alive not enabled", "run_id", run.ID, "error", kerr.Error())
		}
	}

	env, err := ProbeEnv(limCtx, w.Sandboxd, run.SandboxID)
	if err != nil {
		return fail(ErrSandboxUnavailable, fmt.Errorf("probe workspace: %w", err))
	}

	// --- step 3: checkpoint (plan §8) ----------------------------------
	var checkpointRef string
	if env.HasGit {
		ref, cerr := Checkpoint(limCtx, w.Sandboxd, run.SandboxID, env.ProjectDir, run.ID)
		if cerr == nil {
			checkpointRef = ref
			if derr := w.Store.SetCheckpoint(limCtx, orgID, run.ID, ref); derr != nil {
				w.Log.Warn("agent checkpoint not saved", "run_id", run.ID, "error", derr.Error())
			}
			if _, eerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "status", map[string]any{
				"phase": "checkpoint", "ref": ref,
			}); eerr != nil {
				return fail(ErrInternal, eerr)
			}
		} else {
			w.Log.Warn("agent checkpoint failed (run continues without it)",
				"run_id", run.ID, "error", cerr.Error())
		}
	}

	// --- step 4: context (plan §7.4) -----------------------------------
	ws := BuildWorkspace(limCtx, w.Sandboxd, run.SandboxID, env)
	prevRows, serr := w.Store.RecentSummaries(limCtx, orgID, run.ConversationID, run.ID, 3)
	if serr != nil {
		w.Log.Warn("agent earlier-run summaries unavailable", "run_id", run.ID, "error", serr.Error())
	}
	prev := make([]PrevRun, 0, len(prevRows))
	for _, p := range prevRows {
		prev = append(prev, PrevRun{ID: p.ID, Summary: p.Summary, FilesChanged: filesChangedOf(p.Result)})
	}

	choice, err := w.LLM.Client(limCtx, run)
	if err != nil {
		return fail(ErrLLM, fmt.Errorf("resolve llm client: %w", err))
	}

	msgs := []llm.Msg{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: contextBlock(run, ws, prev)},
	}

	// Tool mode: decided ONCE per run (plan §5). A model the admin
	// probed as tool-less starts in fallback mode; anything else
	// starts native and may flip on the provider's first 400.
	fallback := choice.ToolsKnown && !choice.SupportsTools
	toolMode := toolModeNative
	if fallback {
		toolMode = toolModeFallback
	}
	if _, aerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "status", map[string]any{
		"phase": "running", "tool_mode": toolMode,
	}); aerr != nil {
		return fail(ErrInternal, aerr)
	}

	registry := tools.New(w.Sandboxd, run.SandboxID, env.ProjectDir, env.RelPrefix,
		env.HasRg, sb.Ports, w.Log)
	defs := tools.Definitions()

	flusher := &deltaFlusher{store: w.Store, orgID: orgID, runID: run.ID}
	flusherCtx, flushStop := context.WithCancel(limCtx)
	defer flushStop()
	flusher.start(flusherCtx)
	onText := func(d string) error {
		if d == "" {
			return nil
		}
		flusher.add(d)
		if flusher.pending() >= deltaFlushBytes {
			flusher.flush()
		}
		return nil
	}

	// --- step 5: the step loop -----------------------------------------
	var (
		totalIn, totalOut int64
		usageEstimated    bool
		summary           string
		finished          bool
		consecDown        int
		msgStep           = make([]int, 0, 32) // step each history message was added at
	)
	for step := 1; step <= run.MaxSteps; step++ {
		steps = step
		turn, err := choice.Client.StreamTools(limCtx, llm.ToolRequest{
			Messages:  msgs,
			Tools:     defs,
			UserID:    llmActorID(run),
			SessionID: run.ID,
			RequestID: fmt.Sprintf("%s:%d", run.ID, step),
			Fallback:  fallback,
		}, onText)
		// Drain the buffered tail SYNCHRONOUSLY so this turn's message
		// events precede the tool_call events of the same step (the
		// async ticker may not have fired).
		flusher.flush()

		if errors.Is(err, llm.ErrToolsUnsupported) && !fallback {
			// First 400 that blames the tools: flip the run to fallback
			// mode (once — plan §5) and retry the SAME step.
			fallback = true
			toolMode = toolModeFallback
			if _, aerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "status", map[string]any{
				"phase": "running", "tool_mode": toolMode,
				"warning": "provider rejected the tools field; switched to the JSON fallback protocol",
			}); aerr != nil {
				return fail(ErrInternal, aerr)
			}
			steps--
			continue
		}
		if err != nil {
			switch {
			case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.Canceled):
				// Someone cancelled the run (cancel_requested / shutdown).
				res := w.cancelledResult(steps, llm.Usage{
					InputTokens: int(totalIn), OutputTokens: int(totalOut)})
				return w.attachFilesChanged(pctx, run, res)
			case errors.Is(runCtx.Err(), context.DeadlineExceeded) ||
				errors.Is(limCtx.Err(), context.DeadlineExceeded):
				return fail(ErrTimeout, fmt.Errorf("run exceeded %s: %w", timeout, err))
			case errors.Is(err, llm.ErrBadToolBlock) || errors.Is(err, llm.ErrBadToolCall):
				// The tool protocol was violated (fallback fence, or
				// assembled arguments that are not JSON): feed the
				// reason back like a tool_result (plan §5) and spend
				// the step.
				msgs = append(msgs, llm.Msg{Role: "user", Content: "Your last reply violated the tool protocol: " + err.Error() +
					"\nReply again — at most one ```tool block, or plain text."})
				msgStep = append(msgStep, step)
				continue
			default:
				return fail(ErrLLM, err)
			}
		}

		totalIn += int64(turn.Usage.InputTokens)
		totalOut += int64(turn.Usage.OutputTokens)
		usageEstimated = usageEstimated || turn.Usage.UsageEstimated
		if run.MaxTokens > 0 && totalIn+totalOut > int64(run.MaxTokens) {
			return fail(ErrTokenBudget, fmt.Errorf("run exceeded max_tokens=%d (used %d)",
				run.MaxTokens, totalIn+totalOut))
		}

		if len(turn.ToolCalls) == 0 {
			// No tool call: the text is the final summary (plan §7.5).
			summary = strings.TrimSpace(turn.Text)
			finished = true
			break
		}

		msgs = append(msgs, llm.Msg{Role: "assistant", Content: turn.Text, ToolCalls: turn.ToolCalls})
		msgStep = append(msgStep, step)

		done := false
		for _, call := range turn.ToolCalls {
			if _, aerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "tool_call", map[string]any{
				"id": call.ID, "name": call.Name, "args_preview": json.RawMessage(tools.PreviewArgs(call)),
			}); aerr != nil {
				return fail(ErrInternal, aerr)
			}

			started := time.Now()
			res, exErr := registry.Exec(limCtx, call)
			durMs := time.Since(started).Milliseconds()

			if exErr != nil {
				// Transport-level failure: three consecutive ones mean
				// the sandboxd backend is gone (plan §6 sandbox_unavailable).
				consecDown++
				w.Log.Warn("agent tool exec failed", "run_id", run.ID,
					"tool", call.Name, "error", exErr.Error())
				msgs = append(msgs, llm.Msg{Role: "tool", ToolCallID: call.ID,
					Content: "tool infrastructure error (the sandbox is unreachable); if it persists the run will stop"})
				msgStep = append(msgStep, step)
				if _, aerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "tool_result", map[string]any{
					"id": call.ID, "ok": false, "error": "sandbox_unavailable", "duration_ms": durMs,
				}); aerr != nil {
					return fail(ErrInternal, aerr)
				}
				if consecDown >= 3 {
					return fail(ErrSandboxUnavailable, fmt.Errorf("3 consecutive tool failures: %w", exErr))
				}
				continue
			}
			consecDown = 0

			// Audit agent.tool — the subject, never the content (plan §6).
			w.Store.AuditTool(pctx, orgID, run.ID, call.Name,
				toolSubject(call), res.ExitCode, durMs)

			preview := res.Text
			trunc := res.Truncated
			if len(preview) > outputPreviewBytes {
				preview = preview[:outputPreviewBytes] + "…"
				trunc = true
			}
			eventData := map[string]any{
				"id": call.ID, "ok": res.OK, "duration_ms": durMs,
				"output_preview": preview, "truncated": trunc,
			}
			if res.ExitCode >= 0 {
				eventData["exit_code"] = res.ExitCode
			}
			if res.ErrCode != "" {
				eventData["error_code"] = res.ErrCode
			}
			if _, aerr := w.Store.AppendEvent(runCtx, orgID, run.ID, "tool_result", eventData); aerr != nil {
				return fail(ErrInternal, aerr)
			}

			if call.Name == "finish" {
				// finish ends the loop (plan §7.5); remaining calls of
				// the same turn are not executed.
				summary = strings.TrimSpace(res.Text)
				done = true
				break
			}
			msgs = append(msgs, llm.Msg{Role: "tool", ToolCallID: call.ID, Content: res.Text})
			msgStep = append(msgStep, step)
		}

		compressContext(msgs, msgStep, step, choice.ContextWindow)
		if done {
			finished = true
			break
		}
	}

	if !finished {
		return fail(ErrMaxSteps, fmt.Errorf("run reached max_steps=%d without finishing", run.MaxSteps))
	}

	// --- persist the history (A3.3 "continue in the same chat") --------
	if err := w.Store.SaveMessages(pctx, orgID, run.ID, msgs); err != nil {
		w.Log.Warn("agent history not saved", "run_id", run.ID, "error", err.Error())
	}

	// --- step 6: postcheck (plan §7.6) ---------------------------------
	if _, perr := w.Store.AppendEvent(pctx, orgID, run.ID, "status", map[string]any{
		"phase": "postcheck",
	}); perr != nil {
		w.Log.Warn("agent postcheck event failed", "run_id", run.ID, "error", perr.Error())
	}

	if summary == "" {
		summary = "(the model returned an empty reply)"
	}
	out := runResultJSON{
		Summary:        summary,
		FilesChanged:   []FileChange{},
		BuildStatus:    "skipped",
		Steps:          steps,
		InputTokens:    totalIn,
		OutputTokens:   totalOut,
		UsageEstimated: usageEstimated,
	}
	if !env.HasGit {
		out.Warning = "the image has no git: no checkpoint, files_changed is empty"
	} else if checkpointRef != "" {
		if changes, cerr := FilesChanged(pctx, w.Sandboxd, run.SandboxID, env.ProjectDir, checkpointRef); cerr == nil {
			out.FilesChanged = changes
		} else {
			w.Log.Warn("agent files_changed failed", "run_id", run.ID, "error", cerr.Error())
		}
	}
	// Build + preview only for succeeded runs: a cancelled run must
	// still answer its done event inside the 5 s cancel budget.
	if cmd, derr := DetectBuild(pctx, w.Sandboxd, run.SandboxID, env.RelPrefix); derr != nil {
		w.Log.Warn("agent build detect failed", "run_id", run.ID, "error", derr.Error())
	} else if cmd != "" {
		out.BuildCommand = cmd
		br := RunBuild(pctx, w.Sandboxd, run.SandboxID, env.ProjectDir, cmd)
		out.BuildStatus = br.Status
		if _, berr := w.Store.AppendEvent(pctx, orgID, run.ID, "build", map[string]any{
			"status": br.Status, "command": cmd, "tail": br.Tail,
		}); berr != nil {
			w.Log.Warn("agent build event failed", "run_id", run.ID, "error", berr.Error())
		}
	}
	if ok, checked := CheckPreview(pctx, w.Sandboxd, run.SandboxID, sb.Ports); checked {
		out.PreviewOK = &ok
	}

	return executeRunResult{
		Status:       StatusSucceeded,
		Summary:      summary,
		Steps:        steps,
		InputTokens:  totalIn,
		OutputTokens: totalOut,
		Result:       marshalResult(out),
	}
}

// attachFilesChanged gives a failed/cancelled run its files_changed
// list (plan §7.6 "hậu kiểm cả khi fail/cancel") — the cheap part
// only: build + preview stay succeeded-only so a cancelled run keeps
// its 5 s done budget.
func (w *Worker) attachFilesChanged(pctx context.Context, run Run, res executeRunResult) executeRunResult {
	if res.Result != nil {
		return res
	}
	// files_changed needs the checkpoint ref on the row; the loop has
	// it in its locals. Re-read the row — one extra SELECT per
	// terminal failure is nothing.
	stored, err := w.Store.GetRun(pctx, run.OrgID, run.ID)
	if err != nil || stored.CheckpointRef == "" {
		raw, _ := json.Marshal(runResultJSON{
			FilesChanged: []FileChange{},
			BuildStatus:  "skipped",
			Steps:        res.Steps,
			InputTokens:  res.InputTokens,
			OutputTokens: res.OutputTokens,
		})
		res.Result = raw
		return res
	}
	fctx, cancel := context.WithTimeout(pctx, 70*time.Second)
	defer cancel()
	env, err := ProbeEnv(fctx, w.Sandboxd, run.SandboxID)
	if err != nil {
		raw, _ := json.Marshal(runResultJSON{FilesChanged: []FileChange{}, BuildStatus: "skipped",
			Steps: res.Steps, InputTokens: res.InputTokens, OutputTokens: res.OutputTokens})
		res.Result = raw
		return res
	}
	changes, cerr := FilesChanged(fctx, w.Sandboxd, run.SandboxID, env.ProjectDir, stored.CheckpointRef)
	files := changes
	if cerr != nil {
		w.Log.Warn("agent files_changed failed", "run_id", run.ID, "error", cerr.Error())
		files = []FileChange{}
	}
	raw, _ := json.Marshal(runResultJSON{
		FilesChanged: files,
		BuildStatus:  "skipped",
		Steps:        res.Steps,
		InputTokens:  res.InputTokens,
		OutputTokens: res.OutputTokens,
	})
	res.Result = raw
	return res
}

// marshalResult serialises the run result (the done event carries it
// verbatim).
func marshalResult(out runResultJSON) json.RawMessage {
	raw, _ := json.Marshal(out)
	return raw
}

// toolSubject is the audit subject (plan §6 "path hoặc argv[0]"):
// what the tool touched, never its content.
func toolSubject(call llm.ToolCall) string {
	var a struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Pattern string `json:"pattern"`
	}
	_ = json.Unmarshal(call.Arguments, &a)
	switch call.Name {
	case "run_command":
		s := strings.Fields(a.Command)
		if len(s) > 0 {
			return s[0]
		}
		return "bash"
	case "search":
		return a.Pattern
	default:
		return a.Path
	}
}

// compressContext replaces the tool results older than six steps with
// one-line elisions when the history is past 70 % of the context
// window (plan §7.5; the model's declared window, default 128k).
func compressContext(msgs []llm.Msg, msgStep []int, currentStep, window int) bool {
	if window <= 0 {
		window = 128000
	}
	if historyBytes(msgs)/4 < window*7/10 {
		return false
	}
	changed := false
	for i, m := range msgs {
		if m.Role != "tool" || i >= len(msgStep) {
			continue
		}
		if msgStep[i] >= currentStep-6 {
			continue
		}
		if strings.HasPrefix(m.Content, "[earlier tool output elided") {
			continue
		}
		msgs[i].Content = fmt.Sprintf("[earlier tool output elided to save context: %d bytes]",
			len(m.Content))
		changed = true
	}
	return changed
}

func historyBytes(msgs []llm.Msg) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
		for _, c := range m.ToolCalls {
			n += len(c.Arguments)
		}
	}
	return n
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
