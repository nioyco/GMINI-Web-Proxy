package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// googleSim answers like Gemini (signed-in /app page, a listModels catalog,
// RotateCookies without a new token) and counts calls by kind.
type googleSim struct {
	mu       sync.Mutex
	calls    map[string]int
	rotateTS string // if non-empty, RotateCookies returns a fresh __Secure-1PSIDTS
}

func (g *googleSim) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.calls == nil {
		g.calls = map[string]int{}
	}
	body := ""
	hdr := http.Header{}
	switch {
	case r.URL.Host == "accounts.google.com":
		g.calls["rotate"]++
		if g.rotateTS != "" {
			hdr.Add("Set-Cookie", "__Secure-1PSIDTS="+g.rotateTS+"; Path=/; Secure; HttpOnly")
		}
	case strings.HasSuffix(r.URL.Path, "/app"):
		g.calls["app"]++
		body = appPage
	case strings.Contains(r.URL.Path, "batchexecute"):
		g.calls["batch"]++
		body = catalogResponse()
	default:
		g.calls["other"]++
	}
	return &http.Response{StatusCode: 200, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (g *googleSim) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.calls {
		n += v
	}
	return n
}

func (g *googleSim) get(k string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[k]
}

// catalogResponse is a minimal listModels batchexecute answer with one model.
func catalogResponse() string {
	m := make([]any, 12)
	m[0], m[1], m[11] = "hexid", "Fast", "3.5 Flash"
	body := make([]any, 18)
	body[15] = []any{m}
	inner, _ := json.Marshal(body)
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "otAQ7b", string(inner)}})
	return ")]}'\n" + string(outer)
}

