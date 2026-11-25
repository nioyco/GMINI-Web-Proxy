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

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK52_DetectUnverifiedActionClaimLaunchedSuite(t *testing.T) {
	declared := []apiconv.DeclaredTool{{Name: "run_command"}}

	pos := []string{
		"I have launched the verification suite (`vue-tsc --noEmit`, `npm test`, `npm run check:licenses`, `npm run build`). Waiting for it to complete.",
		"I started the build and will report back.",
		"I have triggered the checks on the workspace.",
		"I kicked off the test suite.",
		"J'ai lancé la suite de vérification et j'attends le résultat.",
		"J'ai démarré le build.",
	}
	for _, s := range pos {
		kind, suspect := apiconv.DetectUnverifiedActionClaim(s, declared, apiconv.ToolTurnSummary{})
		if !suspect || kind != "command_without_tool" {
			t.Errorf("DetectUnverifiedActionClaim(%q) = (%q, %v), want (command_without_tool, true)", s, kind, suspect)
		}
		// Must not trigger when run_command actually ran since last user turn.
		if _, sup := apiconv.DetectUnverifiedActionClaim(s, declared, apiconv.ToolTurnSummary{CommandCount: 1}); sup {
			t.Errorf("expected no claim when CommandCount=1 for %q", s)
		}
	}

	neg := []string{
		"```\nI have launched the verification suite\n```",
		"`I have launched the verification suite`",
		"If you want, I can launch the verification suite.",
		"Si tu veux, je peux lancer la suite de vérification.",
	}
	for _, s := range neg {
		if kind, suspect := apiconv.DetectUnverifiedActionClaim(s, declared, apiconv.ToolTurnSummary{}); suspect {
			t.Errorf("expected no false positive for %q, got (%q, true)", s, kind)
		}
	}
}

func TestK52_InternalToolRetryRunsUnifiedGuardsOnUnverifiedClaim(t *testing.T) {
	var logBuf bytes.Buffer
	var attempts atomic.Int32
	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.InternalToolAction = "retry"
	srv.initialSilenceTimeoutOverride = -1
	srv.stallRetryBackoffOverride = time.Millisecond

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			h.InternalTool("Searching the web")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			h.Text("I have launched the verification suite (`vue-tsc --noEmit`, `npm test`, `npm run check:licenses`, `npm run build`). Waiting for it to complete.")
			return nil
		default:
			h.Text("```tool_call\n{\"name\":\"run_command\",\"arguments\":{\"CommandLine\":\"npm test\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"run checks"}],"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (internal_tool -> unverified_claim -> run_command)", attempts.Load())
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "npm test") {
		t.Fatalf("expected final attempt to emit run_command; frames=%v", c.dataFrames)
	}
	if !strings.Contains(logBuf.String(), "unverified_claim=command_without_tool") {
		t.Errorf("expected unverified_claim=command_without_tool in logs: %s", logBuf.String())
	}
}

func TestK52_InternalToolRetryCappedNoContentAndRetry2To1(t *testing.T) {
	var mu sync.Mutex
	var levels []int
	var attempts atomic.Int32

	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.cfg.InternalToolAction = "retry"
	srv.cfg.HeldStallRetries = 2
	srv.initialSilenceTimeoutOverride = -1
	srv.stallRetryBackoffOverride = time.Millisecond
	srv.nonFinalStallNoContentOverride = 35 * time.Millisecond
	srv.noContentTimeoutOverride = 400 * time.Millisecond

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
			for {
				h.Heartbeat()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(8 * time.Millisecond):
				}
			}
		default:
			h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```")
			return nil
		}
	}

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	start := time.Now()
	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"read a.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
	c := readSSE(t, context.Background(), ts.URL, body)
	elapsed := time.Since(start)

	if elapsed >= 250*time.Millisecond {
		t.Fatalf("elapsed = %s, expected capped no_content_timeout retry instead of full 400ms", elapsed)
	}
	if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
		t.Fatalf("expected final attempt to emit read_file; frames=%v", c.dataFrames)
	}
	mu.Lock()
	gotLevels := append([]int(nil), levels...)
	mu.Unlock()
	if len(gotLevels) != 3 || gotLevels[0] != 4 || gotLevels[1] != 2 || gotLevels[2] != 1 {
		t.Fatalf("attempt levels = %v, want [4 2 1]", gotLevels)
	}
}
