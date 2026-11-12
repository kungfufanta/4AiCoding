// agent package tests: the durable queue, the worker lifecycle and
// the cancel/lost paths. Real Postgres via PORTAL_TEST_DATABASE_URL
// (same gate as the rest of the portal tests); the LLM is a fake
// SSE server so no provider is contacted.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// dbBundle bundles the pool with its URL (the notifier needs a
// dedicated connection on the same database).
type dbBundle struct {
	Pool *pgxpool.Pool
	URL  string
}

// setupDB opens the pool and applies migrations; skips when the env
// is unset. The agent package gets its OWN database (cloned schema,
// dropped at cleanup): `go test ./...` runs packages in parallel and
// several of them (httpapi a1/p5b/sandboxes setups) truncate the
// shared tables — long-lived worker state (leases, sweeps, a run
// executing over seconds) cannot survive another package deleting
// its rows mid-test. An isolated database removes the interference
// in both directions; DROP ... WITH (FORCE) reaps any connection the
// notifier or a worker still holds.
func setupDB(t *testing.T) *dbBundle {
	t.Helper()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	agentURL := url[:strings.LastIndex(url, "/")+1] + "portal_test_agent"

	adminConn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect shared db: %v", err)
	}
	drop := func() {
		_, _ = adminConn.Exec(context.Background(), `DROP DATABASE IF EXISTS portal_test_agent WITH (FORCE)`)
	}
	drop()
	if _, err := adminConn.Exec(ctx, `CREATE DATABASE portal_test_agent`); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create agent test db: %v", err)
	}
	t.Cleanup(func() {
		drop()
		_ = adminConn.Close(context.Background())
	})

	admin, err := db.Open(ctx, testLogger(), agentURL, "")
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	if err := db.Migrate(ctx, admin, testLogger()); err != nil {
		admin.Close()
		t.Fatalf("migrate: %v", err)
	}
	admin.Close()
	pool, err := db.Open(ctx, testLogger(), agentURL, "portal_app")
	if err != nil {
		t.Fatalf("open app pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &dbBundle{Pool: pool, URL: agentURL}
}

// seedOrg creates an org (+ its owner user) and a sandbox row, the
// minimum the agent_runs FKs need.
func seedOrg(t *testing.T, st *Store, tag string) (orgID, userID, sandboxID string) {
	t.Helper()
	ctx := context.Background()
	orgID, err := db.NewOrgID()
	if err != nil {
		t.Fatal(err)
	}
	// BootstrapTx pins app.org_id to the brand-new org id — orgs RLS
	// requires it on INSERT (same shape as handleCreateOrg). Emails
	// and slugs are unique per call: the users/orgs tables are not
	// cleaned between runs.
	uniq := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano()%1000000)
	if err := db.BootstrapTx(ctx, st.Pool, orgID, func(tx db.Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, "agent-"+uniq, "agent-"+uniq+"@example.test", "Agent "+uniq)
		if err != nil {
			return err
		}
		userID = u.ID
		_, err = tenants.CreateOrg(ctx, tx, orgID, "agent-"+uniq, "Agent Org "+uniq, u.ID)
		return err
	}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	sandboxID = seedSandbox(t, st, orgID, tag)
	return orgID, userID, sandboxID
}

