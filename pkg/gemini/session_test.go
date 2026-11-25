package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// AUDIT-13 A6: a renewed rotation does not reload /app (one call per rotation).
func TestRenewedRotationDoesNotReloadApp(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		h := http.Header{}
		h.Add("Set-Cookie", "__Secure-1PSIDTS=renewed; Path=/; Secure")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	c.setAt("tok")
	c.RotateCookie(context.Background())
	if c.RotationStatus().LastResult != rotRenewed {
		t.Fatalf("result = %q", c.RotationStatus().LastResult)
	}
	if log.count("") != 1 || log.count(FamilyAccounts) != 1 {
		t.Errorf("calls=%d accounts=%d, want exactly the RotateCookies call", log.count(""), log.count(FamilyAccounts))
	}
}

// AUDIT-13 A6/A4: the build label has no TTL any more; CurrentBL never calls.
func TestCurrentBLIsPure(t *testing.T) {
	var log callLog
	c, _ := newAutoBLClient(t, Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts"}, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, appPageAuth), nil
	}))
	for i := 0; i < 20; i++ {
		_ = c.CurrentBL()
	}
	time.Sleep(20 * time.Millisecond)
	if log.count("") != 0 {
		t.Errorf("CurrentBL made %d calls", log.count(""))
	}
}

// genText is long enough for parseWrbLine's minimum line length.
var genText = "hello from gemini " + strings.Repeat("and more text ", 20)

// genRT simulates StreamGenerate (statuses in order, then text) and a signed-in
// /app page, recording every call.
type genRT struct {
	t        *testing.T
	mu       sync.Mutex
	statuses []int
	gen, app int
	appBody  string
	ats      []string // "at" form field of each generation
}

