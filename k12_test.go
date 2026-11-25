package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
)

func wrbBodyFrame(inner []any) string {
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + strings.Repeat(" ", 60) + "\n"
}

func TestB2_InternalToolActionModes(t *testing.T) {
	t.Run("notify (default) emits reasoning note without retrying", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			inner24 := make([]any, 25)
			inner24[24] = []any{[]any{[]any{"google", "search", 2}}}
			body := wrbBodyFrame(inner24) + fakeStreamServed("Final answer.", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, "Gemini lance un outil interne") {
			t.Errorf("expected notify reasoning note, got %s", joined)
		}
		if !strings.Contains(joined, "Final answer.") {
			t.Errorf("expected Final answer, got %s", joined)
		}
	})

	t.Run("retry cancels upstream and retries once with reinforced reminder", func(t *testing.T) {
		var calls atomic.Int32
		var prompts []string
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			b, _ := io.ReadAll(r.Body)
			prompts = append(prompts, string(b))
			if n == 1 {
				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					inner24 := make([]any, 25)
					inner24[24] = []any{[]any{[]any{"retriever", "expand_skills", 2}}}
					_, _ = pw.Write([]byte(wrbBodyFrame(inner24)))
					<-r.Context().Done()
				}()
				return &http.Response{StatusCode: 200, Body: pr}, nil
			}
			body := fakeStreamServed("Clean retry answer.", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.InternalToolAction = "retry"
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
		if len(prompts) < 2 || !strings.Contains(prompts[1], "CRITICAL+REMINDER") && !strings.Contains(prompts[1], "CRITICAL%20REMINDER") && !strings.Contains(prompts[1], "CRITICAL") {
			t.Errorf("second request missing reinforced reminder: %v", prompts)
		}
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, "Clean retry answer.") {
			t.Errorf("expected Clean retry answer, got %s", joined)
		}
	})

	t.Run("retry does NOT cancel after text has been transmitted", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			longPrefix := strings.Repeat("Hello ", 800) // > earlyHold (4500 bytes on 1st attempt when tools active), so released to client
			inner24 := make([]any, 25)
			inner24[24] = []any{[]any{[]any{"google", "search", 2}}}
			body := fakeStreamServed(longPrefix, "", "e6fa609c3fa255c0", "3.1 Pro") +
				wrbBodyFrame(inner24) +
				fakeStreamServed(longPrefix+"done.", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.InternalToolAction = "retry"
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1 (must not retry after text transmitted)", calls.Load())
		}
	})

	t.Run("off ignores internal tool signal", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			inner24 := make([]any, 25)
			inner24[24] = []any{[]any{[]any{"google", "search", 2}}}
			body := wrbBodyFrame(inner24) + fakeStreamServed("Plain answer.", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.InternalToolAction = "off"
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1", calls.Load())
		}
		joined := strings.Join(c.dataFrames, "\n")
		if strings.Contains(joined, "Gemini lance un outil interne") {
			t.Errorf("off mode must not emit notify note, got %s", joined)
		}
	})
}

func TestB6_CannedErrorDetectionAndIdenticalRetry(t *testing.T) {
	if !apiconv.IsUpstreamCannedError("I seem to be encountering an error. Can I try something else for you?") {
		t.Fatal("expected exact canned error to match")
	}
	if apiconv.IsUpstreamCannedError("Here is a long explanation about encountering an error in your Go code: " + strings.Repeat("x", 200)) {
		t.Fatal("long response must not match canned error")
	}

	t.Run("retries identically and succeeds on second attempt", func(t *testing.T) {
		var calls atomic.Int32
		var bodies []string
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			txt := "I seem to be encountering an error. Can I try something else for you?"
			if n == 2 {
				txt = "Recovered answer."
			}
			body := fakeStreamServed(txt, "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"
		cfg.CannedErrorRetrySec = 25
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
		if strings.Contains(bodies[1], "Retry instruction") {
			t.Errorf("canned error retry must be identical (no Retry instruction added)")
		}
		if !strings.Contains(strings.Join(c.dataFrames, "\n"), "Recovered answer.") {
			t.Errorf("expected Recovered answer, got %v", c.dataFrames)
		}
		stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
		var st struct {
			Counters map[string]int64 `json:"counters"`
		}
		json.Unmarshal(stRec.Body.Bytes(), &st)
		if st.Counters["canned_error_retries"] != 1 {
			t.Errorf("canned_error_retries = %d, want 1", st.Counters["canned_error_retries"])
		}
	})

	t.Run("two canned errors in a row emits clear French message", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := fakeStreamServed("I seem to be encountering an error. Can I try something else for you?", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"
		cfg.CannedErrorRetrySec = 25
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, "Gemini a renvoyé une erreur interne sur") {
			t.Errorf("expected clear French error message, got %s", joined)
		}
	})
}
