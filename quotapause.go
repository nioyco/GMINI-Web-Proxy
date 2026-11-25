package main

// K41 — global premium-model quota pause.
//
// When the Gemini Web 5 h or weekly quota is reached, Google silently serves
// every linked premium model (Hydrogen, Neon, Argon/Carbon) with "3.5 Flash-Lite"
// and may add a "Limit reached" banner (inner[52]). Retrying is pointless and
// burns the remaining window, so the proxy pauses all non-Auto/Flash models
// until the operator resumes them. Only metadata is stored or logged (model
// names, the Google banner text); never prompts, answers, cookies or keys.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

const (
	quotaPauseRetryAfterSec = 300
	quotaPauseErrorCode     = "quota_fallback_paused"
	quotaBannerMaxRunes     = 300
	quotaUsageURL           = "https://gemini.google.com/usage"
)

// QuotaPauseState is the JSON-exposed state of the global premium pause.
type QuotaPauseState struct {
	Active           bool    `json:"active"`
	TriggeredByModel string  `json:"triggered_by_model"`
	TriggeredByLabel string  `json:"triggered_by_label"`
	RequestedID      string  `json:"requested_id"`
	ServedID         string  `json:"served_id"`
	ServedName       string  `json:"served_name"`
	Reason           string  `json:"reason"` // other_model | limit_banner
	LimitBanner      string  `json:"limit_banner"`
	PausedAt         *string `json:"paused_at"`
	BlockedCount     int64   `json:"blocked_count"`
	StaleToken       bool    `json:"stale_token,omitempty"`
}

type quotaPauseTracker struct {
	mu        sync.Mutex
	st        QuotaPauseState
	cookieGen uint64
}

func (t *quotaPauseTracker) snapshot() QuotaPauseState {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.st
	if st.PausedAt != nil {
		v := *st.PausedAt
		st.PausedAt = &v
	}
	return st
}

// flashSlugs are the catalogue slugs that map to Auto / Flash (8c46e95b1a07cecc).
var flashSlugs = map[string]bool{
	"gemini-auto":           true,
	"gemini-3.5-flash":      true,
	"gemini-3.5-flash-lite": true,
	"gemini-quick-answers":  true,
}

// isAutoOrFlashModel reports whether m is the quota-free Auto / Flash model.
func isAutoOrFlashModel(m *gemini.AvailableModel) bool {
	if m == nil {
		return false
	}
	if m.ModelID == gemini.AutoModelHexID || gemini.IsAutoModel(m.ModelID, m.Name, m.DisplayName) {
		return true
	}
	return flashSlugs[strings.ToLower(strings.TrimSpace(m.Name))]
}

func (s *Server) pauseOnFallbackEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.PauseOnFallback
}

func (s *Server) quotaPauseSnapshot() QuotaPauseState { return s.quotaPause.snapshot() }

