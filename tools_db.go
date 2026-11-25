package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const toolsDBMaxEntries = 10000

var binaryTagRe = regexp.MustCompile(`-(k\d+)$`)

func extractBinaryTag(binaryName string) string {
	m := binaryTagRe.FindStringSubmatch(strings.TrimSpace(binaryName))
	if len(m) == 2 {
		return strings.ToUpper(m[1])
	}
	return ""
}

func currentBinaryTag() string {
	_, name := binaryInfo()
	return extractBinaryTag(name)
}

// ToolCallDBRecord stores anonymized metadata for a Gemini tool-call or
// tool-related incident. Never stores secrets, cookies, keys, or prompt/response text.
type ToolCallDBRecord struct {
	TS             string   `json:"ts"`
	BinaryVersion  string   `json:"binary_version"`
	BinaryTag      string   `json:"binary_tag"`
	RequestID      string   `json:"request_id"`
	ConvID         string   `json:"conv_id,omitempty"`
	ModelRequested string   `json:"model_requested"`
	ServedModel    string   `json:"served_model,omitempty"`
	ThinkingLevel  string   `json:"thinking_level,omitempty"`
	PromptBytes    int      `json:"prompt_bytes"`
	ToolKind       string   `json:"tool_kind"` // "local" | "internal_google"
	ToolNames      []string `json:"tool_names"`
	ToolCount      int      `json:"tool_count"`
	ProxyAction    string   `json:"proxy_action"` // "forwarded" | "retry" | "reported" | "blocked" | "early_stop"
	Outcome        string   `json:"outcome"`      // "tool_calls_ok" | "recovered" | "glitch" | "stall" | "tool_refusal" | "timeout" | "error"
	DurationMS     int64    `json:"duration_ms"`
	TTFTMS         int64    `json:"ttft_ms"`
	ResponseBytes  int      `json:"response_bytes"`
	Attempts       int      `json:"attempts"`
	Note           string   `json:"note,omitempty"`
}

type toolsDBVersionStat struct {
	Version        string  `json:"version"`
	Total          int     `json:"total"`
	OK             int     `json:"ok"`
	Fail           int     `json:"fail"`
	SuccessRatePct float64 `json:"success_rate_pct"`
}

type toolsDBToolStat struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Total int    `json:"total"`
	OK    int    `json:"ok"`
	Fail  int    `json:"fail"`
}

type toolsDBSummary struct {
	Total               int                  `json:"total"`
	LocalCount          int                  `json:"local_count"`
	InternalCount       int                  `json:"internal_count"`
	SuccessCount        int                  `json:"success_count"`
	RecoveredCount      int                  `json:"recovered_count"`
	FailureCount        int                  `json:"failure_count"`
	SuccessRatePct      float64              `json:"success_rate_pct"`
	ByVersion           []toolsDBVersionStat `json:"by_version"`
	ByTool              []toolsDBToolStat    `json:"by_tool"`
	TopBlockingInternal []toolsDBToolStat    `json:"top_blocking_internal"`
}

type toolsDBQuery struct {
	Window  string
	Version string
	Kind    string
	Query   string
	Limit   int
}

type toolsDBResponse struct {
	Items   []ToolCallDBRecord `json:"items"`
	Total   int                `json:"total"`
	Summary toolsDBSummary     `json:"summary"`
}

type toolsDB struct {
	mu      sync.RWMutex
	path    string
	max     int
	items   []ToolCallDBRecord
	written int
}

func isTestToolsDBRecord(rec ToolCallDBRecord) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(rec.BinaryVersion)), ".test") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(rec.BinaryTag)), ".test")
}

func defaultToolsDBPath() string {
	if v := strings.TrimSpace(os.Getenv("GEMINI_TOOLS_DB_FILE")); v != "" {
		if strings.EqualFold(v, "off") {
			return ""
		}
		return v
	}
	if testing.Testing() || (len(os.Args) > 0 && strings.HasSuffix(os.Args[0], ".test")) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Logs", "gmini", "gemini_tools_db.jsonl")
}

func newToolsDB(path string, maxEntries int) *toolsDB {
	if maxEntries <= 0 {
		maxEntries = toolsDBMaxEntries
	}
	db := &toolsDB{
		path: path,
		max:  maxEntries,
	}
	db.load()
	return db
}

func (db *toolsDB) load() {
	db.loadWithFilter(!testing.Testing())
}

