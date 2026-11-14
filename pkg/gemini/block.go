package gemini

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrGoogleBlocked is the typed error for Google's anti-bot response: a redirect
// to google.com/sorry/... or an HTTP 429. Use errors.Is(err, ErrGoogleBlocked).
var ErrGoogleBlocked = errors.New("google blocked the request (anti-bot /sorry/ page or HTTP 429)")

// CallFamily groups the Google calls the proxy makes. Block state is tracked per
// family so that a success of one kind of call (e.g. a cookie rotation on
// accounts.google.com) never lifts the block of another (e.g. the /app page).
type CallFamily string

// Call families.
const (
	FamilyAccounts CallFamily = "accounts"     // accounts.google.com (RotateCookies)
	FamilyApp      CallFamily = "app"          // gemini.google.com/app page (bl, at, auth check)
	FamilyBatch    CallFamily = "batchexecute" // gemini batchexecute RPCs (listModels)
	FamilyGenerate CallFamily = "generate"     // StreamGenerate and image upload
	FamilyOther    CallFamily = "other"        // any other Google URL
)

// allFamilies is the stable listing order of the families.
var allFamilies = []CallFamily{FamilyAccounts, FamilyApp, FamilyBatch, FamilyGenerate, FamilyOther}

// familyOf classifies a request URL (host and path only).
func familyOf(u *url.URL) CallFamily {
	if u == nil {
		return FamilyOther
	}
	switch {
	case u.Host == "accounts.google.com":
		return FamilyAccounts
	case strings.HasSuffix(u.Host, "googleapis.com"): // content-push upload
		return FamilyGenerate
	case strings.Contains(u.Path, "/batchexecute"):
		return FamilyBatch
	case strings.Contains(u.Path, "StreamGenerate"):
		return FamilyGenerate
	case strings.HasSuffix(u.Path, "/app"):
		return FamilyApp
	}
	return FamilyOther
}

// gatingFamilies returns the families whose block suspends a background call of
// family f. The three gemini.google.com families share their blocks (same host:
// a /sorry on one is taken as a warning for the others); a block on
// accounts.google.com suspends everything. A gemini.google.com block does NOT
// suspend the cookie rotation (accounts), so the session keeps being maintained
// during a long block (AUDIT-13 S-03).
func gatingFamilies(f CallFamily) []CallFamily {
	switch f {
	case FamilyAccounts:
		return []CallFamily{FamilyAccounts}
	case FamilyApp, FamilyBatch, FamilyGenerate:
		return []CallFamily{FamilyAccounts, FamilyApp, FamilyBatch, FamilyGenerate}
	}
	return []CallFamily{FamilyAccounts, f}
}

// BlockedError carries where the block was seen. It deliberately holds only hosts
// and paths (never a full URL or its query, which may embed the original
// request) so it is safe to log. Host/Path name the request that TRIGGERED the
// block (the first request of the redirect chain); TargetHost/TargetPath name
// where Google sent it (the anti-bot page, or the same request for an HTTP 429).
type BlockedError struct {
	Family     CallFamily
	Host       string
	Path       string
	TargetHost string
	TargetPath string
	Status     int
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%v: family=%s host=%s path=%s target_host=%s target_path=%s status=%d",
		ErrGoogleBlocked, e.family(), e.Host, e.Path, e.TargetHost, e.TargetPath, e.Status)
}

func (e *BlockedError) Unwrap() error { return ErrGoogleBlocked }

func (e *BlockedError) family() CallFamily {
	if e.Family == "" {
		return FamilyOther
	}
	return e.Family
}

// blockSchedule is the exponential backoff applied after consecutive blocks of a
// family; appBlockSchedule is the longer one used for the /app page, whose
// refusal is the costliest (AUDIT-13 A9). Vars so tests can shrink them. The top
// step is replaced by Config.BlockMaxBackoff when it is set.
var (
	blockSchedule    = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute}
	appBlockSchedule = []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, 60 * time.Minute}
)

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

// famBlock is the block state of one family.
type famBlock struct {
	level        int // consecutive escalations
	since        time.Time
	lastSeen     time.Time
	backoffUntil time.Time
	probeUntil   time.Time
	total        int64
	last         BlockedError // origin of the last block (hosts and paths only)
}

