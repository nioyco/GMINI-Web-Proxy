package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUILaunchGuards(t *testing.T) {
	s, h := uiServer(t)
	do := func(remote, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8081/_ui/api/antigravity/launch", strings.NewReader(body))
		r.RemoteAddr = remote
		if token != "" {
			r.Header.Set("X-GMINI-UI", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("127.0.0.1:1", "", `{"path":"/Applications/Antigravity.app"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := do("10.0.0.9:1", s.uiToken, `{"path":"/Applications/Antigravity.app"}`); w.Code != http.StatusForbidden {
		t.Errorf("remote: %d", w.Code)
	}
	// A path that is not one of the scanned copies is never opened.
	for _, p := range []string{"/bin/sh", "/Applications/Calculator.app", "/tmp/Antigravity.app", ""} {
		w := do("127.0.0.1:1", s.uiToken, `{"path":"`+p+`"}`)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusConflict {
			t.Errorf("path %q: %d", p, w.Code)
		}
	}
}