func (db *toolsDB) loadWithFilter(filterTest bool) {
	if db == nil || db.path == "" {
		return
	}
	f, err := os.Open(db.path)
	if err != nil {
		return
	}
	defer f.Close()

	var loaded []ToolCallDBRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec ToolCallDBRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if filterTest && isTestToolsDBRecord(rec) {
			continue
		}
		loaded = append(loaded, rec)
	}
	if len(loaded) > db.max {
		loaded = loaded[len(loaded)-db.max:]
	}
	db.mu.Lock()
	db.items = loaded
	db.written = len(loaded)
	db.mu.Unlock()
}

func (db *toolsDB) record(rec ToolCallDBRecord) {
	if db == nil {
		return
	}
	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}
	if rec.ToolNames == nil {
		rec.ToolNames = []string{}
	}
	if rec.ToolCount == 0 && len(rec.ToolNames) > 0 {
		rec.ToolCount = len(rec.ToolNames)
	}
	if rec.Note != "" && len(rec.Note) > 200 {
		rec.Note = rec.Note[:200]
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	db.items = append(db.items, rec)
	if len(db.items) > db.max {
		db.items = append([]ToolCallDBRecord(nil), db.items[len(db.items)-db.max:]...)
	}
	if db.path == "" {
		return
	}

	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(db.path), 0700); err != nil {
		return
	}
	f, err := os.OpenFile(db.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
		_ = os.Chmod(db.path, 0600)
		db.written++
	}
	if db.written >= db.max+1000 {
		db.compactLocked()
	}
}

func (db *toolsDB) compactLocked() {
	if db.path == "" {
		return
	}
	tmp := db.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	for _, it := range db.items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(b, '\n'))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, db.path); err == nil {
		_ = os.Chmod(db.path, 0600)
		db.written = len(db.items)
	}
}

func isToolsDBSuccess(outcome string) bool {
	return outcome == "tool_calls_ok" || outcome == "recovered"
}

