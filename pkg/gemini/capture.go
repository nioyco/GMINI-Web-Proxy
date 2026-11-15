package gemini

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// captureNames are the rotating Google session cookies that are captured even when
// not yet present in the jar. Any other cookie is only updated when the jar
// already holds it (a browser-like jar update), never added.
var captureNames = map[string]bool{
	"__Secure-1PSIDTS": true, "__Secure-3PSIDTS": true,
	"__Secure-1PSIDCC": true, "__Secure-3PSIDCC": true,
	"SIDCC": true,
}

// captureSetCookies applies the Set-Cookie headers of any Google response to the
// cookie jar and persists the change atomically (file-backed: 0600; inline: in
// memory). Deletions (empty value, Max-Age<=0, past Expires) are ignored. Only
// cookie NAMES are ever logged.
func (c *Client) captureSetCookies(resp *http.Response) {
	cs := resp.Cookies()
	if len(cs) == 0 {
		return
	}
	c.captureMu.Lock()
	defer c.captureMu.Unlock()

	cur, _ := c.loadCookie()
	if cur == "" {
		return // anonymous: nothing to keep in sync
	}
	updated := cur
	var names []string
	psidts := false
	now := time.Now()
	for _, ck := range cs {
		if ck.Value == "" || ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(now)) {
			continue
		}
		have := cookieValue(updated, ck.Name)
		if !captureNames[ck.Name] && have == "" {
			continue
		}
		if have == ck.Value {
			continue
		}
		updated = replaceCookieValue(updated, ck.Name, ck.Value)
		names = append(names, ck.Name)
		if ck.Name == cookiePSIDTS {
			psidts = true
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	if err := c.storeCookie(updated); err != nil {
		c.log.Warn("captured session cookies could not be persisted", "names", strings.Join(names, ","), "err", err)
		return
	}
	if psidts {
		c.cookieMu.Lock()
		c.rotatedAt = now
		c.psidtsAt = now
		c.psidtsHash = psidtsHash(updated)
		c.cookieMu.Unlock()
		c.saveState()
	}
	c.log.Info("captured rotated session cookies from a Google response", "names", strings.Join(names, ","))
}
