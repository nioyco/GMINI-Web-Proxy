package gemini

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func blockClient(t *testing.T, rt http.RoundTripper) (*Client, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c, err := New(Config{RetryAttempts: 3, RetryDelaySec: 0, GeminiBL: "bl", Cookie: "SAPISID=s; __Secure-1PSIDTS=ts"}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rt)
	return c, &logs
}

func sorryRT(calls *atomic.Int32) rtFunc {
	return func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		h := http.Header{}
		h.Set("Location", "https://www.google.com/sorry/index?continue=https%3A%2F%2Fgemini.google.com%2Fapp&q=SECRETQ")
		return &http.Response{StatusCode: 302, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}
}

func TestSorryRedirectIsTypedErrorNoRetryNoQueryInLogs(t *testing.T) {
	var calls atomic.Int32
	c, logs := blockClient(t, sorryRT(&calls))
	_, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
	if !errors.Is(err, ErrGoogleBlocked) {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (no retry on block)", calls.Load())
	}
	l := logs.String()
	if !strings.Contains(l, "google_blocked") || !strings.Contains(l, "host=www.google.com") || !strings.Contains(l, "path=/sorry/index") {
		t.Errorf("log: %s", l)
	}
	if strings.Contains(l, "SECRETQ") || strings.Contains(l, "continue=") || strings.Contains(err.Error(), "SECRETQ") || strings.Contains(err.Error(), "?bl=") {
		t.Errorf("query leaked: log=%s err=%v", l, err)
	}
	bs := c.GoogleBlockStatus()
	if !bs.Active || bs.Total != 1 || bs.Level != 1 || !bs.BackoffUntil.After(time.Now()) {
		t.Errorf("status %+v", bs)
	}
}

func TestHTTP429IsBlockedStreamAndAppPage(t *testing.T) {
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("slow down")), Request: r}, nil
	})
	c, _ := blockClient(t, rt)
	if err := c.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(string) {}); !errors.Is(err, ErrGoogleBlocked) {
		t.Errorf("stream err = %v", err)
	}
	if _, err := c.fetchAppPage(context.Background()); !errors.Is(err, ErrGoogleBlocked) {
		t.Errorf("app page err = %v", err)
	}
	if got := c.CheckAuth(context.Background()); !strings.Contains(got, "blocked") {
		t.Errorf("CheckAuth = %q", got)
	}
}

func TestBlockEscalationAndClear(t *testing.T) {
	oldS, oldJ, oldN := blockSchedule, jitterFn, nowFn
	defer func() { blockSchedule, jitterFn, nowFn = oldS, oldJ, oldN }()
	blockSchedule = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}
	jitterFn = func(d time.Duration) time.Duration { return d }
	now := time.Now()
	nowFn = func() time.Time { return now }

	c, logs := blockClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	be := &BlockedError{Host: "www.google.com", Path: "/sorry/index", Status: 302}
	c.recordBlock(be)
	c.recordBlock(be) // burst inside the backoff: counted, not escalated
	bs := c.GoogleBlockStatus()
	if bs.Level != 1 || bs.Total != 2 || !bs.BackoffUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("after burst: %+v", bs)
	}
	now = now.Add(61 * time.Second)
	c.recordBlock(be)
	if bs = c.GoogleBlockStatus(); bs.Level != 2 || !bs.BackoffUntil.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("after 2nd: %+v", bs)
	}
	for i := 0; i < 5; i++ { // level is capped at the schedule length
		now = now.Add(6 * time.Minute)
		c.recordBlock(be)
	}
	if bs = c.GoogleBlockStatus(); bs.Level != 3 || !bs.BackoffUntil.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("cap: %+v", bs)
	}
	if strings.Count(logs.String(), "google_blocked") != 7 { // 8 calls, 1 inside the backoff
		t.Errorf("WARN lines: %d\n%s", strings.Count(logs.String(), "google_blocked"), logs)
	}
	c.recordGoogleOK(FamilyOther)
	if bs = c.GoogleBlockStatus(); bs.Active || bs.Level != 0 || !bs.BackoffUntil.IsZero() || bs.Total != 8 {
		t.Fatalf("after clear: %+v", bs)
	}
	if !strings.Contains(logs.String(), "google block cleared") {
		t.Errorf("no clear log")
	}
}

func TestAcquireBackgroundGate(t *testing.T) {
	oldS, oldJ, oldN, oldP := blockSchedule, jitterFn, nowFn, probeWindow
	defer func() { blockSchedule, jitterFn, nowFn, probeWindow = oldS, oldJ, oldN, oldP }()
	blockSchedule = []time.Duration{time.Minute}
	jitterFn = func(d time.Duration) time.Duration { return d }
	probeWindow = 30 * time.Second
	now := time.Now()
	nowFn = func() time.Time { return now }
	c, _ := blockClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("x") }))
	if !c.acquireBackground(FamilyOther) || !c.acquireBackground(FamilyOther) {
		t.Fatal("must allow when not blocked")
	}
	c.recordBlock(&BlockedError{Host: "h", Path: "/sorry/"})
	if c.acquireBackground(FamilyOther) {
		t.Fatal("must refuse during backoff")
	}
	now = now.Add(61 * time.Second)
	if !c.acquireBackground(FamilyOther) {
		t.Fatal("first probe after backoff must be granted")
	}
	if c.acquireBackground(FamilyOther) {
		t.Fatal("only one probe per window")
	}
	c.recordGoogleOK(FamilyOther)
	if !c.acquireBackground(FamilyOther) {
		t.Fatal("must allow after clear")
	}
}
