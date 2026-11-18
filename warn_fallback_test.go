package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
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

// K7: The served identity is available during the request (in-flight state in /_ui/api/state)
// and warnings for name_only / other_model are emitted as soon as position 39 arrives.
func TestInFlightStateAndEarlyWarning(t *testing.T) {
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
	}
	client.SetModels(models)

	frame1Received := make(chan struct{})
	allowFrame2 := make(chan struct{})

	// Two frames: frame 1 carries position 39 with name_only id + 10 bytes of thoughts;
	// frame 2 carries the answer text.
	f1 := fakeStreamServed("", "thinking...", "cf41b0e0dd7d53e5", "4 Hydrogen")
	f2 := fakeStreamServed("Here is the answer.", "thinking...", "cf41b0e0dd7d53e5", "4 Hydrogen")

	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(f1))
		close(frame1Received)
		<-allowFrame2
		pw.Write([]byte(f2))
		pw.Close()
	}()

	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 100 * time.Millisecond
	h := s.handler()

	type clientRes struct {
		body string
		code int
	}
	resChan := make(chan clientRes, 1)
	go func() {
		b := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object"}}}]}`
		rec := post(s, "/v1/chat/completions", b)
		resChan <- clientRes{body: rec.Body.String(), code: rec.Code}
	}()

	// Wait until frame 1 has been sent to client
	<-frame1Received
	// Give the server 30ms to process frame 1
	time.Sleep(30 * time.Millisecond)

	// Check /_ui/api/state during generation
	uiRec := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	var stateMap map[string]any
	if err := json.Unmarshal(uiRec.Body.Bytes(), &stateMap); err != nil {
		t.Fatalf("json unmarshal ui state failed: %v", err)
	}
	inFlightRaw, ok := stateMap["in_flight"].([]any)
	if !ok || len(inFlightRaw) == 0 {
		t.Fatalf("expected in_flight entry during generation, got: %v", stateMap["in_flight"])
	}
	entry := inFlightRaw[0].(map[string]any)
	if entry["model"] != "gemini-everyday-help" {
		t.Errorf("in_flight model = %v, want gemini-everyday-help", entry["model"])
	}
	if entry["served_id"] != "cf41b0e0dd7d53e5" {
		t.Errorf("in_flight served_id = %v, want cf41b0e0dd7d53e5", entry["served_id"])
	}
	if entry["identity"] != "name_only" {
		t.Errorf("in_flight identity = %v, want name_only", entry["identity"])
	}

	// Now allow frame 2 to complete generation
	close(allowFrame2)
	res := <-resChan
	if res.code != 200 {
		t.Fatalf("call failed: %d %s", res.code, res.body)
	}

	// Warning must have been emitted in reasoning content
	if !strings.Contains(res.body, "identifiant différent") {
		t.Fatalf("warning missing from stream output: %s", res.body)
	}

	// Warning must not be duplicated twice
	if strings.Count(res.body, "identifiant différent") > 1 {
		t.Fatalf("warning duplicated in stream output: %s", res.body)
	}

	// After generation completes, in_flight must be empty
	uiRecAfter := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	var afterMap map[string]any
	json.Unmarshal(uiRecAfter.Body.Bytes(), &afterMap)
	if inFlightAfter, _ := afterMap["in_flight"].([]any); len(inFlightAfter) != 0 {
		t.Fatalf("in_flight should be empty after request completion, got: %v", inFlightAfter)
	}
}
