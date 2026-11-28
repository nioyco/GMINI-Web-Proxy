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

func TestK61_SoftWarning90PctNonBlockingVsHardLimit100Pct(t *testing.T) {
	phase := 1
	srv := newK42TestServer(t, func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		if phase == 1 {
			if h.QuotaWarning != nil {
				h.QuotaWarning("Quota Google proche de la limite (~90 %, réinitialisation à 12:30)")
			}
			if h.Text != nil {
				h.Text("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```")
			}
			return nil
		}
		if h.LimitBanner != nil {
			h.LimitBanner("Limite de quota Google atteinte (réinitialisation à 12:30)")
		}
		return nil
	})
	srv.cfg.StreamToolsProgress = true
	srv.cfg.StreamThoughts = true
	srv.cfg.PauseOnFallback = true
	srv.keepaliveOverride = 15 * time.Millisecond
	srv.client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: "neon1234567890ab", ModeCode: 3, Description: "4 Neon", DisplayName: "4 Neon"},
	})

	doReq := func() *httptest.ResponseRecorder {
		body := `{
			"model":"gemini-deeper-analysis",
			"stream":true,
			"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],
			"messages":[{"role":"user","content":"read main.go"}]
		}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1:8081"
		req.Header.Set("Authorization", "Bearer test-key")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.handler().ServeHTTP(w, req)
		return w
	}

	// Phase 1: ~90% warning must NOT pause the proxy and must deliver read_file.
	w1 := doReq()
	if w1.Code != http.StatusOK {
		t.Fatalf("phase 1 status = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	if !strings.Contains(w1.Body.String(), `"name":"read_file"`) {
		t.Fatalf("phase 1 expected read_file tool_call, got:\n%s", w1.Body.String())
	}
	if snap := srv.quotaPauseSnapshot(); snap.Active {
		t.Fatalf("phase 1 (~90%% warning) must NOT activate quota pause: %+v", snap)
	}
	dr := srv.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 20}, time.Now())
	foundWarn := false
	for _, it := range dr.Items {
		if it.Type == "quota_warning" && it.Detail == "warning_90pct" && it.Recovered {
			foundWarn = true
			break
		}
	}
	if !foundWarn {
		t.Fatalf("expected non-blocking quota_warning (warning_90pct, recovered=true) in diag, got %+v", dr.Items)
	}

	// Phase 2: 100% hard limit must activate quota pause and block subsequent requests with HTTP 429.
	phase = 2
	_ = doReq()
	if snap := srv.quotaPauseSnapshot(); !snap.Active {
		t.Fatalf("phase 2 (100%% limit) MUST activate quota pause: %+v", snap)
	}
	w3 := doReq()
	if w3.Code != http.StatusTooManyRequests {
		t.Fatalf("phase 3 after hard limit status = %d, want 429; body=%s", w3.Code, w3.Body.String())
	}
}
