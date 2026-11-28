package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK44_FinalAttemptDisablesFastStallEvenOnMode1(t *testing.T) {
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.cfg.HeldStallRetries = 1
	srv.heldStallFastOverride = 25 * time.Millisecond
	srv.heldStallOverride = 160 * time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		h.Text("```tool_call\n{\"name\":\"write_file\",")
		if n == 1 {
			// Initial attempt: stalls -> fast_stall (25ms) cuts it quickly.
			<-ctx.Done()
			return ctx.Err()
		}
		// Final attempt (1/1): pauses 55ms (> fast_stall 25ms, < heldStall 160ms), then completes.
		// Because allowFast=false on final attempt even with Mode==1, it must NOT be cut at 25ms.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(55 * time.Millisecond):
		}
		h.Text("\"arguments\":{\"path\":\"ok.txt\",\"content\":\"done\"}}\n```")
		return nil
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	all := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(all, "write_file") {
		t.Fatalf("expected final attempt to survive >25ms and deliver write_file, got: %s", all)
	}
}

func TestK44_DefaultMaxStallRetries3AndLargePromptThresholds(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.cfg.HeldStallRetries = 0
	srv.heldStallOverride = 0
	srv.heldStallFastOverride = 0

	if got := srv.maxStallRetries(); got != 3 {
		t.Fatalf("default maxStallRetries() = %d, want 3", got)
	}

	// Small prompt keeps 60s / 25s
	sSmall, fSmall := srv.effectiveHeldStallDurations(100 * 1024)
	if sSmall != 60*time.Second || fSmall != 25*time.Second {
		t.Errorf("small prompt durations = (%s, %s), want (60s, 25s)", sSmall, fSmall)
	}

	// Large prompt (>= 250 KiB) uses K59 floors 60s / 30s
	sLarge, fLarge := srv.effectiveHeldStallDurations(260 * 1024)
	if sLarge != 60*time.Second || fLarge != 30*time.Second {
		t.Errorf("large prompt durations = (%s, %s), want (60s, 30s)", sLarge, fLarge)
	}

	// Attempt 3/3 in prepareStallRetry keeps level=1
	a3, ok := srv.prepareStallRetry(context.Background(), nil, "cid", 1, "gemini-everyday-help", false, gemini.GenParams{Mode: 1, Level: 1}, streamToolsResult{}, 3, 3)
	if !ok || a3.Level != 1 {
		t.Fatalf("attempt 3/3 Level = %d (ok=%v), want 1", a3.Level, ok)
	}
}

func TestK44_TerminalStallRecordsDiagIncident(t *testing.T) {
	srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Opened()
		h.Text("```tool_call\n{\"name\":\"write_file\",")
		<-ctx.Done()
		return ctx.Err()
	})
	srv.cfg.HeldStallRetries = -1
	srv.heldStallOverride = 30 * time.Millisecond

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	_ = readSSE(t, context.Background(), ts.URL, body)

	resp := srv.diag.query(diagQuery{Window: "1h", Kind: "incident", Limit: 20}, time.Now())
	var found bool
	for _, it := range resp.Items {
		if it.Type == "stall" && it.Detail == "terminal" && !it.Recovered {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected terminal stall incident with Recovered=false, got: %+v", resp.Items)
	}
}
