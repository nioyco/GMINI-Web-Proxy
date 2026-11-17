package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

const fence = "```"

func mustArgs(t *testing.T, c ToolCall) map[string]string {
	t.Helper()
	m := map[string]string{}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &m); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	return m
}

func callJSON(name, key, val string) string {
	b, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]string{key: val}})
	return string(b)
}

// AUDIT-13 T-01: argument containing a fence (one and several).
func TestFenceInsideArguments(t *testing.T) {
	content := "# Doc\n" + fence + "go\nfmt.Println(1)\n" + fence + "\nfin"
	reply := fence + "tool_call\n" + callJSON("write_to_file", "CodeContent", content) + "\n" + fence
	decl := []DeclaredTool{{Name: "write_to_file", Props: map[string]bool{"CodeContent": true}}}
	text, calls, rep := ParseToolCallsWith(reply, decl)
	if len(calls) != 1 || len(rep.Unparsed) != 0 || text != "" {
		t.Fatalf("calls=%d unparsed=%v text=%q", len(calls), rep.Unparsed, text)
	}
	if got := mustArgs(t, calls[0])["CodeContent"]; got != content {
		t.Errorf("content altered: %q", got)
	}
}

func TestFenceInsideArgumentsNoDeclared(t *testing.T) {
	content := "a " + fence + " b"
	reply := fence + "json\n" + callJSON("w", "c", content) + "\n" + fence // ordinary tag, arguments present
	_, calls := ParseToolCalls(reply)
	if len(calls) != 1 || mustArgs(t, calls[0])["c"] != content {
		t.Fatalf("calls=%v", calls)
	}
}

func TestFenceBlockFollowedByText(t *testing.T) {
	SetDropTextAfterToolCall(false) // this test covers the "keep the text" mode
	defer SetDropTextAfterToolCall(true)
	content := "x" + fence + "y"
	reply := "Je crée le fichier.\n" + fence + "tool_call\n" + callJSON("w", "c", content) + "\n" + fence + "\nVoilà."
	text, calls := ParseToolCalls(reply)
	if len(calls) != 1 || mustArgs(t, calls[0])["c"] != content {
		t.Fatalf("calls=%v", calls)
	}
	if text != "Je crée le fichier.\n\nVoilà." {
		t.Errorf("text=%q", text)
	}
}

func TestFenceTwoCallsOneWithFence(t *testing.T) {
	content := "k" + fence + "\n" + fence + "m"
	reply := fence + "tool_call\n" + callJSON("a", "p", "1") + "\n" + fence + "\n" +
		fence + "tool_call\n" + callJSON("w", "c", content) + "\n" + fence
	text, calls := ParseToolCalls(reply)
	if len(calls) != 2 || text != "" {
		t.Fatalf("calls=%d text=%q", len(calls), text)
	}
	if calls[0].Function.Name != "a" || mustArgs(t, calls[1])["c"] != content {
		t.Errorf("calls=%+v", calls)
	}
}

