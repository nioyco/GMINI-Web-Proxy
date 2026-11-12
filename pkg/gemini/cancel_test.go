package gemini

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cancelClient(t *testing.T, attempts, delay int, rt http.RoundTripper) (*Client, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c, err := New(Config{RetryAttempts: attempts, RetryDelaySec: delay, GeminiBL: "bl"}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	return c, &logs
}

// blockingRT waits for the request context to end, like a slow upstream.
func blockingRT(calls *atomic.Int32) rtFunc {
	return func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
}

func TestCancelDuringUpstreamWaitNoRetry(t *testing.T) {
	var calls atomic.Int32
	c, logs := cancelClient(t, 3, 30, blockingRT(&calls))
	ctx, cancel := context.WithCancel(context.Background())
	var m CallMetrics
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, err := c.Generate(WithMetrics(ctx, &m), GenParams{Prompt: "p"})
	if err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	if time.Since(t0) > 2*time.Second || calls.Load() != 1 || m.Attempts.Load() != 1 {
		t.Errorf("elapsed=%v calls=%d attempts=%d", time.Since(t0), calls.Load(), m.Attempts.Load())
	}
	l := logs.String()
	if strings.Count(l, "client_cancelled") != 1 || strings.Contains(l, "retrying gemini request") || !strings.Contains(l, "upstream_attempts=1") {
		t.Errorf("log:\n%s", l)
	}
}

func TestCancelDuringRetryPause(t *testing.T) {
	var calls atomic.Int32
	c, logs := cancelClient(t, 3, 30, rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("garbage"))}, nil // empty -> retry
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	t0 := time.Now()
	_, err := c.Generate(ctx, GenParams{Prompt: "p"})
	if err != context.Canceled || time.Since(t0) > 2*time.Second || calls.Load() != 1 {
		t.Fatalf("err=%v elapsed=%v calls=%d", err, time.Since(t0), calls.Load())
	}
	if strings.Count(logs.String(), "client_cancelled") != 1 {
		t.Errorf("log:\n%s", logs)
	}
}

func TestNormalFailureStillRetries(t *testing.T) {
	var calls atomic.Int32
	c, logs := cancelClient(t, 3, 0, rtFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("garbage"))}, nil
	}))
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "p"}); err != ErrEmptyResponse {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 3 || strings.Count(logs.String(), "retrying gemini request") != 2 || strings.Contains(logs.String(), "client_cancelled") {
		t.Errorf("calls=%d log:\n%s", calls.Load(), logs)
	}
}

func TestCancelStreamAndPreCancelled(t *testing.T) {
	var calls atomic.Int32
	c, logs := cancelClient(t, 3, 30, blockingRT(&calls))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := c.GenerateStream(ctx, GenParams{Prompt: "p"}, func(string) {}); err != context.Canceled {
		t.Fatalf("stream err = %v", err)
	}
	if strings.Count(logs.String(), "client_cancelled") != 1 {
		t.Errorf("log:\n%s", logs)
	}
	// Already-cancelled context (e.g. while fetching images): upstream never called.
	calls.Store(0)
	dead, cancel2 := context.WithCancel(context.Background())
	cancel2()
	imgs := []InputImage{{Data: []byte("x"), Filename: "a.png"}}
	if _, err := c.Generate(dead, GenParams{Prompt: "p", Images: imgs}); err != context.Canceled || calls.Load() != 0 {
		t.Errorf("pre-cancelled: err=%v calls=%d", err, calls.Load())
	}
}
