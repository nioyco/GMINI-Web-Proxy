package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK48_FaviconEndpointAndUIv305(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	srv := newK42TestServer(t, nil)
	srv.cfg.UIEnabled = true
	srv.uiToken = "test-ui-token"
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatalf("GET /favicon.ico: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	svg := buf.String()
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "#3fb950") {
		t.Errorf("favicon SVG missing expected Antigravity/green-dot markup: %q", svg)
	}

	uiResp, err := http.Get(ts.URL + "/_ui")
	if err != nil {
		t.Fatalf("GET /_ui: %v", err)
	}
	defer uiResp.Body.Close()
	if csp := uiResp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:") {
		t.Errorf("CSP = %q, want img-src 'self' data:", csp)
	}

	html := string(uiHTML)
	for _, want := range []string{
		`<link id="favicon" rel="icon" type="image/svg+xml" href="/favicon.ico">`,
		"function updateFavicon(",
		"#3fb950",
		"#d29922",
		"#f85149",
		`class="diag-detail"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK48_ToolsDBTestIsolationAndFilter(t *testing.T) {
	t.Setenv("GEMINI_TOOLS_DB_FILE", "")
	if got := defaultToolsDBPath(); got != "" {
		t.Fatalf("defaultToolsDBPath() inside go test = %q, want empty", got)
	}

	p := filepath.Join(t.TempDir(), "tools.jsonl")
	lines := []ToolCallDBRecord{
		{
			TS:             time.Now().UTC().Format(time.RFC3339),
			BinaryVersion:  "GMINI-Web-Proxy.test",
			BinaryTag:      "test",
			ModelRequested: "gemini-4-hydrogen",
			Outcome:        "tool_calls_ok",
			ToolCount:      1,
		},
		{
			TS:             time.Now().UTC().Format(time.RFC3339),
			BinaryVersion:  "go-gemini-web2api-p9-0581e87-k48",
			BinaryTag:      "K48",
			ModelRequested: "gemini-4-hydrogen",
			Outcome:        "tool_calls_ok",
			ToolCount:      2,
		},
	}
	var raw bytes.Buffer
	for _, rec := range lines {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(b)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(p, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}

	db := &toolsDB{path: p, max: 100}
	db.loadWithFilter(true)
	resp := db.query(toolsDBQuery{Window: "all", Limit: 50}, time.Now().UTC())
	if len(resp.Items) != 1 {
		t.Fatalf("len(resp.Items) = %d, want 1 (test record filtered)", len(resp.Items))
	}
	if resp.Items[0].BinaryTag != "K48" {
		t.Errorf("kept item BinaryTag = %q, want K48", resp.Items[0].BinaryTag)
	}
}

func TestK48_InternalToolRetryStepdown4To2AndStallTo1(t *testing.T) {
	var logBuf bytes.Buffer
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.InternalToolAction = "retry"
	srv.cfg.HeldStallRetries = 1
	srv.stallRetryBackoffOverride = time.Millisecond
	srv.heldStallOverride = 30 * time.Millisecond
	srv.heldStallFastOverride = 20 * time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		levels = append(levels, p.Level)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			// Level 4 triggers internal tool call -> corrective retry must step down 4->2.
			h.InternalTool("drive_mcp")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			// Level 2 stalls with 0 transmitted bytes (held open fence) -> stall-retry must step down 2->1.
			h.Text("```tool_call\n{\"name\":\"read_file\"")
			<-ctx.Done()
			return ctx.Err()
		default:
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.txt"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
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
	if !strings.Contains(logs, "internal_tool_retry_stepdown") || !strings.Contains(logs, "level_stepdown=4->2") {
		t.Errorf("logs missing 4->2 stepdown: %s", logs)
	}
	if !strings.Contains(logs, "level_stepdown=2->1") {
		t.Errorf("logs missing 2->1 stall stepdown: %s", logs)
	}
}
