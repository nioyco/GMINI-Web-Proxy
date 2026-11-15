package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
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

func TestUISessionDegradedExposedAndCleared(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(cookiePath, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := cookiePath + ".state.json"
	sum := sha256.Sum256([]byte("s"))
	fp := hex.EncodeToString(sum[:])[:16]
	now := time.Now().UTC().Format(time.RFC3339)
	seededState, _ := json.Marshal(map[string]any{
		"version":             1,
		"saved_at":            now,
		"session_fingerprint": fp,
		"at":                  "tok",
		"at_at":               now,
		"bl":                  "boq_x",
		"bl_at":               now,
		"models": []map[string]any{
			{"Name": "gemini-3.5-flash", "ModelID": "id1", "Description": "3.5 Flash"},
			{"Name": "gemini-3.5-flash-lite", "ModelID": "id2", "Description": "3.5 Flash-Lite"},
			{"Name": "gemini-3.1-pro", "ModelID": "id3", "Description": "3.1 Pro"},
		},
		"models_at":     now,
		"models_source": "dynamic",
	})
	if err := os.WriteFile(statePath, seededState, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	cfg.AntigravityModelsFile = "off"
	cfg.Gemini.CookieFile = cookiePath
	cfg.Gemini.StateFile = statePath

	var reply string
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := reply
		if strings.HasSuffix(r.URL.Path, "/app") {
			body = appPage
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	client.RestoreState()

	makeCatalog := func(status int, names ...string) string {
		raw := make([]any, 0, len(names))
		for _, n := range names {
			ml := make([]any, 12)
			ml[0], ml[1], ml[11] = "id_"+n, n, n
			raw = append(raw, ml)
		}
		body := make([]any, 18)
		if status > 0 {
			body[14] = status
		}
		body[15] = raw
		ib, _ := json.Marshal(body)
		outer, _ := json.Marshal([]any{[]any{"wrb.fr", "otAQ7b", string(ib)}})
		return ")]}'\n" + string(outer)
	}
	_ = context.Background()

	s := newServer(cfg, client, testLogger())
	h := s.handler()

	// Degraded 1-model response via POST /_ui/api/models/refresh
	reply = makeCatalog(1016, "3.5 Flash")
	wRef := uiDo(h, "POST", "/_ui/api/models/refresh", "127.0.0.1:5000", s.uiToken)
	if wRef.Code != http.StatusConflict {
		t.Fatalf("refresh status = %d body=%s", wRef.Code, wRef.Body.String())
	}

	wState := uiDo(h, "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	var st struct {
		Status struct {
			ModelsCount  int `json:"models_count"`
			SessionState struct {
				SessionDegraded    bool   `json:"session_degraded"`
				SessionStatusLabel string `json:"session_status_label"`
				AccountStatusCode  int    `json:"account_status_code"`
			} `json:"session_state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(wState.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Status.ModelsCount != 3 || !st.Status.SessionState.SessionDegraded ||
		st.Status.SessionState.SessionStatusLabel != "session affaiblie : actualiser le cookie" ||
		st.Status.SessionState.AccountStatusCode != 1016 {
		t.Fatalf("unexpected UI degraded state: %+v", st.Status)
	}
	wHTML := uiDo(h, "GET", "/_ui", "127.0.0.1:5000", "")
	if !strings.Contains(wHTML.Body.String(), "Session affaiblie : actualiser le cookie") {
		t.Error("ui.html missing degraded session message")
	}
	if !strings.Contains(wHTML.Body.String(), "Rotation suspendue") {
		t.Error("ui.html missing suspended rotation badge")
	}
	if !strings.Contains(wHTML.Body.String(), "Conversations temporaires") {
		t.Error("ui.html missing temporary_chat tile")
	}
	if !strings.Contains(wHTML.Body.String(), "Identité non garantie sur ") || !strings.Contains(wHTML.Body.String(), "actualiser le cookie (raccourci 7)") {
		t.Error("ui.html missing name_only / model identity warning banner")
	}
}