// cleanLimitBanner strips control characters and bounds the banner length.
func cleanLimitBanner(msg string) string {
	var b strings.Builder
	for _, r := range msg {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(out) > quotaBannerMaxRunes {
		out = string([]rune(out)[:quotaBannerMaxRunes]) + "…"
	}
	return out
}

// servedOtherModel reports whether Google served a model other than the premium
// one requested (a name-only difference is not a fallback).
func servedOtherModel(m *gemini.AvailableModel, servedID, servedName string) bool {
	if m == nil {
		return false
	}
	if isAutoOrFlashModel(m) {
		return false
	}
	if servedID != "" {
		if m.ModelID == "" || servedID == m.ModelID {
			return false
		}
		if m.Description != "" && servedName == m.Description {
			return false
		}
		return true
	}
	if servedName != "" && m.Description != "" && servedName != m.Description && (m.DisplayName == "" || servedName != m.DisplayName) {
		return true
	}
	return false
}

func modelLabel(m *gemini.AvailableModel) string {
	if m == nil {
		return ""
	}
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}

// triggerQuotaPause activates the global pause for a premium model that fell back
// (reason other_model) or received a limit banner (reason limit_banner). It
// returns true when the pause is active after the call. Auto/Flash never
// trigger it, and nothing happens when GEMINI_PAUSE_ON_FALLBACK is off.
func (s *Server) triggerQuotaPause(ctx context.Context, m *gemini.AvailableModel, servedID, servedName, reason, banner string) bool {
	if m == nil || isAutoOrFlashModel(m) || !s.pauseOnFallbackEnabled() {
		return false
	}
	banner = cleanLimitBanner(banner)
	stale := false
	if ri := reqFrom(ctx); ri != nil {
		ri.inFlightMu.RLock()
		stale = ri.staleTokenAlert != ""
		ri.inFlightMu.RUnlock()
	}
	if !stale && s.client != nil {
		rot := s.client.RotationStatus()
		if rot.TokenAgeSource != "unknown" && rot.TokenAge >= 90*time.Minute {
			stale = true
		}
	}
	var curGen uint64
	if s.client != nil {
		curGen = s.client.CookieGeneration()
	}
	t := &s.quotaPause
	t.mu.Lock()
	first := !t.st.Active
	enriched := false
	if first {
		now := s.currentNow().UTC().Format(time.RFC3339)
		t.st.Active = true
		t.st.TriggeredByModel = m.Name
		t.st.TriggeredByLabel = modelLabel(m)
		t.st.RequestedID = m.ModelID
		t.st.Reason = reason
		t.st.PausedAt = &now
		t.st.StaleToken = stale
		t.cookieGen = curGen
	} else if stale && !t.st.StaleToken {
		t.st.StaleToken = true
		enriched = true
	}
	if servedID != "" && t.st.ServedID == "" {
		t.st.ServedID, enriched = servedID, true
	}
	if servedName != "" && t.st.ServedName == "" {
		t.st.ServedName, enriched = servedName, true
	}
	if banner != "" && t.st.LimitBanner == "" {
		t.st.LimitBanner, enriched = banner, true
	}
	st := t.st
	t.mu.Unlock()

	if first || enriched {
		attrs := []any{"model", m.Name, "requested_id", m.ModelID, "served_id", st.ServedID,
			"served_name", st.ServedName, "reason", reason, "first", first}
		if st.LimitBanner != "" {
			attrs = append(attrs, "banner", st.LimitBanner)
		}
		s.rlog(ctx).Warn("quota_fallback_pause", attrs...)
		detail := "reason=" + reason
		human := "modèle premium « " + modelLabel(m) + " » remplacé"
		if st.ServedName != "" {
			human += " par « " + st.ServedName + " »"
		}
		if st.LimitBanner != "" {
			human += " ; bannière Google : " + st.LimitBanner
		}
		human += " : pause des modèles premium"
		s.recordDiagIncident(ctx, "quota_fallback_pause", detail, human, false)
	}
	return true
}

// quotaPauseMessage is the French explanation shown to clients while paused.
func quotaPauseMessage(st QuotaPauseState) string {
	var b strings.Builder
	b.WriteString("GMINI : les modèles premium (Hydrogen, Neon, Argon/Carbon) sont en pause. ")
	b.WriteString("Le proxy a détecté un repli ou une limite de quota Google")
	if st.TriggeredByLabel != "" {
		b.WriteString(" sur « " + st.TriggeredByLabel + " »")
	}
	if st.ServedName != "" {
		b.WriteString(" (modèle servi : « " + st.ServedName + " »)")
	}
	b.WriteString(". ")
	if st.LimitBanner != "" {
		b.WriteString("Bannière Google : « " + st.LimitBanner + " ». ")
	}
	b.WriteString("Vérifiez vos limites (fenêtre 5 h / hebdomadaire) sur " + quotaUsageURL)
	b.WriteString(", puis cliquez sur « Reprendre » dans GMINI Console-Web-UI (ou via la console TUI). ")
	b.WriteString("Auto / Flash reste utilisable pendant la pause.")
	return b.String()
}

type apiFlavor int

const (
	flavorOpenAI apiFlavor = iota
	flavorAnthropic
	flavorGoogle
)

// writeQuotaPause429 writes the HTTP 429 in the error format of the endpoint.
func writeQuotaPause429(w http.ResponseWriter, flavor apiFlavor, msg string) {
	w.Header().Set("Retry-After", "300")
	switch flavor {
	case flavorAnthropic:
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "rate_limit_error",
				"message": msg,
				"code":    quotaPauseErrorCode,
			},
		})
	case flavorGoogle:
		sendJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"code":    http.StatusTooManyRequests,
				"message": msg,
				"status":  "RESOURCE_EXHAUSTED",
				"details": []map[string]any{{"reason": quotaPauseErrorCode}},
			},
		})
	default:
		sendJSON(w, http.StatusTooManyRequests, errorResponse{Error: errorDetail{
			Message: msg, Type: "rate_limit_error", Code: quotaPauseErrorCode,
		}})
	}
}

