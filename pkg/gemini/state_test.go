package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
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
