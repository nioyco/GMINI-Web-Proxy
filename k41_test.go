package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

// K41 — global premium quota pause, limit_banner logging, diagnostics embed.

const (
	k41AutoID    = "8c46e95b1a07cecc"
	k41HydroID   = "56fdd199312815e2"
	k41NeonID    = "e6fa609c3fa255c0"
	k41ChatHydro = `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	k41ChatNeon  = `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	k41ChatAuto  = `{"model":"gemini-auto","stream":false,"messages":[{"role":"user","content":"hello"}]}`
	k41ChatFlash = `{"model":"gemini-3.5-flash","stream":false,"messages":[{"role":"user","content":"hello"}]}`
)

type k41LogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *k41LogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *k41LogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// k41Frame builds a StreamGenerate frame with an optional served identity and
// an optional Google limit banner at body[52].
func k41Frame(text, servedID, servedName, banner string) string {
	cand := make([]any, 2)
	cand[0] = "rc_x"
	cand[1] = []any{text}
	body := make([]any, 53)
	body[4] = []any{cand}
	body[39], body[42] = servedID, servedName
	if banner != "" {
		body[52] = []any{[]any{banner}}
	}
	ib, _ := json.Marshal(body)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

// k41Server builds a server whose upstream always answers with the given frame
// and counts upstream calls.
func k41Server(t *testing.T, frame string) (*Server, *httptest.Server, *atomic.Int32, *k41LogBuf) {
	t.Helper()
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.PauseOnFallback = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: k41HydroID, DisplayName: "4 Hydrogen", Description: "4 Hydrogen"},
		{Name: "gemini-3.1-pro", ModelID: k41NeonID, DisplayName: "4 Neon", Description: "4 Neon"},
		{Name: "gemini-auto", ModelID: k41AutoID, DisplayName: "Auto", Description: "Auto"},
		{Name: "gemini-3.5-flash", ModelID: k41AutoID, DisplayName: "3.5 Flash", Description: "3.5 Flash"},
	})
	var calls atomic.Int32
	client.SetTransport(rtFn(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(frame))}, nil
	}))
	logs := &k41LogBuf{}
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(logs, nil)))
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts, &calls, logs
}

