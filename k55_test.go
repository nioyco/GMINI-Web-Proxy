package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK55_CallRecordBinaryTagPersistedExposedAndSeeded(t *testing.T) {
	t.Setenv("GEMINI_TOOLS_DB_FILE", filepath.Join(t.TempDir(), "tools_db.jsonl"))
	srv := newK42TestServer(t, nil)
	srv.client.SetBinaryTag("K55")
	if got := srv.client.BinaryTag(); got != "K55" {
		t.Fatalf("client.BinaryTag() = %q, want K55", got)
	}

	calls := []gemini.CallRecord{
		{
			Time:       time.Now().UTC().Format(time.RFC3339),
			RequestID:  "req-k55-tag-1",
			BinaryTag:  srv.client.BinaryTag(),
			Model:      "3-flash",
			ServedName: "Gemini 3 Flash",
			DurationMS: 420,
			TextBytes:  120,
		},
	}

	raw, err := json.Marshal(calls[0])
	if err != nil {
		t.Fatalf("json.Marshal(CallRecord): %v", err)
	}
	if !strings.Contains(string(raw), `"binary_tag":"K55"`) {
		t.Fatalf("CallRecord JSON missing binary_tag=K55: %s", raw)
	}

	var roundTrip gemini.CallRecord
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal(CallRecord): %v", err)
	}
	if roundTrip.BinaryTag != "K55" {
		t.Fatalf("roundTrip.BinaryTag = %q, want K55", roundTrip.BinaryTag)
	}

	ring := newDiagRing(16, filepath.Join(t.TempDir(), "diag.jsonl"))
	ring.seed(calls)
	resp := ring.query(diagQuery{Window: "1h", Limit: 10}, time.Now())
	if resp.Total != 1 || len(resp.Items) != 1 {
		t.Fatalf("diagRing.seed() count = (%d, %d), want (1, 1)", resp.Total, len(resp.Items))
	}
	if resp.Items[0].BinaryTag != "K55" {
		t.Fatalf("seeded DiagEvent.BinaryTag = %q, want K55", resp.Items[0].BinaryTag)
	}

	// Verify /_ui/api/state includes binary_tag on calls when present.
	srv.cfg.UIEnabled = true
	srv.uiToken = "tok-k55"
	h := srv.handler()
	w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", srv.uiToken)
	if w.Code != http.StatusOK {
		t.Fatalf("/_ui/api/state status = %d", w.Code)
	}
}

func TestK55_WebUIv309DedupAttemptsAndDiagReqCol(t *testing.T) {
	if uiVersion != "3.0.9" {
		t.Fatalf("uiVersion = %q, want 3.0.9", uiVersion)
	}
	html := string(uiHTML)
	for _, want := range []string{
		"callsByReq",
		"attemptsTrail",
		"outcomeSummaryBadge",
		"OK après retry",
		"récupérée",
		"échec",
		"Récupérée (",
		"<th>Heure</th><th>Requête</th><th>Version</th><th>Type / Statut</th>",
		"incByReq",
		`badge("none","Activité")`,
		`badge("warn","Incident")`,
		`badge("ok","direct")`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}
