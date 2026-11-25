package main

// K38 test suite: diagnostics ring persistence to disk (so past events survive a
// proxy restart), ring bounding, atomic 0600 writes, and DiagFile path derivation.
// Everything is offline and deterministic.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK38_DiagFilePathDerivation(t *testing.T) {
	if got := diagFilePath("off", "/x/cookie.state.json"); got != "" {
		t.Errorf(`diagFilePath("off", ...) = %q, want ""`, got)
	}
	if got := diagFilePath("/explicit/diag.json", "/x/cookie.state.json"); got != "/explicit/diag.json" {
		t.Errorf("explicit path = %q, want /explicit/diag.json", got)
	}
	if got := diagFilePath("", "/x/cookie.state.json"); got != "/x/cookie.diagnostics.json" {
		t.Errorf("derived path = %q, want /x/cookie.diagnostics.json", got)
	}
	if got := diagFilePath("", ""); got != "" {
		t.Errorf("no state file = %q, want empty", got)
	}
}

func TestK38_DiagRingPersistenceAndBounding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diag.json")

	// Record cap+5 events so bounding is exercised while saving.
	r := newDiagRing(100, path)
	for i := 0; i < 105; i++ {
		r.record(DiagEvent{
			TS:        "2026-11-20T00:00:00Z",
			Kind:      "activity",
			RequestID: "req-" + strconv.Itoa(i),
			Status:    200,
		})
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("persisted diag file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("diag file perm = %o, want 600", fi.Mode().Perm())
	}

	// Reload into a fresh ring (simulates a proxy restart).
	r2 := newDiagRing(100, path)
	r2.load()
	resp := r2.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now())
	if resp.Total != 100 {
		t.Fatalf("after reload total = %d, want 100", resp.Total)
	}
	if resp.Items[0].RequestID != "req-104" {
		t.Errorf("newest = %s, want req-104", resp.Items[0].RequestID)
	}
	if resp.Items[resp.Total-1].RequestID != "req-5" {
		t.Errorf("oldest kept = %s, want req-5 (first 5 dropped by bounding)", resp.Items[resp.Total-1].RequestID)
	}
}

func TestK38_DiagRingLoadNoFileOrCorrupt(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> empty ring.
	r := newDiagRing(100, filepath.Join(dir, "missing.json"))
	r.load()
	if got := r.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now()); got.Total != 0 {
		t.Errorf("missing file total = %d, want 0", got.Total)
	}

	// Corrupt file -> empty ring, no panic.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := newDiagRing(100, bad)
	r2.load()
	if got := r2.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now()); got.Total != 0 {
		t.Errorf("corrupt file total = %d, want 0", got.Total)
	}
}

func TestK38_DiagSeedFromCalls(t *testing.T) {
	calls := []gemini.CallRecord{
		{Time: "2026-11-20T05:00:00Z", RequestID: "c3", Model: "gemini-x", ServedName: "X", FirstTextMS: 120, DurationMS: 900, PromptBytes: 100},
		{Time: "2026-11-20T04:00:00Z", RequestID: "c2", Model: "gemini-y", ServedName: "Y", FirstTextMS: -1, FirstThoughtMS: 80, DurationMS: 500, PromptBytes: 200},
		{Time: "2026-11-20T03:00:00Z", RequestID: "c1", Model: "gemini-z", Error: "timeout"},
	}

	path := filepath.Join(t.TempDir(), "diag.json")
	r := newDiagRing(100, path)
	r.seed(calls) // newest-first input, oldest-first storage

	resp := r.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now())
	if resp.Total != 3 {
		t.Fatalf("seed total = %d, want 3", resp.Total)
	}
	// Newest-first output.
	if resp.Items[0].RequestID != "c3" || resp.Items[1].RequestID != "c2" || resp.Items[2].RequestID != "c1" {
		t.Errorf("order = %s,%s,%s, want c3,c2,c1", resp.Items[0].RequestID, resp.Items[1].RequestID, resp.Items[2].RequestID)
	}
	if resp.Items[1].TTFTMS != 80 {
		t.Errorf("TTFT fallback to FirstThoughtMS = %d, want 80", resp.Items[1].TTFTMS)
	}
	if resp.Items[2].Outcome != "error" || resp.Items[2].Status != 500 {
		t.Errorf("error mapping = status %d outcome %q, want 500/error", resp.Items[2].Status, resp.Items[2].Outcome)
	}

	// Reload (simulates restart): seed must not duplicate.
	r2 := newDiagRing(100, path)
	r2.load()
	r2.seed(calls)
	if got := r2.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now()).Total; got != 3 {
		t.Errorf("after reload+reseed total = %d, want 3 (no duplicates)", got)
	}

	// Bounding when the journal is larger than the ring.
	r3 := newDiagRing(2, filepath.Join(t.TempDir(), "diag-small.json"))
	r3.seed(calls)
	got := r3.query(diagQuery{Kind: "all", Window: "all", Limit: 200}, time.Now())
	if got.Total != 2 || got.Items[0].RequestID != "c3" || got.Items[1].RequestID != "c2" {
		t.Errorf("bounded seed = %v, want newest c3,c2", k37IDs(got))
	}
}
