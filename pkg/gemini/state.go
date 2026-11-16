package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// DefaultStateMaxAge is how recent a persisted session token must be for a
// restart to skip the /app page (AUDIT-13 A5, R&D-14 R4). The real lifetime of
// the "at" token is unknown; a stale token costs one generation error, one
// /app reload and one new attempt (A6).
const DefaultStateMaxAge = 24 * time.Hour

// DefaultCatalogMaxRestoreAge is the maximum age of a persisted model catalog
// restored at startup (even across a session change, N8).
const DefaultCatalogMaxRestoreAge = 7 * 24 * time.Hour

// stateVersion is the format version of the state file.
const stateVersion = 1

// persistedState is the on-disk session state. It holds a secret (the XSRF "at"
// token): the file is written 0600, atomically, and its content is never logged
// nor exposed (only ages are).
type persistedState struct {
	Version     int               `json:"version"`
	SavedAt     time.Time         `json:"saved_at"`
	Session     string            `json:"session_fingerprint,omitempty"` // truncated SHA-256 of SAPISID
	At          string            `json:"at,omitempty"`
	AtAt        time.Time         `json:"at_at,omitempty"`
	BL          string            `json:"bl,omitempty"`
	BLAt        time.Time         `json:"bl_at,omitempty"`
	PSIDTSAt    time.Time         `json:"psidts_at,omitempty"`   // last real __Secure-1PSIDTS change (R&D-14 R4)
	PSIDTSHash  string            `json:"psidts_hash,omitempty"` // truncated SHA-256 of __Secure-1PSIDTS
	Models      []*AvailableModel `json:"models,omitempty"`
	ModelsAt    time.Time         `json:"models_at,omitempty"`
	ModelsSaved string            `json:"models_source,omitempty"`
	Withdrawn   []WithdrawnModel  `json:"withdrawn,omitempty"` // models a later catalogue dropped
}

func psidtsHash(cookie string) string {
	v := cookieValue(cookie, cookiePSIDTS)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:8])
}

// stateEnabled reports whether the session state is persisted: a state file is
// configured and the cookie is not inline (an inline cookie lives in memory
// only, its session state does too).
func (c *Client) stateEnabled() bool {
	return c.cfg.StateFile != "" && c.cfg.Cookie == ""
}

// sessionFingerprint identifies the Google session of a cookie without storing
// any of it: the first 16 hex digits of SHA-256(SAPISID). Empty without SAPISID.
func sessionFingerprint(cookie string) string {
	v := cookieValue(cookie, cookieSAPISID)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:8])
}

