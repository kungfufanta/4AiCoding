package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// streamOnlyHub mimics the z.ai sandbox WebHub: stream=false → 400,
// no /models (404), tools ignored.
func streamOnlyHub(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
			return
		}
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			http.Error(w, `{"error":{"message":"stream must be true"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
}

func TestTestFallsBackToStream(t *testing.T) {
	srv := streamOnlyHub(t)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1", Flavor: "webhub"}
	res := c.Test(context.Background(), "glm-5.3")
	if !res.OK || res.Reply != "ok" {
		t.Fatalf("Test = %+v, want OK with reply ok", res)
	}
}

func TestListModelsUnsupported(t *testing.T) {
	srv := streamOnlyHub(t)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/v1"}
	if _, err := c.ListModels(context.Background()); !errors.Is(err, ErrModelsUnsupported) {
		t.Fatalf("ListModels err = %v, want ErrModelsUnsupported", err)
	}
}