func (db *toolsDB) query(q toolsDBQuery, now time.Time) toolsDBResponse {
	var resp toolsDBResponse
	resp.Items = []ToolCallDBRecord{}
	resp.Summary.ByVersion = []toolsDBVersionStat{}
	resp.Summary.ByTool = []toolsDBToolStat{}
	resp.Summary.TopBlockingInternal = []toolsDBToolStat{}
	if db == nil {
		return resp
	}

	var cutoff time.Time
	switch strings.ToLower(strings.TrimSpace(q.Window)) {
	case "24h":
		cutoff = now.Add(-24 * time.Hour)
	case "7d":
		cutoff = now.Add(-7 * 24 * time.Hour)
	}
	verFilter := strings.ToUpper(strings.TrimSpace(q.Version))
	kindFilter := strings.ToLower(strings.TrimSpace(q.Kind))
	textFilter := strings.ToLower(strings.TrimSpace(q.Query))
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}

	db.mu.RLock()
	snap := make([]ToolCallDBRecord, len(db.items))
	copy(snap, db.items)
	db.mu.RUnlock()

	verMap := map[string]*toolsDBVersionStat{}
	toolMap := map[string]*toolsDBToolStat{}
	var filtered []ToolCallDBRecord

	for i := len(snap) - 1; i >= 0; i-- {
		it := snap[i]
		if !testing.Testing() && isTestToolsDBRecord(it) {
			continue
		}
		if !cutoff.IsZero() {
			ts, err := time.Parse(time.RFC3339, it.TS)
			if err == nil && ts.Before(cutoff) {
				continue
			}
		}
		if verFilter != "" && verFilter != "ALL" {
			if strings.ToUpper(it.BinaryTag) != verFilter && !strings.Contains(strings.ToUpper(it.BinaryVersion), verFilter) {
				continue
			}
		}
		if kindFilter != "" && kindFilter != "all" && strings.ToLower(it.ToolKind) != kindFilter {
			continue
		}
		if textFilter != "" {
			hay := strings.ToLower(strings.Join([]string{
				it.RequestID, it.ConvID, it.ModelRequested, it.ServedModel,
				it.BinaryTag, it.BinaryVersion, it.ToolKind, it.ProxyAction,
				it.Outcome, it.Note, strings.Join(it.ToolNames, " "),
			}, " "))
			if !strings.Contains(hay, textFilter) {
				continue
			}
		}

		filtered = append(filtered, it)
		resp.Summary.Total++
		if it.ToolKind == "internal_google" {
			resp.Summary.InternalCount++
		} else {
			resp.Summary.LocalCount++
		}
		ok := isToolsDBSuccess(it.Outcome)
		if ok {
			resp.Summary.SuccessCount++
			if it.Outcome == "recovered" {
				resp.Summary.RecoveredCount++
			}
		} else {
			resp.Summary.FailureCount++
		}

		vKey := it.BinaryTag
		if vKey == "" {
			vKey = it.BinaryVersion
		}
		if vKey == "" {
			vKey = "—"
		}
		vs := verMap[vKey]
		if vs == nil {
			vs = &toolsDBVersionStat{Version: vKey}
			verMap[vKey] = vs
		}
		vs.Total++
		if ok {
			vs.OK++
		} else {
			vs.Fail++
		}

		for _, tn := range it.ToolNames {
			tk := it.ToolKind + ":" + tn
			ts := toolMap[tk]
			if ts == nil {
				ts = &toolsDBToolStat{Name: tn, Kind: it.ToolKind}
				toolMap[tk] = ts
			}
			ts.Total++
			if ok {
				ts.OK++
			} else {
				ts.Fail++
			}
		}
	}

	if resp.Summary.Total > 0 {
		resp.Summary.SuccessRatePct = float64(resp.Summary.SuccessCount*1000/resp.Summary.Total) / 10.0
	}
	for _, vs := range verMap {
		if vs.Total > 0 {
			vs.SuccessRatePct = float64(vs.OK*1000/vs.Total) / 10.0
		}
		resp.Summary.ByVersion = append(resp.Summary.ByVersion, *vs)
	}
	sort.Slice(resp.Summary.ByVersion, func(i, j int) bool {
		return resp.Summary.ByVersion[i].Version > resp.Summary.ByVersion[j].Version
	})
	for _, ts := range toolMap {
		resp.Summary.ByTool = append(resp.Summary.ByTool, *ts)
		if ts.Kind == "internal_google" && ts.Fail > 0 {
			resp.Summary.TopBlockingInternal = append(resp.Summary.TopBlockingInternal, *ts)
		}
	}
	sort.Slice(resp.Summary.ByTool, func(i, j int) bool {
		if resp.Summary.ByTool[i].Total != resp.Summary.ByTool[j].Total {
			return resp.Summary.ByTool[i].Total > resp.Summary.ByTool[j].Total
		}
		return resp.Summary.ByTool[i].Name < resp.Summary.ByTool[j].Name
	})
	sort.Slice(resp.Summary.TopBlockingInternal, func(i, j int) bool {
		if resp.Summary.TopBlockingInternal[i].Fail != resp.Summary.TopBlockingInternal[j].Fail {
			return resp.Summary.TopBlockingInternal[i].Fail > resp.Summary.TopBlockingInternal[j].Fail
		}
		return resp.Summary.TopBlockingInternal[i].Name < resp.Summary.TopBlockingInternal[j].Name
	})

	resp.Total = len(filtered)
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if filtered != nil {
		resp.Items = filtered
	}
	return resp
}

func (s *Server) handleUIToolsDB(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	q := r.URL.Query()
	limit := 200
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	res := s.toolsDB.query(toolsDBQuery{
		Window:  q.Get("window"),
		Version: q.Get("version"),
		Kind:    q.Get("kind"),
		Query:   q.Get("q"),
		Limit:   limit,
	}, time.Now())
	sendJSON(w, http.StatusOK, res)
}

