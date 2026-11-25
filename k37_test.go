package main

// K37 test suite: status identity fields, diagnostics ring buffer pagination and
// filters, secret hygiene, export request validation, mocked export execution and
// the embedded diagnostics UI section.
//
// Everything here is deterministic and offline: no upstream Google call, no
// network access, no real export script. The fake exporter is a tiny Python file
// written into t.TempDir().

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// k37Secrets are the values configured on the test server: none of them may ever
// show up in an HTTP response body.
const (
	k37APIKey     = "sk-SECRETKEY"
	k37Cookie     = "SAPISID=SECRETCOOKIE; __Secure-1PSIDTS=SECRETTS"
	k37SecretKeys = "SECRETKEY"
	k37SecretCook = "SECRETCOOKIE"
	k37SecretTS   = "SECRETTS"
)

// k37UIServer builds a server with the management UI enabled and holding real
// looking secrets, so every assertion doubles as a leak check.
func k37UIServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.Listen = "127.0.0.1:8081"
	cfg.APIKeys = []string{k37APIKey}
	cfg.Gemini.Cookie = k37Cookie
	// "off" keeps the Antigravity model file handling away from the real home dir.
	cfg.AntigravityModelsFile = "off"
	s := newTestServer(t, cfg)
	return s, s.handler()
}

// k37Do issues a loopback request to the UI surface with an optional JSON body
// and UI token.
func k37Do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:8081"+path, reader)
	r.Host = "127.0.0.1:8081"
	r.RemoteAddr = "127.0.0.1:5000"
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

// k37Secrets lists the values that must never appear in a response body. The UI
// token is only added when set: an empty token would match everything.
func k37Secrets(s *Server) []string {
	out := []string{k37SecretKeys, k37SecretCook, k37SecretTS}
	if s.uiToken != "" {
		out = append(out, s.uiToken)
	}
	return out
}

// k37GetDiag performs a GET /_ui/api/diagnostics and decodes the envelope.
func k37GetDiag(t *testing.T, h http.Handler, token, query string) diagResponse {
	t.Helper()
	w := k37Do(h, http.MethodGet, "/_ui/api/diagnostics"+query, "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /_ui/api/diagnostics%s = %d: %s", query, w.Code, w.Body.String())
	}
	var resp diagResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode diagnostics%s: %v (body=%s)", query, err, w.Body.String())
	}
	return resp
}

func k37IDs(resp diagResponse) []string {
	ids := make([]string, 0, len(resp.Items))
	for _, ev := range resp.Items {
		ids = append(ids, ev.RequestID)
	}
	return ids
}

func k37SameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// k37SeedDiag records a fixed, time-relative set of events (5 activities and 2
// incidents) and returns the reverse insertion order, which is the order the
// ring buffer serves (newest insertion first).
func k37SeedDiag(t *testing.T, s *Server) []string {
	t.Helper()
	now := time.Now().UTC()
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	seed := []DiagEvent{
		{TS: ts(time.Minute), Kind: "activity", RequestID: "r1", Model: "gemini-3.5-flash", ToolCalls: 2, TTFTMS: 120, DurationMS: 900, PromptBytes: 1024, Status: 200},
		{TS: ts(2 * time.Minute), Kind: "activity", RequestID: "r2", Status: 500, Outcome: "error", ToolCalls: 1},
		{TS: ts(3 * time.Minute), Kind: "activity", RequestID: "r3", Status: 200},
		{TS: ts(4 * time.Minute), Kind: "activity", RequestID: "r4", Outcome: "cancelled"},
		{TS: ts(3 * time.Hour), Kind: "activity", RequestID: "r5", Status: 200},
		{TS: ts(time.Minute), Kind: "incident", RequestID: "i1", Type: "auth", Detail: "cookie_refresh_failed", Recovered: false},
		{TS: ts(2 * time.Minute), Kind: "incident", RequestID: "i2", Type: "upstream_timeout", Detail: "held stall", Recovered: true},
	}
	for _, ev := range seed {
		s.diag.record(ev)
	}
	return []string{"i2", "i1", "r5", "r4", "r3", "r2", "r1"}
}

