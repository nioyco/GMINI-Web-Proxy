package gemini

import (
	"context"
	"encoding/json"
	"errors"
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
	c.setAt("tok") // authenticated: the old loop called listModels after each tick
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
	old := catalogRetryAfter
	catalogRetryAfter = 40 * time.Millisecond
	defer func() { catalogRetryAfter = old }()
	var log callLog
	body := listModelsBody(t, "x1")
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, body), nil
	}))
	c.setAt("tok")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.CatalogRefreshLoop(ctx, time.Hour) // never fetched: one fetch, then nothing for an hour
	if n := log.count(FamilyBatch); n != 1 || log.count("") != 1 {
		t.Errorf("calls=%d batch=%d, want exactly 1 listModels", log.count(""), n)
	}
	if c.ModelsSourceLabel() != "dynamic" {
		t.Errorf("source = %q", c.ModelsSourceLabel())
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

// AUDIT-13 A7: the rotation loop also runs for an unauthenticated session.
func TestRotationLoopRunsWithoutSession(t *testing.T) {
	var log callLog
	c, _ := blockClient(t, rtFunc(func(r *http.Request) (*http.Response, error) {
		log.add(r)
		return resp200(r, ""), nil
	}))
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
