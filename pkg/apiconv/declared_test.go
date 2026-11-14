package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

var declView = []DeclaredTool{{Name: "view_file", Props: map[string]bool{"AbsolutePath": true}}}

func undeclaredReasons(rep ParseReport) []string {
	var out []string
	for _, u := range rep.Unparsed {
		out = append(out, u.Reason)
	}
	return out
}

// AUDIT-13 T-02: an undeclared name is refused on every OpenAI-side path.
func TestUndeclaredNameRefused(t *testing.T) {
	call := `{"name": "delete_everything", "arguments": {"path": "/"}}`
	cases := map[string]string{
		"intent fence":   "```tool_call\n" + call + "\n```",
		"ordinary fence": "Exemple :\n```json\n" + call + "\n```\nC'est tout.",
		"no tag fence":   "```\n" + call + "\n```",
		"bare json":      call,
		"function_call":  "```function_call\n" + call + "\n```",
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			text, calls, rep := ParseToolCallsWith(reply, declView)
			if len(calls) != 0 {
				t.Fatalf("undeclared call accepted: %+v", calls)
			}
			if !strings.Contains(text, "delete_everything") {
				t.Errorf("block must stay in the text, got %q", text)
			}
			r := undeclaredReasons(rep)
			if len(r) != 1 || r[0] != "tool name not declared: delete_everything" {
				t.Errorf("reasons=%v", r)
			}
		})
	}
}

func TestDeclaredNameAccepted(t *testing.T) {
	call := `{"name": "view_file", "arguments": {"AbsolutePath": "/a"}}`
	for name, reply := range map[string]string{
		"intent fence":   "```tool_call\n" + call + "\n```",
		"ordinary fence": "```json\n" + call + "\n```",
		"bare json":      call,
	} {
		t.Run(name, func(t *testing.T) {
			_, calls, rep := ParseToolCallsWith(reply, declView)
			if len(calls) != 1 || calls[0].Function.Name != "view_file" || len(rep.Unparsed) != 0 {
				t.Fatalf("calls=%+v rep=%+v", calls, rep)
			}
		})
	}
}

// No declared tools (legacy ParseToolCalls): unchanged behaviour.
func TestUndeclaredCheckOffWithoutDeclared(t *testing.T) {
	_, calls := ParseToolCalls("```tool_call\n{\"name\": \"anything\", \"arguments\": {}}\n```")
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestUndeclaredNameSanitizedInReport(t *testing.T) {
	bad := "x\nsecret value " + strings.Repeat("a", 200)
	b, _ := json.Marshal(map[string]any{"name": bad, "arguments": map[string]any{}})
	_, _, rep := ParseToolCallsWith("```tool_call\n"+string(b)+"\n```", declView)
	r := undeclaredReasons(rep)
	if len(r) != 1 || !strings.HasPrefix(r[0], "tool name not declared: x?secret?value?") || len(r[0]) > 100 || strings.Contains(r[0], "\n") {
		t.Fatalf("reason not sanitized: %q", r)
	}
}

// A block mixing a declared and an undeclared call is left whole as text.
func TestMixedBlockStaysText(t *testing.T) {
	reply := "```tool_call\n{\"name\":\"view_file\",\"arguments\":{}}\n{\"name\":\"rm\",\"arguments\":{}}\n```"
	text, calls, rep := ParseToolCallsWith(reply, declView)
	if len(calls) != 0 || text != reply || len(rep.Unparsed) != 1 {
		t.Fatalf("calls=%v text=%q rep=%+v", calls, text, rep)
	}
}

func TestInferredNamesAreDeclared(t *testing.T) {
	_, calls, rep := ParseToolCallsWith("```tool_call\n{\"AbsolutePath\": \"/a\"}\n```", declView)
	if len(calls) != 1 || calls[0].Function.Name != "view_file" || len(rep.Inferred) != 1 {
		t.Fatalf("calls=%v rep=%+v", calls, rep)
	}
}

// AUDIT-13 probe T-02, asserted.
func TestAudit13UndeclaredNameFixed(t *testing.T) {
	reply := "Voici un exemple de configuration :\n```json\n{\"name\": \"delete_everything\", \"arguments\": {\"path\": \"/\"}}\n```\nC'est tout."
	text, calls, _ := ParseToolCallsWith(reply, declView)
	if len(calls) != 0 || text != reply {
		t.Fatalf("calls=%v text=%q", calls, text)
	}
}

// Google route: same validation on block, bare block and raw JSON paths.
func TestGoogleUndeclaredRefused(t *testing.T) {
	decl := []DeclaredTool{{Name: "search", Props: map[string]bool{"q": true}}}
	bad := `{"name": "nuke", "args": {"q": "x"}}`
	for name, reply := range map[string]string{
		"block":    "```function_call\n" + bad + "\n```",
		"bare":     "function_call\n" + bad,
		"raw json": bad,
	} {
		t.Run(name, func(t *testing.T) {
			text, calls, rep := ParseGoogleFunctionCallsWith(reply, decl)
			if len(calls) != 0 || !strings.Contains(text, "nuke") || len(rep.Unparsed) != 1 {
				t.Fatalf("calls=%v text=%q rep=%+v", calls, text, rep)
			}
		})
	}
	good := `{"name": "search", "args": {"q": "x"}}`
	for name, reply := range map[string]string{
		"block":    "```function_call\n" + good + "\n```",
		"bare":     "function_call\n" + good,
		"raw json": good,
	} {
		t.Run("ok "+name, func(t *testing.T) {
			text, calls, rep := ParseGoogleFunctionCallsWith(reply, decl)
			if len(calls) != 1 || calls[0].Name != "search" || text != "" || len(rep.Unparsed) != 0 {
				t.Fatalf("calls=%v text=%q rep=%+v", calls, text, rep)
			}
		})
	}
}

func TestGoogleResponseUndeclaredStaysText(t *testing.T) {
	decl := []DeclaredTool{{Name: "search", Props: map[string]bool{"q": true}}}
	resp, _, rep := GoogleResponseWith("m", "p", "```function_call\n{\"name\":\"nuke\",\"args\":{}}\n```", true, true, decl)
	b, _ := json.Marshal(resp)
	if strings.Contains(string(b), `"functionCall"`) || len(rep.Unparsed) != 1 {
		t.Fatalf("function call emitted: %s", b)
	}
}

// Anthropic and Responses requests expose their declared tools, which the server
// passes to ParseToolCallsWith (finishText) like the chat route.
func TestRoutesDeclareTools(t *testing.T) {
	var an AnthropicRequest
	if err := json.Unmarshal([]byte(`{"messages":[],"tools":[{"name":"view_file","input_schema":{"type":"object","properties":{"AbsolutePath":{}}}}]}`), &an); err != nil {
		t.Fatal(err)
	}
	var rs ResponsesRequest
	if err := json.Unmarshal([]byte(`{"tools":[{"type":"function","name":"view_file","parameters":{"type":"object","properties":{"AbsolutePath":{}}}}]}`), &rs); err != nil {
		t.Fatal(err)
	}
	reply := "```json\n{\"name\":\"rm\",\"arguments\":{}}\n```"
	for name, d := range map[string][]DeclaredTool{"anthropic": an.DeclaredTools(), "responses": rs.DeclaredTools()} {
		if len(d) != 1 || d[0].Name != "view_file" {
			t.Fatalf("%s declared=%v", name, d)
		}
		if _, calls, _ := ParseToolCallsWith(reply, d); len(calls) != 0 {
			t.Errorf("%s accepted undeclared call", name)
		}
	}
}
