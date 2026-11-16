package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listModelsBody builds a batchexecute answer carrying a listModels catalog.
func listModelsBody(t *testing.T, ids ...string) string {
	t.Helper()
	body := make([]any, 18)
	var ms []any
	for _, id := range ids {
		m := make([]any, 12)
		m[0], m[1], m[11] = id, "Label "+id, "Model "+id
		ms = append(ms, m)
	}
	body[15] = ms
	inner, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal([]any{[]any{"wrb.fr", listModelsRPC, string(inner)}})
	if err != nil {
		t.Fatal(err)
	}
	return ")]}'\n" + string(outer)
}

// callLog records the family of every outbound request.
type callLog struct {
	mu    sync.Mutex
	calls []CallFamily
}

func (l *callLog) add(r *http.Request) {
	l.mu.Lock()
	l.calls = append(l.calls, familyOf(r.URL))
	l.mu.Unlock()
}

func (l *callLog) count(f CallFamily) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if f == "" || c == f {
			n++
		}
	}
	return n
}

// AUDIT-13 A2: a rotation tick makes exactly one call (RotateCookies), whatever
// the session state; listModels is no longer chained to the tick.
func TestRotationTickMakesOneCall(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, ""), nil
	}))
	c.cfg.CookieRotation = true // mode on
	c.setAt("tok")              // authenticated: the old loop called listModels after each tick
	c.RecordUserActivity()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	c.CookieRefreshLoop(ctx, 20*time.Millisecond)
	rot := c.RotationStatus().Attempts
	if rot < 2 {
		t.Fatalf("only %d ticks", rot)
	}
	if got := log.count(""); int64(got) != rot || log.count(FamilyAccounts) != got {
		t.Errorf("calls=%d accounts=%d ticks=%d: a tick must make one RotateCookies call and nothing else",
			got, log.count(FamilyAccounts), rot)
	}
}

// AUDIT-13 A4: the registry accessors never call Google, even with no catalog
// timestamp at all.
func TestModelAccessorsNeverCallGoogle(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, ""), nil
	}))
	c.setAt("tok")
	c.SetModels([]*AvailableModel{{Name: "gemini-a", ModelID: "a"}})
	c.modelMu.Lock()
	c.catalogAt = time.Time{}
	c.modelMu.Unlock()
	for i := 0; i < 50; i++ {
		_ = c.ListModels()
		_ = c.ModelNames()
		_, _ = c.ResolveModel("gemini-a")
		_ = c.DefaultModelName()
		_ = c.UpstreamParams()
	}
	time.Sleep(30 * time.Millisecond)
	if n := log.count(""); n != 0 {
		t.Errorf("%d Google calls from accessors, want 0", n)
	}
}

func TestCatalogDueIn(t *testing.T) {
	c, _ := blockClient(t, rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	if d := c.catalogDueIn(24 * time.Hour); d != 0 {
		t.Errorf("never fetched: due in %v, want 0", d)
	}
	c.modelMu.Lock()
	c.catalogAt = time.Now().Add(-23 * time.Hour)
	c.modelMu.Unlock()
	if d := c.catalogDueIn(24 * time.Hour); d < 59*time.Minute || d > time.Hour {
		t.Errorf("fetched 23h ago: due in %v, want ~1h", d)
	}
	c.modelMu.Lock()
	c.catalogAt = time.Now().Add(-48 * time.Hour)
	c.catalogTriedAt = time.Now()
	c.modelMu.Unlock()
	if d := c.catalogDueIn(24 * time.Hour); d < 59*time.Minute {
		t.Errorf("overdue but just tried: due in %v, want the retry delay", d)
	}
}

func TestCatalogRefreshLoopOnlyWhenDue(t *testing.T) {
	oldN := nowFn
	now := time.Now()
	nowFn = func() time.Time { return now }
	defer func() { nowFn = oldN }()

	var log callLog
	body := listModelsBody(t, "x1")
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, body), nil
	}))
	c.setAt("tok")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.CatalogRefreshLoop(ctx, time.Millisecond)
		close(done)
	}()
	// Advance clock by 48h without any user request: 0 background catalog calls.
	for h := 0; h < 48; h++ {
		now = now.Add(time.Hour)
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if n := log.count(""); n != 0 {
		t.Errorf("calls=%d over 48h without request, want 0 (K14 P3)", n)
	}
}

