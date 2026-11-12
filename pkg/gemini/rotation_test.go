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
