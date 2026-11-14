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
	c.recordBlock(&BlockedError{Family: FamilyAccounts, Host: "h", Path: "/sorry/"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.CookieRefreshLoop(ctx, 10*time.Millisecond) // many ticks, all suspended
	_ = c.CurrentBL()                             // stale BL refresh must not fire either
	_, _ = c.ListModels(), c.ModelNames()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("%d Google calls while blocked, want 0", calls.Load())
	}
}

func shrinkRecovery(t *testing.T) {
	t.Helper()
	oldS, oldJ, oldB, oldA, oldP, oldPoll := authRecoverySchedule, jitterFn, blockSchedule, appBlockSchedule, probeWindow, authRecoveryPoll
	t.Cleanup(func() {
		authRecoverySchedule, jitterFn, blockSchedule, appBlockSchedule, probeWindow, authRecoveryPoll = oldS, oldJ, oldB, oldA, oldP, oldPoll
	})
	authRecoverySchedule = []time.Duration{10 * time.Millisecond}
	jitterFn = func(d time.Duration) time.Duration { return d }
	blockSchedule = []time.Duration{20 * time.Millisecond}
	appBlockSchedule = []time.Duration{20 * time.Millisecond}
	probeWindow = 5 * time.Millisecond
	authRecoveryPoll = 2 * time.Millisecond
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Block at startup → one probe after the backoff → page without token (dead
// cookie) → NO further /app load until the cookie changes → recovery.
func TestAuthRecoveryAfterBlockAndAnonymousStart(t *testing.T) {
	shrinkRecovery(t)
	var mu sync.Mutex
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
		return resp200(r, ""), nil
	}))
	app := func() int { mu.Lock(); defer mu.Unlock(); return appCalls }
	if got := c.CheckAuth(context.Background()); !strings.Contains(got, "blocked") {
		t.Fatalf("startup auth = %q", got)
	}
	if c.Authenticated() {
		t.Fatal("must start anonymous")
	}
	var recovered atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.AuthRecoveryLoop(ctx, func() { recovered.Add(1) }); close(done) }()
	waitFor(t, "the probe after the block", func() bool { return app() == 2 })
	time.Sleep(80 * time.Millisecond) // many polls, cookie unchanged
	if got := app(); got != 2 {
		t.Fatalf("/app loaded %d times with an unchanged dead cookie, want 2", got)
	}
	if err := c.storeCookie("SAPISID=s; __Secure-1PSIDTS=fresh"); err != nil { // re-exported cookie
		t.Fatal(err)
	}
	waitFor(t, "recovery", func() bool { return recovered.Load() == 1 })
	time.Sleep(30 * time.Millisecond) // authenticated: no more calls
	if got := app(); got != 3 || !c.Authenticated() {
		t.Errorf("/app calls = %d authenticated=%v", got, c.Authenticated())
	}
	cancel()
	<-done
	if !strings.Contains(logs.String(), "session recovered") {
		t.Errorf("no INFO recovery log:\n%s", logs)
	}
	if bs := c.GoogleBlockStatus(); bs.Active {
		t.Errorf("block should be cleared: %+v", bs)
	}
}