func TestP3_CatalogAfterCookieThrottledTo6HoursAndBlockCooldown(t *testing.T) {
	oldN, oldB, oldJ := nowFn, blockSchedule, jitterFn
	now := time.Now()
	nowFn = func() time.Time { return now }
	blockSchedule = []time.Duration{5 * time.Minute}
	jitterFn = func(d time.Duration) time.Duration { return d }
	defer func() { nowFn, blockSchedule, jitterFn = oldN, oldB, oldJ }()

	var log callLog
	var blockNext atomic.Bool
	body := listModelsBody(t, "x1", "x2")
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		if blockNext.Load() {
			return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("sorry")), Request: r}, nil
		}
		return resp200(r, body), nil
	}))
	c.setAt("tok")
	c.RecordUserActivity()

	// 1st after-cookie refresh succeeds.
	if err := c.RefreshCatalogAfterCookie(context.Background()); err != nil {
		t.Fatalf("first after-cookie refresh failed: %v", err)
	}
	if log.count(FamilyBatch) != 1 {
		t.Fatalf("calls = %d, want 1", log.count(FamilyBatch))
	}
	// Within 6h: skipped without Google call.
	now = now.Add(5 * time.Hour)
	if err := c.RefreshCatalogAfterCookie(context.Background()); !errors.Is(err, ErrCatalogRefreshRefused) || log.count(FamilyBatch) != 1 {
		t.Fatalf("within 6h: err=%v calls=%d", err, log.count(FamilyBatch))
	}
	// After 6h: allowed again if user activity occurred recently.
	now = now.Add(61 * time.Minute)
	c.RecordUserActivity()
	if err := c.RefreshCatalogAfterCookie(context.Background()); err != nil || log.count(FamilyBatch) != 2 {
		t.Fatalf("after 6h: err=%v calls=%d", err, log.count(FamilyBatch))
	}

	// Block on manual refresh -> CatalogRefreshNextAt returns BackoffUntil, no auto retry.
	now = now.Add(10 * time.Minute)
	blockNext.Store(true)
	if err := c.RefreshModels(context.Background()); !errors.Is(err, ErrGoogleBlocked) {
		t.Fatalf("expected ErrGoogleBlocked, got %v", err)
	}
	if next := c.CatalogRefreshNextAt(); next.IsZero() || next.Sub(now) < 4*time.Minute {
		t.Fatalf("expected CatalogRefreshNextAt >= 5m block backoff, got %v", next.Sub(now))
	}
	// Even advancing clock 48h without explicit request triggers 0 further calls.
	blockNext.Store(false)
	now = now.Add(48 * time.Hour)
	if log.count(FamilyBatch) != 3 {
		t.Fatalf("expected 3 total calls (no background retries), got %d", log.count(FamilyBatch))
	}
}

func TestCatalogRefreshLoopSilentWithoutSession(t *testing.T) {
	old := catalogRetryAfter
	catalogRetryAfter = 10 * time.Millisecond
	defer func() { catalogRetryAfter = old }()
	var calls atomic.Int32
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return resp200(r, ""), nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	c.CatalogRefreshLoop(ctx, time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("%d calls without an authenticated session", calls.Load())
	}
}

func TestRefreshModelsOnRequest(t *testing.T) {
	var log callLog
	body := listModelsBody(t, "x1", "x2")
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, body), nil
	}))
	ctx := context.Background()
	if err := c.RefreshModels(ctx); !errors.Is(err, ErrCatalogRefreshRefused) || log.count("") != 0 {
		t.Fatalf("anonymous: err=%v calls=%d", err, log.count(""))
	}
	c.setAt("tok")
	if err := c.RefreshModels(ctx); err != nil || log.count(FamilyBatch) != 1 || len(c.ModelNames()) != 2 {
		t.Fatalf("err=%v calls=%d models=%v", err, log.count(""), c.ModelNames())
	}
	if err := c.RefreshModels(ctx); !errors.Is(err, ErrCatalogRefreshRefused) || !strings.Contains(err.Error(), "less than") {
		t.Fatalf("too soon: err=%v", err)
	}
	if log.count("") != 1 {
		t.Errorf("calls = %d, want 1", log.count(""))
	}
}

// AUDIT-13 A7: the rotation loop also runs for an unauthenticated session (when enabled).
func TestRotationLoopRunsWithoutSession(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, ""), nil
	}))
	c.cfg.CookieRotation = true // mode on
	c.RecordUserActivity()
	if c.Authenticated() || !c.HasRotatingToken() {
		t.Fatal("setup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	c.CookieRefreshLoop(ctx, 15*time.Millisecond)
	if log.count(FamilyAccounts) == 0 || log.count("") != log.count(FamilyAccounts) {
		t.Errorf("calls=%d accounts=%d", log.count(""), log.count(FamilyAccounts))
	}
}

func TestRotationDisabledByDefaultMakesNoCalls(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, ""), nil
	}))
	if c.cfg.CookieRotation {
		t.Fatal("CookieRotation must default to false")
	}
	c.StartupRotate(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c.CookieRefreshLoop(ctx, 10*time.Millisecond)
	if got := log.count(""); got != 0 {
		t.Fatalf("expected 0 calls when CookieRotation is off, got %d", got)
	}
	if rs := c.RotationStatus(); rs.Enabled {
		t.Fatalf("expected RotationStatus.Enabled=false, got true")
	}
}
