package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeFakeScript(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "refresh.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitForCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func newCookieTestClient(t *testing.T, initialCookie string, stateAge time.Duration) (*gemini.Client, string, string) {
	t.Helper()
	dir := t.TempDir()
	cookieFile := filepath.Join(dir, "cookie.txt")
	stateFile := filepath.Join(dir, "cookie.txt.state.json")
	if err := os.WriteFile(cookieFile, []byte(initialCookie+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTS := time.Now().Add(-stateAge).UTC().Format(time.RFC3339)
	st := fmt.Sprintf(`{"version":1,"saved_at":%q,"session_fingerprint":"%s","at":"AT_OLD","at_at":%q,"bl":"bl_old","bl_at":%q,"psidts_at":%q}`,
		oldTS, "", oldTS, oldTS, oldTS)
	// Let Client compute fingerprint by restoring or just writing psidts_at via RestoreState after a real save;
	// simpler: set CookieFile and write state with matching sha256(SAPISID)[:16] or use a helper.
	_ = st
	cfg := gemini.Config{
		GeminiBL:           "bl_old",
		CookieFile:         cookieFile,
		StateFile:          stateFile,
		StateMaxAge:        24 * time.Hour,
		RotateSuspendAfter: 1,
	}
	c, err := gemini.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c, cookieFile, stateFile
}

func TestK10_ConfigAndSplitCommandArgs(t *testing.T) {
	t.Setenv("GEMINI_COOKIE_AUTO_REFRESH_MIN", "")
	t.Setenv("GEMINI_COOKIE_REFRESH_CMD", "")
	cfg := loadConfig(filepath.Join(t.TempDir(), "missing.env"))
	if cfg.CookieAutoRefreshMin != 0 || cfg.CookieRefreshCmd != "" {
		t.Fatalf("defaults = (%d, %q), want (0, \"\")", cfg.CookieAutoRefreshMin, cfg.CookieRefreshCmd)
	}

	args, err := splitCommandArgs(`node "/tmp/dir with spaces/tool.mjs" refresh`)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 || args[0] != "node" || args[1] != "/tmp/dir with spaces/tool.mjs" || args[2] != "refresh" {
		t.Fatalf("args = %#v", args)
	}

	// Empty command never launches anything.
	c, _, _ := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 2*time.Hour)
	ar := newCookieAutoRefresher(&Config{CookieAutoRefreshMin: 90, CookieRefreshCmd: ""}, c, nil)
	ar.Tick()
	if st := ar.Status(); st.Configured || st.Enabled || st.LastAttemptAt != nil {
		t.Fatalf("empty cmd should not run: %+v", st)
	}
}

func TestK10_BackgroundTriggerActivityAndHotReload(t *testing.T) {
	c, cookieFile, _ := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 2*time.Hour)
	// Simulate 1 failed rotation so rotation is suspended before refresh.
	c.SetTransport(roundTripFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	}))
	c.RotateCookie(context.Background())
	if !c.RotationStatus().Suspended {
		t.Fatal("expected rotation to be suspended before refresh")
	}

	markerFile := filepath.Join(t.TempDir(), "ran")
	script := writeFakeScript(t, fmt.Sprintf(`
echo "SECRET_PATH_LEAK"
echo "SECRET_ERR_LEAK" >&2
printf "SAPISID=s1; __Secure-1PSIDTS=t2\n" > %q
touch %q
`, cookieFile, markerFile))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	ar := newCookieAutoRefresher(&Config{
		CookieAutoRefreshMin: 90,
		CookieRefreshCmd:     script,
	}, c, logger)
	// Token age is unknown (no state file), so advance clock past 90m.
	now := time.Now()
	ar.nowFn = func() time.Time { return now }
	ar.startedAt = now.Add(-100 * time.Minute)

	// Idle: Tick must NOT launch.
	ar.Tick()
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatal("Tick should not launch while proxy is idle")
	}

	// Active within 90m: Tick MUST launch, hot-reload cookie.txt, and unsuspend rotation.
	ar.lastActivityAt = now.Add(-5 * time.Minute)
	ar.Tick()
	waitForCondition(t, 2*time.Second, func() bool {
		st := ar.Status()
		return !st.Running && st.LastOK != nil && *st.LastOK
	})
	if c.RotationStatus().Suspended {
		t.Fatal("expected rotation suspension to be lifted after successful refresh")
	}
	logs := logBuf.String()
	if strings.Contains(logs, "SECRET_PATH_LEAK") || strings.Contains(logs, "SECRET_ERR_LEAK") {
		t.Fatalf("command output must never be logged: %s", logs)
	}
	if !strings.Contains(logs, "cookie_auto_refresh") || !strings.Contains(logs, "exit_code=0") {
		t.Fatalf("expected single summary log line, got: %s", logs)
	}
}

