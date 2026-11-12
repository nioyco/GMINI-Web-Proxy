package main

import (
	"net"
	"net/http"
	"strings"
)

// hostOnly strips an optional port (and IPv6 brackets) from an HTTP Host value and
// lowercases it. "[::1]:8081" → "::1", "localhost" → "localhost".
func hostOnly(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return strings.ToLower(h)
}

// hostAllowed reports whether the request Host is loopback (127.0.0.1, localhost,
// ::1 on any port) or listed in extra (GEMINI_ALLOWED_HOSTS). This blocks DNS
// rebinding: a browser page on evil.example resolving to 127.0.0.1 still sends
// "Host: evil.example".
func hostAllowed(host string, extra []string) bool {
	h := hostOnly(host)
	switch h {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	for _, e := range extra {
		if h == hostOnly(e) {
			return true
		}
	}
	return false
}

// withHostCheck rejects requests whose Host header is not an allowed host (403).
func (s *Server) withHostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostAllowed(r.Host, s.cfg.AllowedHosts) {
			sendError(w, http.StatusForbidden, "forbidden host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin is in the configured CORS allow-list.
func (s *Server) originAllowed(origin string) bool {
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" {
			continue // a wildcard is deliberately not honoured
		}
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// withCORS emits CORS headers only for origins listed in GEMINI_CORS_ORIGINS. By
// default (empty list) no CORS header is ever sent. A preflight from a
// non-allowed origin is refused with 403 instead of the former 204 + "*".
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && s.originAllowed(origin)
		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			switch {
			case allowed:
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, x-api-key, x-goog-api-key, anthropic-version, X-Request-ID")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
			case origin == "":
				w.WriteHeader(http.StatusNoContent) // plain OPTIONS, no CORS involved
			default:
				sendError(w, http.StatusForbidden, "origin not allowed")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