func TestFenceTwoValuesInOneBlockWithFence(t *testing.T) {
	content := fence + "x" + fence
	reply := fence + "tool_call\n" + callJSON("w", "c", content) + "\n" + callJSON("b", "d", "2") + "\n" + fence
	_, calls := ParseToolCalls(reply)
	if len(calls) != 2 || mustArgs(t, calls[0])["c"] != content || calls[1].Function.Name != "b" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestFenceArrayWithFence(t *testing.T) {
	content := fence + "x"
	reply := fence + "tool_call\n[" + callJSON("w", "c", content) + "," + callJSON("b", "d", "2") + "]\n" + fence
	_, calls := ParseToolCalls(reply)
	if len(calls) != 2 || mustArgs(t, calls[0])["c"] != content {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestFenceCloseOnSameLineWithFenceInside(t *testing.T) {
	SetDropTextAfterToolCall(false) // this test covers the "keep the text" mode
	defer SetDropTextAfterToolCall(true)
	content := "a" + fence + "b"
	reply := fence + "tool_call\n" + callJSON("w", "c", content) + fence + "\nfin"
	text, calls := ParseToolCalls(reply)
	if len(calls) != 1 || text != "fin" || mustArgs(t, calls[0])["c"] != content {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
}

func TestFenceUnterminatedWithFenceInside(t *testing.T) {
	content := "a" + fence + "b"
	reply := fence + "tool_call\n" + callJSON("w", "c", content) // no closing fence
	text, calls := ParseToolCalls(reply)
	if len(calls) != 1 || text != "" || mustArgs(t, calls[0])["c"] != content {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
}

// Truncated JSON containing a fence: not a call, stays text, no panic.
func TestFenceInvalidJSONStaysText(t *testing.T) {
	reply := fence + "tool_call\n{\"name\": \"w\", \"arguments\": {\"c\": \"a " + fence + " b\"" // truncated
	text, calls, rep := ParseToolCallsWith(reply, []DeclaredTool{{Name: "w", Props: map[string]bool{"c": true}}})
	if len(calls) != 0 || text == "" {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
	_ = rep
}

func TestFenceNonJSONBodyKeepsOldRule(t *testing.T) {
	reply := "Voici:\n" + fence + "go\nfunc main() {}\n" + fence + "\net " + fence + "tool_call\n" + callJSON("a", "p", "1") + "\n" + fence
	text, calls := ParseToolCalls(reply)
	if len(calls) != 1 || !strings.Contains(text, "func main() {}") || strings.Contains(text, "tool_call") {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
}

// Large argument (> 1 MiB) containing fences.
func TestFenceHugeArgument(t *testing.T) {
	SetDropTextAfterToolCall(false) // this test covers the "keep the text" mode
	defer SetDropTextAfterToolCall(true)
	content := strings.Repeat("ligne de texte "+fence+"\n", 80000) // ~1.4 MB
	if len(content) < 1<<20 {
		t.Fatalf("test content too small: %d", len(content))
	}
	reply := "ok\n" + fence + "tool_call\n" + callJSON("w", "c", content) + "\n" + fence + "\nfin"
	text, calls := ParseToolCalls(reply)
	if len(calls) != 1 || text != "ok\n\nfin" {
		t.Fatalf("calls=%d text=%q", len(calls), text[:min(len(text), 80)])
	}
	if mustArgs(t, calls[0])["c"] != content {
		t.Errorf("content altered")
	}
}

func TestFenceOrdinaryBlocksUntouched(t *testing.T) {
	in := "Exemple:\n" + fence + "json\n{\"a\": \"x " + fence + " y\"}\n" + fence + "\nfin"
	text, calls := ParseToolCalls(in)
	if len(calls) != 0 || text != in {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
}

// The AUDIT-13 probe, now asserted.
func TestAudit13FenceInsideArgumentsFixed(t *testing.T) {
	reply := "```tool_call\n{\"name\": \"write_to_file\", \"arguments\": {\"TargetFile\": \"README.md\", \"CodeContent\": \"# Doc\\n```go\\nfmt.Println(1)\\n```\\nfin\"}}\n```"
	decl := []DeclaredTool{{Name: "write_to_file", Props: map[string]bool{"TargetFile": true, "CodeContent": true}}}
	_, calls, rep := ParseToolCallsWith(reply, decl)
	if len(calls) != 1 || len(rep.Unparsed) != 0 || !strings.Contains(calls[0].Function.Arguments, "fin") {
		t.Fatalf("calls=%+v rep=%+v", calls, rep)
	}
}

// K15 Q1: arbitrary opening fence tags (?, &, =, spaces, :, 4+ backticks) before,
// between, or unclosed after tool_call blocks must never shift fence pairing.
func TestK15_Q1_ExoticFenceTags(t *testing.T) {
	decl := []DeclaredTool{
		{Name: "run_command", Props: map[string]bool{"CommandLine": true}},
		{Name: "read_file", Props: map[string]bool{"Path": true}},
	}

	t.Run("code_reference_shape_four_calls", func(t *testing.T) {
		reply := "\n\n```bash?code_reference&code_event_index=1\ngit -C /tmp/repo rev-parse --short HEAD\n```\n\n" +
			"```tool_call\n{\"name\": \"run_command\", \"arguments\": {\"CommandLine\": \"git status\"}}\n```\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"a.go\"}}\n```\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"b.go\"}}\n```\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"c.go\"}}\n```\n"
		text, calls, rep := ParseToolCallsWith(reply, decl)
		if len(calls) != 4 || len(rep.Unparsed) != 0 {
			t.Fatalf("calls=%d unparsed=%+v, want 4/0", len(calls), rep.Unparsed)
		}
		if text != "" || rep.CodeRefRemoved != 1 {
			t.Fatalf("expected code_reference removed cleanly: text=%q codeRefRemoved=%d", text, rep.CodeRefRemoved)
		}
		if calls[0].Function.Name != "run_command" || calls[1].Function.Name != "read_file" || calls[3].Function.Name != "read_file" {
			t.Fatalf("unexpected call names: %+v", calls)
		}
	})

	t.Run("code_reference_alone_kept_intact", func(t *testing.T) {
		alone := "Voici le script :\n```bash?code_reference&code_event_index=1\ngit status\n```\nFin."
		text, calls, rep := ParseToolCallsWith(alone, decl)
		if len(calls) != 0 || rep.CodeRefRemoved != 0 || text != alone {
			t.Fatalf("alone code_reference altered: calls=%d removed=%d text=%q", len(calls), rep.CodeRefRemoved, text)
		}
	})

	t.Run("exotic_tag_between_two_calls", func(t *testing.T) {
		SetDropTextAfterToolCall(false)
		defer SetDropTextAfterToolCall(true)
		reply := "```tool_call\n{\"name\": \"run_command\", \"arguments\": {\"CommandLine\": \"ls\"}}\n```\n" +
			"```custom:lang?a=1&b=2 with spaces\nsome output\n```\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"x.go\"}}\n```\n"
		text, calls, _ := ParseToolCallsWith(reply, decl)
		if len(calls) != 2 {
			t.Fatalf("calls=%d, want 2 (text=%q)", len(calls), text)
		}
		if !strings.Contains(text, "custom:lang?a=1&b=2 with spaces") {
			t.Errorf("exotic block between calls was lost: %q", text)
		}
	})

	t.Run("unclosed_exotic_block_at_end", func(t *testing.T) {
		reply := "```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"a.go\"}}\n```\n" +
			"```log?tail=1&mode=raw:live\nunclosed trailing content"
		_, calls, _ := ParseToolCallsWith(reply, decl)
		if len(calls) != 1 {
			t.Fatalf("calls=%d, want 1", len(calls))
		}
		alone := "```log?tail=1&mode=raw:live\nunclosed trailing content"
		text, calls2, _ := ParseToolCallsWith(alone, decl)
		if len(calls2) != 0 || text != alone {
			t.Fatalf("alone: calls=%d text=%q", len(calls2), text)
		}
	})

	t.Run("four_backticks_with_inner_triple_before_call", func(t *testing.T) {
		reply := "````bash?x=1&y=2\n```inner\ncode\n```\n````\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"a.go\"}}\n```"
		text, calls, _ := ParseToolCallsWith(reply, decl)
		if len(calls) != 1 || !strings.Contains(text, "```inner") {
			t.Fatalf("calls=%d text=%q", len(calls), text)
		}
	})

	t.Run("inline_triple_backticks_before_call", func(t *testing.T) {
		reply := "Utilisez (` ```tool_call ... ``` `) pour appeler.\n" +
			"```tool_call\n{\"name\": \"read_file\", \"arguments\": {\"Path\": \"a.go\"}}\n```"
		text, calls, _ := ParseToolCallsWith(reply, decl)
		if len(calls) != 1 || !strings.Contains(text, "Utilisez") {
			t.Fatalf("calls=%d text=%q", len(calls), text)
		}
	})
}
