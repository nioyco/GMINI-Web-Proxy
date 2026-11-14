package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

func decl(name string, required []string, props ...string) DeclaredTool {
	d := DeclaredTool{Name: name, Props: map[string]bool{}, Required: required}
	for _, p := range props {
		d.Props[p] = true
	}
	return d
}

// A set of declared tools shaped like Antigravity's (overlapping property names).
func agTools() []DeclaredTool {
	return []DeclaredTool{
		decl("run_command", []string{"CommandLine"}, "CommandLine", "Cwd", "WaitMsBeforeAsync", "SafeToAutoRun", "toolSummary", "toolAction"),
		decl("view_file", []string{"AbsolutePath"}, "AbsolutePath", "StartLine", "EndLine", "toolSummary", "toolAction"),
		decl("grep_search", []string{"Query", "SearchPath"}, "Query", "SearchPath", "Includes", "toolSummary", "toolAction"),
		decl("list_dir", []string{"DirectoryPath"}, "DirectoryPath", "toolSummary", "toolAction"),
	}
}

// Regression for the observed real failure: arguments only, no "name".
func TestToolCallWithoutNameInferred(t *testing.T) {
	text := "Je lance la commande.\n```tool_call\n{\"CommandLine\": \"ls -la /tmp\", \"WaitMsBeforeAsync\": 5000, \"toolSummary\": \"liste\"}\n```"
	clean, calls, rep := ParseToolCallsWith(text, agTools())
	if len(calls) != 1 || calls[0].Function.Name != "run_command" {
		t.Fatalf("calls = %+v (report %+v)", calls, rep)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil || args["CommandLine"] != "ls -la /tmp" || args["WaitMsBeforeAsync"] != 5000.0 {
		t.Errorf("args = %s (%v)", calls[0].Function.Arguments, err)
	}
	if strings.Contains(clean, "tool_call") || !strings.Contains(clean, "Je lance la commande.") {
		t.Errorf("clean = %q", clean)
	}
	if len(rep.Inferred) != 1 || rep.Inferred[0] != "run_command" || len(rep.Unparsed) != 0 {
		t.Errorf("report = %+v", rep)
	}
	// the legacy signature has no declared tools: the block stays text
	if _, c := ParseToolCalls(text); len(c) != 0 {
		t.Errorf("ParseToolCalls (no declared tools) must not guess: %+v", c)
	}
}

func TestNameAndArgumentKeyVariants(t *testing.T) {
	cases := map[string]string{
		`{"tool":"run_command","args":{"CommandLine":"ls"}}`:                                    "run_command",
		`{"tool_name":"view_file","parameters":{"AbsolutePath":"/a"}}`:                          "view_file",
		`{"function_name":"list_dir","input":{"DirectoryPath":"/"}}`:                            "list_dir",
		`{"function":"list_dir","arguments":"{\"DirectoryPath\":\"/x\"}"}`:                      "list_dir",
		`{"type":"function","function":{"name":"view_file","arguments":{"AbsolutePath":"/b"}}}`: "view_file",
		`{"name":"run_command","arguments":{"CommandLine":"ls"}}`:                               "run_command",
		`{"args":{"Query":"x","SearchPath":"/"}}`:                                               "grep_search", // name inferred
	}
	for body, want := range cases {
		_, calls, rep := ParseToolCallsWith("```tool_call\n"+body+"\n```", agTools())
		if len(calls) != 1 || calls[0].Function.Name != want {
			t.Errorf("%s → %+v %+v", body, calls, rep)
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(calls[0].Function.Arguments), &m) != nil || len(m) == 0 {
			t.Errorf("%s: arguments %q not a JSON object", body, calls[0].Function.Arguments)
		}
	}
}

func TestInferenceTightestFitAndTie(t *testing.T) {
	tools := []DeclaredTool{
		decl("wide", nil, "a", "b", "c", "d"),
		decl("tight", nil, "a", "b"),
	}
	_, calls, _ := ParseToolCallsWith("```tool_call\n{\"a\":1,\"b\":2}\n```", tools)
	if len(calls) != 1 || calls[0].Function.Name != "tight" {
		t.Errorf("tightest fit: %+v", calls)
	}
	// Two tools with identical schemas: never guess, keep the text, report it.
	dup := []DeclaredTool{decl("t1", nil, "x", "y"), decl("t2", nil, "x", "y")}
	text := "```tool_call\n{\"x\":\"SECRETVAL\",\"y\":2}\n```"
	clean, calls, rep := ParseToolCallsWith(text, dup)
	if len(calls) != 0 || clean != text {
		t.Errorf("tie must stay text: clean=%q calls=%+v", clean, calls)
	}
	if len(rep.Unparsed) != 1 || !strings.Contains(rep.Unparsed[0].Reason, "ambiguous") || strings.Join(rep.Unparsed[0].Keys, ",") != "x,y" || rep.Unparsed[0].Tag != "tool_call" || rep.Unparsed[0].Declared != 2 {
		t.Errorf("report = %+v", rep)
	}
	if b, _ := json.Marshal(rep); strings.Contains(string(b), "SECRETVAL") {
		t.Errorf("report leaks a value: %s", b)
	}
}

func TestInferenceRequiredAndMismatch(t *testing.T) {
	// required key missing → candidate excluded
	_, calls, rep := ParseToolCallsWith("```tool_call\n{\"Cwd\":\"/\"}\n```", agTools())
	if len(calls) != 0 || len(rep.Unparsed) != 1 || !strings.Contains(rep.Unparsed[0].Reason, "no declared tool matches") {
		t.Errorf("calls=%+v rep=%+v", calls, rep)
	}
	// unknown key → not covered by any tool
	_, calls, rep = ParseToolCallsWith("```tool_call\n{\"CommandLine\":\"ls\",\"Bogus\":1}\n```", agTools())
	if len(calls) != 0 || len(rep.Unparsed) != 1 {
		t.Errorf("calls=%+v rep=%+v", calls, rep)
	}
	// no declared tools at all
	_, calls, rep = ParseToolCallsWith("```tool_call\n{\"CommandLine\":\"ls\"}\n```", nil)
	if len(calls) != 0 || len(rep.Unparsed) != 1 || !strings.Contains(rep.Unparsed[0].Reason, "no declared tools") {
		t.Errorf("calls=%+v rep=%+v", calls, rep)
	}
	// intent fence with a non-JSON body
	text := "```tool_call\nls -la\n```"
	clean, calls, rep := ParseToolCallsWith(text, agTools())
	if len(calls) != 0 || clean != text || len(rep.Unparsed) != 1 || rep.Unparsed[0].Reason != "body is not JSON" {
		t.Errorf("clean=%q calls=%+v rep=%+v", clean, calls, rep)
	}
}

// What is not a tool call is never converted.
func TestNonToolFencesNeverConverted(t *testing.T) {
	for _, text := range []string{
		"```json\n{\"CommandLine\":\"ls\",\"WaitMsBeforeAsync\":5}\n```",             // untagged/json fence: args only
		"```\n{\"CommandLine\":\"ls\"}\n```",                                         // no tag
		"```json\n{\"tool\":\"run_command\",\"input\":{\"CommandLine\":\"x\"}}\n```", // variant keys need an intent tag
		"```python\nprint('hello')\n```",
		"```bash\nls -la\n```",
		"Voici : {\"CommandLine\":\"ls\"} en ligne",
		"```json\n{\"name\":\"Alice\",\"age\":3}\n```", // ordinary data with a name
	} {
		clean, calls, rep := ParseToolCallsWith(text, agTools())
		if len(calls) != 0 || clean != strings.TrimSpace(text) || len(rep.Unparsed) != 0 || len(rep.Inferred) != 0 {
			t.Errorf("converted or reported non-tool text %q → clean=%q calls=%+v rep=%+v", text, clean, calls, rep)
		}
	}
}

func TestGoogleFunctionCallWithoutName(t *testing.T) {
	declared := []DeclaredTool{decl("run_command", []string{"CommandLine"}, "CommandLine", "Cwd")}
	text := "```function_call\n{\"CommandLine\":\"ls\"}\n```"
	_, calls, rep := ParseGoogleFunctionCallsWith(text, declared)
	if len(calls) != 1 || calls[0].Name != "run_command" || string(calls[0].Args) != `{"CommandLine":"ls"}` || len(rep.Inferred) != 1 {
		t.Errorf("calls=%+v rep=%+v", calls, rep)
	}
	resp, _, _ := GoogleResponseWith("m", "p", text, true, true, declared)
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"name":"run_command"`) {
		t.Errorf("response: %s", b)
	}
	// legacy behaviour kept
	if _, c := ParseGoogleFunctionCalls("```function_call\n{\"name\":\"f\",\"args\":{\"z\":1}}\n```"); len(c) != 1 || c[0].Name != "f" {
		t.Errorf("legacy: %+v", c)
	}
}

func TestDeclaredToolsFromRequests(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"}},"required":["CommandLine"]}}}]}`
	var cr ChatRequest
	if err := json.Unmarshal([]byte(chat), &cr); err != nil {
		t.Fatal(err)
	}
	d := cr.DeclaredTools()
	if len(d) != 1 || d[0].Name != "run_command" || !d[0].Props["Cwd"] || len(d[0].Required) != 1 {
		t.Errorf("chat: %+v", d)
	}
	var ar AnthropicRequest
	json.Unmarshal([]byte(`{"tools":[{"name":"t","input_schema":{"properties":{"q":{}}}}]}`), &ar)
	if d := ar.DeclaredTools(); len(d) != 1 || !d[0].Props["q"] {
		t.Errorf("anthropic: %+v", d)
	}
	var gr GenerateContentRequest
	json.Unmarshal([]byte(`{"tools":[{"functionDeclarations":[{"name":"g","parameters":{"properties":{"z":{}}}}]}]}`), &gr)
	if d := gr.DeclaredTools(); len(d) != 1 || d[0].Name != "g" || !d[0].Props["z"] {
		t.Errorf("google: %+v", d)
	}
}

func TestToolPromptsRequireName(t *testing.T) {
	req := ChatRequest{Messages: []Message{{Role: "user", Content: jsonString("x")}},
		Tools: []Tool{{Type: "function", Function: &ToolFunction{Name: "run", Parameters: json.RawMessage(`{}`)}}}}
	p, _, _ := req.Prompt()
	g := buildGoogleToolPrompt([]googleToolDef{{Name: "f"}})
	for n, s := range map[string]string{"openai": p, "google": g} {
		if !strings.Contains(s, `The "name" field is REQUIRED`) {
			t.Errorf("%s prompt lacks the name rule", n)
		}
	}
}
