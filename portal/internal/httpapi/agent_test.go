// agent_runs_test.go — A3.1 HTTP acceptance (docs/AGENT_PLAN_A3.md
// §11 "A3.1 Xong khi"): the BFF flow (attach sandbox → create run →
// stream), the 409 single-active gate, the cancel budget, SSE replay
// with Last-Event-ID, cross-org 404 on BFF and /v1, the scope 403 and
// the agent-token run pin. Real Postgres; the LLM is a fake.
package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/agenttoken"
	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
)

// cleanAgent runs before each agent test: the agent tables plus the
// sandboxes/chats they reference (the admin pool bypasses RLS).
func cleanAgent(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	admin, err := db.Open(ctx, discardLogger(), url, "")
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()
	for _, table := range []string{"agent_runs", "chat_messages", "chat_conversations", "sandboxes"} {
		if _, err := admin.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
}

// agentLLM is a scriptable OpenAI-compatible fake: a fixed reply or a
// slow drip (for the cancel test).
type agentLLM struct {
	mu       sync.Mutex
	chunks   []string
	delay    time.Duration
	requests int
	srv      *httptest.Server
}

func newAgentLLM(t *testing.T, chunks []string, delay time.Duration) *agentLLM {
	f := &agentLLM{chunks: chunks, delay: delay}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for _, part := range f.chunks {
			if r.Context().Err() != nil {
				return
			}
			b, _ := json.Marshal(map[string]any{
				"choices": []any{map[string]any{"delta": map[string]any{"content": part}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			time.Sleep(f.delay)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// setupAgent wires the BFF for agent tests: no model_connections in
// the DB (resolveRunModel falls back to the env-style s.LLM), a fake
// upstream, an API-key verifier and the shared notifier.
func setupAgent(t *testing.T) (*Server, *agentLLM, *agent.Notifier) {
	t.Helper()
	srv := setupBFF(t) // skips without PORTAL_TEST_DATABASE_URL
	cleanAgent(t)
	fake := newAgentLLM(t, []string{"Tạo ", "file ", "xong."}, 5*time.Millisecond)
	srv.LLM = &llm.Client{BaseURL: fake.srv.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	srv.Keys = &apikeys.Verifier{Pool: srv.Pool, Log: discardLogger()}
	srv.Tokens, _ = agenttoken.NewSigner()
	notify := agent.NewNotifier(context.Background(), discardLogger(), os.Getenv("PORTAL_TEST_DATABASE_URL"))
	srv.AgentNotify = notify
	t.Cleanup(notify.Close)
	return srv, fake, notify
}

// startAgentWorker runs a worker against the test pool with fast
// timings; cleanup cancels and waits.
func startAgentWorker(t *testing.T, srv *Server, notify *agent.Notifier, src agent.LLMSource) *agent.Worker {
	t.Helper()
	w := &agent.Worker{
		ID:         fmt.Sprintf("httpapi-%d", time.Now().UnixNano()%100000),
		Log:        discardLogger(),
		Store:      agent.NewStore(srv.Pool),
		LLM:        src,
		Notify:     notify,
		Lease:      3 * time.Second,
		Heartbeat:  300 * time.Millisecond,
		ClaimEvery: 100 * time.Millisecond,
		CancelPoll: 200 * time.Millisecond,
		SweepEvery: 250 * time.Millisecond,
		SweepGrace: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() {
		cancel()
		w.Shutdown(0)
		w.Wait()
	})
	return w
}

// seedAgentSandbox inserts one running sandbox row for the org.
func seedAgentSandbox(t *testing.T, srv *Server, orgID, tag string) string {
	t.Helper()
	ctx := context.Background()
	id := "SBX" + fmt.Sprint(time.Now().UnixNano()) + "-" + tag
	if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO sandboxes (id, org_id, status, image, created_at, updated_at)
			VALUES ($1, $2::uuid, 'running', 'ubuntu:24.04', now(), now())`, id, orgID)
		return err
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
	return id
}

// waitForRunStatus polls GET /api/orgs/{org}/agent/runs/{id} until
// terminal, answering the last seen body.
func waitForRunStatus(t *testing.T, srv *Server, orgID, runID string, cookies []*http.Cookie, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var body string
	for time.Now().Before(deadline) {
		rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/agent/runs/"+runID, "", cookies...)
		body = rec.Body.String()
		if rec.Code == http.StatusOK &&
			(strings.Contains(body, `"status":"succeeded"`) ||
				strings.Contains(body, `"status":"failed"`) ||
				strings.Contains(body, `"status":"cancelled"`)) {
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s not terminal within %s: %s", runID, timeout, body)
	return body
}

// TestAgentBFFFlowAndGuards — criterion 1 (queued→succeeded with the
// full event contract), 2 (409 second run), sandbox attach rules and
// the sandbox_required guard.
func TestAgentBFFFlowAndGuards(t *testing.T) {
	srv, _, notify := setupAgent(t)
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM})

	sfx := fmt.Sprint(time.Now().UnixNano())
	aliceID, alice, aliceCSRF := login(t, srv, "agent-alice-"+sfx, "agent-alice-"+sfx+"@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "agent-bob-"+sfx, "agent-bob-"+sfx+"@example.test", "Bob")

	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Agent Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sbx := seedAgentSandbox(t, srv, orgID, "flow")

	// Bob (not a member): 404 on attach and create — no leak.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	chat := extractString(t, rec.Body.String(), "id")

	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sbx+`"}`, alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach sandbox = %d: %s", rec.Code, rec.Body.String())
	}
	// Foreign sandbox id attaches 404.
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"SBXNOTINORG"}`, alice, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attach foreign sandbox = %d, want 404", rec.Code)
	}
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sbx+`"}`, bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("attach as outsider = %d, want 404", rec.Code)
	}

	// Create on a chat without a sandbox → 400 sandbox_required.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	emptyChat := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+emptyChat+"/agent/runs", `{"prompt":"làm việc"}`, alice, aliceCSRF)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sandbox_required") {
		t.Fatalf("create without sandbox = %d: %s", rec.Code, rec.Body.String())
	}

	// The happy path: create → worker claims → succeeded.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", `{"prompt":"tạo trang login"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run = %d: %s", rec.Code, rec.Body.String())
	}
	runID := extractString(t, rec.Body.String(), "id")
	if !strings.Contains(rec.Body.String(), `"status":"queued"`) {
		t.Fatalf("create body = %s", rec.Body.String())
	}

	// Second run on the same sandbox → 409 run_in_progress (the first
	// is queued or running while this lands).
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", `{"prompt":"thứ hai"}`, alice, aliceCSRF)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "run_in_progress") {
		t.Fatalf("second run = %d: %s", rec.Code, rec.Body.String())
	}

	// Wait for success (criterion 1: queued → running → succeeded).
	final := waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)
	if !strings.Contains(final, `"status":"succeeded"`) {
		t.Fatalf("final = %s", final)
	}
	if !strings.Contains(final, `"summary":"Tạo file xong."`) || !strings.Contains(final, `"build_status":"skipped"`) {
		t.Fatalf("result missing summary/build_status: %s", final)
	}

	// SSE replay carries the full contract: status(queued), status
	// (running), message, done — dense seqs, done last.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/events?follow=false", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("events = %d: %s", rec.Code, rec.Body.String())
	}
	var events []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &events); err != nil {
		t.Fatalf("events json: %v (%s)", err, rec.Body.String())
	}
	if len(events) < 4 {
		t.Fatalf("events = %d, want >= 4", len(events))
	}
	types := []string{}
	for i, e := range events {
		if int(e["seq"].(float64)) != i+1 {
			t.Fatalf("seq gap at %d", i)
		}
		types = append(types, e["type"].(string))
	}
	if types[0] != "status" || types[len(types)-1] != "done" ||
		!contains(types, "message") || !contains(types, "status") {
		t.Fatalf("event types = %v", types)
	}
	doneData, _ := json.Marshal(events[len(events)-1]["data"])
	if !strings.Contains(string(doneData), `"status":"succeeded"`) {
		t.Fatalf("done data = %s", doneData)
	}

	// The summary landed in the chat as an assistant message tied to
	// the run.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats/"+chat+"/messages", "", alice, aliceCSRF)
	if !strings.Contains(rec.Body.String(), "Tạo file xong.") {
		t.Fatalf("chat messages missing summary: %s", rec.Body.String())
	}

	// List runs of the chat.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), runID) {
		t.Fatalf("list runs = %d: %s", rec.Code, rec.Body.String())
	}

	// Bob (still an outsider) cannot read the run or its events.
	for _, path := range []string{
		"/api/orgs/" + orgID + "/agent/runs/" + runID,
		"/api/orgs/" + orgID + "/agent/runs/" + runID + "/events?follow=false",
	} {
		if rec := do(t, srv, http.MethodGet, path, "", bob, bobCSRF); rec.Code != http.StatusNotFound {
			t.Fatalf("outsider GET %s = %d, want 404", path, rec.Code)
		}
	}
	_ = aliceID
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestAgentSSELastEventID — criterion 5: replay with Last-Event-ID 5
// answers exactly seq > 5 (no dup, no gap).
func TestAgentSSELastEventID(t *testing.T) {
	srv, _, _ := setupAgent(t)

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "agent-sse-"+sfx, "agent-sse-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"SSE Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sbx := seedAgentSandbox(t, srv, orgID, "sse")

	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sbx+`"}`, alice, aliceCSRF)

	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", `{"prompt":"p"}`, alice, aliceCSRF)
	runID := extractString(t, rec.Body.String(), "id")

	// Seed five more events so the run has seq 1..6.
	st := agent.NewStore(srv.Pool)
	for i := 0; i < 5; i++ {
		if _, err := st.AppendEvent(context.Background(), orgID, runID, "message",
			map[string]string{"delta": fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}

	fetch := func(lastEventID string, since string) string {
		target := "/api/orgs/" + orgID + "/agent/runs/" + runID + "/events?follow=false"
		if since != "" {
			target += "&since=" + since
		}
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(alice)
		req.AddCookie(aliceCSRF)
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, req)
		if rec2.Code != http.StatusOK {
			t.Fatalf("events = %d: %s", rec2.Code, rec2.Body.String())
		}
		return rec2.Body.String()
	}

	all := fetch("", "")
	if strings.Contains(all, `"seq":7`) || !strings.Contains(all, `"seq":6`) {
		t.Fatalf("full replay = %s", all)
	}
	for _, seq := range []int{1, 2, 3, 4, 5, 6} {
		if !strings.Contains(all, fmt.Sprintf(`"seq":%d`, seq)) {
			t.Fatalf("full replay missing seq %d: %s", seq, all)
		}
	}

	tail := fetch("5", "")
	if !strings.Contains(tail, `"seq":6`) || strings.Contains(tail, `"seq":5`) || strings.Contains(tail, `"seq":4`) {
		t.Fatalf("Last-Event-ID 5 replay = %s, want only seq 6", tail)
	}
	if count := strings.Count(tail, `"seq":`); count != 1 {
		t.Fatalf("Last-Event-ID 5 replay has %d events, want exactly 1", count)
	}

	// ?since= behaves the same as the header.
	sinceTail := fetch("", "2")
	if strings.Contains(sinceTail, `"seq":1`) || strings.Contains(sinceTail, `"seq":2`) {
		t.Fatalf("since=2 replay = %s, want from 3", sinceTail)
	}
	for _, seq := range []int{3, 4, 5, 6} {
		if !strings.Contains(sinceTail, fmt.Sprintf(`"seq":%d`, seq)) {
			t.Fatalf("since=2 replay missing seq %d", seq)
		}
	}
}