// quotaPreflight blocks a non Auto/Flash model while the pause is active, with
// zero call to Google. It returns true when the request was answered with 429.
func (s *Server) quotaPreflight(w http.ResponseWriter, r *http.Request, m *gemini.AvailableModel, flavor apiFlavor) bool {
	if m == nil || isAutoOrFlashModel(m) || !s.pauseOnFallbackEnabled() {
		return false
	}
	t := &s.quotaPause
	t.mu.Lock()
	if !t.st.Active {
		t.mu.Unlock()
		return false
	}
	staleActive := t.st.StaleToken
	pausedGen := t.cookieGen
	t.mu.Unlock()

	if staleActive && s.client != nil {
		rot := s.client.RotationStatus()
		if s.client.CookieGeneration() != pausedGen || (rot.TokenAgeSource != "unknown" && rot.TokenAge == 0) {
			if s.resumeStaleTokenQuotaPause(r.Context(), "cookie_refresh_stale_token") {
				return false
			}
		}
	}

	t.mu.Lock()
	if !t.st.Active {
		t.mu.Unlock()
		return false
	}
	t.st.BlockedCount++
	st := t.st
	t.mu.Unlock()

	ctx := r.Context()
	setReqModel(ctx, m.Name)
	if ri := reqFrom(ctx); ri != nil {
		ri.setTerminalError("Pause quota (429)", "quota_pause_blocked")
		detail := "Pause quota active : bloquée avant envoi à Google"
		if st.ServedName != "" && st.TriggeredByLabel != "" {
			detail = "Pause quota active : bloquée avant envoi à Google (" + st.ServedName + " servi à la place de " + st.TriggeredByLabel + ")"
		}
		ri.setErrorContext("pre-flight/quota", "quotaPreFlight", "quota_pause_blocked", detail)
	}
	s.rlog(ctx).Warn("quota_pause_blocked", "model", m.Name, "blocked_count", st.BlockedCount, "reason", st.Reason)
	s.recordDiagIncident(ctx, "quota_pause_blocked", "blocked_count="+strconv.FormatInt(st.BlockedCount, 10),
		"requête « "+modelLabel(m)+" » bloquée (pause quota active)", false)
	writeQuotaPause429(w, flavor, quotaPauseMessage(st))
	return true
}

// resumeQuotaPause clears the pause and records the resumption.
func (s *Server) resumeQuotaPause(ctx context.Context, source string) QuotaPauseState {
	t := &s.quotaPause
	t.mu.Lock()
	was := t.st
	t.st = QuotaPauseState{}
	t.cookieGen = 0
	t.mu.Unlock()
	s.rlog(ctx).Info("quota_pause_resumed", "source", source, "was_active", was.Active, "blocked_count", was.BlockedCount)
	s.recordDiagIncident(ctx, "quota_pause_resumed", "source="+source, "pause quota levée ("+source+")", true)
	return QuotaPauseState{}
}

