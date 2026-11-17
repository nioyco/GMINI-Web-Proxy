package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
)

// K17 S2: Explicit rule against built-in code execution and code_reference blocks
// appears in both K8 tool rules and B3 reminder only when a command tool is declared,
// and is controlled by GEMINI_NO_INTERNAL_CODE_EXEC_RULE (default on).
func TestK17_S2_NoInternalCodeExecRule(t *testing.T) {
	apiconv.SetToolPromptRules(true)
	apiconv.SetToolPromptReminder(true)
	apiconv.SetNoInternalCodeExecRule(true)
	defer apiconv.SetNoInternalCodeExecRule(true)

	// 1. Declared run_command -> rule in K8 and B3.
	reqRunCmd := apiconv.ChatRequest{
		Messages: []apiconv.Message{
			{Role: "user", Content: json.RawMessage(`"ctx\n<USER_REQUEST>\nlist files\n</USER_REQUEST>"`)},
		},
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "run_command", Parameters: json.RawMessage(`{"type":"object","properties":{"CommandLine":{"type":"string"}},"required":["CommandLine"]}`)}},
		},
	}
	p, _, _ := reqRunCmd.Prompt()
	if !strings.Contains(p, `Never use built-in code execution or write a code_reference block; to run any command, call the provided "run_command" tool directly.`) {
		t.Fatalf("missing K8 code_reference rule for run_command: %s", p)
	}
	if !strings.Contains(p, `NEVER use built-in code execution or write a code_reference block; call "run_command" directly.`) {
		t.Fatalf("missing B3 reminder code_reference rule for run_command: %s", p)
	}

	// 2. Equivalent command tool detected by schema -> rule uses its name.
	reqEquiv := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}} ,
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "exec_shell", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
		},
	}
	pEquiv, _, _ := reqEquiv.Prompt()
	if !strings.Contains(pEquiv, `call the provided "exec_shell" tool directly`) {
		t.Fatalf("missing K8 rule for schema-detected command tool: %s", pEquiv)
	}
	if !strings.Contains(pEquiv, `call "exec_shell" directly`) {
		t.Fatalf("missing B3 rule for schema-detected command tool: %s", pEquiv)
	}

	// 3. No command tool declared -> rule absent.
	reqNoCmd := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}} ,
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"Path":{"type":"string"}}}`)}},
		},
	}
	pNoCmd, _, _ := reqNoCmd.Prompt()
	if strings.Contains(pNoCmd, "code_reference") {
		t.Fatalf("unexpected code_reference rule without command tool: %s", pNoCmd)
	}

	// 4. Toggle off -> rule absent even with run_command.
	apiconv.SetNoInternalCodeExecRule(false)
	pOff, _, _ := reqRunCmd.Prompt()
	if strings.Contains(pOff, "code_reference") {
		t.Fatalf("unexpected code_reference rule when disabled: %s", pOff)
	}

	// 5. Env var GEMINI_NO_INTERNAL_CODE_EXEC_RULE=false loads false; default true.
	if cfg := loadConfig(""); !cfg.NoInternalCodeExecRule {
		t.Fatal("default NoInternalCodeExecRule must be true")
	}
	t.Setenv("GEMINI_NO_INTERNAL_CODE_EXEC_RULE", "false")
	if cfg := loadConfig(""); cfg.NoInternalCodeExecRule {
		t.Fatal("GEMINI_NO_INTERNAL_CODE_EXEC_RULE=false must disable rule")
	}
}

// fakeStreamBody24 builds a single wrb.fr streaming line with text, body[24][0], and identity.
func fakeStreamBody24(text string, body24Tool string) string {
	inner := make([]any, 43)
	inner[4] = []any{[]any{"rc_1", []any{text}}}
	if body24Tool != "" {
		inner[24] = []any{[]any{[]any{body24Tool, nil, 2}}}
	}
	inner[39] = "e6fa609c3fa255c0"
	inner[42] = "3.1 Pro"
	ib, _ := json.Marshal(inner)
	outer := []any{[]any{"wrb.fr", nil, string(ib)}}
	ob, _ := json.Marshal(outer)
	return ")]}'\n\n" + string(ob) + "\n"
}

// K17 S3: GEMINI_CODE_REF_FASTPATH=on converts a single complete code_reference block
// into a command tool call and cancels the upstream pause when a command tool is
// declared and body[24][0] names that same tool.
func TestK17_S3_CodeRefFastpath(t *testing.T) {
	runCmdChatBody := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"check status"}],"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"},"Cwd":{"type":"string"},"WaitMsBeforeAsync":{"type":"integer"},"SafeToAutoRun":{"type":"boolean"}},"required":["CommandLine","Cwd","WaitMsBeforeAsync"]}}}]}`

	t.Run("on: block followed by long pause -> fastpath delivers tool_call and cancels upstream", func(t *testing.T) {
		var upstreamCancelled atomic.Bool
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			pr, pw := io.Pipe()
			go func() {
				pw.Write([]byte(fakeStreamBody24("```bash?code_reference&code_event_index=1\ngit status\n```\n", "run_command")))
				<-r.Context().Done()
				upstreamCancelled.Store(true)
				pw.Close()
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.AllowNoAuth = true
		cfg.UIEnabled = true
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.CodeRefFastpath = true
		cfg.SSEKeepaliveSec = 1
		var logBuf lockedBuf
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		s := newServer(cfg, client, logger)
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		start := time.Now()
		c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
		if time.Since(start) > 2*time.Second {
			t.Fatalf("fastpath took too long: %v", time.Since(start))
		}
		if !upstreamCancelled.Load() {
			t.Fatal("expected upstream request to be cancelled by fastpath")
		}
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, `"name":"run_command"`) || !strings.Contains(joined, `git status`) {
			t.Fatalf("expected converted run_command tool_call, got: %s", joined)
		}
		if !strings.Contains(joined, `\"Cwd\":\".\"`) || !strings.Contains(joined, `\"WaitMsBeforeAsync\":5000`) {
			t.Fatalf("expected required schema defaults in tool_call arguments, got: %s", joined)
		}
		logStr := logBuf.String()
		if !strings.Contains(logStr, "code_ref_fastpath") || strings.Contains(logStr, "git status") {
			t.Fatalf("log must record code_ref_fastpath without command content: %s", logStr)
		}

		// /_status counter and UI page
		stRec := uiDo(s.handler(), http.MethodGet, "/_status", "127.0.0.1:12345", "")
		var st statusResponse
		if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if !st.CodeRefFastpath || st.Counters["code_ref_fastpath"] != 1 {
			t.Fatalf("status fastpath=%v counter=%d, want true / 1", st.CodeRefFastpath, st.Counters["code_ref_fastpath"])
		}
		uiRec := uiDo(s.handler(), http.MethodGet, "/_ui", "127.0.0.1:12345", "")
		if !strings.Contains(uiRec.Body.String(), "code_ref_fastpath") {
			t.Fatalf("UI missing code_ref_fastpath tile")
		}
	})

	t.Run("on: no command tool declared -> nothing changes", func(t *testing.T) {
		noCmdBody := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			body := fakeStreamBody24("```bash?code_reference&code_event_index=1\nls -la\n```\n", "run_command")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.CodeRefFastpath = true
		s := newServer(cfg, client, testLogger())
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, noCmdBody)
		joined := strings.Join(c.dataFrames, "\n")
		if strings.Contains(joined, `"tool_calls"`) {
			t.Fatalf("must not fastpath without declared command tool: %s", joined)
		}
		if s.stats.codeRefFastpath.Load() != 0 {
			t.Fatalf("counter = %d, want 0", s.stats.codeRefFastpath.Load())
		}
	})

	t.Run("on: multi-command block -> nothing changes", func(t *testing.T) {
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			body := fakeStreamBody24("```bash?code_reference&code_event_index=1\nls -la\npwd\n```\n", "run_command")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.CodeRefFastpath = true
		s := newServer(cfg, client, testLogger())
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
		joined := strings.Join(c.dataFrames, "\n")
		if strings.Contains(joined, `"tool_calls"`) {
			t.Fatalf("must not fastpath multi-command block: %s", joined)
		}
		if s.stats.codeRefFastpath.Load() != 0 {
			t.Fatalf("counter = %d, want 0", s.stats.codeRefFastpath.Load())
		}
	})

	t.Run("off (default): waits for normal tool_call", func(t *testing.T) {
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			body := fakeStreamBody24("```bash?code_reference&code_event_index=1\nls -la\n```\n```tool_call\n{\"name\":\"run_command\",\"arguments\":{\"CommandLine\":\"pwd\"}}\n```\n", "run_command")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		// CodeRefFastpath is false by default
		if cfg.CodeRefFastpath {
			t.Fatal("CodeRefFastpath default must be false")
		}
		s := newServer(cfg, client, testLogger())
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
		joined := strings.Join(c.dataFrames, "\n")
		if !strings.Contains(joined, `pwd`) || strings.Contains(joined, `ls -la`) {
			t.Fatalf("in off mode, expected normal tool_call pwd (and stripped code_ref), got: %s", joined)
		}
		if s.stats.codeRefFastpath.Load() != 0 {
			t.Fatalf("counter = %d, want 0", s.stats.codeRefFastpath.Load())
		}
	})
}

