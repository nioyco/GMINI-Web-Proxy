package main

import (
	"fmt"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const uiVersion = "3.0.10"

// statusResponse is the read-only diagnostic payload of GET /_status. It never
// contains secrets: no cookie, key, token or prompt content.
type statusResponse struct {
	Version                string                  `json:"version"`
	UIVersion              string                  `json:"ui_version"`
	PID                    int                     `json:"pid"`
	BinaryPath             string                  `json:"binary_path"`
	BinaryName             string                  `json:"binary_name"`
	Commit                 string                  `json:"commit"`
	Dirty                  bool                    `json:"dirty"`
	GoVersion              string                  `json:"go_version"`
	UptimeSec              int64                   `json:"uptime_sec"`
	Authenticated          bool                    `json:"authenticated"`
	CookieRotationAgeS     *int64                  `json:"cookie_last_rotation_age_sec"` // null: no rotation yet in this process
	ModelsSource           string                  `json:"models_source"`                // dynamic | fallback | static | persisted
	ModelsCount            int                     `json:"models_count"`
	DefaultModel           string                  `json:"default_model"`
	ActiveRequests         int                     `json:"active_requests"` // includes this /_status request
	SSEKeepaliveSec        int                     `json:"sse_keepalive_sec"`
	ShutdownGraceSec       int                     `json:"shutdown_grace_sec"`
	Counters               map[string]int64        `json:"counters"`
	CookieRotation         rotationStatus          `json:"cookie_rotation"`
	CookieAutoRefresh      cookieAutoRefreshStatus `json:"cookie_auto_refresh"`
	GoogleBlock            googleBlock             `json:"google_block"`
	UpstreamParams         gemini.UpstreamParams   `json:"upstream_params"`
	ToolPromptRules        bool                    `json:"tool_prompt_noecho_rules"`   // GEMINI_TOOL_PROMPT_NOECHO_RULES
	NoInternalCodeExecRule bool                    `json:"no_internal_code_exec_rule"` // GEMINI_NO_INTERNAL_CODE_EXEC_RULE
	CodeRefFastpath        bool                    `json:"code_ref_fastpath"`          // GEMINI_CODE_REF_FASTPATH
	ToolAuditNote          bool                    `json:"tool_audit_note"`            // GEMINI_TOOL_AUDIT_NOTE
	SplitReadWriteBatch    bool                    `json:"split_read_write_batch"`     // GEMINI_SPLIT_READ_WRITE_BATCH
	MarkerNeutralized      bool                    `json:"neutralize_history_markers"` // GEMINI_NEUTRALIZE_HISTORY_MARKERS
	StreamProgress         bool                    `json:"stream_tools_progress"`      // GEMINI_STREAM_TOOLS_PROGRESS
	StreamThoughts         bool                    `json:"stream_thoughts"`            // GEMINI_STREAM_THOUGHTS
	ThinkingLevel          string                  `json:"thinking_level_default"`     // GEMINI_THINKING_LEVEL ("" = not sent)
	MaxPromptBytes         int                     `json:"max_prompt_bytes"`           // GEMINI_MAX_PROMPT_BYTES (0 = no limit)
	PromptWarnKB           int                     `json:"prompt_warn_kb"`             // GEMINI_PROMPT_WARN_KB (default 300; 0 = off)
	MaxHeldBytes           int                     `json:"max_held_bytes"`             // GEMINI_MAX_HELD_BYTES (0 = no limit)
	MaxHeldSec             int                     `json:"max_held_sec"`               // GEMINI_MAX_HELD_SEC (0 = no limit)
	HeldStallSec           int                     `json:"held_stall_sec"`             // GEMINI_HELD_STALL_SEC (0 = disabled)
	HeldStallFastSec       int                     `json:"held_stall_fast_sec"`        // GEMINI_HELD_STALL_FAST_SEC (0 = disabled)
	HeldStallRetriesMax    int                     `json:"held_stall_retries_max"`     // GEMINI_HELD_STALL_RETRIES (0..5)
	InternalToolAction     string                  `json:"internal_tool_action"`       // GEMINI_INTERNAL_TOOL_ACTION (notify|retry|off)
	CannedErrorRetry       string                  `json:"canned_error_retry"`         // GEMINI_CANNED_ERROR_RETRY (off|once)
	CannedErrorRetrySec    int                     `json:"canned_error_retry_sec"`     // GEMINI_CANNED_ERROR_RETRY_SEC (default 55)
	NoContentSec           int                     `json:"no_content_sec"`             // GEMINI_NO_CONTENT_SEC (default 0)
	PauseOnFallback        bool                    `json:"pause_on_fallback"`          // GEMINI_PAUSE_ON_FALLBACK (default true; K41)
	QuotaPause             QuotaPauseState         `json:"quota_pause"`                // global premium pause state (K41)
	WaitNoteSec            int                     `json:"wait_note_sec"`              // GEMINI_WAIT_NOTE_SEC (default 15; K18 T2)
	ConfigLoadedAt         string                  `json:"config_loaded_at,omitempty"` // RFC 3339 timestamp of last .env load (K18 T5)
	EnvModifiedSinceStart  bool                    `json:"env_modified_since_start"`   // true when .env mtime > ConfigLoadedAt (K18 T5)
	TraceEnabled           bool                    `json:"content_trace_enabled"`      // GEMINI_TRACE_DIR set (files hold conversation content)
	TemporaryChat          bool                    `json:"temporary_chat"`             // GEMINI_TEMPORARY_CHAT
	IgnoredFields          ignoredFields           `json:"ignored_request_fields"`
	SessionState           sessionState            `json:"session_state"`
	ModelIdentities        []modelIdentity         `json:"model_identities"`
	ModelFailures          []modelFailureStatus    `json:"model_failures"`
	BackgroundCalls        backgroundCallsStatus   `json:"background_calls"`
}

type backgroundCallsStatus struct {
	IdleQuietMin              int            `json:"idle_quiet_min"`
	CookieRotation            bool           `json:"cookie_rotation"`
	CatalogRefreshAfterCookie bool           `json:"catalog_refresh_after_cookie"`
	AuthRecovery              bool           `json:"auth_recovery"`
	Total24h                  int            `json:"total_24h"`
	Refused24h                int            `json:"refused_24h"`
	ByFamily24h               map[string]int `json:"by_family_24h"`
	LastAt                    *string        `json:"last_at"`
	LastFamily                string         `json:"last_family,omitempty"`
	LastReason                string         `json:"last_reason,omitempty"`
	Label                     string         `json:"label"`
}

type modelFailureStatus struct {
	Model               string  `json:"model"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	Since               *string `json:"since,omitempty"`
	Unresponsive        bool    `json:"unresponsive"`
}

// modelIdentity summarizes the latest served-model verdict for one model and
// since when Google has returned a different id for it (empty when confirmed).
type modelIdentity struct {
	Model         string  `json:"model"`
	ModelID       string  `json:"model_id,omitempty"`
	LastVerdict   string  `json:"last_verdict"`
	MismatchSince *string `json:"mismatch_since,omitempty"`
	ServedID      string  `json:"served_id,omitempty"`
	ServedName    string  `json:"served_name,omitempty"`
	LastTime      *string `json:"last_time,omitempty"`
}

// sessionState describes the persisted session state by ages only: never the
// token, never the file path (AUDIT-13 A5).
type sessionState struct {
	Persisted              bool    `json:"persisted"`
	LastSavedAgeSec        *int64  `json:"last_saved_age_sec"`
	TokenAgeSec            *int64  `json:"token_age_sec"`       // XSRF "at" token (value never exposed)
	BuildLabelAge          *int64  `json:"build_label_age_sec"` // null: fallback label
	CatalogAgeSec          *int64  `json:"catalog_age_sec"`     // null: never fetched from Google
	CatalogStale           bool    `json:"catalog_stale"`       // persisted catalog served without a session
	CatalogRefreshCooldown int64   `json:"catalog_refresh_cooldown_sec"`
	CatalogRefreshNextAt   *string `json:"catalog_refresh_next_at,omitempty"`
	SessionDegraded        bool    `json:"session_degraded"` // listModels shrank to <=1 model while >=2 known
	SessionDegradedSince   *string `json:"session_degraded_since,omitempty"`
	SessionStatusLabel     string  `json:"session_status_label,omitempty"`
	AccountStatusCode      int     `json:"account_status_code,omitempty"`
}

// ignoredFields lists request fields received but not used by the proxy.
type ignoredFields struct {
	Note   string           `json:"note"`
	Counts map[string]int64 `json:"counts"`
}

// googleBlock reports the anti-bot block state (no URLs, no query strings). The
// top-level fields are derived from the families (active if any family is).
type googleBlock struct {
	Active       bool                         `json:"active"`
	Since        *string                      `json:"since"`
	LastSeen     *string                      `json:"last_seen"`
	BackoffUntil *string                      `json:"backoff_until"`
	Level        int                          `json:"level"`
	Total        int64                        `json:"total"`
	Families     map[string]googleBlockFamily `json:"families"`
}

// googleBlockFamily is the block state of one family of Google calls (accounts,
// app, batchexecute, generate, other). Trigger = the request that was refused;
// target = where Google sent it. Hosts and paths only.
type googleBlockFamily struct {
	Active       bool    `json:"active"`
	Since        *string `json:"since"`
	LastSeen     *string `json:"last_seen"`
	BackoffUntil *string `json:"backoff_until"`
	Level        int     `json:"level"`
	Total        int64   `json:"total"`
	TriggerHost  string  `json:"last_trigger_host"`
	TriggerPath  string  `json:"last_trigger_path"`
	TargetHost   string  `json:"last_target_host"`
	TargetPath   string  `json:"last_target_path"`
	LastStatus   int     `json:"last_status"`
}

func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

// rotationStatus describes the cookie rotation state (no secrets).
type rotationStatus struct {
	Enabled              bool     `json:"enabled"`
	AuthState            string   `json:"auth_state"` // authenticated | anonymous
	PSIDTSPresent        bool     `json:"psidts_present"`
	LastAttemptAgeSec    *int64   `json:"last_attempt_age_sec"`
	LastSuccessAgeSec    *int64   `json:"last_success_age_sec"`
	LastResult           string   `json:"last_result"`
	Attempts             int64    `json:"attempts"`
	Renewed              int64    `json:"renewed"`
	NotRenewed           int64    `json:"not_renewed"`
	ConsecutiveNoRenewal int64    `json:"consecutive_no_renewal"`
	TokenAgeSec          *int64   `json:"token_age_sec"`
	TokenAgeSource       string   `json:"token_age_source"`
	LastHTTPStatus       int      `json:"last_http_status"`      // RotateCookies HTTP status (0: none)
	LastSetCookieNames   []string `json:"last_set_cookie_names"` // names only, never values
	LastRedirected       bool     `json:"last_redirected"`
	Suspended            bool     `json:"suspended"`
	SuspendAfter         int      `json:"suspend_after"`
	ConsecutiveNoNewTok  int64    `json:"consecutive_no_new_token"`
}

func ageSec(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	v := int64(time.Since(t).Seconds())
	return &v
}

// buildInfo extracts the VCS revision stamped by `go build`.
func buildInfo() (commit string, dirty bool, goVersion string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false, ""
	}
	commit = "unknown"
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			commit = kv.Value
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	return commit, dirty, bi.GoVersion
}

// binaryInfo returns the resolved executable path and its base name.
func binaryInfo() (path, name string) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "", ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}
	return exe, filepath.Base(exe)
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	commit, dirty, goVer := buildInfo()
	binPath, binName := binaryInfo()
	cfg := s.cfgSnapshot()
	resp := statusResponse{
		Version:             version,
		UIVersion:           uiVersion,
		PID:                 os.Getpid(),
		BinaryPath:          binPath,
		BinaryName:          binName,
		Commit:              commit,
		Dirty:               dirty,
		GoVersion:           goVer,
		UptimeSec:           int64(time.Since(s.started).Seconds()),
		Authenticated:       s.client.Authenticated(),
		ModelsSource:        s.client.ModelsSourceLabel(),
		ModelsCount:         len(s.client.ModelNames()),
		DefaultModel:        s.client.DefaultModelName(),
		ActiveRequests:      s.active.count(),
		SSEKeepaliveSec:     cfg.SSEKeepaliveSec,
		ShutdownGraceSec:    cfg.ShutdownGraceSec,
		InternalToolAction:  normalizeInternalToolAction(cfg.InternalToolAction),
		CannedErrorRetry:    normalizeCannedErrorRetry(cfg.CannedErrorRetry),
		CannedErrorRetrySec: positive(cfg.CannedErrorRetrySec, 55),
		NoContentSec:        nonNegative(cfg.NoContentSec, 0),
		PauseOnFallback:     cfg.PauseOnFallback,
		QuotaPause:          s.quotaPauseSnapshot(),
		Counters: map[string]int64{
			"requests_total":            s.stats.requests.Load(),
			"responses_2xx":             s.stats.status2xx.Load(),
			"responses_4xx":             s.stats.status4xx.Load(),
			"responses_5xx":             s.stats.status5xx.Load(),
			"keepalives_sent":           s.stats.keepalives.Load(),
			"aborted_on_shutdown":       s.stats.abortedOnShutdown.Load(),
			"echo_stripped":             s.stats.echoStripped.Load(),
			"echo_fabricated":           s.stats.echoFabricated.Load(),
			"upstream_refusal":          s.stats.upstreamRefusal.Load(),
			"tool_call_unparsed":        s.stats.toolCallUnparsed.Load(),
			"tool_call_unconverted":     s.stats.toolCallUnconverted.Load(),
			"tool_name_inferred":        s.stats.toolNameInferred.Load(),
			"internal_tool_detected":    s.stats.internalToolDetected.Load(),
			"code_ref_fastpath":         s.stats.codeRefFastpath.Load(),
			"limit_banner_detected":     s.stats.limitBannerDetected.Load(),
			"canned_error_retries":      s.stats.cannedErrorRetries.Load(),
			"upstream_empty_reply":      s.stats.upstreamEmptyReply.Load(),
			"early_stop":                s.stats.earlyStop.Load(),
			"early_stop_marker":         s.stats.earlyStopMarker.Load(),
			"early_stop_size":           s.stats.earlyStopSize.Load(),
			"early_stop_duration":       s.stats.earlyStopDuration.Load(),
			"early_stop_stall":          s.stats.earlyStopStall.Load(),
			"held_stall_retries":        s.stats.heldStallRetries.Load(),
			"unverified_action_claim":   s.stats.unverifiedClaim.Load(),
			"read_write_batch_split":    s.stats.readWriteBatchSplit.Load(),
			"upstream_requests":         s.stats.upstreamRequests.Load(),
			"upstream_attempts":         s.stats.upstreamAttempts.Load(),
			"upstream_retries":          s.stats.upstreamRetries.Load(),
			"upstream_ms_total":         s.stats.upstreamMS.Load(),
			"first_byte_ms_total":       s.stats.firstByteMS.Load(),
			"upstream_req_bytes_total":  s.stats.upstreamReqBytes.Load(),
			"upstream_resp_bytes_total": s.stats.upstreamRespBytes.Load(),
		},
	}
	if t := s.client.LastCookieRotation(); !t.IsZero() {
		age := int64(time.Since(t).Seconds())
		resp.CookieRotationAgeS = &age
	}
	rs := s.client.RotationStatus()
	resp.CookieRotation = rotationStatus{
		Enabled:   rs.Enabled,
		AuthState: "anonymous", PSIDTSPresent: rs.PSIDTSPresent,
		LastAttemptAgeSec: ageSec(rs.LastAttempt), LastSuccessAgeSec: ageSec(rs.LastSuccess),
		LastResult: rs.LastResult, Attempts: rs.Attempts, Renewed: rs.Renewed, NotRenewed: rs.NotRenewed,
		ConsecutiveNoRenewal: rs.ConsecutiveFailure, TokenAgeSource: rs.TokenAgeSource,
		LastHTTPStatus: rs.LastHTTPStatus, LastSetCookieNames: rs.LastSetCookieNames, LastRedirected: rs.LastRedirected,
		Suspended: rs.Suspended, SuspendAfter: rs.SuspendAfter, ConsecutiveNoNewTok: rs.NoNewTokenStreak,
	}
	if resp.CookieRotation.LastSetCookieNames == nil {
		resp.CookieRotation.LastSetCookieNames = []string{}
	}
	if resp.Authenticated {
		resp.CookieRotation.AuthState = "authenticated"
	}
	if rs.TokenAgeSource != "unknown" {
		a := int64(rs.TokenAge.Seconds())
		resp.CookieRotation.TokenAgeSec = &a
	}
	if s.autoRefresh != nil {
		resp.CookieAutoRefresh = s.autoRefresh.Status()
	}
	bs := s.client.GoogleBlockStatus()
	resp.GoogleBlock = googleBlock{Active: bs.Active, Since: rfc3339Ptr(bs.Since), LastSeen: rfc3339Ptr(bs.LastSeen),
		BackoffUntil: rfc3339Ptr(bs.BackoffUntil), Level: bs.Level, Total: bs.Total,
		Families: map[string]googleBlockFamily{}}
	for f, fs := range bs.Families {
		resp.GoogleBlock.Families[string(f)] = googleBlockFamily{Active: fs.Active, Since: rfc3339Ptr(fs.Since),
			LastSeen: rfc3339Ptr(fs.LastSeen), BackoffUntil: rfc3339Ptr(fs.BackoffUntil), Level: fs.Level, Total: fs.Total,
			TriggerHost: fs.LastTriggerHost, TriggerPath: fs.LastTriggerPath, TargetHost: fs.LastTargetHost,
			TargetPath: fs.LastTargetPath, LastStatus: fs.LastStatus}
	}
	resp.Counters["google_blocks"] = bs.Total
	ss := s.client.SessionState()
	ch := s.client.CatalogHealth()
	resp.SessionState = sessionState{
		Persisted: ss.Persisted, LastSavedAgeSec: ageSec(ss.LastSaved),
		TokenAgeSec: ageSec(ss.TokenAt), BuildLabelAge: ageSec(ss.BuildLabelAt),
		CatalogAgeSec: ageSec(ss.CatalogAt), CatalogStale: ss.CatalogStale,
		SessionDegraded:      ch.SessionDegraded,
		SessionDegradedSince: rfc3339Ptr(ch.Since),
		AccountStatusCode:    ch.AccountStatus,
	}
	if ch.SessionDegraded {
		resp.SessionState.SessionStatusLabel = "session affaiblie : actualiser le cookie"
	}
	if next := s.client.CatalogRefreshNextAt(); !next.IsZero() {
		if rem := int64(time.Until(next).Seconds() + 0.999); rem > 0 {
			resp.SessionState.CatalogRefreshCooldown = rem
			resp.SessionState.CatalogRefreshNextAt = rfc3339Ptr(next)
		}
	}
	ids := s.client.ModelIdentities()
	resp.ModelIdentities = make([]modelIdentity, 0, len(ids))
	for _, mi := range ids {
		resp.ModelIdentities = append(resp.ModelIdentities, modelIdentity{
			Model:         mi.Model,
			ModelID:       mi.ModelID,
			LastVerdict:   mi.LastVerdict,
			MismatchSince: rfc3339Ptr(mi.MismatchSince),
			ServedID:      mi.ServedID,
			ServedName:    mi.ServedName,
			LastTime:      rfc3339Ptr(mi.LastTime),
		})
	}
	failures := s.modelFailures()
	resp.ModelFailures = make([]modelFailureStatus, 0, len(failures))
	for _, mf := range failures {
		resp.ModelFailures = append(resp.ModelFailures, modelFailureStatus{
			Model:               mf.Model,
			ConsecutiveFailures: mf.Count,
			Since:               rfc3339Ptr(mf.Since),
			Unresponsive:        mf.Unresponsive,
		})
	}
	resp.UpstreamParams = s.client.UpstreamParams()
	resp.ToolPromptRules = apiconv.ToolPromptRules()
	resp.NoInternalCodeExecRule = apiconv.NoInternalCodeExecRule()
	resp.CodeRefFastpath = cfg.CodeRefFastpath
	resp.ToolAuditNote = cfg.ToolAuditNote
	resp.SplitReadWriteBatch = cfg.SplitReadWriteBatch
	resp.MarkerNeutralized = apiconv.MarkerNeutralization()
	resp.StreamProgress, resp.StreamThoughts = cfg.StreamToolsProgress, cfg.StreamThoughts
	resp.ThinkingLevel, resp.MaxPromptBytes = cfg.DefaultThinkingLevel, cfg.MaxPromptBytes
	resp.PromptWarnKB = s.promptWarnKB()
	resp.MaxHeldBytes, resp.MaxHeldSec, resp.HeldStallSec = s.maxHeldBytes(), int(s.maxHeldDuration().Seconds()), int(s.heldStallDuration().Seconds())
	resp.HeldStallFastSec, resp.HeldStallRetriesMax = int(s.heldStallFastDuration().Seconds()), s.maxStallRetries()
	resp.WaitNoteSec = int(s.noContentNoteInterval().Seconds())
	if !cfg.ConfigLoadedAt.IsZero() {
		resp.ConfigLoadedAt = cfg.ConfigLoadedAt.UTC().Format(time.RFC3339)
	}
	resp.EnvModifiedSinceStart = s.envModifiedSinceStart()
	resp.TraceEnabled = s.client.TraceEnabled()
	resp.TemporaryChat = s.client.TemporaryChat()
	resp.IgnoredFields = ignoredFields{
		Note:   "top-level fields received from clients that the proxy parses or forwards nowhere (e.g. temperature, reasoning_effort); counted since start",
		Counts: s.ignored.snapshot(),
	}
	bg := s.client.BackgroundCallStatus()
	byFam := make(map[string]int, len(bg.ByFamily24h))
	for k, v := range bg.ByFamily24h {
		byFam[string(k)] = v
	}
	bgLabel := "aucun depuis le démarrage"
	if !bg.LastAt.IsZero() {
		bgLabel = fmt.Sprintf("%d sur 24 h", bg.Total24h)
	}
	idleQuietMin := cfg.IdleQuietMin
	if cfg.Gemini.IdleQuiet < 0 {
		idleQuietMin = 0
	} else if idleQuietMin <= 0 {
		idleQuietMin = int(gemini.DefaultIdleQuiet / time.Minute)
	}
	resp.BackgroundCalls = backgroundCallsStatus{
		IdleQuietMin:              idleQuietMin,
		CookieRotation:            cfg.CookieRotation,
		CatalogRefreshAfterCookie: cfg.CatalogRefreshAfterCookie,
		AuthRecovery:              cfg.AuthRecovery,
		Total24h:                  bg.Total24h,
		Refused24h:                bg.Refused24h,
		ByFamily24h:               byFam,
		LastAt:                    rfc3339Ptr(bg.LastAt),
		LastFamily:                string(bg.LastFamily),
		LastReason:                bg.LastReason,
		Label:                     bgLabel,
	}
	sendJSON(w, http.StatusOK, resp)
}
