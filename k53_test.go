package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK53_NoPinkElephantGoogleAppNamesInNotesAndPrompts(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)(canva|google\s+drive|\bdrive\b|drive_mcp|gmail|workspace|google\s+docs|\bdocs\b|google\s+search|notebook|youtube|maps)`)

	notes := map[string]string{
		"glitchRetryNote":                glitchRetryNote,
		"internalToolRetryNote":          internalToolRetryNote,
		"initialSilenceAntiInternalNote": initialSilenceAntiInternalNote,
		"toolRefusalRetryNote":           toolRefusalRetryNote,
		"silentSkipRetryNote":            silentSkipRetryNote,
		"unverifiedActionRetryNote":      unverifiedActionRetryNote,
		"postStallHandoffRetryNote":      postStallHandoffRetryNote,
		"toolUnparsedRetryNote":          toolUnparsedRetryNote,
		"stallWriteFileRetryHint":        stallWriteFileRetryHint,
	}
	for name, body := range notes {
		if m := forbidden.FindString(body); m != "" {
			t.Errorf("retry note %s contains forbidden extension/app name %q: %q", name, m, body)
		}
	}

	apiconv.SetToolPromptRules(true)
	apiconv.SetToolPromptReminder(true)
	req := apiconv.ChatRequest{
		Model: "gemini-4-hydrogen",
		Messages: []apiconv.Message{
			{Role: "user", Content: json.RawMessage(`"step 1"`)},
			{Role: "assistant", Content: json.RawMessage(`"ok"`)},
			{Role: "user", Content: json.RawMessage(`"step 2"`)},
		},
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "read_file"}},
			{Type: "function", Function: &apiconv.ToolFunction{Name: "write_file"}},
			{Type: "function", Function: &apiconv.ToolFunction{Name: "run_command"}},
		},
	}
	prompt, _, _ := req.Prompt()
	if m := forbidden.FindString(prompt); m != "" {
		t.Errorf("generated tool prompt contains forbidden extension/app name %q", m)
	}
}

func TestK53_ToolRefusalThenInternalToolRetryRecoveredWithoutEmptyResponse(t *testing.T) {
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.cfg.InternalToolAction = "retry"
	srv.initialSilenceTimeoutOverride = -1
	srv.stallRetryBackoffOverride = time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			h.Text("Les outils locaux ne sont pas disponibles dans cet environnement.")
			return nil
		case 2:
			// During tool_refusal retry, Gemini invokes an internal tool -> cut with internalToolRetry=true
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
	joined := strings.Join(c.dataFrames, "\n")
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (tool_refusal -> internal_tool_retry -> ok)", attempts.Load())
	}
	if strings.Contains(joined, "upstream error") || strings.Contains(joined, "empty response") {
		t.Fatalf("unexpected ErrEmptyResponse in SSE: %s", joined)
	}
	if !strings.Contains(joined, "read_file") {
		t.Fatalf("expected final attempt to emit read_file; frames=%v", c.dataFrames)
	}
}

func TestK53_ToolRefusalThenSilentEmptyReplyRetries2To1(t *testing.T) {
	var logBuf bytes.Buffer
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.initialSilenceTimeoutOverride = -1
	srv.stallRetryBackoffOverride = time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			h.Text("Les outils locaux ne sont pas disponibles.")
			return nil
		case 2:
			// Silent empty reply at level 2 (no text, no error)
			return nil
		default:
			h.Text("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"ok.txt\",\"content\":\"done\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "write_file") {
		t.Fatalf("expected final attempt to emit write_file; frames=%v", c.dataFrames)
	}
	mu.Lock()
	gotLevels := append([]int(nil), levels...)
	mu.Unlock()
	if len(gotLevels) != 3 || gotLevels[0] != 4 || gotLevels[1] != 2 || gotLevels[2] != 1 {
		t.Fatalf("attempt levels = %v, want [4 2 1]", gotLevels)
	}
	if !strings.Contains(logBuf.String(), "empty_reply_retry") {
		t.Errorf("expected empty_reply_retry in logs: %s", logBuf.String())
	}
	dr := srv.diag.query(diagQuery{Kind: "incident", Type: "empty_reply", Window: "all", Limit: 10}, time.Now())
	if len(dr.Items) == 0 || !dr.Items[0].Recovered {
		t.Fatalf("expected recovered empty_reply incident in diagRing, got: %+v", dr.Items)
	}
}
