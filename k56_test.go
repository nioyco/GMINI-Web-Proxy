package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK56_ApplyCookieFileOverrideDerivesDiagFile(t *testing.T) {
	t.Setenv("GEMINI_STATE_FILE", "")
	t.Setenv("GEMINI_CALLS_FILE", "")
	t.Setenv("GEMINI_TOKENS_FILE", "")
	t.Setenv("GEMINI_FLIGHTS_FILE", "")
	t.Setenv("GEMINI_DIAGNOSTICS_FILE", "")

	var cfg Config
	cookiePath := filepath.Join(t.TempDir(), "cookie.txt")
	applyCookieFileOverride(&cfg, cookiePath)

	wantState := cookiePath + ".state.json"
	if cfg.Gemini.StateFile != wantState {
		t.Fatalf("StateFile = %q, want %q", cfg.Gemini.StateFile, wantState)
	}
	wantDiag := cookiePath + ".diagnostics.json"
	if cfg.DiagFile != wantDiag {
		t.Fatalf("DiagFile = %q, want %q", cfg.DiagFile, wantDiag)
	}
	if cfg.FlightsFile != cookiePath+".flights.json" {
		t.Fatalf("FlightsFile = %q, want %q", cfg.FlightsFile, cookiePath+".flights.json")
	}
}

func TestK56_SelectRecentCallsKeepsMin200Over48Hours(t *testing.T) {
	now := time.Now().UTC()

	// Case 1: 220 calls all older than 48h -> must return at least 200 newest-first.
	oldCalls := make([]gemini.CallRecord, 220)
	for i := range oldCalls {
		oldCalls[i] = gemini.CallRecord{
			Time:      now.Add(-time.Duration(49+i) * time.Hour).Format(time.RFC3339),
			RequestID: fmt.Sprintf("old-%03d", i),
		}
	}
	gotOld := selectRecentCalls(oldCalls, recentCallsMinCount, recentCallsHistoryWindow, now)
	if len(gotOld) != 200 {
		t.Fatalf("selectRecentCalls(>48h) len = %d, want 200", len(gotOld))
	}
	if gotOld[0].RequestID != "old-000" || gotOld[199].RequestID != "old-199" {
		t.Fatalf("selectRecentCalls(>48h) order mismatch: first=%s last=%s", gotOld[0].RequestID, gotOld[199].RequestID)
	}

	// Case 2: 230 calls within 12h + 20 older -> must return all 230 within 12h.
	mixed := make([]gemini.CallRecord, 250)
	for i := range mixed {
		age := time.Duration(i) * time.Minute
		if i >= 230 {
			age = 72 * time.Hour
		}
		mixed[i] = gemini.CallRecord{
			Time:      now.Add(-age).Format(time.RFC3339),
			RequestID: fmt.Sprintf("mix-%03d", i),
		}
	}
	gotMixed := selectRecentCalls(mixed, recentCallsMinCount, recentCallsHistoryWindow, now)
	if len(gotMixed) != 230 {
		t.Fatalf("selectRecentCalls(230 in 12h) len = %d, want 230", len(gotMixed))
	}
}

func TestK56_PruneInFlightEntriesKeepsMin200AcrossManyConvs(t *testing.T) {
	// 250 distinct conversations (1 entry each): first 200 must be kept despite > 20 convs,
	// and entries beyond 200 with new conversations are evicted.
	combined := make([]InFlightRequest, 250)
	for i := range combined {
		combined[i] = InFlightRequest{
			ID:     fmt.Sprintf("req-%03d", i),
			ConvID: fmt.Sprintf("conv-%03d", i),
			State:  "Terminée",
		}
	}
	kept := pruneInFlightEntries(combined)
	if len(kept) != 200 {
		t.Fatalf("pruneInFlightEntries len = %d, want 200", len(kept))
	}
	if kept[0].ID != "req-000" || kept[199].ID != "req-199" {
		t.Fatalf("kept bounds = (%s..%s), want (req-000..req-199)", kept[0].ID, kept[199].ID)
	}
}

func TestK56_DiagRingAllWindowReturns200OldEvents(t *testing.T) {
	ring := newDiagRing(500, filepath.Join(t.TempDir(), "diag.jsonl"))
	now := time.Now().UTC()
	for i := 0; i < 210; i++ {
		ring.record(DiagEvent{
			TS:        now.Add(-time.Duration(72+i) * time.Hour).Format(time.RFC3339),
			Kind:      "activity",
			RequestID: fmt.Sprintf("diag-old-%03d", i),
			Status:    200,
			Outcome:   "text_only",
		})
	}
	resp := ring.query(diagQuery{Window: "all", Limit: 200}, now)
	if resp.Total != 210 || len(resp.Items) != 200 {
		t.Fatalf("diagRing.query(all, 200) = (%d total, %d items), want (210, 200)", resp.Total, len(resp.Items))
	}
}

func TestK56_WebUIv3010DefaultsAndHeaders(t *testing.T) {
	if uiVersion != "3.0.10" {
		t.Fatalf("uiVersion = %q, want 3.0.10", uiVersion)
	}
	html := string(uiHTML)
	for _, want := range []string{
		`value="all" selected`,
		`window:"all"`,
		`Demandes &amp; Appels (temps réel + historique ≥ 200)`,
		`min. 200, max 500`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
	if strings.Contains(html, `Demandes &amp; Appels (temps réel + 12 h)`) {
		t.Errorf("ui.html still contains legacy '+ 12 h' title")
	}
}
