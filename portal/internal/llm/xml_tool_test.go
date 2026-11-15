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

func TestParseTextToolShapes(t *testing.T) {
	tools := []Tool{{Name: "run_command"}, {Name: "read_file"}, {Name: "write_file"}}
	cases := []struct {
		text, name, key string
		want            any
	}{
		{"First:\n\n<run_command>\ncommand=\"ls -la\"\ndescription=\"List current directory contents\"\n</run_command>", "run_command", "command", "ls -la"},
		{"<read_file><path>src/app.js</path></read_file>", "read_file", "path", "src/app.js"},
		{"<write_file>\n{\"path\": \"index.html\", \"content\": \"<h1>Hi</h1>\"}\n</write_file>", "write_file", "path", "index.html"},
		{"<run_command>\ncommand: npm run build\n</run_command>", "run_command", "command", "npm run build"},
	}
	for _, c := range cases {
		turn, ok := parseXMLToolTurn(c.text, tools)
		if !ok || turn.ToolCalls[0].Name != c.name {
			t.Fatalf("%q: got %+v ok=%v", c.text, turn, ok)
		}
		var args map[string]any
		_ = json.Unmarshal(turn.ToolCalls[0].Arguments, &args)
		if args[c.key] != c.want {
			t.Fatalf("%q: args = %v", c.text, args)
		}
	}
	if !mentionsTextToolCall("<run_command />", tools) {
		t.Fatal("unreadable call not detected")
	}
	if mentionsTextToolCall("plain text about run_command", tools) {
		t.Fatal("plain mention treated as a call")
	}
}

func TestFallbackTakesFirstOfSeveralBlocks(t *testing.T) {
	tools := []Tool{{Name: "run_command"}}
	text := "Plan:\n\n```tool\n{\"name\":\"run_command\",\"arguments\":{\"command\":\"mkdir -p site\"}}\n```\n\nThen:\n\n```tool\n{\"name\":\"run_command\",\"arguments\":{\"command\":\"ls\"}}\n```"
	turn, err := parseFallbackTurn(text, tools)
	if err != nil || len(turn.ToolCalls) != 1 {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
	var args map[string]any
	_ = json.Unmarshal(turn.ToolCalls[0].Arguments, &args)
	if args["command"] != "mkdir -p site" || turn.Text != "Plan:" {
		t.Fatalf("args=%v text=%q", args, turn.Text)
	}
}
