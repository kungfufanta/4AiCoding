package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the adapter's real model precedence and CLI boundary. The fixture
// records the generated config and arguments instead of invoking a paid model.
func TestOpencodeRunMiniMaxModelSelection(t *testing.T) {
	for _, tc := range []struct {
		name, taskModel, savedModel, wantModel, upstream string
	}{
		{"flash task", "opencode/MiniMax-M3.1-Flash", "MiniMax-M3", "MiniMax-M3.1-Flash", "minimax"},
		{"flash saved default", "", "MiniMax-M3.1-Flash", "MiniMax-M3.1-Flash", "minimax"},
		{"saved M3", "", "MiniMax-M3", "MiniMax-M3", "minimax"},
		{"saved legacy", "", "MiniMax-M2.7", "MiniMax-M2.7", "minimax"},
		{"explicit legacy", "MiniMax-M2.7", "MiniMax-M3.1-Flash", "MiniMax-M2.7", "minimax"},
		{"custom", "custom/private-model", "MiniMax-M3.1-Flash", "private-model", "zengo"},
		{"gateway M3", "opencode/minimax-m3", "", "minimax-m3", "zengo"},
		{"general default", "", "", "glm-5", "zengo"},
	} {
		for _, region := range []string{"global_en", "cn_zh"} {
			t.Run(tc.name+"/"+region, func(t *testing.T) {
				dir := t.TempDir()
				fixture := `#!/bin/sh
set -eu
cp "$OPENCODE_CONFIG" "$CAPTURE_DIR/config.json"
printf '%s\n' "$@" > "$CAPTURE_DIR/args"
printf '%s\n' '{"type":"text","part":{"id":"1","text":"ok"}}'
`
				if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(fixture), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("RUNTIMED_ANTHROPIC_PROXY", "http://127.0.0.1:9100")
				t.Setenv("RUNTIMED_OPENCODE_MODEL", tc.savedModel)
				t.Setenv("SANDBOXD_MINIMAX_REGION", region)
				t.Setenv("SANDBOXD_OPENCODE_ZEN_PATH", "zengo")
				text, _, err := (&opencodeAgent{}).run(context.Background(), agentSpec{
					workDir: dir, prompt: "hello", model: tc.taskModel, cont: true,
					env: map[string]string{"CAPTURE_DIR": dir}, rawLog: io.Discard,
				}, func(string, any) {})
				if err != nil || text != "ok" {
					t.Fatalf("run = %q, %v", text, err)
				}
				args, err := os.ReadFile(filepath.Join(dir, "args"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(args), "--model\nproxy/"+tc.wantModel+"\n--continue\n") {
					t.Fatalf("model or continued session lost: %s", args)
				}
				var cfg struct {
					Provider map[string]struct {
						Npm     string `json:"npm"`
						Options struct {
							BaseURL string `json:"baseURL"`
							APIKey  string `json:"apiKey"`
						} `json:"options"`
						Models map[string]any `json:"models"`
					} `json:"provider"`
				}
				b, err := os.ReadFile(filepath.Join(dir, "config.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(b, &cfg); err != nil {
					t.Fatal(err)
				}
				up := tc.upstream
				if up == "minimax" && region == "cn_zh" {
					up = "minimax-cn"
				}
				p := cfg.Provider["proxy"]
				if p.Npm != "@ai-sdk/openai-compatible" || p.Options.BaseURL != "http://127.0.0.1:9100/opencode/"+up || p.Options.APIKey != dummyKey {
					t.Fatalf("unexpected provider: %+v", p)
				}
				if _, ok := p.Models[tc.wantModel]; !ok || len(p.Models) != 1 {
					t.Fatalf("requested model not preserved: %+v", p.Models)
				}
			})
		}
	}
}
