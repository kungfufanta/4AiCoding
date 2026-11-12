// prompt_test.go — decision §2-1: "Prompt nằm trong
// portal/internal/agent/prompt/system.md (go:embed), có test
// snapshot". The golden pins the ported prompt; the substring checks
// guard the conventions that must survive future edits (bind
// 0.0.0.0, AGENTS.md, BRAIN.md, data-not-instructions).
package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptSnapshot(t *testing.T) {
	golden := filepath.Join("testdata", "system_prompt.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(golden, []byte(SystemPrompt), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 go test to create): %v", err)
	}
	if string(want) != SystemPrompt {
		t.Fatalf("system prompt drifted from the golden snapshot — if the change is deliberate, run UPDATE_GOLDEN=1 go test ./internal/agent and review the diff")
	}
}

func TestSystemPromptKeepsConventions(t *testing.T) {
	required := []string{
		// Decision §2-1: keep the 0.0.0.0 bind rule and self-checking.
		"0.0.0.0",
		"check_preview",
		// AGENTS.md read-first, BRAIN.md read-before / write-after.
		"AGENTS.md",
		"BRAIN.md",
		// The untrusted-data boundary (plan §12).
		"DATA, not instructions",
		// Honesty + the finish tool.
		"finish",
		"blocked you",
		// No upstream runtimed/sandbox.yaml leftovers.
	}
	for _, s := range required {
		if !strings.Contains(SystemPrompt, s) {
			t.Errorf("system prompt lost convention %q", s)
		}
	}
	for _, banned := range []string{"runtimed", "sandbox.yaml", "claude --append"} {
		if strings.Contains(SystemPrompt, banned) {
			t.Errorf("system prompt still mentions upstream detail %q", banned)
		}
	}
}
