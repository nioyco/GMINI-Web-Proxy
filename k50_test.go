package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK50_WebUIv307VersionColsLimitAndTempChatAlert(t *testing.T) {
	if uiVersion == "" {
		t.Errorf("uiVersion must not be empty")
	}
	html := string(uiHTML)
	for _, want := range []string{
		"<th>Heure début</th><th>Conv.</th><th>Version</th>",
		"<th>Heure</th><th>Requête</th><th>Version</th><th>Type / Statut</th>",
		"limit:200,",
		"20 conv., max 500",
		"GEMINI_TEMPORARY_CHAT=true",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
}

func TestK50_ExtendedHistoryCapacityAndBinaryTag(t *testing.T) {
	if inFlightHistoryMaxConvs != 20 || inFlightHistoryMaxEntries != 500 {
		t.Fatalf("inFlightHistory caps = (%d convs, %d entries), want (20, 500)", inFlightHistoryMaxConvs, inFlightHistoryMaxEntries)
	}
	if diagDefaultLimit != 200 || diagMaxLimit != 500 {
		t.Fatalf("diag limits = (%d default, %d max), want (200, 500)", diagDefaultLimit, diagMaxLimit)
	}

	var entries []InFlightRequest
	for i := 0; i < 25; i++ {
		entries = append(entries, InFlightRequest{
			ID:        fmt.Sprintf("req-%d", i),
			ConvID:    fmt.Sprintf("conv-%d", i),
			Model:     "gemini-4-hydrogen",
			BinaryTag: "K50",
		})
	}
	kept := pruneInFlightEntries(entries)
	if len(kept) != 20 {
		t.Fatalf("pruneInFlightEntries kept %d distinct convs, want 20", len(kept))
	}
	if kept[0].BinaryTag != "K50" {
		t.Errorf("kept[0].BinaryTag = %q, want K50", kept[0].BinaryTag)
	}

	srv := newK42TestServer(t, nil)
	ev := DiagEvent{Kind: "activity", Model: "gemini-4-hydrogen", BinaryTag: "K50"}
	srv.recordDiagActivity(ev)
	dr := srv.diag.query(diagQuery{Kind: "activity", Window: "all", Limit: 10}, time.Now())
	if len(dr.Items) == 0 || dr.Items[0].BinaryTag != "K50" {
		t.Fatalf("diag activity BinaryTag missing: %+v", dr.Items)
	}
}

func TestK50_StaleTokenQuotaPauseAutoResume(t *testing.T) {
	var logBuf bytes.Buffer
	srv := newK42TestServer(t, nil)
	srv.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	srv.cfg.PauseOnFallback = true

	ri := &reqInfo{id: "req-stale", model: "gemini-everyday-help", start: time.Now()}
	ri.setStaleTokenAlert("jeton périmé", "cookie_stale")
	ctx := context.WithValue(context.Background(), ctxKey{}, ri)

	m := &gemini.AvailableModel{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen"}
	if !srv.triggerQuotaPause(ctx, m, "f2994417f76a9d1e", "3.5 Flash", "other_model", "") {
		t.Fatal("expected triggerQuotaPause to activate")
	}
	st := srv.quotaPause.snapshot()
	if !st.Active || !st.StaleToken {
		t.Fatalf("QuotaPauseState = %+v, want Active=true StaleToken=true", st)
	}

	if !srv.resumeStaleTokenQuotaPause(context.Background(), "cookie_refresh_stale_token") {
		t.Fatal("expected resumeStaleTokenQuotaPause to return true")
	}
	if st2 := srv.quotaPause.snapshot(); st2.Active {
		t.Fatalf("expected pause to be lifted, got %+v", st2)
	}
	if !strings.Contains(logBuf.String(), "quota_pause_resumed") || !strings.Contains(logBuf.String(), "source=cookie_refresh_stale_token") {
		t.Errorf("missing quota_pause_resumed log: %s", logBuf.String())
	}

	// Verify quotaPreflight does not block after pause lifted.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if srv.quotaPreflight(w, r, m, flavorOpenAI) {
		t.Fatal("expected quotaPreflight to allow request after resume")
	}
}

func TestK50_CookieAutoRefreshSkippedLog(t *testing.T) {
	var logBuf bytes.Buffer
	client, err := gemini.New(testConfig().Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.CookieAutoRefreshEnabled = false
	cfg.CookieAutoRefreshEnabledSet = true
	cfg.CookieAutoRefreshMin = 60
	ar := newCookieAutoRefresher(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	ar.startedAt = time.Now().Add(-2 * time.Hour)
	ar.PrepareRequest(context.Background(), nil)
	ar.PrepareRequest(context.Background(), nil) // deduplicated

	logs := logBuf.String()
	if strings.Count(logs, "cookie_auto_refresh_skipped") != 1 {
		t.Fatalf("expected exactly 1 deduplicated cookie_auto_refresh_skipped log, got: %s", logs)
	}
	if !strings.Contains(logs, "reason=disabled") {
		t.Errorf("expected reason=disabled in log: %s", logs)
	}
}
