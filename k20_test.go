package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK20_OpenSSEIdempotentNoSuperfluousWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sse1, ok := openSSE(sw)
	if !ok || sse1 == nil {
		t.Fatal("first openSSE failed")
	}
	if sw.status != http.StatusOK {
		t.Fatalf("sw.status = %d, want 200", sw.status)
	}
	// Second openSSE on the same statusWriter (e.g. after PrepareRequest onWait)
	// must reuse the open stream without calling WriteHeader again.
	sse2, ok := openSSE(sw)
	if !ok || sse2 == nil {
		t.Fatal("second openSSE failed")
	}
	if err := sse2.comment("ok"); err != nil {
		t.Fatalf("write after second openSSE failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), ": ok\n\n") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestK20_WaitNoteAt2sDoesNotDisableFallbackToGenerate(t *testing.T) {
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Opened()
		time.Sleep(60 * time.Millisecond) // let initial wait note fire
		return errors.New("temporary stream reset")
	})
	f.srv.initialWaitNoteOverride = 20 * time.Millisecond
	fallbackCalls := 0
	f.srv.generate = func(_ context.Context, _ gemini.GenParams) (string, error) {
		fallbackCalls++
		return "Réponse récupérée par le repli.", nil
	}
	c := readSSE(t, context.Background(), f.url, body(""))
	all := strings.Join(c.dataFrames, "\n")
	if fallbackCalls != 1 {
		t.Fatalf("fallbackCalls = %d, want 1 (2s wait note must not block fallback); all=%s", fallbackCalls, all)
	}
	if !strings.Contains(all, "Réponse récupérée par le repli.") {
		t.Fatalf("missing fallback reply: %s", all)
	}
}

func TestK20_SecondAttemptThoughtSimulationCaught(t *testing.T) {
	attempts := 0
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		attempts++
		h.Thought("Drafted a simulated counter-review report even though file tools are not available.")
		h.Text("Voici un rapport simulé sans appel d'outil.\n" + strings.Repeat("détail simulé\n", 40))
		return nil
	})
	c := readSSE(t, context.Background(), f.url, body(""))
	all := strings.Join(c.dataFrames, "\n")
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if strings.Contains(all, "détail simulé") {
		t.Fatalf("second simulated report leaked to client: %s", all)
	}
	if !strings.Contains(all, "Gemini a refusé ou simulé deux fois") {
		t.Fatalf("expected explicit GMINI refusal error, got: %s", all)
	}
}

func TestK20_FakeWriteDumpRetriedAndWarningFallbackAndReadWriteSplit(t *testing.T) {
	// 1. Real 23:02:02 shape: model dumps updated file without calling write_file -> held & retried
	attempts := 0
	var retryPrompt string
	f1 := progressServer(t, func(_ context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		attempts++
		if attempts == 1 {
			h.Text("Voici le fichier `/tmp/CONTRE_REVUE_INDEPENDANTE.md` entièrement mis à jour :\n\n```markdown\n" + strings.Repeat("ligne\n", 100) + "```")
			return nil
		}
		retryPrompt = p.Prompt
		h.Text("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"Path\":\"/tmp/CONTRE_REVUE_INDEPENDANTE.md\",\"Content\":\"ok\"}}\n```")
		return nil
	})
	reqBody := `{
		"model":"gemini-3.5-flash","stream":true,
		"messages":[{"role":"user","content":"Mets à jour /tmp/CONTRE_REVUE_INDEPENDANTE.md"}],
		"tools":[
			{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"Path":{"type":"string"}}}}},
			{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"Path":{"type":"string"},"Content":{"type":"string"}}}}}
		]
	}`
	c1 := readSSE(t, context.Background(), f1.url, reqBody)
	all1 := strings.Join(c1.dataFrames, "\n")
	if attempts != 2 || !strings.Contains(all1, "write_file") || !strings.Contains(retryPrompt, "WITHOUT emitting any tool_call block") {
		t.Fatalf("23:02:02 retry failed: attempts=%d retryPrompt=%q all=%s", attempts, retryPrompt, all1)
	}

	// 2. Real 23:07:03 shape: read_file + write_file emitted in same turn -> write_file deferred
	f2 := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"Path\":\"/tmp/a.go\"}}\n```\n```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"Path\":\"/tmp/CONTRE_REVUE.md\",\"Content\":\"x\"}}\n```")
		return nil
	})
	f2.srv.cfg.SplitReadWriteBatch = true
	c2 := readSSE(t, context.Background(), f2.url, reqBody)
	all2 := strings.Join(c2.dataFrames, "\n")
	if !strings.Contains(all2, `"name":"read_file"`) || strings.Contains(all2, `"name":"write_file"`) {
		t.Fatalf("23:07:03 split failed: all=%s", all2)
	}
	if f2.srv.stats.readWriteBatchSplit.Load() != 1 {
		t.Fatalf("readWriteBatchSplit = %d, want 1", f2.srv.stats.readWriteBatchSplit.Load())
	}

	// 3. Real 23:25:54 shape: if model still claims write without tool on 2nd attempt -> warning appended + audit note
	f3 := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("C'est fait ! J'ai bien exécuté l'écriture sur le disque. Le livrable est maintenant à jour sur votre machine !")
		return nil
	})
	f3.srv.cfg.ToolAuditNote = true
	c3 := readSSE(t, context.Background(), f3.url, reqBody)
	all3 := strings.Join(c3.dataFrames, "\n")
	if !strings.Contains(all3, "GMINI (vérification mécanique)") || !strings.Contains(all3, "Bilan vérifié des outils") {
		t.Fatalf("expected mechanical warning and audit note, got: %s", all3)
	}
	if f3.srv.stats.unverifiedClaim.Load() != 1 {
		t.Fatalf("unverifiedClaim = %d, want 1", f3.srv.stats.unverifiedClaim.Load())
	}
}

