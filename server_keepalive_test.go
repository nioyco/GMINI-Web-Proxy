package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// Scenario timings. By default the 60s / 180s scenarios are scaled down so the
// suite stays fast; SSE_TEST_REALTIME=1 runs them with the real production
// values (15s keepalive, 60s and 180s generation).
func keepaliveTimings() (interval, shortWait, longWait, gapSlack time.Duration) {
	if os.Getenv("SSE_TEST_REALTIME") == "1" {
		return 15 * time.Second, 60 * time.Second, 180 * time.Second, 2 * time.Second
	}
	return 100 * time.Millisecond, 600 * time.Millisecond, 1800 * time.Millisecond, 120 * time.Millisecond
}

const toolReply = "ok\n```tool_call\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"NYC\"}}\n```"

const toolsStreamBody = `{"model":"gemini-3.1-pro","stream":true,
"messages":[{"role":"user","content":"weather in NYC?"}],
"tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

// sseCapture records what a client observed on the wire.
type sseCapture struct {
	status      int
	contentType string
	headerAfter time.Duration // time until response headers arrived
	comments    int           // ": ..." comment frames
	dataFrames  []string      // payloads of "data:" frames (excluding [DONE])
	done        bool          // saw "data: [DONE]"
	maxGap      time.Duration // largest gap between two reads that returned bytes
	raw         string
}

// readSSE issues the request and reads the body, timestamping every read.
func readSSE(t *testing.T, ctx context.Context, url, body string) *sseCapture {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	c := &sseCapture{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), headerAfter: time.Since(start)}

	var sb strings.Builder
	buf := make([]byte, 4096)
	last := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if gap := time.Since(last); gap > c.maxGap {
				c.maxGap = gap
			}
			last = time.Now()
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	c.raw = sb.String()
	for _, frame := range strings.Split(c.raw, "\n\n") {
		frame = strings.TrimSpace(frame)
		switch {
		case frame == "":
		case strings.HasPrefix(frame, ":"):
			c.comments++
		case frame == "data: [DONE]":
			c.done = true
		case strings.HasPrefix(frame, "data: "):
			c.dataFrames = append(c.dataFrames, strings.TrimPrefix(frame, "data: "))
		}
	}
	return c
}

// keepaliveServer starts the real handler with a fake generation function.
// handlerDone is closed each time a /v1/chat/completions handler returns.
func keepaliveServer(t *testing.T, interval time.Duration, gen func(context.Context, gemini.GenParams) (string, error)) (url string, handlerDone chan struct{}) {
	t.Helper()
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 1 // enables the path; the real period comes from keepaliveOverride
	srv := newServer(cfg, mustTestClient(t, cfg), testLogger())
	srv.generate = gen
	srv.keepaliveOverride = interval
	handlerDone = make(chan struct{}, 8)
	h := srv.handler() // the real routed handler, including handleChat
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { handlerDone <- struct{}{} }()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, handlerDone
}

func mustTestClient(t *testing.T, cfg *Config) *gemini.Client {
	t.Helper()
	c, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.SetModels(testModels())
	return c
}

// assertToolStream checks the invariants shared by every successful scenario.
func assertToolStream(t *testing.T, c *sseCapture) {
	t.Helper()
	if c.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", c.status)
	}
	if c.contentType != "text/event-stream" {
		t.Errorf("content-type = %q", c.contentType)
	}
	if !c.done {
		t.Errorf("missing terminating [DONE]; raw=%q", c.raw)
	}
	if !strings.HasSuffix(c.raw, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]; tail=%q", tail(c.raw))
	}
	// No fake content chunks: exactly one JSON data frame (the complete message).
	if len(c.dataFrames) != 1 {
		t.Fatalf("data frames = %d, want exactly 1 (no fake content chunks); raw=%q", len(c.dataFrames), c.raw)
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Function struct{ Name, Arguments string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(c.dataFrames[0]), &chunk); err != nil {
		t.Fatalf("final frame is not JSON: %v (%q)", err, c.dataFrames[0])
	}
	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("expected exactly one tool call in final frame, got %q", c.dataFrames[0])
	}
	tc := chunk.Choices[0].Delta.ToolCalls[0]
	if tc.Function.Name != "get_weather" || !strings.Contains(tc.Function.Arguments, "NYC") {
		t.Errorf("tool call = %+v", tc)
	}
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

// A: immediate generation with tools.
func TestKeepaliveA_Immediate(t *testing.T) {
	interval, _, _, _ := keepaliveTimings()
	url, _ := keepaliveServer(t, interval, func(context.Context, gemini.GenParams) (string, error) {
		return toolReply, nil
	})
	c := readSSE(t, context.Background(), url, toolsStreamBody)
	assertToolStream(t, c)
}

// B and C: delayed generation; keepalives must flow with a bounded gap.
func testKeepaliveDelay(t *testing.T, wait time.Duration) {
	interval, _, _, slack := keepaliveTimings()
	url, _ := keepaliveServer(t, interval, func(ctx context.Context, _ gemini.GenParams) (string, error) {
		select {
		case <-time.After(wait):
			return toolReply, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	c := readSSE(t, context.Background(), url, toolsStreamBody)
	assertToolStream(t, c)

	if c.headerAfter > interval {
		t.Errorf("headers arrived after %v, want immediately (< %v)", c.headerAfter, interval)
	}
	// 1 initial comment + one per elapsed interval (allow one tick of jitter).
	minComments := int(wait/interval) - 1 + 1
	if c.comments < minComments {
		t.Errorf("comments = %d, want >= %d for a %v wait", c.comments, minComments, wait)
	}
	if limit := interval + slack; c.maxGap > limit {
		t.Errorf("max gap between network reads = %v, want <= %v", c.maxGap, limit)
	}
	t.Logf("wait=%v interval=%v comments=%d maxGap=%v headers=%v", wait, interval, c.comments, c.maxGap.Round(time.Millisecond), c.headerAfter.Round(time.Millisecond))
}

func TestKeepaliveB_Wait60(t *testing.T) {
	_, short, _, _ := keepaliveTimings()
	testKeepaliveDelay(t, short)
}

func TestKeepaliveC_Wait180(t *testing.T) {
	_, _, long, _ := keepaliveTimings()
	testKeepaliveDelay(t, long)
}

// D: the client cancels while waiting. Generation must be cancelled, the handler
// must return, and nothing may be written afterwards.
func TestKeepaliveD_ClientCancel(t *testing.T) {
	interval, _, _, _ := keepaliveTimings()
	genExited := make(chan error, 1)
	var started atomic.Bool
	url, handlerDone := keepaliveServer(t, interval, func(ctx context.Context, _ gemini.GenParams) (string, error) {
		started.Store(true)
		<-ctx.Done()
		genExited <- ctx.Err()
		return "", ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/chat/completions", strings.NewReader(toolsStreamBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for at least one keepalive, then drop the connection.
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("expected initial bytes: %v", err)
	}
	cancel()
	resp.Body.Close()

	select {
	case err := <-genExited:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("generate saw %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("generate goroutine still running after client cancel (orphan)")
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after client cancel")
	}
	if !started.Load() {
		t.Fatal("generate never started")
	}
}

// E: Google/Gemini error during generation, after the SSE stream is open.
func TestKeepaliveE_UpstreamError(t *testing.T) {
	interval, short, _, _ := keepaliveTimings()
	url, _ := keepaliveServer(t, interval, func(ctx context.Context, _ gemini.GenParams) (string, error) {
		select {
		case <-time.After(short / 2):
			return "", errors.New("gemini request failed: boom")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	c := readSSE(t, context.Background(), url, toolsStreamBody)
	if c.status != http.StatusOK {
		t.Fatalf("status = %d: headers were already sent, must stay 200", c.status)
	}
	if c.comments < 1 {
		t.Errorf("expected keepalive comments before the error, got %d", c.comments)
	}
	if len(c.dataFrames) != 1 || !strings.Contains(c.dataFrames[0], `"error"`) || !strings.Contains(c.dataFrames[0], "boom") {
		t.Fatalf("expected one in-band error frame, got %v", c.dataFrames)
	}
	if strings.Contains(c.raw, "tool_calls") {
		t.Errorf("error stream must not contain tool calls: %q", c.raw)
	}
	if !c.done || !strings.HasSuffix(c.raw, "data: [DONE]\n\n") {
		t.Errorf("error stream must end with [DONE]; tail=%q", tail(c.raw))
	}
}

// The comment frame must be a pure SSE comment: no "data:" prefix, no JSON.
func TestWriteSSEComment(t *testing.T) {
	rr := httptest.NewRecorder()
	if err := writeSSEComment(rr, rr, "keepalive"); err != nil {
		t.Fatal(err)
	}
	if got := rr.Body.String(); got != ": keepalive\n\n" {
		t.Errorf("comment frame = %q, want %q", got, ": keepalive\n\n")
	}
}

// Config: default is 15s, 0 disables (legacy behaviour), explicit values win.
func TestSSEKeepaliveConfig(t *testing.T) {
	const none = "/nonexistent/.env"
	t.Run("default", func(t *testing.T) {
		t.Setenv("GEMINI_SSE_KEEPALIVE_SEC", "") // registers restore on cleanup
		os.Unsetenv("GEMINI_SSE_KEEPALIVE_SEC")
		if got := loadConfig(none).SSEKeepaliveSec; got != 15 {
			t.Errorf("default = %d, want 15", got)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("GEMINI_SSE_KEEPALIVE_SEC", "0")
		if got := loadConfig(none).SSEKeepaliveSec; got != 0 {
			t.Errorf("= %d, want 0", got)
		}
	})
	t.Run("custom", func(t *testing.T) {
		t.Setenv("GEMINI_SSE_KEEPALIVE_SEC", "7")
		if got := loadConfig(none).SSEKeepaliveSec; got != 7 {
			t.Errorf("= %d, want 7", got)
		}
	})
}
