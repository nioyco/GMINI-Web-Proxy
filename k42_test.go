package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func newK42TestServer(t *testing.T, streamFn func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error) *Server {
	t.Helper()
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "once"
	cfg.HeldStallRetries = 2
	cfg.DefaultThinkingLevel = "max"
	cfg.ThinkingModes = parseModeCodes(defaultThinkingModeCodes)

	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen", ModeCode: 1},
		{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0", Description: "4 Neon", ModeCode: 1},
		{Name: "gemini-3.5-flash", ModelID: "f2994417f76a9d1e", Description: "3.5 Flash", ModeCode: 0},
	})

	srv := newServer(cfg, client, testLogger())
	srv.diag = newDiagRing(200, "")
	srv.streamEvents = streamFn
	srv.keepaliveOverride = 20 * time.Millisecond
	srv.stallRetryBackoffOverride = 5 * time.Millisecond
	srv.heldStallOverride = 50 * time.Millisecond
	srv.heldStallFastOverride = 25 * time.Millisecond
	srv.noContentNoteOverride = 10 * time.Second
	srv.initialWaitNoteOverride = 10 * time.Second
	srv.initialSilenceTimeoutOverride = -1
	return srv
}

func TestK42_PrepareRecoveryParamsStepdownAnyPromptSize(t *testing.T) {
	cases := []struct {
		name      string
		in        gemini.GenParams
		wantLevel int
		wantStep  bool
	}{
		{
			name:      "small_prompt_hydrogen_steps_down",
			in:        gemini.GenParams{Mode: 1, Level: 4, Prompt: "hello"},
			wantLevel: 2,
			wantStep:  true,
		},
		{
			name:      "empty_prompt_hydrogen_steps_down",
			in:        gemini.GenParams{Mode: 1, Level: 4, Prompt: ""},
			wantLevel: 2,
			wantStep:  true,
		},
		{
			name:      "large_prompt_hydrogen_steps_down",
			in:        gemini.GenParams{Mode: 1, Level: 4, Prompt: strings.Repeat("x", 200*1024)},
			wantLevel: 2,
			wantStep:  true,
		},
		{
			name:      "already_level_1_no_stepdown",
			in:        gemini.GenParams{Mode: 1, Level: 1, Prompt: "hello"},
			wantLevel: 1,
			wantStep:  false,
		},
		{
			name:      "non_mode_1_also_steps_down_k57",
			in:        gemini.GenParams{Mode: 3, Level: 4, Prompt: "hello"},
			wantLevel: 2,
			wantStep:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, stepped := prepareRecoveryParams(tc.in)
			if out.TemporaryChat != tc.in.TemporaryChat {
				t.Errorf("TemporaryChat = %v, want %v (preserved)", out.TemporaryChat, tc.in.TemporaryChat)
			}
			if out.Level != tc.wantLevel {
				t.Errorf("Level = %d, want %d", out.Level, tc.wantLevel)
			}
			if stepped != tc.wantStep {
				t.Errorf("stepped = %v, want %v", stepped, tc.wantStep)
			}
		})
	}
}

func TestK42_HeldStallRetryNonFinalNoContentCap(t *testing.T) {
	srv := newK42TestServer(t, nil)
	if got := srv.stallRetryNoContentCap(1, 2); got != nonFinalStallRetryNoContentTimeout {
		t.Fatalf("cap(1, 2) = %s, want %s", got, nonFinalStallRetryNoContentTimeout)
	}
	if got := srv.stallRetryNoContentCap(2, 2); got != 0 {
		t.Fatalf("cap(2, 2) = %s, want 0 (full timeout on final attempt)", got)
	}

	var attempts atomic.Int32
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		n := attempts.Add(1)
		h.Opened()
		switch n {
		case 1:
			// Initial attempt stalls on an open tool_call fence.
			h.Text("```tool_call\n{\"name\":\"write_file\"")
			<-ctx.Done()
			return ctx.Err()
		case 2:
			// Non-final retry 1/2 emits only heartbeats -> must hit short cap, not 240s/500ms.
			for {
				h.Heartbeat()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		default:
			// Final retry 2/2 succeeds with a valid tool_call.
			h.Text("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"ok.txt\",\"content\":\"done\"}}\n```")
			return nil
		}
	}
	srv.nonFinalStallNoContentOverride = 60 * time.Millisecond
	srv.noContentTimeoutOverride = 400 * time.Millisecond

	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"write ok.txt"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
	start := time.Now()
	c := readSSE(t, context.Background(), ts.URL, body)
	elapsed := time.Since(start)

	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (initial stall + 1/2 short no-content + 2/2 success)", attempts.Load())
	}
	if elapsed >= 350*time.Millisecond {
		t.Fatalf("elapsed = %s, expected non-final 1/2 to cap well below full noContentTimeout (400ms)", elapsed)
	}
	all := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(all, "write_file") {
		t.Fatalf("expected attempt 2/2 to deliver write_file, got: %s", all)
	}
}

