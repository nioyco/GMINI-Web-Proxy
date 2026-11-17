package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// testConfig is the app config used by handler tests. Its Gemini settings mirror
// the gemini package's own test config (no network, single retry).
func testConfig() *Config {
	return &Config{
		NoInternalCodeExecRule: true,
		Gemini: gemini.Config{
			RetryAttempts:  1,
			RetryDelaySec:  0,
			RequestTimeout: 10,
			GeminiBL:       "test_bl",
			DefaultModel:   "gemini-3.5-flash",
		},
	}
}

// testLogger returns a logger that discards all output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testModels is the deterministic catalog seeded into the test client so handler
// tests never hit the network. gemini-3.5-flash matches testConfig's default.
func testModels() []*gemini.AvailableModel {
	return []*gemini.AvailableModel{
		{Name: "gemini-3.5-flash", ModelID: "fastid", DisplayName: "Fast", Description: "Fast general-purpose model", Capacity: 1, CapacityField: 13},
		{Name: "gemini-3.1-pro", ModelID: "proid", DisplayName: "Pro", Description: "Pro model", Capacity: 2, CapacityField: 12},
		{Name: "gemini-3.1-flash-lite", ModelID: "liteid", DisplayName: "3.1 Flash-Lite", Description: "Lightweight fast model", Capacity: 1, CapacityField: 12},
	}
}

func newTestServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatalf("gemini.New: %v", err)
	}
	client.SetModels(testModels()) // deterministic registry; never touches the network
	return newServer(cfg, client, testLogger())
}