func (g *genRT) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/app") {
		g.app++
		return resp200(r, g.appBody), nil
	}
	g.gen++
	_ = r.ParseForm()
	g.ats = append(g.ats, r.PostForm.Get("at"))
	status := 200
	if len(g.statuses) > 0 {
		status, g.statuses = g.statuses[0], g.statuses[1:]
	}
	body := ""
	if status == 200 {
		body = makeWrbLine(g.t, genText)
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func newGenClient(t *testing.T, g *genRT) *Client {
	t.Helper()
	g.t = t
	c, _ := newAutoBLClient(t, Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts", GeminiBL: "x", RetryAttempts: 3}, g)
	c.setAt("OLD_AT")
	old := reauthMinInterval
	t.Cleanup(func() { reauthMinInterval = old })
	return c
}

// AUDIT-13 A6+A8: a 401 gives one /app reload then one new attempt (with the new
// token) — 2 generations and 1 /app, not 3 generations.
func TestAuthErrorReloadsOnceAndRetriesOnce(t *testing.T) {
	g := &genRT{statuses: []int{401}, appBody: appPageAuth}
	c := newGenClient(t, g)
	text, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
	if err != nil || text != strings.TrimSpace(genText) {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if g.gen != 2 || g.app != 1 {
		t.Errorf("generations=%d /app=%d, want 2 and 1", g.gen, g.app)
	}
	if g.ats[0] != "OLD_AT" || g.ats[1] != "AT_TOKEN_1" {
		t.Errorf("tokens sent = %v: the retry must carry the reloaded token", g.ats)
	}
}

func TestAuthErrorTwiceNoSecondReload(t *testing.T) {
	g := &genRT{statuses: []int{401, 401, 401}, appBody: appPageAuth}
	c := newGenClient(t, g)
	_, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !errors.Is(err, ErrSessionAuth) {
		t.Fatalf("err = %v", err)
	}
	if g.gen != 2 || g.app != 1 {
		t.Errorf("generations=%d /app=%d, want 2 and 1", g.gen, g.app)
	}
	// A second request inside reauthMinInterval: no reload, 1 generation.
	_, err = c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !errors.Is(err, ErrSessionAuth) || g.gen != 3 || g.app != 1 {
		t.Errorf("err=%v generations=%d /app=%d", err, g.gen, g.app)
	}
}

func TestAuthErrorDeadSessionNoRetry(t *testing.T) {
	g := &genRT{statuses: []int{403}, appBody: appPageAnon}
	c := newGenClient(t, g)
	_, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !errors.Is(err, ErrSessionAuth) || g.gen != 1 || g.app != 1 {
		t.Errorf("err=%v generations=%d /app=%d", err, g.gen, g.app)
	}
	if c.Authenticated() {
		t.Error("a page without token must drop the stale token")
	}
}

func TestAuthErrorNoReloadDuringAppBlock(t *testing.T) {
	g := &genRT{statuses: []int{401}, appBody: appPageAuth}
	c := newGenClient(t, g)
	c.recordBlock(&BlockedError{Family: FamilyApp, Host: "gemini.google.com", Path: "/app"})
	_, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !errors.Is(err, ErrSessionAuth) || g.gen != 1 || g.app != 0 {
		t.Errorf("err=%v generations=%d /app=%d", err, g.gen, g.app)
	}
}

func TestStreamAuthErrorReloadsOnce(t *testing.T) {
	g := &genRT{statuses: []int{401}, appBody: appPageAuth}
	c := newGenClient(t, g)
	var out strings.Builder
	err := c.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(d string) { out.WriteString(d) })
	if err != nil || strings.TrimSpace(out.String()) != strings.TrimSpace(genText) || g.gen != 2 || g.app != 1 {
		t.Errorf("err=%v out=%q generations=%d /app=%d", err, out.String(), g.gen, g.app)
	}
}

func TestBLOpportunisticRefresh(t *testing.T) {
	origNow := nowFn
	curNow := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return curNow }
	defer func() { nowFn = origNow }()

	origCatInterval := catalogManualMinInterval
	catalogManualMinInterval = 0
	defer func() { catalogManualMinInterval = origCatInterval }()

	var appCalls, appStatus int
	curBL := "boq_gemini-web-uiserver_20261118.02_p0"
	catPayload := `)]}'` + "\n" + `[["wrb.fr","otAQ7b","[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,[[\"8c46e95b1a07cecc\",\"Auto\",\"Auto\",null,null,null,null,null,\"\",null,null,\"Auto\",null,null,null,null,null,6],[\"56fdd199312815e2\",\"4 Hydrogen\",\"4 Hydrogen\",null,null,null,null,null,\"\",null,null,\"Everyday help\",null,null,null,null,null,1]],[0,1],[1,1]]"]]`

	c, _ := newAutoBLClient(t, Config{
		Cookie:    "SAPISID=sapi; __Secure-1PSIDTS=ts",
		IdleQuiet: 10 * time.Minute,
	}, rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			appCalls++
			if appStatus >= 400 {
				return &http.Response{StatusCode: appStatus, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			}
			html := `<html>"SNlM0e":"AT_TOK","cfb2h":"` + curBL + `"</html>`
			return resp200(r, html), nil
		case strings.Contains(r.URL.Path, "batchexecute"):
			return resp200(r, catPayload), nil
		case strings.Contains(r.URL.Host, "accounts.google.com"):
			return resp200(r, ""), nil
		default:
			return resp200(r, ""), nil
		}
	}))
	appStatus = 200
	c.RecordUserActivity()
	if got := c.CheckAuth(context.Background()); got != "authenticated" {
		t.Fatalf("CheckAuth = %q", got)
	}
	if appCalls != 1 || c.CurrentBL() != "boq_gemini-web-uiserver_20261118.02_p0" {
		t.Fatalf("initial appCalls=%d bl=%q", appCalls, c.CurrentBL())
	}

	// 1) blAt <= 6 h: active RefreshModels must NOT re-fetch /app.
	curNow = curNow.Add(5 * time.Hour)
	c.RecordUserActivity()
	curBL = "boq_gemini-web-uiserver_20261118.09_p0"
	if err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels(<6h): %v", err)
	}
	if appCalls != 1 || c.CurrentBL() != "boq_gemini-web-uiserver_20261118.02_p0" {
		t.Fatalf("expected no /app refresh before 6h, appCalls=%d bl=%q", appCalls, c.CurrentBL())
	}

	// 2) blAt > 6 h, idle_on_demand (no user activity for >10m): RotateCookie must NOT call /app.
	curNow = curNow.Add(2 * time.Hour) // +7h total, lastUserActivity was 2h ago (> 10m IdleQuiet)
	c.RotateCookie(context.Background())
	if appCalls != 1 {
		t.Fatalf("idle RotateCookie called /app (%d), want 1", appCalls)
	}

	// 3) blAt > 6 h and active: RefreshModels triggers opportunistic /app refresh and updates bl.
	c.RecordUserActivity()
	if err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels(>6h): %v", err)
	}
	if appCalls != 2 || c.CurrentBL() != "boq_gemini-web-uiserver_20261118.09_p0" {
		t.Fatalf("expected /app refresh after 6h, appCalls=%d bl=%q", appCalls, c.CurrentBL())
	}

	// 4) Non-fatal /app error + 30 min anti-spam cooldown via RotateCookie.
	curNow = curNow.Add(7 * time.Hour)
	c.RecordUserActivity()
	appStatus = 500
	curBL = "boq_gemini-web-uiserver_20261119.01_p0"
	if err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels must not fail on /app 500: %v", err)
	}
	if appCalls != 3 || c.CurrentBL() != "boq_gemini-web-uiserver_20261118.09_p0" {
		t.Fatalf("after 500: appCalls=%d bl=%q", appCalls, c.CurrentBL())
	}
	// Inside 30m cooldown: RotateCookie must NOT hit /app again.
	curNow = curNow.Add(15 * time.Minute)
	c.RecordUserActivity()
	appStatus = 200
	c.RotateCookie(context.Background())
	if appCalls != 3 {
		t.Fatalf("cooldown violated on RotateCookie: appCalls=%d, want 3", appCalls)
	}
	// After 30m cooldown elapses: RotateCookie refreshes bl cleanly.
	curNow = curNow.Add(20 * time.Minute)
	c.RecordUserActivity()
	c.RotateCookie(context.Background())
	if appCalls != 4 || c.CurrentBL() != "boq_gemini-web-uiserver_20261119.01_p0" {
		t.Fatalf("after cooldown RotateCookie: appCalls=%d bl=%q", appCalls, c.CurrentBL())
	}
}
