package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// rotateEndpoint issues a fresh __Secure-1PSIDTS via Set-Cookie.
const rotateEndpoint = "https://accounts.google.com/RotateCookies"

// rotatePayload is the fixed body the Gemini web client sends to RotateCookies.
const rotatePayload = `[000,"-0000000000000000000"]`

// Google cookie names used by the proxy: SAPISID feeds the SAPISIDHASH auth
// header; __Secure-1PSIDTS is the rotating session-freshness token.
const (
	cookieSAPISID = "SAPISID"
	cookiePSIDTS  = "__Secure-1PSIDTS"
)

// CookieRefreshLoop periodically rotates __Secure-1PSIDTS so the configured cookie
// (file-backed or inline) does not go stale, and refreshes the model list after
// each rotation (a renewed session may surface a changed account catalog). The
// initial rotation is expected to be done synchronously at startup, so the loop
// only ticks on the interval and stops when ctx is cancelled.
func (c *Client) CookieRefreshLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// While Google blocks us, background calls are suspended (one probe
			// per window once the backoff elapsed).
			if !c.acquireBackground() {
				c.log.Debug("cookie refresh skipped: google block backoff in force")
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			c.RotateCookie(rctx)
			if !c.blockedNow() {
				c.fetchModels(rctx)
			}
			cancel()
		}
	}
}

// startupRotateMinAge is how recent the token must be for the startup rotation to
// be skipped (a restart right after a rotation must not call Google again).
const startupRotateMinAge = 5 * time.Minute

// RotateCookieIfStale rotates unless the current token is younger than minAge
// (age from the last successful rotation, else the cookie file's mtime).
func (c *Client) RotateCookieIfStale(ctx context.Context, minAge time.Duration) {
	if age, src := c.tokenAge(); src != "unknown" && age < minAge {
		c.log.Info("cookie rotation skipped: token is recent",
			"token_age_sec", int64(age.Seconds()), "token_age_source", src)
		return
	}
	c.RotateCookie(ctx)
}

// StartupRotate is RotateCookieIfStale with the default recency threshold.
func (c *Client) StartupRotate(ctx context.Context) { c.RotateCookieIfStale(ctx, startupRotateMinAge) }

// authRecoverySchedule spaces the auth re-checks while the session is not
// authenticated (a var so tests can shrink it).
var authRecoverySchedule = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

// AuthRecoveryLoop re-checks the session until it authenticates, so a proxy that
// started anonymous (Google block, expired cookie later re-exported to the cookie
// file) recovers without a restart. It honours the block backoff, spaces checks
// by authRecoverySchedule (with jitter), and on success refreshes the model list
// and calls onRecovered (e.g. to start the cookie refresh loop), then returns.
func (c *Client) AuthRecoveryLoop(ctx context.Context, onRecovered func()) {
	fails := 0
	for {
		i := fails
		if i >= len(authRecoverySchedule) {
			i = len(authRecoverySchedule) - 1
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitterFn(authRecoverySchedule[i])):
		}
		if !c.acquireBackground() {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c.invalidateAppPage() // a re-check must see the live page, not the startup copy
		status := c.CheckAuth(rctx)
		if status != "authenticated" {
			cancel()
			if status == "anonymous" {
				continue // cookie not configured (yet): keep waiting at the slowest pace
			}
			fails++
			continue
		}
		c.fetchModels(rctx)
		cancel()
		c.modelMu.Lock()
		count, source := len(c.modelOrder), c.modelsSource
		c.modelMu.Unlock()
		c.log.Info("gemini session recovered: authenticated, model list refreshed",
			"models", count, "source", source)
		if onRecovered != nil {
			onRecovered()
		}
		return
	}
}

