package authproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/agentauth"
)

// Use real HTTP on both sides of the production reverse proxy. Bodies remain
// opaque: neither explicit legacy/custom models nor reasoning/tool/media history
// may be rewritten, and omitted effort must remain omitted for provider defaults.
func TestMiniMaxHTTPPayloadPreservation(t *testing.T) {
	for _, route := range []struct {
		up, base, suffix, history string
	}{
		{"minimax", "/v1", "/chat/completions", "chat"},
		{"minimax-cn", "/v1", "/chat/completions", "chat"},
		{"minimax-anthropic", "/anthropic", "/v1/messages", "anthropic"},
		{"minimax-anthropic-cn", "/anthropic", "/v1/messages", "anthropic"},
		// Native Responses is documented for the global endpoint. Keep full
		// client-side history rather than relying on previous_response_id.
		{"minimax", "/v1", "/responses", "responses"},
	} {
		t.Run(route.up+route.suffix, func(t *testing.T) {
			var gotBody []byte
			var gotPath, gotAuth, gotAPIKey string
			up := stubUpstream(t, route.up, func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				gotPath, gotAuth, gotAPIKey = r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"delta\":\"ok\"}\n\ndata: [DONE]\n\n")
			})
			upstreams[route.up] = up.URL + route.base
			st := agentauth.NewStore(t.TempDir())
			writeAPIKey(t, st, "minimax", "fixture-minimax-key")
			proxy := httptest.NewServer(New(st, nil))
			defer proxy.Close()
			for _, model := range []string{"MiniMax-M3.1-Flash", "MiniMax-M3", "MiniMax-M2.7", "custom-model"} {
				for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
					t.Run(model+"/effort="+effort, func(t *testing.T) {
						var payload map[string]any
						history := map[string]string{
							"chat":      `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]},{"role":"assistant","content":null,"reasoning_content":"retained reasoning","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`,
							"anthropic": `{"max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},{"type":"text","text":"describe"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"retained reasoning","signature":"fixture-signature"},{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`,
							"responses": `{"input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]},{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"retained reasoning"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"result"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`,
						}[route.history]
						if err := json.Unmarshal([]byte(history), &payload); err != nil {
							t.Fatal(err)
						}
						payload["model"], payload["stream"] = model, true
						if effort != "" {
							switch route.history {
							case "chat":
								payload["reasoning_effort"] = effort
							case "anthropic":
								payload["thinking"] = map[string]string{"type": "adaptive"}
								payload["output_config"] = map[string]string{"effort": effort}
							case "responses":
								payload["reasoning"] = map[string]string{"effort": effort}
							}
						}
						body, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						req, err := http.NewRequest(http.MethodPost, proxy.URL+"/opencode/"+route.up+route.suffix+"?fixture=1", bytes.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Authorization", "Bearer fixture-dummy")
						req.Header.Set("X-Api-Key", "fixture-dummy")
						resp, err := proxy.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						response, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						if err != nil || resp.StatusCode != 200 || !strings.Contains(string(response), "data: [DONE]") {
							t.Fatalf("response = %d %s, %v", resp.StatusCode, response, err)
						}
						if !bytes.Equal(body, gotBody) {
							t.Fatalf("request body changed: %s", gotBody)
						}
						if gotPath != route.base+route.suffix+"?fixture=1" || gotAuth != "Bearer fixture-minimax-key" || gotAPIKey != "" {
							t.Fatalf("incorrect route or credential injection: path=%s", gotPath)
						}
					})
				}
			}
		})
	}
}
