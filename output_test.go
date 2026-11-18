package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

type rtFn func(*http.Request) (*http.Response, error)

func (f rtFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func wrbFake(text string) string {
	inner := []any{nil, nil, nil, nil, []any{[]any{"rc_1", []any{text}}}, nil, nil, nil, nil, nil, nil, nil, "padding-padding-padding-padding"}
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return ")]}'\n\n" + string(line) + strings.Repeat(" ", 200) + "\n"
}

// echoServer builds a server whose simulated Gemini upstream always replies text.
func echoServer(t *testing.T, text string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := testConfig()
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels(testModels())
	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(wrbFake(text)))}, nil
	}))
	var logs bytes.Buffer
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logs, nil)))
	return s, &logs
}

const echoReply = "[Assistant]:\n\n[Tool result for call_fca76f45]: चयन: {\"output\":\"total 16\"}\n\n[Tool result for call_fca76f45]: x\n\nVoici la vraie réponse."

func post(s *Server, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081"+path, strings.NewReader(body))
	s.handler().ServeHTTP(rec, req)
	return rec
}

func TestChatNonStreamStripsEchoAndLogs(t *testing.T) {
	s, logs := echoServer(t, echoReply)
	rec := post(s, "/v1/chat/completions", `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"salut"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Tool result") || strings.Contains(body, "[Assistant]") || !strings.Contains(body, "Voici la vraie réponse.") {
		t.Errorf("body: %s", body)
	}
	l := logs.String()
	for _, want := range []string{"echo_stripped", "fabricated_tool_results=2", "upstream_attempts=1", "upstream_req_bytes=", "first_byte_ms=", "upstream_resp_bytes="} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "Voici la vraie") || strings.Contains(l, "fca76f45") {
		t.Errorf("log leaks content:\n%s", l)
	}
	if s.stats.echoFabricated.Load() != 2 || s.stats.upstreamAttempts.Load() != 1 {
		t.Errorf("counters: fab=%d attempts=%d", s.stats.echoFabricated.Load(), s.stats.upstreamAttempts.Load())
	}
}

func TestChatStreamStripsEcho(t *testing.T) {
	s, _ := echoServer(t, echoReply)
	rec := post(s, "/v1/chat/completions", `{"model":"gemini-3.5-flash","stream":true,"messages":[{"role":"user","content":"salut"}]}`)
	b := rec.Body.String()
	if strings.Contains(b, "Tool result") || strings.Contains(b, "Assistant]") || !strings.Contains(b, "Voici la vraie") {
		t.Errorf("stream: %s", b)
	}
}

func TestChatToolsKeepsRealCallStripsEcho(t *testing.T) {
	reply := "[Tool result for call_fca76f45]: {\"fake\":1}\n\n```tool_call\n{\"name\":\"run\",\"arguments\":{\"cmd\":\"ls\"}}\n```"
	s, _ := echoServer(t, reply)
	rec := post(s, "/v1/chat/completions", `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"salut"}],"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object"}}}]}`)
	b := rec.Body.String()
	if !strings.Contains(b, `"tool_calls"`) || !strings.Contains(b, "run") || strings.Contains(b, "fake") {
		t.Errorf("body: %s", b)
	}
}

func TestUpstreamRefusalMarker(t *testing.T) {
	s, logs := echoServer(t, "I'm having a hard time fulfilling your request. Can I help you with something else instead?")
	rec := post(s, "/v1/chat/completions", `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"x"}]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hard time fulfilling") {
		t.Fatalf("refusal must pass through unchanged: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "upstream_refusal") || !strings.Contains(logs.String(), "upstream_refusal=true") || s.stats.upstreamRefusal.Load() != 1 {
		t.Errorf("marker missing:\n%s", logs)
	}
	// streaming variant
	s2, logs2 := echoServer(t, "I'm having a hard time fulfilling your request. Can I help you with something else instead?")
	post(s2, "/v1/chat/completions", `{"model":"gemini-3.5-flash","stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if s2.stats.upstreamRefusal.Load() != 1 || !strings.Contains(logs2.String(), "msg=upstream_refusal") {
		t.Errorf("stream marker missing:\n%s", logs2)
	}
}

func TestStatusExposesNewCounters(t *testing.T) {
	s, _ := echoServer(t, "ok")
	post(s, "/v1/chat/completions", `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"x"}]}`)
	if s.stats.upstreamRequests.Load() != 1 || s.stats.upstreamReqBytes.Load() <= 0 || s.stats.upstreamRespBytes.Load() <= 0 {
		t.Errorf("aggregates not updated")
	}
}

func TestEchoFilterKillSwitch(t *testing.T) {
	googleBody := `{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`
	paths := []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"x"}]}`},
		{"chat-stream", "/v1/chat/completions", `{"model":"gemini-3.5-flash","stream":true,"messages":[{"role":"user","content":"x"}]}`},
		{"google", "/v1beta/models/gemini-3.5-flash:generateContent", googleBody},
		{"google-stream", "/v1beta/models/gemini-3.5-flash:streamGenerateContent", googleBody},
	}
	for _, disabled := range []bool{false, true} {
		for _, c := range paths {
			s, _ := echoServer(t, echoReply)
			s.cfg.DisableEchoFilter = disabled
			b := post(s, c.path, c.body).Body.String()
			has := strings.Contains(b, "Tool result for")
			if has != disabled {
				t.Errorf("%s disabled=%v: echo present=%v: %s", c.name, disabled, has, b)
			}
			if got := s.stats.echoStripped.Load(); (got != 0) == disabled {
				t.Errorf("%s disabled=%v: echoStripped=%d", c.name, disabled, got)
			}
		}
	}
}
