package gemini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func rotClient(t *testing.T, rt http.RoundTripper) (*Client, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c, err := New(Config{Cookie: "SAPISID=sapi; __Secure-1PSIDTS=oldts", GeminiBL: "bl"}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	return c, &logs
}

func TestRotationNoNewTokenLoggedAndCounted(t *testing.T) {
	old := rotateWarnEvery
	rotateWarnEvery = time.Hour
	defer func() { rotateWarnEvery = old }()

	c, logs := rotClient(t, rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	for i := 0; i < 3; i++ {
		c.RotateCookie(context.Background())
	}
	rs := c.RotationStatus()
	if rs.Attempts != 3 || rs.NotRenewed != 3 || rs.ConsecutiveFailure != 3 || rs.LastResult != rotNoNewToken || !rs.LastSuccess.IsZero() || !rs.PSIDTSPresent {
		t.Errorf("status = %+v", rs)
	}
	if n := strings.Count(logs.String(), "did not renew"); n != 1 {
		t.Errorf("WARN count = %d, want 1 (rate limited):\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "consecutive_failures=1") || !strings.Contains(logs.String(), "token_age_source=unknown") {
		t.Errorf("log: %s", logs)
	}
	if strings.Contains(logs.String(), "oldts") || strings.Contains(logs.String(), "sapi") {
		t.Errorf("log leaks cookie: %s", logs)
	}

	// Unlimited warnings when the rate limit is lifted.
	rotateWarnEvery = 0
	c.RotateCookie(context.Background())
	if n := strings.Count(logs.String(), "did not renew"); n != 2 || !strings.Contains(logs.String(), "consecutive_failures=4") {
		t.Errorf("after lifting limit: %s", logs)
	}
}

func TestRotationSuccessResetsStreak(t *testing.T) {
	var renew bool
	c, _ := rotClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		if renew && strings.Contains(r.URL.Host, "accounts.google.com") {
			h.Add("Set-Cookie", "__Secure-1PSIDTS=newts; Path=/; Secure")
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(""))}, nil
	}))
	c.RotateCookie(context.Background())
	renew = true
	c.RotateCookie(context.Background())
	rs := c.RotationStatus()
	if rs.Renewed != 1 || rs.NotRenewed != 1 || rs.ConsecutiveFailure != 0 || rs.LastResult != rotRenewed || rs.LastSuccess.IsZero() || rs.TokenAgeSource != "rotation" {
		t.Errorf("status = %+v", rs)
	}
}

func TestRotationTransportErrorAndSkipped(t *testing.T) {
	c, logs := rotClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }))
	c.RotateCookie(context.Background())
	if rs := c.RotationStatus(); rs.LastResult != rotError || rs.ConsecutiveFailure != 1 {
		t.Errorf("status = %+v", rs)
	}
	if !strings.Contains(logs.String(), "result=error") {
		t.Errorf("log: %s", logs)
	}
	anon, _ := New(Config{GeminiBL: "bl"}, nil)
	anon.RotateCookie(context.Background())
	if rs := anon.RotationStatus(); rs.LastResult != rotSkipped || rs.PSIDTSPresent {
		t.Errorf("anon status = %+v", rs)
	}
}

func TestRotationClassification(t *testing.T) {
	type tc struct {
		name   string
		resp   func(r *http.Request) (*http.Response, error)
		result string
		status int
		names  []string
		redir  bool
	}
	h := func(kv ...string) http.Header {
		hd := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			hd.Add(kv[i], kv[i+1])
		}
		return hd
	}
	cases := []tc{
		{"200 no cookie", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: h(), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, rotNoNewToken, 200, nil, false},
		{"200 other cookie only", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: h("Set-Cookie", "NID=SECRETVAL; Path=/", "Set-Cookie", "AEC=SECRET2"), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, rotNoNewToken, 200, []string{"AEC", "NID"}, false},
		{"500", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Header: h(), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, rotHTTPError, 500, nil, false},
		{"redirected to other host", func(r *http.Request) (*http.Response, error) {
			r2, _ := http.NewRequest("GET", "https://consent.google.com/x", nil)
			return &http.Response{StatusCode: 200, Header: h(), Body: io.NopCloser(strings.NewReader("")), Request: r2}, nil
		}, rotHTTPError, 200, nil, true},
		{"429 blocked", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Header: h(), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, rotBlocked, 0, nil, false},
		{"sorry redirect blocked", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 302, Header: h("Location", "https://www.google.com/sorry/index?q=Z"), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}, rotBlocked, 0, nil, false},
		{"transport error", func(*http.Request) (*http.Response, error) { return nil, errors.New("boom") }, rotError, 0, nil, false},
	}
	for _, c := range cases {
		cl, logs := rotClient(t, rtFunc(c.resp))
		cl.RotateCookie(context.Background())
		rs := cl.RotationStatus()
		if rs.LastResult != c.result || rs.LastHTTPStatus != c.status || rs.LastRedirected != c.redir || strings.Join(rs.LastSetCookieNames, ",") != strings.Join(c.names, ",") {
			t.Errorf("%s: %+v", c.name, rs)
		}
		if strings.Contains(logs.String(), "SECRETVAL") || strings.Contains(logs.String(), "SECRET2") {
			t.Errorf("%s: value leaked in logs: %s", c.name, logs)
		}
		if c.result == rotNoNewToken && len(c.names) > 0 && !strings.Contains(logs.String(), "set_cookie_names=AEC,NID") {
			t.Errorf("%s: names missing in log: %s", c.name, logs)
		}
	}
}
