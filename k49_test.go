package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK49_WebUIv306ConnectButtonColorsToolsLayout(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	for _, want := range []string{
		"Lancer la connexion Google",
		`id="hdr-connect"`,
		`id="ar-connect"`,
		"/_ui/api/cookies/refresh",
		"triggerGoogleConnect",
		`const col = isErr ? "var(--bad)" : "var(--info)"`,
		"function cleanInternalToolLabel(",
		"tdb-toolbar",
		"tdb-summary-box",
		"tdb-stats-grid",
		"tdb-table-wrap",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK49_CookieConnectEndpointRecordsDiagIncident(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.cfg.UIEnabled = true
	srv.uiToken = "tok-k49"
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/_ui/api/cookies/refresh", bytes.NewBufferString(`{"source":"ui_connect"}`))
	req.Header.Set("X-GMINI-UI", "tok-k49")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /_ui/api/cookies/refresh: %v", err)
	}
	defer resp.Body.Close()

	// autoRefresh is not configured in test server -> 409 + incident "cookie_connect" recorded
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 409 or 503", resp.StatusCode)
	}

	dreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/_ui/api/diagnostics?kind=incident&window=all", nil)
	dreq.Header.Set("X-GMINI-UI", "tok-k49")
	dresp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatalf("GET /_ui/api/diagnostics: %v", err)
	}
	defer dresp.Body.Close()
	var dr diagResponse
	if err := json.NewDecoder(dresp.Body).Decode(&dr); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range dr.Items {
		if it.Type == "cookie_connect" && it.Service == "ui/auth" && it.Func == "handleUICookieRefreshRun" && !it.Recovered {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected cookie_connect incident in diagRing, got: %+v", dr.Items)
	}
}

func TestK49_DiagActivityPropagatesAndCleansInternalTool(t *testing.T) {
	if got := cleanInternalToolLabel("https://www.gstatic.com/images/icons/material/system/2x/search_gm_blue_24dp.png Google Search — Searching the web"); got != "Google Search — Searching the web" {
		t.Errorf("cleanInternalToolLabel(search) = %q", got)
	}
	if got := cleanInternalToolLabel("https://www.gstatic.com/foo/bar.png drive_mcp"); got != "Google Drive (drive_mcp)" {
		t.Errorf("cleanInternalToolLabel(drive_mcp) = %q", got)
	}
	if got := cleanInternalToolLabel("context_service_agent:get_context"); got != "context_service_agent:get_context" {
		t.Errorf("cleanInternalToolLabel(context_service_agent) = %q", got)
	}

	srv := newK42TestServer(t, nil)
	srv.cfg.UIEnabled = true
	srv.uiToken = "tok-k49"
	srv.cfg.InternalToolAction = "off"
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Opened()
		h.InternalTool("https://www.gstatic.com/search.png Searching the web")
		h.Text("ok")
		return nil
	}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	_ = readSSE(t, context.Background(), ts.URL, body)

	dr := srv.diag.query(diagQuery{Kind: "activity", Window: "all", Limit: 20}, time.Now())
	if len(dr.Items) == 0 {
		t.Fatal("expected activity item in diagRing")
	}
	if dr.Items[0].InternalTool != "Google Search — Searching the web" {
		t.Errorf("activity InternalTool = %q, want %q", dr.Items[0].InternalTool, "Google Search — Searching the web")
	}
}

func TestK49_ToolPromptRulesAndInitialSilence25s(t *testing.T) {
	apiconv.SetToolPromptRules(true)
	req := apiconv.ChatRequest{
		Model:    "gemini-4-hydrogen",
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"check repo"`)}},
		Tools: []apiconv.Tool{{
			Type:     "function",
			Function: &apiconv.ToolFunction{Name: "read_file"},
		}},
	}
	prompt, _, _ := req.Prompt()
	for _, want := range []string{"built-in assistant feature", "internal tool", "external connector", "prefix tool names with any namespace"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(prompt), "canva") {
		t.Errorf("prompt must NEVER mention canva/Canvas")
	}

	srv := newK42TestServer(t, nil)
	srv.initialSilenceTimeoutOverride = 0
	if got := srv.initialSilenceTimeout(); got != 25*time.Second {
		t.Errorf("initialSilenceTimeout() = %s, want 25s", got)
	}
}
