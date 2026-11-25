package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK58_Body54QuotaPauseEndToEnd4Neon(t *testing.T) {
	inner2 := make([]any, 55)
	inner2[4] = []any{
		[]any{"rc_1", []any{"OK"}, nil, nil, nil},
	}
	inner2[54] = []any{
		nil, nil, float64(1), nil,
		[]any{
			[]any{float64(3), []any{float64(1795650303), float64(894116000)}, float64(1), nil, []any{float64(1)}},
		},
		nil, nil, nil, false, false, float64(1436),
	}
	innerJSON, err := json.Marshal(inner2)
	if err != nil {
		t.Fatalf("marshal inner2: %v", err)
	}
	outerJSON, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerJSON)}})
	if err != nil {
		t.Fatalf("marshal outer: %v", err)
	}
	payload := ")]}'\n\n" + string(outerJSON) + "\n"

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(payload)),
		}, nil
	}))
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: "neon1234", ModeCode: 3, Description: "4 Neon", DisplayName: "4 Neon"},
	})

	cfg := testConfig()
	cfg.StreamToolsProgress = true
	srv := newServer(cfg, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.cfg.PauseOnFallback = true

	body := `{"model":"gemini-deeper-analysis","stream":false,"messages":[{"role":"user","content":"hello"}]}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req1.Host = "127.0.0.1:8081"
	req1.Header.Set("Authorization", "Bearer test-key")
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.handler().ServeHTTP(w1, req1)

	snap := srv.quotaPauseSnapshot()
	if !snap.Active {
		t.Fatalf("expected quotaPause Active=true after body[54] limit frame, got %+v (resp=%d %s)", snap, w1.Code, w1.Body.String())
	}
	if !strings.Contains(snap.LimitBanner, "Limite de quota Google atteinte") {
		t.Fatalf("snap.LimitBanner = %q, want Limite de quota Google atteinte", snap.LimitBanner)
	}

	dr := srv.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 20}, time.Now())
	hasPauseBanner := false
	for _, it := range dr.Items {
		if it.Type == "quota_fallback_pause" && strings.Contains(it.Detail, "limit_banner") {
			hasPauseBanner = true
			break
		}
	}
	if !hasPauseBanner {
		t.Fatalf("expected quota_fallback_pause (reason=limit_banner) incident, got %+v", dr.Items)
	}

	// Next request must be blocked with HTTP 429.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req2.Host = "127.0.0.1:8081"
	req2.Header.Set("Authorization", "Bearer test-key")
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429; body=%s", w2.Code, w2.Body.String())
	}
}

func TestK58_ServedOtherModelEmptyServedID(t *testing.T) {
	m := &gemini.AvailableModel{
		Name:        "gemini-deeper-analysis",
		ModelID:     "neon1234",
		Description: "4 Neon",
		DisplayName: "4 Neon",
	}
	if !servedOtherModel(m, "", "3.5 Flash-Lite") {
		t.Fatal("expected servedOtherModel=true when servedID is empty and servedName is 3.5 Flash-Lite for 4 Neon")
	}
	if servedOtherModel(m, "", "4 Neon") {
		t.Fatal("expected servedOtherModel=false when servedName matches DisplayName")
	}
}