// TestAgentCancelLive — criterion 3: cancel during a slow stream
// stops the run in ≤5 s (SSE frame reader on a real HTTP server).
func TestAgentCancelLive(t *testing.T) {
	srv, _, notify := setupAgent(t)
	// Replace the fast fake with a slow one for this test.
	slow := newAgentLLM(t, makeAgentChunks(30, "đoạn "), 200*time.Millisecond) // ~6 s stream
	srv.LLM = &llm.Client{BaseURL: slow.srv.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM})

	// Real HTTP server so the SSE response can be read while another
	// request runs.
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{Timeout: 30 * time.Second}

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "agent-cancel-"+sfx, "agent-cancel-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Cancel Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sbx := seedAgentSandbox(t, srv, orgID, "cancel")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sbx+`"}`, alice, aliceCSRF)
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", `{"prompt":"viết dài"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run = %d: %s", rec.Code, rec.Body.String())
	}
	runID := extractString(t, rec.Body.String(), "id")

	// Open the SSE stream (cookie header for the raw client).
	cookiePair := sessionCookiePair(alice, aliceCSRF)
	sseReq, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/api/orgs/"+orgID+"/agent/runs/"+runID+"/events", nil)
	sseReq.Header.Set("Cookie", cookiePair)
	sse, err := client.Do(sseReq)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer sse.Body.Close()
	if sse.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d", sse.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(sse.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event:") {
				frames <- strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
		}
		close(frames)
	}()

	// Wait until the run is streaming (first message frame), then
	// cancel through the BFF.
