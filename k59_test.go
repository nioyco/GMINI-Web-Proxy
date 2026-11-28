package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK59_CannedErrorRetryExceededLargePromptHeartbeats(t *testing.T) {
	srv := &Server{cfg: &Config{CannedErrorRetrySec: 55}}
	large := cannedErrorLargePromptBytes

	if srv.cannedErrorRetryExceeded(55*time.Second, large, 0) {
		t.Fatal("55s with heartbeats=0 on large prompt must NOT exceed budget (waits until 70s for 1st heartbeat)")
	}
	if srv.cannedErrorRetryExceeded(65*time.Second, large, 0) {
		t.Fatal("65s with heartbeats=0 on large prompt must NOT exceed budget")
	}
	if !srv.cannedErrorRetryExceeded(70*time.Second, large, 0) {
		t.Fatal("70s with heartbeats=0 on large prompt MUST exceed budget")
	}
	if srv.cannedErrorRetryExceeded(90*time.Second, large, 1) {
		t.Fatal("90s with heartbeats=1 on large prompt must NOT exceed 120s budget")
	}
	if !srv.cannedErrorRetryExceeded(120*time.Second, large, 1) {
		t.Fatal("120s with heartbeats=1 on large prompt MUST exceed 120s budget")
	}
}

func TestK59_UIModelsPreservesIdentityOverTimeoutAndReflectsInFlight(t *testing.T) {
	// 1. Seed a confirmed CallRecord followed by a newer stream_interrupted (not_stated) CallRecord.
	now := time.Now().UTC()
	var calls []string
	// Confirmed call first (older)
	c1, _ := json.Marshal(gemini.CallRecord{
		Time:       now.Add(-2 * time.Minute).Format(time.RFC3339),
		RequestID:  "req-old-ok",
		Model:      "gemini-deeper-analysis",
		ModelID:    "neon1234",
		ServedID:   "neon1234",
		ServedName: "4 Neon",
		Identity:   "confirmed",
		Stream:     true,
	})
	// Timeout call second (newer)
	c2, _ := json.Marshal(gemini.CallRecord{
		Time:      now.Add(-1 * time.Minute).Format(time.RFC3339),
		RequestID: "req-new-timeout",
		Model:     "gemini-deeper-analysis",
		ModelID:   "neon1234",
		Identity:  "not_stated",
		Error:     "stream_interrupted",
		Stream:    true,
	})
	calls = append(calls, string(c1), string(c2))
	_ = calls

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: "neon1234", ModeCode: 3, Description: "4 Neon", DisplayName: "4 Neon"},
	})
	// Record confirmed call, then timeout call via fake Generate calls or direct journal?
	// Let's use RecordTestCall? Wait: let's check if client exposes a method or use k9Client responses.
	_ = c1
	_ = c2

	// Actually, we can trigger two real calls through client.Generate with wrb.fr responses!
	const neonID = "0123456789abcdef"
	var step int
	_, client2 := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		step++
		if step == 1 {
			inner2 := make([]any, 43)
			inner2[4] = []any{[]any{"rc_1", []any{"OK"}, nil, nil, nil}}
			inner2[39] = neonID
			inner2[42] = "4 Neon"
			ij, _ := json.Marshal(inner2)
			oj, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ij)}})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(")]}'\n\n" + string(oj) + "\n"))}, nil
		}
		// Second call: interrupted stream (sent=true, no identity -> rec.Error="stream_interrupted", rec.Identity="not_stated")
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(")]}'\n\n"))
			_ = pw.CloseWithError(io.ErrUnexpectedEOF)
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	client2.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: neonID, ModeCode: 3, Description: "4 Neon", DisplayName: "4 Neon"},
	})
	m := &gemini.AvailableModel{Name: "gemini-deeper-analysis", ModelID: neonID, ModeCode: 3, Description: "4 Neon", DisplayName: "4 Neon"}
	_, _ = client2.Generate(context.Background(), gemini.GenParams{Model: m, Prompt: "hi"})
	// Second call: streamOnce that errors after opening so rec.Error="stream_interrupted", rec.Identity="not_stated"
	_ = client2.GenerateStreamEvents(context.Background(), gemini.GenParams{Model: m, Prompt: "hi2"}, gemini.StreamHandlers{})

	cfg := testConfig()
	srv := newServer(cfg, client2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	models := srv.uiModels()
	if len(models) != 1 {
		t.Fatalf("len(uiModels) = %d, want 1", len(models))
	}
	if models[0].LastIdentity != "confirmed" || models[0].LastServed != "4 Neon" {
		t.Fatalf("uiModels()[0] = (%q, %q), want (confirmed, 4 Neon)", models[0].LastIdentity, models[0].LastServed)
	}

	// 2. Active in-flight request with confirmed identity overrides immediately in live view.
	srv.active.add(&reqInfo{
		id:              "inf-k59-1",
		model:           "gemini-deeper-analysis",
		inFlightVerdict: "other_model",
		servedName:      "3.5 Flash-Lite",
		start:           now,
	})
	modelsLive := srv.uiModels()
	if modelsLive[0].LastIdentity != "other_model" || modelsLive[0].LastServed != "3.5 Flash-Lite" {
		t.Fatalf("live uiModels()[0] = (%q, %q), want (other_model, 3.5 Flash-Lite)", modelsLive[0].LastIdentity, modelsLive[0].LastServed)
	}
}

func TestK59_TimeoutsAndLargePromptStallFloors(t *testing.T) {
	srv := &Server{cfg: &Config{}}
	if got := srv.firstAttemptNoContentCap(); got != 75*time.Second {
		t.Fatalf("firstAttemptNoContentCap() = %v, want 75s", got)
	}
	if got := srv.nonFinalStallNoContentCap(); got != 75*time.Second {
		t.Fatalf("nonFinalStallNoContentCap() = %v, want 75s", got)
	}
	if got := srv.finalNoContentCap(); got != 120*time.Second {
		t.Fatalf("finalNoContentCap() = %v, want 120s", got)
	}
	stall, fast := srv.effectiveHeldStallDurations(300 * 1024)
	if stall != 60*time.Second || fast != 30*time.Second {
		t.Fatalf("effectiveHeldStallDurations(300KB) = (%v, %v), want (60s, 30s)", stall, fast)
	}
}

func TestK59_WebUIv3011DynamicPolling(t *testing.T) {
	if uiVersion != "3.0.11" {
		t.Fatalf("uiVersion = %q, want 3.0.11", uiVersion)
	}
	html := string(uiHTML)
	if !strings.Contains(html, "function schedulePoll(") || !strings.Contains(html, "schedulePoll(inflight.length > 0 ? 2000 : 5000)") {
		t.Fatal("ui.html missing dynamic 2000ms/5000ms schedulePoll")
	}
}
