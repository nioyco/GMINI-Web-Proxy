package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// headerRequestID is the correlation header accepted from clients and echoed back.
const headerRequestID = "X-Request-ID"

// requestIDRe is the accepted shape of an inbound X-Request-ID: 1–64 chars of
// [A-Za-z0-9._-]. Anything else (control chars, spaces, long values — log injection
// vectors) is discarded and a fresh id is generated.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// errAbortedOnShutdown is the cancellation cause set on requests still active when
// the shutdown grace period expires.
var errAbortedOnShutdown = errors.New("aborted_on_shutdown")

// newRequestID returns a random 16-hex-char id.
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000" + hex.EncodeToString([]byte(time.Now().Format("0405")))[:8]
	}
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID returns the inbound id when valid, otherwise a generated one.
func sanitizeRequestID(in string) string {
	if requestIDRe.MatchString(in) {
		return in
	}
	return newRequestID()
}

// reqInfo is the per-request record shared between the middleware and handlers.
// Fields other than the atomics are written only by the handler goroutine (before
// the middleware reads them once the handler returns), so they need no lock.
type reqInfo struct {
	id         string
	method     string
	route      string
	start      time.Time
	model      string
	stream     bool
	keepalives atomic.Int64
	cancel     context.CancelCauseFunc

	echoStripped   atomic.Int64
	echoFabricated atomic.Int64
	refusal        atomic.Bool
	internalTool   atomic.Bool
	met            gemini.CallMetrics // upstream timing/size, filled by the gemini client

	// In-flight tracking (K7, K8, K9)
	inFlightMu      sync.RWMutex
	servedID        string
	servedName      string
	inFlightVerdict string
	thoughtBytes    int
	textBytes       int
	sentBytes       int
	lastFrameTime   time.Time
	statusNote      string
}

func (ri *reqInfo) setServed(id, name, verdict string) {
	if ri == nil {
		return
	}
	ri.inFlightMu.Lock()
	ri.servedID = id
	ri.servedName = name
	ri.inFlightVerdict = verdict
	ri.lastFrameTime = time.Now()
	ri.inFlightMu.Unlock()
}

func (ri *reqInfo) setProgress(thoughtBytes, textBytes int) {
	if ri == nil {
		return
	}
	ri.inFlightMu.Lock()
	ri.thoughtBytes = thoughtBytes
	ri.textBytes = textBytes
	ri.lastFrameTime = time.Now()
	ri.inFlightMu.Unlock()
}

func (ri *reqInfo) setReceived(thoughtBytes, textBytes int) {
	if ri == nil {
		return
	}
	ri.inFlightMu.Lock()
	if thoughtBytes > ri.thoughtBytes {
		ri.thoughtBytes = thoughtBytes
	}
	if textBytes > ri.textBytes {
		ri.textBytes = textBytes
	}
	ri.lastFrameTime = time.Now()
	ri.inFlightMu.Unlock()
}

func (ri *reqInfo) setSent(sentBytes int) {
	if ri == nil {
		return
	}
	ri.inFlightMu.Lock()
	ri.sentBytes = sentBytes
	ri.inFlightMu.Unlock()
}

func (ri *reqInfo) setStatusNote(note string) {
	if ri == nil {
		return
	}
	ri.inFlightMu.Lock()
	ri.statusNote = note
	ri.inFlightMu.Unlock()
}

// InFlightRequest summarizes one request currently in progress (K7, K8, K9).
type InFlightRequest struct {
	ID              string  `json:"id"`
	Model           string  `json:"model"`
	ServedID        string  `json:"served_id,omitempty"`
	ServedName      string  `json:"served_name,omitempty"`
	Identity        string  `json:"identity,omitempty"`
	StartTime       string  `json:"start_time"`
	DurationMS      int64   `json:"duration_ms"`
	ThoughtBytes    int     `json:"thought_bytes"`
	TextBytes       int     `json:"text_bytes"`
	SentBytes       int     `json:"sent_bytes"`
	LastFrameAgeSec float64 `json:"last_frame_age_sec"`
	StatusNote      string  `json:"status_note,omitempty"`
}

