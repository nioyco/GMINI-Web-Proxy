package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func extractStrLogField(t *testing.T, line, key string) string {
	t.Helper()
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `=([^\s]+)`)
	m := re.FindStringSubmatch(line)
	if len(m) != 2 {
		t.Fatalf("missing %s in log line: %q", key, line)
	}
	return strings.Trim(m[1], `"`)
}

// TestK23_1_ClientCancelledLogIncludesRequestIDAndHeartbeats verifies that
// client_cancelled logs include request_id matching request_end and the
// upstream_heartbeats counter from CallMetrics.
func TestK23_1_ClientCancelledLogIncludesRequestIDAndHeartbeats(t *testing.T) {
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
			time.Sleep(25 * time.Millisecond)
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	s := newServer(cfg, client, logger)
	s.keepaliveOverride = 20 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	const reqID = "k23-cancel-trace-01"
	reqCtx, cancelReq := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(k9ChatToolsBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
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
		t.Fatal("timed out waiting for heartbeats")
	}
	time.Sleep(20 * time.Millisecond)
	cancelReq()
	resp.Body.Close()

	waitForCondition(t, 2*time.Second, func() bool {
		logs := logBuf.String()
		return strings.Contains(logs, "msg=client_cancelled") && strings.Contains(logs, "msg=request_end")
	})

	logs := logBuf.String()
	ccLine := findLogLine(logs, "msg=client_cancelled")
	reLine := findLogLine(logs, "msg=request_end")
	if ccLine == "" || reLine == "" {
		t.Fatalf("missing client_cancelled or request_end in:\n%s", logs)
	}
	ccID := extractStrLogField(t, ccLine, "request_id")
	reID := extractStrLogField(t, reLine, "request_id")
	if ccID != reqID || ccID != reID {
		t.Fatalf("request_id mismatch: client_cancelled=%q request_end=%q want=%q\n%s", ccID, reID, reqID, logs)
	}
	if hb := extractIntLogField(t, ccLine, "upstream_heartbeats"); hb != 2 {
		t.Fatalf("client_cancelled upstream_heartbeats = %d, want 2; line=%s", hb, ccLine)
	}
}

// TestK23_2_QuietHeaderSuppressesStatusPollLogs verifies that GET / and
// GET /v1/models with X-GMINI-Quiet: 1 suppress request_start/request_end logs
// on 200 OK while preserving normal logging without the header (or on >=400).
func TestK23_2_QuietHeaderSuppressesStatusPollLogs(t *testing.T) {
	cfg := testConfig()
	s, logBuf := loggedServer(t, cfg, "text")
	h := s.handler()

	for _, path := range []string{"/", "/v1/models"} {
		req := newReq(http.MethodGet, path, nil)
		req.Header.Set("X-GMINI-Quiet", "1")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s with X-GMINI-Quiet=1 status = %d, want 200", path, rr.Code)
		}
	}
	if got := logBuf.String(); strings.Contains(got, "request_start") || strings.Contains(got, "request_end") {
		t.Fatalf("expected quiet polls to emit no request_start/request_end logs, got:\n%s", got)
	}

	// Same path without X-GMINI-Quiet must still emit request_start and request_end.
	reqNormal := newReq(http.MethodGet, "/", nil)
	rrNormal := httptest.NewRecorder()
	h.ServeHTTP(rrNormal, reqNormal)
	if rrNormal.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rrNormal.Code)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "msg=request_start") || !strings.Contains(logs, "msg=request_end") {
		t.Fatalf("expected GET / without X-GMINI-Quiet to log request_start/request_end, got:\n%s", logs)
	}

	// >=400 status (e.g. forbidden host) must still log request_end even with X-GMINI-Quiet: 1.
	reqBadHost := newReq(http.MethodGet, "/", nil)
	reqBadHost.Host = "evil.example"
	reqBadHost.Header.Set("X-GMINI-Quiet", "1")
	rrBadHost := httptest.NewRecorder()
	h.ServeHTTP(rrBadHost, reqBadHost)
	if rrBadHost.Code != http.StatusForbidden {
		t.Fatalf("GET / bad host status = %d, want 403", rrBadHost.Code)
	}
	if !strings.Contains(logBuf.String(), "status=403") {
		t.Fatalf("expected 403 response to be logged in request_end despite quiet header, got:\n%s", logBuf.String())
	}
}

