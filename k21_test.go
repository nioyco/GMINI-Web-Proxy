package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestK21_P2_CodeRefFastpathDrainsDoneBeforeRequestEnd verifies that when
// code_ref_fastpath cancels the upstream stream early, runStreamTools waits
// (bounded) on <-done so streamOnce's defer finishes (logging served_model and
// setting upstream_ms) before request_end is logged.
func TestK21_P2_CodeRefFastpathDrainsDoneBeforeRequestEnd(t *testing.T) {
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	cfg := testConfig()
	cfg.Gemini.LogParams = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.CodeRefFastpath = true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen"},
	})
	client.SetTransport(rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			frame := fakeStreamServed("```bash?code_reference\ngit status -s\n```", "", "56fdd199312815e2", "4 Hydrogen")
			_, _ = pw.Write([]byte(frame))
			<-r.Context().Done()
			// Simulate streamOnce defer / body teardown taking ~25 ms after cancel(ErrEarlyStop).
			time.Sleep(25 * time.Millisecond)
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	s := newServer(cfg, client, logger)
	s.keepaliveOverride = 20 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	body := `{
		"model":"gemini-everyday-help",
		"stream":true,
		"messages":[{"role":"user","content":"check git status"}],
		"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"}},"required":["CommandLine"]}}}]
	}`
	c := readSSE(t, context.Background(), ts.URL, body)
	assertProgressStream(t, c)
	if !strings.Contains(c.raw, `"name":"run_command"`) {
		t.Fatalf("expected code_ref_fastpath to convert to run_command, got: %s", c.raw)
	}

	logs := logBuf.String()
	idxServed := strings.Index(logs, "served_model")
	idxReqEnd := strings.Index(logs, "request_end")
	if idxServed < 0 || idxReqEnd < 0 {
		t.Fatalf("expected both served_model and request_end in logs, got:\n%s", logs)
	}
	if idxServed > idxReqEnd {
		t.Fatalf("expected served_model before request_end (no race), got served=%d request_end=%d\n%s", idxServed, idxReqEnd, logs)
	}
	if strings.Contains(logs[idxReqEnd:], "upstream_ms=0 ") {
		t.Fatalf("expected non-zero upstream_ms in request_end, got:\n%s", logs[idxReqEnd:])
	}
}

// TestK21_NoContentSecCountsMultipleEmptyHeartbeats proves that repeated
// {"37":[N]} keepalive heartbeats without thoughts, text, or status frames
// do not reset the GEMINI_NO_CONTENT_SEC watchdog and all heartbeats are counted.
func TestK21_NoContentSecCountsMultipleEmptyHeartbeats(t *testing.T) {
	canceled := make(chan struct{}, 1)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(")]}'\n\n36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			time.Sleep(15 * time.Millisecond)
			_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[1]}]\"]]\n"))
			time.Sleep(15 * time.Millisecond)
			_, _ = pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[2]}]\"]]\n"))
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
	cfg.HeldStallRetries = -1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 20 * time.Millisecond
	s.noContentTimeoutOverride = 70 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("expected upstream request to be canceled after 3 empty heartbeats")
	}
	content, _ := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "Google n'envoie que des signaux d'attente depuis") {
		t.Fatalf("expected no-content timeout message, got %q", content)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "upstream_heartbeats=3") {
		t.Fatalf("expected upstream_heartbeats=3 in request_end log, got:\n%s", logs)
	}
}