func TestK10_OnDemandWaitProgressNoteAndTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 15
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.CookieAutoRefreshMin = 90
	script := writeFakeScript(t, "sleep 0.05")
	cfg.CookieRefreshCmd = script
	s := newTestServer(t, cfg)
	now := time.Now()
	s.autoRefresh.nowFn = func() time.Time { return now }
	s.autoRefresh.startedAt = now.Add(-2 * time.Hour)
	s.autoRefresh.waitTimeout = 2 * time.Second

	s.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("OK")
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`))
	req.Host = "127.0.0.1:8081"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GMINI : actualisation de la session…") {
		t.Errorf("expected progress note in SSE stream, got:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "OK") {
		t.Errorf("expected request to proceed after refresh, got:\n%s", rec.Body.String())
	}

	// Timeout cap (45s, shrunk to 40ms): request must still proceed even if command is slow.
	c2, _, _ := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 2*time.Hour)
	slowScript := writeFakeScript(t, "sleep 0.5")
	ar2 := newCookieAutoRefresher(&Config{CookieAutoRefreshMin: 90, CookieRefreshCmd: slowScript}, c2, nil)
	ar2.nowFn = func() time.Time { return now }
	ar2.startedAt = now.Add(-2 * time.Hour)
	ar2.waitTimeout = 40 * time.Millisecond
	ar2.execTimeout = 150 * time.Millisecond

	t0 := time.Now()
	waited := false
	ar2.PrepareRequest(context.Background(), func() { waited = true })
	if !waited {
		t.Fatal("expected onWait callback to be called")
	}
	if elapsed := time.Since(t0); elapsed > 250*time.Millisecond {
		t.Fatalf("PrepareRequest exceeded wait cap: %s", elapsed)
	}
	waitForCondition(t, 2*time.Second, func() bool { return !ar2.Status().Running })
}

func TestK10_GuardrailsPauseResumeAndEarlyTrigger(t *testing.T) {
	c, cookieFile, _ := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 2*time.Hour)
	countFile := filepath.Join(t.TempDir(), "count")
	failScript := writeFakeScript(t, fmt.Sprintf("echo x >> %q\nexit 1", countFile))

	ar := newCookieAutoRefresher(&Config{CookieAutoRefreshMin: 90, CookieRefreshCmd: failScript}, c, nil)
	ar.minSpacing = 20 * time.Millisecond

	// 2 consecutive failures -> paused.
	ar.TriggerEarly()
	waitForCondition(t, 2*time.Second, func() bool { return !ar.Status().Running })
	time.Sleep(30 * time.Millisecond)
	ar.TriggerEarly()
	waitForCondition(t, 2*time.Second, func() bool { return !ar.Status().Running })

	st := ar.Status()
	if !st.Paused || st.ConsecutiveFailures != 2 {
		t.Fatalf("expected paused after 2 failures, got %+v", st)
	}

	// 3rd trigger must be skipped while paused.
	time.Sleep(30 * time.Millisecond)
	ar.TriggerEarly()
	time.Sleep(30 * time.Millisecond)
	data, _ := os.ReadFile(countFile)
	if lines := strings.Count(string(data), "\n"); lines != 2 {
		t.Fatalf("expected 2 runs before pause, got %d", lines)
	}

	// Changing cookie.txt resumes auto-refresh.
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(cookieFile, []byte("SAPISID=s1; __Secure-1PSIDTS=t_new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := ar.Status(); st.Paused || st.ConsecutiveFailures != 0 {
		t.Fatalf("expected pause cleared after cookie.txt change, got %+v", st)
	}

	// Min spacing blocks immediate fourth launch.
	time.Sleep(30 * time.Millisecond)
	ar.TriggerEarly()
	waitForCondition(t, 2*time.Second, func() bool { return !ar.Status().Running })
	ar.minSpacing = time.Hour
	ar.TriggerEarly()
	time.Sleep(30 * time.Millisecond)
	data, _ = os.ReadFile(countFile)
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Fatalf("expected minSpacing to block 4th run, got %d", lines)
	}

	// Google block prevents launch.
	c.SetTransport(roundTripFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("sorry")), Request: r}, nil
	}))
	_, _ = c.Generate(context.Background(), gemini.GenParams{Prompt: "p"})
	if !c.GoogleBlockStatus().Active {
		t.Fatal("expected google block active")
	}
	ar.minSpacing = 0
	ar.TriggerEarly()
	time.Sleep(30 * time.Millisecond)
	data, _ = os.ReadFile(countFile)
	if lines := strings.Count(string(data), "\n"); lines != 3 {
		t.Fatalf("expected google block to prevent run, got %d", lines)
	}
}

func TestK10_UIToggleProtectedAndStatus(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.CookieAutoRefreshMin = 90
	cfg.CookieRefreshCmd = writeFakeScript(t, "exit 0")
	s := newTestServer(t, cfg)

	// Missing X-GMINI-UI token -> 401.
	req := httptest.NewRequest(http.MethodPost, "/_ui/api/cookie-refresh/toggle", nil)
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}

	// Valid token -> toggles off, then on.
	req = httptest.NewRequest(http.MethodPost, "/_ui/api/cookie-refresh/toggle", strings.NewReader(`{"enabled":false}`))
	req.Host = "127.0.0.1:8081"
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-GMINI-UI", s.uiToken)
	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle status = %d body = %s", rec.Code, rec.Body.String())
	}
	var st cookieAutoRefreshStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Fatalf("expected enabled=false, got %+v", st)
	}
}

func TestB8_TickUsesWallClockAcrossSleep(t *testing.T) {
	cfg := testConfig()
	cfg.CookieAutoRefreshMin = 90
	cfg.CookieRefreshCmd = writeFakeScript(t, "exit 0")
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ar := newCookieAutoRefresher(cfg, client, testLogger())

	// Simulate a request served 3h13m ago in wall-clock time, with monotonic clock
	// NOT advancing (as happens on macOS during system sleep).
	base := time.Now()
	ar.nowFn = func() time.Time { return base }
	ar.PrepareRequest(context.Background(), nil)

	// Wall clock jumps +3h13m, while monotonic clock only advances +10s.
	ar.nowFn = func() time.Time {
		return base.Round(0).Add(3*time.Hour + 13*time.Minute)
	}
	ar.startedAt = base.Round(0).Add(-4 * time.Hour)
	ar.Tick()

	st := ar.Status()
	if st.Running || st.LastAttemptAt != nil {
		t.Fatalf("expected background Tick NOT to fire after 3h13m wall-clock inactivity, got %+v", st)
	}
}
