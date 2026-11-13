package gemini

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrGoogleBlocked is the typed error for Google's anti-bot response: a redirect
// to google.com/sorry/... or an HTTP 429. Use errors.Is(err, ErrGoogleBlocked).
var ErrGoogleBlocked = errors.New("google blocked the request (anti-bot /sorry/ page or HTTP 429)")

// BlockedError carries where the block was seen. It deliberately holds only the
// host and path (never the full URL or its query, which embeds the original
// request) so it is safe to log.
type BlockedError struct {
	Host   string
	Path   string
	Status int
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%v: host=%s path=%s status=%d", ErrGoogleBlocked, e.Host, e.Path, e.Status)
}

func (e *BlockedError) Unwrap() error { return ErrGoogleBlocked }

// blockSchedule is the exponential backoff applied after consecutive blocks.
// A var so tests can shrink it.
var blockSchedule = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

// probeWindow is the minimum spacing between background probes once the backoff
// has elapsed: at most one background Google call per window while blocked.
var probeWindow = time.Minute

// nowFn and jitterFn are test seams.
var (
	nowFn    = time.Now
	jitterFn = func(d time.Duration) time.Duration { // ±20 %
		return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
	}
)

// blockState tracks Google blocks. Guarded by mu.
type blockState struct {
	mu           sync.Mutex
	level        int // consecutive escalations
	since        time.Time
	lastSeen     time.Time
	backoffUntil time.Time
	probeUntil   time.Time
	total        int64
}

// BlockStatus is a secret-free snapshot of the block state.
type BlockStatus struct {
	Active       bool
	Since        time.Time
	LastSeen     time.Time
	BackoffUntil time.Time
	Level        int
	Total        int64
}

// GoogleBlockStatus returns the current block state.
func (c *Client) GoogleBlockStatus() BlockStatus {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	return BlockStatus{Active: !b.since.IsZero(), Since: b.since, LastSeen: b.lastSeen,
		BackoffUntil: b.backoffUntil, Level: b.level, Total: b.total}
}

// recordBlock registers a block. The backoff escalates only when the previous
// backoff has elapsed, so a burst of concurrent blocked requests counts once.
func (c *Client) recordBlock(be *BlockedError) {
	b := &c.block
	now := nowFn()
	b.mu.Lock()
	b.total++
	b.lastSeen = now
	if b.since.IsZero() {
		b.since = now
	}
	escalated := false
	if b.level == 0 || !now.Before(b.backoffUntil) {
		if b.level < len(blockSchedule) {
			b.level++
		}
		b.backoffUntil = now.Add(jitterFn(blockSchedule[b.level-1]))
		escalated = true
	}
	level, until := b.level, b.backoffUntil
	b.mu.Unlock()
	if escalated {
		c.log.Warn("google_blocked",
			"host", be.Host, "path", be.Path, "status", be.Status,
			"level", level, "backoff_until", until.Format(time.RFC3339),
			"hint", "Google served an anti-bot page; background calls are suspended")
	}
}

// recordGoogleOK clears the block state after any non-blocked Google response.
func (c *Client) recordGoogleOK() {
	b := &c.block
	b.mu.Lock()
	if b.since.IsZero() {
		b.mu.Unlock()
		return
	}
	since := b.since
	b.level, b.since, b.backoffUntil, b.probeUntil = 0, time.Time{}, time.Time{}, time.Time{}
	b.mu.Unlock()
	c.log.Info("google block cleared", "blocked_for_sec", int64(nowFn().Sub(since).Seconds()))
}

// blockedNow reports whether a backoff is currently in force.
func (c *Client) blockedNow() bool {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.since.IsZero() && nowFn().Before(b.backoffUntil)
}

// acquireBackground gates every background Google call (cookie rotation, model
// list refresh, build label, auth check). While not blocked it always allows.
// While blocked it refuses until the backoff elapsed, then grants one probe per
// probeWindow.
func (c *Client) acquireBackground() bool {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.since.IsZero() {
		return true
	}
	now := nowFn()
	if now.Before(b.backoffUntil) || now.Before(b.probeUntil) {
		return false
	}
	b.probeUntil = now.Add(probeWindow)
	return true
}

// blockedErrorFor returns a *BlockedError when resp (the answer to req) is an
// anti-bot response: HTTP 429, a request already on a /sorry/ path, or a
// redirect whose Location points to /sorry/.
func blockedErrorFor(req *http.Request, resp *http.Response) *BlockedError {
	be := &BlockedError{Host: req.URL.Host, Path: req.URL.Path, Status: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests {
		return be
	}
	if isSorryPath(req.URL.Path) && strings.Contains(req.URL.Host, "google.") {
		return be
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := req.URL.Parse(loc); err == nil && isSorryPath(u.Path) && strings.Contains(u.Host, "google.") {
			be.Host, be.Path = u.Host, u.Path
			return be
		}
	}
	return nil
}

func isSorryPath(p string) bool { return strings.HasPrefix(p, "/sorry/") || p == "/sorry" }

// googleTransport inspects every response (including each redirect hop) coming
// from Google: it detects anti-bot blocks and captures rotated session cookies.
type googleTransport struct {
	base http.RoundTripper
	c    *Client
}

func (t *googleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if be := blockedErrorFor(req, resp); be != nil {
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		t.c.recordBlock(be)
		return nil, be
	}
	t.c.recordGoogleOK()
	t.c.captureSetCookies(resp)
	return resp, nil
}

// SetTransport replaces the underlying HTTP transport (test seam for simulated
// upstreams; never used in production). The block/cookie inspection stays in
// place around it.
func (c *Client) SetTransport(rt http.RoundTripper) {
	c.http.Transport = &googleTransport{base: rt, c: c}
}

// cleanErr replaces an *url.Error wrapping a BlockedError by the BlockedError
// itself, so messages and logs carry host+path only (url.Error would add the
// request URL).
func cleanErr(err error) error {
	var be *BlockedError
	if errors.As(err, &be) {
		return be
	}
	return err
}
