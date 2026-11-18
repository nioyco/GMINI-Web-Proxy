package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestStatusRequiresKey(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"sk-test"}
	s := newTestServer(t, cfg)

	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", rr.Code)
	}
	rr = httptest.NewRecorder()
	req := newReq(http.MethodGet, "/_status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	s.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: status = %d, want 401", rr.Code)
	}
}

func TestStatusContentAndNoSecrets(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"sk-SECRETKEY"}
	cfg.Gemini.Cookie = "SAPISID=SECRETCOOKIE; __Secure-1PSIDTS=SECRETTS"
	cfg.ShutdownGraceSec = 30
	s := newTestServer(t, cfg)
	h := s.handler()

	// Generate a little traffic first.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newReq(http.MethodGet, "/", nil))

	rr = httptest.NewRecorder()
	req := newReq(http.MethodGet, "/_status", nil)
	req.Header.Set("Authorization", "Bearer sk-SECRETKEY")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{"SECRETKEY", "SECRETCOOKIE", "SECRETTS"} {
		if strings.Contains(body, secret) {
			t.Errorf("/_status leaks %q: %s", secret, body)
		}
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "commit", "uptime_sec", "authenticated", "cookie_last_rotation_age_sec",
		"models_source", "default_model", "active_requests", "counters"} {
		if _, ok := st[k]; !ok {
			t.Errorf("missing field %q in %s", k, body)
		}
	}
	if st["authenticated"] != false {
		t.Errorf("authenticated = %v, want false (no upstream auth in tests)", st["authenticated"])
	}
	if st["models_source"] != "static" || st["default_model"] != "gemini-3.5-flash" {
		t.Errorf("models/default = %v / %v", st["models_source"], st["default_model"])
	}
	if st["active_requests"].(float64) != 1 {
		t.Errorf("active_requests = %v, want 1 (the status request itself)", st["active_requests"])
	}
	if c := st["counters"].(map[string]any); c["requests_total"].(float64) < 2 {
		t.Errorf("counters = %v", c)
	}
}

func TestModelsAndHealthStayOpen(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"sk-test"}
	s := newTestServer(t, cfg)
	for _, p := range []string{"/", "/v1/models"} {
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, newReq(http.MethodGet, p, nil))
		if rr.Code != 200 {
			t.Errorf("GET %s without key = %d, want 200 (DEC-36)", p, rr.Code)
		}
	}
}

func TestStatusCookieRotationBlock(t *testing.T) {
	cfg := testConfig()
	cfg.Gemini.Cookie = "SAPISID=SECRETCOOKIE; __Secure-1PSIDTS=SECRETTS"
	s := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		CR map[string]any `json:"cookie_rotation"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"auth_state", "psidts_present", "last_attempt_age_sec", "last_success_age_sec", "last_result",
		"attempts", "renewed", "not_renewed", "consecutive_no_renewal", "token_age_sec", "token_age_source",
		"last_http_status", "last_set_cookie_names", "last_redirected"} {
		if _, ok := st.CR[k]; !ok {
			t.Errorf("cookie_rotation missing %q: %s", k, rr.Body)
		}
	}
	if st.CR["psidts_present"] != true || st.CR["auth_state"] != "anonymous" {
		t.Errorf("block = %v", st.CR)
	}
	if strings.Contains(rr.Body.String(), "SECRET") {
		t.Errorf("leak: %s", rr.Body)
	}
}

func TestStatusGoogleBlockBlock(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		GB       map[string]any   `json:"google_block"`
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"active", "since", "last_seen", "backoff_until", "level", "total", "families"} {
		if _, ok := st.GB[k]; !ok {
			t.Errorf("google_block missing %q: %s", k, rr.Body)
		}
	}
	if st.GB["active"] != false {
		t.Errorf("block = %v", st.GB)
	}
	if _, ok := st.Counters["google_blocks"]; !ok {
		t.Errorf("counter google_blocks missing")
	}
}

func TestP6_StatusBackgroundCallsBlock(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var st struct {
		BG backgroundCallsStatus `json:"background_calls"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.BG.Total24h != 0 || st.BG.LastAt != nil || st.BG.Label != "aucun depuis le démarrage" {
		t.Fatalf("initial background_calls = %+v, want none since startup", st.BG)
	}
	if st.BG.IdleQuietMin != 10 || st.BG.CookieRotation || st.BG.CatalogRefreshAfterCookie || st.BG.AuthRecovery {
		t.Fatalf("unexpected defaults in background_calls = %+v", st.BG)
	}

	// Trigger one allowed background call (e.g. after user activity).
	s.client.RecordUserActivity()
	if !s.client.AcquireBackground(gemini.FamilyBatch, "catalog_manual") {
		t.Fatal("expected AcquireBackground to allow after user activity")
	}
	rr2 := httptest.NewRecorder()
	s.handler().ServeHTTP(rr2, newReq(http.MethodGet, "/_status", nil))
	if err := json.Unmarshal(rr2.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.BG.Total24h != 1 || st.BG.ByFamily24h["batchexecute"] != 1 || st.BG.LastAt == nil || st.BG.LastFamily != "batchexecute" || st.BG.LastReason != "catalog_manual" {
		t.Fatalf("after call background_calls = %+v", st.BG)
	}
}
