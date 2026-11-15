package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// fakeStreamServed builds a StreamGenerate frame with position 39 (served id) and
// position 42 (served name), optionally carrying thoughts at candidate[37].
func fakeStreamServed(text, thought, servedID, servedName string) string {
	cand := make([]any, 38)
	cand[0] = "rc_x"
	cand[1] = []any{text}
	if thought != "" {
		cand[37] = []any{[]any{thought}}
	}
	body := make([]any, 43)
	body[4] = []any{cand}
	body[39], body[42] = servedID, servedName
	ib, _ := json.Marshal(body)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

// fakeStream builds a StreamGenerate body: one frame, with or without thoughts
// (thoughts sit at index 37 of the first candidate).
func fakeStream(text, thought string) string {
	cand := make([]any, 38)
	cand[0] = "rc_x"
	cand[1] = []any{text}
	if thought != "" {
		cand[37] = []any{[]any{thought}}
	}
	ib, _ := json.Marshal([]any{nil, nil, nil, nil, []any{cand}})
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

// A model that usually streams thoughts and suddenly does not: the reply carries
// a warning (as reasoning text) and the answer itself is untouched.
func TestWarnSuspectFallbackInStream(t *testing.T) {
	cfg := testConfig()
	cfg.StreamToolsProgress, cfg.StreamThoughts, cfg.WarnSuspectFallback = true, true, true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels(testModels())
	thought := "je réfléchis"
	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fakeStream("Réponse.", thought)))}, nil
	}))
	s := newServer(cfg, client, testLogger())
	body := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"x"}],
	 "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}}]}`
	call := func() string {
		rec := post(s, "/v1/chat/completions", body)
		if rec.Code != 200 {
			t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	for i := 0; i < 2; i++ {
		if out := call(); strings.Contains(out, "GMINI") {
			t.Fatalf("warning on a normal reply: %s", out)
		}
	}
	thought = ""
	out := call()
	if !strings.Contains(out, "GMINI") || !strings.Contains(out, "reasoning_content") || !strings.Contains(out, "Réponse.") {
		t.Fatalf("no warning on a reply without thoughts: %s", out)
	}
	cfg.WarnSuspectFallback = false
	if out := call(); strings.Contains(out, "GMINI") {
		t.Fatalf("warning although disabled")
	}
}

// K3: name_only emits a warning mentioning shortcut 7, rate-limits repeats per
// model for 10 minutes, and re-emits immediately if the verdict changes or after 10m.
// Requests are never blocked and no extra Google call is ever made.
func TestNameOnlyWarningRateLimitAndUIState(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress, cfg.StreamThoughts, cfg.WarnSuspectFallback = true, true, true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	models := []*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen"},
		{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"},
	}
	client.SetModels(models)

	var servedID, servedName = "cf41b0e0dd7d53e5", "4 Hydrogen"
	upstreamCalls := 0
	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		upstreamCalls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fakeStreamServed("Réponse.", "", servedID, servedName)))}, nil
	}))

	s := newServer(cfg, client, testLogger())
	now := time.Date(2026, 11, 15, 7, 42, 0, 0, time.UTC)
	s.warnNow = func() time.Time { return now }

	ask := func(model string) string {
		b := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"c":{"type":"string"}}}}}]}`
		rec := post(s, "/v1/chat/completions", b)
		if rec.Code != 200 {
			t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}

	// 1st call -> name_only warning emitted
	out1 := ask("gemini-everyday-help")
	if !strings.Contains(out1, "identifiant différent") || !strings.Contains(out1, "raccourci 7") {
		t.Fatalf("missing name_only warning: %s", out1)
	}
	// 2nd call 3 min later with same verdict -> warning suppressed, request still served
	now = now.Add(3 * time.Minute)
	out2 := ask("gemini-everyday-help")
	if strings.Contains(out2, "GMINI") || !strings.Contains(out2, "Réponse.") {
		t.Fatalf("expected suppressed warning on repeat within 10m: %s", out2)
	}
	// 3rd call 1 min later on DIFFERENT verdict (other_model) -> re-emits immediately
	now = now.Add(1 * time.Minute)
	servedID, servedName = "e6fa609c3fa255c0", "3.1 Pro"
	out3 := ask("gemini-everyday-help")
	if !strings.Contains(out3, "ne vient probablement pas") {
		t.Fatalf("expected immediate re-emit on verdict change: %s", out3)
	}
	// 4th call 10+ min later with same verdict (other_model) -> emits again
	now = now.Add(10 * time.Minute)
	out4 := ask("gemini-everyday-help")
	if !strings.Contains(out4, "ne vient probablement pas") {
		t.Fatalf("expected warning again after 10m: %s", out4)
	}
	// Check exact 1:1 upstream calls (never triggered extra Google call)
	if upstreamCalls != 4 {
		t.Fatalf("upstreamCalls=%d, want 4", upstreamCalls)
	}

	// Check /_status exposes mismatch_since for gemini-everyday-help
	h := s.handler()
	stRec := uiDo(h, "GET", "/_status", "127.0.0.1:5000", "")
	if !strings.Contains(stRec.Body.String(), `"last_verdict":"other_model"`) || !strings.Contains(stRec.Body.String(), `"mismatch_since"`) {
		t.Fatalf("/_status missing model_identities mismatch: %s", stRec.Body.String())
	}
	uiRec := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	if !strings.Contains(uiRec.Body.String(), `"mismatch_since"`) {
		t.Fatalf("/_ui/api/state missing mismatch_since: %s", uiRec.Body.String())
	}

	// Confirmed reply clears mismatch_since
	servedID, servedName = "56fdd199312815e2", "4 Hydrogen"
	out5 := ask("gemini-everyday-help")
	if strings.Contains(out5, "GMINI") {
		t.Fatalf("unexpected warning on confirmed reply: %s", out5)
	}
	stRec2 := uiDo(h, "GET", "/_status", "127.0.0.1:5000", "")
	if strings.Contains(stRec2.Body.String(), `"mismatch_since"`) {
		t.Fatalf("mismatch_since not cleared after confirmed reply: %s", stRec2.Body.String())
	}
}
