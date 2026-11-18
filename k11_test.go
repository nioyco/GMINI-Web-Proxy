package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK11_StatusProgressRateLimitedAndInFlight(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 15
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	s := newTestServer(t, cfg)
	s.keepaliveOverride = 200 * time.Millisecond
	s.statusNoteOverride = 60 * time.Millisecond

	var seenInFlightStatus string
	s.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Status("Analyzing user requests")
		time.Sleep(15 * time.Millisecond)
		// Within rate-limit window: should NOT emit a second reasoning note, but updates in-flight.
		h.Status("Intermediate step")
		time.Sleep(15 * time.Millisecond)
		if inf := s.inFlightRequests(); len(inf) == 1 {
			seenInFlightStatus = inf[0].StatusNote
		}
		time.Sleep(55 * time.Millisecond)
		// Same text as last emitted: deduplicated.
		h.Status("Analyzing user requests")
		// New text after cooldown: should emit.
		h.Status("Fulfilling request")
		h.Text("All done.")
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`))
	req.Host = "127.0.0.1:8081"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Google : Analyzing user requests") {
		t.Errorf("expected first Google status in reasoning stream, got:\n%s", body)
	}
	if strings.Contains(body, "Intermediate step") {
		t.Errorf("unexpected rate-limited status in reasoning stream, got:\n%s", body)
	}
	if !strings.Contains(body, "Google : Fulfilling request") {
		t.Errorf("expected second Google status after cooldown, got:\n%s", body)
	}
	if !strings.Contains(body, "All done.") {
		t.Errorf("expected final text, got:\n%s", body)
	}
	if seenInFlightStatus != "Google : Intermediate step" {
		t.Errorf("in-flight StatusNote = %q, want %q", seenInFlightStatus, "Google : Intermediate step")
	}
}

func TestK11_StatusResetsSilenceDetector(t *testing.T) {
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 15
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SilenceWarningSec = 1 // 1s silence threshold
	s := newTestServer(t, cfg)
	s.keepaliveOverride = 100 * time.Millisecond

	s.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Thought("Thinking...")
		// Send status heartbeats every 400ms for 1.2s (> 1s silence threshold)
		for i := 0; i < 3; i++ {
			time.Sleep(400 * time.Millisecond)
			h.Status("Analyzing user requests")
		}
		h.Text("Done.")
		return nil
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]
	}`))
	req.Host = "127.0.0.1:8081"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Gemini ne répond plus") {
		t.Errorf("status frames should count as life and prevent silence warning, got:\n%s", body)
	}
}
