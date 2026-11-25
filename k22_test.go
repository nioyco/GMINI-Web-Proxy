package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

// TestK22_1_WaitNotesEmittedWithNoContentSecPositive verifies that periodic
// wait notes (with heartbeat counters) still stream in reasoning_content even
// when GEMINI_NO_CONTENT_SEC > 0 before the no-content timeout fires.
func TestK22_1_WaitNotesEmittedWithNoContentSecPositive(t *testing.T) {
	canceled := make(chan struct{}, 1)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			time.Sleep(25 * time.Millisecond)
			_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[1]}]\"]]\n"))
			<-r.Context().Done()
			select {
			case canceled <- struct{}{}:
			default:
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	var logBuf syncBuffer
	cfg := testConfig()
	cfg.Gemini.LogParams = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.NoContentSec = 240
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 20 * time.Millisecond
	s.noContentTimeoutOverride = 150 * time.Millisecond
	s.noContentNoteOverride = 40 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("expected upstream request to be canceled by noContentTimeout")
	}

	content, reasoning := sseContentAndReasoning(t, c)
	if !strings.Contains(reasoning, "Google n'a encore envoyé aucune pensée ni réponse") {
		t.Fatalf("expected wait note in reasoning when NoContentSec > 0, got %q", reasoning)
	}
	if !strings.Contains(reasoning, "2 battement(s) reçu(s)") && !strings.Contains(reasoning, "1 battement(s) reçu(s)") {
		t.Fatalf("expected heartbeat counter in wait note, got %q", reasoning)
	}
	if !strings.Contains(content, "Google n'envoie que des signaux d'attente depuis") {
		t.Fatalf("expected no-content timeout message, got %q", content)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "upstream_no_content_timeout") || !strings.Contains(logs, "heartbeats=2") {
		t.Fatalf("expected upstream_no_content_timeout with heartbeats=2, got:\n%s", logs)
	}
	if !strings.Contains(logs, "upstream_heartbeats=2") {
		t.Fatalf("expected upstream_heartbeats=2 in request_end log, got:\n%s", logs)
	}
}

func extractIntLogField(t *testing.T, line, key string) int64 {
	t.Helper()
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `=(-?\d+)\b`)
	m := re.FindStringSubmatch(line)
	if len(m) != 2 {
		t.Fatalf("missing %s in log line: %q", key, line)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", key, m[1], err)
	}
	return v
}

func findLogLine(logs, needle string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// TestK22_2_ClientCancelDrainsDoneBeforeClientCancelledAndRequestEnd reproduces
// the 34b9da3707a2dd08 cancellation race: empty {"37":[N]} heartbeats followed
// by client ctx cancel while stream teardown sleeps 25 ms. served_model must
// log before client_cancelled and request_end, and both must report non-zero
// upstream_ms and upstream_resp_bytes.
func TestK22_2_ClientCancelDrainsDoneBeforeClientCancelledAndRequestEnd(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	cfg := testConfig()
	cfg.Gemini.LogParams = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen"},
	})

	hbWritten := make(chan struct{}, 1)
	client.SetTransport(rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			time.Sleep(10 * time.Millisecond)
			_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[1]}]\"]]\n"))
			select {
			case hbWritten <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			// Simulate stream teardown / body close taking 25 ms before streamOnce exits.
			time.Sleep(25 * time.Millisecond)
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	s := newServer(cfg, client, logger)
	s.keepaliveOverride = 20 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(k9ChatToolsBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("expected initial keepalive bytes: %v", err)
	}
	select {
	case <-hbWritten:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat frames")
	}
	// Let streamOnce finish reading the second heartbeat line before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancelReq()
	resp.Body.Close()

	waitForCondition(t, 2*time.Second, func() bool {
		logs := logBuf.String()
		return strings.Contains(logs, "request_end") &&
			strings.Contains(logs, "client_cancelled") &&
			strings.Contains(logs, "served_model")
	})

	logs := logBuf.String()
	idxServed := strings.Index(logs, "msg=served_model")
	idxClientCancelled := strings.Index(logs, "msg=client_cancelled")
	idxStreamToolsCancelled := strings.Index(logs, "msg=\"stream+tools cancelled by client\"")
	idxReqEnd := strings.Index(logs, "msg=request_end")
	if idxServed < 0 || idxClientCancelled < 0 || idxStreamToolsCancelled < 0 || idxReqEnd < 0 {
		t.Fatalf("missing expected log lines:\n%s", logs)
	}
	if !(idxServed < idxClientCancelled && idxClientCancelled < idxStreamToolsCancelled && idxStreamToolsCancelled < idxReqEnd) {
		t.Fatalf("unexpected log order served=%d client_cancelled=%d stream+tools=%d request_end=%d\n%s",
			idxServed, idxClientCancelled, idxStreamToolsCancelled, idxReqEnd, logs)
	}

	ccLine := findLogLine(logs, "msg=client_cancelled")
	reLine := findLogLine(logs, "msg=request_end")
	if ms := extractIntLogField(t, ccLine, "upstream_ms"); ms <= 0 {
		t.Fatalf("client_cancelled upstream_ms = %d, want > 0; line=%s", ms, ccLine)
	}
	if rb := extractIntLogField(t, ccLine, "upstream_resp_bytes"); rb <= 0 {
		t.Fatalf("client_cancelled upstream_resp_bytes = %d, want > 0; line=%s", rb, ccLine)
	}
	if ms := extractIntLogField(t, reLine, "upstream_ms"); ms <= 0 {
		t.Fatalf("request_end upstream_ms = %d, want > 0; line=%s", ms, reLine)
	}
	if rb := extractIntLogField(t, reLine, "upstream_resp_bytes"); rb <= 0 {
		t.Fatalf("request_end upstream_resp_bytes = %d, want > 0; line=%s", rb, reLine)
	}
	if hb := extractIntLogField(t, reLine, "upstream_heartbeats"); hb != 2 {
		t.Fatalf("request_end upstream_heartbeats = %d, want 2; line=%s", hb, reLine)
	}
}

