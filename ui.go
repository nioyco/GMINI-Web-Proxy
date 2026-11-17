package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// Local management page (GET /_ui). It is served to loopback clients only and
// shows state that is already content-free: status, catalogue, the journal of
// generations and the Antigravity entries that point to this proxy (names and
// timings only, never the key). The page carries a per-process token that its
// API calls must send back, so another site open in the browser cannot read the
// API. GEMINI_UI=0 turns the page off.

//go:embed ui.html
var uiHTML []byte

func newUIToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func loopbackRemote(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) uiAllowed(w http.ResponseWriter, r *http.Request, needToken bool) bool {
	if !s.cfg.UIEnabled || s.uiToken == "" {
		sendError(w, http.StatusNotFound, "not found")
		return false
	}
	if !loopbackRemote(r) {
		sendError(w, http.StatusForbidden, "the management page is local only")
		return false
	}
	if needToken && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-GMINI-UI")), []byte(s.uiToken)) != 1 {
		sendError(w, http.StatusUnauthorized, "reload the management page")
		return false
	}
	return true
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, false) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	_, _ = w.Write(bytes.Replace(uiHTML, []byte("__UI_TOKEN__"), []byte(s.uiToken), 1))
}

type bufWriter struct {
	h    http.Header
	buf  bytes.Buffer
	code int
}

func (b *bufWriter) Header() http.Header         { return b.h }
func (b *bufWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufWriter) WriteHeader(c int)           { b.code = c }