// TestK23_3_CannedErrorRetryNonProgressiveFallback exercises the non-progressive
// awaitGenerate retry path in streamChatToolsProgress (when s.streamEvents is
// nil during the retry and CannedErrorRetry == "once").
func TestK23_3_CannedErrorRetryNonProgressiveFallback(t *testing.T) {
	const canned91 = "I'm having a hard time fulfilling your request. Can I help you with something else instead?"

	t.Run("recovers on second attempt via awaitGenerate", func(t *testing.T) {
		var streamCalls, genCalls atomic.Int32
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"

		client, err := gemini.New(cfg.Gemini, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		client.SetModels(testModels())
		var logBuf syncBuffer
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 20 * time.Millisecond
		s.cannedRetryTimeoutOverride = 120 * time.Millisecond
		s.streamEvents = func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
			streamCalls.Add(1)
			// Disable progressive streaming before the retry so streamChatToolsProgress falls back to awaitGenerate.
			s.streamEvents = nil
			h.Text(canned91)
			return nil
		}
		s.generate = func(_ context.Context, _ gemini.GenParams) (string, error) {
			genCalls.Add(1)
			return "Réponse récupérée via repli non-progressif.", nil
		}

		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, body(""))
		assertProgressStream(t, c)
		if streamCalls.Load() != 1 || genCalls.Load() != 1 {
			t.Fatalf("calls stream=%d gen=%d, want 1/1", streamCalls.Load(), genCalls.Load())
		}
		content, reasoning := sseContentAndReasoning(t, c)
		if !strings.Contains(reasoning, "Gemini a renvoyé une erreur interne : nouvelle tentative à l'identique") {
			t.Fatalf("expected retry wait note in reasoning, got %q", reasoning)
		}
		if !strings.Contains(content, "Réponse récupérée via repli non-progressif.") {
			t.Fatalf("expected recovered content, got %q", content)
		}
		if got := s.stats.cannedErrorRetries.Load(); got != 1 {
			t.Fatalf("cannedErrorRetries = %d, want 1", got)
		}
		if f := s.modelFailures(); len(f) != 0 {
			t.Fatalf("expected 0 model failures after recovery, got %+v", f)
		}
	})

	t.Run("times out via cannedRetryTimeout and records model failure", func(t *testing.T) {
		var streamCalls, genCalls atomic.Int32
		genCancelled := make(chan struct{}, 1)
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.CannedErrorRetry = "once"

		client, err := gemini.New(cfg.Gemini, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		client.SetModels(testModels())
		var logBuf syncBuffer
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 20 * time.Millisecond
		s.cannedRetryTimeoutOverride = 50 * time.Millisecond
		s.streamEvents = func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
			streamCalls.Add(1)
			s.streamEvents = nil
			h.Text(canned91)
			return nil
		}
		s.generate = func(ctx context.Context, _ gemini.GenParams) (string, error) {
			genCalls.Add(1)
			<-ctx.Done()
			select {
			case genCancelled <- struct{}{}:
			default:
			}
			return "", ctx.Err()
		}

		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, body(""))
		assertProgressStream(t, c)
		if streamCalls.Load() != 1 || genCalls.Load() != 1 {
			t.Fatalf("calls stream=%d gen=%d, want 1/1", streamCalls.Load(), genCalls.Load())
		}
		select {
		case <-genCancelled:
		case <-time.After(time.Second):
			t.Fatal("expected awaitGenerate retry to be canceled on cannedRetryTimeout")
		}
		content, _ := sseContentAndReasoning(t, c)
		if !strings.Contains(content, "GMINI : Gemini a renvoyé une erreur interne") {
			t.Fatalf("expected French canned error message, got %q", content)
		}
		logs := logBuf.String()
		if !strings.Contains(logs, "timed_out=true") || !strings.Contains(logs, "action=reported") {
			t.Fatalf("expected timed_out=true action=reported in logs, got:\n%s", logs)
		}
		if f := s.modelFailures(); len(f) != 1 || f[0].Count != 1 {
			t.Fatalf("expected 1 model failure recorded, got %+v", f)
		}
	})
}
