package llm

import (
	"encoding/json"
	"testing"
)

func TestParseXMLToolTurn(t *testing.T) {
	tools := []Tool{{Name: "list_files"}, {Name: "run_command"}}
	text := "Đầu tiên, hãy khám phá dự án.\n\n<tool name=\"list_files\">\n<parameter name=\"path\">/home/sandbox/workspace</parameter>\n<parameter name=\"depth\">2</parameter>\n</tool>"
	turn, ok := parseXMLToolTurn(text, tools)
	if !ok || len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Name != "list_files" {
		t.Fatalf("got %+v ok=%v", turn, ok)
	}
	var args map[string]any
	if err := json.Unmarshal(turn.ToolCalls[0].Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["path"] != "/home/sandbox/workspace" || args["depth"] != float64(2) {
		t.Fatalf("args = %v", args)
	}
	if turn.Text != "Đầu tiên, hãy khám phá dự án." {
		t.Fatalf("visible text = %q", turn.Text)
	}
	if _, ok := parseXMLToolTurn(`<tool name="rm_rf"><parameter name="p">/</parameter></tool>`, tools); ok {
		t.Fatal("unknown tool accepted")
	}
	if _, ok := parseXMLToolTurn("plain answer", tools); ok {
		t.Fatal("plain text treated as a tool call")
	}
	// Fallback mode takes the XML form too when there is no ```tool fence.
	ft, err := parseFallbackTurn(text, tools)
	if err != nil || len(ft.ToolCalls) != 1 {
		t.Fatalf("fallback: %+v %v", ft, err)
	}
}
