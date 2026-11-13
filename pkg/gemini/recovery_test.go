package gemini

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const appPageAnon = `<html>"cfb2h":"boq_assistant-bard-web-server_20261110.00_p0"</html>`
const appPageAuth = `<html>"cfb2h":"boq_assistant-bard-web-server_20261110.00_p0" "SNlM0e":"AT_TOKEN_1"</html>`

func resp200(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func newAutoBLClient(t *testing.T, cfg Config, rt http.RoundTripper) (*Client, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	cfg.GeminiBLAuto = true
	c, err := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	return c, &logs
}

func TestStartupBuildLabelAndAuthShareOnePageFetch(t *testing.T) {
	var appCalls atomic.Int32
	c, _ := newAutoBLClient(t, Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts", GeminiBL: "fallback"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		appCalls.Add(1)
		return resp200(r, appPageAuth), nil
	}))
	c.ResolveBuildLabel(context.Background())
	if got := c.CheckAuth(context.Background()); got != "authenticated" {
		t.Fatalf("auth = %q", got)
	}
	if appCalls.Load() != 1 {
		t.Errorf("/app fetched %d times, want 1", appCalls.Load())
	}
	if !strings.HasPrefix(c.CurrentBL(), "boq_assistant") {
		t.Errorf("bl = %q", c.CurrentBL())
	}
}

func TestStartupRotateSkippedWhenTokenRecent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(path, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	c, logs := newAutoBLClient(t, Config{CookieFile: path}, rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return resp200(r, ""), nil
	}))
	c.loadCookie() // primes the mtime used as token age
	c.StartupRotate(context.Background())
	if calls.Load() != 0 || !strings.Contains(logs.String(), "rotation skipped") {
		t.Errorf("recent token: calls=%d log=%s", calls.Load(), logs)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	c.loadCookie()
	c.StartupRotate(context.Background())
	if calls.Load() == 0 {
		t.Error("stale token must trigger a rotation")
	}
}

func TestBackgroundSuspendedDuringBlock(t *testing.T) {
	oldS := blockSchedule
	blockSchedule = []time.Duration{time.Hour}
	defer func() { blockSchedule = oldS }()
	var calls atomic.Int32
	c, _ := newAutoBLClient(t, Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts", GeminiBL: "x"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return resp200(r, ""), nil
	}))
	c.recordBlock(&BlockedError{Host: "h", Path: "/sorry/"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.CookieRefreshLoop(ctx, 10*time.Millisecond) // many ticks, all suspended
	_ = c.CurrentBL()                             // stale BL refresh must not fire either
	c.maybeRefreshModels()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("%d Google calls while blocked, want 0", calls.Load())
	}
}

func TestAuthRecoveryAfterBlockAndAnonymousStart(t *testing.T) {
	oldS, oldJ, oldB, oldP := authRecoverySchedule, jitterFn, blockSchedule, probeWindow
	defer func() { authRecoverySchedule, jitterFn, blockSchedule, probeWindow = oldS, oldJ, oldB, oldP }()
	authRecoverySchedule = []time.Duration{10 * time.Millisecond}
	jitterFn = func(d time.Duration) time.Duration { return d }
	blockSchedule = []time.Duration{20 * time.Millisecond}
	probeWindow = 5 * time.Millisecond

	var mu sync.Mutex
	phase := 0 // 0: blocked, 1: anonymous page, 2: authenticated
	var appCalls int
	c, logs := newAutoBLClient(t, Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts", GeminiBL: "x"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/app") {
			appCalls++
			switch {
			case appCalls == 1:
				return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			case appCalls == 2:
				return resp200(r, appPageAnon), nil
			default:
				return resp200(r, appPageAuth), nil
			}
		}
		_ = phase
		return resp200(r, ""), nil
	}))
	if got := c.CheckAuth(context.Background()); !strings.Contains(got, "blocked") {
		t.Fatalf("startup auth = %q", got)
	}
	if c.Authenticated() {
		t.Fatal("must start anonymous")
	}
	var recovered atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.AuthRecoveryLoop(ctx, func() { recovered.Store(true) }); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery loop did not finish")
	}
	if !c.Authenticated() || !recovered.Load() {
		t.Errorf("authenticated=%v recovered=%v\n%s", c.Authenticated(), recovered.Load(), logs)
	}
	if !strings.Contains(logs.String(), "session recovered") {
		t.Errorf("no INFO recovery log:\n%s", logs)
	}
	if bs := c.GoogleBlockStatus(); bs.Active {
		t.Errorf("block should be cleared: %+v", bs)
	}
}
