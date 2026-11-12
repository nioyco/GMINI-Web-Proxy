package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

const (
	responsesToolsBody = `{"model":"gemini-3.1-pro","stream":true,"input":"weather in NYC?",
"tools":[{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	messagesToolsBody = `{"model":"gemini-3.1-pro","stream":true,"max_tokens":100,
"messages":[{"role":"user","content":"weather in NYC?"}],
"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	googleToolsBody = `{"contents":[{"role":"user","parts":[{"text":"weather in NYC?"}]}],
"tools":[{"functionDeclarations":[{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}]}`
)

type routeCase struct {
	name, path, body, finalMarker string
	errMarker                     string // substring expected in the in-band error frame
}

var routeCases = []routeCase{
	{"responses", "/v1/responses", responsesToolsBody, "response.completed", "event: error"},
	{"messages", "/v1/messages", messagesToolsBody, "message_stop", "event: error"},
	{"google", "/v1beta/models/gemini-3.1-pro:streamGenerateContent?alt=sse", googleToolsBody, "get_weather", `"error"`},
}

func postRaw(t *testing.T, ctx context.Context, url, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// Simulated (fake generation): every stream+tools route sends keepalive comments
// while generation is pending, then the final payload; same helper as chat.
func TestKeepaliveAllRoutes(t *testing.T) {
	interval, _, _, _ := keepaliveTimings()
	for _, rc := range routeCases {
		t.Run(rc.name, func(t *testing.T) {
			url, _ := keepaliveServer(t, interval, func(ctx context.Context, _ gemini.GenParams) (string, error) {
				select {
				case <-time.After(6 * interval):
					return toolReply, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
			resp, raw := postRaw(t, context.Background(), url, rc.path, rc.body)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status/ct = %d %q: %s", resp.StatusCode, resp.Header.Get("Content-Type"), raw)
			}
			if n := strings.Count(raw, ": keepalive\n\n"); n < 3 {
				t.Errorf("keepalive comments = %d, want >= 3; raw=%q", n, raw)
			}
			if !strings.HasPrefix(raw, ": keepalive") {
				t.Errorf("stream must start with a keepalive; raw=%q", raw)
			}
			if !strings.Contains(raw, rc.finalMarker) || !strings.Contains(raw, "get_weather") {
				t.Errorf("final payload missing (%q); raw=%q", rc.finalMarker, raw)
			}
		})
	}
}

func TestKeepaliveAllRoutesUpstreamError(t *testing.T) {
	interval, _, _, _ := keepaliveTimings()
	for _, rc := range routeCases {
		t.Run(rc.name, func(t *testing.T) {
			url, _ := keepaliveServer(t, interval, func(context.Context, gemini.GenParams) (string, error) {
				return "", errors.New("boom")
			})
			resp, raw := postRaw(t, context.Background(), url, rc.path, rc.body)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d (headers already sent)", resp.StatusCode)
			}
			if !strings.Contains(raw, "upstream error: boom") || !strings.Contains(raw, rc.errMarker) {
				t.Errorf("in-band error missing; raw=%q", raw)
			}
		})
	}
}

// failWriter makes every body write fail, simulating a closed connection.
type failWriter struct {
	httptest.ResponseRecorder
	writes int
}

func (f *failWriter) Write([]byte) (int, error) { f.writes++; return 0, errors.New("broken pipe") }

// A failed SSE write ends the wait and cancels the generation.
func TestKeepaliveWriteErrorStopsGeneration(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 1
	s := newTestServer(t, cfg)
	cancelled := make(chan struct{})
	s.keepaliveOverride = 20 * time.Millisecond
	s.generate = func(ctx context.Context, _ gemini.GenParams) (string, error) {
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}
	fw := &failWriter{ResponseRecorder: *httptest.NewRecorder()}
	req := newReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(toolsStreamBody))
	done := make(chan struct{})
	go func() { s.handler().ServeHTTP(fw, req); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not stop after write error")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("generation not cancelled after write error")
	}
	if fw.writes > 2 {
		t.Errorf("kept writing after failure: %d writes", fw.writes)
	}
}