func TestK42_DiagIncidentsAfterRefusalOrGlitch(t *testing.T) {
	t.Run("canned_error_after_tool_refusal_records_incidents", func(t *testing.T) {
		var attempts atomic.Int32
		srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
			n := attempts.Add(1)
			h.Opened()
			switch n {
			case 1:
				h.Text("Le quota d'appels d'outils pour ce tour est épuisé (0 appel restant).")
			default:
				h.Text("I seem to be encountering an error. Can I try something else for you?")
			}
			return nil
		})
		srv.cannedRetryTimeoutOverride = 200 * time.Millisecond

		ts := httptest.NewServer(srv.handler())
		defer ts.Close()

		body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
		_ = readSSE(t, context.Background(), ts.URL, body)

		resp := srv.diag.query(diagQuery{Window: "1h", Kind: "incident", Limit: 50}, time.Now())
		var gotRetryOnce, gotReported bool
		for _, it := range resp.Items {
			if it.Type == "canned_error" && it.Detail == "retry_once" {
				gotRetryOnce = true
			}
			if it.Type == "canned_error" && it.Detail == "reported" {
				gotReported = true
			}
		}
		if !gotRetryOnce || !gotReported {
			t.Fatalf("expected canned_error retry_once and reported incidents, got: %+v", resp.Items)
		}
	})

	t.Run("tool_refusal_after_glitch_records_incident", func(t *testing.T) {
		var attempts atomic.Int32
		srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
			n := attempts.Add(1)
			h.Opened()
			switch n {
			case 1:
				h.Text("I can't use Workspace because required Gmail settings are off. Turn on these settings to continue.")
			case 2:
				h.Text("Le quota d'appels d'outils pour ce tour est épuisé (0 appel restant).")
			default:
				h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.txt\"}}\n```")
			}
			return nil
		})

		ts := httptest.NewServer(srv.handler())
		defer ts.Close()

		body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
		c := readSSE(t, context.Background(), ts.URL, body)
		if !strings.Contains(strings.Join(c.dataFrames, "\n"), "read_file") {
			t.Fatalf("expected read_file after glitch->tool_refusal recovery")
		}

		resp := srv.diag.query(diagQuery{Window: "1h", Kind: "incident", Limit: 50}, time.Now())
		var gotGlitch, gotRefusal bool
		for _, it := range resp.Items {
			if it.Type == "glitch" && it.Detail == "retry" {
				gotGlitch = true
			}
			if it.Type == "tool_refusal" && it.Detail == "retry" {
				gotRefusal = true
			}
		}
		if !gotGlitch || !gotRefusal {
			t.Fatalf("expected glitch and tool_refusal retry incidents, got: %+v", resp.Items)
		}
	})
}

func TestK42_TerminalErrorStateInFlightHistoryAndDiagRing(t *testing.T) {
	cases := []struct {
		name        string
		streamFn    func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error
		setup       func(s *Server)
		wantState   string
		wantOutcome string
		wantReason  string
	}{
		{
			name: "no_content_timeout",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Heartbeat()
				<-ctx.Done()
				return ctx.Err()
			},
			setup: func(s *Server) {
				s.noContentTimeoutOverride = 60 * time.Millisecond
			},
			wantState:   "Timeout",
			wantOutcome: "error",
			wantReason:  "no_content_timeout",
		},
		{
			name: "canned_error_terminal",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Text("I seem to be encountering an error. Can I try something else for you?")
				return nil
			},
			setup: func(s *Server) {
				s.cfg.CannedErrorRetry = "off"
			},
			wantState:   "Erreur interne",
			wantOutcome: "error",
			wantReason:  "canned_error",
		},
		{
			name: "tool_refusal_terminal",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Text("Le quota d'appels d'outils pour ce tour est épuisé (0 appel restant).")
				return nil
			},
			wantState:   "Refus outils",
			wantOutcome: "error",
			wantReason:  "tool_refusal",
		},
		{
			name: "quota_pause_terminal",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Identity("f2994417f76a9d1e", "3.5 Flash")
				<-ctx.Done()
				return ctx.Err()
			},
			setup: func(s *Server) {
				s.cfg.PauseOnFallback = true
			},
			wantState:   "Pause quota",
			wantOutcome: "error",
			wantReason:  "quota_pause",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newK42TestServer(t, tc.streamFn)
			if tc.setup != nil {
				tc.setup(srv)
			}
			ts := httptest.NewServer(srv.handler())
			defer ts.Close()

			body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
			c := readSSE(t, context.Background(), ts.URL, body)
			if c.status != http.StatusOK {
				t.Fatalf("status = %d, want 200", c.status)
			}

			hist := srv.flightHist.list()
			if len(hist) == 0 {
				t.Fatalf("expected non-empty in_flight_history")
			}
			if hist[0].State != tc.wantState {
				t.Errorf("in_flight_history State = %q, want %q", hist[0].State, tc.wantState)
			}

			act := srv.diag.query(diagQuery{Window: "1h", Kind: "activity", Limit: 10}, time.Now())
			if len(act.Items) == 0 {
				t.Fatalf("expected non-empty diagRing activity")
			}
			if act.Items[0].Outcome != tc.wantOutcome {
				t.Errorf("diagRing Outcome = %q, want %q", act.Items[0].Outcome, tc.wantOutcome)
			}
			if act.Items[0].Reason != tc.wantReason {
				t.Errorf("diagRing Reason = %q, want %q", act.Items[0].Reason, tc.wantReason)
			}
		})
	}
}

func TestK42_WebUIVersionAndHelpers(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	if !strings.Contains(html, `Math.round(s)+" s"`) {
		t.Errorf("ui.html age() missing Math.round(s)")
	}
	if !strings.Contains(html, `isErrState`) || !strings.Contains(html, `badge("bad", c.state)`) {
		t.Errorf("ui.html stateBadge() missing bad-class styling for terminal errors")
	}

	srv := newK42TestServer(t, nil)
	srv.uiToken = "test-token"
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.UIVersion != uiVersion {
		t.Errorf("/_status ui_version = %q, want %q", st.UIVersion, uiVersion)
	}
	if st.Version != version {
		t.Errorf("/_status version = %q, want %q", st.Version, version)
	}
}
