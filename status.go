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
			"requests_total":      s.stats.requests.Load(),
			"responses_2xx":       s.stats.status2xx.Load(),
			"responses_4xx":       s.stats.status4xx.Load(),
			"responses_5xx":       s.stats.status5xx.Load(),
			"keepalives_sent":     s.stats.keepalives.Load(),
			"aborted_on_shutdown": s.stats.abortedOnShutdown.Load(),
		},
	}
	if t := s.client.LastCookieRotation(); !t.IsZero() {
		age := int64(time.Since(t).Seconds())
		resp.CookieRotationAgeS = &age
	}
	sendJSON(w, http.StatusOK, resp)
}
