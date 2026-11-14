package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

func startRealServer(t *testing.T, s *Server) (base string, hs *http.Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs = &http.Server{Handler: s.handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	return "http://" + ln.Addr().String(), hs
}

type streamResult struct {
	status int
	body   string
}

func streamAsync(base string) <-chan streamResult {
	out := make(chan streamResult, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(toolsStreamBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- streamResult{0, err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		out <- streamResult{resp.StatusCode, string(b)}
	}()
	return out
}

func waitActive(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.active.count() != n {
		if time.Now().After(deadline) {
			t.Fatalf("active = %d, want %d", s.active.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A stream that outlives the grace period is cancelled at expiry, logged as
// aborted_on_shutdown, and the client gets an in-band error + [DONE].
func TestShutdownAbortsStreamAfterGrace(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 1
	s, logs := loggedServer(t, cfg, "text")
	s.keepaliveOverride = 20 * time.Millisecond
	genCancelled := make(chan struct{})
	s.generate = func(ctx context.Context, _ gemini.GenParams) (string, error) {
		<-ctx.Done() // never finishes on its own
		close(genCancelled)
		return "", ctx.Err()
	}
	base, hs := startRealServer(t, s)
	res := streamAsync(base)
	waitActive(t, s, 1)

	grace := 400 * time.Millisecond
	start := time.Now()
	shutdownServer(hs, s, grace, s.log)
	took := time.Since(start)

	if took < grace-50*time.Millisecond {
		t.Errorf("shutdown returned after %v, stream must survive until the grace (%v)", took, grace)
	}
	if took > grace+abortFlushWait+time.Second {
		t.Errorf("shutdown took too long: %v", took)
	}
	select {
	case <-genCancelled:
	case <-time.After(time.Second):
		t.Fatal("generation context not cancelled at grace expiry")
	}
	r := <-res
	if r.status != 200 || !strings.Contains(r.body, "server shutting down") || !strings.HasSuffix(r.body, "data: [DONE]\n\n") {
		t.Errorf("client saw %d %q", r.status, r.body)
	}
	if n := strings.Count(r.body, ": keepalive"); n < 5 {
		t.Errorf("keepalives during grace = %d, want >= 5", n)
	}
	if !strings.Contains(logs.String(), "aborted_on_shutdown") {
		t.Errorf("missing aborted_on_shutdown log:\n%s", logs.String())
	}
	if got := s.stats.abortedOnShutdown.Load(); got != 1 {
		t.Errorf("abortedOnShutdown = %d", got)
	}
}

// A stream finishing inside the grace completes normally and nothing is aborted.
func TestShutdownLetsStreamFinishWithinGrace(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 1
	s, logs := loggedServer(t, cfg, "text")
	s.keepaliveOverride = 20 * time.Millisecond
	s.generate = func(ctx context.Context, _ gemini.GenParams) (string, error) {
		select {
		case <-time.After(300 * time.Millisecond):
			return toolReply, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	base, hs := startRealServer(t, s)
	res := streamAsync(base)
	waitActive(t, s, 1)

	shutdownServer(hs, s, 5*time.Second, s.log)
	r := <-res
	if r.status != 200 || !strings.Contains(r.body, "get_weather") || strings.Contains(r.body, "shutting down") {
		t.Errorf("stream should complete normally: %d %q", r.status, r.body)
	}
	if strings.Contains(logs.String(), "aborted_on_shutdown") {
		t.Errorf("nothing should be aborted:\n%s", logs.String())
	}
}

func TestShutdownGraceConfig(t *testing.T) {
	const none = "/nonexistent/.env"
	t.Setenv("GEMINI_SHUTDOWN_GRACE_SEC", "")
	if got := loadConfig(none).ShutdownGraceSec; got != 30 {
		t.Errorf("empty value → %d, want default 30", got)
	}
	t.Setenv("GEMINI_SHUTDOWN_GRACE_SEC", "7")
	if got := loadConfig(none).ShutdownGraceSec; got != 7 {
		t.Errorf("= %d, want 7", got)
	}
	t.Setenv("GEMINI_SHUTDOWN_GRACE_SEC", "-3")
	if got := loadConfig(none).ShutdownGraceSec; got != 30 {
		t.Errorf("negative → %d, want 30", got)
	}
}

// AUDIT-13 A13 (F-01): main's serve loop shuts down through shutdownServer with
// the configured grace, not a fixed 5 s Shutdown.
func TestRunServerUsesConfiguredGrace(t *testing.T) {
	cfg := testConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.ShutdownGraceSec = 42
	s := newTestServer(t, cfg)
	hs := newHTTPServer(cfg, s.handler())
	var got time.Duration
	called := false
	old := shutdownFn
	shutdownFn = func(h *http.Server, srv *Server, grace time.Duration, l *slog.Logger) {
		called, got = true, grace
		_ = h.Close()
	}
	defer func() { shutdownFn = old }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, hs, s, cfg, testLogger()) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return")
	}
	if !called || got != 42*time.Second {
		t.Errorf("shutdownServer called=%v grace=%v, want 42s", called, got)
	}
}
