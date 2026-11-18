package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func seedCookieAndState(t *testing.T, dir string, psidtsAge time.Duration) (cookieFile, stateFile string) {
	t.Helper()
	cookieFile = filepath.Join(dir, "cookie.txt")
	stateFile = cookieFile + ".state.json"
	if err := os.WriteFile(cookieFile, []byte("SAPISID=s1; __Secure-1PSIDTS=t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("s1"))
	fp := hex.EncodeToString(sum[:])[:16]
	now := time.Now().UTC()
	psidtsAt := now.Add(-psidtsAge).Format(time.RFC3339)
	freshAt := now.Add(-5 * time.Minute).Format(time.RFC3339)
	doc, _ := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            freshAt,
		"session_fingerprint": fp,
		"at":                  "AT_OK",
		"at_at":               freshAt,
		"bl":                  "boq_old",
		"bl_at":               freshAt,
		"psidts_at":           psidtsAt,
		"models": []map[string]any{
			{"Name": "gemini-3.5-flash", "ModelID": "id_flash", "Description": "3.5 Flash"},
		},
		"models_at":     freshAt,
		"models_source": "dynamic",
	})
	if err := os.WriteFile(stateFile, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	return cookieFile, stateFile
}

func TestK16_A4_StartupDisabledSixHoursWithAndWithoutRequests(t *testing.T) {
	dir := t.TempDir()
	cookieFile, stateFile := seedCookieAndState(t, dir, 3*time.Hour)
	markerFile := filepath.Join(dir, "chrome_launched")
	script := writeFakeScript(t, fmt.Sprintf("touch %q\n", markerFile))

	t.Setenv("GEMINI_COOKIE_AUTO_REFRESH_MIN", "90")
	t.Setenv("GEMINI_COOKIE_REFRESH_CMD", script)
	t.Setenv("GEMINI_COOKIE_FILE", cookieFile)
	t.Setenv("GEMINI_STATE_FILE", stateFile)
	t.Setenv("GEMINI_UI", "1")
	t.Setenv("GEMINI_ALLOW_NO_AUTH", "1")

	cfg := loadConfig(filepath.Join(dir, "missing.env"))
	if cfg.CookieAutoRefreshMin != 90 {
		t.Fatalf("CookieAutoRefreshMin = %d, want 90", cfg.CookieAutoRefreshMin)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	var googleCalls int
	client, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		t.Fatal(err)
	}
	client.SetTransport(roundTripFn(func(r *http.Request) (*http.Response, error) {
		googleCalls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	client.RestoreState()

	s := newServer(cfg, client, logger)
	s.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("OK")
		return nil
	}
	now := time.Now().Round(0)
	s.autoRefresh.nowFn = func() time.Time { return now }
	s.autoRefresh.startedAt = now

	if st := s.autoRefresh.Status(); st.Enabled || !st.Configured || st.IntervalMin != 90 {
		t.Fatalf("startup status = %+v, want Enabled=false Configured=true IntervalMin=90", st)
	}

	// 1. Advance clock 6 hours WITHOUT requests: 0 launches, 0 Google calls.
	for i := 0; i < 12; i++ {
		now = now.Add(30 * time.Minute)
		s.autoRefresh.Tick()
	}

	// 2. Advance clock another 6 hours WITH requests (PrepareRequest, TriggerEarly, HTTP chat):
	//    still 0 launches, 0 Google calls, 0 background_call logs.
	for i := 0; i < 12; i++ {
		now = now.Add(30 * time.Minute)
		waited := false
		s.autoRefresh.PrepareRequest(context.Background(), func() { waited = true })
		if waited {
			t.Fatal("disabled auto-refresh must never delay requests")
		}
		s.autoRefresh.Tick()
		s.autoRefresh.TriggerEarly()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
			"model":"gemini-3.5-flash","stream":true,
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
		}`))
		req.Host = "127.0.0.1:8081"
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat status = %d", rec.Code)
		}
	}
	time.Sleep(40 * time.Millisecond)

	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatal("auto-refresh launched process while disabled at startup")
	}
	if googleCalls != 0 {
		t.Fatalf("expected 0 Google calls, got %d", googleCalls)
	}
	if strings.Contains(logBuf.String(), "background_call") || strings.Contains(logBuf.String(), "cookie_auto_refresh_toggle") {
		t.Fatalf("unexpected background/toggle logs while untouched:\n%s", logBuf.String())
	}

	// 3. Toggle ON via UI route -> logs source=ui, enables only in memory.
	h := s.handler()
	wOn := uiDo(h, http.MethodPost, "/_ui/api/cookie-refresh/toggle", "127.0.0.1:5000", s.uiToken)
	if wOn.Code != http.StatusOK {
		t.Fatalf("toggle on = %d %s", wOn.Code, wOn.Body.String())
	}
	if !s.autoRefresh.Status().Enabled {
		t.Fatal("expected enabled=true after UI toggle")
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "cookie_auto_refresh_toggle") || !strings.Contains(logs, "enabled=true") || !strings.Contains(logs, "source=ui") {
		t.Fatalf("expected UI toggle log with source=ui, got:\n%s", logs)
	}

	// 4. Simulated restart (new client + new server on the same state/cookie files):
	//    K18 T4 persists the user's explicit UI toggle across restarts; toggling off
	//    persists disabled as well.
	client2, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		t.Fatal(err)
	}
	client2.RestoreState()
	s2 := newServer(cfg, client2, logger)
	if st2 := s2.autoRefresh.Status(); !st2.Enabled {
		t.Fatalf("after simulated restart, persisted auto-refresh must remain enabled, got %+v", st2)
	}
	wOff := uiDo(s2.handler(), http.MethodPost, "/_ui/api/cookie-refresh/toggle", "127.0.0.1:12345", s2.uiToken)
	if wOff.Code != http.StatusOK {
		t.Fatalf("toggle off = %d %s", wOff.Code, wOff.Body.String())
	}
	s2.autoRefresh.nowFn = func() time.Time { return now.Add(6 * time.Hour) }
	s2.autoRefresh.PrepareRequest(context.Background(), nil)
	s2.autoRefresh.Tick()
	s2.autoRefresh.TriggerEarly()
	time.Sleep(40 * time.Millisecond)
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatal("disabled server launched process unexpectedly")
	}

	// 5. Manual refresh still works when auto-refresh is disabled and leaves it disabled.
	wMan := uiDo(s2.handler(), http.MethodPost, "/_ui/api/cookie-refresh/run", "127.0.0.1:5000", s2.uiToken)
	if wMan.Code != http.StatusOK {
		t.Fatalf("manual run status = %d %s", wMan.Code, wMan.Body.String())
	}
	waitForCondition(t, 2*time.Second, func() bool { return !s2.autoRefresh.Status().Running })
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("manual refresh did not run script: %v", err)
	}
	if s2.autoRefresh.Status().Enabled {
		t.Fatal("manual refresh must leave auto-refresh disabled")
	}
}

func TestK16_A3_StaleTokenAlertInStreamInFlightAndUI(t *testing.T) {
	dir := t.TempDir()
	cookieFile, stateFile := seedCookieAndState(t, dir, 135*time.Minute)
	markerFile := filepath.Join(dir, "must_not_run")
	script := writeFakeScript(t, fmt.Sprintf("touch %q\n", markerFile))

	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CookieAutoRefreshMin = 90
	cfg.CookieRefreshCmd = script
	cfg.Gemini.CookieFile = cookieFile
	cfg.Gemini.StateFile = stateFile
	cfg.Gemini.StateMaxAge = 24 * time.Hour

	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.RestoreState()

	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	now := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	s.warnNow = func() time.Time { return now }

	started := make(chan struct{}, 4)
	release := make(chan struct{})
	s.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		h.Text("Réponse normale.")
		return nil
	}
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// 1. First request with 135m token: check in-flight alert AND SSE stream note.
	var sse1 *sseCapture
	done1 := make(chan struct{})
	go func() {
		sse1 = readSSE(t, context.Background(), ts.URL, `{
			"model":"gemini-3.5-flash","stream":true,
			"messages":[{"role":"user","content":"bonjour"}],
			"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
		}`)
		close(done1)
	}()

	<-started
	var inFlightAlert string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		wSt := uiDo(s.handler(), http.MethodGet, "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
		var payload struct {
			InFlight []InFlightRequest `json:"in_flight"`
		}
		if json.Unmarshal(wSt.Body.Bytes(), &payload) == nil && len(payload.InFlight) > 0 && payload.InFlight[0].StaleTokenAlert != "" {
			inFlightAlert = payload.InFlight[0].StaleTokenAlert
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	close(release)
	<-done1

	if !strings.Contains(inFlightAlert, "GMINI : jeton de ") || !strings.Contains(inFlightAlert, "risque de repli sur un autre modèle ; actualisez le cookie depuis GMINI Control") {
		t.Fatalf("in-flight stale_token_alert = %q", inFlightAlert)
	}
	joined1 := strings.Join(sse1.dataFrames, "\n")
	if !strings.Contains(joined1, "GMINI : jeton de ") || !strings.Contains(joined1, "risque de repli sur un autre modèle ; actualisez le cookie depuis GMINI Control") {
		t.Fatalf("expected stale token note in SSE stream, got:\n%s", joined1)
	}

	// 2. Second request 10 min later (within 30 min cooldown): NO note in SSE stream.
	now = now.Add(10 * time.Minute)
	sse2 := readSSE(t, context.Background(), ts.URL, `{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"encore"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`)
	joined2 := strings.Join(sse2.dataFrames, "\n")
	if strings.Contains(joined2, "risque de repli sur un autre modèle") {
		t.Fatalf("expected rate-limited suppression within 30m, got:\n%s", joined2)
	}

	// 3. Third request 31 min after first (>30m): note appears again.
	now = now.Add(21 * time.Minute)
	sse3 := readSSE(t, context.Background(), ts.URL, `{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"encore 2"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`)
	joined3 := strings.Join(sse3.dataFrames, "\n")
	if !strings.Contains(joined3, "risque de repli sur un autre modèle") {
		t.Fatalf("expected stale token note again after 30m, got:\n%s", joined3)
	}

	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatal("stale token alert must never launch external refresh command")
	}

	// 4. Verify UI HTML includes >90m orange, >120m red, and stale_token_alert rendering.
	wHTML := uiDo(s.handler(), http.MethodGet, "/_ui", "127.0.0.1:5000", "")
	html := wHTML.Body.String()
	for _, want := range []string{"7200", "5400", "var(--bad)", "var(--warn)", "pensez à actualiser", "stale_token_alert"} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK16_B2_UpstreamEmptyReplyNoRetryAndMetrics(t *testing.T) {
	var upstreamCalls int
	empty186 := ")]}'\n\n177\n[[\"wrb.fr\",null,\"[null,[\\\"c_1\\\",\\\"r_1\\\"],{\\\"18\\\":\\\"r_1\\\",\\\"21\\\":[\\\"tok\\\"],\\\"44\\\":true}]\"]]\n"
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(empty186)),
			Request:    r,
		}, nil
	}))
	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 50 * time.Millisecond
	s.generate = func(context.Context, gemini.GenParams) (string, error) {
		t.Fatal("B2 must not fall back or retry via Generate on empty stream close")
		return "", nil
	}
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	bigPrompt := strings.Repeat("a", 10*1024)
	reqBody := fmt.Sprintf(`{
		"model":"gemini-3.1-pro","stream":true,
		"messages":[{"role":"user","content":%q}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`, bigPrompt)

	// 1st empty reply
	sse1 := readSSE(t, context.Background(), ts.URL, reqBody)
	if upstreamCalls != 1 {
		t.Fatalf("expected exactly 1 upstream call (no retry), got %d", upstreamCalls)
	}
	joined := strings.Join(sse1.dataFrames, "\n")
	if !strings.Contains(joined, "GMINI : Google a renvoyé une réponse vide sur 3.1 Pro (prompt de ") ||
		!strings.Contains(joined, "Ko). Réessayez, raccourcissez la conversation ou changez de modèle.") {
		t.Fatalf("unexpected client message on empty reply:\n%s", joined)
	}
	if !strings.Contains(logBuf.String(), "upstream_empty_reply") || !strings.Contains(logBuf.String(), "prompt_bytes=") {
		t.Fatalf("expected upstream_empty_reply log with prompt_bytes, got:\n%s", logBuf.String())
	}

	// 2nd empty reply -> N3 failure memory marks model unresponsive (2 failures)
	_ = readSSE(t, context.Background(), ts.URL, reqBody)
	if upstreamCalls != 2 {
		t.Fatalf("expected exactly 2 upstream calls total (no retry), got %d", upstreamCalls)
	}

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/_status code = %d", rr.Code)
	}
	var st struct {
		Counters      map[string]int64     `json:"counters"`
		ModelFailures []modelFailureStatus `json:"model_failures"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Counters["upstream_empty_reply"] != 2 {
		t.Fatalf("upstream_empty_reply counter = %d, want 2", st.Counters["upstream_empty_reply"])
	}
	if len(st.ModelFailures) != 1 || st.ModelFailures[0].Model != "gemini-3.1-pro" ||
		st.ModelFailures[0].ConsecutiveFailures != 2 || !st.ModelFailures[0].Unresponsive {
		t.Fatalf("unexpected model_failures = %+v", st.ModelFailures)
	}

	wHTML := uiDo(s.handler(), http.MethodGet, "/_ui", "127.0.0.1:5000", "")
	if !strings.Contains(wHTML.Body.String(), "Réponses vides Google") {
		t.Error("ui.html missing Réponses vides Google tile")
	}

	// Stream with thoughts only (no text) must NOT trigger upstream_empty_reply.
	_, clientThought := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		body := fakeStreamServed("", "Pensée seule.", "e6fa609c3fa255c0", "3.1 Pro")
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	sThought := newServer(cfg, clientThought, testLogger())
	sThought.keepaliveOverride = 50 * time.Millisecond
	tsThought := httptest.NewServer(sThought.handler())
	defer tsThought.Close()
	sseThought := readSSE(t, context.Background(), tsThought.URL, reqBody)
	if strings.Contains(strings.Join(sseThought.dataFrames, "\n"), "Google a renvoyé une réponse vide") {
		t.Fatalf("stream with candidate/thought must not be classified as upstream_empty_reply")
	}
}

func TestK16_B3_PromptWarnKBConfigStatusAndUI(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "missing.env")
	t.Setenv("GEMINI_PROMPT_WARN_KB", "")
	cfg := loadConfig(noEnv)
	if cfg.PromptWarnKB != 300 {
		t.Fatalf("default PromptWarnKB = %d, want 300", cfg.PromptWarnKB)
	}
	sDef := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	sDef.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var stDef struct {
		PromptWarnKB int `json:"prompt_warn_kb"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &stDef); err != nil || stDef.PromptWarnKB != 300 {
		t.Fatalf("/_status default prompt_warn_kb = %d (err=%v), want 300", stDef.PromptWarnKB, err)
	}

	t.Setenv("GEMINI_PROMPT_WARN_KB", "150")
	cfgCustom := loadConfig(noEnv)
	cfgCustom.UIEnabled = true
	sCustom := newTestServer(t, cfgCustom)
	rr2 := httptest.NewRecorder()
	sCustom.handler().ServeHTTP(rr2, newReq(http.MethodGet, "/_status", nil))
	var stCustom struct {
		PromptWarnKB int `json:"prompt_warn_kb"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &stCustom); err != nil || stCustom.PromptWarnKB != 150 {
		t.Fatalf("/_status custom prompt_warn_kb = %d (err=%v), want 150", stCustom.PromptWarnKB, err)
	}

	t.Setenv("GEMINI_PROMPT_WARN_KB", "0")
	cfgOff := loadConfig(noEnv)
	sOff := newTestServer(t, cfgOff)
	rr3 := httptest.NewRecorder()
	sOff.handler().ServeHTTP(rr3, newReq(http.MethodGet, "/_status", nil))
	var stOff struct {
		PromptWarnKB int `json:"prompt_warn_kb"`
	}
	if err := json.Unmarshal(rr3.Body.Bytes(), &stOff); err != nil || stOff.PromptWarnKB != 0 {
		t.Fatalf("/_status disabled prompt_warn_kb = %d (err=%v), want 0", stOff.PromptWarnKB, err)
	}

	wHTML := uiDo(sCustom.handler(), http.MethodGet, "/_ui", "127.0.0.1:5000", "")
	html := wHTML.Body.String()
	for _, want := range []string{"koKB", "prompt_warn_kb", "fmtPrompt(c.prompt_bytes)"} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}
