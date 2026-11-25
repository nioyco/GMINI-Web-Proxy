package main

import (
	"context"
	"encoding/json"
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

func TestK46_InternalToolRetryGlitch64StepsDownTo1(t *testing.T) {
	var mu sync.Mutex
	var seen []gemini.GenParams
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "high" // mode=1, level=2
	srv.cfg.InternalToolAction = "retry"
	srv.toolsDB = newToolsDB("", 100)
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			h.InternalTool("context_service_agent:get_context")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			h.Text("I can't use Workspace because required Gmail settings are off. Turn on these settings to continue.")
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
	if attempts.Load() != 3 || len(seen) != 3 {
		t.Fatalf("attempts = %d (seen=%d), want 3", attempts.Load(), len(seen))
	}
	if seen[1].Level != 2 || seen[2].Level != 1 {
		t.Errorf("level progression = %d -> %d -> %d, want 2 -> 2 -> 1", seen[0].Level, seen[1].Level, seen[2].Level)
	}
	if seen[2].TemporaryChat {
		t.Errorf("attempt 3 TemporaryChat = true, want false")
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "write_file") {
		t.Fatalf("expected attempt 3 to deliver write_file")
	}
}

func TestK46_InternalToolRetryStallRetriesOnce(t *testing.T) {
	var mu sync.Mutex
	var seen []gemini.GenParams
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "low" // mode=1, level=2
	srv.cfg.InternalToolAction = "retry"
	srv.toolsDB = newToolsDB("", 100)
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			h.InternalTool("retriever:expand_tools")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			h.Text("```tool_call\n{\"name\":\"read_file\",")
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
	if attempts.Load() != 3 || len(seen) != 3 {
		t.Fatalf("attempts = %d (seen=%d), want 3", attempts.Load(), len(seen))
	}
	if seen[2].Level != 1 {
		t.Errorf("attempt 3 Level = %d, want 1", seen[2].Level)
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
		t.Fatalf("expected attempt 3 to deliver read_file")
	}
}

func TestK46_ToolsDBRecordPermissionsFilterAndExport(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	if !strings.Contains(string(uiHTML), "BASE APPELS OUTILS GEMINI") {
		t.Errorf("ui.html missing BASE APPELS OUTILS GEMINI section")
	}

	dbPath := filepath.Join(t.TempDir(), "gemini_tools_db.jsonl")
	var attempts atomic.Int32
	srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		if n == 1 {
			h.InternalTool("drive_mcp")
			<-ctx.Done()
			return ctx.Err()
		}
		h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```")
		return nil
	})
	srv.cfg.InternalToolAction = "retry"
	srv.cfg.UIEnabled = true
	srv.uiToken = "tok-k46"
	srv.toolsDB = newToolsDB(dbPath, 100)

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	_ = readSSE(t, context.Background(), ts.URL, body)

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat %s: %v", dbPath, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("toolsDB file mode = %o, want 0600", mode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/_ui/api/tools-db?kind=internal_google&window=all", nil)
	req.Header.Set("X-GMINI-UI", "tok-k46")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /_ui/api/tools-db status = %d, want 200", resp.StatusCode)
	}
	var res toolsDBResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Items[0].ToolKind != "internal_google" || res.Items[0].Outcome != "recovered" {
		t.Fatalf("unexpected tools-db internal_google result: %+v", res)
	}

	reqCSV, _ := http.NewRequest(http.MethodGet, ts.URL+"/_ui/api/tools-db/export?format=csv&window=all", nil)
	reqCSV.Header.Set("X-GMINI-UI", "tok-k46")
	respCSV, err := http.DefaultClient.Do(reqCSV)
	if err != nil {
		t.Fatal(err)
	}
	defer respCSV.Body.Close()
	if respCSV.StatusCode != http.StatusOK || !strings.Contains(respCSV.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("CSV export status=%d ct=%q", respCSV.StatusCode, respCSV.Header.Get("Content-Type"))
	}

	// Verify idempotency of load()
	db2 := newToolsDB(dbPath, 100)
	q2 := db2.query(toolsDBQuery{Window: "all"}, time.Now())
	if q2.Total != 2 {
		t.Errorf("reloaded toolsDB total = %d, want 2 (internal_google + local)", q2.Total)
	}
}
