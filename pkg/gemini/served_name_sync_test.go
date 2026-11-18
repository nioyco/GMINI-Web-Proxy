package gemini

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestK20_ConfirmedServedNameSyncsCatalogAndSurvivesRefresh(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	statePath := filepath.Join(dir, "cookie.txt.state.json")
	callsPath := filepath.Join(dir, "cookie.txt.calls.jsonl")
	if err := os.WriteFile(cookiePath, []byte("__Secure-1PSID=abc; __Secure-1PSIDTS=ts1"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	c, err := New(Config{
		CookieFile: cookiePath,
		StateFile:  statePath,
		CallsFile:  callsPath,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate otAQ7b returning "4 Carbon" for 95a4c0abcba24364.
	c.storeModels([]*AvailableModel{
		{Name: "gemini-most-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Carbon", Family: "Carbon4p0", ModeCode: 8},
	}, "dynamic")

	// A real confirmed StreamGenerate reply states inner[39]=95a4c0abcba24364, inner[42]="4 Argon".
	m, err := c.ResolveModel("gemini-most-complex-work")
	if err != nil {
		t.Fatal(err)
	}
	rec := newCallRecord(&CallMetrics{ID: "req-argon"}, GenParams{Prompt: "hello", Model: m}, time.Now())
	rec.TextBytes = 100
	c.recordCall(rec, &idScan{servedID: "95a4c0abcba24364", servedName: "4 Argon"})

	got, err := c.ResolveModel("gemini-most-complex-work")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "4 Argon" {
		t.Fatalf("catalog Description after confirmed call = %q, want %q", got.Description, "4 Argon")
	}

	// Subsequent otAQ7b refresh still returning "4 Carbon" must keep "4 Argon".
	c.storeModels([]*AvailableModel{
		{Name: "gemini-most-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Carbon", Family: "Carbon4p0", ModeCode: 8},
	}, "dynamic")
	got2, _ := c.ResolveModel("gemini-most-complex-work")
	if got2.Description != "4 Argon" {
		t.Fatalf("catalog Description after subsequent otAQ7b refresh = %q, want %q", got2.Description, "4 Argon")
	}

	// Fresh client startup with legacy persisted state saying "4 Carbon" + calls.jsonl saying "4 Argon".
	legacyState := persistedState{
		Version:  stateVersion,
		SavedAt:  time.Now().UTC(),
		Session:  sessionFingerprint("__Secure-1PSID=abc; __Secure-1PSIDTS=ts1"),
		At:       "tok",
		AtAt:     time.Now().UTC(),
		ModelsAt: time.Now().UTC(),
		Models: []*AvailableModel{
			{Name: "gemini-most-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Carbon", Family: "Carbon4p0", ModeCode: 8},
		},
	}
	raw, _ := json.Marshal(legacyState)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c2, err := New(Config{
		CookieFile: cookiePath,
		StateFile:  statePath,
		CallsFile:  callsPath,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c2.RestoreState()
	got3, err := c2.ResolveModel("gemini-most-complex-work")
	if err != nil {
		t.Fatal(err)
	}
	if got3.Description != "4 Argon" {
		t.Fatalf("restarted client Description = %q, want %q", got3.Description, "4 Argon")
	}
}