func (s *Server) handleUIToolsDBExport(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	q := r.URL.Query()
	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "csv" {
		sendError(w, http.StatusBadRequest, "invalid format (expected json or csv)")
		return
	}
	res := s.toolsDB.query(toolsDBQuery{
		Window:  q.Get("window"),
		Version: q.Get("version"),
		Kind:    q.Get("kind"),
		Query:   q.Get("q"),
		Limit:   toolsDBMaxEntries,
	}, time.Now())

	if format == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="gemini-tools-db.json"`)
		sendJSON(w, http.StatusOK, res)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gemini-tools-db.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"ts", "binary_version", "binary_tag", "request_id", "conv_id",
		"model_requested", "served_model", "thinking_level", "prompt_bytes",
		"tool_kind", "tool_names", "tool_count", "proxy_action", "outcome",
		"duration_ms", "ttft_ms", "response_bytes", "attempts", "note",
	})
	for _, it := range res.Items {
		_ = cw.Write([]string{
			it.TS, it.BinaryVersion, it.BinaryTag, it.RequestID, it.ConvID,
			it.ModelRequested, it.ServedModel, it.ThinkingLevel, strconv.Itoa(it.PromptBytes),
			it.ToolKind, strings.Join(it.ToolNames, ";"), strconv.Itoa(it.ToolCount),
			it.ProxyAction, it.Outcome, strconv.FormatInt(it.DurationMS, 10),
			strconv.FormatInt(it.TTFTMS, 10), strconv.Itoa(it.ResponseBytes),
			strconv.Itoa(it.Attempts), it.Note,
		})
	}
	cw.Flush()
}

func (s *Server) recordToolsDBOnClose(ri *reqInfo, snap InFlightRequest, termReason string, status int) {
	if s == nil || s.toolsDB == nil || ri == nil || ri.model == "" {
		return
	}
	ri.inFlightMu.RLock()
	thinkLvl := ri.thinkingLevel
	internalTool := ri.internalToolName
	ri.inFlightMu.RUnlock()

	hasLocal := len(snap.EmittedTools) > 0
	hasInternal := internalTool != ""
	isToolTerm := termReason == "tool_refusal" || termReason == "tool_refusal_released" || termReason == "glitch" || termReason == "stall"
	if !hasLocal && !hasInternal && !isToolTerm {
		return
	}

	_, binName := binaryInfo()
	binTag := extractBinaryTag(binName)
	attempts := int(ri.met.Attempts.Load())
	if attempts < 1 {
		attempts = 1
	}

	base := ToolCallDBRecord{
		TS:             time.Now().UTC().Format(time.RFC3339),
		BinaryVersion:  binName,
		BinaryTag:      binTag,
		RequestID:      ri.id,
		ConvID:         snap.ConvID,
		ModelRequested: ri.model,
		ServedModel:    snap.ServedName,
		ThinkingLevel:  thinkLvl,
		PromptBytes:    snap.PromptBytes,
		DurationMS:     time.Since(ri.start).Milliseconds(),
		TTFTMS:         ri.met.FirstByteMS.Load(),
		ResponseBytes:  snap.SentBytes,
		Attempts:       attempts,
	}

	if hasInternal {
		rec := base
		rec.ToolKind = "internal_google"
		rec.ToolNames = []string{internalTool}
		rec.ToolCount = 1
		switch {
		case hasLocal:
			rec.ProxyAction = "retry"
			rec.Outcome = "recovered"
			rec.Note = "outil interne détecté ; retry réussi"
		case termReason == "stall":
			rec.ProxyAction = "early_stop"
			rec.Outcome = "stall"
			rec.Note = "outil interne détecté ; stall"
		case termReason == "glitch":
			rec.ProxyAction = "reported"
			rec.Outcome = "glitch"
			rec.Note = "outil interne détecté ; glitch"
		case termReason == "tool_refusal" || termReason == "tool_refusal_released":
			rec.ProxyAction = "reported"
			rec.Outcome = "tool_refusal"
			rec.Note = "outil interne détecté ; refus d'outils"
		case termReason == "no_content_timeout" || termReason == "canned_retry_timeout":
			rec.ProxyAction = "reported"
			rec.Outcome = "timeout"
			rec.Note = "outil interne détecté ; timeout"
		case termReason != "" || status >= 400:
			rec.ProxyAction = "reported"
			rec.Outcome = "error"
			rec.Note = "outil interne détecté ; erreur"
		default:
			rec.ProxyAction = "reported"
			rec.Outcome = "recovered"
			rec.Note = "outil interne détecté ; réponse texte"
		}
		s.toolsDB.record(rec)
	}

	if hasLocal {
		rec := base
		rec.ToolKind = "local"
		rec.ToolNames = append([]string(nil), snap.EmittedTools...)
		rec.ToolCount = len(snap.EmittedTools)
		if attempts > 1 || hasInternal {
			rec.ProxyAction = "retry"
			rec.Outcome = "recovered"
			rec.Note = "appel(s) d'outil local transmis après retry"
		} else {
			rec.ProxyAction = "forwarded"
			rec.Outcome = "tool_calls_ok"
		}
		s.toolsDB.record(rec)
	} else if !hasInternal && isToolTerm {
		rec := base
		rec.ToolKind = "local"
		rec.ToolNames = []string{}
		rec.ToolCount = 0
		switch termReason {
		case "stall":
			rec.ProxyAction = "early_stop"
			rec.Outcome = "stall"
			rec.Note = "blocage en cours d'appel d'outil"
		case "glitch":
			rec.ProxyAction = "reported"
			rec.Outcome = "glitch"
			rec.Note = "message de service Google"
		default:
			rec.ProxyAction = "reported"
			rec.Outcome = "tool_refusal"
			rec.Note = "refus d'outils locaux"
		}
		s.toolsDB.record(rec)
	}
}