func startupConfig(t *testing.T, dir string) *Config {
	t.Helper()
	cookie := filepath.Join(dir, "cookie.txt")
	if _, err := os.Stat(cookie); os.IsNotExist(err) {
		if err := os.WriteFile(cookie, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.CookieRefresh = 9
	cfg.Gemini.GeminiBL = ""
	cfg.Gemini.GeminiBLAuto = true
	cfg.Gemini.CookieFile = cookie
	cfg.Gemini.StateFile = stateFilePath("", cookie, "")
	return cfg
}

func startupClient(t *testing.T, cfg *Config) (*gemini.Client, *googleSim) {
	t.Helper()
	c, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	sim := &googleSim{}
	c.SetTransport(sim)
	return c, sim
}

// AUDIT-13 A5 + R&D-14 R4: cold start = 1 /app + 1 RotateCookies (psidts age
// unknown without state) + 1 listModels; restart with the saved state = 0
// Google calls; /_status shows ages only, never the token.
func TestStartupColdThenWarmRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := startupConfig(t, dir)

	c1, sim1 := startupClient(t, cfg)
	sim1.rotateTS = "ts_rotated"
	auth, refresh := startSession(context.Background(), cfg, c1, testLogger())
	if !strings.HasPrefix(auth, "authenticated") || !refresh {
		t.Fatalf("cold: auth=%q refresh=%v", auth, refresh)
	}
	if sim1.get("app") != 1 || sim1.get("batch") != 1 || sim1.get("rotate") != 1 || sim1.total() != 3 {
		t.Errorf("cold start calls = %v, want 1 /app + 1 rotate + 1 listModels", sim1.calls)
	}
	info, err := os.Stat(cfg.Gemini.StateFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", err, info)
	}

	c2, sim2 := startupClient(t, cfg)
	auth, refresh = startSession(context.Background(), cfg, c2, testLogger())
	if !strings.HasPrefix(auth, "authenticated") || !refresh || !c2.Authenticated() {
		t.Fatalf("warm: auth=%q refresh=%v", auth, refresh)
	}
	if sim2.total() != 0 {
		t.Errorf("warm restart calls = %v, want none", sim2.calls)
	}
	if len(c2.ModelNames()) != 1 {
		t.Errorf("catalog not restored: %v", c2.ModelNames())
	}

	s := newServer(cfg, c2, testLogger())
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	if strings.Contains(rr.Body.String(), "FAKE_AT") || strings.Contains(rr.Body.String(), "state.json") {
		t.Errorf("/_status leaks the token or the state path: %s", rr.Body)
	}
	var st struct {
		SS map[string]any `json:"session_state"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.SS["persisted"] != true || st.SS["token_age_sec"] == nil || st.SS["catalog_stale"] != false {
		t.Errorf("session_state = %v", st.SS)
	}
	if sim2.total() != 0 {
		t.Errorf("/_status made calls: %v", sim2.calls)
	}
}

func TestStateFilePath(t *testing.T) {
	cases := []struct{ setting, file, inline, want string }{
		{"", "/d/cookie.txt", "", "/d/cookie.txt.state.json"},
		{"/x/s.json", "/d/cookie.txt", "", "/x/s.json"},
		{"off", "/d/cookie.txt", "", ""},
		{"", "/d/cookie.txt", "SAPISID=x", ""},
		{"/x/s.json", "", "SAPISID=x", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := stateFilePath(c.setting, c.file, c.inline); got != c.want {
			t.Errorf("stateFilePath(%q,%q,inline=%v) = %q, want %q", c.setting, c.file, c.inline != "", got, c.want)
		}
	}
}

// AUDIT-13 A7 (S-03): the rotation runs even when the startup finds no signed-in
// session, as long as the cookie carries the rotating token.
func TestRotationEnabledWhenStartingUnauthenticated(t *testing.T) {
	dir := t.TempDir()
	cfg := startupConfig(t, dir)
	c, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		if strings.HasSuffix(r.URL.Path, "/app") {
			body = `<html>"cfb2h":"boq_x"</html>` // page served without a session token
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	auth, refresh := startSession(context.Background(), cfg, c, testLogger())
	if !refresh || !strings.HasPrefix(auth, "NOT authenticated") {
		t.Errorf("auth=%q refresh=%v: rotation must run without a signed-in session", auth, refresh)
	}
	cfg.Gemini.CookieFile = ""
	cfg.Gemini.Cookie = "SAPISID=s" // no rotating token
	c2, _ := gemini.New(cfg.Gemini, testLogger())
	c2.SetTransport(&googleSim{})
	if _, refresh := startSession(context.Background(), cfg, c2, testLogger()); refresh {
		t.Error("no rotation without __Secure-1PSIDTS")
	}
}

// TestStartupWithBrowserScriptStateSkipsApp proves that when recuperer_cookie_gemini.mjs
// writes <cookie>.state.json with at/bl from Chrome, proxy startup makes 0 calls to /app.
func TestStartupWithBrowserScriptStateSkipsApp(t *testing.T) {
	dir := t.TempDir()
	cfg := startupConfig(t, dir)
	sum := sha256.Sum256([]byte("s"))
	fp := hex.EncodeToString(sum[:])[:16]
	now := time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            now,
		"session_fingerprint": fp,
		"at":                  "SCRIPT_AT_1",
		"at_at":               now,
		"bl":                  "boq_script_bl",
		"bl_at":               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Gemini.StateFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c, sim := startupClient(t, cfg)
	auth, _ := startSession(context.Background(), cfg, c, testLogger())
	if !strings.HasPrefix(auth, "authenticated") || !c.Authenticated() {
		t.Fatalf("auth = %q authenticated=%v", auth, c.Authenticated())
	}
	if sim.get("app") != 0 {
		t.Errorf("startup /app calls = %d, want 0", sim.get("app"))
	}
	if sim.get("batch") != 1 {
		t.Errorf("startup listModels calls = %d, want 1 (catalog was absent)", sim.get("batch"))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestN8_StartupSkipsCatalogCallWhenUnder24hEvenOnNewSession(t *testing.T) {
	dir := t.TempDir()
	cfg := startupConfig(t, dir)
	oldSum := sha256.Sum256([]byte("old_sapisid"))
	oldFP := hex.EncodeToString(oldSum[:])[:16]
	now := time.Now().UTC().Format(time.RFC3339)

	// State written 2h ago under a different session, containing 4 models.
	twoHoursAgo := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	raw, err := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            now,
		"session_fingerprint": oldFP,
		"at":                  "OLD_AT",
		"at_at":               twoHoursAgo,
		"bl":                  "boq_old",
		"bl_at":               twoHoursAgo,
		"models": []map[string]any{
			{"Name": "gemini-quick-answers", "ModelID": "8c46e95b1a07cecc"},
			{"Name": "gemini-everyday-help", "ModelID": "56fdd199312815e2"},
			{"Name": "gemini-detailed-research", "ModelID": "e6fa609c3fa255c0"},
			{"Name": "gemini-most-complex-work", "ModelID": "95a4c0abcba24364"},
		},
		"models_at":     twoHoursAgo,
		"models_source": "dynamic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Gemini.StateFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c, sim := startupClient(t, cfg)
	_, _ = startSession(context.Background(), cfg, c, testLogger())
	if sim.get("batch") != 0 {
		t.Fatalf("expected 0 batchexecute calls at startup with <24h catalog, got %d", sim.get("batch"))
	}
	if len(c.ModelNames()) != 4 {
		t.Fatalf("expected 4 restored models, got %v", c.ModelNames())
	}

	// Catalog 3 days old (< 7d, >= 24h) + batchexecute blocked (429): keeps all 4 models.
	threeDaysAgo := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	rawOld, _ := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            now,
		"session_fingerprint": oldFP,
		"models": []map[string]any{
			{"Name": "gemini-quick-answers", "ModelID": "8c46e95b1a07cecc"},
			{"Name": "gemini-everyday-help", "ModelID": "56fdd199312815e2"},
			{"Name": "gemini-detailed-research", "ModelID": "e6fa609c3fa255c0"},
			{"Name": "gemini-most-complex-work", "ModelID": "95a4c0abcba24364"},
		},
		"models_at":     threeDaysAgo,
		"models_source": "dynamic",
	})
	if err := os.WriteFile(cfg.Gemini.StateFile, rawOld, 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var batchCalls int
	c2.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/app") {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(appPage)), Request: r}, nil
		}
		if strings.Contains(r.URL.Path, "batchexecute") {
			batchCalls++
			return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("sorry")), Request: r}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	_, _ = startSession(context.Background(), cfg, c2, testLogger())
	if batchCalls != 1 {
		t.Fatalf("expected 1 startup batchexecute attempt for >24h catalog, got %d", batchCalls)
	}
	if len(c2.ModelNames()) != 4 || c2.ModelsSourceLabel() != "persisted" {
		t.Fatalf("expected 4 persisted models retained on block, got %v (%q)", c2.ModelNames(), c2.ModelsSourceLabel())
	}
}
