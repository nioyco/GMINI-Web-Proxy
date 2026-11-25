package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK43_NoForcedTemporaryChatOnRetries(t *testing.T) {
	pFalse := gemini.GenParams{Mode: 1, Level: 4, TemporaryChat: false, Prompt: "small"}
	out, stepped := prepareRecoveryParams(pFalse)
	if out.TemporaryChat || !stepped || out.Level != 2 {
		t.Fatalf("prepareRecoveryParams(false) = %+v (stepped=%v), want TemporaryChat=false Level=2 stepped=true", out, stepped)
	}

	pTrue := gemini.GenParams{Mode: 1, Level: 4, TemporaryChat: true, Prompt: "small"}
	outTrue, _ := prepareRecoveryParams(pTrue)
	if !outTrue.TemporaryChat {
		t.Fatalf("prepareRecoveryParams(true) must preserve TemporaryChat=true")
	}

	srv := newK42TestServer(t, nil)
	stallOut, ok := srv.prepareStallRetry(context.Background(), nil, "cid", 1, "gemini-everyday-help", false, pFalse, streamToolsResult{heldBytes: 10}, 1, 2)
	if !ok || stallOut.TemporaryChat || stallOut.Level != 2 {
		t.Fatalf("prepareStallRetry(1/2) = %+v (ok=%v), want TemporaryChat=false Level=2", stallOut, ok)
	}
}

func TestK43_OptimalStepdownAndToolRefusalLevel1To2(t *testing.T) {
	// 1. postStallHandoffRetryParams: 4 -> 2
	handoff := postStallHandoffRetryParams(gemini.GenParams{Mode: 1, Level: 4, Prompt: "hi"})
	if handoff.Level != 2 || handoff.TemporaryChat {
		t.Errorf("postStallHandoffRetryParams = Level:%d TempChat:%v, want Level:2 TempChat:false", handoff.Level, handoff.TemporaryChat)
	}

	// 2. toolRefusalRetryParams: 1 -> 2, 2 stays 2
	ref1 := toolRefusalRetryParams(gemini.GenParams{Mode: 1, Level: 1, Prompt: "hi"})
	if ref1.Level != 2 {
		t.Errorf("toolRefusalRetryParams(level=1) = %d, want 2", ref1.Level)
	}
	ref2 := toolRefusalRetryParams(gemini.GenParams{Mode: 1, Level: 2, Prompt: "hi"})
	if ref2.Level != 2 {
		t.Errorf("toolRefusalRetryParams(level=2) = %d, want 2", ref2.Level)
	}

	// 3. prepareStallRetry: attempt 1 -> level 2 (any size), attempt 2 -> level 1
	srv := newK42TestServer(t, nil)
	a1, ok := srv.prepareStallRetry(context.Background(), nil, "cid", 1, "gemini-everyday-help", false, gemini.GenParams{Mode: 1, Level: 4, Prompt: "tiny"}, streamToolsResult{}, 1, 2)
	if !ok || a1.Level != 2 {
		t.Fatalf("stall attempt 1 = Level:%d ok:%v, want 2", a1.Level, ok)
	}
	a2, ok := srv.prepareStallRetry(context.Background(), nil, "cid", 1, "gemini-everyday-help", false, a1, streamToolsResult{}, 2, 2)
	if !ok || a2.Level != 1 {
		t.Fatalf("stall attempt 2 = Level:%d ok:%v, want 1", a2.Level, ok)
	}
}

func TestK43_InitialSilence35sAndChainsIntoNoContentCap(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.cfg.DefaultThinkingLevel = "medium"
	srv.initialSilenceTimeoutOverride = 0
	if got := srv.initialSilenceTimeout(); got != 25*time.Second {
		t.Fatalf("default initialSilenceTimeout() = %s, want 25s", got)
	}

	var mu sync.Mutex
	var seen []gemini.GenParams
	var attempts atomic.Int32

	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			// Initial attempt: total silence -> hits initialSilenceTimeout (30ms)
			<-ctx.Done()
			return ctx.Err()
		case 2:
			// initial_silence_retry: heartbeats only -> must hit short 60ms cap and chain to attempt 3!
			for {
				h.Heartbeat()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		default:
			// Chained retry after initial_silence_retry + non-final no_content_timeout succeeds!
			h.Text("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"ok.txt\",\"content\":\"done\"}}\n```")
			return nil
		}
	}
	srv.initialSilenceTimeoutOverride = 30 * time.Millisecond
	srv.nonFinalStallNoContentOverride = 60 * time.Millisecond
	srv.noContentTimeoutOverride = 400 * time.Millisecond

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	start := time.Now()
	c := readSSE(t, context.Background(), ts.URL, body)
	elapsed := time.Since(start)

	if attempts.Load() != 3 || len(seen) != 3 {
		t.Fatalf("attempts = %d (seen=%d), want 3 (initial_silence -> capped no_content -> recovered)", attempts.Load(), len(seen))
	}
	if elapsed >= 350*time.Millisecond {
		t.Fatalf("elapsed = %s, expected capped noContentTimeout after initial_silence_retry", elapsed)
	}
	if seen[0].Level != 4 || seen[1].Level != 2 || seen[2].Level != 1 {
		t.Errorf("level progression = %d -> %d -> %d, want 4 -> 2 -> 1", seen[0].Level, seen[1].Level, seen[2].Level)
	}
	if seen[0].TemporaryChat || seen[1].TemporaryChat || seen[2].TemporaryChat {
		t.Errorf("TemporaryChat progression = %v/%v/%v, want false/false/false", seen[0].TemporaryChat, seen[1].TemporaryChat, seen[2].TemporaryChat)
	}
	all := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(all, "write_file") {
		t.Fatalf("expected chained retry to deliver write_file, got: %s", all)
	}
}

func TestK43_ExporterIgnoresInternalCancelCauses(t *testing.T) {
	py := `
import json, sys
sys.path.insert(0, "tools")
import export_gmini_diagnostics as exp

r1 = {
    "status": "200",
    "tools": 1,
    "events": [
        {"msg": "client_cancelled", "attrs": {"cause": "initial_silence"}},
        {"msg": "client_cancelled", "attrs": {"cause": "canned_retry_timeout"}},
    ],
    "trace": {"tool_call_blocks": 1},
}
r2 = {
    "status": "200",
    "tools": 1,
    "events": [
        {"msg": "client_cancelled", "attrs": {"cause": "context_canceled"}},
    ],
    "trace": {},
}
print(json.dumps([exp.classify(r1), exp.classify(r2)]))
`
	out, err := exec.Command("python3", "-c", py).CombinedOutput()
	if err != nil {
		t.Fatalf("python3 failed: %v\n%s", err, out)
	}
	var res []string
	if err := json.Unmarshal(out, &res); err != nil || len(res) != 2 {
		t.Fatalf("unexpected output: %s (err=%v)", out, err)
	}
	if res[0] != "tool_calls_ok" {
		t.Errorf("r1 outcome = %q, want tool_calls_ok", res[0])
	}
	if res[1] != "client_cancelled" {
		t.Errorf("r2 outcome = %q, want client_cancelled", res[1])
	}
}