type uiModel struct {
	Name        string `json:"name"`
	ModelID     string `json:"model_id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	MenuLabel   string `json:"menu_label"`
	Withdrawn   string `json:"withdrawn_since"` // RFC 3339; "" = listed by Google now
	Family      string `json:"family"`
	// Journal figures (successful streamed calls among the recent ones).
	Calls                int    `json:"calls"`
	WithThoughts         int    `json:"with_thoughts"`
	MedianFirstMS        int64  `json:"median_first_text_ms"`    // -1: no data
	MedianFirstSignMS    int64  `json:"median_first_sign_ms"`    // -1: no data (min of first_thought_ms, first_text_ms)
	MedianFirstThoughtM  int64  `json:"median_first_thought_ms"` // -1: no data
	LastTime             string `json:"last_time"`
	LastIdentity         string `json:"last_identity"`
	LastSuspect          string `json:"last_suspect"`
	LastSeenModels       string `json:"last_seen_models"`         // display names of catalogue ids found in the last reply
	LastServed           string `json:"last_served"`              // model name stated by Google in the last reply
	Calls5h              int    `json:"calls_5h"`                 // generations sent to Gemini in the last 5 hours
	CodeRef5h            int    `json:"code_ref_5h"`              // requests with code_reference block in the last 5 hours (K17 S4)
	MedianCodeRefPauseMS int64  `json:"median_code_ref_pause_ms"` // -1: no data (K17 S4)
	MismatchSince        string `json:"mismatch_since,omitempty"`
	FailureCount         int    `json:"failure_count,omitempty"`
	FailureSince         string `json:"failure_since,omitempty"`
	Unresponsive         bool   `json:"unresponsive,omitempty"`
}

type uiAntigravity struct {
	File    string           `json:"file_state"` // ok | absent | unreadable | off
	Entries []uiAntigravityE `json:"entries"`
	Missing []string         `json:"missing"` // catalogue models with no Antigravity entry
}

type uiAntigravityE struct {
	DisplayName string `json:"display_name"`
	Model       string `json:"model"`  // externalModelName (may carry @effort)
	Status      string `json:"status"` // ok | not_in_catalogue
	TimeoutMS   int64  `json:"timeout_ms"`
	RelayMS     int64  `json:"relay_keepalive_ms"`
}

const recentCallsHistoryWindow = 12 * time.Hour

func filterCallsLastHours(calls []gemini.CallRecord, window time.Duration, now time.Time) []gemini.CallRecord {
	if window <= 0 {
		return calls
	}
	cutoff := now.Add(-window)
	out := make([]gemini.CallRecord, 0, len(calls))
	for _, c := range calls {
		if t, err := time.Parse(time.RFC3339, c.Time); err == nil {
			if t.Before(cutoff) {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) handleUIState(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	st := &bufWriter{h: http.Header{}}
	s.handleStatus(st, r)
	calls := filterCallsLastHours(s.client.RecentCalls(), recentCallsHistoryWindow, s.currentNow())
	models := s.uiModels()
	sendJSON(w, http.StatusOK, map[string]any{
		"status":            json.RawMessage(st.buf.Bytes()),
		"models":            models,
		"calls":             calls,
		"in_flight":         s.inFlightRequests(),
		"in_flight_history": s.flightHist.list(),
		"antigravity":       s.uiAntigravity(models),
		"apps":              listAntigravity(),
		"listen":            s.cfg.Listen,
	})
}

func (s *Server) handleUIRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.handleModelsRefresh(w, r)
}

func (s *Server) handleUICookieRefreshToggle(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	if s.autoRefresh == nil {
		sendError(w, http.StatusServiceUnavailable, "cookie auto-refresh unavailable")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	st := s.autoRefresh.ToggleFrom(req.Enabled, "ui")
	sendJSON(w, http.StatusOK, st)
}

func (s *Server) handleUICookieRefreshRun(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	if s.autoRefresh == nil {
		sendError(w, http.StatusServiceUnavailable, "cookie auto-refresh unavailable")
		return
	}
	inFlight := len(s.inFlightRequests())
	st, code, reason := s.autoRefresh.RunManual(inFlight)
	if reason != "" {
		msg := reason
		switch reason {
		case "not_configured":
			msg = "commande non réglée — ajouter GEMINI_COOKIE_REFRESH_CMD dans .env"
		case "already_running":
			msg = "actualisation déjà en cours"
		case "google_blocked":
			msg = "refus : un blocage Google est actif"
		case "requests_in_flight":
			msg = "refus : une demande est en cours"
		case "cooldown":
			msg = "refus : actualisation récente — attendre encore"
		}
		sendJSON(w, code, map[string]any{
			"started":      false,
			"reason":       reason,
			"error":        msg,
			"cooldown_sec": st.ManualCooldownSec,
			"status":       st,
		})
		return
	}
	sendJSON(w, code, map[string]any{
		"started": true,
		"status":  st,
	})
}

func (s *Server) uiModels() []uiModel {
	all := s.client.RecentCalls() // newest first
	byID := map[string]string{}
	list := s.client.ListModels()
	for _, m := range list {
		if m.ModelID != "" {
			byID[m.ModelID] = m.Description
			if byID[m.ModelID] == "" {
				byID[m.ModelID] = m.DisplayName
			}
		}
	}
	def := s.client.DefaultModelName()
	identByModel := map[string]string{}
	for _, mi := range s.client.ModelIdentities() {
		if !mi.MismatchSince.IsZero() {
			identByModel[mi.Model] = mi.MismatchSince.UTC().Format(time.RFC3339)
		}
	}
	failByModel := map[string]modelFailureSummary{}
	for _, mf := range s.modelFailures() {
		failByModel[mf.Model] = mf
	}
	codeRefByModel := s.codeRefStats5h()
	out := make([]uiModel, 0, len(list))
	for _, m := range list {
		mf := failByModel[m.Name]
		var failSince string
		if !mf.Since.IsZero() {
			failSince = mf.Since.UTC().Format(time.RFC3339)
		}
		crList := codeRefByModel[m.Name]
		var crPauses []int64
		for _, p := range crList {
			if p >= 0 {
				crPauses = append(crPauses, p)
			}
		}
		crMedian := int64(-1)
		if len(crPauses) > 0 {
			sort.Slice(crPauses, func(i, j int) bool { return crPauses[i] < crPauses[j] })
			crMedian = crPauses[len(crPauses)/2]
		}
		u := uiModel{Name: m.Name, ModelID: m.ModelID, DisplayName: m.DisplayName, Description: m.Description,
			Default: m.Name == def, MedianFirstMS: -1, MedianFirstSignMS: -1, MedianFirstThoughtM: -1, MenuLabel: m.MenuLabel, Family: m.Family,
			Calls5h: 0, CodeRef5h: len(crList), MedianCodeRefPauseMS: crMedian,
			MismatchSince: identByModel[m.Name], FailureCount: mf.Count, FailureSince: failSince, Unresponsive: mf.Unresponsive}
		var firsts, firstSigns, firstThoughts []int64
		for _, c := range all {
			if c.Model != m.Name {
				continue
			}
			if t, err := time.Parse(time.RFC3339, c.Time); err == nil && time.Since(t) < 5*time.Hour {
				u.Calls5h++
			}
			if u.LastTime == "" {
				u.LastTime, u.LastIdentity, u.LastSuspect = c.Time, c.Identity, c.Suspect
				u.LastServed = c.ServedName
				if u.LastServed == "" && c.ServedID != "" {
					u.LastServed = byID[c.ServedID]
				}
				names := []string{}
				for _, id := range c.SeenModelIDs {
					names = append(names, byID[id])
				}
				u.LastSeenModels = strings.Join(names, ", ")
			}
			if c.Error != "" || c.Identity == "canned_error" || !c.Stream || len(firsts) >= 20 {
				continue
			}
			u.Calls++
			if c.ThoughtBytes > 0 {
				u.WithThoughts++
			}
			if c.FirstTextMS >= 0 {
				firsts = append(firsts, c.FirstTextMS)
			}
			if c.FirstThoughtMS >= 0 {
				firstThoughts = append(firstThoughts, c.FirstThoughtMS)
			}
			sign := c.FirstTextMS
			if c.FirstThoughtMS >= 0 && (sign < 0 || c.FirstThoughtMS < sign) {
				sign = c.FirstThoughtMS
			}
			if sign >= 0 {
				firstSigns = append(firstSigns, sign)
			}
		}
		if len(firsts) > 0 {
			sort.Slice(firsts, func(i, j int) bool { return firsts[i] < firsts[j] })
			u.MedianFirstMS = firsts[len(firsts)/2]
		}
		if len(firstSigns) > 0 {
			sort.Slice(firstSigns, func(i, j int) bool { return firstSigns[i] < firstSigns[j] })
			u.MedianFirstSignMS = firstSigns[len(firstSigns)/2]
		}
		if len(firstThoughts) > 0 {
			sort.Slice(firstThoughts, func(i, j int) bool { return firstThoughts[i] < firstThoughts[j] })
			u.MedianFirstThoughtM = firstThoughts[len(firstThoughts)/2]
		}
		out = append(out, u)
	}
	for _, wd := range s.client.WithdrawnModels() {
		out = append(out, uiModel{Name: wd.Model.Name, ModelID: wd.Model.ModelID, DisplayName: wd.Model.DisplayName,
			Description: wd.Model.Description, MenuLabel: wd.Model.MenuLabel, Family: wd.Model.Family,
			MedianFirstMS: -1, MedianFirstSignMS: -1, MedianFirstThoughtM: -1, Withdrawn: wd.Since.Format(time.RFC3339)})
	}
	return out
}

// antigravityModelsFile returns the custom models file of Antigravity ("" = off).
func (s *Server) antigravityModelsFile() string {
	v := strings.TrimSpace(s.cfg.AntigravityModelsFile)
	if strings.EqualFold(v, "off") {
		return ""
	}
	if v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity", "custom_models.json")
}

// uiAntigravity reads the Antigravity entries that point to this proxy. Only
// display names, model names and two timings are decoded: the key field is
// never read into memory as a typed value nor returned.
func (s *Server) uiAntigravity(models []uiModel) uiAntigravity {
	out := uiAntigravity{File: "off", Entries: []uiAntigravityE{}, Missing: []string{}}
	path := s.antigravityModelsFile()
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		out.File = "absent"
		return out
	}
	type entry struct {
		Name        string  `json:"name"`
		DisplayName string  `json:"displayName"`
		External    string  `json:"externalModelName"`
		APIURL      string  `json:"apiUrl"`
		Timeout     float64 `json:"timeout"`
		Relay       float64 `json:"relayKeepaliveMs"`
	}
	var doc struct {
		Models json.RawMessage `json:"models"`
	}
	var entries []entry
	if json.Unmarshal(data, &doc) != nil {
		out.File = "unreadable"
		return out
	}
	if json.Unmarshal(doc.Models, &entries) != nil {
		var m map[string]entry
		if json.Unmarshal(doc.Models, &m) != nil {
			out.File = "unreadable"
			return out
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			entries = append(entries, m[k])
		}
	}
	out.File = "ok"
	_, port, _ := net.SplitHostPort(s.cfg.Listen)
	catalogue := map[string]bool{}
	for _, m := range models {
		if m.Withdrawn == "" {
			catalogue[m.Name] = true
		}
	}
	have := map[string]bool{}
	for _, e := range entries {
		if port == "" || !(strings.Contains(e.APIURL, "127.0.0.1:"+port) || strings.Contains(e.APIURL, "localhost:"+port)) {
			continue
		}
		model := e.External
		if model == "" {
			model = e.Name[strings.LastIndex(e.Name, "/")+1:]
		}
		base, _, _ := strings.Cut(model, "@")
		row := uiAntigravityE{DisplayName: e.DisplayName, Model: model, Status: "ok",
			TimeoutMS: int64(e.Timeout), RelayMS: int64(e.Relay)}
		if !catalogue[base] {
			row.Status = "not_in_catalogue"
		}
		have[base] = true
		out.Entries = append(out.Entries, row)
	}
	for _, m := range models {
		if !have[m.Name] && m.Withdrawn == "" {
			out.Missing = append(out.Missing, m.Name)
		}
	}
	return out
}

func (s *Server) envModifiedSinceStart() bool {
	path := s.cfg.EnvFilePath
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !s.cfg.EnvFileModTime.IsZero() {
		return info.ModTime().After(s.cfg.EnvFileModTime)
	}
	if !s.cfg.ConfigLoadedAt.IsZero() {
		return info.ModTime().After(s.cfg.ConfigLoadedAt)
	}
	return false
}

func (s *Server) handleUIConfigReload(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	reloadedAt, err := reloadNonSecretDotenv(s.cfg.EnvFilePath, s.cfg)
	if err != nil {
		sendError(w, http.StatusBadRequest, "impossible de lire le fichier .env")
		return
	}
	apiconv.SetNoInternalCodeExecRule(s.cfg.NoInternalCodeExecRule)
	s.log.Info("config_reloaded_from_ui", "loaded_at", reloadedAt.UTC().Format(time.RFC3339))
	sendJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"reloaded_at": reloadedAt.UTC().Format(time.RFC3339),
	})
}
