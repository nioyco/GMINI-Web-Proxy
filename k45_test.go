package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK45_InternalToolDefaultRetryLogAndDiagIncident(t *testing.T) {
	if prev, ok := os.LookupEnv("GEMINI_INTERNAL_TOOL_ACTION"); ok {
		t.Cleanup(func() { _ = os.Setenv("GEMINI_INTERNAL_TOOL_ACTION", prev) })
		_ = os.Unsetenv("GEMINI_INTERNAL_TOOL_ACTION")
	}
	cfg := loadConfig(t.TempDir() + "/.env")
	if cfg.InternalToolAction != "retry" {
		t.Fatalf("default InternalToolAction = %q, want \"retry\"", cfg.InternalToolAction)
	}

	var logBuf bytes.Buffer
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.InternalToolAction = "retry"
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		if n == 1 {
			h.InternalTool("drive_mcp")
			<-ctx.Done()
			return ctx.Err()
		}
		if !strings.Contains(p.Prompt, "Never invoke any built-in assistant feature") {
			t.Errorf("attempt 2 missing internalToolRetryNote")
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
		t.Fatalf("expected retry to deliver write_file")
	}

	logs := logBuf.String()
	for _, want := range []string{"internal_tool_detected", "tool=drive_mcp", "action=retry", "model=gemini-everyday-help", "prompt_bytes="} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in logs:\n%s", want, logs)
		}
	}

	resp := srv.diag.query(diagQuery{Window: "1h", Kind: "incident", Limit: 20}, time.Now())
	var found bool
	for _, it := range resp.Items {
		if it.Type == "internal_tool" && it.Detail == "retry" && it.Recovered && strings.Contains(it.Reason, "drive_mcp") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected internal_tool diag incident for drive_mcp, got: %+v", resp.Items)
	}
}

func TestK45_ToolRefusalFRAndEN53c3580f(t *testing.T) {
	yes := []string{
		"Je n'ai pas accès à votre environnement local ni à vos dépôts git.",
		"Ces fichiers ne sont pas exposés dans mes outils disponibles.",
		"Cet outil n'est pas exposé dans mes outils disponibles.",
		"I do not have direct access to your local environment or git repositories.",
		"Local files are not exposed in my available tools.",
	}
	for _, s := range yes {
		if !apiconv.IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = false, want true", s)
		}
	}
	no := []string{
		"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```",
		"<summary>Je n'ai pas accès à votre environnement local.</summary>",
	}
	for _, s := range no {
		if apiconv.IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = true, want false (guarded)", s)
		}
	}
}

func TestK45_AppendResumeHintOnTerminalError(t *testing.T) {
	srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Opened()
		h.Heartbeat()
		<-ctx.Done()
		return ctx.Err()
	})
	srv.noContentTimeoutOverride = 50 * time.Millisecond

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[` +
		`{"role":"user","content":"check a.go"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"ok"},` +
		`{"role":"user","content":"now edit"}` +
		`],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	content, _ := sseContentAndReasoning(t, c)
	if !strings.Contains(content, "Contexte de reprise GMINI") || !strings.Contains(content, "1 appel(s) d'outil déjà exécuté(s)") {
		t.Fatalf("expected resume hint in terminal error message, got: %q", content)
	}
}

func TestK45_WebUIv302ColorsAndStatusNoteCleared(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	if !strings.Contains(html, "isInfoNote") || !strings.Contains(html, "Demande envoyée") {
		t.Errorf("ui.html missing isInfoNote blue styling for informational notes")
	}

	srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		h.Opened()
		h.Heartbeat()
		time.Sleep(15 * time.Millisecond)
		h.Text("Réponse normale terminée.")
		return nil
	})
	srv.initialWaitNoteOverride = 5 * time.Millisecond

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	_ = readSSE(t, context.Background(), ts.URL, body)

	hist := srv.flightHist.list()
	if len(hist) == 0 {
		t.Fatalf("expected in_flight_history entry")
	}
	if hist[0].State != "Terminée" {
		t.Fatalf("State = %q, want Terminée", hist[0].State)
	}
	if hist[0].StatusNote != "" {
		t.Errorf("StatusNote = %q, want empty on Terminée", hist[0].StatusNote)
	}
}

func TestK45_QuotaPauseBlockedSetsModelAndExporterClassifies(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.cfg.PauseOnFallback = true
	srv.triggerQuotaPause(context.Background(), &gemini.AvailableModel{
		Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen",
	}, "f2994417f76a9d1e", "3.5 Flash", "other_model", "")

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}

	act := srv.diag.query(diagQuery{Window: "1h", Kind: "activity", Limit: 10}, time.Now())
	if len(act.Items) == 0 || act.Items[0].Model != "gemini-everyday-help" {
		t.Fatalf("diag activity Model = %+v, want gemini-everyday-help", act.Items)
	}

	py := `
import json, sys
sys.path.insert(0, "tools")
import export_gmini_diagnostics as exp
req = {"status": "429", "events": [{"msg": "quota_pause_blocked", "attrs": {"model": "gemini-everyday-help"}}], "trace": {}}
print(json.dumps(exp.classify(req)))
`
	out, err := exec.Command("python3", "-c", py).CombinedOutput()
	if err != nil {
		t.Fatalf("python3 failed: %v\n%s", err, out)
	}
	var got string
	if err := json.Unmarshal(out, &got); err != nil || got != "quota_pause_blocked" {
		t.Fatalf("exporter classify = %q (err=%v), want quota_pause_blocked", got, err)
	}
}