func (ri *reqInfo) inFlightSnapshot(now time.Time) InFlightRequest {
	ri.inFlightMu.RLock()
	defer ri.inFlightMu.RUnlock()
	lastAge := 0.0
	if !ri.lastFrameTime.IsZero() {
		lastAge = now.Sub(ri.lastFrameTime).Seconds()
		if lastAge < 0 {
			lastAge = 0
		}
	}
	durMS := now.Sub(ri.start).Milliseconds()
	if durMS < 0 {
		durMS = 0
	}
	return InFlightRequest{
		ID:              ri.id,
		Model:           ri.model,
		ServedID:        ri.servedID,
		ServedName:      ri.servedName,
		Identity:        ri.inFlightVerdict,
		StartTime:       ri.start.UTC().Format(time.RFC3339),
		DurationMS:      durMS,
		ThoughtBytes:    ri.thoughtBytes,
		TextBytes:       ri.textBytes,
		SentBytes:       ri.sentBytes,
		LastFrameAgeSec: lastAge,
		StatusNote:      ri.statusNote,
	}
}

type ctxKey struct{}

// reqFrom returns the request record in ctx, or nil.
func reqFrom(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(ctxKey{}).(*reqInfo)
	return ri
}

// setReqMeta records the resolved model and stream flag for the request log.
func setReqMeta(ctx context.Context, model string, stream bool) {
	if ri := reqFrom(ctx); ri != nil {
		ri.model, ri.stream = model, stream
	}
}

// addKeepalive counts one keepalive comment sent for this request.
func addKeepalive(ctx context.Context) {
	if ri := reqFrom(ctx); ri != nil {
		ri.keepalives.Add(1)
	}
}

// statsCounters are the simple process-wide counters exposed by /_status.
type statsCounters struct {
	requests             atomic.Int64
	status2xx            atomic.Int64
	status4xx            atomic.Int64
	status5xx            atomic.Int64
	keepalives           atomic.Int64
	abortedOnShutdown    atomic.Int64
	echoStripped         atomic.Int64
	echoFabricated       atomic.Int64
	upstreamRefusal      atomic.Int64
	toolCallUnparsed     atomic.Int64
	toolNameInferred     atomic.Int64
	internalToolDetected atomic.Int64
	earlyStop            atomic.Int64
	earlyStopMarker      atomic.Int64
	earlyStopSize        atomic.Int64
	earlyStopDuration    atomic.Int64

	// Upstream aggregates (only requests that reached Gemini).
	upstreamRequests  atomic.Int64
	upstreamAttempts  atomic.Int64
	upstreamRetries   atomic.Int64
	upstreamMS        atomic.Int64
	firstByteMS       atomic.Int64
	upstreamReqBytes  atomic.Int64
	upstreamRespBytes atomic.Int64
}

// activeSet tracks in-flight requests so shutdown can cancel them.
type activeSet struct {
	mu sync.Mutex
	m  map[*reqInfo]struct{}
}

func (a *activeSet) add(ri *reqInfo) {
	a.mu.Lock()
	if a.m == nil {
		a.m = map[*reqInfo]struct{}{}
	}
	a.m[ri] = struct{}{}
	a.mu.Unlock()
}

func (a *activeSet) remove(ri *reqInfo) {
	a.mu.Lock()
	delete(a.m, ri)
	a.mu.Unlock()
}

func (a *activeSet) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.m)
}

func (a *activeSet) snapshot() []*reqInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*reqInfo, 0, len(a.m))
	for ri := range a.m {
		out = append(out, ri)
	}
	return out
}

