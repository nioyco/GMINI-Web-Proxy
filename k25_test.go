package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func uiJSON(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:8081"+path, rdr)
	r.RemoteAddr = "127.0.0.1:51234"
	r.Host = "127.0.0.1:8081"
	if token != "" {
		r.Header.Set("X-GMINI-UI", token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func makeFakeAGApp(t *testing.T, dir, appName, version, asarBody string) string {
	t.Helper()
	appPath := filepath.Join(dir, appName)
	resDir := filepath.Join(appPath, "Contents", "Resources")
	if err := os.MkdirAll(resDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleShortVersionString</key><string>%s</string>
</dict></plist>
`, version)
	if err := os.WriteFile(filepath.Join(appPath, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resDir, "app.asar"), []byte(asarBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return appPath
}

func TestK25_ConfigThinkingModeCodesAndEnablerRepo(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "none.env")
	t.Setenv("GEMINI_THINKING_MODE_CODES", "")
	os.Unsetenv("GEMINI_THINKING_MODE_CODES")
	t.Setenv("GEMINI_ENABLER_REPO", "/tmp/fake-enabler-repo")

	cfg := loadConfig(noEnv)
	if got := cfg.ThinkingModes["95a4c0abcba24364"]; got != 8 {
		t.Errorf("ThinkingModes[95a4c0abcba24364] = %d, want 8", got)
	}
	if got := cfg.ThinkingModes["e6fa609c3fa255c0"]; got != 3 {
		t.Errorf("ThinkingModes[e6fa609c3fa255c0] = %d, want 3", got)
	}
	if cfg.EnablerRepo != "/tmp/fake-enabler-repo" {
		t.Errorf("EnablerRepo = %q, want /tmp/fake-enabler-repo", cfg.EnablerRepo)
	}
}

func TestK25_RoutedAutoStreamingNoSuspectWarning(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress, cfg.StreamThoughts, cfg.WarnSuspectFallback = true, true, true
	cfg.SSEKeepaliveSec = 1

	var logs lockedBuf
	client, err := gemini.New(cfg.Gemini, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Auto", ModeCode: 6},
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", DisplayName: "4 Hydrogen", Description: "4 Hydrogen", ModeCode: 1},
	})
	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(fakeStreamServed("Réponse auto.", "", "56fdd199312815e2", "4 Hydrogen"))),
		}, nil
	}))

	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logs, nil)))
	body := `{"model":"gemini-auto","stream":true,"messages":[{"role":"user","content":"bonjour"}]}`
	rec := post(s, "/v1/chat/completions", body)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "Réponse auto.") {
		t.Fatalf("missing answer in stream output: %s", out)
	}
	if strings.Contains(out, "reasoning_content") || strings.Contains(out, "autre modèle") || strings.Contains(out, "identifiant différent") {
		t.Fatalf("unexpected fallback warning on routed_auto stream: %s", out)
	}
	ids := client.ModelIdentities()
	if len(ids) != 1 || ids[0].LastVerdict != "routed_auto" || !ids[0].MismatchSince.IsZero() {
		t.Fatalf("ModelIdentities = %+v, want routed_auto with zero MismatchSince", ids)
	}
	calls := client.RecentCalls()
	if len(calls) != 1 || calls[0].Identity != "routed_auto" || calls[0].ServedID != "56fdd199312815e2" {
		t.Fatalf("RecentCalls = %+v", calls)
	}
	if strings.Contains(logs.String(), "served_model_doubt") {
		t.Fatalf("unexpected served_model_doubt log: %s", logs.String())
	}
	h := s.handler()
	w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	if w.Code != 200 {
		t.Fatalf("ui state code=%d", w.Code)
	}
	var st struct {
		InFlight []any `json:"in_flight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.InFlight) != 0 {
		t.Fatalf("in_flight not empty after completion: %v", st.InFlight)
	}
}

func TestK25_UIAntigravitySemverAndMultiVersion(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS bundle probe (/usr/bin/plutil) applies on darwin")
	}
	if compareSemver("2.33.10", "2.33.0") <= 0 || compareSemver("2.33.0", "2.32.2") <= 0 || compareSemver("2.32.2", "2.32.1") <= 0 {
		t.Fatalf("compareSemver ordering failed")
	}

	origDirs, origRun, origHome := agDirsFn, runningAppsFn, userHomeFn
	t.Cleanup(func() {
		agDirsFn, runningAppsFn, userHomeFn = origDirs, origRun, origHome
	})

	root := t.TempDir()
	appsSys := filepath.Join(root, "Applications")
	homeDir := filepath.Join(root, "home")
	appsUsr := filepath.Join(homeDir, "Applications")
	dlDir := filepath.Join(homeDir, "Downloads")
	for _, d := range []string{appsSys, appsUsr, dlDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	makeFakeAGApp(t, appsSys, "Antigravity.app", "2.33.10", "vanilla-2.33.10")
	makeFakeAGApp(t, appsSys, "Antigravity-2.33.0.app", "2.33.0", "vanilla-2.33.0")
	makeFakeAGApp(t, appsUsr, "Antigravity-2.32.2.app", "2.32.2", "vanilla-2.32.2")
	makeFakeAGApp(t, appsUsr, "Antigravity-2.32.1.app", "2.32.1", "vanilla-2.32.1")
	makeFakeAGApp(t, appsUsr, "Antigravity (add-model 2.32.1 P1P5a).app", "2.32.1", "custom_models.json relayKeepaliveMs")

	if err := os.WriteFile(filepath.Join(dlDir, "Antigravity-2.33.10.dmg"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dlDir, "Other.dmg"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}

	agDirsFn = func() []string { return []string{appsSys, appsUsr} }
	runningAppsFn = func() map[string]int { return map[string]int{} }
	userHomeFn = func() (string, error) { return homeDir, nil }

	s, h := uiServer(t)
	w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	if w.Code != 200 {
		t.Fatalf("state code=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Apps []agApp `json:"apps"`
		DMGs []agDMG `json:"antigravity_dmgs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Apps) != 5 {
		t.Fatalf("apps len = %d, want 5 (%+v)", len(payload.Apps), payload.Apps)
	}
	// Patched clone (relay=true) sorts first.
	if payload.Apps[0].Kind != "clone" || !payload.Apps[0].Relay || payload.Apps[0].Version != "2.32.1" {
		t.Fatalf("first app = %+v, want patched clone 2.32.1", payload.Apps[0])
	}
	// Remaining official apps sort descending by semver: 2.33.10 > 2.33.0 > 2.32.2 > 2.32.1.
	gotOffOrder := []string{payload.Apps[1].Version, payload.Apps[2].Version, payload.Apps[3].Version, payload.Apps[4].Version}
	wantOrder := []string{"2.33.10", "2.33.0", "2.32.2", "2.32.1"}
	for i := range wantOrder {
		if gotOffOrder[i] != wantOrder[i] {
			t.Fatalf("official version order = %v, want %v", gotOffOrder, wantOrder)
		}
	}
	byVer := map[string]agApp{}
	for _, a := range payload.Apps {
		if a.Kind == "official" {
			byVer[a.Version] = a
		}
	}
	if !byVer["2.33.10"].IsLatest || byVer["2.33.0"].IsLatest {
		t.Errorf("IsLatest flags wrong: 2.33.10=%v 2.33.0=%v", byVer["2.33.10"].IsLatest, byVer["2.33.0"].IsLatest)
	}
	if !byVer["2.32.1"].HasPatchedClone || byVer["2.33.0"].HasPatchedClone || byVer["2.33.10"].HasPatchedClone {
		t.Errorf("HasPatchedClone flags wrong: %+v", byVer)
	}
	if !byVer["2.33.0"].Patchable || byVer["2.33.0"].TargetCloneName != "Antigravity (add-model 2.33.0 P1P5a).app" {
		t.Errorf("2.33.0 patch metadata = %+v", byVer["2.33.0"])
	}
	if len(payload.DMGs) != 1 || payload.DMGs[0].Name != "Antigravity-2.33.10.dmg" || payload.DMGs[0].SizeMB != 2 {
		t.Fatalf("antigravity_dmgs = %+v", payload.DMGs)
	}
}

func TestK25_UIAntigravityAliasAndSync(t *testing.T) {
	origRun := runningAppsFn
	t.Cleanup(func() { runningAppsFn = origRun })
	var isRunning bool
	runningAppsFn = func() map[string]int {
		if isRunning {
			return map[string]int{"/Applications/Antigravity.app": 4242}
		}
		return map[string]int{}
	}

	catalog := []*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Auto", ModeCode: 6},
		{Name: "gemini-deeper-analysis", ModelID: "e6fa609c3fa255c0", DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0", ModeCode: 3},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Argon", Family: "Argon4p0", ModeCode: 8},
	}

	t.Run("array_format_dry_run_conflict_apply_and_migrate", func(t *testing.T) {
		dir := t.TempDir()
		agPath := filepath.Join(dir, "custom_models.json")
		doc := `{
  "models": [
    {
      "name": "models/custom/keepalive-test",
      "displayName": "GMINI keepalive test",
      "externalModelName": "gemini-auto",
      "apiUrl": "http://127.0.0.1:8081/v1",
      "apiKey": "SENTINEL-DO-NOT-LEAK",
      "timeout": 999
    },
    {
      "name": "models/custom/deeper-template",
      "displayName": "Gemini 3.1 Pro (GMINI-Proxy) (Low)",
      "externalModelName": "gemini-detailed-research@low",
      "apiUrl": "http://127.0.0.1:8081/v1",
      "apiKey": "SENTINEL-DO-NOT-LEAK",
      "timeout": 1800000,
      "relayKeepaliveMs": 15000,
      "maxTokens": 65536
    },
    {
      "name": "models/custom/non-gmini",
      "displayName": "External Provider",
      "apiUrl": "https://api.example.invalid/v1",
      "apiKey": "SENTINEL-DO-NOT-LEAK",
      "customField": {"nested": [1, 2, 3]}
    }
  ]
}
`
		if err := os.WriteFile(agPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}

		var logs lockedBuf
		cfg := testConfig()
		cfg.UIEnabled = true
		cfg.Listen = "127.0.0.1:8081"
		cfg.AntigravityModelsFile = agPath
		client, err := gemini.New(cfg.Gemini, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil {
			t.Fatal(err)
		}
		client.SetModels(catalog)
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logs, nil)))
		h := s.handler()

		// 1. State classifies gemini-detailed-research@low as status="alias" -> "gemini-deeper-analysis"
		wState := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
		if wState.Code != 200 {
			t.Fatalf("state code=%d", wState.Code)
		}
		if strings.Contains(wState.Body.String(), "SENTINEL-DO-NOT-LEAK") {
			t.Fatalf("sentinel leaked into state response")
		}
		var stateResp struct {
			AG uiAntigravity `json:"antigravity"`
		}
		if err := json.Unmarshal(wState.Body.Bytes(), &stateResp); err != nil {
			t.Fatal(err)
		}
		var foundAlias bool
		for _, e := range stateResp.AG.Entries {
			if e.Model == "gemini-detailed-research@low" {
				foundAlias = true
				if e.Status != "alias" || e.AliasTarget != "gemini-deeper-analysis" {
					t.Fatalf("alias entry = %+v, want status=alias target=gemini-deeper-analysis", e)
				}
			}
		}
		if !foundAlias {
			t.Fatalf("alias entry not found in state: %+v", stateResp.AG.Entries)
		}

		// 2. dry_run leaves file untouched on disk.
		wDry := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models", `{"mode":"dry_run","with_levels":true}`, s.uiToken)
		if wDry.Code != 200 {
			t.Fatalf("dry_run code=%d body=%s", wDry.Code, wDry.Body.String())
		}
		afterDry, _ := os.ReadFile(agPath)
		if string(afterDry) != doc {
			t.Fatalf("dry_run modified disk file")
		}
		if strings.Contains(wDry.Body.String(), "SENTINEL-DO-NOT-LEAK") {
			t.Fatalf("sentinel leaked into dry_run response")
		}

		// 3. apply while Antigravity is running returns 409 Conflict.
		isRunning = true
		wRun := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models", `{"mode":"apply"}`, s.uiToken)
		if wRun.Code != http.StatusConflict || !strings.Contains(wRun.Body.String(), "antigravity_running") {
			t.Fatalf("running apply got code=%d body=%s", wRun.Code, wRun.Body.String())
		}
		isRunning = false

		// 4. apply when closed (keep_legacy=true, migrate_aliases=false).
		wApply := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models",
			`{"mode":"apply","with_levels":true,"keep_legacy":true,"migrate_aliases":false}`, s.uiToken)
		if wApply.Code != 200 {
			t.Fatalf("apply code=%d body=%s", wApply.Code, wApply.Body.String())
		}
		if strings.Contains(wApply.Body.String(), "SENTINEL-DO-NOT-LEAK") {
			t.Fatalf("sentinel leaked into apply response")
		}
		var res agSyncResult
		if err := json.Unmarshal(wApply.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if !res.OK || res.BackupPath == "" || len(res.Added) != 6 {
			// gemini-auto already existed (1), so 3 levels for deeper-analysis + 3 levels for code-complex-work = 6 added.
			t.Fatalf("unexpected sync result: %+v", res)
		}
		info, err := os.Stat(agPath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file perm = %v (err=%v), want 0600", info.Mode().Perm(), err)
		}
		bakInfo, err := os.Stat(res.BackupPath)
		if err != nil || bakInfo.Mode().Perm() != 0o600 {
			t.Fatalf("backup perm = %v (err=%v), want 0600", bakInfo.Mode().Perm(), err)
		}
		if !strings.Contains(res.BackupPath, string(filepath.Separator)+"backups"+string(filepath.Separator)) {
			t.Errorf("backup path %q not in backups/", res.BackupPath)
		}

		// Verify generated entries use the clean template (maxTokens=65536, timeout=1800000), NOT keepalive-test (timeout=999),
		// and preserve non-GMINI entry intact.
		writtenBytes, _ := os.ReadFile(agPath)
		var writtenDoc struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(writtenBytes, &writtenDoc); err != nil {
			t.Fatal(err)
		}
		gotByExt := map[string]map[string]any{}
		var nonGmini map[string]any
		for _, m := range writtenDoc.Models {
			if ext, _ := m["externalModelName"].(string); ext != "" {
				gotByExt[ext] = m
			}
			if name, _ := m["name"].(string); name == "models/custom/non-gmini" {
				nonGmini = m
			}
		}
		for _, wantExt := range []string{
			"gemini-auto",
			"gemini-deeper-analysis@low", "gemini-deeper-analysis@medium", "gemini-deeper-analysis@high",
			"gemini-code-complex-work@low", "gemini-code-complex-work@medium", "gemini-code-complex-work@high",
		} {
			e, ok := gotByExt[wantExt]
			if !ok {
				t.Errorf("missing entry %q after apply", wantExt)
				continue
			}
			if wantExt != "gemini-auto" && e["timeout"] != float64(1800000) {
				t.Errorf("%s used wrong template timeout=%v, want 1800000", wantExt, e["timeout"])
			}
		}
		if nonGmini == nil || nonGmini["apiKey"] != "SENTINEL-DO-NOT-LEAK" || nonGmini["customField"] == nil {
			t.Fatalf("non-GMINI entry altered: %+v", nonGmini)
		}

		// 5. Migrate aliases + drop legacy (migrate_aliases=true, keep_legacy=false) removes legacy alias without harming non-GMINI.
		wMig := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models",
			`{"mode":"apply","with_levels":true,"keep_legacy":false,"migrate_aliases":true,"rename_existing":true}`, s.uiToken)
		if wMig.Code != 200 {
			t.Fatalf("migrate code=%d body=%s", wMig.Code, wMig.Body.String())
		}
		var resMig agSyncResult
		if err := json.Unmarshal(wMig.Body.Bytes(), &resMig); err != nil {
			t.Fatal(err)
		}
		if len(resMig.Removed) != 1 || resMig.Removed[0].Model != "gemini-detailed-research@low" {
			t.Fatalf("expected gemini-detailed-research@low removed once @low already existed, got %+v", resMig)
		}
		if strings.Contains(logs.String(), "SENTINEL-DO-NOT-LEAK") {
			t.Fatalf("sentinel leaked into logs")
		}
	})

	t.Run("object_map_format_alias_migration_in_place", func(t *testing.T) {
		dir := t.TempDir()
		agPath := filepath.Join(dir, "custom_models.json")
		doc := `{
  "models": {
    "0": {
      "name": "models/custom/old-research-high",
      "displayName": "Gemini 3.1 Pro (GMINI-Proxy) (High)",
      "externalModelName": "gemini-detailed-research@high",
      "apiUrl": "http://127.0.0.1:8081/v1",
      "apiKey": "SENTINEL-DO-NOT-LEAK",
      "timeout": 1800000,
      "relayKeepaliveMs": 15000
    },
    "customKey": {
      "name": "models/custom/external-map",
      "displayName": "External Map Entry",
      "apiUrl": "https://other.example/v1",
      "apiKey": "SENTINEL-DO-NOT-LEAK"
    }
  }
}
`
		if err := os.WriteFile(agPath, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := testConfig()
		cfg.UIEnabled = true
		cfg.Listen = "127.0.0.1:8081"
		cfg.AntigravityModelsFile = agPath
		client, err := gemini.New(cfg.Gemini, testLogger())
		if err != nil {
			t.Fatal(err)
		}
		client.SetModels(catalog)
		s := newServer(cfg, client, testLogger())
		h := s.handler()

		w := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models",
			`{"mode":"apply","with_levels":true,"keep_legacy":false,"migrate_aliases":true}`, s.uiToken)
		if w.Code != 200 {
			t.Fatalf("map apply code=%d body=%s", w.Code, w.Body.String())
		}
		var res agSyncResult
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if len(res.Migrated) != 1 || res.Migrated[0].FromModel != "gemini-detailed-research@high" || res.Migrated[0].Model != "gemini-deeper-analysis@high" {
			t.Fatalf("Migrated = %+v", res.Migrated)
		}
		rawOut, _ := os.ReadFile(agPath)
		var outDoc struct {
			Models map[string]map[string]any `json:"models"`
		}
		if err := json.Unmarshal(rawOut, &outDoc); err != nil {
			t.Fatal(err)
		}
		if outDoc.Models["customKey"] == nil || outDoc.Models["customKey"]["apiKey"] != "SENTINEL-DO-NOT-LEAK" {
			t.Fatalf("object-map non-GMINI entry altered: %+v", outDoc.Models["customKey"])
		}
		if got, _ := outDoc.Models["0"]["externalModelName"].(string); got != "gemini-deeper-analysis@high" {
			t.Fatalf("migrated key 0 externalModelName = %q, want gemini-deeper-analysis@high", got)
		}
	})
}

func TestK25_UIAntigravityPatchEndpoint(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS clone patch test uses /usr/bin/plutil")
	}

	origDirs, origRun, origNode, origHome, origRepo, origCS := agDirsFn, runningAppsFn, nodeBinFn, userHomeFn, enablerRepoFn, codesignVerifyFn
	t.Cleanup(func() {
		agDirsFn, runningAppsFn, nodeBinFn, userHomeFn, enablerRepoFn, codesignVerifyFn = origDirs, origRun, origNode, origHome, origRepo, origCS
	})

	root := t.TempDir()
	appsSys := filepath.Join(root, "Applications")
	homeDir := filepath.Join(root, "home")
	appsUsr := filepath.Join(homeDir, "Applications")
	enabler := filepath.Join(root, "enabler")
	binDir := filepath.Join(root, "bin")
	agDir := filepath.Join(root, "agcfg")
	for _, d := range []string{appsSys, appsUsr, filepath.Join(enabler, "scripts"), binDir, agDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	srcApp := makeFakeAGApp(t, appsSys, "Antigravity.app", "2.33.0", "vanilla-2.33.0")
	// Pre-create an old target clone to verify archival to .sauvegardes_antigravity.
	makeFakeAGApp(t, appsUsr, "Antigravity (add-model 2.33.0 P1P5a).app", "2.33.0", "old-clone")

	if err := os.WriteFile(filepath.Join(enabler, "scripts", "deploy.mjs"), []byte("// fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(enabler, "scripts", "macos-clone.mjs"), []byte("// fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeNPM := filepath.Join(binDir, "npm")
	fakeNPMScript := `#!/bin/sh
set -e
mkdir -p dist
printf '// compiled\n' > dist/proxy.js
echo "fake npm build ok"
`
	if err := os.WriteFile(fakeNPM, []byte(fakeNPMScript), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeNode := filepath.Join(binDir, "node")
	fakeNodeScript := `#!/bin/sh
set -e
script="$1"
shift
case "$script" in
  scripts/deploy.mjs)
    echo "deploy check ok: $*"
    ;;
  scripts/macos-clone.mjs)
    src=""
    target=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --source) src="$2"; shift 2 ;;
        --target) target="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    mkdir -p "$target/Contents/Resources"
    cp "$src/Contents/Info.plist" "$target/Contents/Info.plist"
    printf 'patched custom_models.json and relayKeepaliveMs\n' > "$target/Contents/Resources/app.asar"
    echo "cloned $src -> $target"
    ;;
  *)
    echo "unexpected script: $script" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(fakeNode, []byte(fakeNodeScript), 0o755); err != nil {
		t.Fatal(err)
	}

	agFile := filepath.Join(agDir, "custom_models.json")
	agDoc := `{"models":[{"name":"models/custom/a","displayName":"Gemini Auto (GMINI-Proxy)","externalModelName":"gemini-auto","apiUrl":"http://127.0.0.1:8081/v1","apiKey":"SENTINEL-DO-NOT-LEAK","timeout":1800000,"relayKeepaliveMs":15000}]}`
	if err := os.WriteFile(agFile, []byte(agDoc), 0o600); err != nil {
		t.Fatal(err)
	}

	var isRunning bool
	agDirsFn = func() []string { return []string{appsSys, appsUsr} }
	runningAppsFn = func() map[string]int {
		if isRunning {
			return map[string]int{srcApp: 9001}
		}
		return map[string]int{}
	}
	nodeBinFn = func() (string, string, error) { return fakeNode, fakeNPM, nil }
	userHomeFn = func() (string, error) { return homeDir, nil }
	enablerRepoFn = func(*Config) string { return enabler }
	codesignVerifyFn = func(context.Context, string) (string, error) { return "valid on disk", nil }

	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	cfg.AntigravityModelsFile = agFile
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Auto", ModeCode: 6},
		{Name: "gemini-deeper-analysis", ModelID: "e6fa609c3fa255c0", DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0", ModeCode: 3},
	})
	s := newServer(cfg, client, testLogger())
	h := s.handler()

	// 1. Running refuses 409.
	isRunning = true
	wRun := uiJSON(h, "POST", "/_ui/api/antigravity/patch",
		fmt.Sprintf(`{"source_path":%q}`, srcApp), s.uiToken)
	if wRun.Code != http.StatusConflict || !strings.Contains(wRun.Body.String(), "antigravity_running") {
		t.Fatalf("running patch code=%d body=%s", wRun.Code, wRun.Body.String())
	}
	isRunning = false

	// 2. Unknown source refuses 400.
	wBad := uiJSON(h, "POST", "/_ui/api/antigravity/patch", `{"source_path":"/nope/Antigravity.app"}`, s.uiToken)
	if wBad.Code != http.StatusBadRequest || !strings.Contains(wBad.Body.String(), "unknown_source") {
		t.Fatalf("bad source patch code=%d body=%s", wBad.Code, wBad.Body.String())
	}

	// 3. Valid patch starts async job (202) and finishes with succeeded.
	wStart := uiJSON(h, "POST", "/_ui/api/antigravity/patch",
		fmt.Sprintf(`{"source_path":%q,"rebuild":true,"sync_models_after":true}`, srcApp), s.uiToken)
	if wStart.Code != http.StatusAccepted {
		t.Fatalf("start patch code=%d body=%s", wStart.Code, wStart.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	var job agPatchJob
	for time.Now().Before(deadline) {
		wPol := uiDo(h, "GET", "/_ui/api/antigravity/patch/status", "127.0.0.1:5000", s.uiToken)
		if wPol.Code != 200 {
			t.Fatalf("patch status code=%d", wPol.Code)
		}
		if err := json.Unmarshal(wPol.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State != "running" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if job.State != "succeeded" {
		t.Fatalf("patch job finished with state=%q err=%q step=%q logs=%v", job.State, job.Error, job.Step, job.Logs)
	}
	if !job.SyncApplied || job.ArchivedClone == "" || job.BackupPath == "" {
		t.Fatalf("missing expected job metadata: %+v", job)
	}
	if _, err := os.Stat(job.ArchivedClone); err != nil {
		t.Errorf("archived clone missing at %q: %v", job.ArchivedClone, err)
	}
	bakInfo, err := os.Stat(job.BackupPath)
	if err != nil || bakInfo.Mode().Perm() != 0o600 {
		t.Errorf("patch backup perm = %v (err=%v), want 0600", bakInfo.Mode().Perm(), err)
	}
	if !fileExists(filepath.Join(enabler, "dist", "proxy.js")) {
		t.Errorf("fake npm build did not produce dist/proxy.js")
	}
}

func TestK25_ApplyEffortSkipsAuto(t *testing.T) {
	cfg := testConfig()
	cfg.DefaultThinkingLevel = "medium"
	s := newTestServer(t, cfg)

	autoModel := &gemini.AvailableModel{
		Name:        "gemini-auto",
		ModelID:     "8c46e95b1a07cecc",
		DisplayName: "Auto",
		ModeCode:    6,
	}
	var p gemini.GenParams
	s.applyEffort(&p, autoModel, "low", "high")
	if p.Level != 0 || p.Mode != 0 {
		t.Errorf("applyEffort on Auto = %+v, want Level=0 Mode=0", p)
	}

	neonModel := &gemini.AvailableModel{
		Name:        "gemini-deeper-analysis",
		ModelID:     "e6fa609c3fa255c0",
		DisplayName: "Neon",
		ModeCode:    3,
	}
	var p2 gemini.GenParams
	s.applyEffort(&p2, neonModel, "low", "")
	if p2.Level != 1 || p2.Mode != 3 {
		t.Errorf("applyEffort on Neon = %+v, want Level=1 Mode=3", p2)
	}
}

func TestK26_SyncDeduplicateArgonAndAuto(t *testing.T) {
	origRun := runningAppsFn
	t.Cleanup(func() { runningAppsFn = origRun })
	runningAppsFn = func() map[string]int { return map[string]int{} }

	dir := t.TempDir()
	agPath := filepath.Join(dir, "custom_models.json")
	initial := `{
  "models": [
    {"name": "models/openai/gpt-4o", "displayName": "GPT-4o", "apiUrl": "https://api.openai.com/v1", "apiKey": "enc:KEEP_SECRET"},
    {"name": "models/custom/gemini-most-complex-work-low", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Low)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-most-complex-work@low", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-most-complex-work-medium", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Medium)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-most-complex-work@medium", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-most-complex-work-high", "displayName": "Gemini 4 Argon (GMINI-Proxy) (High)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-most-complex-work@high", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-low", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Low)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-&-complex-work@low", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-medium", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Medium)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-&-complex-work@medium", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-high", "displayName": "Gemini 4 Argon (GMINI-Proxy) (High)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-&-complex-work@high", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-auto-low", "displayName": "Gemini Auto (GMINI-Proxy) (Low)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-auto@low", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-detailed-research-medium", "displayName": "Gemini 3.1 Pro (GMINI-Proxy) (Medium)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-detailed-research@medium", "apiKey": "enc:GMINI"}
  ]
}
`
	if err := os.WriteFile(agPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	cfg.AntigravityModelsFile = agPath
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Adapts to your needs", ModeCode: 6},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Carbon", Description: "4 Argon", ModeCode: 8},
	})
	s := newServer(cfg, client, testLogger())
	h := s.handler()

	w := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models",
		`{"mode":"apply","with_levels":true,"deduplicate":true,"keep_legacy":true}`, s.uiToken)
	if w.Code != 200 {
		t.Fatalf("dedup sync code=%d body=%s", w.Code, w.Body.String())
	}
	var res agSyncResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 7 {
		t.Fatalf("removed=%d want 7 (6 Argon dupes + 1 Auto@low): %+v", len(res.Removed), res.Removed)
	}
	if len(res.Added) != 4 {
		t.Fatalf("added=%d want 4 (Auto + Argon Low/Med/High): %+v", len(res.Added), res.Added)
	}
	if len(res.KeptLegacy) != 1 || res.KeptLegacy[0].Model != "gemini-detailed-research@medium" {
		t.Fatalf("expected 3.1 Pro legacy alias kept when keep_legacy=true, got %+v", res.KeptLegacy)
	}
	// Verify no duplicate displayNames remain and Argon has clean Low/Medium/High.
	raw, err := os.ReadFile(agPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, entries, err := parseAGModelsDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	seenDisp := map[string]int{}
	argonCount := 0
	for _, e := range entries {
		seenDisp[e.meta.DisplayName]++
		if strings.HasPrefix(e.meta.DisplayName, "Gemini 4 Argon (GMINI-Proxy) (") {
			argonCount++
			if !strings.HasPrefix(e.meta.External, "gemini-code-complex-work@") {
				t.Errorf("Argon entry has unexpected externalModelName: %s", e.meta.External)
			}
		}
	}
	if argonCount != 3 {
		t.Errorf("argonCount=%d want 3 (Low/Medium/High)", argonCount)
	}
	for disp, cnt := range seenDisp {
		if cnt > 1 {
			t.Errorf("duplicate displayName %q appears %d times", disp, cnt)
		}
	}
}

func TestK26_SyncDeduplicateByNameArgonAndAuto(t *testing.T) {
	origRun := runningAppsFn
	t.Cleanup(func() { runningAppsFn = origRun })
	runningAppsFn = func() map[string]int { return map[string]int{} }

	dir := t.TempDir()
	agPath := filepath.Join(dir, "custom_models.json")
	initial := `{
  "models": [
    {"name": "models/openai/gpt-4o", "displayName": "GPT-4o", "apiUrl": "https://api.openai.com/v1", "apiKey": "enc:KEEP_SECRET"},
    {"name": "models/custom/gemini-auto", "displayName": "Gemini Auto (alias)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-quick-answers", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-auto", "displayName": "Gemini Auto (GMINI-Proxy)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-auto", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-auto", "displayName": "Gemini Auto (GMINI-Proxy)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-auto", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-low", "displayName": "Gemini 4 Carbon (GMINI-Proxy) (Low)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@low", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-low", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Low)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@low", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-medium", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Medium)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@medium", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-medium", "displayName": "Gemini 4 Argon (GMINI-Proxy) (Medium)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@medium", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-high", "displayName": "Gemini 4 Argon (GMINI-Proxy) (High)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@high", "apiKey": "enc:GMINI"},
    {"name": "models/custom/gemini-code-complex-work-high", "displayName": "Gemini 4 Argon (GMINI-Proxy) (High)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-code-complex-work@high", "apiKey": "enc:GMINI"},
    {"name": "models/custom/legacy-argon-other", "displayName": "Gemini 4 Argon (GMINI-Proxy) (High)", "apiUrl": "http://127.0.0.1:8081/v1", "externalModelName": "gemini-unmapped-slug", "apiKey": "enc:GMINI"}
  ]
}
`
	if err := os.WriteFile(agPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	cfg.AntigravityModelsFile = agPath
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Adapts to your needs", ModeCode: 6},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Carbon", Description: "4 Argon", ModeCode: 8},
	})
	s := newServer(cfg, client, testLogger())
	h := s.handler()

	w := uiJSON(h, "POST", "/_ui/api/antigravity/sync-models",
		`{"mode":"apply","with_levels":true,"deduplicate":true,"rename_existing":true,"keep_legacy":true}`, s.uiToken)
	if w.Code != 200 {
		t.Fatalf("dedup by name code=%d body=%s", w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(agPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, entries, err := parseAGModelsDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	seenName := map[string]int{}
	seenDisp := map[string]int{}
	autoCount, argonCount := 0, 0
	for _, e := range entries {
		seenName[e.meta.Name]++
		seenDisp[e.meta.DisplayName]++
		if e.meta.External == "gemini-auto" {
			autoCount++
		}
		if strings.HasPrefix(e.meta.DisplayName, "Gemini 4 Argon (GMINI-Proxy) (") {
			argonCount++
		}
	}
	if autoCount != 1 || argonCount != 3 {
		t.Errorf("autoCount=%d (want 1) argonCount=%d (want 3)", autoCount, argonCount)
	}
	if seenName["models/openai/gpt-4o"] != 1 {
		t.Errorf("non-GMINI entry altered or missing: %+v", seenName)
	}
	for name, cnt := range seenName {
		if cnt > 1 {
			t.Errorf("duplicate name %q appears %d times", name, cnt)
		}
	}
	for disp, cnt := range seenDisp {
		if cnt > 1 {
			t.Errorf("duplicate displayName %q appears %d times", disp, cnt)
		}
	}
}
