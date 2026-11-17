package gemini

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenLedgerPersistenceAndPastCallsBackfill(t *testing.T) {
	dir := t.TempDir()
	callsPath := filepath.Join(dir, "cookie.txt.calls.jsonl")
	tokensPath := filepath.Join(dir, "cookie.txt.tokens.jsonl")

	now := time.Now().UTC().Truncate(time.Second)
	pastWithin5h := now.Add(-2 * time.Hour).Format(time.RFC3339)
	pastWithin12h := now.Add(-8 * time.Hour).Format(time.RFC3339)
	pastOlder := now.Add(-20 * time.Hour).Format(time.RFC3339)

	// Legacy calls written earlier without token fields (e.g. before upgrade).
	legacy := []CallRecord{
		{
			Time:         pastWithin5h,
			RequestID:    "req-pro-ok",
			Model:        "gemini-3.1-pro",
			ModelID:      "e6fa609c3fa255c0",
			ServedID:     "e6fa609c3fa255c0",
			ServedName:   "3.1 Pro",
			Identity:     "confirmed",
			PromptBytes:  4000,
			ThoughtBytes: 800,
			TextBytes:    400,
			ImageCount:   1,
		},
		{
			Time:         pastWithin12h,
			RequestID:    "req-fallback",
			Model:        "gemini-3.1-pro",
			ModelID:      "e6fa609c3fa255c0",
			ServedID:     "cccccccccccccccc",
			ServedName:   "3.5 Flash",
			Identity:     "other_model",
			PromptBytes:  2000,
			ThoughtBytes: 0,
			TextBytes:    400,
		},
		{
			Time:        pastOlder,
			RequestID:   "req-nostate",
			Model:       "gemini-3.5-flash",
			Identity:    "not_stated",
			PromptBytes: 1000,
			TextBytes:   200,
		},
	}
	var raw strings.Builder
	for _, r := range legacy {
		b, _ := json.Marshal(r)
		raw.Write(b)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(callsPath, []byte(raw.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	c, err := New(Config{
		Cookie:     "SID=test",
		CallsFile:  callsPath,
		TokensFile: tokensPath,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Verify backfill happened onto tokensPath with 0600 permissions.
	info, err := os.Stat(tokensPath)
	if err != nil {
		t.Fatalf("expected tokens file created at startup: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("tokens file perm = %v, want 0600", info.Mode().Perm())
	}

	snap := c.TokenUsageSummary(now)
	if snap.RecordsCount != 3 || !snap.Persisted {
		t.Fatalf("unexpected initial snapshot: %+v", snap)
	}
	// req-pro-ok: 4000/4 + 258 + 800/4 + 400/4 = 1000 + 258 + 200 + 100 = 1558
	if snap.Totals5h.TotalTokens != 1558 || snap.Totals5h.Requests != 1 {
		t.Errorf("Totals5h = %+v, want 1558 tokens / 1 req", snap.Totals5h)
	}
	// 12h adds req-fallback (500 + 100 = 600) => 2158
	if snap.Totals12h.TotalTokens != 2158 || snap.Totals12h.Requests != 2 {
		t.Errorf("Totals12h = %+v, want 2158 tokens / 2 req", snap.Totals12h)
	}
	// All adds req-nostate (250 + 50 = 300) => 2458
	if snap.TotalsAll.TotalTokens != 2458 || snap.TotalsAll.Requests != 3 {
		t.Errorf("TotalsAll = %+v, want 2458 tokens / 3 req", snap.TotalsAll)
	}

	// Incremental new call appends immediately to tokensPath and credits Google's indicated model.
	pro := &AvailableModel{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0"}
	rec := newCallRecord(&CallMetrics{ID: "req-live"}, GenParams{Prompt: strings.Repeat("a", 800), Model: pro}, now)
	rec.TextBytes = 400
	rec.TextTokens = 100
	c.recordCall(rec, &idScan{servedID: "cccccccccccccccc", servedName: "3.5 Flash"})

	// Reload into a fresh Client using only TokensFile (proving incremental persistence survives restarts).
	c2, err := New(Config{
		Cookie:     "SID=test",
		TokensFile: tokensPath,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	snap2 := c2.TokenUsageSummary(now)
	if snap2.RecordsCount != 4 {
		t.Fatalf("reloaded RecordsCount = %d, want 4", snap2.RecordsCount)
	}
	// req-live added 200 + 100 = 300 tokens attributed to Google-indicated "3.5 Flash"
	var flashSummary *TokenModelSummary
	for i := range snap2.ByGoogleModel {
		if snap2.ByGoogleModel[i].EffectiveModel == "3.5 Flash" && snap2.ByGoogleModel[i].GoogleIndicated {
			flashSummary = &snap2.ByGoogleModel[i]
		}
	}
	if flashSummary == nil {
		t.Fatalf("missing Google-indicated 3.5 Flash group: %+v", snap2.ByGoogleModel)
	}
	if flashSummary.Window5h.TotalTokens != 300 || flashSummary.AllSaved.TotalTokens != 900 {
		t.Errorf("3.5 Flash totals = 5h:%d all:%d, want 300 / 900", flashSummary.Window5h.TotalTokens, flashSummary.AllSaved.TotalTokens)
	}
}
