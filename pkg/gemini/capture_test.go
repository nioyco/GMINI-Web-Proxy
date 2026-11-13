package gemini

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureSetCookiesFileBacked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(path, []byte("SAPISID=sapi; SID=sidv; __Secure-1PSIDTS=oldts"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	c, err := New(Config{CookieFile: path, GeminiBL: "bl", RetryAttempts: 1}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var lastCookie string
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		lastCookie = r.Header.Get("Cookie")
		h := http.Header{}
		h.Add("Set-Cookie", "__Secure-1PSIDTS=NEWTS1; Path=/; Secure")
		h.Add("Set-Cookie", "__Secure-1PSIDCC=NEWCC1; Path=/")
		h.Add("Set-Cookie", "NID=NOTINJAR; Path=/")                          // not held, not rotating: ignored
		h.Add("Set-Cookie", "SID=; Max-Age=0; Path=/")                       // deletion: ignored
		h.Add("Set-Cookie", "SIDCC=; Expires=Thu, 01 Jan 1970 00:00:00 GMT") // deletion: ignored
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(wrbBody("ok"))), Request: r}, nil
	}))
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if cookieValue(got, cookiePSIDTS) != "NEWTS1" || cookieValue(got, "__Secure-1PSIDCC") != "NEWCC1" || cookieValue(got, "SID") != "sidv" || cookieValue(got, "NID") != "" || cookieValue(got, "SAPISID") != "sapi" {
		t.Errorf("file = %q", got)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp files left: %v", entries)
	}
	l := logs.String()
	if !strings.Contains(l, "captured rotated session cookies") || !strings.Contains(l, "names=__Secure-1PSIDCC,__Secure-1PSIDTS") {
		t.Errorf("log: %s", l)
	}
	for _, v := range []string{"NEWTS1", "NEWCC1", "NOTINJAR"} {
		if strings.Contains(l, v) {
			t.Errorf("cookie value %q leaked in logs", v)
		}
	}
	if c.LastCookieRotation().IsZero() {
		t.Error("PSIDTS capture should refresh the token age")
	}
	// The next request must carry the captured values.
	c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !strings.Contains(lastCookie, "__Secure-1PSIDTS=NEWTS1") || !strings.Contains(lastCookie, "__Secure-1PSIDCC=NEWCC1") {
		t.Errorf("next request cookie = %q", lastCookie)
	}
}

func TestCaptureInlineCookieStaysInMemory(t *testing.T) {
	c, _ := New(Config{Cookie: "SAPISID=s; __Secure-1PSIDTS=old", GeminiBL: "bl", RetryAttempts: 1}, nil)
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Add("Set-Cookie", "__Secure-1PSIDTS=fresh; Path=/")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(appPageAuth)), Request: r}, nil
	}))
	if _, err := c.fetchAppPage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.loadCookie(); cookieValue(got, cookiePSIDTS) != "fresh" {
		t.Errorf("cookie = %q", got)
	}
}

func TestCaptureNoCookieAnonymousIgnored(t *testing.T) {
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1}, nil)
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Add("Set-Cookie", "__Secure-1PSIDTS=x; Path=/")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	c.fetchAppPage(context.Background())
	if got, _ := c.loadCookie(); got != "" {
		t.Errorf("anonymous client must not gain a cookie, got %q", got)
	}
}
