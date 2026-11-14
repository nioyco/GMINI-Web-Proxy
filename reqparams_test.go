package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const paramsBody = `{"model":"gemini-3.5-flash","stream":false,"temperature":0.2,"top_p":0.9,"max_completion_tokens":4096,
"reasoning_effort":"high","n":1,"user":"SECRET_USER","metadata":{"k":"SECRET_META"},
"tool_choice":{"type":"function","function":{"name":"SECRET_TOOL"}},
"thinking":{"type":"enabled","budget_tokens":2048,"hidden":"SECRET_NESTED"},
"messages":[{"role":"user","content":"SECRET_MESSAGE"},{"role":"assistant","content":"SECRET_REPLY"}],
"tools":[{"type":"function","function":{"name":"SECRET_FN","parameters":{}}}]}`

func TestDescribeRequestContentFree(t *testing.T) {
	rp := describeRequest("chat", []byte(paramsBody), 2)
	b, _ := json.Marshal(rp)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("content leaked: %s", b)
	}
	want := map[string]string{
		"model": "gemini-3.5-flash", "stream": "false", "temperature": "0.2", "top_p": "0.9",
		"max_completion_tokens": "4096", "reasoning_effort": "high", "n": "1",
		"tool_choice": "(object)", "thinking.type": "enabled", "thinking.budget_tokens": "2048",
	}
	for k, v := range want {
		if rp.Scalars[k] != v {
			t.Errorf("scalar %s = %q, want %q (all: %v)", k, rp.Scalars[k], v, rp.Scalars)
		}
	}
	if _, bad := rp.Scalars["thinking.hidden"]; bad || rp.Scalars["user"] != "" {
		t.Errorf("non-allow-listed scalar recorded: %v", rp.Scalars)
	}
	if rp.Messages != 2 || rp.Tools != 1 || rp.Images != 2 {
		t.Errorf("counts: %+v", rp)
	}
	if strings.Join(rp.Ignored, ",") != "max_completion_tokens,metadata,n,temperature,thinking,top_p,user" {
		t.Errorf("ignored = %v", rp.Ignored)
	}
	for _, f := range []string{"messages", "tools", "tool_choice", "model", "stream"} {
		for _, ig := range rp.Ignored {
			if ig == f {
				t.Errorf("%s is used by the proxy but listed as ignored", f)
			}
		}
	}
}

func TestDescribeRequestGoogleNested(t *testing.T) {
	body := `{"contents":[{"role":"user","parts":[{"text":"SECRET"}]}],"generationConfig":{"temperature":0.7,"maxOutputTokens":8192,"thinkingConfig":{"thinkingBudget":1024,"includeThoughts":true},"stopSequences":["SECRET"]},"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"safetySettings":[{"x":"SECRET"}]}`
	rp := describeRequest("google", []byte(body), 0)
	b, _ := json.Marshal(rp)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("leak: %s", b)
	}
	for k, v := range map[string]string{
		"generationConfig.temperature": "0.7", "generationConfig.maxOutputTokens": "8192",
		"generationConfig.thinkingConfig.thinkingBudget": "1024", "generationConfig.thinkingConfig.includeThoughts": "true",
		"toolConfig.functionCallingConfig.mode": "ANY",
	} {
		if rp.Scalars[k] != v {
			t.Errorf("%s = %q want %q (%v)", k, rp.Scalars[k], v, rp.Scalars)
		}
	}
	if strings.Join(rp.Ignored, ",") != "generationConfig,safetySettings" || rp.Messages != 1 {
		t.Errorf("rp = %+v", rp)
	}
}

func TestDescribeRequestBadInputs(t *testing.T) {
	if rp := describeRequest("chat", []byte("not json"), 0); len(rp.Fields) != 0 {
		t.Errorf("%+v", rp)
	}
	rp := describeRequest("chat", []byte(`{"model":"bad model with spaces\nSECRET","bad name!":1}`), 0)
	if _, ok := rp.Scalars["model"]; ok {
		t.Errorf("unsafe string recorded: %v", rp.Scalars)
	}
	if strings.Join(rp.Fields, ",") != "(unsafe-name),model" {
		t.Errorf("fields = %v", rp.Fields)
	}
}

func TestRequestParamsLoggedAndStatusExposed(t *testing.T) {
	s, logs := echoServer(t, "ok")
	s.cfg.LogRequests = true
	rec := post(s, "/v1/chat/completions", paramsBody)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	l := logs.String()
	for _, want := range []string{"msg=request_params", "api=chat", "temperature=0.2", "reasoning_effort=high", "messages=2", "tools=1", "ignored="} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "SECRET") {
		t.Errorf("log leaks content:\n%s", l)
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		Up  map[string]any `json:"upstream_params"`
		Ign struct {
			Counts map[string]int64 `json:"counts"`
		} `json:"ignored_request_fields"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ign.Counts["temperature"] != 1 || st.Ign.Counts["reasoning_effort"] != 0 || st.Ign.Counts["messages"] != 0 {
		t.Errorf("ignored counts = %v", st.Ign.Counts)
	}
	if st.Up["inner_17"] != "[[0]]" {
		t.Errorf("upstream_params = %v", st.Up)
	}
	if strings.Contains(rr.Body.String(), "SECRET") {
		t.Errorf("/_status leaks: %s", rr.Body)
	}
}

func TestRequestParamsNotLoggedByDefault(t *testing.T) {
	s, logs := echoServer(t, "ok")
	post(s, "/v1/chat/completions", paramsBody)
	if strings.Contains(logs.String(), "request_params") || strings.Contains(logs.String(), "upstream_params") {
		t.Errorf("params logged without GEMINI_LOG_REQUESTS:\n%s", logs)
	}
	if s.ignored.snapshot()["temperature"] != 1 {
		t.Error("ignored counters must work without GEMINI_LOG_REQUESTS")
	}
}
