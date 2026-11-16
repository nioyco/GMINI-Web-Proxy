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
			longPrefix := strings.Repeat("Hello ", 80) // > earlyHold (400 bytes), so released to client
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
