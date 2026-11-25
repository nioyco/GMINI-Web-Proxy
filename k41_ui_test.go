package main

import (
	"net/http"
	"strings"
	"testing"
)

// K41 — GMINI Console-Web-UI v3.0.0 : console multi-pages, menu latéral, bandeau Pause Quota.
func TestK41_ConsoleWebUIv3(t *testing.T) {
	cfg := testConfig()
	cfg.UIEnabled = true
	s := newTestServer(t, cfg)
	h := s.handler()

	rec := uiDo(h, http.MethodGet, "/_ui", "127.0.0.1:5000", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_ui status=%d", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{
		"GMINI Console-Web-UI", "sidebar-toggle", `id="quotapause"`, "quota-resume-banner", "quota-resume-page",
		"pof-toggle", "gmini_ui_sidebar_collapsed", "/_ui/api/quota/resume", "https://gemini.google.com/usage",
		"#etat", "#modeles", "#diagnostic", "#demandes", "#quota", "#reglages", "errMsg", "DIAGNOSTIC PROXY",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("ui.html missing %q", want)
		}
	}
	for _, bad := range []string{"onclick=\"", "<form", "https://cdn", "<link rel=\"stylesheet"} {
		if strings.Contains(html, bad) {
			t.Errorf("ui.html must not contain %q (CSP / zero dependency)", bad)
		}
	}
	if strings.Count(html, "<style>") != 1 || strings.Count(html, "<script>") != 1 {
		t.Errorf("ui.html must have exactly one inline <style> and one <script>")
	}

	st := uiDo(h, http.MethodGet, "/_status", "127.0.0.1:5000", "")
	if !strings.Contains(st.Body.String(), `"ui_version":"`+uiVersion+`"`) {
		t.Errorf("/_status must report ui_version %s, got: %s", uiVersion, st.Body.String())
	}
}