waitLoop:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("sse closed before streaming")
			}
			if f == "message" {
				break waitLoop
			}
		case <-time.After(8 * time.Second):
			t.Fatal("no message frame within 8s")
		}
	}

	cancelStart := time.Now()
	cancelReq, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/orgs/"+orgID+"/agent/runs/"+runID+"/cancel", nil)
	cancelReq.Header.Set("Cookie", cookiePair)
	// The BFF enforces double-submit CSRF on every state change — the
	// raw client mirrors the SPA and echoes the CSRF cookie back.
	cancelReq.Header.Set("X-CSRF-Token", aliceCSRF.Value)
	cancelRes, err := client.Do(cancelReq)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer cancelRes.Body.Close()
	if cancelRes.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel = %d", cancelRes.StatusCode)
	}

	// The done frame must arrive within the 5 s budget.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("sse closed without done")
			}
			if f == "done" {
				elapsed := time.Since(cancelStart)
				if elapsed > 5*time.Second {
					t.Fatalf("cancel took %s, want <= 5s", elapsed)
				}
				// The run row says cancelled.
				rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/agent/runs/"+runID, "", alice, aliceCSRF)
				if !strings.Contains(rec.Body.String(), `"status":"cancelled"`) {
					t.Fatalf("run after cancel = %s", rec.Body.String())
				}
				return
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("done frame did not arrive within 5s of cancel")
		}
	}
}