func k41Post(t *testing.T, url, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func k41DiagTypes(s *Server) map[string]string {
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	out := map[string]string{}
	for _, e := range s.diag.entries {
		if e.Kind == "incident" {
			out[e.Type] = e.Reason
		}
	}
	return out
}

func TestK41_IsAutoOrFlashModel(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *gemini.AvailableModel
		want bool
	}{
		{"nil", nil, false},
		{"auto", &gemini.AvailableModel{Name: "gemini-auto", ModelID: k41AutoID, DisplayName: "Auto"}, true},
		{"flash by id", &gemini.AvailableModel{Name: "x", ModelID: k41AutoID}, true},
		{"flash slug", &gemini.AvailableModel{Name: "gemini-3.5-flash", ModelID: "other"}, true},
		{"flash-lite slug", &gemini.AvailableModel{Name: "gemini-3.5-flash-lite", ModelID: "other"}, true},
		{"quick answers", &gemini.AvailableModel{Name: "gemini-quick-answers", ModelID: "other"}, true},
		{"hydrogen", &gemini.AvailableModel{Name: "gemini-everyday-help", ModelID: k41HydroID}, false},
		{"neon", &gemini.AvailableModel{Name: "gemini-3.1-pro", ModelID: k41NeonID}, false},
	} {
		if got := isAutoOrFlashModel(tc.m); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// 1. other_model (Hydrogen asked, Flash-Lite served) pauses all premium models;
// a following Neon call is rejected with 429 and zero upstream call.
func TestK41_OtherModelPausesAndBlocksNeon(t *testing.T) {
	s, ts, calls, logs := k41Server(t, k41Frame("hi", k41AutoID, "3.5 Flash-Lite", ""))

	resp, body := k41Post(t, ts.URL, "/v1/chat/completions", k41ChatHydro, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first request: status=%d body=%s", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") != "300" || !strings.Contains(body, "quota_fallback_paused") {
		t.Errorf("missing Retry-After/code: %v %s", resp.Header, body)
	}
	st := s.quotaPauseSnapshot()
	if !st.Active || st.Reason != "other_model" || st.RequestedID != k41HydroID || st.ServedID != k41AutoID || st.PausedAt == nil {
		t.Fatalf("unexpected pause state: %+v", st)
	}
	upstream := calls.Load()

	resp, body = k41Post(t, ts.URL, "/v1/chat/completions", k41ChatNeon, nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "300" {
		t.Fatalf("neon: status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{"gemini.google.com/usage", "Reprendre", "Auto / Flash", "Hydrogen"} {
		if !strings.Contains(body, want) {
			t.Errorf("429 message missing %q: %s", want, body)
		}
	}
	if calls.Load() != upstream {
		t.Errorf("Neon call reached Google (%d -> %d)", upstream, calls.Load())
	}
	if got := s.quotaPauseSnapshot().BlockedCount; got != 1 {
		t.Errorf("blocked_count=%d want 1", got)
	}
	types := k41DiagTypes(s)
	if _, ok := types["quota_fallback_pause"]; !ok {
		t.Errorf("diag incident quota_fallback_pause missing: %v", types)
	}
	if _, ok := types["quota_pause_blocked"]; !ok {
		t.Errorf("diag incident quota_pause_blocked missing: %v", types)
	}
	if l := logs.String(); !strings.Contains(l, "quota_fallback_pause") || !strings.Contains(l, "quota_pause_blocked") {
		t.Errorf("proxy.log missing quota entries")
	}

	// The other API flavours use their own error shape.
	resp, body = k41Post(t, ts.URL, "/v1/messages", `{"model":"gemini-3.1-pro","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`, nil)
	if resp.StatusCode != 429 || !strings.Contains(body, `"rate_limit_error"`) || !strings.Contains(body, `"type":"error"`) {
		t.Errorf("anthropic 429: %d %s", resp.StatusCode, body)
	}
	resp, body = k41Post(t, ts.URL, "/v1beta/models/gemini-3.1-pro:generateContent", `{"contents":[{"parts":[{"text":"x"}]}]}`, nil)
	if resp.StatusCode != 429 || !strings.Contains(body, "RESOURCE_EXHAUSTED") {
		t.Errorf("google 429: %d %s", resp.StatusCode, body)
	}
	resp, body = k41Post(t, ts.URL, "/v1/responses", `{"model":"gemini-3.1-pro","input":"x"}`, nil)
	if resp.StatusCode != 429 {
		t.Errorf("responses 429: %d %s", resp.StatusCode, body)
	}
	if calls.Load() != upstream {
		t.Errorf("blocked calls reached Google")
	}
}

// 2. A limit banner alone triggers the pause (stream+tools, SSE already open:
// the notice is delivered in-band) and is logged / recorded.
func TestK41_LimitBannerTriggersAndIsLogged(t *testing.T) {
	banner := "Limit reached. Try again later or switch to Fast."
	s, ts, _, logs := k41Server(t, k41Frame("partial", "", "", banner))

	resp, body := k41Post(t, ts.URL, "/v1/chat/completions", `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"go"}],`+
		`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "pause") || !strings.Contains(body, "Reprendre") {
		t.Errorf("in-band pause notice missing: %s", body)
	}
	if strings.Contains(body, "partial") {
		t.Errorf("model text must not be forwarded: %s", body)
	}
	st := s.quotaPauseSnapshot()
	if !st.Active || st.Reason != "limit_banner" || st.LimitBanner != banner {
		t.Fatalf("unexpected pause state: %+v", st)
	}
	l := logs.String()
	for _, want := range []string{"limit_banner_detected", "quota_fallback_pause", "Limit reached"} {
		if !strings.Contains(l, want) {
			t.Errorf("proxy.log missing %q", want)
		}
	}
	types := k41DiagTypes(s)
	if r, ok := types["limit_banner"]; !ok || !strings.Contains(r, "Limit reached") {
		t.Errorf("diag limit_banner incident missing/unreadable: %v", types)
	}
	if r, ok := types["quota_fallback_pause"]; !ok || !strings.Contains(r, "Limit reached") {
		t.Errorf("diag quota_fallback_pause incident missing: %v", types)
	}
}

// 3. Auto / Flash stay usable while the premium pool is paused.
func TestK41_AutoAndFlashPassDuringPause(t *testing.T) {
	s, ts, calls, _ := k41Server(t, k41Frame("ok", k41AutoID, "3.5 Flash-Lite", ""))
	s.triggerQuotaPause(context.Background(), &gemini.AvailableModel{Name: "gemini-everyday-help", ModelID: k41HydroID}, k41AutoID, "3.5 Flash-Lite", "other_model", "")
	if !s.quotaPauseSnapshot().Active {
		t.Fatal("pause not active")
	}
	for _, body := range []string{k41ChatAuto, k41ChatFlash} {
		resp, out := k41Post(t, ts.URL, "/v1/chat/completions", body, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls=%d want 2", calls.Load())
	}
	if !s.quotaPauseSnapshot().Active || s.quotaPauseSnapshot().BlockedCount != 0 {
		t.Errorf("Auto/Flash must neither trigger nor count: %+v", s.quotaPauseSnapshot())
	}
}

// 4. Resume via the UI endpoint and the authenticated /_models/resume endpoint.
func TestK41_ResumeEndpoints(t *testing.T) {
	for _, route := range []string{"ui", "models"} {
		t.Run(route, func(t *testing.T) {
			s, ts, calls, logs := k41Server(t, k41Frame("ok", k41NeonID, "4 Neon", ""))
			s.triggerQuotaPause(context.Background(), &gemini.AvailableModel{Name: "gemini-everyday-help", ModelID: k41HydroID}, "", "", "limit_banner", "Limit reached")
			if resp, _ := k41Post(t, ts.URL, "/v1/chat/completions", k41ChatNeon, nil); resp.StatusCode != 429 {
				t.Fatalf("paused neon status=%d", resp.StatusCode)
			}

			path, hdr := "/_models/resume", map[string]string(nil)
			if route == "ui" {
				path, hdr = "/_ui/api/quota/resume", map[string]string{"X-GMINI-UI": s.uiToken}
				if resp, _ := k41Post(t, ts.URL, path, "", nil); resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("missing UI token got %d", resp.StatusCode)
				}
			}
			resp, body := k41Post(t, ts.URL, path, "", hdr)
			if resp.StatusCode != 200 {
				t.Fatalf("resume status=%d body=%s", resp.StatusCode, body)
			}
			var out struct {
				Resumed    bool            `json:"resumed"`
				QuotaPause QuotaPauseState `json:"quota_pause"`
			}
			if err := json.Unmarshal([]byte(body), &out); err != nil || !out.Resumed || out.QuotaPause.Active {
				t.Fatalf("bad resume payload: %s", body)
			}
			if s.quotaPauseSnapshot().Active {
				t.Fatal("pause still active")
			}
			if resp, b := k41Post(t, ts.URL, "/v1/chat/completions", k41ChatNeon, nil); resp.StatusCode != 200 {
				t.Fatalf("neon after resume status=%d body=%s", resp.StatusCode, b)
			}
			if calls.Load() != 1 {
				t.Errorf("upstream calls=%d want 1", calls.Load())
			}
			if !strings.Contains(logs.String(), "quota_pause_resumed") || k41DiagTypes(s)["quota_pause_resumed"] == "" {
				t.Errorf("resume not logged/recorded")
			}
		})
	}

	t.Run("models route needs auth", func(t *testing.T) {
		s, ts, _, _ := k41Server(t, k41Frame("ok", k41NeonID, "4 Neon", ""))
		s.cfg.APIKeys = []string{"k41-test-key"}
		if resp, _ := k41Post(t, ts.URL, "/_models/resume", "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated resume got %d", resp.StatusCode)
		}
	})
}

// 5. pause_on_fallback toggle through guardrails/set, status exposure and .env.
func TestK41_PauseOnFallbackToggle(t *testing.T) {
	s, ts, _, _ := k41Server(t, k41Frame("ok", k41AutoID, "3.5 Flash-Lite", ""))
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("GEMINI_NO_CONTENT_SEC=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg.EnvFilePath = envPath
	hdr := map[string]string{"X-GMINI-UI": s.uiToken}

	resp, body := k41Post(t, ts.URL, "/_ui/api/guardrails/set", `{"pause_on_fallback":false}`, hdr)
	if resp.StatusCode != 200 || !strings.Contains(body, `"pause_on_fallback":false`) {
		t.Fatalf("set false: %d %s", resp.StatusCode, body)
	}
	if s.pauseOnFallbackEnabled() {
		t.Fatal("config not updated")
	}
	if data, _ := os.ReadFile(envPath); !strings.Contains(string(data), "GEMINI_PAUSE_ON_FALLBACK=false") {
		t.Errorf(".env not updated: %q", data)
	}
	if info, _ := os.Stat(envPath); info.Mode().Perm() != 0o600 {
		t.Errorf(".env perms=%v", info.Mode().Perm())
	}

	// Disabled: the fallback is served as before, nothing is paused or blocked.
	if resp, b := k41Post(t, ts.URL, "/v1/chat/completions", k41ChatHydro, nil); resp.StatusCode != 200 {
		t.Fatalf("disabled pause should not block: %d %s", resp.StatusCode, b)
	}
	if s.quotaPauseSnapshot().Active {
		t.Fatal("pause must not trigger when disabled")
	}

	// /_status and /_ui/api/state expose the flag and the state.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/_status", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(b), `"pause_on_fallback":false`) || !strings.Contains(string(b), `"quota_pause":{"active":false`) {
		t.Errorf("/_status lacks pause fields: %s", b)
	}
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/_ui/api/state", nil)
	req.Header.Set("X-GMINI-UI", s.uiToken)
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r.Body)
	r.Body.Close()
	var state struct {
		PauseOnFallback bool            `json:"pause_on_fallback"`
		QuotaPause      QuotaPauseState `json:"quota_pause"`
	}
	if err := json.Unmarshal(b, &state); err != nil || state.PauseOnFallback {
		t.Errorf("ui state: err=%v pause=%v", err, state.PauseOnFallback)
	}

	// Hot reload picks the value up from .env as well.
	if err := os.WriteFile(envPath, []byte("GEMINI_PAUSE_ON_FALLBACK=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	if _, err := reloadNonSecretDotenv(envPath, cfg); err != nil || !cfg.PauseOnFallback {
		t.Errorf("hot reload: err=%v value=%v", err, cfg.PauseOnFallback)
	}

	resp, body = k41Post(t, ts.URL, "/_ui/api/guardrails/set", `{"pause_on_fallback":true}`, hdr)
	if resp.StatusCode != 200 || !s.pauseOnFallbackEnabled() {
		t.Fatalf("set true: %d %s", resp.StatusCode, body)
	}
	if resp, _ := k41Post(t, ts.URL, "/v1/chat/completions", k41ChatHydro, nil); resp.StatusCode != 429 {
		t.Errorf("enabled pause should trigger 429, got %d", resp.StatusCode)
	}
}

// Blocking (non-stream) requests are covered by the post-generation safety net.
func TestK41_BlockingRequestSafetyNet(t *testing.T) {
	s, ts, _, _ := k41Server(t, k41Frame("hi", k41AutoID, "3.5 Flash-Lite", ""))
	resp, body := k41Post(t, ts.URL, "/v1/chat/completions", `{"model":"gemini-everyday-help","stream":false,"messages":[{"role":"user","content":"x"}]}`, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !s.quotaPauseSnapshot().Active {
		t.Fatal("pause not triggered by blocking request")
	}
}

// 6. The embedded script is the last-resort fallback: no cwd/exe copy needed.
func TestK41_ResolveDiagnosticsScriptEmbedFallback(t *testing.T) {
	t.Setenv("GEMINI_DIAGNOSTICS_SCRIPT", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if err := os.Chdir(empty); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if exe, err := os.Executable(); err == nil {
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "tools", "export_gmini_diagnostics.py")); err == nil {
			t.Skip("test binary directory already contains the script")
		}
	}

	path, err := resolveDiagnosticsScript()
	if err != nil {
		t.Fatalf("embed fallback failed: %v", err)
	}
	if strings.HasPrefix(path, empty) {
		t.Fatalf("unexpected cwd candidate %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, embeddedDiagnosticsScript) || len(data) == 0 {
		t.Fatalf("materialized script differs: err=%v len=%d", err, len(data))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("script perms=%v want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o700 {
		t.Errorf("dir perms=%v want 0700", info.Mode().Perm())
	}
	// Second call reuses the cached file.
	if again, err := resolveDiagnosticsScript(); err != nil || again != path {
		t.Errorf("cache not reused: %q vs %q (%v)", again, path, err)
	}
	// Removed: it is rewritten.
	_ = os.RemoveAll(filepath.Dir(path))
	if p3, err := resolveDiagnosticsScript(); err != nil {
		t.Errorf("rewrite after removal failed: %v", err)
	} else {
		_ = os.RemoveAll(filepath.Dir(p3))
	}
}
