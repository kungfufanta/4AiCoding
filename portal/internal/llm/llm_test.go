package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeHub(t *testing.T, chunks []string, status int, seen *http.Request, seenBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		*seen = *r.Clone(context.Background())
		_ = json.NewDecoder(r.Body).Decode(seenBody)
		if status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"account secret detail"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		for _, c := range chunks {
			b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": c}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestStreamWebHub(t *testing.T) {
	var seen http.Request
	var body map[string]any
	srv := fakeHub(t, []string{"Xin ", "chào", "!"}, 200, &seen, &body)
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/v1/", APIKey: "wh_secret", Model: "glm-4.6", Flavor: "webhub"}
	var got []string
	reply, err := c.Stream(context.Background(), Request{
		Messages:  []Message{{Role: "user", Content: "hi"}},
		UserID:    "11111111-1111-1111-1111-111111111111",
		SessionID: "22222222-2222-2222-2222-222222222222",
		RequestID: "req-1",
	}, func(d string) error { got = append(got, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Xin chào!" || len(got) != 3 {
		t.Fatalf("reply=%q deltas=%v", reply, got)
	}
	if seen.Header.Get("Authorization") != "Bearer wh_secret" ||
		seen.Header.Get("X-WebHub-User") != "u_11111111-1111-1111-1111-111111111111" ||
		seen.Header.Get("X-Session-Id") != "s_22222222-2222-2222-2222-222222222222" ||
		seen.Header.Get("X-Request-Id") != "req-1" {
		t.Fatalf("headers = %v", seen.Header)
	}
	if body["model"] != "glm-4.6" || body["stream"] != true {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["user"]; ok {
		t.Fatal("webhub flavor must use the header, not the body user field")
	}
}

func TestStreamGenericUsesUserField(t *testing.T) {
	var seen http.Request
	var body map[string]any
	srv := fakeHub(t, []string{"ok"}, 200, &seen, &body)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m", Flavor: "generic"}
	if _, err := c.Stream(context.Background(), Request{UserID: "abc", SessionID: "s"}, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if body["user"] != "u_abc" || seen.Header.Get("X-WebHub-User") != "" || seen.Header.Get("X-Session-Id") != "" {
		t.Fatalf("body=%v headers=%v", body, seen.Header)
	}
}

func TestStreamUpstreamErrorHidesBody(t *testing.T) {
	var seen http.Request
	var body map[string]any
	srv := fakeHub(t, nil, 401, &seen, &body)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	_, err := c.Stream(context.Background(), Request{}, func(string) error { return nil })
	if !errors.Is(err, ErrUpstream) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamOutputCap(t *testing.T) {
	var seen http.Request
	var body map[string]any
	srv := fakeHub(t, []string{"aaaa", "bbbb", "cccc"}, 200, &seen, &body)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m", MaxOutputBytes: 6}
	reply, err := c.Stream(context.Background(), Request{}, func(string) error { return nil })
	if !errors.Is(err, ErrUpstream) || reply != "aaaa" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}

func TestStreamCallbackAbort(t *testing.T) {
	var seen http.Request
	var body map[string]any
	srv := fakeHub(t, []string{"a", "b"}, 200, &seen, &body)
	defer srv.Close()
	stop := errors.New("client gone")
	c := &Client{BaseURL: srv.URL + "/v1", Model: "m"}
	reply, err := c.Stream(context.Background(), Request{}, func(string) error { return stop })
	if !errors.Is(err, stop) || reply != "a" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
}