// TestK37_StatusFields pins the identity block of /_status: pid, binary path and
// name, and both version numbers. /_ui/api/state reuses the same handler
// (ui.go handleUIState -> handleStatus), so covering /_status covers both.
func TestK37_StatusFields(t *testing.T) {
	_, h := k37UIServer(t)

	req := newReq(http.MethodGet, "/_status", nil)
	req.Header.Set("Authorization", "Bearer "+k37APIKey)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /_status = %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{k37SecretKeys, k37SecretCook, k37SecretTS} {
		if strings.Contains(body, secret) {
			t.Errorf("/_status leaks %q: %s", secret, body)
		}
	}

	var st struct {
		Version    string `json:"version"`
		UIVersion  string `json:"ui_version"`
		PID        int    `json:"pid"`
		BinaryPath string `json:"binary_path"`
		BinaryName string `json:"binary_name"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode /_status: %v (body=%s)", err, body)
	}
	if st.Version != version {
		t.Errorf("version = %q, want %q (const version = \"1.2.0\")", st.Version, version)
	}
	if st.UIVersion != uiVersion {
		t.Errorf("ui_version = %q, want %q (const uiVersion = \"2.2.0\")", st.UIVersion, uiVersion)
	}
	if st.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d", st.PID, os.Getpid())
	}
	if st.BinaryName == "" || st.BinaryPath == "" {
		t.Errorf("binary identity incomplete: path=%q name=%q", st.BinaryPath, st.BinaryName)
	}
	if want := filepath.Base(st.BinaryPath); st.BinaryName != want {
		t.Errorf("binary_name = %q, want filepath.Base(binary_path) = %q", st.BinaryName, want)
	}
}

// TestK37_DiagnosticsPaginationAndFilters exercises the whole GET contract:
// pagination (limit/offset/has_more), kind/status/type filters, the time window
// and the window-level summary counters (computed before the item filters).
func TestK37_DiagnosticsPaginationAndFilters(t *testing.T) {
	s, h := k37UIServer(t)
	reverseOrder := k37SeedDiag(t, s)

	// The endpoint is token-protected even on loopback.
	if w := k37Do(h, http.MethodGet, "/_ui/api/diagnostics", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET without token = %d, want 401", w.Code)
	}

	t.Run("all events newest first", func(t *testing.T) {
		resp := k37GetDiag(t, h, s.uiToken, "?window=all")
		if resp.Total != 7 || len(resp.Items) != 7 || resp.HasMore {
			t.Fatalf("total=%d items=%d has_more=%v, want 7/7/false", resp.Total, len(resp.Items), resp.HasMore)
		}
		if got := k37IDs(resp); !k37SameIDs(got, reverseOrder) {
			t.Errorf("order = %v, want reverse insertion %v", got, reverseOrder)
		}
		want := DiagSummary{ActivityCount: 5, IncidentCount: 2, OKCount: 3, ErrorCount: 1, ToolCallsTotal: 3}
		if resp.Summary != want {
			t.Errorf("summary = %+v, want %+v", resp.Summary, want)
		}
		if resp.Items[0].Kind != "incident" || resp.Items[0].Type != "upstream_timeout" {
			t.Errorf("first item = %+v", resp.Items[0])
		}
	})

	t.Run("pagination", func(t *testing.T) {
		page1 := k37GetDiag(t, h, s.uiToken, "?window=all&limit=2&offset=0")
		if page1.Total != 7 || !page1.HasMore || !k37SameIDs(k37IDs(page1), []string{"i2", "i1"}) {
			t.Errorf("page1 = total %d has_more %v ids %v", page1.Total, page1.HasMore, k37IDs(page1))
		}
		page2 := k37GetDiag(t, h, s.uiToken, "?window=all&limit=2&offset=2")
		if page2.Total != 7 || !page2.HasMore || !k37SameIDs(k37IDs(page2), []string{"r5", "r4"}) {
			t.Errorf("page2 = total %d has_more %v ids %v", page2.Total, page2.HasMore, k37IDs(page2))
		}
		last := k37GetDiag(t, h, s.uiToken, "?window=all&limit=4&offset=6")
		if last.Total != 7 || last.HasMore || !k37SameIDs(k37IDs(last), []string{"r1"}) {
			t.Errorf("last page = total %d has_more %v ids %v", last.Total, last.HasMore, k37IDs(last))
		}
		past := k37GetDiag(t, h, s.uiToken, "?window=all&limit=5&offset=7")
		if past.Total != 7 || past.HasMore || len(past.Items) != 0 {
			t.Errorf("offset past the end = total %d has_more %v items %d", past.Total, past.HasMore, len(past.Items))
		}
	})

	t.Run("kind filter keeps window summary", func(t *testing.T) {
		act := k37GetDiag(t, h, s.uiToken, "?kind=activity&window=all")
		if act.Total != 5 || !k37SameIDs(k37IDs(act), []string{"r5", "r4", "r3", "r2", "r1"}) {
			t.Errorf("activity = total %d ids %v", act.Total, k37IDs(act))
		}
		// The summary counts the whole window, not the filtered slice.
		if act.Summary.ActivityCount != 5 || act.Summary.IncidentCount != 2 {
			t.Errorf("activity summary = %+v, want 5 activities / 2 incidents", act.Summary)
		}
		inc := k37GetDiag(t, h, s.uiToken, "?kind=incident&window=all")
		if inc.Total != 2 || !k37SameIDs(k37IDs(inc), []string{"i2", "i1"}) {
			t.Errorf("incident = total %d ids %v", inc.Total, k37IDs(inc))
		}
	})

	t.Run("status and type filters", func(t *testing.T) {
		ok := k37GetDiag(t, h, s.uiToken, "?kind=activity&status=200&window=all")
		if !k37SameIDs(k37IDs(ok), []string{"r5", "r3", "r1"}) {
			t.Errorf("status=200 ids = %v", k37IDs(ok))
		}
		if ok.Summary.OKCount != 3 || ok.Summary.ErrorCount != 1 {
			t.Errorf("status=200 summary = %+v, want summary unaffected by the status filter", ok.Summary)
		}
		bad := k37GetDiag(t, h, s.uiToken, "?kind=activity&status=error&window=all")
		if !k37SameIDs(k37IDs(bad), []string{"r2"}) {
			t.Errorf("status=error ids = %v, want only r2 (r4 is cancelled, not an error)", k37IDs(bad))
		}
		auth := k37GetDiag(t, h, s.uiToken, "?kind=incident&type=auth&window=all")
		if auth.Total != 1 || !k37SameIDs(k37IDs(auth), []string{"i1"}) {
			t.Errorf("type=auth total=%d ids=%v", auth.Total, k37IDs(auth))
		}
	})

	t.Run("time windows", func(t *testing.T) {
		hour := k37GetDiag(t, h, s.uiToken, "?window=1h")
		if hour.Total != 6 || !k37SameIDs(k37IDs(hour), []string{"i2", "i1", "r4", "r3", "r2", "r1"}) {
			t.Errorf("window=1h total=%d ids=%v (r5 is 3h old)", hour.Total, k37IDs(hour))
		}
		if hour.Summary.ActivityCount != 4 || hour.Summary.ToolCallsTotal != 3 {
			t.Errorf("window=1h summary = %+v", hour.Summary)
		}
		quarter := k37GetDiag(t, h, s.uiToken, "?window=15m")
		if quarter.Total != 6 {
			t.Errorf("window=15m total = %d, want 6", quarter.Total)
		}
		explicitAll := k37GetDiag(t, h, s.uiToken, "?window=all&limit=50&offset=0&kind=all")
		if explicitAll.Total != 7 {
			t.Errorf("explicit kind=all total = %d, want 7", explicitAll.Total)
		}
	})

	t.Run("invalid parameters", func(t *testing.T) {
		for _, q := range []string{"?kind=bogus", "?window=2h", "?limit=0", "?limit=abc", "?offset=-1"} {
			if w := k37Do(h, http.MethodGet, "/_ui/api/diagnostics"+q, "", s.uiToken); w.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", q, w.Code)
			}
		}
	})

	t.Run("no server secret leaks", func(t *testing.T) {
		w := k37Do(h, http.MethodGet, "/_ui/api/diagnostics?window=all&limit=200", "", s.uiToken)
		body := w.Body.String()
		for _, secret := range k37Secrets(s) {
			if strings.Contains(body, secret) {
				t.Errorf("diagnostics body leaks %q: %s", secret, body)
			}
		}
	})
}

// TestK37_DiagnosticsNoSecrets proves the endpoint never echoes the secrets the
// server holds and that a Detail string is reduced to one truncated line. Note:
// diagRing.record only sanitizes Detail (no CRLF, 160-byte cap); it does not
// redact arbitrary text, so production call sites must never pass tokens there.
func TestK37_DiagnosticsNoSecrets(t *testing.T) {
	s, h := k37UIServer(t)
	now := time.Now().UTC().Format(time.RFC3339)
	s.diag.record(DiagEvent{TS: now, Kind: "activity", RequestID: "r1", Model: "gemini-3.5-flash", Status: 200})
	longDetail := strings.Repeat("a", 400) + "\r\nsecond line"
	s.diag.record(DiagEvent{TS: now, Kind: "incident", RequestID: "i1", Type: "auth", Detail: longDetail})

	w := k37Do(h, http.MethodGet, "/_ui/api/diagnostics?window=all", "", s.uiToken)
	if w.Code != http.StatusOK {
		t.Fatalf("GET diagnostics = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range k37Secrets(s) {
		if strings.Contains(body, secret) {
			t.Errorf("diagnostics body leaks %q: %s", secret, body)
		}
	}

	var resp diagResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	var got *DiagEvent
	for i := range resp.Items {
		if resp.Items[i].RequestID == "i1" {
			got = &resp.Items[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("incident i1 missing from %s", body)
	}
	if strings.ContainsAny(got.Detail, "\r\n") {
		t.Errorf("detail keeps line breaks: %q", got.Detail)
	}
	if len(got.Detail) > 160 {
		t.Errorf("detail length = %d, want <= 160", len(got.Detail))
	}
	if want := sanitizeDiagDetail(longDetail); got.Detail != want {
		t.Errorf("detail = %q, want sanitizeDiagDetail(longDetail) = %q", got.Detail, want)
	}
	// Sanity check on the sanitizer itself, independent of the handler.
	if s := sanitizeDiagDetail("x\ny\r\nz"); strings.ContainsAny(s, "\r\n") {
		t.Errorf("sanitizeDiagDetail keeps line breaks: %q", s)
	}
}

// TestK37_DiagnosticsExportValidation checks that malformed export requests are
// refused with 400 before any script lookup or execution. The configured script
// path does not exist, so an accepted request would answer 503 instead.
func TestK37_DiagnosticsExportValidation(t *testing.T) {
	s, h := k37UIServer(t)
	missing := filepath.Join(t.TempDir(), "absent_export_script.py")
	t.Setenv("GEMINI_DIAGNOSTICS_SCRIPT", missing)

	cases := []struct{ name, body string }{
		{"invalid format", `{"format":"yaml"}`},
		{"invalid window", `{"format":"md","window":"2h"}`},
		{"invalid model", `{"format":"md","model":"bad model;rm -rf"}`},
		{"invalid since", `{"format":"csv","since":"yesterday"}`},
		{"invalid json", `{"format":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", tc.body, s.uiToken)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status = %d, want 400 (body=%s)", tc.name, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "python3") || strings.Contains(w.Body.String(), "diagnostics script") {
				t.Errorf("%s: validation ran the script lookup: %s", tc.name, w.Body.String())
			}
		})
	}

	if w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", `{"format":"yaml"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("export without token = %d, want 401", w.Code)
	}
	// A valid request with a missing script is a clean 503, never a panic.
	if w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", `{"format":"md","window":"1h"}`, s.uiToken); w.Code != http.StatusServiceUnavailable {
		t.Errorf("valid request with missing script = %d, want 503 (body=%s)", w.Code, w.Body.String())
	}
}