func sessionCookiePair(session, csrf *http.Cookie) string {
	return session.Name + "=" + session.Value + "; " + csrf.Name + "=" + csrf.Value
}

func makeAgentChunks(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d ", s, i)
	}
	return out
}

// TestAgentV1AuthAndIsolation — criteria 6+7 over the public API:
// cross-org 404 (sandbox, run, events) and 403 without the agent
// scope, plus Idempotency-Key replay and the agent-token run pin.
func TestAgentV1AuthAndIsolation(t *testing.T) {
	srv, fake, notify := setupAgent(t)
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM})

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "agent-v1a-"+sfx, "agent-v1a-"+sfx+"@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "agent-v1b-"+sfx, "agent-v1b-"+sfx+"@example.test", "Bob")

	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"V1 Org A `+sfx+`"}`, alice, aliceCSRF)
	orgA := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs", `{"name":"V1 Org B `+sfx+`"}`, bob, bobCSRF)
	orgB := extractString(t, rec.Body.String(), "id")
	sbxA := seedAgentSandbox(t, srv, orgA, "v1a")

	// Keys: A has the agent scopes; B's key has NO agent scope; C is
	// org B's key WITH agent scopes (for the cross-org checks).
	keyA, _ := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgA, "agent-a",
		`["agent:run","agent:read"]`)
	keyBNoScope, _ := createKey(t, srv, []*http.Cookie{bob, bobCSRF}, orgB, "no-scope",
		`["sandbox:read"]`)
	keyB, _ := createKey(t, srv, []*http.Cookie{bob, bobCSRF}, orgB, "agent-b",
		`["agent:run","agent:read"]`)

	v1 := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec2 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec2, req)
		return rec2
	}

	// No key → 401.
	if rec := v1(http.MethodPost, "/v1/sandboxes/"+sbxA+"/agent/runs", `{"prompt":"p"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", rec.Code)
	}
	// Wrong scope → 403 (criterion 7).
	if rec := v1(http.MethodPost, "/v1/sandboxes/"+sbxA+"/agent/runs", `{"prompt":"p"}`, keyBNoScope); rec.Code != http.StatusForbidden {
		t.Fatalf("scope-less key = %d: %s, want 403", rec.Code, rec.Body.String())
	}
	if rec := v1(http.MethodGet, "/v1/sandboxes/"+sbxA+"/agent/runs", "", keyBNoScope); rec.Code != http.StatusForbidden {
		t.Fatalf("scope-less read = %d, want 403", rec.Code)
	}
	// Foreign org's sandbox → 404 (criterion 6), never 403.
	if rec := v1(http.MethodPost, "/v1/sandboxes/"+sbxA+"/agent/runs", `{"prompt":"p"}`, keyB); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org create = %d: %s, want 404", rec.Code, rec.Body.String())
	}

	// A creates a run through /v1 with an Idempotency-Key.
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sbxA+"/agent/runs", strings.NewReader(`{"prompt":"v1 chạy giúp tôi"}`))
	req.Header.Set("Authorization", "Bearer "+keyA)
	req.Header.Set("Idempotency-Key", "v1-idem-1")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("v1 create = %d: %s", rec2.Code, rec2.Body.String())
	}
	runID := extractString(t, rec2.Body.String(), "id")

	// Replay: same key header, same run, replay marker.
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sbxA+"/agent/runs", strings.NewReader(`{"prompt":"v1 chạy giúp tôi"}`))
	req.Header.Set("Authorization", "Bearer "+keyA)
	req.Header.Set("Idempotency-Key", "v1-idem-1")
	rec2 = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusCreated || rec2.Header().Get("Idempotency-Replayed") != "true" ||
		extractString(t, rec2.Body.String(), "id") != runID {
		t.Fatalf("replay = %d %s %s", rec2.Code, rec2.Header().Get("Idempotency-Replayed"), rec2.Body.String())
	}

	// Reads: A yes; B (agent scopes, foreign org) 404 — run, events,
	// list.
	if rec := v1(http.MethodGet, "/v1/agent/runs/"+runID, "", keyA); rec.Code != http.StatusOK {
		t.Fatalf("v1 get run = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := v1(http.MethodGet, "/v1/agent/runs/"+runID+"?follow=false", "", keyA); rec.Code != http.StatusOK {
		t.Fatalf("v1 events = %d", rec.Code)
	}
	if rec := v1(http.MethodGet, "/v1/agent/runs/"+runID, "", keyB); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org run = %d: %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := v1(http.MethodGet, "/v1/agent/runs/"+runID+"/events", "", keyB); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org events = %d, want 404", rec.Code)
	}
	if rec := v1(http.MethodGet, "/v1/sandboxes/"+sbxA+"/agent/runs", "", keyB); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org list = %d, want 404", rec.Code)
	}

	// The run finishes with the fake.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rec := v1(http.MethodGet, "/v1/agent/runs/"+runID, "", keyA); strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Agent token: A mints a token pinned to the run (agent:read only).
	req = httptest.NewRequest(http.MethodPost, "/v1/tokens",
		strings.NewReader(`{"scopes":["agent:read"],"agent_run_id":"`+runID+`"}`))
	req.Header.Set("Authorization", "Bearer "+keyA)
	rec2 = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("mint token = %d: %s", rec2.Code, rec2.Body.String())
	}
	tok := extractString(t, rec2.Body.String(), "token")

	v1t := func(method, path string) *httptest.ResponseRecorder {
		req3 := httptest.NewRequest(method, path, nil)
		req3.Header.Set("Authorization", "Bearer "+tok)
		rec3 := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec3, req3)
		return rec3
	}
	if rec := v1t(http.MethodGet, "/v1/agent/runs/"+runID); rec.Code != http.StatusOK {
		t.Fatalf("token get run = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := v1t(http.MethodGet, "/v1/agent/runs/"+runID+"/events"); rec.Code != http.StatusOK {
		t.Fatalf("token events = %d", rec.Code)
	}
	// Pinned token cannot read a different run: same 404 as foreign.
	other := seedAgentSandbox(t, srv, orgA, "other")
	req = httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+other+"/agent/runs", strings.NewReader(`{"prompt":"x"}`))
	req.Header.Set("Authorization", "Bearer "+keyA)
	rec2 = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second run = %d: %s", rec2.Code, rec2.Body.String())
	}
	otherRun := extractString(t, rec2.Body.String(), "id")
	if rec := v1t(http.MethodGet, "/v1/agent/runs/"+otherRun); rec.Code != http.StatusNotFound {
		t.Fatalf("pinned token other run = %d, want 404", rec.Code)
	}
	// Token cannot create or cancel (scope agent:read only).
	if rec := v1t(http.MethodPost, "/v1/agent/runs/"+runID+"/cancel"); rec.Code == http.StatusAccepted {
		t.Fatalf("read-only token cancelled a run")
	}
	_ = fake
}