// K17 S4: When a code_reference block is detected and the next frame takes longer than
// 10s (overridden in test), emit a single GMINI note in the stream, update in-flight
// status_note, and record per-model 5h count & median pause on the UI state.
func TestK17_S4_CodeRefWaitNoteAndModelStats(t *testing.T) {
	runCmdChatBody := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"check"}],"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"CommandLine":{"type":"string"}}}}}]}`

	noteSeenInFlight := make(chan string, 1)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			pw.Write([]byte(fakeStreamBody24("```bash?code_reference&code_event_index=1\nls -la\n```\n", "run_command")))
			time.Sleep(220 * time.Millisecond)
			pw.Write([]byte(fakeStreamBody24("```bash?code_reference&code_event_index=1\nls -la\n```\n```tool_call\n{\"name\":\"run_command\",\"arguments\":{\"CommandLine\":\"ls -la\"}}\n```\n", "run_command")))
			pw.Close()
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.AllowNoAuth = true
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	s.codeRefWaitOverride = 80 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	// Poll /_ui/api/state during the pause to verify in-flight status_note.
	go func() {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			rec := uiDo(s.handler(), http.MethodGet, "/_ui/api/state", "127.0.0.1:12345", s.uiToken)
			var st struct {
				InFlight []InFlightRequest `json:"in_flight"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &st) == nil {
				for _, req := range st.InFlight {
					if strings.Contains(req.StatusNote, "Gemini tente son outil interne d'exécution de code") {
						select {
						case noteSeenInFlight <- req.StatusNote:
						default:
						}
						return
					}
				}
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	c := readSSE(t, context.Background(), ts.URL, runCmdChatBody)
	joined := strings.Join(c.dataFrames, "\n")
	wantMsg := "Gemini tente son outil interne d'exécution de code ; l'appel d'outil suit en général sous 10 à 60 s."
	if n := strings.Count(joined, wantMsg); n != 1 {
		t.Fatalf("expected GMINI code_ref wait note once in stream, got %d in: %s", n, joined)
	}
	if !strings.Contains(joined, "GMINI (pas le modèle") {
		t.Fatalf("expected K13 GMINI note format, got: %s", joined)
	}
	select {
	case gotNote := <-noteSeenInFlight:
		if !strings.Contains(gotNote, "exécution de code") {
			t.Fatalf("unexpected in-flight note: %s", gotNote)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("did not observe code_reference wait note in in-flight state")
	}

	// Verify per-model 5h count & median pause in /_ui/api/state.
	stateRec := uiDo(s.handler(), http.MethodGet, "/_ui/api/state", "127.0.0.1:12345", s.uiToken)
	var uiSt struct {
		Models []uiModel `json:"models"`
	}
	if err := json.Unmarshal(stateRec.Body.Bytes(), &uiSt); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range uiSt.Models {
		if m.Name == "gemini-3.1-pro" {
			found = true
			if m.CodeRef5h != 1 {
				t.Fatalf("gemini-3.1-pro code_ref_5h = %d, want 1", m.CodeRef5h)
			}
			if m.MedianCodeRefPauseMS < 150 {
				t.Fatalf("gemini-3.1-pro median_code_ref_pause_ms = %d, want >= 150", m.MedianCodeRefPauseMS)
			}
		}
	}
	if !found {
		t.Fatal("gemini-3.1-pro missing from /_ui/api/state models")
	}

	uiRec := uiDo(s.handler(), http.MethodGet, "/_ui", "127.0.0.1:12345", "")
	if !strings.Contains(uiRec.Body.String(), "code_reference (5 h)") {
		t.Fatal("UI page missing code_reference (5 h) column")
	}
}