// resumeStaleTokenQuotaPause lifts an active pause only if it was triggered on a
// stale token, preventing false-positive HTTP 429s after a cookie refresh.
func (s *Server) resumeStaleTokenQuotaPause(ctx context.Context, source string) bool {
	t := &s.quotaPause
	t.mu.Lock()
	active := t.st.Active && t.st.StaleToken
	t.mu.Unlock()
	if !active {
		return false
	}
	s.resumeQuotaPause(ctx, source)
	return true
}

// quotaAfterGenerate is the safety net for blocking (non-event) generations: it
// inspects the call record of the finished request and triggers the pause when
// Google served another model than the premium one requested.
func (s *Server) quotaAfterGenerate(ctx context.Context, p gemini.GenParams) bool {
	if p.Model == nil || isAutoOrFlashModel(p.Model) || !s.pauseOnFallbackEnabled() {
		return false
	}
	ri := reqFrom(ctx)
	if ri == nil {
		return false
	}
	rec, ok := s.client.CallByRequest(ri.id)
	if !ok {
		return false
	}
	if rec.LimitBanner != "" {
		return s.triggerQuotaPause(ctx, p.Model, rec.ServedID, rec.ServedName, "limit_banner", rec.LimitBanner)
	}
	if servedOtherModel(p.Model, rec.ServedID, rec.ServedName) {
		return s.triggerQuotaPause(ctx, p.Model, rec.ServedID, rec.ServedName, "other_model", "")
	}
	return false
}

// quotaGuard wires Identity / LimitBanner stream handlers for the no-tools
// streaming paths. While nothing was sent to the client, a trigger cancels the
// upstream stream so the caller can answer HTTP 429.
type quotaGuard struct {
	s       *Server
	ctx     context.Context
	p       gemini.GenParams
	started func() bool // true once bytes reached the client
	cancel  context.CancelFunc
	tripped atomic.Bool
}

func (s *Server) newQuotaGuard(ctx context.Context, p gemini.GenParams, started func() bool, cancel context.CancelFunc) *quotaGuard {
	return &quotaGuard{s: s, ctx: ctx, p: p, started: started, cancel: cancel}
}

func (g *quotaGuard) fire(servedID, servedName, reason, banner string) {
	if !g.s.triggerQuotaPause(g.ctx, g.p.Model, servedID, servedName, reason, banner) {
		return
	}
	if !g.started() {
		g.tripped.Store(true)
		g.cancel()
	}
}

// wrap returns h with the guard's handlers chained in front of existing ones.
func (g *quotaGuard) wrap(h gemini.StreamHandlers) gemini.StreamHandlers {
	prevID, prevBanner, prevText := h.Identity, h.LimitBanner, h.Text
	if prevText != nil {
		// Once tripped, the rest of the current frame must not reach the client.
		h.Text = func(d string) {
			if !g.isTripped() {
				prevText(d)
			}
		}
	}
	h.Identity = func(servedID, servedName string) {
		if prevID != nil {
			prevID(servedID, servedName)
		}
		if servedOtherModel(g.p.Model, servedID, servedName) {
			g.fire(servedID, servedName, "other_model", "")
		}
	}
	h.LimitBanner = func(msg string) {
		if prevBanner != nil {
			prevBanner(msg)
		}
		if msg != "" {
			g.fire("", "", "limit_banner", msg)
		}
	}
	return h
}

func (g *quotaGuard) isTripped() bool { return g != nil && g.tripped.Load() }

// send429 answers the request with the quota-pause 429.
func (s *Server) sendQuotaPause429(w http.ResponseWriter, flavor apiFlavor) {
	writeQuotaPause429(w, flavor, quotaPauseMessage(s.quotaPauseSnapshot()))
}

// ─── resume endpoints ─────────────────────────────────────────────────────────

func (s *Server) handleUIQuotaResume(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	st := s.resumeQuotaPause(r.Context(), "ui")
	sendJSON(w, http.StatusOK, map[string]any{"resumed": true, "quota_pause": st})
}

func (s *Server) handleModelsResume(w http.ResponseWriter, r *http.Request) {
	st := s.resumeQuotaPause(r.Context(), "api")
	sendJSON(w, http.StatusOK, map[string]any{"resumed": true, "quota_pause": st})
}
