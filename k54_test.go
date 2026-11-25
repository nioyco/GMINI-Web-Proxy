package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK54_WebUIv308MergedViewsAndStickyExport(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	idxExport := strings.Index(html, `class="diag-export-sticky"`)
	idxDiagUnified := strings.Index(html, `id="diag-unified"`)
	if idxExport < 0 || idxDiagUnified < 0 || idxExport > idxDiagUnified {
		t.Fatalf("expected sticky export before diag-unified (export=%d, table=%d)", idxExport, idxDiagUnified)
	}
	for _, want := range []string{
		`id="diag-nature"`,
		`<option value="errors">Erreurs</option>`,
		`id="diag-unified"`,
		`id="demandes-unified"`,
		`class="full-table-wrap"`,
		"function diagActBadge(",
		"function diagTypeBadge(",
		`background:var(--infobg)`,
		`badge("ok",`,
		`badge("bad",`,
		`badge("warn",`,
		`badge("info",`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK54_CannedErrorAfterInternalToolRetryStepsDown2To1(t *testing.T) {
	var logBuf bytes.Buffer
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.cfg.InternalToolAction = "retry"
	srv.cfg.CannedErrorRetry = "once"
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
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			h.Text("Something went wrong. Please try again.")
			return nil
		default:
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
		t.Fatalf("expected final attempt to emit read_file; frames=%v", c.dataFrames)
	}
	mu.Lock()
	gotLevels := append([]int(nil), levels...)
	mu.Unlock()
	if len(gotLevels) != 3 || gotLevels[0] != 4 || gotLevels[1] != 2 || gotLevels[2] != 1 {
		t.Fatalf("attempt levels = %v, want [4 2 1]", gotLevels)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "upstream_canned_error") || !strings.Contains(logs, "after=internal_tool") || !strings.Contains(logs, "level_stepdown=2->1") {
		t.Errorf("expected canned_error 2->1 retry log after internal_tool, got: %s", logs)
	}
}

func TestK54_EmptyReplyAfterInternalToolRetryStepsDown2To1(t *testing.T) {
	var logBuf bytes.Buffer
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.cfg.InternalToolAction = "retry"
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
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		case 2:
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
}

func TestK54_ToolRefusalAfterInternalToolRetryHandsOff1To2(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "low"
	srv.cfg.InternalToolAction = "retry"
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
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			h.Text("Les outils locaux ne sont pas disponibles dans cet environnement.")
			return nil
		default:
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
		t.Fatalf("expected final attempt to emit read_file; frames=%v", c.dataFrames)
	}
	mu.Lock()
	gotLevels := append([]int(nil), levels...)
	mu.Unlock()
	if len(gotLevels) != 3 || gotLevels[0] != 1 || gotLevels[1] != 1 || gotLevels[2] != 2 {
		t.Fatalf("attempt levels = %v, want [1 1 2]", gotLevels)
	}
}
