package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestFlightHistoryPersistenceAndTokenUI(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "cookie.txt.state.json")
	flightsPath := filepath.Join(dir, "cookie.txt.flights.json")
	tokensPath := filepath.Join(dir, "cookie.txt.tokens.jsonl")
	callsPath := filepath.Join(dir, "cookie.txt.calls.jsonl")

	cfg := &Config{
		Listen:      "127.0.0.1:0",
		AllowNoAuth: true,
		UIEnabled:   true,
		FlightsFile: flightsPath,
		Gemini: gemini.Config{
			Cookie:     "SID=test",
			StateFile:  statePath,
			CallsFile:  callsPath,
			TokensFile: tokensPath,
		},
	}

	c1, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s1 := newServer(cfg, c1, testLogger())
	s1.flightHist.record(InFlightRequest{
		ID:            "req12345",
		ConvID:        "conv01",
		State:         "Terminée",
		Model:         "gemini-3.1-pro",
		ServedID:      "e6fa609c3fa255c0",
		ServedName:    "3.1 Pro",
		GoogleModel:   "3.1 Pro",
		Identity:      "confirmed",
		StartTime:     time.Now().UTC().Format(time.RFC3339),
		DurationMS:    1200,
		PromptBytes:   4000,
		ThoughtBytes:  800,
		TextBytes:     400,
		InputTokens:   1000,
		ThoughtTokens: 200,
		TextTokens:    100,
		TotalTokens:   1300,
	})

	info, err := os.Stat(flightsPath)
	if err != nil {
		t.Fatalf("flights file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("flights file perm = %v, want 0600", info.Mode().Perm())
	}

	// Simulate proxy restart: new server loads flightsPath automatically.
	c2, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	s2 := newServer(cfg, c2, testLogger())
	h := s2.handler()

	wState := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s2.uiToken)
	if wState.Code != http.StatusOK {
		t.Fatalf("/_ui/api/state status = %d", wState.Code)
	}
	var st struct {
		InFlightHistory []InFlightRequest         `json:"in_flight_history"`
		TokenUsage      gemini.TokenUsageSnapshot `json:"token_usage"`
	}
	if err := json.Unmarshal(wState.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.InFlightHistory) != 1 || st.InFlightHistory[0].TotalTokens != 1300 || st.InFlightHistory[0].GoogleModel != "3.1 Pro" {
		t.Fatalf("reloaded in_flight_history = %+v", st.InFlightHistory)
	}
	if !st.TokenUsage.Persisted || st.TokenUsage.FileName != "cookie.txt.tokens.jsonl" {
		t.Errorf("unexpected token_usage meta: %+v", st.TokenUsage)
	}

	wHTML := uiDo(h, "GET", "/_ui", "127.0.0.1:5000", "")
	for _, want := range []string{
		"Quota Google & Estimation des tokens",
		"Tokens (est.)",
		"Modèle indiqué par Google",
	} {
		if !strings.Contains(wHTML.Body.String(), want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}
