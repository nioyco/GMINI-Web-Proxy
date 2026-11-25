package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func newK57NeonServer(t *testing.T, noContentSec int, fn func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error) *Server {
	t.Helper()
	srv := newK42TestServer(t, fn)
	srv.cfg.NoContentSec = noContentSec
	srv.cfg.StreamToolsProgress = true
	srv.cfg.StreamThoughts = true
	srv.cfg.HeldStallRetries = 2
	srv.keepaliveOverride = 15 * time.Millisecond
	srv.noContentNoteOverride = 15 * time.Millisecond
	srv.stallRetryBackoffOverride = 5 * time.Millisecond
	srv.firstAttemptNoContentOverride = 60 * time.Millisecond
	srv.nonFinalStallNoContentOverride = 60 * time.Millisecond
	if noContentSec > 0 {
		srv.noContentTimeoutOverride = 60 * time.Millisecond
	}
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: "neon1234", ModeCode: 3, ThinkingLevels: []int{1, 2, 4}, DisplayName: "4 Neon"},
	})
	return srv
}

func doK57ToolChat(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{
		"model":"gemini-deeper-analysis",
		"stream":true,
		"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
		"messages":[{"role":"user","content":"read main.go"}]
	}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:8081"
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handler().ServeHTTP(w, req)
	return w
}

func TestK57_4NeonNoContentTimeoutFirstAttemptTriggersStepdown4To2AndSucceeds(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	var modes []int
	srv := newK57NeonServer(t, 240, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		modes = append(modes, p.Mode)
		attempt := len(levels)
		mu.Unlock()
		if attempt == 1 {
			for {
				if h.Heartbeat != nil {
					h.Heartbeat()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(15 * time.Millisecond):
				}
			}
		}
		if h.Thought != nil {
			h.Thought("Reading file")
		}
		if h.Text != nil {
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```")
		}
		return nil
	})

	w := doK57ToolChat(t, srv)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 2 || levels[0] != 4 || levels[1] != 2 {
		t.Fatalf("attempt levels = %v, want [4 2]", levels)
	}
	if modes[0] != 3 || modes[1] != 3 {
		t.Fatalf("attempt modes = %v, want [3 3]", modes)
	}
	if !strings.Contains(w.Body.String(), `"name":"read_file"`) {
		t.Fatalf("expected tool_call read_file in SSE output, got:\n%s", w.Body.String())
	}
	dr := srv.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 20}, time.Now())
	foundRecovered := false
	for _, it := range dr.Items {
		if it.Type == "no_content_timeout" && it.Recovered {
			foundRecovered = true
			break
		}
	}
	if !foundRecovered {
		t.Fatalf("expected recovered no_content_timeout incident, got %+v", dr.Items)
	}
}

func TestK57_4NeonDoubleNoContentTimeoutStepsDown2To1AndSucceeds(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	srv := newK57NeonServer(t, 240, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		attempt := len(levels)
		mu.Unlock()
		if attempt <= 2 {
			for {
				if h.Heartbeat != nil {
					h.Heartbeat()
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(15 * time.Millisecond):
				}
			}
		}
		if h.Text != nil {
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```")
		}
		return nil
	})

	w := doK57ToolChat(t, srv)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 3 || levels[0] != 4 || levels[1] != 2 || levels[2] != 1 {
		t.Fatalf("attempt levels = %v, want [4 2 1]", levels)
	}
	if !strings.Contains(w.Body.String(), `"name":"read_file"`) {
		t.Fatalf("expected tool_call read_file in SSE output, got:\n%s", w.Body.String())
	}
}

func TestK57_4NeonTripleNoContentTimeoutExhaustsRetriesTerminally(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	srv := newK57NeonServer(t, 240, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		mu.Unlock()
		for {
			if h.Heartbeat != nil {
				h.Heartbeat()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(15 * time.Millisecond):
			}
		}
	})

	w := doK57ToolChat(t, srv)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(levels) != 3 || levels[0] != 4 || levels[1] != 2 || levels[2] != 1 {
		t.Fatalf("attempt levels = %v, want [4 2 1]", levels)
	}
	if !strings.Contains(w.Body.String(), "GMINI") {
		t.Fatalf("expected terminal GMINI message in SSE output, got:\n%s", w.Body.String())
	}
	dr := srv.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 20}, time.Now())
	foundTerminal := false
	for _, it := range dr.Items {
		if it.Type == "no_content_timeout" && !it.Recovered {
			foundTerminal = true
			break
		}
	}
	if !foundTerminal {
		t.Fatalf("expected terminal (recovered=false) no_content_timeout incident, got %+v", dr.Items)
	}
}

func TestK57_FirstAttemptNoContentCapAndZeroSetting(t *testing.T) {
	if got := (&Server{}).firstAttemptNoContentCap(); got != 90*time.Second {
		t.Fatalf("firstAttemptNoContentCap() = %v, want 90s", got)
	}

	srv := newK57NeonServer(t, 0, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		if h.Heartbeat != nil {
			h.Heartbeat()
		}
		time.Sleep(80 * time.Millisecond)
		if h.Text != nil {
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```")
		}
		return nil
	})
	w := doK57ToolChat(t, srv)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"read_file"`) {
		t.Fatalf("NoContentSec=0 should not time out at 60ms: code=%d body=%s", w.Code, w.Body.String())
	}
}
