package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// K18 T1: GEMINI_CODE_REF_FASTPATH defaults to true, BuildCommandToolCallFromCodeRef
// converts explicit shell code_reference blocks when a unique command tool is declared
// even without body[24][0], and triggerCap("duration") rescues a complete code_reference
// block instead of aborting with ErrEarlyStopAborted.
func TestK18_T1_CodeRefBashAndTriggerCap(t *testing.T) {
	if cfg := loadConfig(""); !cfg.CodeRefFastpath {
		t.Fatal("default CodeRefFastpath must be true")
	}
	t.Setenv("GEMINI_CODE_REF_FASTPATH", "false")
	if cfg := loadConfig(""); cfg.CodeRefFastpath {
		t.Fatal("GEMINI_CODE_REF_FASTPATH=false must disable fastpath")
	}

	runCmdChatBody := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"check repo"}],"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"},"WaitMsBeforeAsync":{"type":"integer"}},"required":["CommandLine","Cwd","WaitMsBeforeAsync"]}}}]}`
	bashBlock := "```bash?code_reference&code_event_index=1\ngit -C /tmp/repo status --short && ls -la /tmp/repo/subdir\n```\n"

	t.Run("BuildCommandToolCallFromCodeRef: shell tag without body24Tools", func(t *testing.T) {
		declared := []apiconv.DeclaredTool{{
			Name:      "run_command",
			PropTypes: map[string]string{"CommandLine": "string", "Cwd": "string", "WaitMsBeforeAsync": "integer"},
			Required:  []string{"CommandLine", "Cwd", "WaitMsBeforeAsync"},
		}}
		rawJSON, name, ok := apiconv.BuildCommandToolCallFromCodeRef(bashBlock, declared, nil)
		if !ok || name != "run_command" || !strings.Contains(rawJSON, "git -C /tmp/repo status --short") {
			t.Fatalf("expected direct shell conversion without body24Tools, got ok=%v name=%q raw=%q", ok, name, rawJSON)
		}

		// Non-shell tag (e.g. python) must still require body24Tools.
		pyBlock := "```python?code_reference&code_event_index=1\nprint('hi')\n```\n"
		if _, _, ok := apiconv.BuildCommandToolCallFromCodeRef(pyBlock, declared, nil); ok {
			t.Fatal("python code_reference without body24Tools must not convert")
		}

		// Multiple command tools declared without body24Tools must not guess.
		twoCmds := append(declared, apiconv.DeclaredTool{Name: "exec_shell", Props: map[string]bool{"command": true}, PropTypes: map[string]string{"command": "string"}})
		if _, _, ok := apiconv.BuildCommandToolCallFromCodeRef(bashBlock, twoCmds, nil); ok {
			t.Fatal("ambiguous multiple command tools without body24Tools must not convert")
		}
	})

	t.Run("fastpath=on without body24Tools (08:30:46 case) -> delivers tool_call immediately", func(t *testing.T) {
		var upstreamCancelled atomic.Bool
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			pr, pw := io.Pipe()
			go func() {
				pw.Write([]byte(fakeStreamBody24(bashBlock, "")))
				<-r.Context().Done()
				upstreamCancelled.Store(true)
				pw.Close()
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.CodeRefFastpath = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		start := time.Now()
		c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("fastpath took too long: %v", time.Since(start))
		}
		if !upstreamCancelled.Load() {
			t.Fatal("expected upstream request to be cancelled by fastpath")
		}
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, `"name":"run_command"`) || !strings.Contains(joined, `git -C /tmp/repo status --short`) {
			t.Fatalf("expected converted run_command tool_call, got: %s", joined)
		}
		if s.stats.codeRefFastpath.Load() != 1 {
			t.Fatalf("codeRefFastpath counter = %d, want 1", s.stats.codeRefFastpath.Load())
		}
	})

	t.Run("fastpath=off + triggerCap(duration) -> rescues bash code_reference instead of aborting", func(t *testing.T) {
		var upstreamCancelled atomic.Bool
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			pr, pw := io.Pipe()
			go func() {
				pw.Write([]byte(fakeStreamBody24(bashBlock, "")))
				<-r.Context().Done()
				upstreamCancelled.Store(true)
				pw.Close()
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.CodeRefFastpath = false
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.heldDurationOverride = 120 * time.Millisecond
		s.keepaliveOverride = 30 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
		if !upstreamCancelled.Load() {
			t.Fatal("expected upstream request to be cancelled by triggerCap rescue")
		}
		joined := strings.Join(c.dataFrames, "\n")
		if strings.Contains(joined, "s'emballe") {
			t.Fatalf("must not abort with runaway message when code_reference is convertible: %s", joined)
		}
		if !strings.Contains(joined, `"name":"run_command"`) || !strings.Contains(joined, `git -C /tmp/repo status --short`) {
			t.Fatalf("expected rescued run_command tool_call, got: %s", joined)
		}
		if s.stats.codeRefFastpath.Load() != 1 {
			t.Fatalf("codeRefFastpath counter = %d, want 1", s.stats.codeRefFastpath.Load())
		}
	})
}

// K18 T2: GEMINI_WAIT_NOTE_SEC defaults to 15s, an initial bold wait note is emitted
// 2s after stream open when !hasContent, and codeRefWaitThreshold defaults to 3s.
func TestK18_T2_WaitNoteFrom2sAndFormat(t *testing.T) {
	if cfg := loadConfig(""); cfg.WaitNoteSec != 15 {
		t.Fatalf("default WaitNoteSec = %d, want 15", cfg.WaitNoteSec)
	}
	t.Setenv("GEMINI_WAIT_NOTE_SEC", "0")
	if cfg := loadConfig(""); cfg.WaitNoteSec >= 0 {
		t.Fatalf("GEMINI_WAIT_NOTE_SEC=0 should disable wait notes, got %d", cfg.WaitNoteSec)
	}

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			// Delay 130ms before sending final text so initial + periodic wait notes fire.
			time.Sleep(130 * time.Millisecond)
			pw.Write([]byte(fakeStreamBody24("Bonjour !", "")))
			pw.Close()
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.initialWaitNoteOverride = 25 * time.Millisecond
	s.noContentNoteOverride = 70 * time.Millisecond
	s.warnNow = func() time.Time { return time.Date(2026, 11, 17, 9, 15, 0, 0, time.UTC) }

	if got := s.codeRefWaitThreshold(); got != 3*time.Second {
		t.Fatalf("codeRefWaitThreshold() = %v, want 3s", got)
	}

	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	content, reasoning := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "Bonjour !") {
		t.Fatalf("expected final content, got %q", content)
	}
	if !strings.Contains(reasoning, "**[09:15:00] GMINI — Demande envoyée à") {
		t.Fatalf("expected initial bold wait note at 2s, got %q", reasoning)
	}
	if !strings.Contains(reasoning, "**[09:15:00] GMINI — En attente de Google…**") {
		t.Fatalf("expected periodic bold wait note, got %q", reasoning)
	}
	if !strings.Contains(reasoning, "GMINI (pas le modèle — attente technique) :") {
		t.Fatalf("expected technical wait attribution, got %q", reasoning)
	}
}

// K18 T3: PrepareRequest launches on_demand refresh when enabled and tokenDueLocked(now)
// is true even if inactivity was 10–90 min (< activityWindow), where Tick() is blocked by quiet gate.
func TestK18_T3_PrepareRequestBlindSpot10To90Min(t *testing.T) {
	c, cookieFile, _ := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 70*time.Minute)
	now := time.Now().Round(0)
	realNow := time.Now()
	if err := os.Chtimes(cookieFile, realNow.Add(-70*time.Minute), realNow.Add(-70*time.Minute)); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(t.TempDir(), "ran_ondemand")
	script := writeFakeScript(t, "touch "+marker)
	ar := newCookieAutoRefresher(&Config{CookieAutoRefreshMin: 90, CookieRefreshCmd: script}, c, testLogger())
	ar.nowFn = func() time.Time { return now }
	ar.startedAt = now.Add(-70 * time.Minute)
	on := true
	ar.Toggle(&on)

	// t = 0: user activity (token age 70m < 90m -> no refresh).
	ar.PrepareRequest(context.Background(), nil)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("expected 0 refreshes at t=0 (age 70m < 90m)")
	}

	// t = +25 min: user paused 25 min (10 min < 25 min < 90 min activityWindow),
	// and token is now 95 min old (>= 90 min).
	// PrepareRequest must refresh on_demand even though wasIdle is false!
	now = now.Add(25 * time.Minute)
	if err := os.Chtimes(cookieFile, realNow.Add(-95*time.Minute), realNow.Add(-95*time.Minute)); err != nil {
		t.Fatal(err)
	}
	waited := false
	ar.PrepareRequest(context.Background(), func() { waited = true })
	if !waited {
		t.Fatal("expected on_demand refresh in 10-90 min window")
	}
	waitForCondition(t, 2*time.Second, func() bool { return !ar.Status().Running })
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected refresh script to have run: %v", err)
	}
}

// K18 T4: auto_refresh_enabled persists in .state.json on UI toggle and restores on restart,
// and GEMINI_COOKIE_AUTO_REFRESH_ENABLED in .env can also set it (default false if unset).
func TestK18_T4_AutoRefreshPersistAndEnv(t *testing.T) {
	dir := t.TempDir()
	cookieFile := filepath.Join(dir, "cookie.txt")
	stateFile := filepath.Join(dir, "cookie.txt.state.json")
	if err := os.WriteFile(cookieFile, []byte("SAPISID=s1; __Secure-1PSIDTS=t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := writeFakeScript(t, "echo ok")

	newClient := func() *gemini.Client {
		c, err := gemini.New(gemini.Config{
			GeminiBL:   "bl_old",
			CookieFile: cookieFile,
			StateFile:  stateFile,
		}, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		c.RestoreState()
		return c
	}

	// 1. Fresh start -> disabled by default.
	c1 := newClient()
	cfg1 := testConfig()
	cfg1.UIEnabled = true
	cfg1.CookieRefreshCmd = script
	cfg1.CookieAutoRefreshMin = 90
	s1 := newServer(cfg1, c1, testLogger())
	if s1.autoRefresh.Status().Enabled {
		t.Fatal("expected disabled on fresh start")
	}

	// 2. Toggle ON via UI API -> persisted to .state.json.
	uiRec := uiDo(s1.handler(), http.MethodPost, "/_ui/api/cookie-refresh/toggle", "127.0.0.1:12345", s1.uiToken)
	if uiRec.Code != http.StatusOK {
		t.Fatalf("toggle status=%d body=%s", uiRec.Code, uiRec.Body.String())
	}
	if !s1.autoRefresh.Status().Enabled {
		t.Fatal("expected enabled after toggle")
	}

	// 3. Simulated proxy restart -> restored as enabled from .state.json!
	c2 := newClient()
	s2 := newServer(cfg1, c2, testLogger())
	if !s2.autoRefresh.Status().Enabled {
		t.Fatal("expected auto_refresh_enabled=true to persist across restart")
	}

	// 4. Toggle OFF -> persisted as false across restart.
	uiDo(s2.handler(), http.MethodPost, "/_ui/api/cookie-refresh/toggle", "127.0.0.1:12345", s2.uiToken)
	c3 := newClient()
	s3 := newServer(cfg1, c3, testLogger())
	if s3.autoRefresh.Status().Enabled {
		t.Fatal("expected auto_refresh_enabled=false to persist after disabling")
	}

	// 5. Env var GEMINI_COOKIE_AUTO_REFRESH_ENABLED=true overrides when set.
	t.Setenv("GEMINI_COOKIE_AUTO_REFRESH_ENABLED", "true")
	cfgEnv := loadConfig("")
	if !cfgEnv.CookieAutoRefreshEnabledSet || !cfgEnv.CookieAutoRefreshEnabled {
		t.Fatalf("expected CookieAutoRefreshEnabled=true from env, got %+v", cfgEnv)
	}
	cfgEnv.CookieRefreshCmd = script
	s4 := newServer(cfgEnv, c3, testLogger())
	if !s4.autoRefresh.Status().Enabled {
		t.Fatal("expected GEMINI_COOKIE_AUTO_REFRESH_ENABLED=true to enable auto-refresh")
	}
}

// K18 T5: detect .env modification since startup via os.Stat (without exposing secrets)
// and hot-reload non-secret settings via POST /_ui/api/config/reload.
func TestK18_T5_EnvModifiedAndReload(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("GEMINI_CODE_REF_FASTPATH=true\nGEMINI_WAIT_NOTE_SEC=15\nGEMINI_COOKIE=SUPER_SECRET_COOKIE_VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-5 * time.Minute)
	_ = os.Chtimes(envPath, t0, t0)

	cfg := loadConfig(envPath)
	cfg.UIEnabled = true
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	s := newServer(cfg, client, testLogger())

	// Initially not modified.
	stRec1 := uiDo(s.handler(), http.MethodGet, "/_status", "127.0.0.1:12345", "")
	if stRec1.Code != http.StatusOK {
		t.Fatalf("/_status code=%d", stRec1.Code)
	}
	if strings.Contains(stRec1.Body.String(), "SUPER_SECRET_COOKIE_VALUE") {
		t.Fatal("secret from .env leaked in /_status")
	}
	var st1 statusResponse
	if err := json.Unmarshal(stRec1.Body.Bytes(), &st1); err != nil {
		t.Fatal(err)
	}
	if st1.EnvModifiedSinceStart || st1.ConfigLoadedAt == "" || st1.WaitNoteSec != 15 {
		t.Fatalf("unexpected initial status: modified=%v loadedAt=%q waitNote=%d", st1.EnvModifiedSinceStart, st1.ConfigLoadedAt, st1.WaitNoteSec)
	}

	// Modify .env on disk after startup.
	if err := os.WriteFile(envPath, []byte("GEMINI_CODE_REF_FASTPATH=false\nGEMINI_WAIT_NOTE_SEC=22\nGEMINI_COOKIE=SUPER_SECRET_COOKIE_VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(time.Second)
	_ = os.Chtimes(envPath, t1, t1)

	stRec2 := uiDo(s.handler(), http.MethodGet, "/_status", "127.0.0.1:12345", "")
	var st2 statusResponse
	if err := json.Unmarshal(stRec2.Body.Bytes(), &st2); err != nil {
		t.Fatal(err)
	}
	if !st2.EnvModifiedSinceStart {
		t.Fatal("expected env_modified_since_start=true after .env modification")
	}

	// Reload via POST /_ui/api/config/reload (requires valid UI token).
	if wBad := uiDo(s.handler(), http.MethodPost, "/_ui/api/config/reload", "127.0.0.1:12345", ""); wBad.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without UI token, got %d", wBad.Code)
	}
	wReload := uiDo(s.handler(), http.MethodPost, "/_ui/api/config/reload", "127.0.0.1:12345", s.uiToken)
	if wReload.Code != http.StatusOK {
		t.Fatalf("reload status=%d body=%s", wReload.Code, wReload.Body.String())
	}
	if strings.Contains(wReload.Body.String(), "SUPER_SECRET_COOKIE_VALUE") {
		t.Fatal("secret leaked in reload response")
	}

	// After reload: env_modified_since_start is false and non-secret settings updated.
	stRec3 := uiDo(s.handler(), http.MethodGet, "/_status", "127.0.0.1:12345", "")
	var st3 statusResponse
	if err := json.Unmarshal(stRec3.Body.Bytes(), &st3); err != nil {
		t.Fatal(err)
	}
	if st3.EnvModifiedSinceStart || st3.CodeRefFastpath || st3.WaitNoteSec != 22 {
		t.Fatalf("unexpected post-reload status: modified=%v fastpath=%v waitNote=%d", st3.EnvModifiedSinceStart, st3.CodeRefFastpath, st3.WaitNoteSec)
	}
}
