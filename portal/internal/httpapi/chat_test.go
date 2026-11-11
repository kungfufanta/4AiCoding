package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
)

func TestChatTitle(t *testing.T) {
	if got := chatTitle("  hello \n  world "); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("ă", 80)
	got := chatTitle(long)
	if !strings.HasSuffix(got, "…") || len([]rune(got)) != chatTitleRunes+1 {
		t.Fatalf("got %q (%d runes)", got, len([]rune(got)))
	}
}

func TestTrimHistory(t *testing.T) {
	var msgs []llm.Message
	for i := 0; i < 60; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: fmt.Sprint(i)})
	}
	got := trimHistory(msgs)
	if len(got) != chatHistoryMessages || got[len(got)-1].Content != "59" {
		t.Fatalf("len=%d last=%q", len(got), got[len(got)-1].Content)
	}
	big := strings.Repeat("x", chatHistoryBytes)
	got = trimHistory([]llm.Message{{Content: "old"}, {Content: big}, {Content: big}})
	if len(got) != 1 || got[0].Content != big {
		t.Fatalf("byte budget: len=%d", len(got))
	}
}

// fakeLLM is an OpenAI-compatible stream server that records what the
// Portal sent.
type fakeLLM struct {
	mu       sync.Mutex
	fail     bool
	requests []struct {
		header   http.Header
		messages []llm.Message
	}
}

func (f *fakeLLM) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.requests = append(f.requests, struct {
			header   http.Header
			messages []llm.Message
		}{r.Header.Clone(), body.Messages})
		fail := f.fail
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{"Chào ", "bạn"} {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": part}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestChatFlow(t *testing.T) {
	srv := setupBFF(t)
	fake := &fakeLLM{}
	hub := fake.server(t)

	sfx := uuid.NewString()[:8]
	aliceID, alice, aliceCSRF := login(t, srv, "chat-alice-"+sfx, "chat-alice-"+sfx+"@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "chat-bob-"+sfx, "chat-bob-"+sfx+"@example.test", "Bob")

	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Chat Org `+sfx+`"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org = %d: %s", rec.Code, rec.Body.String())
	}
	orgID := extractString(t, rec.Body.String(), "id")
	base := "/api/orgs/" + orgID + "/chats"

	rec = do(t, srv, http.MethodPost, base, "", alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	chat := extractString(t, rec.Body.String(), "id")

	// Not configured yet → 503 and nothing stored.
	rec = do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"hi"}`, alice, aliceCSRF)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("send without LLM = %d", rec.Code)
	}
	rec = do(t, srv, http.MethodGet, "/api/chat/status", "", alice, aliceCSRF)
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("status = %s", rec.Body.String())
	}

	srv.LLM = &llm.Client{BaseURL: hub.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}

	// Validation.
	rec = do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"   "}`, alice, aliceCSRF)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty content = %d", rec.Code)
	}

	rec = do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"Viết giúp tôi trang landing page bán cà phê"}`, alice, aliceCSRF)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("send = %d %s: %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"event: user", "event: delta", `"text":"Chào "`, "event: done", `"title":"Viết giúp tôi trang landing page bán cà phê"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %q:\n%s", want, out)
		}
	}
	h := fake.requests[0].header
	if h.Get("X-WebHub-User") != "u_"+aliceID || h.Get("X-Session-Id") != "s_"+chat || h.Get("Authorization") != "Bearer wh_test" {
		t.Fatalf("hub headers = %v", h)
	}

	// Second turn carries the history.
	rec = do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"thêm menu"}`, alice, aliceCSRF)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"title":"thêm`) {
		t.Fatalf("second send = %d: %s", rec.Code, rec.Body.String())
	}
	msgs := fake.requests[1].messages
	if len(msgs) != 4 || msgs[0].Role != "system" || msgs[2].Content != "Chào bạn" || msgs[3].Content != "thêm menu" {
		t.Fatalf("history sent = %+v", msgs)
	}

	rec = do(t, srv, http.MethodGet, base+"/"+chat+"/messages", "", alice, aliceCSRF)
	var stored []chatMessageJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &stored)
	if rec.Code != http.StatusOK || len(stored) != 4 || stored[1].Role != "assistant" {
		t.Fatalf("messages = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, base, "", alice, aliceCSRF)
	if !strings.Contains(rec.Body.String(), "landing page") {
		t.Fatalf("list = %s", rec.Body.String())
	}

	// Upstream failure: error event, user turn kept, no assistant turn.
	fake.mu.Lock()
	fake.fail = true
	fake.mu.Unlock()
	rec = do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"lỗi?"}`, alice, aliceCSRF)
	if !strings.Contains(rec.Body.String(), "event: error") || strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("failing upstream stream = %s", rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, base+"/"+chat+"/messages", "", alice, aliceCSRF)
	stored = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &stored)
	if len(stored) != 5 || stored[4].Role != "user" {
		t.Fatalf("after failure = %s", rec.Body.String())
	}

	// Bob, not a member: 404 everywhere.
	for _, p := range []string{base, base + "/" + chat + "/messages"} {
		if rec := do(t, srv, http.MethodGet, p, "", bob, bobCSRF); rec.Code != http.StatusNotFound {
			t.Fatalf("outsider GET %s = %d", p, rec.Code)
		}
	}

	// Bob joins the org: still cannot see Alice's conversation.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites", `{"email":"chat-bob-`+sfx+`@example.test","role":"admin"}`, alice, aliceCSRF)
	inviteURL := extractString(t, rec.Body.String(), "url")
	token := inviteURL[strings.Index(inviteURL, "token=")+len("token="):]
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept", `{"token":"`+token+`"}`, bob, bobCSRF); rec.Code != http.StatusOK {
		t.Fatalf("accept = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, base, "", bob, bobCSRF)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("org-mate list = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, http.MethodGet, base+"/"+chat+"/messages", "", bob, bobCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("org-mate read = %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodPost, base+"/"+chat+"/messages", `{"content":"x"}`, bob, bobCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("org-mate send = %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodDelete, base+"/"+chat, "", bob, bobCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("org-mate delete = %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, base+"/not-a-uuid/messages", "", alice, aliceCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d", rec.Code)
	}

	// Alice deletes it.
	if rec := do(t, srv, http.MethodDelete, base+"/"+chat, "", alice, aliceCSRF); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, base+"/"+chat+"/messages", "", alice, aliceCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete = %d", rec.Code)
	}
}
