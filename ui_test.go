package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uiServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	dir := t.TempDir()
	cfg.AntigravityModelsFile = filepath.Join(dir, "custom_models.json")
	first := testModels()[0].Name
	doc := `{"models":[
	 {"name":"models/custom/a","displayName":"Pro (GMINI-Proxy)","externalModelName":"` + first + `@low","apiUrl":"http://127.0.0.1:8081/v1","apiKey":"SECRET-KEY-VALUE","timeout":1800000,"relayKeepaliveMs":15000},
	 {"name":"models/custom/gone","displayName":"Disparu","externalModelName":"gemini-gone","apiUrl":"http://localhost:8081/v1","apiKey":"SECRET-KEY-VALUE"},
	 {"name":"models/custom/other","displayName":"Ailleurs","apiUrl":"http://127.0.0.1:9999/v1","apiKey":"SECRET-KEY-VALUE"}]}`
	if err := os.WriteFile(cfg.AntigravityModelsFile, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, cfg)
	return s, s.handler()
}

func uiDo(h http.Handler, method, path, remote, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8081"+path, nil)
	r.RemoteAddr = remote
	if token != "" {
		r.Header.Set("X-GMINI-UI", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUIPageLocalOnlyAndToken(t *testing.T) {
	s, h := uiServer(t)
	if w := uiDo(h, "GET", "/_ui", "192.168.1.20:5000", ""); w.Code != http.StatusForbidden {
		t.Errorf("remote client got %d", w.Code)
	}
	w := uiDo(h, "GET", "/_ui", "127.0.0.1:5000", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), s.uiToken) || strings.Contains(w.Body.String(), "__UI_TOKEN__") {
		t.Fatalf("page: code=%d", w.Code)
	}
	if w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("state without token got %d", w.Code)
	}
	if w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("state with wrong token got %d", w.Code)
	}
	if w := uiDo(h, "POST", "/_ui/api/models/refresh", "127.0.0.1:5000", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("refresh without token got %d", w.Code)
	}
	if w := uiDo(h, "GET", "/_ui/api/state", "10.0.0.2:5000", s.uiToken); w.Code != http.StatusForbidden {
		t.Errorf("remote state got %d", w.Code)
	}
}

func TestUIStateContent(t *testing.T) {
	s, h := uiServer(t)
	w := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "SECRET-KEY-VALUE") || strings.Contains(strings.ToLower(body), "apikey") {
		t.Fatalf("the Antigravity key leaks into the page state")
	}
	var d struct {
		Status map[string]any `json:"status"`
		Models []uiModel      `json:"models"`
		AG     uiAntigravity  `json:"antigravity"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Status["version"] == nil || len(d.Models) != len(testModels()) {
		t.Fatalf("status/models missing: %v %d", d.Status["version"], len(d.Models))
	}
	if d.AG.File != "ok" || len(d.AG.Entries) != 2 {
		t.Fatalf("antigravity=%+v", d.AG)
	}
	if d.AG.Entries[0].Status != "ok" || d.AG.Entries[0].RelayMS != 15000 || d.AG.Entries[1].Status != "not_in_catalogue" {
		t.Errorf("entries=%+v", d.AG.Entries)
	}
	if len(d.AG.Missing) != len(testModels())-1 {
		t.Errorf("missing=%v", d.AG.Missing)
	}
}

func TestUIDisabled(t *testing.T) {
	cfg := testConfig()
	s := newTestServer(t, cfg)
	if w := uiDo(s.handler(), "GET", "/_ui", "127.0.0.1:5000", ""); w.Code != http.StatusNotFound {
		t.Errorf("disabled page got %d", w.Code)
	}
}
