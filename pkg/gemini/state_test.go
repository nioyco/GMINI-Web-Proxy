package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stateClient builds a file-backed client with a state file in dir.
func stateClient(t *testing.T, dir string, rt http.RoundTripper) (*Client, *bytes.Buffer) {
	t.Helper()
	cookiePath := filepath.Join(dir, "cookie.txt")
	if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
		if err := os.WriteFile(cookiePath, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	c, err := New(Config{CookieFile: cookiePath, StateFile: cookiePath + ".state.json"}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	return c, &logs
}

func sessionRT(t *testing.T, log *callLog) rtFunc {
	body := listModelsBody(t, "m1", "m2")
	return func(r *http.Request) (*http.Response, error) {
		log.add(r)
		switch familyOf(r.URL) {
		case FamilyApp:
			return resp200(r, appPageAuth), nil
		case FamilyBatch:
			return resp200(r, body), nil
		}
		return resp200(r, ""), nil
	}
}

func TestStateFileWritten0600Atomically(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c, logs := stateClient(t, dir, sessionRT(t, &log))
	if got := c.CheckAuth(context.Background()); got != "authenticated" {
		t.Fatalf("auth = %q", got)
	}
	_ = c.fetchModels(context.Background())
	path := c.cfg.StateFile
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	var st persistedState
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.At != "AT_TOKEN_1" || st.AtAt.IsZero() || st.BL == "" || len(st.Models) != 2 || st.ModelsAt.IsZero() || st.Session == "" {
		t.Errorf("state = %+v", st)
	}
	if strings.Contains(logs.String(), "AT_TOKEN_1") || st.Session == "s" {
		t.Errorf("token or SAPISID leaked")
	}
}

// AUDIT-13 A5: a restart with a recent state makes no /app (and no listModels) call.
func TestRestartWithRecentStateNoAppCall(t *testing.T) {
	dir := t.TempDir()
	var log1 callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log1))
	c1.CheckAuth(context.Background())
	_ = c1.fetchModels(context.Background())

	var log2 callLog
	c2, logs := stateClient(t, dir, sessionRT(t, &log2))
	r := c2.RestoreState()
	if !r.SessionFresh || !r.CatalogRestored {
		t.Fatalf("restore = %+v\n%s", r, logs)
	}
	if !c2.Authenticated() || len(c2.ModelNames()) != 2 || c2.ModelsSourceLabel() != "persisted" || !strings.HasPrefix(c2.CurrentBL(), "boq_") {
		t.Errorf("auth=%v models=%v source=%q bl=%q", c2.Authenticated(), c2.ModelNames(), c2.ModelsSourceLabel(), c2.CurrentBL())
	}
	if ss := c2.SessionState(); ss.CatalogStale || ss.TokenAt.IsZero() || ss.CatalogAt.IsZero() {
		t.Errorf("session state = %+v", ss)
	}
	if log2.count("") != 0 {
		t.Errorf("%d Google calls on restart with a recent state, want 0", log2.count(""))
	}
	if strings.Contains(logs.String(), "AT_TOKEN_1") {
		t.Error("token logged")
	}
}

func TestOldStateRestoresCatalogNotToken(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log))
	c1.CheckAuth(context.Background())
	_ = c1.fetchModels(context.Background())
	c2, _ := stateClient(t, dir, sessionRT(t, &log))
	c2.cfg.StateMaxAge = time.Nanosecond
	r := c2.RestoreState()
	if r.SessionFresh || !r.CatalogRestored || c2.Authenticated() {
		t.Fatalf("restore = %+v auth=%v", r, c2.Authenticated())
	}
	// Fallback mode: the last persisted catalog is served, marked stale.
	if !c2.SessionState().CatalogStale || c2.ModelsSourceLabel() != "persisted" {
		t.Errorf("state = %+v source=%q", c2.SessionState(), c2.ModelsSourceLabel())
	}
}

