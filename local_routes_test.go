package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

type countingRT struct{ n atomic.Int32 }

// appPage is a signed-in /app page (fake token, fake build label).
const appPage = `<html>"cfb2h":"boq_assistant-bard-web-server_20261110.00_p0" "SNlM0e":"FAKE_AT"</html>`

func (c *countingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	body := ""
	if strings.HasSuffix(r.URL.Path, "/app") {
		body = appPage
	}
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// newCountingServer builds a server whose client auto-resolves the build label
// (the case where CurrentBL used to refresh on a TTL) and has a cookie, so any
// outbound call made by a diagnostic route would be counted.
func newCountingServer(t *testing.T, withSession bool) (*Server, *countingRT) {
	t.Helper()
	cfg := testConfig()
	cfg.Gemini.GeminiBL = "" // auto
	cfg.Gemini.Cookie = "SAPISID=s; __Secure-1PSIDTS=ts"
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	rt := &countingRT{}
	client.SetTransport(rt)
	client.SetModels(testModels())
	if withSession {
		if got := client.CheckAuth(context.Background()); got != "authenticated" {
			t.Fatalf("auth = %q", got)
		}
		rt.n.Store(0)
	}
	return newServer(cfg, client, testLogger()), rt
}

// AUDIT-13 A4 (S-05): /_status, GET /, /v1/models and /v1beta/models in a burst
// make no outbound call at all.
func TestDiagnosticRoutesBurstMakeNoOutboundCall(t *testing.T) {
	s, rt := newCountingServer(t, true)
	h := s.handler()
	for i := 0; i < 40; i++ {
		for _, path := range []string{"/_status", "/", "/v1/models", "/v1beta/models"} {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, newReq(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s -> %d", path, rr.Code)
			}
		}
		rr := httptest.NewRecorder()
		req := newReq(http.MethodGet, "/v1/models", nil)
		req.Header.Set("anthropic-version", "2023-06-01")
		h.ServeHTTP(rr, req)
	}
	time.Sleep(50 * time.Millisecond) // any background refresh would have fired by now
	if n := rt.n.Load(); n != 0 {
		t.Errorf("%d outbound calls from diagnostic routes, want 0", n)
	}
}

func TestModelsRefreshRouteRefusesWithoutSession(t *testing.T) {
	s, rt := newCountingServer(t, false)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodPost, "/_models/refresh", nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not authenticated") {
		t.Errorf("code=%d body=%s", rr.Code, rr.Body)
	}
	if rt.n.Load() != 0 {
		t.Errorf("%d outbound calls", rt.n.Load())
	}
}
