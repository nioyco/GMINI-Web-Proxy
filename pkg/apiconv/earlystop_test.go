package apiconv

import (
	"strings"
	"testing"
)

func TestScanToolCallsMarkersAndBatching(t *testing.T) {
	declared := []DeclaredTool{
		{Name: "read_file", Props: map[string]bool{"path": true}},
		{Name: "write_file", Props: map[string]bool{"path": true, "content": true}},
	}

	t.Run("marker after one complete tool call", func(t *testing.T) {
		txt := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n[Tool result for read_file]: pkg\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"b.go\"}}\n```"
		res := ScanToolCalls(txt, declared)
		if !res.HasMarker || res.CompleteCalls != 1 {
			t.Fatalf("got %+v, want HasMarker=true CompleteCalls=1", res)
		}
		_, calls, _ := ParseToolCallsWith(txt[:res.CutAt], declared)
		if len(calls) != 1 || !strings.Contains(calls[0].Function.Arguments, "a.go") {
			t.Fatalf("unexpected calls after cut: %+v", calls)
		}
	})

	t.Run("multiple consecutive tool calls before marker all kept", func(t *testing.T) {
		txt := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"1.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"2.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"3.go\"}}\n```\n" +
			"[User]: continue"
		res := ScanToolCalls(txt, declared)
		if !res.HasMarker || res.CompleteCalls != 3 {
			t.Fatalf("got %+v, want HasMarker=true CompleteCalls=3", res)
		}
		_, calls, _ := ParseToolCallsWith(txt[:res.CutAt], declared)
		if len(calls) != 3 {
			t.Fatalf("got %d calls, want 3", len(calls))
		}
	})

	t.Run("marker inside open write_file payload does not trigger", func(t *testing.T) {
		txt := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"doc.md\",\"content\":\"[User]: example\\n[Tool result for x]: y"
		res := ScanToolCalls(txt, declared)
		if res.HasMarker || res.CompleteCalls != 0 {
			t.Fatalf("false positive inside open write_file: %+v", res)
		}
		closed := txt + "\"}\n```"
		res = ScanToolCalls(closed, declared)
		if res.HasMarker || res.CompleteCalls != 1 {
			t.Fatalf("after close got %+v, want HasMarker=false CompleteCalls=1", res)
		}
	})

	t.Run("all four forbidden markers trigger after complete call", func(t *testing.T) {
		base := "```function_call\n{\"name\":\"read_file\",\"args\":{\"path\":\"a.go\"}}\n```\n"
		for _, m := range []string{"[Tool result for read_file]: ok", "[User]: hi", "[Assistant]: ok", "[System instruction]: x"} {
			res := ScanToolCalls(base+m, declared)
			if !res.HasMarker || res.CompleteCalls != 1 || res.CutAt != len(base) {
				t.Errorf("marker %q: got %+v, want HasMarker=true CutAt=%d", m, res, len(base))
			}
		}
	})

	t.Run("marker before first tool call does not trigger", func(t *testing.T) {
		txt := "[Assistant]: voici :\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```"
		res := ScanToolCalls(txt, declared)
		if res.HasMarker || res.CompleteCalls != 1 {
			t.Fatalf("got %+v, want HasMarker=false CompleteCalls=1", res)
		}
	})

	t.Run("bare JSON followed by marker", func(t *testing.T) {
		txt := "{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n[System instruction]: reset"
		res := ScanToolCalls(txt, declared)
		if !res.HasMarker || res.CompleteCalls != 1 {
			t.Fatalf("got %+v, want HasMarker=true CompleteCalls=1", res)
		}
	})

	t.Run("streaming write_file with inner code fence and quoted marker after read_file", func(t *testing.T) {
		txt := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"doc.md\",\"content\":\"```go\\n// [Tool result for x]: y\\n"
		res := ScanToolCalls(txt, declared)
		if res.HasMarker {
			t.Fatalf("false positive on open write_file with inner fence and marker: %+v", res)
		}
	})

	t.Run("two bare JSON calls where second is open write_file quoting marker", func(t *testing.T) {
		txt := "{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n" +
			"{\"name\":\"write_file\",\"arguments\":{\"path\":\"doc.md\",\"content\":\"[Tool result for x]: y"
		res := ScanToolCalls(txt, declared)
		if res.HasMarker {
			t.Fatalf("false positive on open second bare JSON write_file: %+v", res)
		}
	})

	t.Run("exotic opening fence before tool_calls counts all calls", func(t *testing.T) {
		txt := "\n\n```bash?code_reference&code_event_index=1\ngit rev-parse HEAD\n```\n\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"1.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"2.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"3.go\"}}\n```\n" +
			"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"4.go\"}}\n```\n"
		res := ScanToolCalls(txt, declared)
		if res.CompleteCalls != 4 {
			t.Fatalf("CompleteCalls=%d, want 4 (%+v)", res.CompleteCalls, res)
		}
	})

	t.Run("unclosed exotic fence at end keeps earlier call", func(t *testing.T) {
		txt := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"1.go\"}}\n```\n```foo?a=1&b=2\npartial"
		res := ScanToolCalls(txt, declared)
		if res.CompleteCalls != 1 {
			t.Fatalf("CompleteCalls=%d, want 1", res.CompleteCalls)
		}
	})
}