func (f *famBlock) active() bool { return !f.since.IsZero() }

// blockState tracks Google blocks per family. Guarded by mu.
type blockState struct {
	mu   sync.Mutex
	fams map[CallFamily]*famBlock
}

// fam returns (creating it) the state of family f. Caller holds mu.
func (b *blockState) fam(f CallFamily) *famBlock {
	if b.fams == nil {
		b.fams = map[CallFamily]*famBlock{}
	}
	fb := b.fams[f]
	if fb == nil {
		fb = &famBlock{}
		b.fams[f] = fb
	}
	return fb
}

// FamilyBlockStatus is a secret-free snapshot of one family's block state.
type FamilyBlockStatus struct {
	Active          bool
	Since           time.Time
	LastSeen        time.Time
	BackoffUntil    time.Time
	Level           int
	Total           int64
	LastTriggerHost string // request that triggered the last block (host only)
	LastTriggerPath string // … and its path (no query)
	LastTargetHost  string // where Google sent it (host only)
	LastTargetPath  string // … and its path (no query)
	LastStatus      int
}

// BlockStatus is a secret-free snapshot of the block state: a global view derived
// from the families (active if any is; earliest since; latest last-seen and
// backoff; highest level; summed total) plus the per-family detail.
type BlockStatus struct {
	Active       bool
	Since        time.Time
	LastSeen     time.Time
	BackoffUntil time.Time
	Level        int
	Total        int64
	Families     map[CallFamily]FamilyBlockStatus
}

// GoogleBlockStatus returns the current block state.
func (c *Client) GoogleBlockStatus() BlockStatus {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	out := BlockStatus{Families: map[CallFamily]FamilyBlockStatus{}}
	for _, f := range allFamilies {
		fb := b.fams[f]
		if fb == nil {
			continue
		}
		fs := FamilyBlockStatus{Active: fb.active(), Since: fb.since, LastSeen: fb.lastSeen,
			BackoffUntil: fb.backoffUntil, Level: fb.level, Total: fb.total,
			LastTriggerHost: fb.last.Host, LastTriggerPath: fb.last.Path,
			LastTargetHost: fb.last.TargetHost, LastTargetPath: fb.last.TargetPath, LastStatus: fb.last.Status}
		out.Families[f] = fs
		out.Total += fb.total
		if fb.lastSeen.After(out.LastSeen) {
			out.LastSeen = fb.lastSeen
		}
		if !fs.Active {
			continue
		}
		out.Active = true
		if out.Since.IsZero() || fb.since.Before(out.Since) {
			out.Since = fb.since
		}
		if fb.backoffUntil.After(out.BackoffUntil) {
			out.BackoffUntil = fb.backoffUntil
		}
		if fb.level > out.Level {
			out.Level = fb.level
		}
	}
	return out
}

// backoffFor returns the backoff of family f at escalation level (1-based).
func (c *Client) backoffFor(f CallFamily, level int) time.Duration {
	sched := blockSchedule
	if f == FamilyApp {
		sched = appBlockSchedule
	}
	max := c.cfg.BlockMaxBackoff
	if level >= len(sched) {
		if max > 0 {
			return max
		}
		return sched[len(sched)-1]
	}
	d := sched[level-1]
	if max > 0 && d > max {
		d = max
	}
	return d
}

// recordBlock registers a block on the family of be. The backoff escalates only
// when that family's previous backoff has elapsed, so a burst of concurrent
// blocked requests counts once.
func (c *Client) recordBlock(be *BlockedError) {
	f := be.family()
	b := &c.block
	now := nowFn()
	b.mu.Lock()
	fb := b.fam(f)
	fb.total++
	fb.lastSeen = now
	fb.last = *be
	if fb.since.IsZero() {
		fb.since = now
	}
	escalated := false
	if fb.level == 0 || !now.Before(fb.backoffUntil) {
		fb.level++
		lvl := fb.level
		if n := c.scheduleLen(f); lvl > n {
			lvl = n
			fb.level = n
		}
		fb.backoffUntil = now.Add(jitterFn(c.backoffFor(f, lvl)))
		escalated = true
	}
	level, until := fb.level, fb.backoffUntil
	b.mu.Unlock()
	if escalated {
		c.log.Warn("google_blocked",
			"family", string(f), "host", be.Host, "path", be.Path,
			"target_host", be.TargetHost, "target_path", be.TargetPath, "status", be.Status,
			"level", level, "backoff_until", until.Format(time.RFC3339),
			"hint", "Google served an anti-bot page; background calls of this family are suspended")
	}
}