func (a *activeSet) inFlight() []InFlightRequest {
	snap := a.snapshot()
	now := time.Now()
	out := make([]InFlightRequest, 0, len(snap))
	for _, ri := range snap {
		if ri.model != "" {
			out = append(out, ri.inFlightSnapshot(now))
		}
	}
	return out
}

// AbortActive cancels every request still in flight (cause aborted_on_shutdown)
// and logs one aborted_on_shutdown line each. It returns how many were aborted.
func (s *Server) AbortActive() int {
	reqs := s.active.snapshot()
	for _, ri := range reqs {
		s.log.Warn("aborted_on_shutdown",
			"request_id", ri.id, "route", ri.route, "model", ri.model,
			"stream", ri.stream, "elapsed_ms", time.Since(ri.start).Milliseconds())
		s.stats.abortedOnShutdown.Add(1)
		ri.cancel(errAbortedOnShutdown)
	}
	return len(reqs)
}

// statusWriter records the status code and body size, and forwards Flush so SSE
// keeps working through the wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// withTracking is the outermost middleware: it assigns/validates the request id,
// echoes it in X-Request-ID, registers the request as active (cancellable on
// shutdown), and emits request_start / request_end log lines. The lines carry only
// metadata — never the prompt, response, tool content, headers, cookies, keys or
// the query string.
func (s *Server) withTracking(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(headerRequestID))
		w.Header().Set(headerRequestID, id)

		ctx, cancel := context.WithCancelCause(r.Context())
		ri := &reqInfo{id: id, method: r.Method, route: r.URL.Path, start: time.Now(), cancel: cancel}
		ctx = context.WithValue(ctx, ctxKey{}, ri)
		ri.met.ID = id
		ctx = gemini.WithMetrics(ctx, &ri.met)
		s.active.add(ri)
		s.stats.requests.Add(1)
		sw := &statusWriter{ResponseWriter: w}
		// The management page polls its own API every few seconds: not logged.
		quiet := strings.HasPrefix(ri.route, "/_ui") && r.Method == http.MethodGet
		if !quiet {
			s.log.Info("request_start", "request_id", id, "method", r.Method, "route", ri.route)
		}

		defer func() {
			s.active.remove(ri)
			cancel(nil)
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			switch {
			case status >= 500:
				s.stats.status5xx.Add(1)
			case status >= 400:
				s.stats.status4xx.Add(1)
			default:
				s.stats.status2xx.Add(1)
			}
			s.stats.keepalives.Add(ri.keepalives.Load())
			m := &ri.met
			if m.Attempts.Load() > 0 {
				s.stats.upstreamRequests.Add(1)
				s.stats.upstreamAttempts.Add(m.Attempts.Load())
				s.stats.upstreamRetries.Add(m.Retries.Load())
				s.stats.upstreamMS.Add(m.UpstreamMS.Load())
				s.stats.firstByteMS.Add(m.FirstByteMS.Load())
				s.stats.upstreamReqBytes.Add(m.ReqBytes.Load())
				s.stats.upstreamRespBytes.Add(m.RespBytes.Load())
			}
			if quiet && status < 400 {
				return
			}
			s.log.Info("request_end",
				"request_id", id, "method", r.Method, "route", ri.route,
				"model", ri.model, "stream", ri.stream, "status", status,
				"duration_ms", time.Since(ri.start).Milliseconds(),
				"keepalives", ri.keepalives.Load(), "bytes", sw.bytes,
				"echo_stripped", ri.echoStripped.Load(), "upstream_refusal", ri.refusal.Load(),
				"upstream_req_bytes", m.ReqBytes.Load(), "prompt_bytes", m.PromptBytes.Load(),
				"upstream_attempts", m.Attempts.Load(), "upstream_retries", m.Retries.Load(),
				"retry_wait_ms", m.RetryWaitMS.Load(), "first_byte_ms", m.FirstByteMS.Load(),
				"upstream_ms", m.UpstreamMS.Load(), "upstream_resp_bytes", m.RespBytes.Load(),
				"resp_bytes", sw.bytes)
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}
