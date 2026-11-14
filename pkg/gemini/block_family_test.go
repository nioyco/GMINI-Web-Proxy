package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sorryResp(r *http.Request) *http.Response {
	h := http.Header{}
	h.Set("Location", "https://www.google.com/sorry/index?continue=https%3A%2F%2Fgemini.google.com%2Fapp&q=SECRETQ")
	return &http.Response{StatusCode: 302, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}
}

func fixedBlockTiming(t *testing.T) *time.Time {
	t.Helper()
	oldS, oldA, oldJ, oldN, oldP := blockSchedule, appBlockSchedule, jitterFn, nowFn, probeWindow
	t.Cleanup(func() { blockSchedule, appBlockSchedule, jitterFn, nowFn, probeWindow = oldS, oldA, oldJ, oldN, oldP })
	blockSchedule = []time.Duration{time.Minute, 2 * time.Minute}
	appBlockSchedule = []time.Duration{5 * time.Minute, 30 * time.Minute}
	jitterFn = func(d time.Duration) time.Duration { return d }
	probeWindow = time.Minute
	now := time.Now()
	nowFn = func() time.Time { return now }
	return &now
}

// AUDIT-13 S-01: a successful RotateCookies must not lift an /app block, and a
// second /app refusal must escalate the level.
func TestRotateSuccessDoesNotClearAppBlockAndLevelEscalates(t *testing.T) {
	now := fixedBlockTiming(t)
	var appCalls atomic.Int32
	c, logs := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "accounts.google.com" {
			return resp200(r, ""), nil // rotation OK, no new token
		}
		appCalls.Add(1)
		return sorryResp(r), nil
	}))
	ctx := context.Background()
	if _, err := c.fetchAppPage(ctx); !errors.Is(err, ErrGoogleBlocked) {
		t.Fatalf("err = %v", err)
	}
	*now = now.Add(5*time.Minute + time.Second) // /app backoff elapsed
	c.RotateCookie(ctx)
	bs := c.GoogleBlockStatus()
	if !bs.Active || bs.Families[FamilyApp].Level != 1 || !bs.Families[FamilyApp].Active {
		t.Fatalf("rotation success cleared the /app block: %+v", bs)
	}
	if _, err := c.fetchAppPage(ctx); !errors.Is(err, ErrGoogleBlocked) {
		t.Fatalf("err = %v", err)
	}
	bs = c.GoogleBlockStatus()
	if got := bs.Families[FamilyApp].Level; got != 2 {
		t.Errorf("level = %d, want 2 (escalation)", got)
	}
	if !bs.Families[FamilyApp].BackoffUntil.Equal(now.Add(30 * time.Minute)) {
		t.Errorf("backoff until %v, want +30m (long /app schedule)", bs.Families[FamilyApp].BackoffUntil)
	}
	if strings.Contains(logs.String(), "google block cleared") {
		t.Errorf("a block was reported cleared:\n%s", logs)
	}
}

// AUDIT-13 A3/O-01: the block names the triggering request (host + path), not
// only the anti-bot page, and never a query string.
func TestBlockedErrorNamesTriggeringRequest(t *testing.T) {
	fixedBlockTiming(t)
	c, logs := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) { return sorryResp(r), nil }))
	c.setAt("tok")
	_, err := c.batchExecute(context.Background(), listModelsRPC, "[]")
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v", err)
	}
	if be.Family != FamilyBatch || be.Host != "gemini.google.com" || be.Path != "/_/BardChatUi/data/batchexecute" ||
		be.TargetHost != "www.google.com" || be.TargetPath != "/sorry/index" {
		t.Errorf("blocked error = %+v", be)
	}
	l := logs.String()
	for _, want := range []string{"family=batchexecute", "host=gemini.google.com", "path=/_/BardChatUi/data/batchexecute", "target_path=/sorry/index"} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	for _, bad := range []string{"SECRETQ", "continue=", "rpcids", "tok"} {
		if strings.Contains(l, bad) || strings.Contains(err.Error(), bad) {
			t.Errorf("%q leaked: log=%s err=%v", bad, l, err)
		}
	}
	fs := c.GoogleBlockStatus().Families[FamilyBatch]
	if fs.LastTriggerPath != "/_/BardChatUi/data/batchexecute" || fs.LastTargetPath != "/sorry/index" {
		t.Errorf("family status = %+v", fs)
	}
}

func TestBlockGatingByFamily(t *testing.T) {
	now := fixedBlockTiming(t)
	c, _ := blockClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	c.recordBlock(&BlockedError{Family: FamilyApp, Host: "gemini.google.com", Path: "/app"})
	if c.acquireBackground(FamilyBatch) || c.acquireBackground(FamilyApp) || !c.blockedNow(FamilyGenerate) {
		t.Fatal("a gemini.google.com block must suspend the other gemini families")
	}
	if !c.acquireBackground(FamilyAccounts) || c.blockedNow(FamilyAccounts) {
		t.Fatal("an /app block must not suspend the cookie rotation")
	}
	c.recordGoogleOK(FamilyBatch) // a success of another family lifts nothing
	if !c.GoogleBlockStatus().Families[FamilyApp].Active {
		t.Fatal("batchexecute success cleared the /app block")
	}
	*now = now.Add(5*time.Minute + time.Second)
	if !c.acquireBackground(FamilyApp) {
		t.Fatal("one probe after the backoff")
	}
	if c.acquireBackground(FamilyBatch) {
		t.Fatal("a single probe per window across the gemini families")
	}
	c.recordBlock(&BlockedError{Family: FamilyAccounts, Host: "accounts.google.com", Path: "/RotateCookies"})
	if c.acquireBackground(FamilyAccounts) {
		t.Fatal("an accounts block suspends the rotation")
	}
	c.recordGoogleOK(FamilyApp)
	if c.acquireBackground(FamilyApp) {
		t.Fatal("an accounts block suspends the gemini families too")
	}
	bs := c.GoogleBlockStatus()
	if !bs.Active || bs.Families[FamilyApp].Active || !bs.Families[FamilyAccounts].Active {
		t.Errorf("status %+v", bs)
	}
}

func TestBlockMaxBackoffConfigurable(t *testing.T) {
	fixedBlockTiming(t)
	c, _ := blockClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	c.cfg.BlockMaxBackoff = 45 * time.Minute
	if got := c.backoffFor(FamilyApp, 2); got != 45*time.Minute {
		t.Errorf("top /app step = %v, want the configured 45m", got)
	}
	if got := c.backoffFor(FamilyApp, 1); got != 5*time.Minute {
		t.Errorf("first /app step = %v", got)
	}
	c.cfg.BlockMaxBackoff = 3 * time.Minute
	if got := c.backoffFor(FamilyApp, 1); got != 3*time.Minute {
		t.Errorf("steps are capped by the max: %v", got)
	}
}

// AUDIT-13 O-02: a failed catalog refresh is a WARN, rate-limited.
func TestCatalogRefreshFailureWarnRateLimited(t *testing.T) {
	c, logs := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("x")), Request: r}, nil
	}))
	c.SetModels([]*AvailableModel{{Name: "m", ModelID: "id"}})
	c.setAt("tok")
	c.fetchModels(context.Background())
	c.fetchModels(context.Background())
	if n := strings.Count(logs.String(), "level=WARN msg=\"model list refresh failed"); n != 1 {
		t.Errorf("WARN count = %d, want 1\n%s", n, logs)
	}
}