func TestStateOfAnotherSessionIgnored(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log))
	c1.CheckAuth(context.Background())
	if err := os.WriteFile(filepath.Join(dir, "cookie.txt"), []byte("SAPISID=other; __Secure-1PSIDTS=ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, _ := stateClient(t, dir, sessionRT(t, &log))
	if r := c2.RestoreState(); r.SessionFresh || r.CatalogRestored || c2.Authenticated() {
		t.Errorf("restored a foreign session: %+v", r)
	}
}

func TestStateDisabledForInlineCookie(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	c, err := New(Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=ts", StateFile: path}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var log callLog
	c.SetTransport(sessionRT(t, &log))
	c.CheckAuth(context.Background())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("state written for an inline cookie: %v", err)
	}
	if c.SessionState().Persisted {
		t.Error("persisted must be false for an inline cookie")
	}
}

func TestStateWidePermissionsTightened(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log))
	c1.CheckAuth(context.Background())
	if err := os.Chmod(c1.cfg.StateFile, 0o644); err != nil {
		t.Fatal(err)
	}
	c2, _ := stateClient(t, dir, sessionRT(t, &log))
	c2.RestoreState()
	info, _ := os.Stat(c1.cfg.StateFile)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestDeadSessionDropsPersistedToken(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log))
	c1.CheckAuth(context.Background())
	c1.setAt("")
	c1.saveState()
	data, _ := os.ReadFile(c1.cfg.StateFile)
	if strings.Contains(string(data), "AT_TOKEN_1") {
		t.Error("a dropped token stays on disk")
	}
}