// RotateCookie fetches a fresh __Secure-1PSIDTS and applies it. A file-backed
// cookie is rewritten in place (so it survives restarts); an inline cookie is
// rotated in memory only (it survives the process run but not a restart).
func (c *Client) RotateCookie(ctx context.Context) {
	cookie, _ := c.loadCookie()
	if cookie == "" || cookieValue(cookie, cookiePSIDTS) == "" {
		c.recordRotation(rotSkipped, nil, rotateInfo{})
		return
	}
	newTS, info, err := c.fetchRotatedTS(ctx, cookie)
	if err != nil {
		if errors.Is(err, ErrGoogleBlocked) {
			c.recordRotation(rotBlocked, err, info)
			return
		}
		c.log.Warn("cookie rotation request failed", "err", cleanErr(err))
		c.recordRotation(rotError, err, info)
		return
	}
	if newTS == "" {
		// Google issued no new token this cycle: the session is not being renewed.
		// A non-2xx status or a redirect is an HTTP-level failure, a plain 200
		// without the cookie is "no_new_token".
		if info.Status < 200 || info.Status > 299 || info.Redirected {
			c.recordRotation(rotHTTPError, nil, info)
		} else {
			c.recordRotation(rotNoNewToken, nil, info)
		}
		return
	}
	// The response may already have been applied by the Set-Cookie capture in the
	// transport; start from the current jar so nothing is overwritten.
	cur, _ := c.loadCookie()
	if cur == "" {
		cur = cookie
	}
	if newCookie := replaceCookieValue(cur, cookiePSIDTS, newTS); newCookie != cur {
		if err := c.storeCookie(newCookie); err != nil {
			c.log.Warn("cookie rotation persist failed", "err", err)
			return
		}
	}
	c.cookieMu.Lock()
	c.rotatedAt = time.Now()
	c.cookieMu.Unlock()
	c.recordRotation(rotRenewed, nil, info)
	c.log.Info("rotated __Secure-1PSIDTS cookie")
	// Keep the XSRF "at" token in sync with the rotated session. The model list is
	// refreshed by the caller (startup, then the periodic refresh loop).
	c.refreshAtToken(ctx)
}

// storeCookie makes newCookie the current cookie jar: a file-backed cookie is
// rewritten atomically (0600, DEC-33), an inline cookie is kept in memory only.
func (c *Client) storeCookie(newCookie string) error {
	if c.cfg.Cookie == "" && c.cfg.CookieFile != "" {
		if err := writeFileAtomic(c.cfg.CookieFile, []byte(newCookie), 0o600); err != nil {
			return err
		}
	}
	c.cookieMu.Lock()
	c.cookieStr, c.cookieSAPI, c.cookieLoaded = newCookie, cookieValue(newCookie, cookieSAPISID), true
	if c.cfg.CookieFile != "" {
		if info, err := os.Stat(c.cfg.CookieFile); err == nil {
			c.cookieMtime = info.ModTime()
		}
	}
	c.cookieMu.Unlock()
	return nil
}

// Rotation outcomes.
const (
	rotRenewed    = "renewed"
	rotNoNewToken = "no_new_token"
	rotError      = "error"
	rotBlocked    = "blocked"    // Google anti-bot (/sorry/ redirect or HTTP 429)
	rotHTTPError  = "http_error" // non-2xx status or redirect, no token
	rotSkipped    = "skipped_no_psidts"
)

// rotateWarnEvery rate-limits the "not renewed" WARN (a var so tests can shrink it).
var rotateWarnEvery = 10 * time.Minute

// rotState tracks rotation attempts (guarded by cookieMu).
type rotState struct {
	lastAttempt time.Time
	lastResult  string
	attempts    int64
	renewed     int64
	notRenewed  int64 // attempts that did not renew the token (total)
	streak      int64 // consecutive attempts without renewal
	lastWarn    time.Time
	lastInfo    rotateInfo // HTTP-level details of the last attempt (no values)
}

// rotateInfo describes the RotateCookies exchange without any cookie value.
type rotateInfo struct {
	Status      int      // HTTP status of the final response (0: no response)
	CookieNames []string // names of the Set-Cookie headers received, sorted
	Redirected  bool     // the final response did not come from the endpoint itself
}

// RotationStatus is a secret-free snapshot of the cookie rotation state.
type RotationStatus struct {
	LastAttempt        time.Time
	LastSuccess        time.Time
	LastResult         string // renewed | no_new_token | error | skipped_no_psidts | "" (no attempt yet)
	Attempts           int64
	Renewed            int64
	NotRenewed         int64
	ConsecutiveFailure int64
	TokenAge           time.Duration // age of the current token; 0 when unknown
	TokenAgeSource     string        // rotation | file_mtime | unknown
	PSIDTSPresent      bool
	LastHTTPStatus     int      // HTTP status of the last RotateCookies call (0: none/transport error)
	LastSetCookieNames []string // names (never values) of the Set-Cookie headers of the last call
	LastRedirected     bool
}

// recordRotation updates the rotation state and, when the token was not renewed,
// emits a WARN (first occurrence, then at most once per rotateWarnEvery) that
// carries the token age and the consecutive-failure count.
func (c *Client) recordRotation(result string, err error, info rotateInfo) {
	c.cookieMu.Lock()
	r := &c.rot
	r.lastAttempt, r.lastResult, r.lastInfo = time.Now(), result, info
	r.attempts++
	if result == rotRenewed {
		r.renewed++
		r.streak = 0
		c.cookieMu.Unlock()
		return
	}
	r.notRenewed++
	r.streak++
	streak := r.streak
	warn := r.lastWarn.IsZero() || time.Since(r.lastWarn) >= rotateWarnEvery
	if warn {
		r.lastWarn = time.Now()
	}
	c.cookieMu.Unlock()
	if !warn {
		return
	}
	age, src := c.tokenAge()
	c.log.Warn("cookie rotation did not renew the session token",
		"result", result, "http_status", info.Status, "set_cookie_names", strings.Join(info.CookieNames, ","),
		"redirected", info.Redirected,
		"token_age_sec", int64(age.Seconds()), "token_age_source", src,
		"consecutive_failures", streak)
}

