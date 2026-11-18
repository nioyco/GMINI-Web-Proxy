package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

// K8: (a) Tool prompt rules include artifact/search_web instructions only when
// write_file or search_web are declared in the tools list.
func TestToolPromptConditionalRules(t *testing.T) {
	reqBoth := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "write_file", Parameters: json.RawMessage(`{}`)}},
			{Type: "function", Function: &apiconv.ToolFunction{Name: "search_web", Parameters: json.RawMessage(`{}`)}},
		},
	}
	pBoth, _, _ := reqBoth.Prompt()
	if !strings.Contains(pBoth, "The word \"artifact\" in instructions refers to a local file") {
		t.Fatalf("missing artifact instruction when write_file is declared: %s", pBoth)
	}
	if !strings.Contains(pBoth, "Web searches must be performed only using the provided \"search_web\" tool") {
		t.Fatalf("missing search_web instruction when search_web is declared: %s", pBoth)
	}

	reqNone := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "read_file", Parameters: json.RawMessage(`{}`)}},
		},
	}
	pNone, _, _ := reqNone.Prompt()
	if strings.Contains(pNone, "The word \"artifact\"") {
		t.Fatalf("unexpected artifact instruction when write_file is not declared: %s", pNone)
	}
	if strings.Contains(pNone, "Web searches must be performed only") {
		t.Fatalf("unexpected search_web instruction when search_web is not declared: %s", pNone)
	}
}

// K8: Telemetry counter for internal tool signals (expand_skills, widget, internal artifact).
func TestInternalToolSignalDetectionAndCounter(t *testing.T) {
	signals := []string{
		"retriever:expand_skills to trigger artifact generation",
		"Decided to call expand_skills per prompt instructions",
		`<GenerateWidget component_placeholder_id="GenerateWidget_c_7b" type="artifact"/>`,
		"trigger artifact generation for the user",
	}
	for _, s := range signals {
		if !apiconv.IsInternalToolSignal(s) {
			t.Errorf("expected internal tool signal detected in: %q", s)
		}
	}
	nonSignals := []string{
		"Calling read_file for local inspection.",
		"Here is the local document.",
		"Ordinary explanation about artifacts in Antigravity.",
	}
	for _, s := range nonSignals {
		if apiconv.IsInternalToolSignal(s) {
			t.Errorf("false positive internal tool signal in: %q", s)
		}
	}
}

// K8: Silence after thoughts (no text, no new frames for SilenceWarningSec)
// emits a progress note into SSE reasoning and sets status_note in in-flight state.
func TestProlongedSilenceAfterThoughtsWarning(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress, cfg.StreamThoughts = true, true
	cfg.SilenceWarningSec = 1 // 1 second for fast test
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	models := []*gemini.AvailableModel{
		{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"},
	}
	client.SetModels(models)

	pr, pw := io.Pipe()
	go func() {
		// Frame 1: thoughts arrive
		f1 := fakeStreamServed("", "Thinking about moon phases and expand_skills...", "e6fa609c3fa255c0", "3.1 Pro")
		pw.Write([]byte(f1))
		// Silence: do not write text for 1.5 seconds (triggering the 1s silence warning)
		time.Sleep(1500 * time.Millisecond)
		// Frame 2: text finally arrives
		f2 := fakeStreamServed("La prochaine pleine lune est le 25 novembre.", "Thinking...", "e6fa609c3fa255c0", "3.1 Pro")
		pw.Write([]byte(f2))
		pw.Close()
	}()

	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 100 * time.Millisecond

	// Check status counter after request
	b := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"lune"}],"tools":[{"type":"function","function":{"name":"search_web","parameters":{"type":"object"}}}]}`
	rec := post(s, "/v1/chat/completions", b)
	if rec.Code != 200 {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	resBody := rec.Body.String()

	// Must contain the silence note
	if !strings.Contains(resBody, "Gemini ne répond plus depuis") || !strings.Contains(resBody, "outil interne") {
		t.Fatalf("missing silence warning in stream output: %s", resBody)
	}
	// Must contain the final text
	if !strings.Contains(resBody, "25 novembre") {
		t.Fatalf("missing final text in stream output: %s", resBody)
	}

	// Must have incremented internal_tool_detected in /_status
	h := s.handler()
	stRec := uiDo(h, "GET", "/_status", "127.0.0.1:5000", "")
	var stMap map[string]any
	json.Unmarshal(stRec.Body.Bytes(), &stMap)
	counters := stMap["counters"].(map[string]any)
	if detected, ok := counters["internal_tool_detected"].(float64); !ok || detected < 1 {
		t.Fatalf("expected internal_tool_detected >= 1, got %v", counters["internal_tool_detected"])
	}
}
