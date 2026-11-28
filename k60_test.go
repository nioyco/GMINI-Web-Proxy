package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func newK60HydrogenServer(t *testing.T, fn func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error) *Server {
	t.Helper()
	srv := newK42TestServer(t, fn)
	srv.cfg.StreamToolsProgress = true
	srv.cfg.StreamThoughts = true
	srv.cfg.CannedErrorRetry = "once"
	srv.cfg.CannedErrorRetrySec = 55
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.keepaliveOverride = 15 * time.Millisecond
	srv.stallRetryBackoffOverride = 5 * time.Millisecond
	srv.cannedRetryTimeoutOverride = 100 * time.Millisecond
	srv.client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "hyd1234567890abc", ModeCode: 1, ThinkingLevels: []int{1, 2, 4}, DisplayName: "4 Hydrogen"},
	})
	return srv
}

func doK60ToolChat(t *testing.T, srv *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{
		"model":"` + model + `",
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

func TestK60_DoubleCannedErrorStepsDown2To1AndSucceeds(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	srv := newK60HydrogenServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		attempt := len(levels)
		mu.Unlock()
		if attempt <= 2 {
			if h.ErrorMarker != nil {
				h.ErrorMarker()
			}
			if h.Text != nil {
				h.Text("Sorry, something went wrong.")
			}
			return nil
		}
		if h.Text != nil {
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```")
		}
		return nil
	})

	w := doK60ToolChat(t, srv, "gemini-everyday-help")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
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

func TestK60_TripleCannedErrorFailsTerminally(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	srv := newK60HydrogenServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		mu.Unlock()
		if h.ErrorMarker != nil {
			h.ErrorMarker()
		}
		if h.Text != nil {
			h.Text("Sorry, something went wrong.")
		}
		return nil
	})

	w := doK60ToolChat(t, srv, "gemini-everyday-help")
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
}

func TestK60_UnknownModelHTTP400RecordedInFlightsAndDiag(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: gemini.AutoModelHexID, DisplayName: "Auto"},
	})

	w := doK60ToolChat(t, srv, "gemini-everyday-help")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	hist := srv.flightHist.list()
	if len(hist) == 0 || hist[0].State != "Modèle inconnu (HTTP 400)" || hist[0].Model != "gemini-everyday-help" {
		t.Fatalf("flightHist[0] = %+v, want State=Modèle inconnu (HTTP 400), Model=gemini-everyday-help", hist)
	}

	dr := srv.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 20}, time.Now())
	found := false
	for _, it := range dr.Items {
		if it.Type == "unknown_model" && it.Detail == "http_400" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected unknown_model (http_400) incident in diag, got %+v", dr.Items)
	}
}
