package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// loggedServer builds a test server whose Info logs go to a buffer.
func loggedServer(t *testing.T, cfg *Config, format string) (*Server, *lockedBuf) {
	t.Helper()
	buf := &lockedBuf{}
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels(testModels())
	return newServer(cfg, client, newLoggerTo(buf, true, format)), buf
}

func TestRequestIDHandling(t *testing.T) {
	s := newTestServer(t, testConfig())
	h := s.handler()

	do := func(in string) string {
		rr := httptest.NewRecorder()
		req := newReq(http.MethodGet, "/", nil)
		if in != "" {
			req.Header.Set("X-Request-ID", in)
		}
		h.ServeHTTP(rr, req)
		return rr.Header().Get("X-Request-ID")
	}
	if got := do("abc-123_X.9"); got != "abc-123_X.9" {
		t.Errorf("valid id not echoed: %q", got)
	}
	hex16 := regexp.MustCompile(`^[0-9a-f]{16}$`)
	for _, bad := range []string{"has space", "a\r\nb", strings.Repeat("a", 65), "é", "x;y"} {
		if got := do(bad); got == bad || !hex16.MatchString(got) {
			t.Errorf("invalid id %q → %q, want generated", bad, got)
		}
	}
	a, b := do(""), do("")
	if !hex16.MatchString(a) || a == b {
		t.Errorf("generated ids = %q, %q", a, b)
	}
	// Also set on rejected requests (403 host).
	rr := httptest.NewRecorder()
	req := newReq(http.MethodGet, "/", nil)
	req.Host = "evil.example"
	h.ServeHTTP(rr, req)
	if rr.Code != 403 || rr.Header().Get("X-Request-ID") == "" {
		t.Errorf("403 response must carry X-Request-ID (code %d)", rr.Code)
	}
}

func TestRequestLogsNoSecrets(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"SECRETKEY123"}
	cfg.SSEKeepaliveSec = 1
	s, buf := loggedServer(t, cfg, "json")
	s.generate = func(context.Context, gemini.GenParams) (string, error) {
		return "REPLYTEXT_SECRET ok\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"TOOLARG_SECRET\"}}\n```", nil
	}
	body := strings.Replace(toolsStreamBody, "weather in NYC?", "PROMPT_SECRET", 1)
	rr := httptest.NewRecorder()
	req := newReq(http.MethodPost, "/v1/chat/completions?key=SECRETKEY123", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer SECRETKEY123")
	req.Header.Set("Cookie", "COOKIE_SECRET=1")
	req.Header.Set("X-Request-ID", "rid-42")
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	logs := buf.String()
	for _, secret := range []string{"PROMPT_SECRET", "REPLYTEXT_SECRET", "TOOLARG_SECRET", "SECRETKEY123", "COOKIE_SECRET", "key="} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q:\n%s", secret, logs)
		}
	}
	var start, end map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		switch m["msg"] {
		case "request_start":
			start = m
		case "request_end":
			end = m
		}
	}
	if start == nil || end == nil {
		t.Fatalf("missing request_start/request_end:\n%s", logs)
	}
	if end["request_id"] != "rid-42" || end["route"] != "/v1/chat/completions" ||
		end["model"] != "gemini-3.1-pro" || end["stream"] != true || end["status"].(float64) != 200 {
		t.Errorf("request_end = %v", end)
	}
	if end["keepalives"].(float64) < 1 || end["bytes"].(float64) <= 0 {
		t.Errorf("keepalives/bytes not recorded: %v", end)
	}
	if _, ok := end["duration_ms"]; !ok {
		t.Errorf("no duration_ms: %v", end)
	}
}

func TestLogFormatDefaultIsText(t *testing.T) {
	var b bytes.Buffer
	newLoggerTo(&b, true, "").Info("x", "k", "v")
	if !strings.Contains(b.String(), "msg=x") {
		t.Errorf("default must be text, got %q", b.String())
	}
	b.Reset()
	newLoggerTo(&b, true, "JSON").Info("x")
	if !strings.HasPrefix(b.String(), "{") {
		t.Errorf("json expected, got %q", b.String())
	}
}