// k37FakeExportScript is a stand-in for tools/export_gmini_diagnostics.py. It
// only honours --out/--format and writes the files the handler expects.
const k37FakeExportScript = `import os
import sys

out = ""
fmt = "all"
args = sys.argv[1:]
i = 0
while i < len(args):
    if args[i] == "--out" and i + 1 < len(args):
        out = args[i + 1]
        i += 2
        continue
    if args[i] == "--format" and i + 1 < len(args):
        fmt = args[i + 1]
        i += 2
        continue
    i += 1

if not out or not os.path.isdir(out):
    sys.exit(2)

files = {
    "report.md": "# GMINI diagnostics report\n\nK37 mock report line.\n",
    "requests.csv": "ts,kind,status\n2026-01-02T03:04:05Z,activity,200\n",
    "summary.json": '{"activity_count": 1, "incident_count": 0}',
}

if fmt == "md":
    wanted = ["report.md"]
elif fmt == "csv":
    wanted = ["requests.csv"]
else:
    wanted = ["report.md", "requests.csv", "summary.json"]

for name in wanted:
    with open(os.path.join(out, name), "w", encoding="utf-8") as handle:
        handle.write(files[name])

sys.exit(0)
`

// TestK37_DiagnosticsExportMock runs the export endpoints against a temporary
// Python exporter and checks the delivered artifacts (markdown body, CSV body,
// zip archive contents) plus the attachment headers.
func TestK37_DiagnosticsExportMock(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	s, h := k37UIServer(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "fake_export.py")
	if err := os.WriteFile(script, []byte(k37FakeExportScript), 0o700); err != nil {
		t.Fatalf("write fake exporter: %v", err)
	}
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatalf("chmod fake exporter: %v", err)
	}
	t.Setenv("GEMINI_DIAGNOSTICS_SCRIPT", script)

	t.Run("markdown", func(t *testing.T) {
		w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", `{"format":"md","window":"1h"}`, s.uiToken)
		if w.Code != http.StatusOK {
			t.Fatalf("format=md = %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
			t.Errorf("content-type = %q, want text/markdown", ct)
		}
		cd := w.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".md") {
			t.Errorf("content-disposition = %q, want attachment with a .md filename", cd)
		}
		if !strings.Contains(w.Body.String(), "K37 mock report line.") {
			t.Errorf("markdown body = %s", w.Body.String())
		}
	})

	t.Run("csv", func(t *testing.T) {
		w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", `{"format":"csv","window":"all"}`, s.uiToken)
		if w.Code != http.StatusOK {
			t.Fatalf("format=csv = %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
			t.Errorf("content-type = %q, want text/csv", ct)
		}
		if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".csv") {
			t.Errorf("content-disposition = %q", cd)
		}
		if !strings.Contains(w.Body.String(), "ts,kind,status") {
			t.Errorf("csv body = %s", w.Body.String())
		}
	})

	t.Run("all is a zip", func(t *testing.T) {
		w := k37Do(h, http.MethodPost, "/_ui/api/diagnostics/export", `{"format":"all","window":"all"}`, s.uiToken)
		if w.Code != http.StatusOK {
			t.Fatalf("format=all = %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/zip") {
			t.Errorf("content-type = %q, want application/zip", ct)
		}
		cd := w.Header().Get("Content-Disposition")
		if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".zip") {
			t.Errorf("content-disposition = %q, want attachment with a .zip filename", cd)
		}
		raw := w.Body.Bytes()
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("zip.NewReader: %v", err)
		}
		contents := make(map[string]string, len(zr.File))
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open %s in zip: %v", f.Name, err)
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Fatalf("read %s in zip: %v", f.Name, err)
			}
			contents[f.Name] = string(data)
		}
		for _, want := range []string{"report.md", "requests.csv", "summary.json"} {
			if _, ok := contents[want]; !ok {
				t.Errorf("zip misses %s (entries: %v)", want, zr.File)
			}
		}
		if !strings.Contains(contents["report.md"], "K37 mock report line.") {
			t.Errorf("report.md in zip = %q", contents["report.md"])
		}
		if !strings.Contains(contents["summary.json"], "activity_count") {
			t.Errorf("summary.json in zip = %q", contents["summary.json"])
		}
	})
}

// TestK37_UIIndexHasDiagnosticsSection guards the embedded UI page: the
// diagnostics panel must ship inside ui.html (//go:embed into uiHTML).
func TestK37_UIIndexHasDiagnosticsSection(t *testing.T) {
	if len(uiHTML) == 0 {
		t.Fatal("uiHTML is empty: ui.html was not embedded")
	}
	if !strings.Contains(string(uiHTML), "DIAGNOSTIC PROXY") {
		t.Fatal("ui.html is missing the diagnostics section marker \"DIAGNOSTIC PROXY\"")
	}
	// The page must fetch the new endpoints client-side.
	for _, endpoint := range []string{"/_ui/api/diagnostics", "/_ui/api/diagnostics/export"} {
		if !strings.Contains(string(uiHTML), endpoint) {
			t.Errorf("ui.html does not reference %s", endpoint)
		}
	}
}
