package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK51_InternalToolRetryAllowedAfterInitialSilenceRetry(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var prompts []string

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.cfg.InternalToolAction = "retry"
	srv.initialSilenceTimeoutOverride = 25 * time.Millisecond
	srv.stallRetryBackoffOverride = time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		prompts = append(prompts, p.Prompt)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			// Initial silence -> triggers initial_silence_retry (isRetry=true on attempt 2)
			<-ctx.Done()
			return ctx.Err()
		case 2:
			// Attempt 2 triggers internal tool -> must STILL trigger internal_tool_retry!
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		default:
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (initial_silence -> internal_tool_retry -> ok)", attempts.Load())
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
		t.Fatalf("expected final attempt to emit read_file; frames=%v", c.dataFrames)
	}
	mu.Lock()
	p2 := prompts[1]
	mu.Unlock()
	if !strings.Contains(p2, "Use ONLY the declared local tools") {
		t.Errorf("attempt 2 missing initialSilenceAntiInternalNote: %q", p2)
	}
}

func TestK51_InternalToolStatusDoesNotDisarmNoContentTimeout(t *testing.T) {
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.cfg.InternalToolAction = "retry"
	srv.initialSilenceTimeoutOverride = -1
	srv.noContentTimeoutOverride = 40 * time.Millisecond
	srv.stallRetryBackoffOverride = time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		if n == 1 {
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		}
		// Attempt 2 (after internalToolRetried=true): emits ONLY internal tool status, no text/thought.
		h.Status("Searching the web")
		<-ctx.Done()
		return ctx.Err()
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	start := time.Now()
	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %s, expected noContentTimeout to fire promptly despite internal tool status", elapsed)
	}
	joined := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(joined, "signaux d'attente") {
		t.Fatalf("expected no_content_timeout message in SSE, got: %s", joined)
	}
}

func TestK51_ShortToolPromptReminderAntiGoogle(t *testing.T) {
	apiconv.SetToolPromptRules(true)
	apiconv.SetToolPromptReminder(true)
	req := apiconv.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []apiconv.Message{
			{Role: "user", Content: json.RawMessage(`"step 1"`)},
			{Role: "assistant", Content: json.RawMessage(`"ok"`)},
			{Role: "user", Content: json.RawMessage(`"step 2"`)},
		},
		Tools: []apiconv.Tool{{
			Type:     "function",
			Function: &apiconv.ToolFunction{Name: "read_file"},
		}},
	}
	prompt, _, _ := req.Prompt()
	for _, want := range []string{"built-in assistant feature", "internal tool", "external connector", "prefix tool names with any namespace", "```tool_call```"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(prompt), "canva") {
		t.Errorf("prompt must NEVER mention canva/Canvas")
	}
}

func TestK51_IsUpstreamToolRefusalFRAndEN(t *testing.T) {
	pos := []string{
		"les outils d'accès aux fichiers locaux (`read_file`, `write_file`) ne sont pas disponibles dans cet environnement (seuls des connecteurs externes comme Drive, Docs... sont déclarés).",
		"Les outils locaux ne sont pas déclarés dans cette session.",
		"Seuls des connecteurs externes comme Google Drive sont déclarés.",
		"The local file access tools are not available in this environment.",
		"Local tools are not exposed; only external connectors are declared.",
	}
	for _, s := range pos {
		if !apiconv.IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = false, want true", s)
		}
	}
	neg := []string{
		"les outils ne sont pas disponibles\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a\"}}\n```",
		"<summary>les outils d'accès aux fichiers locaux ne sont pas disponibles</summary>",
	}
	for _, s := range neg {
		if apiconv.IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = true, want false", s)
		}
	}
}

func TestK51_ToolCallUnparsedRetry(t *testing.T) {
	var logBuf bytes.Buffer
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.initialSilenceTimeoutOverride = -1

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		if n == 1 {
			h.Text("```tool_call\nnot valid json body\n```")
			return nil
		}
		if !strings.Contains(p.Prompt, "strictly valid JSON") {
			t.Errorf("retry prompt missing toolUnparsedRetryNote: %q", p.Prompt)
		}
		h.Text("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"ok.txt\",\"content\":\"done\"}}\n```")
		return nil
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "write_file") {
		t.Fatalf("expected retry to deliver write_file; frames=%v", c.dataFrames)
	}
	if !strings.Contains(logBuf.String(), "tool_unparsed_retry") {
		t.Errorf("expected tool_unparsed_retry in logs: %s", logBuf.String())
	}
	dr := srv.diag.query(diagQuery{Kind: "incident", Type: "tool_unparsed", Window: "all", Limit: 10}, time.Now())
	if len(dr.Items) == 0 || !dr.Items[0].Recovered {
		t.Fatalf("expected recovered tool_unparsed incident in diagRing, got: %+v", dr.Items)
	}
}
