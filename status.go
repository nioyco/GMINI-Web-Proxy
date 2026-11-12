package main

import (
	"net/http"
	"runtime/debug"
	"time"
)

// statusResponse is the read-only diagnostic payload of GET /_status. It never
// contains secrets: no cookie, key, token or prompt content.
type statusResponse struct {
	Version            string           `json:"version"`
	Commit             string           `json:"commit"`
	Dirty              bool             `json:"dirty"`
	GoVersion          string           `json:"go_version"`
	UptimeSec          int64            `json:"uptime_sec"`
	Authenticated      bool             `json:"authenticated"`
	CookieRotationAgeS *int64           `json:"cookie_last_rotation_age_sec"` // null: no rotation yet in this process
	ModelsSource       string           `json:"models_source"`                // dynamic | fallback | static
	ModelsCount        int              `json:"models_count"`
	DefaultModel       string           `json:"default_model"`
	ActiveRequests     int              `json:"active_requests"` // includes this /_status request
	SSEKeepaliveSec    int              `json:"sse_keepalive_sec"`
	ShutdownGraceSec   int              `json:"shutdown_grace_sec"`
	Counters           map[string]int64 `json:"counters"`
	CookieRotation     rotationStatus   `json:"cookie_rotation"`
}

// rotationStatus describes the cookie rotation state (no secrets).
type rotationStatus struct {
	AuthState            string `json:"auth_state"` // authenticated | anonymous
	PSIDTSPresent        bool   `json:"psidts_present"`
	LastAttemptAgeSec    *int64 `json:"last_attempt_age_sec"`
	LastSuccessAgeSec    *int64 `json:"last_success_age_sec"`
	LastResult           string `json:"last_result"`
	Attempts             int64  `json:"attempts"`
	Renewed              int64  `json:"renewed"`
	NotRenewed           int64  `json:"not_renewed"`
	ConsecutiveNoRenewal int64  `json:"consecutive_no_renewal"`
	TokenAgeSec          *int64 `json:"token_age_sec"`
	TokenAgeSource       string `json:"token_age_source"`
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

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	commit, dirty, goVer := buildInfo()
	resp := statusResponse{
		Version:          version,
		Commit:           commit,
		Dirty:            dirty,
		GoVersion:        goVer,
		UptimeSec:        int64(time.Since(s.started).Seconds()),
		Authenticated:    s.client.Authenticated(),
		ModelsSource:     s.client.ModelsSourceLabel(),
		ModelsCount:      len(s.client.ModelNames()),
		DefaultModel:     s.client.DefaultModelName(),
		ActiveRequests:   s.active.count(),
		SSEKeepaliveSec:  s.cfg.SSEKeepaliveSec,
		ShutdownGraceSec: s.cfg.ShutdownGraceSec,
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
		AuthState: "anonymous", PSIDTSPresent: rs.PSIDTSPresent,
		LastAttemptAgeSec: ageSec(rs.LastAttempt), LastSuccessAgeSec: ageSec(rs.LastSuccess),
		LastResult: rs.LastResult, Attempts: rs.Attempts, Renewed: rs.Renewed, NotRenewed: rs.NotRenewed,
		ConsecutiveNoRenewal: rs.ConsecutiveFailure, TokenAgeSource: rs.TokenAgeSource,
	}
	if resp.Authenticated {
		resp.CookieRotation.AuthState = "authenticated"
	}
	if rs.TokenAgeSource != "unknown" {
		a := int64(rs.TokenAge.Seconds())
		resp.CookieRotation.TokenAgeSec = &a
	}
	sendJSON(w, http.StatusOK, resp)
}
