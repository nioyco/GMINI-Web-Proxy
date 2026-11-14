package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fifteenTools builds an Antigravity-like request body with 15 declared tools, the
// first being run_command.
func fifteenToolsBody(stream bool) string {
	var tools []string
	tools = append(tools, `{"type":"function","function":{"name":"run_command","description":"d","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"},"WaitMsBeforeAsync":{"type":"integer"},"toolSummary":{"type":"string"}},"required":["CommandLine"]}}}`)
	for i := 1; i < 15; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","function":{"name":"tool_%d","description":"d","parameters":{"type":"object","properties":{"Arg%d":{"type":"string"},"toolSummary":{"type":"string"}}}}}`, i, i))
	}
	return fmt.Sprintf(`{"model":"gemini-3.5-flash","stream":%v,"messages":[{"role":"user","content":"liste"}],"tools":[%s]}`, stream, strings.Join(tools, ","))
}

const argsOnlyReply = "```tool_call\n{\"CommandLine\": \"ls -la /tmp\", \"WaitMsBeforeAsync\": 5000, \"toolSummary\": \"liste\"}\n```"

func TestChatRecoversNamelessToolCall(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s, logs := echoServer(t, argsOnlyReply)
		rec := post(s, "/v1/chat/completions", fifteenToolsBody(stream))
		b := rec.Body.String()
		if rec.Code != 200 || !strings.Contains(b, `"tool_calls"`) || !strings.Contains(b, "run_command") || strings.Contains(b, "```") {
			t.Errorf("stream=%v: %d %s", stream, rec.Code, b)
		}
		if s.stats.toolNameInferred.Load() != 1 || s.stats.toolCallUnparsed.Load() != 0 || !strings.Contains(logs.String(), "tool_name_inferred") {
			t.Errorf("stream=%v: counters inferred=%d unparsed=%d\n%s", stream, s.stats.toolNameInferred.Load(), s.stats.toolCallUnparsed.Load(), logs)
		}
	}
}

func TestChatUnparsedFenceLoggedNoValues(t *testing.T) {
	// "toolSummary" alone fits 15 tools equally: ambiguous, stays text.
	reply := "```tool_call\n{\"toolSummary\": \"SECRET_SUMMARY\"}\n```"
	s, logs := echoServer(t, reply)
	rec := post(s, "/v1/chat/completions", fifteenToolsBody(false))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"tool_calls"`) || !strings.Contains(rec.Body.String(), "tool_call") {
		t.Fatalf("ambiguous fence must stay text: %s", rec.Body)
	}
	l := logs.String()
	for _, want := range []string{"msg=tool_call_unparsed", "tag=tool_call", "keys=toolSummary", "declared_tools=15", "reason=\"ambiguous"} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "SECRET_SUMMARY") {
		t.Errorf("value leaked:\n%s", l)
	}
	if s.stats.toolCallUnparsed.Load() != 1 {
		t.Errorf("counter = %d", s.stats.toolCallUnparsed.Load())
	}
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		C map[string]int64 `json:"counters"`
	}
	json.Unmarshal(rr.Body.Bytes(), &st)
	if st.C["tool_call_unparsed"] != 1 || st.C["tool_name_inferred"] != 0 {
		t.Errorf("/_status counters = %v", st.C)
	}
}

func TestOrdinaryCodeFenceNotConvertedByServer(t *testing.T) {
	reply := "Voici :\n```json\n{\"CommandLine\":\"ls\"}\n```"
	s, logs := echoServer(t, reply)
	rec := post(s, "/v1/chat/completions", fifteenToolsBody(false))
	if strings.Contains(rec.Body.String(), `"tool_calls"`) || strings.Contains(logs.String(), "tool_call_unparsed") {
		t.Errorf("ordinary json fence touched: %s\n%s", rec.Body, logs)
	}
}

func TestGoogleRouteRecoversNamelessCall(t *testing.T) {
	reply := "```function_call\n{\"CommandLine\":\"ls\"}\n```"
	s, _ := echoServer(t, reply)
	body := `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"}},"required":["CommandLine"]}}]}]}`
	rec := post(s, "/v1beta/models/gemini-3.5-flash:generateContent", body)
	if !strings.Contains(rec.Body.String(), `"name":"run_command"`) || s.stats.toolNameInferred.Load() != 1 {
		t.Errorf("%d %s inferred=%d", rec.Code, rec.Body, s.stats.toolNameInferred.Load())
	}
}