func TestK20_AutoRefreshStatusDeferredIdleOnDemand(t *testing.T) {
	cfg := testConfig()
	cfg.CookieRefreshCmd = "/bin/true"
	cfg.CookieAutoRefreshMin = 90
	cfg.CookieAutoRefreshEnabled = true
	cfg.CookieAutoRefreshEnabledSet = true
	cfg.Gemini.IdleQuiet = 10 * time.Minute

	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Round(0)
	ar := newCookieAutoRefresher(cfg, client, testLogger())
	ar.nowFn = func() time.Time { return now }
	ar.startedAt = now.Add(-130 * time.Minute) // > 90m interval while idle (no user activity)

	st := ar.Status()
	raw, _ := json.Marshal(st)
	if st.DeferredReason != "idle_on_demand" || st.NextRefreshInSec != nil {
		t.Fatalf("expected deferred_reason=idle_on_demand and nil NextRefreshInSec, got: %s", raw)
	}
}

func TestK20_TemporaryChatToggleAndPersist(t *testing.T) {
	client, cookieFile, stateFile := newCookieTestClient(t, "SAPISID=s1; __Secure-1PSIDTS=t1", 5*time.Minute)
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Gemini.Cookie = ""
	cfg.Gemini.CookieFile = cookieFile
	cfg.Gemini.StateFile = stateFile
	srv := newServer(cfg, client, testLogger())
	h := srv.handler()

	// Missing token -> 401
	w401 := uiDo(h, "POST", "/_ui/api/temporary-chat/toggle", "127.0.0.1:5000", "")
	if w401.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w401.Code)
	}

	// Toggle ON
	wOn := uiDo(h, "POST", "/_ui/api/temporary-chat/toggle", "127.0.0.1:5000", srv.uiToken)
	if wOn.Code != http.StatusOK || !strings.Contains(wOn.Body.String(), `"temporary_chat":true`) {
		t.Fatalf("toggle ON failed: code=%d body=%s", wOn.Code, wOn.Body.String())
	}
	if !client.TemporaryChat() || !client.UpstreamParams().TemporaryChat {
		t.Fatal("client.TemporaryChat() must be true after toggle ON")
	}

	// Restarted client restores persisted temporary_chat=true from .state.json
	client2, err := gemini.New(gemini.Config{CookieFile: cookieFile, StateFile: stateFile}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client2.RestoreState()
	if !client2.TemporaryChat() {
		t.Fatal("restarted client did not restore persisted temporary_chat=true")
	}

	// Toggle OFF
	wOff := uiDo(h, "POST", "/_ui/api/temporary-chat/toggle", "127.0.0.1:5000", srv.uiToken)
	if wOff.Code != http.StatusOK || !strings.Contains(wOff.Body.String(), `"temporary_chat":false`) {
		t.Fatalf("toggle OFF failed: code=%d body=%s", wOff.Code, wOff.Body.String())
	}
	if client.TemporaryChat() {
		t.Fatal("client.TemporaryChat() must be false after toggle OFF")
	}
}
