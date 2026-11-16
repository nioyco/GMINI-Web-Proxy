package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sseContentAndReasoning(t *testing.T, c *sseCapture) (content string, reasoning string) {
	t.Helper()
	var cb, rb strings.Builder
	for _, v := range views(t, c) {
		if len(v.Choices) == 0 {
			continue
		}
		if v.Choices[0].Delta.Content != nil {
			cb.WriteString(*v.Choices[0].Delta.Content)
		}
		if v.Choices[0].Delta.Reasoning != nil {
			rb.WriteString(*v.Choices[0].Delta.Reasoning)
		}
	}
	return cb.String(), rb.String()
}

func TestN2_ConfigDefaultsAndStatus(t *testing.T) {
	t.Setenv("GEMINI_CANNED_ERROR_RETRY", "")
	t.Setenv("GEMINI_CANNED_ERROR_RETRY_SEC", "")
	cfg := loadConfig(filepath.Join(t.TempDir(), "missing.env"))
	if cfg.CannedErrorRetry != "off" || cfg.CannedErrorRetrySec != 25 {
		t.Fatalf("defaults = (%q, %d), want (\"off\", 25)", cfg.CannedErrorRetry, cfg.CannedErrorRetrySec)
	}

	t.Setenv("GEMINI_CANNED_ERROR_RETRY", "once")
	t.Setenv("GEMINI_CANNED_ERROR_RETRY_SEC", "15")
	cfg = loadConfig(filepath.Join(t.TempDir(), "missing.env"))
	if cfg.CannedErrorRetry != "once" || cfg.CannedErrorRetrySec != 15 {
		t.Fatalf("configured = (%q, %d), want (\"once\", 15)", cfg.CannedErrorRetry, cfg.CannedErrorRetrySec)
	}

	s := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		Retry    string `json:"canned_error_retry"`
		RetrySec int    `json:"canned_error_retry_sec"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Retry != "once" || st.RetrySec != 15 {
		t.Fatalf("/_status = %+v, want once/15", st)
	}
}

func TestN2_DefaultOffNoRetryAndFrenchMessageInContent(t *testing.T) {
	phrases := []string{
		"I seem to be encountering an error. Can I try something else for you?",
		"I'm having a hard time fulfilling your request. Can I help you with something else instead?",
		"I encountered an error doing what you asked. Could you try again?",
		"Sorry, something went wrong. Please try your request again.",
	}
	for _, phrase := range phrases {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := fakeStreamServed(phrase, "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "off"
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		ts.Close()
		assertProgressStream(t, c)
		if calls.Load() != 1 {
			t.Fatalf("phrase %q: calls = %d, want 1", phrase, calls.Load())
		}
		content, reasoning := sseContentAndReasoning(t, c)
		want := "GMINI : Gemini a renvoyé une erreur interne sur 4 Hydrogen (pas une réponse du modèle). Réessayez, ou changez de modèle."
		if content != want {
			t.Fatalf("phrase %q: content = %q, want %q", phrase, content, want)
		}
		if reasoning != "" {
			t.Fatalf("phrase %q: unexpected reasoning = %q", phrase, reasoning)
		}
	}
}

func TestN2_OnceBoundedTimeoutAbortsStalledRetry(t *testing.T) {
	var calls atomic.Int32
	canceled := make(chan struct{}, 1)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			body := fakeStreamServed("I encountered an error doing what you asked. Could you try again?", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			<-r.Context().Done()
			select {
			case canceled <- struct{}{}:
			default:
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "once"
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.cannedRetryTimeoutOverride = 60 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	t0 := time.Now()
	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	elapsed := time.Since(t0)
	assertProgressStream(t, c)
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if elapsed > time.Second {
		t.Fatalf("retry did not time out quickly: %s", elapsed)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("expected stalled retry upstream request to be canceled")
	}
	content, _ := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "GMINI : Gemini a renvoyé une erreur interne sur 4 Hydrogen (pas une réponse du modèle).") {
		t.Fatalf("content = %q", content)
	}
}

func TestN2_OnceAllowsSlowRetryOnceThoughtArrives(t *testing.T) {
	var calls atomic.Int32
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			body := fakeStreamServed("I'm having a hard time fulfilling your request. Can I help you with something else instead?", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			time.Sleep(25 * time.Millisecond)
			_, _ = pw.Write([]byte(fakeStreamServed("", "Thinking...", "e6fa609c3fa255c0", "3.1 Pro")))
			time.Sleep(70 * time.Millisecond)
			_, _ = pw.Write([]byte(fakeStreamServed("Réponse finale après réflexion.", "Thinking...", "e6fa609c3fa255c0", "3.1 Pro")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "once"
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.cannedRetryTimeoutOverride = 60 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	content, reasoning := sseContentAndReasoning(t, c)
	if !strings.Contains(reasoning, "Thinking...") || !strings.Contains(content, "Réponse finale après réflexion.") {
		t.Fatalf("expected retry to finish once thoughts arrived: reasoning=%q content=%q", reasoning, content)
	}
}

func TestN3_ModelFailureMemorySkipsRetryAndResetsOnSuccess(t *testing.T) {
	var calls atomic.Int32
	var succeed atomic.Bool
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		txt := "I seem to be encountering an error. Can I try something else for you?"
		if succeed.Load() {
			txt = "Réponse valide du modèle."
		}
		body := fakeStreamServed(txt, "", "56fdd199312815e2", "4 Hydrogen")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "once"
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	now := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	s.warnNow = func() time.Time { return now }
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// Request 1 & 2: both fail (in once mode each triggers 2 calls -> 2 failures recorded).
	_ = readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	now = now.Add(2 * time.Minute)
	_ = readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	if calls.Load() != 4 {
		t.Fatalf("calls after 2 failed requests = %d, want 4", calls.Load())
	}

	// Request 3 within 15 min: must NOT be blocked, must NOT retry even in once mode (1 call only!).
	now = now.Add(2 * time.Minute)
	c3 := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c3)
	if calls.Load() != 5 {
		t.Fatalf("calls on 3rd request = %d, want 5 (retry must be skipped after 2 recent failures)", calls.Load())
	}

	// Verify /_status and /_ui/api/state expose unresponsive=true and count=3.
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Failures []modelFailureStatus `json:"model_failures"`
	}
	if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Failures) != 1 || !st.Failures[0].Unresponsive || st.Failures[0].ConsecutiveFailures != 3 {
		t.Fatalf("/_status model_failures = %+v", st.Failures)
	}
	uiRec := uiDo(s.handler(), "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	var uiSt struct {
		Models []uiModel `json:"models"`
	}
	if err := json.Unmarshal(uiRec.Body.Bytes(), &uiSt); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range uiSt.Models {
		if m.Name == "gemini-everyday-help" {
			found = m.Unresponsive && m.FailureCount == 3 && m.FailureSince != ""
		}
	}
	if !found {
		t.Fatalf("expected gemini-everyday-help unresponsive in /_ui/api/state: %+v", uiSt.Models)
	}

	// Success resets the counter to zero.
	succeed.Store(true)
	now = now.Add(time.Minute)
	c4 := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	content4, _ := sseContentAndReasoning(t, c4)
	if !strings.Contains(content4, "Réponse valide du modèle.") {
		t.Fatalf("expected request 4 to succeed (not blocked): %q", content4)
	}
	if len(s.modelFailures()) != 0 {
		t.Fatalf("expected modelFailures cleared after success, got %+v", s.modelFailures())
	}

	// Failures older than 15 minutes expire naturally.
	succeed.Store(false)
	_ = readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	now = now.Add(16 * time.Minute)
	if s.shouldSkipCannedRetry("gemini-everyday-help") || len(s.modelFailures()) != 0 {
		t.Fatalf("expected failure older than 15m to expire, got %+v", s.modelFailures())
	}
}

func TestN4_DefaultZeroEmitsWaitNoteAndCountsHeartbeats(t *testing.T) {
	t.Setenv("GEMINI_NO_CONTENT_SEC", "")
	cfgLoad := loadConfig(filepath.Join(t.TempDir(), "missing.env"))
	if cfgLoad.NoContentSec != 0 {
		t.Fatalf("default GEMINI_NO_CONTENT_SEC = %d, want 0", cfgLoad.NoContentSec)
	}

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			time.Sleep(35 * time.Millisecond)
			_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[1]}]\"]]\n"))
			time.Sleep(35 * time.Millisecond)
			_, _ = pw.Write([]byte(fakeStreamServed("Réponse après 2 battements.", "", "56fdd199312815e2", "4 Hydrogen")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.NoContentSec = 0
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 20 * time.Millisecond
	s.noContentNoteOverride = 30 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	content, reasoning := sseContentAndReasoning(t, c)
	if !strings.Contains(reasoning, "aucune pensée ni réponse") {
		t.Fatalf("expected wait note in reasoning, got %q", reasoning)
	}
	if !strings.Contains(content, "Réponse après 2 battements.") {
		t.Fatalf("expected final answer not cut when NoContentSec=0, got %q", content)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "upstream_heartbeats=2") {
		t.Fatalf("expected upstream_heartbeats=2 in request_end log, got:\n%s", logs)
	}
}

func TestN4_PositiveNoContentSecCancelsAndRecordsFailure(t *testing.T) {
	canceled := make(chan struct{}, 1)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			<-r.Context().Done()
			select {
			case canceled <- struct{}{}:
			default:
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.NoContentSec = 75
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.noContentTimeoutOverride = 60 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("expected upstream request to be canceled on N4 timeout")
	}
	content, _ := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "GMINI : 4 Hydrogen ne répond pas (Google n'envoie que des signaux d'attente depuis") {
		t.Fatalf("unexpected timeout content: %q", content)
	}
	if f := s.modelFailures(); len(f) != 1 || f[0].Count != 1 {
		t.Fatalf("expected N4 timeout to record model failure for N3, got %+v", f)
	}
}

func TestN4_Body2StatusPreventsNoContentCutoff(t *testing.T) {
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n"))
			time.Sleep(15 * time.Millisecond)
			b2 := make([]any, 5)
			b2[2] = map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Analyzing user requests"}}}
			_, _ = pw.Write([]byte(wrbBodyFrame(b2)))
			time.Sleep(80 * time.Millisecond)
			_, _ = pw.Write([]byte(fakeStreamServed("Réponse après statut.", "", "56fdd199312815e2", "4 Hydrogen")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.NoContentSec = 75
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.noContentTimeoutOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	content, _ := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "Réponse après statut.") {
		t.Fatalf("body[2][7] status should disarm N4 timeout, got %q", content)
	}
}

func TestN5_WaitAndRetryNotesExplicitlyTimestamped(t *testing.T) {
	var calls atomic.Int32
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			body := fakeStreamServed("I seem to be encountering an error. Can I try something else for you?", "", "56fdd199312815e2", "4 Hydrogen")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		body := fakeStreamServed("Réponse finale.", "", "56fdd199312815e2", "4 Hydrogen")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "once"
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.warnNow = func() time.Time { return time.Date(2026, 11, 16, 21, 5, 9, 0, time.UTC) }
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	_, reasoning := sseContentAndReasoning(t, c)
	if !strings.Contains(reasoning, "[21:05:09] GMINI (pas le modèle — attente technique) :") {
		t.Fatalf("expected timestamped GMINI (not model) note, got %q", reasoning)
	}
}
