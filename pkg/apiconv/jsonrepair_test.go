package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

var writeDecl = []DeclaredTool{{Name: "write_file", Props: map[string]bool{"Content": true, "Path": true}}, {Name: "search_web", Props: map[string]bool{"query": true}}}

// Real case (14/11): a LaTeX backslash inside the file content made the whole
// tool call invalid JSON, with no closing fence. It must be recovered, and the
// backslash kept in the content.
func TestToolCallWithInvalidEscapeIsRepaired(t *testing.T) {
	reply := "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"Modèle Standard ($\\Lambda CDM$).\\n```go\\nx\\n```\\nFin\", \"Path\": \"/tmp/a.md\"}}"
	text, calls, rep := ParseToolCallsWith(reply, writeDecl)
	if len(calls) != 1 || text != "" || rep.Repaired != 1 || len(rep.Unparsed) != 0 {
		t.Fatalf("calls=%d text=%q rep=%+v", len(calls), text, rep)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments must be valid JSON: %v", err)
	}
	if !strings.Contains(args["Content"], `$\Lambda CDM$`) || !strings.HasSuffix(args["Content"], "Fin") {
		t.Errorf("content altered: %q", args["Content"])
	}
}

func TestRepairJSONStrings(t *testing.T) {
	for in, want := range map[string]string{
		`{"a": "x\Ly"}`:           `{"a": "x\\Ly"}`,
		`{"a": "ok\n\"q\" \\ /"}`: `{"a": "ok\n\"q\" \\ /"}`, // valid: unchanged
		"{\"a\": \"l1\nl2\"}":     `{"a": "l1\nl2"}`,
		`{"a": "\underline"}`:     `{"a": "\\underline"}`,
		`{"a": "é"}`:              `{"a": "é"}`,
	} {
		got, _ := repairJSONStrings(in)
		if got != want {
			t.Errorf("repair(%q) = %q, want %q", in, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("repaired text is not valid JSON: %q", got)
		}
	}
	if s, ins := repairJSONStrings("a\n{\"k\": 1}\n"); s != "a\n{\"k\": 1}\n" || ins != nil {
		t.Error("text outside strings must not be touched")
	}
}

// Real case (14/11): after its tool call the model went on writing leaked
// reasoning and a second call. The calls are kept, the noise is dropped.
func TestTextAfterToolCallIsDropped(t *testing.T) {
	reply := "Je cherche.\n```tool_call\n{\"name\": \"search_web\", \"arguments\": {\"query\": \"exoplanet\"}}\n```Resultat: Let's wait for tool result. Oh! Wait]]>\n" +
		"```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"x\", \"Path\": \"/tmp/b.md\"}}\n```\n] Let's wait.]]>"
	text, calls, rep := ParseToolCallsWith(reply, writeDecl)
	if len(calls) != 2 || text != "Je cherche." || rep.DroppedAfter == 0 {
		t.Fatalf("calls=%d text=%q dropped=%d", len(calls), text, rep.DroppedAfter)
	}
	// Without any tool call nothing is dropped.
	if text, calls, rep := ParseToolCallsWith("Réponse simple.", writeDecl); text != "Réponse simple." || len(calls) != 0 || rep.DroppedAfter != 0 {
		t.Errorf("plain answer altered: %q %d", text, len(calls))
	}
}

// Real case (14/11, request 7f30): one closing brace missing at the end of a
// nested tool call, no closing fence, and fences inside the content.
func TestToolCallWithMissingClosingBrace(t *testing.T) {
	base := "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"# T ($\\Lambda$)\\n```go\\nx\\n```\\nFin\", \"Path\": \"/tmp/a.md\", \"Meta\": {\"Summary\": \"s\", \"UserFacing\": true}}"
	for _, reply := range []string{base, base + "\n```", base + "\n"} {
		text, calls, rep := ParseToolCallsWith(reply, writeDecl)
		if len(calls) != 1 || text != "" || rep.Repaired != 1 {
			t.Fatalf("reply %q: calls=%d text=%q rep=%+v", reply[len(reply)-12:], len(calls), text, rep)
		}
		var args struct {
			Content string
			Meta    struct{ UserFacing bool }
		}
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil || !strings.HasSuffix(args.Content, "Fin") || !args.Meta.UserFacing {
			t.Fatalf("arguments wrong: %v %+v", err, args)
		}
	}
	// Cut in the middle of a string, or after a comma: NOT repaired, stays text.
	for _, cut := range []string{"```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"# début", "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"x\","} {
		if _, calls, _ := ParseToolCallsWith(cut, writeDecl); len(calls) != 0 {
			t.Errorf("a truncated call must not be executed: %q", cut)
		}
	}
}

func TestK34_UnescapedQuotesAndPartialBatchUnparsed(t *testing.T) {
	t.Run("repairs_unescaped_quotes_and_raw_newlines_in_vue_write_file", func(t *testing.T) {
		vueContent := "<template>\n  <div class=\"card\" style=\"color: red;\">\n    <Btn v-if=\"ok\" />\n  </div>\n</template>\n<script setup>\nconst cfg = {\"k\": \"v\"}\nconsole.log(cfg)\n</script>"
		// Deliberately unescaped quotes and raw newlines inside Content:
		rawContent := strings.ReplaceAll(vueContent, `\"`, `"`)
		reply := "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Path\": \"/app/App.vue\", \"Content\": \"" + rawContent + "\"}}\n```"

		text, calls, rep := ParseToolCallsWith(reply, writeDecl)
		if text != "" || len(calls) != 1 || rep.Repaired != 1 || len(rep.Unparsed) != 0 {
			t.Fatalf("expected repaired write_file call: text=%q calls=%d rep=%+v", text, len(calls), rep)
		}
		var args map[string]string
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
			t.Fatalf("invalid arguments JSON: %v (%s)", err, calls[0].Function.Arguments)
		}
		if args["Path"] != "/app/App.vue" || !strings.Contains(args["Content"], `class="card"`) || !strings.Contains(args["Content"], `{"k": "v"}`) {
			t.Errorf("repaired Content altered: %+v", args)
		}
	})

	t.Run("repairs_3rd_call_in_single_multi_call_fence", func(t *testing.T) {
		reply := "```tool_call\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"Path\":\"1.vue\",\"Content\":\"ok\"}}\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"Path\":\"2.vue\",\"Content\":\"ok\"}}\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"Path\":\"3.vue\",\"Content\":\"<div class=\"x\">\nhello</div>\"}}\n" +
			"```"
		text, calls, rep := ParseToolCallsWith(reply, writeDecl)
		if text != "" || len(calls) != 3 || rep.Repaired != 1 || len(rep.Unparsed) != 0 {
			t.Fatalf("expected 3 calls recovered, got text=%q calls=%d rep=%+v", text, len(calls), rep)
		}
	})

	t.Run("partial_single_fence_keeps_valid_prefix_and_reports_trailing_unparsed", func(t *testing.T) {
		reply := "```tool_call\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"Path\":\"1.go\",\"Content\":\"ok\"}}\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"Path\":\"2.go\",\"Content\":\"ok\"}}\n" +
			"{\"name\":\"write_file\", BROKEN SYNTAX WITHOUT COLON}\n" +
			"```"
		_, calls, rep := ParseToolCallsWith(reply, writeDecl)
		if len(calls) != 2 || len(rep.Unparsed) != 1 {
			t.Fatalf("expected 2 valid calls + 1 unparsed report, got calls=%d rep=%+v", len(calls), rep)
		}
	})

	t.Run("partial_multi_fence_batch_keeps_3_valid_and_reports_1_unparsed", func(t *testing.T) {
		reply := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"Path\":\"1.go\",\"Content\":\"ok\"}}\n```\n" +
			"```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"Path\":\"2.go\",\"Content\":\"ok\"}}\n```\n" +
			"```tool_call\n{\"name\":\"write_file\", BROKEN}\n```\n" +
			"```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"Path\":\"4.go\",\"Content\":\"ok\"}}\n```"
		_, calls, rep := ParseToolCallsWith(reply, writeDecl)
		if len(calls) != 3 || len(rep.Unparsed) != 1 {
			t.Fatalf("expected 3 valid calls + 1 unparsed report, got calls=%d rep=%+v", len(calls), rep)
		}
	})

	t.Run("truncated_unescaped_quote_call_never_repaired", func(t *testing.T) {
		cut := "```tool_call\n{\"name\": \"write_file\", \"arguments\": {\"Content\": \"<div class=\"card\">début"
		_, calls, _ := ParseToolCallsWith(cut, writeDecl)
		if len(calls) != 0 {
			t.Fatalf("truncated block must never execute: %+v", calls)
		}
	})
}