func (c *Client) scheduleLen(f CallFamily) int {
	if f == FamilyApp {
		return len(appBlockSchedule)
	}
	return len(blockSchedule)
}

// recordGoogleOK clears the block state of family f after a non-blocked final
// response of that family. Other families are left untouched (AUDIT-13 S-01).
func (c *Client) recordGoogleOK(f CallFamily) {
	b := &c.block
	b.mu.Lock()
	fb := b.fams[f]
	if fb == nil || !fb.active() {
		b.mu.Unlock()
		return
	}
	since := fb.since
	fb.level, fb.since, fb.backoffUntil, fb.probeUntil = 0, time.Time{}, time.Time{}, time.Time{}
	b.mu.Unlock()
	c.log.Info("google block cleared", "family", string(f), "blocked_for_sec", int64(nowFn().Sub(since).Seconds()))
}

// blockedNow reports whether a backoff currently suspends calls of family f
// (its own block, or a gating family's: see gatingFamilies).
func (c *Client) blockedNow(f CallFamily) bool {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	now := nowFn()
	for _, g := range gatingFamilies(f) {
		if fb := b.fams[g]; fb != nil && fb.active() && now.Before(fb.backoffUntil) {
			return true
		}
	}
	return false
}

// acquireBackground gates every background Google call of family f (cookie
// rotation, catalog refresh, /app reload, auth check). While no gating family is
// blocked it always allows. Otherwise it refuses until every gating backoff has
// elapsed, then grants one probe per probeWindow (shared by the gating families).
func (c *Client) acquireBackground(f CallFamily) bool {
	b := &c.block
	b.mu.Lock()
	defer b.mu.Unlock()
	now := nowFn()
	type gated struct {
		fb *famBlock
		f  CallFamily
	}
	var active []gated
	for _, g := range gatingFamilies(f) {
		fb := b.fams[g]
		if fb == nil || !fb.active() {
			continue
		}
		if now.Before(fb.backoffUntil) || now.Before(fb.probeUntil) {
			return false
		}
		active = append(active, gated{fb, g})
	}
	// One probe per window: the window is the family's current backoff step (at
	// least probeWindow), so an inconclusive probe (network error, no answer) is
	// not followed by another one before a full step (AUDIT-13 A9).
	for _, a := range active {
		w := probeWindow
		if d := c.backoffFor(a.f, a.fb.level); d > w {
			w = d
		}
		a.fb.probeUntil = now.Add(w)
	}
	return true
}

// originRequest walks a redirect chain back to the request that started it.
func originRequest(req *http.Request) *http.Request {
	for req.Response != nil && req.Response.Request != nil {
		req = req.Response.Request
	}
	return req
}

// blockedErrorFor returns a *BlockedError when resp (the answer to req) is an
// anti-bot response: HTTP 429, a request already on a /sorry/ path, or a
// redirect whose Location points to /sorry/.
func blockedErrorFor(req *http.Request, resp *http.Response) *BlockedError {
	orig := originRequest(req)
	be := &BlockedError{Family: familyOf(orig.URL), Host: orig.URL.Host, Path: orig.URL.Path,
		TargetHost: req.URL.Host, TargetPath: req.URL.Path, Status: resp.StatusCode}
	if resp.StatusCode == http.StatusTooManyRequests {
		return be
	}
	if isSorryPath(req.URL.Path) && strings.Contains(req.URL.Host, "google.") {
		return be
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := req.URL.Parse(loc); err == nil && isSorryPath(u.Path) && strings.Contains(u.Host, "google.") {
			be.TargetHost, be.TargetPath = u.Host, u.Path
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
	// Only a final (non-redirect) answer proves the family is reachable again: an
	// intermediate hop may still end on the anti-bot page.
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		t.c.recordGoogleOK(familyOf(originRequest(req).URL))
	}
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
// itself, so messages and logs carry hosts and paths only (url.Error would add the
// request URL).
func cleanErr(err error) error {
	var be *BlockedError
	if errors.As(err, &be) {
		return be
	}
	return err
}