func TestAuthorized(t *testing.T) {
	t.Run("open when no keys", func(t *testing.T) {
		s := newTestServer(t, testConfig())
		req := newReq(http.MethodPost, "/v1/chat/completions", nil)
		if !s.authorized(req) {
			t.Errorf("should be open with no keys configured")
		}
	})

	cfg := testConfig()
	cfg.APIKeys = []string{"secret1", "secret2"}
	s := newTestServer(t, cfg)

	t.Run("valid bearer", func(t *testing.T) {
		req := newReq(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer secret2")
		if !s.authorized(req) {
			t.Errorf("valid bearer rejected")
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		req := newReq(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		if s.authorized(req) {
			t.Errorf("invalid key accepted")
		}
	})

	t.Run("missing key", func(t *testing.T) {
		req := newReq(http.MethodPost, "/v1/chat/completions", nil)
		if s.authorized(req) {
			t.Errorf("missing key accepted")
		}
	})

	t.Run("x-goog-api-key", func(t *testing.T) {
		req := newReq(http.MethodPost, "/v1beta/models/x:generateContent", nil)
		req.Header.Set("x-goog-api-key", "secret1")
		if !s.authorized(req) {
			t.Errorf("x-goog-api-key rejected")
		}
	})

	t.Run("query param key", func(t *testing.T) {
		req := newReq(http.MethodPost, "/v1beta/models/x:generateContent?key=secret1", nil)
		if !s.authorized(req) {
			t.Errorf("query key rejected")
		}
	})
}

func TestHandleModelsOpenAI(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	req := newReq(http.MethodGet, "/v1/models", nil)
	s.handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Object != "list" || len(resp.Data) != len(testModels()) {
		t.Errorf("got object=%q, models=%d, want list/%d", resp.Object, len(resp.Data), len(testModels()))
	}
}

func TestHandleModelsAnthropic(t *testing.T) {
	s := newTestServer(t, testConfig())
	req := newReq(http.MethodGet, "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var resp struct {
		Data []struct {
			Type string `json:"type"`
		} `json:"data"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != len(testModels()) || resp.Data[0].Type != "model" {
		t.Errorf("data = %+v", resp.Data)
	}
	if resp.HasMore {
		t.Errorf("has_more should be false")
	}
}

func TestModelsFormatDetection(t *testing.T) {
	s := newTestServer(t, testConfig())

	// Without the header → OpenAI format ("object":"list").
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(rr.Body.String(), `"object":"list"`) {
		t.Errorf("expected OpenAI format, got: %s", rr.Body.String())
	}
}

func TestHandleCountTokens(t *testing.T) {
	s := newTestServer(t, testConfig())
	body := `{"model":"claude-x","messages":[{"role":"user","content":"hello world here"}]}`
	rr := httptest.NewRecorder()
	req := newReq(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	s.handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want > 0", resp.InputTokens)
	}
}

func TestHandleHealth(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "ok" {
		t.Errorf("status field = %v, want ok", resp["status"])
	}
}

func TestCORSDefaultNoHeaders(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	req := newReq(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	s.handler().ServeHTTP(rr, req)
	if v := rr.Header().Get("Access-Control-Allow-Origin"); v != "" {
		t.Errorf("ACAO = %q, want none by default", v)
	}
}

func TestCORSPreflightDisallowedOrigin(t *testing.T) {
	s := newTestServer(t, testConfig())
	rr := httptest.NewRecorder()
	req := newReq(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	s.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("OPTIONS status = %d, want 403", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("must not emit ACAO for a disallowed origin")
	}
}

func TestCORSAllowedOrigin(t *testing.T) {
	cfg := testConfig()
	cfg.CORSOrigins = []string{"https://app.example", "*"}
	s := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	req := newReq(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://app.example")
	s.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("ACAO = %q", got)
	}
	// The literal "*" entry is never honoured.
	rr = httptest.NewRecorder()
	req = newReq(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://other.example")
	s.handler().ServeHTTP(rr, req)
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("wildcard must not be honoured")
	}
}

func TestHostCheck(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedHosts = []string{"proxy.lan"}
	s := newTestServer(t, cfg)
	cases := map[string]int{
		"127.0.0.1:8081": 200, "127.0.0.1": 200, "localhost:9": 200, "LOCALHOST": 200,
		"[::1]:8081": 200, "[::1]": 200, "proxy.lan:8081": 200,
		"evil.example": 403, "evil.example:8081": 403, "127.0.0.1.evil.example": 403,
		"192.168.1.5:8081": 403, "": 403,
	}
	for host, want := range cases {
		rr := httptest.NewRecorder()
		req := newReq(http.MethodGet, "/", nil)
		req.Host = host
		s.handler().ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("Host %q: status = %d, want %d", host, rr.Code, want)
		}
	}
}

func TestAuthGateBlocksUnauthorized(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"sk-test"}
	s := newTestServer(t, cfg)

	rr := httptest.NewRecorder()
	req := newReq(http.MethodPost, "/v1/chat/completions", nil)
	s.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}

	// Health stays open even with keys configured.
	rr2 := httptest.NewRecorder()
	s.handler().ServeHTTP(rr2, newReq(http.MethodGet, "/", nil))
	if rr2.Code != http.StatusOK {
		t.Errorf("health status = %d, want 200", rr2.Code)
	}
}

func TestMessagesAuthGate(t *testing.T) {
	cfg := testConfig()
	cfg.APIKeys = []string{"sk-test"}
	s := newTestServer(t, cfg)

	rr := httptest.NewRecorder()
	req := newReq(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	s.handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

// newReq builds a test request addressed to the loopback host, as a real local
// client would (httptest.NewRequest defaults to Host "example.com", which the
// Host check rejects).
func newReq(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Host = "127.0.0.1:8081"
	return r
}

func TestRemoteImageURLRejectedByDefault(t *testing.T) {
	cfg := testConfig()
	s := newTestServer(t, cfg)
	body := `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"http://169.254.169.254/x.png"}}]}]}`
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	// Without an authenticated session the images check answers first; either way it
	// must be a 400 and never reach the network.
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}

func TestHasRemoteImage(t *testing.T) {
	if hasRemoteImage([]gemini.InputImage{{Data: []byte("x")}}) {
		t.Error("data image is not remote")
	}
	if !hasRemoteImage([]gemini.InputImage{{Data: []byte("x")}, {URL: "https://a.example/i.png"}}) {
		t.Error("URL image is remote")
	}
}
