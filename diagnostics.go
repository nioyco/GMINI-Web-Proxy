package main

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

const (
	// diagRingCapacity bounds the ring in memory and on disk. 1000 anonymized
	// records is small (each is only timestamps, model names and counters) and
	// keeps the last ~1000 events across a restart.
	diagRingCapacity  = 1000
	diagDefaultLimit  = 200
	diagMaxLimit      = 500
	diagExportTimeout = 30 * time.Second
)

// DiagEvent is a single anonymized activity or incident record kept in memory.
// It never stores secrets, cookies, API keys, or prompt/response content.
type DiagEvent struct {
	TS          string `json:"ts"`
	Kind        string `json:"kind"` // "activity" | "incident"
	BinaryTag   string `json:"binary_tag,omitempty"`
	RequestID   string `json:"request_id,omitempty"`
	Model       string `json:"model,omitempty"`
	ServedModel string `json:"served_model,omitempty"`

	// Activity fields
	ToolCalls    int      `json:"tool_calls,omitempty"`
	ToolNames    []string `json:"tool_names,omitempty"`
	InternalTool string   `json:"internal_tool,omitempty"`
	TTFTMS       int64    `json:"ttft_ms,omitempty"`
	DurationMS   int64    `json:"duration_ms,omitempty"`
	PromptBytes  int      `json:"prompt_bytes,omitempty"`
	Status       int      `json:"status,omitempty"`
	Outcome      string   `json:"outcome,omitempty"`

	// Incident fields
	Type      string `json:"type,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Recovered bool   `json:"recovered,omitempty"`

	// K47: structured error provenance (metadata only).
	Route   string `json:"route,omitempty"`
	Service string `json:"service,omitempty"`
	Func    string `json:"func,omitempty"`
}

// DiagSummary aggregates counts over the selected time window.
type DiagSummary struct {
	ActivityCount  int `json:"activity_count"`
	IncidentCount  int `json:"incident_count"`
	OKCount        int `json:"ok_count"`
	ErrorCount     int `json:"error_count"`
	ToolCallsTotal int `json:"tool_calls_total"`
}

type diagQuery struct {
	Kind   string // "all" | "activity" | "incident"
	Status string // optional: "200", "500", "2xx", "4xx", "5xx", "ok", "error", or outcome
	Type   string // optional incident type
	Window string // "15m" | "1h" | "6h" | "24h" | "all"
	Limit  int
	Offset int
}

type diagResponse struct {
	Items   []DiagEvent `json:"items"`
	Total   int         `json:"total"`
	HasMore bool        `json:"has_more"`
	Summary DiagSummary `json:"summary"`
}

// diagRing is a thread-safe bounded ring buffer of DiagEvent. When path is
// non-empty, the ring is persisted atomically (0600) and reloaded on startup so
// past events survive a proxy restart.
type diagRing struct {
	mu      sync.RWMutex
	cap     int
	path    string
	entries []DiagEvent
}

func newDiagRing(capacity int, path string) *diagRing {
	if capacity <= 0 {
		capacity = diagRingCapacity
	}
	return &diagRing{
		cap:     capacity,
		path:    path,
		entries: make([]DiagEvent, 0, capacity),
	}
}

func sanitizeDiagDetail(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// sanitizeToolNames trims tool identifiers, drops empty ones, and bounds the
// slice so a single event never stores unbounded data. Tool names are schema
// identifiers (read_file, run_command, …), never user content.
func sanitizeToolNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if len(n) > 64 {
			n = n[:64]
		}
		out = append(out, n)
		if len(out) >= 16 {
			break
		}
	}
	return out
}

func (r *diagRing) record(ev DiagEvent) {
	if r == nil {
		return
	}
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if ev.Detail != "" {
		ev.Detail = sanitizeDiagDetail(ev.Detail)
	}
	if ev.Reason != "" {
		ev.Reason = sanitizeDiagDetail(ev.Reason)
	}
	ev.ToolNames = sanitizeToolNames(ev.ToolNames)
	r.mu.Lock()
	if len(r.entries) >= r.cap {
		copy(r.entries, r.entries[1:])
		r.entries[len(r.entries)-1] = ev
	} else {
		r.entries = append(r.entries, ev)
	}
	r.saveLocked()
	r.mu.Unlock()
}

// load reads the persisted diagnostics ring (if any) and bounds it to cap.
// Missing or corrupt files are treated as an empty ring.
func (r *diagRing) load() {
	if r == nil || r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return
	}
	var loaded []DiagEvent
	if json.Unmarshal(raw, &loaded) != nil {
		return
	}
	if len(loaded) > r.cap {
		loaded = loaded[len(loaded)-r.cap:]
	}
	r.mu.Lock()
	r.entries = loaded
	r.mu.Unlock()
}

// saveLocked writes the current entries to path atomically with 0600 perms.
// The caller must hold r.mu.
func (r *diagRing) saveLocked() {
	if r == nil || r.path == "" {
		return
	}
	data, err := json.Marshal(r.entries)
	if err != nil {
		return
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(r.path)+".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		cleanup()
	}
}

// seed backfills the ring from the persisted calls journal (newest-first input)
// but only when the ring is still empty, so it acts once on the very first start
// after an upgrade that had no persisted diagnostics. It bounds to cap and persists.
func (r *diagRing) seed(calls []gemini.CallRecord) {
	if r == nil || len(calls) == 0 {
		return
	}
	r.mu.Lock()
	if len(r.entries) != 0 {
		r.mu.Unlock()
		return
	}
	events := diagEventsFromCalls(calls)
	if len(events) > r.cap {
		events = events[len(events)-r.cap:]
	}
	r.entries = events
	r.saveLocked()
	r.mu.Unlock()
}

// diagEventsFromCalls converts persisted call records (newest-first) into an
// oldest-first slice of anonymized activity events, mirroring the live records
// written by recordDiagActivity. Tool call counts are unknown here (the journal
// does not store them), so they are left at zero.
func diagEventsFromCalls(calls []gemini.CallRecord) []DiagEvent {
	out := make([]DiagEvent, 0, len(calls))
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		ev := DiagEvent{
			TS:          c.Time,
			Kind:        "activity",
			BinaryTag:   c.BinaryTag,
			RequestID:   c.RequestID,
			Model:       c.Model,
			ServedModel: c.ServedName,
			TTFTMS:      c.FirstTextMS,
			DurationMS:  c.DurationMS,
			PromptBytes: c.PromptBytes,
			Status:      200,
			Outcome:     "text_only",
		}
		if ev.TTFTMS < 0 {
			ev.TTFTMS = c.FirstThoughtMS
		}
		if c.Error != "" {
			ev.Status = 500
			ev.Outcome = "error"
			ev.Reason = "erreur amont: " + c.Error
		}
		out = append(out, ev)
	}
	return out
}

func parseDiagWindow(w string) (time.Duration, bool) {
	switch strings.ToLower(strings.TrimSpace(w)) {
	case "", "all":
		return 0, true
	case "15m":
		return 15 * time.Minute, true
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "24h":
		return 24 * time.Hour, true
	default:
		return 0, false
	}
}

func matchDiagStatus(ev DiagEvent, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(strings.TrimSpace(filter))
	switch f {
	case "2xx":
		return ev.Status >= 200 && ev.Status < 300
	case "4xx":
		return ev.Status >= 400 && ev.Status < 500
	case "5xx":
		return ev.Status >= 500
	case "ok":
		if ev.Kind == "activity" {
			return ev.Status >= 200 && ev.Status < 400 && ev.Outcome != "error" && ev.Outcome != "timeout" && ev.Outcome != "cancelled"
		}
		return ev.Recovered
	case "error":
		if ev.Kind == "activity" {
			return ev.Status >= 400 || ev.Outcome == "error" || ev.Outcome == "timeout"
		}
		return !ev.Recovered
	}
	if code, err := strconv.Atoi(f); err == nil {
		return ev.Status == code
	}
	return strings.EqualFold(ev.Outcome, f)
}

func (r *diagRing) query(q diagQuery, now time.Time) diagResponse {
	resp := diagResponse{Items: []DiagEvent{}}
	if r == nil {
		return resp
	}
	kind := strings.ToLower(strings.TrimSpace(q.Kind))
	if kind == "" {
		kind = "all"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = diagDefaultLimit
	}
	if limit > diagMaxLimit {
		limit = diagMaxLimit
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	wDur, _ := parseDiagWindow(q.Window)
	var cutoff time.Time
	if wDur > 0 {
		cutoff = now.Add(-wDur)
	}

	r.mu.RLock()
	snap := make([]DiagEvent, len(r.entries))
	copy(snap, r.entries)
	r.mu.RUnlock()

	filtered := make([]DiagEvent, 0, len(snap))
	// Iterate newest-first.
	for i := len(snap) - 1; i >= 0; i-- {
		ev := snap[i]
		if !cutoff.IsZero() {
			if t, err := time.Parse(time.RFC3339, ev.TS); err == nil && t.Before(cutoff) {
				continue
			}
		}
		// Window-level summary counts.
		if ev.Kind == "activity" {
			resp.Summary.ActivityCount++
			resp.Summary.ToolCallsTotal += ev.ToolCalls
			if ev.Status >= 400 || ev.Outcome == "error" || ev.Outcome == "timeout" {
				resp.Summary.ErrorCount++
			} else if ev.Outcome != "cancelled" {
				resp.Summary.OKCount++
			}
		} else if ev.Kind == "incident" {
			resp.Summary.IncidentCount++
		}

		if kind != "all" && ev.Kind != kind {
			continue
		}
		if q.Type != "" && !strings.EqualFold(ev.Type, q.Type) {
			continue
		}
		if !matchDiagStatus(ev, q.Status) {
			continue
		}
		filtered = append(filtered, ev)
	}

	resp.Total = len(filtered)
	if offset >= len(filtered) {
		return resp
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	resp.Items = filtered[offset:end]
	resp.HasMore = end < resp.Total
	return resp
}

func cleanInternalToolLabel(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	parts := strings.Fields(s)
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		pl := strings.ToLower(p)
		if strings.HasPrefix(pl, "http://") || strings.HasPrefix(pl, "https://") {
			continue
		}
		kept = append(kept, p)
	}
	cleaned := strings.TrimSpace(strings.Join(kept, " "))
	if cleaned == "" {
		return "outil interne Google"
	}
	low := strings.ToLower(cleaned)
	switch {
	case strings.Contains(low, "searching the web") || strings.Contains(low, "google_search"):
		if strings.Contains(low, "searching the web") {
			return "Google Search — Searching the web"
		}
		return "Google Search"
	case strings.Contains(low, "drive_mcp"):
		return "Google Drive (drive_mcp)"
	default:
		return cleaned
	}
}

func (s *Server) recordDiagActivity(ev DiagEvent) {
	if s == nil || s.diag == nil {
		return
	}
	ev.Kind = "activity"
	if ev.BinaryTag == "" {
		ev.BinaryTag = currentBinaryTag()
	}
	s.diag.record(ev)
}

// activityReason returns a short human-readable cause for an activity outcome,
// used by the diagnostics ring (and, transitively, the UI "Raison" column).
func activityReason(outcome string, status int) string {
	switch outcome {
	case "cancelled":
		return "annulée (arrêt proxy ou client)"
	case "error":
		return fmt.Sprintf("HTTP %d", status)
	default:
		return ""
	}
}

func (s *Server) recordDiagIncident(ctx context.Context, incType, detail, reason string, recovered bool) {
	if s == nil || s.diag == nil {
		return
	}
	ev := DiagEvent{
		Kind:      "incident",
		BinaryTag: currentBinaryTag(),
		Type:      incType,
		Detail:    detail,
		Reason:    reason,
		Recovered: recovered,
	}
	if ri := reqFrom(ctx); ri != nil {
		ev.RequestID = ri.id
		ev.Model = ri.model
		if ri.method != "" || ri.route != "" {
			ev.Route = strings.TrimSpace(ri.method + " " + ri.route)
		}
		ri.inFlightMu.RLock()
		ev.ServedModel = ri.servedName
		ev.Service = ri.errorService
		ev.Func = ri.errorFunc
		ev.InternalTool = cleanInternalToolLabel(ri.internalToolName)
		ri.inFlightMu.RUnlock()
	}
	s.diag.record(ev)
}

func (s *Server) handleUIDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	q := r.URL.Query()
	kind := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	if kind == "" {
		kind = "all"
	}
	switch kind {
	case "all", "activity", "incident":
	default:
		sendError(w, http.StatusBadRequest, "invalid kind (expected all, activity, or incident)")
		return
	}
	window := strings.TrimSpace(q.Get("window"))
	if _, ok := parseDiagWindow(window); !ok {
		sendError(w, http.StatusBadRequest, "invalid window (expected 15m, 1h, 6h, 24h, or all)")
		return
	}
	limit := diagDefaultLimit
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			sendError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = v
	}
	offset := 0
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			sendError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = v
	}
	resp := s.diag.query(diagQuery{
		Kind:   kind,
		Status: strings.TrimSpace(q.Get("status")),
		Type:   strings.TrimSpace(q.Get("type")),
		Window: window,
		Limit:  limit,
		Offset: offset,
	}, s.currentNow())
	sendJSON(w, http.StatusOK, resp)
}

var (
	diagSinceRe = regexp.MustCompile(`^(?:\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2})?)?|\d{2}:\d{2})$`)
	diagModelRe = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)
)

func resolveDiagnosticsScript() (string, error) {
	if env := strings.TrimSpace(os.Getenv("GEMINI_DIAGNOSTICS_SCRIPT")); env != "" {
		info, err := os.Stat(env)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("diagnostics script not found")
		}
		return env, nil
	}
	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		candidates = append(candidates, filepath.Join(cwd, "tools", "export_gmini_diagnostics.py"))
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
			exe = resolved
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "tools", "export_gmini_diagnostics.py"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	// Last resort: the script embedded in the binary, so a standalone build
	// (e.g. gmini-builds/go-gemini-web2api-current) never answers 503 for it.
	return materializeEmbeddedDiagnosticsScript()
}

//go:embed tools/export_gmini_diagnostics.py
var embeddedDiagnosticsScript []byte

var (
	embeddedScriptMu   sync.Mutex
	embeddedScriptPath string
)

// materializeEmbeddedDiagnosticsScript writes the embedded script once into a
// private (0700) temporary directory as a 0600 file and reuses it while its
// content is intact. The directory is process-scoped and rewritten if removed.
func materializeEmbeddedDiagnosticsScript() (string, error) {
	embeddedScriptMu.Lock()
	defer embeddedScriptMu.Unlock()
	if len(embeddedDiagnosticsScript) == 0 {
		return "", fmt.Errorf("diagnostics script not found")
	}
	if embeddedScriptPath != "" {
		if data, err := os.ReadFile(embeddedScriptPath); err == nil && bytes.Equal(data, embeddedDiagnosticsScript) {
			return embeddedScriptPath, nil
		}
	}
	dir, err := os.MkdirTemp("", "gmini-diag-script-*")
	if err != nil {
		return "", fmt.Errorf("diagnostics script not found")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("diagnostics script not found")
	}
	path := filepath.Join(dir, "export_gmini_diagnostics.py")
	if err := os.WriteFile(path, embeddedDiagnosticsScript, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("diagnostics script not found")
	}
	embeddedScriptPath = path
	return path, nil
}

func resolvePython3() (string, error) {
	for _, p := range []string{"/opt/homebrew/bin/python3", "/usr/local/bin/python3"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return exec.LookPath("python3")
}

func zipDirectory(dir string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			_ = zw.Close()
			return nil, err
		}
		fw, err := zw.Create(name)
		if err != nil {
			_ = zw.Close()
			return nil, err
		}
		if _, err := fw.Write(data); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Server) handleUIDiagnosticsExport(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	if !s.diagExportBusy.CompareAndSwap(false, true) {
		sendError(w, http.StatusConflict, "un export de diagnostics est déjà en cours")
		return
	}
	defer s.diagExportBusy.Store(false)

	var req struct {
		Format string `json:"format"`
		Window string `json:"window"`
		Since  string `json:"since"`
		Model  string `json:"model"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
		if err != nil {
			sendError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				sendError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
		}
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "all"
	}
	switch format {
	case "json", "csv", "md", "all":
	default:
		sendError(w, http.StatusBadRequest, "invalid format (expected json, csv, md, or all)")
		return
	}

	window := strings.ToLower(strings.TrimSpace(req.Window))
	if window == "" {
		window = "all"
	}
	wDur, ok := parseDiagWindow(window)
	if !ok {
		sendError(w, http.StatusBadRequest, "invalid window (expected 15m, 1h, 6h, 24h, or all)")
		return
	}

	since := strings.TrimSpace(req.Since)
	if since != "" && !diagSinceRe.MatchString(since) {
		sendError(w, http.StatusBadRequest, "invalid since format")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !diagModelRe.MatchString(model) {
		sendError(w, http.StatusBadRequest, "invalid model filter")
		return
	}

	scriptPath, err := resolveDiagnosticsScript()
	if err != nil {
		sendError(w, http.StatusServiceUnavailable, "script d'export de diagnostics introuvable")
		return
	}
	pyPath, err := resolvePython3()
	if err != nil {
		sendError(w, http.StatusServiceUnavailable, "python3 introuvable")
		return
	}

	tmpDir, err := os.MkdirTemp("", "gmini-diag-export-*")
	if err != nil {
		sendError(w, http.StatusInternalServerError, "impossible de créer le dossier temporaire")
		return
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	args := []string{scriptPath, "--out", tmpDir, "--format", format}
	if since != "" {
		args = append(args, "--since", since)
	} else if wDur > 0 {
		args = append(args, "--since", s.currentNow().Add(-wDur).Format("2006-01-02T15:04:05"))
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	ctx, cancel := context.WithTimeout(r.Context(), diagExportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, pyPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			sendError(w, http.StatusGatewayTimeout, "délai dépassé lors de l'export des diagnostics")
			return
		}
		s.log.Warn("diagnostics_export_failed", "error", err.Error())
		sendError(w, http.StatusInternalServerError, "échec de l'export des diagnostics")
		return
	}

	stamp := s.currentNow().UTC().Format("20060102-150405")
	switch format {
	case "md":
		if data, err := os.ReadFile(filepath.Join(tmpDir, "report.md")); err == nil {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gmini-diagnostics-%s.md"`, stamp))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	case "csv":
		if data, err := os.ReadFile(filepath.Join(tmpDir, "requests.csv")); err == nil {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gmini-diagnostics-%s.csv"`, stamp))
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
	}

	zipBytes, err := zipDirectory(tmpDir)
	if err != nil {
		sendError(w, http.StatusInternalServerError, "impossible de créer l'archive zip")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="gmini-diagnostics-%s.zip"`, stamp))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(zipBytes)
}