// seedSandbox adds one running sandbox row to an existing org.
func seedSandbox(t *testing.T, st *Store, orgID, tag string) string {
	t.Helper()
	ctx := context.Background()
	sandboxID := "SBX" + fmt.Sprint(time.Now().UnixNano()) + "-" + tag
	if err := db.WithOrg(ctx, st.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO sandboxes (id, org_id, status, image, created_at, updated_at)
                        VALUES ($1, $2::uuid, 'running', 'ubuntu:24.04', now(), now())`, sandboxID, orgID)
		return err
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	return sandboxID
}

// slowLLM is an OpenAI-compatible fake that streams chunks with a
// delay, records who called, and never returns tool calls (A3.1).
type slowLLM struct {
	mu       sync.Mutex
	delay    time.Duration
	chunks   []string
	requests int
	headers  []http.Header
	srv      *httptest.Server
}

func newSlowLLM(t *testing.T, chunks []string, delay time.Duration) *slowLLM {
	f := &slowLLM{chunks: chunks, delay: delay}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for _, part := range chunks {
			if r.Context().Err() != nil {
				return // client cancelled: stop streaming
			}
			b, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"content": part},
				}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			time.Sleep(f.delay)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *slowLLM) client() *llm.Client {
	return &llm.Client{BaseURL: f.srv.URL, APIKey: "wh_test", Model: "glm-4.6", Flavor: "webhub"}
}

type fixedSource struct{ c *llm.Client }

func (s fixedSource) Client(_ context.Context, run Run) (*llm.Client, error) {
	c := *s.c
	c.Model = run.Model
	return &c, nil
}

// newWorker builds a worker with test-fast timings.
func newWorker(st *Store, src LLMSource, notify *Notifier) *Worker {
	return &Worker{
		ID:         fmt.Sprintf("test-%d", time.Now().UnixNano()%100000),
		Log:        testLogger(),
		Store:      st,
		LLM:        src,
		Notify:     notify,
		Lease:      3 * time.Second,
		Heartbeat:  300 * time.Millisecond,
		ClaimEvery: 100 * time.Millisecond,
		CancelPoll: 200 * time.Millisecond,
		SweepEvery: 250 * time.Millisecond,
		SweepGrace: time.Second,
	}
}

// waitForStatus polls until the run reaches a terminal state.
func waitForStatus(t *testing.T, st *Store, orgID, runID string, timeout time.Duration) Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := st.GetRun(context.Background(), orgID, runID)
		if err == nil && Terminal(run.Status) {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %s", runID, timeout)
	return Run{}
}

func eventTypes(t *testing.T, st *Store, orgID, runID string) []Event {
	t.Helper()
	events, err := st.EventsSince(context.Background(), orgID, runID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return events
}

// TestRunLifecycle — criterion 1: a run with a fake LLM walks
// queued → running → succeeded with status/message/done events.
func TestRunLifecycle(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, userID, sandboxID := seedOrg(t, st, "lifecycle")

	fake := newSlowLLM(t, []string{"Xin ", "chào, ", "đã xong."}, 10*time.Millisecond)
	notify := NewNotifier(context.Background(), testLogger(), handle.URL)
	defer func() { _ = notify }()
	w := newWorker(st, fixedSource{fake.client()}, notify)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	defer func() { cancel(); w.Shutdown(0); w.Wait() }()

	// A conversation so the summary lands in chat_messages. RLS pins
	// org AND user, so the worker-side insert runs with the creator
	// pinned (exactly what SaveSummary does).
	var chatID string
	if err := db.WithOrg(db.WithUserID(ctx, userID), st.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(ctx, `
                        INSERT INTO chat_conversations (org_id, user_id, title)
                        VALUES ($1::uuid, $2::uuid, 'agent flow')
                        RETURNING id::text`, orgID, userID).Scan(&chatID)
	}); err != nil {
		t.Fatal(err)
	}

	// The conversation check rides the caller's RLS context (chat
	// tables pin org AND user) — pin the creator like the BFF does.
	run, replayed, err := st.CreateRun(db.WithUserID(ctx, userID), CreateIn{
		OrgID: orgID, SandboxID: sandboxID, ConversationID: chatID,
		CreatedByUser: userID, Prompt: "Viết hello world",
		Model: "glm-4.6", MaxSteps: 30,
	})
	if err != nil || replayed {
		t.Fatalf("create run: %v (replayed=%v)", err, replayed)
	}

	fin := waitForStatus(t, st, orgID, run.ID, 10*time.Second)
	if fin.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", fin.Status, fin.ErrorCode)
	}
	if fin.Steps != 1 {
		t.Fatalf("steps = %d, want 1", fin.Steps)
	}
	if fin.InputTokens <= 0 || fin.OutputTokens <= 0 {
		t.Fatalf("tokens = %d/%d, want estimated > 0", fin.InputTokens, fin.OutputTokens)
	}
	var result runResultJSON
	if err := json.Unmarshal(fin.Result, &result); err != nil {
		t.Fatalf("result json: %v", err)
	}
	if result.Summary == "" || result.BuildStatus != "skipped" || result.FilesChanged == nil {
		t.Fatalf("result = %+v", result)
	}

	// Event contract: status(queued) → status(running) → message* → done.
	events := eventTypes(t, st, orgID, run.ID)
	if len(events) < 4 {
		t.Fatalf("events = %d, want >= 4", len(events))
	}
	var firstPhase struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal(events[0].Data, &firstPhase)
	if events[0].Type != "status" || firstPhase.Phase != "queued" {
		t.Fatalf("first event = %s %s (phase=%q)", events[0].Type, events[0].Data, firstPhase.Phase)
	}
	found := map[string]bool{}
	for _, e := range events {
		found[e.Type] = true
		if e.Type == "done" {
			var done struct {
				Status    string `json:"status"`
				ErrorCode string `json:"error_code"`
			}
			_ = json.Unmarshal(e.Data, &done)
			if done.Status != StatusSucceeded || done.ErrorCode != "" {
				t.Fatalf("done = %s/%s", done.Status, done.ErrorCode)
			}
		}
	}
	for _, want := range []string{"status", "message", "done"} {
		if !found[want] {
			t.Fatalf("missing %s event in %v", want, eventTypes(t, st, orgID, run.ID))
		}
	}
	// Seqs are dense 1..n.
	for i, e := range events {
		if e.Seq != i+1 {
			t.Fatalf("seq gap at %d: %d", i, e.Seq)
		}
	}

	// The summary landed as an assistant chat message (read with the
	// creator pinned — chat_messages RLS pins org AND user).
	var summaries []string
	if err := db.WithOrg(db.WithUserID(ctx, userID), st.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `
                        SELECT content FROM chat_messages WHERE agent_run_id = $1::uuid`, run.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			summaries = append(summaries, c)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0] != result.Summary {
		t.Fatalf("chat summaries = %v, want [%s]", summaries, result.Summary)
	}

	// The full LLM history is stored for "continue".
	msgs, err := st.LoadMessages(context.Background(), orgID, run.ID)
	if err != nil || len(msgs) != 2 || msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Fatalf("stored messages = %v (%v)", msgs, err)
	}

	// The fake got the run prompt with the WebHub tenant headers.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.requests != 1 {
		t.Fatalf("llm requests = %d", fake.requests)
	}
	h := fake.headers[0]
	if h.Get("Authorization") != "Bearer wh_test" || h.Get("X-WebHub-User") != "u_"+userID {
		t.Fatalf("headers = %v", h)
	}
}

// TestOneActiveRunPerSandbox — criterion 2: a second run on the same
// sandbox is ErrRunInProgress until the first finishes.
func TestOneActiveRunPerSandbox(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "one-active")
	ctx := context.Background()

	in := CreateIn{OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "m", MaxSteps: 5}
	first, _, err := st.CreateRun(ctx, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, _, err := st.CreateRun(ctx, in); err != ErrRunInProgress {
		t.Fatalf("second = %v, want ErrRunInProgress", err)
	}
	// A different sandbox of the SAME org is unaffected.
	sandbox2 := seedSandbox(t, st, orgID, "one-active2")
	if _, _, err := st.CreateRun(ctx, CreateIn{OrgID: orgID, SandboxID: sandbox2, Prompt: "p", Model: "m", MaxSteps: 5}); err != nil {
		t.Fatalf("other sandbox: %v", err)
	}
	// After the first finishes, the sandbox frees up.
	if err := st.SetFinal(ctx, orgID, first.ID, FinalState{Status: StatusSucceeded, Steps: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateRun(ctx, in); err != nil {
		t.Fatalf("after finish: %v", err)
	}
}

// TestIdempotentCreate — the Idempotency-Key replays the same run for
// the same sandbox and rejects a different one.
func TestIdempotentCreate(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "idem")
	ctx := context.Background()
	in := CreateIn{OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "m", MaxSteps: 5, IdempotencyKey: "idem-1"}
	first, replayed, err := st.CreateRun(ctx, in)
	if err != nil || replayed {
		t.Fatalf("first: %v replayed=%v", err, replayed)
	}
	second, replayed, err := st.CreateRun(ctx, in)
	if err != nil || !replayed {
		t.Fatalf("replay: %v replayed=%v", err, replayed)
	}
	if second.ID != first.ID {
		t.Fatalf("replay returned different run %s != %s", second.ID, first.ID)
	}
	_, _, other := seedOrg(t, st, "idem2")
	if _, _, err := st.CreateRun(ctx, CreateIn{OrgID: orgID, SandboxID: other, Prompt: "p", Model: "m", MaxSteps: 5, IdempotencyKey: "idem-1"}); err != ErrIdempotencyKeyReuse {
		t.Fatalf("different sandbox same key = %v, want ErrIdempotencyKeyReuse", err)
	}
}

// TestCancelWithin5s — criterion 3: cancel during a slow stream stops
// the run in ≤5 s with status cancelled.
func TestCancelWithin5s(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "cancel")

	// ~6 s of stream so the cancel lands mid-stream.
	fake := newSlowLLM(t, makeChunks(30, "đoạn "), 200*time.Millisecond)
	notify := NewNotifier(context.Background(), testLogger(), handle.URL)
	w := newWorker(st, fixedSource{fake.client()}, notify)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	defer func() { cancel(); w.Shutdown(0); w.Wait() }()

	run, _, err := st.CreateRun(ctx, CreateIn{
		OrgID: orgID, SandboxID: sandboxID, Prompt: "viết dài", Model: "glm-4.6", MaxSteps: 30,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the LLM call is in flight, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := st.GetRun(ctx, orgID, run.ID)
		if r.Status == StatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	start := time.Now()
	if _, err := st.RequestCancel(ctx, orgID, run.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	fin := waitForStatus(t, st, orgID, run.ID, 5*time.Second)
	elapsed := time.Since(start)
	if fin.Status != StatusCancelled {
		t.Fatalf("status = %s (%s), want cancelled", fin.Status, fin.ErrorCode)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancel took %s, want <= 5s", elapsed)
	}
	if !fin.CancelRequested {
		t.Fatalf("cancel_requested not persisted")
	}
	// The done event closes the stream.
	events := eventTypes(t, st, orgID, run.ID)
	last := events[len(events)-1]
	if last.Type != "done" {
		t.Fatalf("last event = %s, want done", last.Type)
	}
	var done struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(last.Data, &done)
	if done.Status != StatusCancelled {
		t.Fatalf("done.status = %s", done.Status)
	}
}

// TestWorkerLostSwept — criterion 4: a worker that dies mid-run
// (claim without ever renewing) is reaped failed/worker_lost within
// the sweep budget, with its done event.
func TestWorkerLostSwept(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "lost")
	ctx := context.Background()

	run, _, err := st.CreateRun(ctx, CreateIn{
		OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "m", MaxSteps: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// "Crash": claim with a 1-second lease and never come back.
	if _, ok, err := st.ClaimNext(ctx, "crashed-worker", time.Second); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	// A healthy worker's sweeper reaps the stale lease (grace 0.5 s).
	w := newWorker(st, fixedSource{newSlowLLM(t, []string{"hi"}, 5*time.Millisecond).client()}, nil)
	wctx, wcancel := context.WithCancel(ctx)
	w.Start(wctx)
	defer func() { wcancel(); w.Shutdown(0); w.Wait() }()

	fin := waitForStatus(t, st, orgID, run.ID, 6*time.Second)
	if fin.Status != StatusFailed || fin.ErrorCode != ErrWorkerLost {
		t.Fatalf("status = %s/%s, want failed/worker_lost", fin.Status, fin.ErrorCode)
	}
	events := eventTypes(t, st, orgID, run.ID)
	last := events[len(events)-1]
	if last.Type != "done" || !strings.Contains(string(last.Data), "worker_lost") {
		t.Fatalf("last event = %s %s, want done/worker_lost", last.Type, last.Data)
	}
}

// TestNotifierWake — the LISTEN connection delivers notifications to
// subscribers within a second; the cancel channel carries the run id.
func TestNotifierWake(t *testing.T) {
	handle := setupDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notify := NewNotifier(ctx, testLogger(), handle.URL)
	ch, stop := notify.Subscribe()
	defer stop()

	// Fire from a second connection (like the API transaction does).
	// The LISTEN connection may still be dialling when the test
	// starts, so retry the notify until the wake arrives.
	runID := "11111111-2222-3333-4444-555555555555"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := handle.Pool.Exec(ctx, `SELECT pg_notify('agent_cancel', $1)`, runID); err != nil {
			t.Fatal(err)
		}
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("subscription closed unexpectedly")
			}
			if msg.Channel != NotifyCancel || msg.RunID != runID {
				t.Fatalf("msg = %+v", msg)
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no notification within 5s (LISTEN broken)")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestEventsSinceCursor — criterion 5's data path: replay returns
// exactly seq > since.
func TestEventsSinceCursor(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "cursor")
	ctx := context.Background()
	run, _, err := st.CreateRun(ctx, CreateIn{OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "m", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := st.AppendEvent(ctx, orgID, run.ID, "message", map[string]string{"delta": fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.EventsSince(ctx, orgID, run.ID, 0)
	if err != nil || len(all) != 6 {
		t.Fatalf("all = %d (%v), want 6", len(all), err)
	}
	tail, err := st.EventsSince(ctx, orgID, run.ID, 5)
	if err != nil || len(tail) != 1 || tail[0].Seq != 6 {
		t.Fatalf("since=5 → %+v (%v), want exactly seq 6", tail, err)
	}
}

// TestSaveLoadMessages round-trips the LLM history.
func TestSaveLoadMessages(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, _, sandboxID := seedOrg(t, st, "msgs")
	ctx := context.Background()
	run, _, err := st.CreateRun(ctx, CreateIn{OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "m", MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Msg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Name: "write_file", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: "ok"},
	}
	if err := st.SaveMessages(ctx, orgID, run.ID, msgs); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadMessages(ctx, orgID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[2].ToolCalls[0].Name != "write_file" || got[3].Content != "ok" {
		t.Fatalf("round trip = %+v", got)
	}
}

func makeChunks(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d ", s, i)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
