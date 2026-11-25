package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK47_QuotaPreflightErrorContextInFlightAndDiag(t *testing.T) {
	srv := newK42TestServer(t, nil)
	srv.cfg.PauseOnFallback = true
	srv.toolsDB = newToolsDB("", 100)
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

	hist := srv.flightHist.list()
	if len(hist) == 0 {
		t.Fatalf("expected non-empty in_flight_history")
	}
	h := hist[0]
	if h.State != "Pause quota (429)" || h.Method != "POST" || h.Route != "/v1/chat/completions" {
		t.Errorf("unexpected state/method/route: %+v", h)
	}
	if h.ErrorService != "pre-flight/quota" || h.ErrorFunc != "quotaPreFlight" || h.ErrorCode != "quota_pause_blocked" || h.ErrorDetail == "" {
		t.Errorf("unexpected error context: service=%q func=%q code=%q detail=%q", h.ErrorService, h.ErrorFunc, h.ErrorCode, h.ErrorDetail)
	}

	act := srv.diag.query(diagQuery{Window: "1h", Kind: "activity", Limit: 10}, time.Now())
	if len(act.Items) == 0 || act.Items[0].Service != "pre-flight/quota" || act.Items[0].Func != "quotaPreFlight" || act.Items[0].Route != "POST /v1/chat/completions" {
		t.Errorf("diag activity provenance missing: %+v", act.Items)
	}
}

func TestK47_TerminalErrorContextPopulated(t *testing.T) {
	cases := []struct {
		name        string
		streamFn    func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error
		setup       func(s *Server)
		wantService string
		wantFunc    string
		wantCode    string
	}{
		{
			name: "no_content_timeout",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Heartbeat()
				<-ctx.Done()
				return ctx.Err()
			},
			setup:       func(s *Server) { s.noContentTimeoutOverride = 50 * time.Millisecond },
			wantService: "stream/watchdog",
			wantFunc:    "finishNoContentTimeout",
			wantCode:    "no_content_timeout",
		},
		{
			name: "early_stop_stall",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Text("```tool_call\n{\"name\":\"write_file\",")
				<-ctx.Done()
				return ctx.Err()
			},
			setup: func(s *Server) {
				s.cfg.HeldStallRetries = -1
				s.heldStallOverride = 30 * time.Millisecond
			},
			wantService: "stream/watchdog",
			wantFunc:    "runStreamTools",
			wantCode:    "early_stop:stall",
		},
		{
			name: "tool_refusal",
			streamFn: func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
				h.Opened()
				h.Text("Le quota d'appels d'outils pour ce tour est épuisé (0 appel restant).")
				return nil
			},
			wantService: "guard/tools",
			wantFunc:    "finishTerminalNonAnswer",
			wantCode:    "tool_refusal",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newK42TestServer(t, tc.streamFn)
			srv.toolsDB = newToolsDB("", 100)
			if tc.setup != nil {
				tc.setup(srv)
			}
			ts := httptest.NewServer(srv.handler())
			defer ts.Close()

			body := `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`
			_ = readSSE(t, context.Background(), ts.URL, body)

			hist := srv.flightHist.list()
			if len(hist) == 0 {
				t.Fatalf("expected non-empty in_flight_history")
			}
			h := hist[0]
			if h.ErrorService != tc.wantService || h.ErrorFunc != tc.wantFunc || h.ErrorCode != tc.wantCode || h.ErrorDetail == "" {
				t.Errorf("got (%q, %q, %q, %q), want (%q, %q, %q, non-empty)", h.ErrorService, h.ErrorFunc, h.ErrorCode, h.ErrorDetail, tc.wantService, tc.wantFunc, tc.wantCode)
			}
		})
	}
}

func TestK47_WebUIv304ErrorDetailAndGminiProxyHint(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	for _, want := range []string{
		"errBoxInflight",
		"error_service",
		"error_func",
		"error_detail",
		"Bloqué par GMINI (pré-vol)",
		"(GMINI-Proxy)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}