// TestRestoreStateFromBrowserScriptFormat verifies that a .state.json emitted by
// recuperer_cookie_gemini.mjs (with SNlM0e/cfb2h and SAPISID fingerprint, even
// before any catalog has been fetched) is accepted by RestoreState and skips /app.
func TestRestoreStateFromBrowserScriptFormat(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(cookiePath, []byte("__Secure-1PSID=p1; __Secure-1PSIDTS=ts1; SAPISID=sapi-browser-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("sapi-browser-1"))
	fp := hex.EncodeToString(sum[:])[:16]
	now := time.Now().UTC().Format(time.RFC3339)

	// Exact schema written by recuperer_cookie_gemini.mjs writeSessionStateFromPage.
	payload := map[string]any{
		"version":             1,
		"saved_at":            now,
		"session_fingerprint": fp,
		"at":                  "SCRIPT_AT_TOKEN",
		"at_at":               now,
		"bl":                  "boq_script_20261115",
		"bl_at":               now,
		"psidts_at":           now,
		"psidts_hash":         fp,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	statePath := cookiePath + ".state.json"
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var log callLog
	var logs bytes.Buffer
	c, err := New(Config{
		CookieFile:   cookiePath,
		StateFile:    statePath,
		GeminiBLAuto: true,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(sessionRT(t, &log))

	r := c.RestoreState()
	if !r.SessionFresh || r.CatalogRestored || !c.Authenticated() {
		t.Fatalf("RestoreState = %+v auth=%v logs=%s", r, c.Authenticated(), logs.String())
	}
	if c.CurrentBL() != "boq_script_20261115" {
		t.Errorf("CurrentBL = %q, want boq_script_20261115", c.CurrentBL())
	}
	c.ResolveBuildLabel(context.Background())
	if got := log.count("/app"); got != 0 {
		t.Errorf("expected 0 /app calls after browser script state restore, got %d", got)
	}
}

// TestCheckAuthBlockedOrErrorPreservesPersistedTokenAndBL proves R&D-14 R4 (C2a):
// a /sorry refusal or network failure during verification must never clear the cached
// at/bl in memory or in .state.json; only a 200 response proving logout (no SNlM0e) clears at.
func TestCheckAuthBlockedOrErrorPreservesPersistedTokenAndBL(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(cookiePath, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := cookiePath + ".state.json"

	var mode string // "auth" | "sorry" | "neterr" | "logout"
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		switch mode {
		case "sorry":
			h := http.Header{}
			h.Set("Location", "https://www.google.com/sorry/index?continue=x")
			return &http.Response{StatusCode: 302, Header: h, Body: http.NoBody, Request: r}, nil
		case "neterr":
			return nil, errors.New("dial tcp: connection refused")
		case "logout":
			return resp200(r, appPageAnon), nil
		default:
			return resp200(r, appPageAuth), nil
		}
	})

	var logs bytes.Buffer
	c, err := New(Config{CookieFile: cookiePath, StateFile: statePath, GeminiBLAuto: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	mode = "auth"
	if got := c.CheckAuth(context.Background()); got != "authenticated" {
		t.Fatalf("initial CheckAuth = %q", got)
	}
	wantBL := c.CurrentBL()

	for _, failMode := range []string{"sorry", "neterr"} {
		mode = failMode
		c.invalidateAppPage()
		got := c.CheckAuth(context.Background())
		if !strings.HasPrefix(got, "unknown") {
			t.Fatalf("mode=%s CheckAuth=%q, want unknown", failMode, got)
		}
		if c.currentAt() != "AT_TOKEN_1" || c.CurrentBL() != wantBL || !c.Authenticated() {
			t.Fatalf("mode=%s cleared cached session: at=%q bl=%q", failMode, c.currentAt(), c.CurrentBL())
		}
		raw, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "AT_TOKEN_1") || !strings.Contains(string(raw), wantBL) {
			t.Fatalf("mode=%s wiped persisted state on disk", failMode)
		}
	}

	// Clear the simulated /app block cleanly via recordGoogleOK, then verify explicit signed-out 200 clears at.
	c.recordGoogleOK(FamilyApp)
	mode = "logout"
	c.invalidateAppPage()
	if got := c.CheckAuth(context.Background()); got != "NOT authenticated" {
		t.Fatalf("logout CheckAuth = %q", got)
	}
	if c.Authenticated() || c.currentAt() != "" {
		t.Fatal("signed-out /app page must clear at")
	}
}

// K4: A running Client reloads both cookie.txt and .state.json (at, bl, psidts_at)
// written externally (by recuperer_cookie_gemini.mjs) without restarting, clears
// rotation suspension, and works both when cookie.txt changes and when only
// .state.json is refreshed.
func TestRunningClientHotReloadsCookieAndStateFile(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	statePath := cookiePath + ".state.json"

	writePair := func(sapi, ts, at, bl string, tsAt time.Time, bumpTime time.Time) {
		cookie := "__Secure-1PSID=p1; __Secure-1PSIDTS=" + ts + "; SAPISID=" + sapi
		if err := writeFileAtomic(cookiePath, []byte(cookie), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(cookiePath, bumpTime, bumpTime)

		sSum := sha256.Sum256([]byte(sapi))
		tsSum := sha256.Sum256([]byte(ts))
		raw, _ := json.Marshal(map[string]any{
			"version":             1,
			"saved_at":            bumpTime.UTC().Format(time.RFC3339),
			"session_fingerprint": hex.EncodeToString(sSum[:8]),
			"at":                  at,
			"at_at":               time.Now().UTC().Format(time.RFC3339),
			"bl":                  bl,
			"bl_at":               bumpTime.UTC().Format(time.RFC3339),
			"psidts_at":           tsAt.UTC().Format(time.RFC3339),
			"psidts_hash":         hex.EncodeToString(tsSum[:8]),
		})
		if err := writeFileAtomic(statePath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(statePath, bumpTime, bumpTime)
	}

	t1 := time.Now().Add(-2 * time.Hour)
	writePair("sapi-1", "ts-1", "AT_1", "bl_1", t1, t1)

	var gotURL, gotAT, gotCookie string
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Host, "accounts.google.com") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		gotURL = r.URL.String()
		gotCookie = r.Header.Get("Cookie")
		b, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(b))
		gotAT = vals.Get("at")
		return resp200(r, ")]}'\n\n50\n"+servedLine("e6fa609c3fa255c0", "3.1 Pro")+"\n"), nil
	})

	c, err := New(Config{
		CookieFile:         cookiePath,
		StateFile:          statePath,
		GeminiBLAuto:       true,
		RotateSuspendAfter: 3,
		RetryAttempts:      1,
		RequestTimeout:     5,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	c.RestoreState()

	// Simulate 3 no_new_token rotations suspending auto-rotation.
	for i := 0; i < 3; i++ {
		c.RotateCookie(context.Background())
	}
	if !c.RotationStatus().Suspended {
		t.Fatal("expected rotation to be suspended after 3 no_new_token")
	}

	// External update: new cookie + new state written while client stays running.
	t2 := time.Now().Add(-10 * time.Second)
	writePair("sapi-1", "ts-2", "AT_2", "bl_2", t2, t2)

	if _, err := c.Generate(context.Background(), GenParams{Prompt: "ping"}); err != nil {
		t.Fatalf("Generate after external update: %v", err)
	}
	if gotAT != "AT_2" || !strings.Contains(gotURL, "bl=bl_2") || !strings.Contains(gotCookie, "ts-2") {
		t.Fatalf("running client used stale auth/bl/cookie: at=%q url=%q cookie=%q", gotAT, gotURL, gotCookie)
	}
	rs := c.RotationStatus()
	if rs.Suspended || rs.TokenAgeSource != "state" || rs.TokenAge > time.Minute {
		t.Fatalf("rotation status not updated after hot-reload: %+v", rs)
	}

	// External update of ONLY .state.json (cookie.txt unchanged): must still reload at & bl.
	t3 := time.Now()
	sSum := sha256.Sum256([]byte("sapi-1"))
	tsSum := sha256.Sum256([]byte("ts-2"))
	raw3, _ := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            t3.UTC().Format(time.RFC3339),
		"session_fingerprint": hex.EncodeToString(sSum[:8]),
		"at":                  "AT_3",
		"at_at":               t3.UTC().Format(time.RFC3339),
		"bl":                  "bl_3",
		"bl_at":               t3.UTC().Format(time.RFC3339),
		"psidts_at":           t3.UTC().Format(time.RFC3339),
		"psidts_hash":         hex.EncodeToString(tsSum[:8]),
	})
	if err := writeFileAtomic(statePath, raw3, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(statePath, t3, t3)
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "pong"}); err != nil {
		t.Fatalf("Generate after state-only update: %v", err)
	}
	if gotAT != "AT_3" || !strings.Contains(gotURL, "bl=bl_3") {
		t.Fatalf("state-only hot reload missed: at=%q url=%q", gotAT, gotURL)
	}
}

func TestN8_CatalogRestoredAcrossSessionChangeWithin7Days(t *testing.T) {
	dir := t.TempDir()
	var log callLog
	c1, _ := stateClient(t, dir, sessionRT(t, &log))
	c1.CheckAuth(context.Background())
	if err := c1.fetchModels(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Switch to a different Google session (different SAPISID).
	if err := os.WriteFile(filepath.Join(dir, "cookie.txt"), []byte("SAPISID=new_session; __Secure-1PSIDTS=ts2"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Case 1: catalog < 24h old -> restored across session change, token NOT restored.
	c2, _ := stateClient(t, dir, sessionRT(t, &log))
	r := c2.RestoreState()
	if r.SessionFresh || c2.Authenticated() {
		t.Fatalf("token must not be restored across session change: %+v auth=%v", r, c2.Authenticated())
	}
	if !r.CatalogRestored || !r.CatalogFresh || len(c2.ModelNames()) != 2 {
		t.Fatalf("expected <24h catalog restored across session change, got %+v models=%v", r, c2.ModelNames())
	}

	// Case 2: catalog 3 days old (< 7 days, >= 24h) -> CatalogRestored=true, CatalogFresh=false.
	raw, _ := os.ReadFile(c1.cfg.StateFile)
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	st.ModelsAt = time.Now().Add(-72 * time.Hour)
	b, _ := json.Marshal(st)
	if err := os.WriteFile(c1.cfg.StateFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c3, _ := stateClient(t, dir, sessionRT(t, &log))
	r3 := c3.RestoreState()
	if !r3.CatalogRestored || r3.CatalogFresh || len(c3.ModelNames()) != 2 {
		t.Fatalf("expected 3d catalog restored with CatalogFresh=false, got %+v models=%v", r3, c3.ModelNames())
	}

	// Case 3: catalog 8 days old (>= 7 days) -> NOT restored.
	st.ModelsAt = time.Now().Add(-8 * 24 * time.Hour)
	b, _ = json.Marshal(st)
	if err := os.WriteFile(c1.cfg.StateFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c4, _ := stateClient(t, dir, sessionRT(t, &log))
	r4 := c4.RestoreState()
	if r4.CatalogRestored || r4.CatalogFresh || len(c4.ModelNames()) != 0 {
		t.Fatalf("expected >=7d catalog NOT restored, got %+v models=%v", r4, c4.ModelNames())
	}
}