// TestK22_3_QuietGateSuppressesRepeatedAutoRefreshBackgroundLogs verifies that
// Tick() logs background_call reason=cookie_auto_refresh_background gate=quiet
// at most once per quiet period (not every 60s), and re-arms after new user
// activity followed by a new quiet period (and runs once user activity is fresh).
func TestK22_3_QuietGateSuppressesRepeatedAutoRefreshBackgroundLogs(t *testing.T) {
	dir := t.TempDir()
	cookieFile := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(cookieFile, []byte("SAPISID=s1; __Secure-1PSIDTS=t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerFile := filepath.Join(dir, "ran_refresh")
	script := writeFakeScript(t, fmt.Sprintf("touch %q\n", markerFile))

	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	c, err := gemini.New(gemini.Config{
		GeminiBL:   "bl_old",
		CookieFile: cookieFile,
		StateFile:  cookieFile + ".state.json",
		IdleQuiet:  40 * time.Millisecond,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	ar := newCookieAutoRefresher(&Config{
		CookieAutoRefreshMin: 90,
		CookieRefreshCmd:     script,
	}, c, logger)
	on := true
	ar.Toggle(&on)
	now := time.Now().Round(0)
	ar.nowFn = func() time.Time { return now }
	ar.startedAt = now.Add(-4 * time.Hour)
	ar.lastActivityAt = now.Add(-20 * time.Minute)

	// 1. First quiet period: 10 consecutive ticks must emit gate=quiet only ONCE.
	for i := 0; i < 10; i++ {
		now = now.Add(time.Minute)
		ar.Tick()
	}
	if got := strings.Count(logBuf.String(), "gate=quiet"); got != 1 {
		t.Fatalf("expected 1 gate=quiet log across 10 idle ticks, got %d:\n%s", got, logBuf.String())
	}
	if got := c.BackgroundCallStatus().Refused24h; got != 1 {
		t.Fatalf("Refused24h = %d, want 1", got)
	}
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatal("refresh command must not run while quiet")
	}

	// 2. New user activity (RecordUserActivity) followed by entering a second quiet
	//    period: 10 more ticks must emit exactly 1 more gate=quiet log (total 2).
	c.RecordUserActivity()
	time.Sleep(60 * time.Millisecond) // > IdleQuiet (40ms)
	for i := 0; i < 10; i++ {
		now = now.Add(time.Minute)
		ar.Tick()
	}
	if got := strings.Count(logBuf.String(), "gate=quiet"); got != 2 {
		t.Fatalf("expected 2 total gate=quiet logs after re-arming via RecordUserActivity, got %d:\n%s", got, logBuf.String())
	}
	if got := c.BackgroundCallStatus().Refused24h; got != 2 {
		t.Fatalf("Refused24h = %d, want 2", got)
	}

	// 3. New activity via PrepareRequest (while token not yet due), followed by
	//    entering quiet again: re-arms Tick() for 1 more gate=quiet log (total 3).
	savedStarted := ar.startedAt
	ar.startedAt = now
	ar.PrepareRequest(context.Background(), nil)
	ar.startedAt = savedStarted
	time.Sleep(60 * time.Millisecond) // > IdleQuiet (40ms)
	for i := 0; i < 10; i++ {
		now = now.Add(time.Minute)
		ar.Tick()
	}
	if got := strings.Count(logBuf.String(), "gate=quiet"); got != 3 {
		t.Fatalf("expected 3 total gate=quiet logs after re-arming via PrepareRequest, got %d:\n%s", got, logBuf.String())
	}

	// 4. New activity while active re-arms Tick() and allows the refresh.
	c.RecordUserActivity()
	ar.Tick()
	waitForCondition(t, 2*time.Second, func() bool {
		st := ar.Status()
		return !st.Running && st.LastOK != nil && *st.LastOK
	})
	if _, err := os.Stat(markerFile); err != nil {
		t.Fatalf("expected refresh script to run once active again: %v", err)
	}
	if !strings.Contains(logBuf.String(), "reason=cookie_auto_refresh_background allowed=true") {
		t.Fatalf("expected allowed=true log after re-arm, got:\n%s", logBuf.String())
	}
}

// TestK22_4_CannedErrorRetryWithNoContentSecAndHeartbeats covers the interaction
// between CannedErrorRetry=once, cannedRetryTimeout, and NoContentSec>0 in
// streamChatToolsProgress, plus exact heartbeat reporting on noContentTimedOut.
func TestK22_4_CannedErrorRetryWithNoContentSecAndHeartbeats(t *testing.T) {
	const canned91 = "I'm having a hard time fulfilling your request. Can I help you with something else instead?"

	t.Run("canned_error retries once and recovers even when NoContentSec > 0", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				body := fakeStreamWithErrorMarker(canned91, "e6fa609c3fa255c0", "3.1 Pro", true)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				time.Sleep(15 * time.Millisecond)
				_, _ = pw.Write([]byte(fakeStreamServed("Réponse récupérée après erreur.", "", "e6fa609c3fa255c0", "3.1 Pro")))
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))

		var logBuf syncBuffer
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"
		cfg.NoContentSec = 240
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 20 * time.Millisecond
		s.noContentTimeoutOverride = 120 * time.Millisecond
		s.cannedRetryTimeoutOverride = 80 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
		content, reasoning := sseContentAndReasoning(t, c)
		if !strings.Contains(reasoning, "Gemini a renvoyé une erreur interne : nouvelle tentative à l'identique") {
			t.Fatalf("expected retry wait note in reasoning, got %q", reasoning)
		}
		if !strings.Contains(content, "Réponse récupérée après erreur.") {
			t.Fatalf("expected recovered content, got %q", content)
		}
		if !strings.Contains(logBuf.String(), "action=retry_once") {
			t.Fatalf("expected action=retry_once log, got:\n%s", logBuf.String())
		}
		if got := s.stats.cannedErrorRetries.Load(); got != 1 {
			t.Fatalf("cannedErrorRetries = %d, want 1", got)
		}
	})

	t.Run("canned_error retry uses cannedRetryTimeout when shorter than NoContentSec", func(t *testing.T) {
		var calls atomic.Int32
		canceled := make(chan struct{}, 1)
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				body := fakeStreamWithErrorMarker(canned91, "e6fa609c3fa255c0", "3.1 Pro", true)
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

		var logBuf syncBuffer
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"
		cfg.NoContentSec = 240
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 20 * time.Millisecond
		s.noContentTimeoutOverride = 300 * time.Millisecond
		s.cannedRetryTimeoutOverride = 50 * time.Millisecond
		s.stallRetryBackoffOverride = -1
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		t0 := time.Now()
		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		elapsed := time.Since(t0)
		assertProgressStream(t, c)
		if elapsed >= 250*time.Millisecond {
			t.Fatalf("retry waited for noContentTimeout instead of cannedRetryTimeout (%s)", elapsed)
		}
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("expected stalled retry upstream request to be canceled")
		}
		content, _ := sseContentAndReasoning(t, c)
		if !strings.Contains(content, "GMINI : Gemini a renvoyé une erreur interne") {
			t.Fatalf("expected French canned error message, got %q", content)
		}
		logs := logBuf.String()
		if !strings.Contains(logs, "timed_out=true") || !strings.Contains(logs, "action=reported") {
			t.Fatalf("expected timed_out=true action=reported log, got:\n%s", logs)
		}
		if f := s.modelFailures(); len(f) != 1 || f[0].Count != 1 {
			t.Fatalf("expected 1 model failure recorded, got %+v", f)
		}
	})

	t.Run("tool_refusal retry handles noContentTimedOut cleanly", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				body := fakeStreamServed("Aucun outil local n'est disponible.", "", "e6fa609c3fa255c0", "3.1 Pro")
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
				time.Sleep(20 * time.Millisecond)
				_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[1]}]\"]]\n"))
				<-r.Context().Done()
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))

		var logBuf syncBuffer
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.NoContentSec = 240
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 20 * time.Millisecond
		s.noContentTimeoutOverride = 60 * time.Millisecond
		s.stallRetryBackoffOverride = -1
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		content, _ := sseContentAndReasoning(t, c)
		if !strings.Contains(content, "Google n'envoie que des signaux d'attente depuis") {
			t.Fatalf("expected no-content timeout message on retry, got %q", content)
		}
		if !strings.Contains(logBuf.String(), "upstream_no_content_timeout") || !strings.Contains(logBuf.String(), "heartbeats=2") {
			t.Fatalf("expected upstream_no_content_timeout with heartbeats=2, got:\n%s", logBuf.String())
		}
	})
}
