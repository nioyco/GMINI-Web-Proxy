package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK24_1_StatusAndUIExposeNoContentSecAndGuardrailsUI(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.CannedErrorRetry = "off"
	cfg.NoContentSec = 0
	s := newTestServer(t, cfg)
	h := s.handler()

	stRec := uiDo(h, http.MethodGet, "/_status", "127.0.0.1:5000", "")
	if stRec.Code != http.StatusOK {
		t.Fatalf("/_status status=%d body=%s", stRec.Code, stRec.Body.String())
	}
	if !strings.Contains(stRec.Body.String(), `"no_content_sec":0`) || !strings.Contains(stRec.Body.String(), `"canned_error_retry":"off"`) {
		t.Fatalf("expected no_content_sec=0 and canned_error_retry=off in /_status, got: %s", stRec.Body.String())
	}

	uiStateRec := uiDo(h, http.MethodGet, "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	if uiStateRec.Code != http.StatusOK {
		t.Fatalf("/_ui/api/state status=%d body=%s", uiStateRec.Code, uiStateRec.Body.String())
	}
	var state struct {
		Status struct {
			NoContentSec     int    `json:"no_content_sec"`
			CannedErrorRetry string `json:"canned_error_retry"`
		} `json:"status"`
	}
	if err := json.Unmarshal(uiStateRec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status.NoContentSec != 0 || state.Status.CannedErrorRetry != "off" {
		t.Fatalf("unexpected /_ui/api/state status: %+v", state.Status)
	}

	htmlRec := uiDo(h, http.MethodGet, "/_ui", "127.0.0.1:5000", "")
	html := htmlRec.Body.String()
	for _, want := range []string{`id="nc-toggle"`, `id="cer-toggle"`, `/_ui/api/guardrails/set`} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK24_1_GuardrailsSetAuthAndAtomicDotenvPersistence(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	initialEnv := strings.Join([]string{
		"# Sample comment",
		"# GEMINI_NO_CONTENT_SEC=999",
		"GEMINI_COOKIE=FAKE_SECRET_COOKIE_K24",
		"GEMINI_API_KEYS=FAKE_KEY_K24",
		"GEMINI_CANNED_ERROR_RETRY=off",
		"",
	}, "\n")
	if err := os.WriteFile(envPath, []byte(initialEnv), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf syncBuffer
	t.Setenv("GEMINI_COOKIE", "FAKE_SECRET_COOKIE_K24")
	t.Setenv("GEMINI_API_KEYS", "FAKE_KEY_K24")
	t.Setenv("GEMINI_CANNED_ERROR_RETRY", "off")
	t.Setenv("GEMINI_NO_CONTENT_SEC", "0")
	cfg := loadConfig(envPath)
	cfg.UIEnabled = true
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels(testModels())
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	h := s.handler()

	// Auth and loopback checks.
	if w := uiDo(h, http.MethodPost, "/_ui/api/guardrails/set", "127.0.0.1:5000", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", w.Code)
	}
	if w := uiDo(h, http.MethodPost, "/_ui/api/guardrails/set", "10.0.0.9:5000", s.uiToken); w.Code != http.StatusForbidden {
		t.Fatalf("remote IP status = %d, want 403", w.Code)
	}

	// Enable canned_error_retry=once and no_content_sec=240 via UI route.
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/_ui/api/guardrails/set",
		strings.NewReader(`{"canned_error_retry":"once","no_content_sec":240}`))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-GMINI-UI", s.uiToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("guardrails set status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		OK               bool   `json:"ok"`
		CannedErrorRetry string `json:"canned_error_retry"`
		NoContentSec     int    `json:"no_content_sec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.CannedErrorRetry != "once" || resp.NoContentSec != 240 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	// Secrets must never appear in HTTP responses or logs.
	combined := rec.Body.String() + "\n" + logBuf.String()
	for _, secret := range []string{"FAKE_SECRET_COOKIE_K24", "FAKE_KEY_K24", envPath} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret or path leaked: %q in %s", secret, combined)
		}
	}

	// Verify in-memory config and /_status match immediately and env is not flagged dirty.
	if s.cannedErrorRetryMode() != "once" || s.noContentSecSetting() != 240 {
		t.Fatalf("in-memory config not updated: retry=%q nc=%d", s.cannedErrorRetryMode(), s.noContentSecSetting())
	}
	stReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8081/_status", nil)
	stReq.RemoteAddr = "127.0.0.1:5000"
	stReq.Header.Set("Authorization", "Bearer FAKE_KEY_K24")
	stRec := httptest.NewRecorder()
	h.ServeHTTP(stRec, stReq)
	if stRec.Code != http.StatusOK {
		t.Fatalf("/_status code=%d body=%s", stRec.Code, stRec.Body.String())
	}
	var st statusResponse
	if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.CannedErrorRetry != "once" || st.NoContentSec != 240 || st.EnvModifiedSinceStart {
		t.Fatalf("unexpected /_status after guardrails/set: retry=%q nc=%d modified=%v",
			st.CannedErrorRetry, st.NoContentSec, st.EnvModifiedSinceStart)
	}

	// Verify atomic .env write preserved mode 0600, comments, and secret keys verbatim.
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env permissions = %#o, want 0600", info.Mode().Perm())
	}
	diskBytes, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	diskText := string(diskBytes)
	for _, want := range []string{
		"# GEMINI_NO_CONTENT_SEC=999",
		"GEMINI_COOKIE=FAKE_SECRET_COOKIE_K24",
		"GEMINI_API_KEYS=FAKE_KEY_K24",
		"GEMINI_CANNED_ERROR_RETRY=once",
		"GEMINI_NO_CONTENT_SEC=240",
	} {
		if !strings.Contains(diskText, want) {
			t.Fatalf(".env missing %q; content:\n%s", want, diskText)
		}
	}

	// Normalization: negative no_content_sec clamps to 0, bogus retry normalizes to off.
	req2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/_ui/api/guardrails/set",
		strings.NewReader(`{"canned_error_retry":"bogus","no_content_sec":-5}`))
	req2.RemoteAddr = "127.0.0.1:5000"
	req2.Header.Set("X-GMINI-UI", s.uiToken)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("guardrails normalize status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	if s.cannedErrorRetryMode() != "off" || s.noContentSecSetting() != 0 {
		t.Fatalf("expected off/0 after normalization, got %q/%d", s.cannedErrorRetryMode(), s.noContentSecSetting())
	}

	// Allowlist rejects secret keys.
	if _, _, err := upsertNonSecretDotenvKey(envPath, "GEMINI_COOKIE", "hijack"); err == nil {
		t.Fatal("expected upsertNonSecretDotenvKey to reject non-allowlisted key")
	}

	// Unwritable EnvFilePath returns 500 without mutating in-memory config.
	s.cfgMu.Lock()
	s.cfg.EnvFilePath = filepath.Join(dir, "missing_subdir", ".env")
	s.cfgMu.Unlock()
	reqErr := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/_ui/api/guardrails/set",
		strings.NewReader(`{"canned_error_retry":"once","no_content_sec":240}`))
	reqErr.RemoteAddr = "127.0.0.1:5000"
	reqErr.Header.Set("X-GMINI-UI", s.uiToken)
	reqErr.Header.Set("Content-Type", "application/json")
	recErr := httptest.NewRecorder()
	h.ServeHTTP(recErr, reqErr)
	if recErr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on unwritable .env, got %d body=%s", recErr.Code, recErr.Body.String())
	}
	if s.cannedErrorRetryMode() != "off" || s.noContentSecSetting() != 0 {
		t.Fatalf("in-memory config mutated despite disk failure: %q/%d", s.cannedErrorRetryMode(), s.noContentSecSetting())
	}

	// Empty EnvFilePath succeeds in memory only.
	cfgNoEnv := testConfig()
	cfgNoEnv.UIEnabled = true
	sNoEnv := newTestServer(t, cfgNoEnv)
	reqMem := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8081/_ui/api/guardrails/set",
		strings.NewReader(`{"canned_error_retry":"once","no_content_sec":180}`))
	reqMem.RemoteAddr = "127.0.0.1:5000"
	reqMem.Header.Set("X-GMINI-UI", sNoEnv.uiToken)
	reqMem.Header.Set("Content-Type", "application/json")
	recMem := httptest.NewRecorder()
	sNoEnv.handler().ServeHTTP(recMem, reqMem)
	if recMem.Code != http.StatusOK || sNoEnv.cannedErrorRetryMode() != "once" || sNoEnv.noContentSecSetting() != 180 {
		t.Fatalf("in-memory-only update failed: code=%d retry=%q nc=%d", recMem.Code, sNoEnv.cannedErrorRetryMode(), sNoEnv.noContentSecSetting())
	}
}

func TestK24_1_ReloadNonSecretDotenvUpdatesGuardrails(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("GEMINI_CANNED_ERROR_RETRY=off\nGEMINI_NO_CONTENT_SEC=0\nGEMINI_COOKIE=FAKE_SECRET_RELOAD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_CANNED_ERROR_RETRY", "off")
	t.Setenv("GEMINI_NO_CONTENT_SEC", "0")
	t.Setenv("GEMINI_COOKIE", "FAKE_SECRET_RELOAD")
	t.Setenv("GEMINI_API_KEYS", "")
	cfg := loadConfig(envPath)
	cfg.UIEnabled = true
	s := newTestServer(t, cfg)
	h := s.handler()

	if err := os.WriteFile(envPath, []byte("GEMINI_CANNED_ERROR_RETRY=once\nGEMINI_NO_CONTENT_SEC=240\nGEMINI_HELD_STALL_FAST_SEC=18\nGEMINI_HELD_STALL_RETRIES=3\nGEMINI_COOKIE=FAKE_SECRET_RELOAD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfgMu.Lock()
	_, err := reloadNonSecretDotenv(envPath, cfg)
	s.cfgMu.Unlock()
	if err != nil {
		t.Fatalf("reloadNonSecretDotenv: %v", err)
	}
	if s.cannedErrorRetryMode() != "once" || s.noContentSecSetting() != 240 {
		t.Fatalf("after reloadNonSecretDotenv: retry=%q nc=%d", s.cannedErrorRetryMode(), s.noContentSecSetting())
	}

	stRec := uiDo(h, http.MethodGet, "/_status", "127.0.0.1:5000", "")
	if stRec.Code != http.StatusOK {
		t.Fatalf("/_status code=%d body=%s", stRec.Code, stRec.Body.String())
	}
	var st statusResponse
	if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.CannedErrorRetry != "once" || st.NoContentSec != 240 || st.HeldStallFastSec != 18 || st.HeldStallRetriesMax != 3 {
		t.Fatalf("expected once/240/18/3 in /_status after reload, got %q/%d/%d/%d", st.CannedErrorRetry, st.NoContentSec, st.HeldStallFastSec, st.HeldStallRetriesMax)
	}
}

func TestK24_2_CodeRefFastpathEmitsWaitMsBeforeAsync10000(t *testing.T) {
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			frame := fakeStreamServed("```bash?code_reference\ngit status -s\n```", "", "56fdd199312815e2", "4 Hydrogen")
			_, _ = pw.Write([]byte(frame))
			<-r.Context().Done()
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.CodeRefFastpath = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	body := `{
		"model":"gemini-everyday-help",
		"stream":true,
		"messages":[{"role":"user","content":"check git status"}],
		"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"},"WaitMsBeforeAsync":{"type":"integer"}},"required":["CommandLine","Cwd","WaitMsBeforeAsync"]}}}]
	}`
	c := readSSE(t, context.Background(), ts.URL, body)
	assertProgressStream(t, c)
	if !strings.Contains(c.raw, `"name":"run_command"`) || !strings.Contains(c.raw, `\"WaitMsBeforeAsync\":10000`) {
		t.Fatalf("expected code_ref_fastpath to emit WaitMsBeforeAsync:10000, got: %s", c.raw)
	}
}