// saveState writes the current token, build label and catalog (with their
// timestamps) to the state file. A dropped token (session not authenticated) is
// written as absent. Failures are logged without content.
func (c *Client) saveState() {
	if !c.stateEnabled() {
		return
	}
	cookie, _ := c.currentCookieDirect()
	st := persistedState{Version: stateVersion, SavedAt: time.Now(), Session: sessionFingerprint(cookie)}
	c.atMu.Lock()
	st.At, st.AtAt = c.atToken, c.atAt
	c.atMu.Unlock()
	if st.At == "" {
		st.AtAt = time.Time{}
	}
	c.blMu.Lock()
	if c.blAuto && !c.blAt.IsZero() {
		st.BL, st.BLAt = c.bl, c.blAt
	}
	c.blMu.Unlock()
	if curHash := psidtsHash(cookie); curHash != "" {
		c.cookieMu.Lock()
		if c.psidtsHash != "" && c.psidtsHash != curHash {
			c.psidtsAt = time.Now()
		}
		c.psidtsHash = curHash
		if !c.psidtsAt.IsZero() {
			st.PSIDTSAt, st.PSIDTSHash = c.psidtsAt, c.psidtsHash
		}
		c.cookieMu.Unlock()
	}
	c.modelMu.Lock()
	if !c.catalogAt.IsZero() && (c.modelsSource == "dynamic" || c.modelsSource == "persisted") {
		for _, n := range c.modelOrder {
			if m := c.models[n]; m != nil {
				cp := *m
				st.Models = append(st.Models, &cp)
			}
		}
		st.ModelsAt, st.ModelsSaved = c.catalogAt, "dynamic"
		for _, w := range c.withdrawn {
			st.Withdrawn = append(st.Withdrawn, w)
		}
	}
	c.modelMu.Unlock()

	data, err := json.Marshal(st)
	if err != nil {
		c.log.Warn("session state not saved", "err", err)
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if err := writeFileAtomic(c.cfg.StateFile, data, 0o600); err != nil {
		c.log.Warn("session state not saved", "path", c.cfg.StateFile, "err", err)
		return
	}
	c.stateSavedAt = st.SavedAt
	if info, err := os.Stat(c.cfg.StateFile); err == nil {
		c.stateMtime = info.ModTime()
	}
}

// StateRestore tells what RestoreState found.
type StateRestore struct {
	// SessionFresh: a token younger than StateMaxAge for the configured session
	// was restored; the startup /app load can be skipped.
	SessionFresh bool
	// CatalogRestored: a catalog (< 7 days) was restored (source "persisted");
	// CatalogAt is the time it was fetched from Google.
	CatalogRestored bool
	// CatalogFresh: the restored catalog is younger than 24 h, so startup must
	// not send a batchexecute listModels call.
	CatalogFresh bool
	CatalogAt    time.Time
}

// RestoreState loads the state file (no Google call). The token is restored
// only when it belongs to the configured session and is younger than
// StateMaxAge; the build label (auto mode) and the catalog are restored for the
// same session whatever their age (the catalog is then refreshed by the catalog
// loop when due, and marked stale in /_status while the session is not
// authenticated).
func (c *Client) currentCookieDirect() (string, string) {
	if c.cfg.Cookie != "" {
		c.cookieMu.Lock()
		cached, cachedSapi := c.cookieStr, c.cookieSAPI
		c.cookieMu.Unlock()
		if cached != "" {
			return cached, cachedSapi
		}
		return c.cfg.Cookie, cookieValue(c.cfg.Cookie, cookieSAPISID)
	}
	if c.cfg.CookieFile == "" {
		return "", ""
	}
	cookie, sapisid, _ := c.loadCookieFile()
	return cookie, sapisid
}

func (c *Client) readPersistedState() (persistedState, time.Time, bool) {
	info, err := os.Stat(c.cfg.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return persistedState{}, time.Time{}, false
	}
	if err != nil {
		c.log.Warn("session state unreadable", "path", c.cfg.StateFile, "err", err)
		return persistedState{}, time.Time{}, false
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(c.cfg.StateFile, 0o600); err != nil {
			c.log.Warn("session state permissions too wide and not fixable; ignored", "path", c.cfg.StateFile)
			return persistedState{}, time.Time{}, false
		}
		c.log.Warn("session state permissions tightened to 0600", "path", c.cfg.StateFile)
	}
	data, err := os.ReadFile(c.cfg.StateFile)
	if err != nil {
		c.log.Warn("session state unreadable", "path", c.cfg.StateFile, "err", err)
		return persistedState{}, time.Time{}, false
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		c.log.Warn("session state ignored: unrecognised format", "path", c.cfg.StateFile)
		return persistedState{}, time.Time{}, false
	}
	return st, info.ModTime(), true
}

func (c *Client) applyRestoredSession(cookie string, st persistedState, mtime time.Time, resetRotation bool) bool {
	c.stateMu.Lock()
	c.stateMtime = mtime
	c.stateMu.Unlock()
	if st.BL != "" && c.blAuto {
		c.blMu.Lock()
		c.bl, c.blAt = st.BL, st.BLAt
		c.blMu.Unlock()
	}
	if curHash := psidtsHash(cookie); curHash != "" && !st.PSIDTSAt.IsZero() && (st.PSIDTSHash == "" || st.PSIDTSHash == curHash) {
		c.cookieMu.Lock()
		c.psidtsAt, c.psidtsHash = st.PSIDTSAt, curHash
		if resetRotation {
			c.rotatedAt = time.Time{}
			c.rot.noNewTokenStreak = 0
			c.rot.suspended = false
		}
		c.cookieMu.Unlock()
	} else if resetRotation && st.At != "" {
		c.cookieMu.Lock()
		c.rot.noNewTokenStreak = 0
		c.rot.suspended = false
		c.cookieMu.Unlock()
	}
	if st.At != "" && !st.AtAt.IsZero() && time.Since(st.AtAt) < c.cfg.StateMaxAge {
		c.atMu.Lock()
		c.atToken, c.atAt = st.At, st.AtAt
		c.atMu.Unlock()
		c.recordAuthCheck(authOK, c.cookieGeneration())
		return true
	}
	return false
}

// maybeReloadExternalState reloads at, bl and psidts_at in a running client when
// either cookie.txt or .state.json was rewritten externally (e.g. shortcut 7).
func (c *Client) maybeReloadExternalState(cookie string, cookieChanged bool) {
	if !c.stateEnabled() || c.cfg.Cookie != "" || c.cfg.CookieFile == "" {
		return
	}
	info, err := os.Stat(c.cfg.StateFile)
	if err != nil {
		return
	}
	c.stateMu.Lock()
	stateChanged := c.stateMtime.IsZero() || !info.ModTime().Equal(c.stateMtime)
	c.stateMu.Unlock()
	if !cookieChanged && !stateChanged {
		return
	}
	st, mtime, ok := c.readPersistedState()
	if !ok {
		return
	}
	if fp := sessionFingerprint(cookie); fp == "" || fp != st.Session {
		return
	}
	c.applyRestoredSession(cookie, st, mtime, true)
	c.log.Info("session state hot-reloaded from disk", "token_age_sec", ageSeconds(st.AtAt))
}

func (c *Client) restoreCatalog(st persistedState) (bool, time.Time) {
	if len(st.Models) == 0 || st.ModelsAt.IsZero() || time.Since(st.ModelsAt) >= DefaultCatalogMaxRestoreAge {
		return false, time.Time{}
	}
	c.storeModels(st.Models, "persisted")
	c.modelMu.Lock()
	c.catalogAt = st.ModelsAt
	for _, w := range st.Withdrawn {
		if _, listed := c.models[w.Model.Name]; !listed && w.Model.Name != "" {
			if c.withdrawn == nil {
				c.withdrawn = map[string]WithdrawnModel{}
			}
			c.withdrawn[w.Model.Name] = w
		}
	}
	c.modelMu.Unlock()
	return true, st.ModelsAt
}

func (c *Client) RestoreState() StateRestore {
	var out StateRestore
	if !c.stateEnabled() {
		return out
	}
	st, mtime, ok := c.readPersistedState()
	if !ok {
		return out
	}
	out.CatalogRestored, out.CatalogAt = c.restoreCatalog(st)
	out.CatalogFresh = out.CatalogRestored && time.Since(out.CatalogAt) < DefaultModelsRefreshInterval

	cookie, _ := c.currentCookieDirect()
	if fp := sessionFingerprint(cookie); fp == "" || fp != st.Session {
		c.log.Warn("session state token ignored: it belongs to another session (cookie changed)",
			"catalog_restored", out.CatalogRestored, "catalog_age_sec", ageSeconds(out.CatalogAt))
		return out
	}
	out.SessionFresh = c.applyRestoredSession(cookie, st, mtime, false)
	c.log.Info("session state restored", "session_fresh", out.SessionFresh,
		"token_age_sec", ageSeconds(st.AtAt), "catalog_restored", out.CatalogRestored,
		"catalog_age_sec", ageSeconds(out.CatalogAt))
	return out
}

func ageSeconds(t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return int64(time.Since(t).Seconds())
}

// SessionStateStatus is a secret-free view of the session state (ages only).
type SessionStateStatus struct {
	Persisted    bool      // a state file is in use
	LastSaved    time.Time // last successful write (zero: none in this process)
	TokenAt      time.Time // when the current XSRF token was obtained (zero: none)
	BuildLabelAt time.Time // when the build label was last resolved (zero: fallback)
	CatalogAt    time.Time // when the catalog was last fetched from Google (zero: never)
	CatalogStale bool      // a persisted catalog served while the session is not authenticated
}

// SessionState returns the session state view. It makes no Google call.
func (c *Client) SessionState() SessionStateStatus {
	_, _ = c.loadCookie()
	out := SessionStateStatus{Persisted: c.stateEnabled()}
	c.stateMu.Lock()
	out.LastSaved = c.stateSavedAt
	c.stateMu.Unlock()
	c.atMu.Lock()
	out.TokenAt = c.atAt
	authed := c.atToken != ""
	c.atMu.Unlock()
	c.blMu.Lock()
	out.BuildLabelAt = c.blAt
	c.blMu.Unlock()
	c.modelMu.Lock()
	out.CatalogAt = c.catalogAt
	out.CatalogStale = c.modelsSource == "persisted" && !authed
	c.modelMu.Unlock()
	return out
}