// tokenAge returns the age of the current __Secure-1PSIDTS: since the last
// successful rotation, else since the cookie file's mtime, else unknown.
func (c *Client) tokenAge() (time.Duration, string) {
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	switch {
	case !c.rotatedAt.IsZero():
		return time.Since(c.rotatedAt), "rotation"
	case !c.cookieMtime.IsZero():
		return time.Since(c.cookieMtime), "file_mtime"
	}
	return 0, "unknown"
}

// RotationStatus returns a snapshot of the rotation state. It exposes no secret.
func (c *Client) RotationStatus() RotationStatus {
	cookie, _ := c.loadCookie()
	age, src := c.tokenAge()
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	return RotationStatus{
		LastAttempt: c.rot.lastAttempt, LastSuccess: c.rotatedAt, LastResult: c.rot.lastResult,
		Attempts: c.rot.attempts, Renewed: c.rot.renewed, NotRenewed: c.rot.notRenewed,
		ConsecutiveFailure: c.rot.streak, TokenAge: age, TokenAgeSource: src,
		PSIDTSPresent:  cookieValue(cookie, cookiePSIDTS) != "",
		LastHTTPStatus: c.rot.lastInfo.Status, LastSetCookieNames: append([]string(nil), c.rot.lastInfo.CookieNames...),
		LastRedirected: c.rot.lastInfo.Redirected,
	}
}

// renameFile is os.Rename; a variable so tests can simulate a failing rename.
var renameFile = os.Rename

// writeFileAtomic writes data to a temporary file in the same directory (created
// 0600), fsyncs it, then renames it over path. On any failure the temp file is
// removed and the previous file is left untouched. No backup copy is kept.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := renameFile(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// fetchRotatedTS POSTs to RotateCookies and returns the fresh __Secure-1PSIDTS
// value from the response Set-Cookie header (empty if none was issued) together
// with the HTTP-level details of the exchange (status, Set-Cookie NAMES, redirect).
func (c *Client) fetchRotatedTS(ctx context.Context, cookie string) (string, rotateInfo, error) {
	var info rotateInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rotateEndpoint, strings.NewReader(rotatePayload))
	if err != nil {
		return "", info, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", info, cleanErr(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	info.Status = resp.StatusCode
	if resp.Request != nil && resp.Request.URL != nil {
		info.Redirected = resp.Request.URL.Host != req.URL.Host || resp.Request.URL.Path != req.URL.Path
	}
	info.CookieNames = setCookieNames(resp)
	for _, sc := range resp.Header["Set-Cookie"] {
		if v, ok := setCookieValue(sc, cookiePSIDTS); ok {
			return v, info, nil
		}
	}
	return "", info, nil
}

// setCookieNames returns the sorted, de-duplicated names of the Set-Cookie
// headers of resp. Values are never read.
func setCookieNames(resp *http.Response) []string {
	seen := map[string]bool{}
	var names []string
	for _, sc := range resp.Header["Set-Cookie"] {
		name, _, _ := strings.Cut(strings.TrimSpace(sc), "=")
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// cookieValue returns the value of the named cookie in a "k=v; k=v" cookie jar
// string, or "" when absent. It tolerates either "; " or ";" between pairs.
func cookieValue(cookie, name string) string {
	for _, pair := range strings.Split(cookie, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && k == name {
			return v
		}
	}
	return ""
}

// setCookieValue extracts the value of key from a Set-Cookie header line.
func setCookieValue(setCookie, key string) (string, bool) {
	if !strings.HasPrefix(setCookie, key+"=") {
		return "", false
	}
	v := setCookie[len(key)+1:]
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return v, true
}

// replaceCookieValue returns cookieStr with key's value replaced (appending the
// pair if key is absent), normalizing the "k=v; k=v" formatting.
func replaceCookieValue(cookieStr, key, value string) string {
	var out []string
	found := false
	for _, p := range strings.Split(cookieStr, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, _, _ := strings.Cut(p, "=")
		if k == key {
			out = append(out, key+"="+value)
			found = true
		} else {
			out = append(out, p)
		}
	}
	if !found {
		out = append(out, key+"="+value)
	}
	return strings.Join(out, "; ")
}
