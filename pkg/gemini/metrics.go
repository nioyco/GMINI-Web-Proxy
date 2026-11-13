package gemini

import (
	"context"
	"sync/atomic"
	"time"
)

// CallMetrics collects per-request upstream timing and size figures, with no
// content. A request handler attaches one to the context (WithMetrics) and the
// Client fills it while generating. All fields are safe for concurrent use.
type CallMetrics struct {
	ReqBytes    atomic.Int64 // upstream request body size (bytes)
	PromptBytes atomic.Int64 // prompt size inside it (bytes)
	Attempts    atomic.Int64 // upstream attempts made
	Retries     atomic.Int64 // of which retries (Attempts-1)
	RetryWaitMS atomic.Int64 // time slept between attempts
	FirstByteMS atomic.Int64 // first attempt: time to first upstream byte (response headers / first data)
	UpstreamMS  atomic.Int64 // total time spent talking to upstream (attempt durations, no retry sleeps)
	RespBytes   atomic.Int64 // upstream response bytes read
	fbSet       atomic.Bool
}

type metricsKey struct{}

// WithMetrics returns ctx carrying m.
func WithMetrics(ctx context.Context, m *CallMetrics) context.Context {
	return context.WithValue(ctx, metricsKey{}, m)
}

func metricsFrom(ctx context.Context) *CallMetrics {
	m, _ := ctx.Value(metricsKey{}).(*CallMetrics)
	return m
}

// nil-safe recorders.

func (m *CallMetrics) request(promptBytes, bodyBytes int) {
	if m == nil {
		return
	}
	m.PromptBytes.Store(int64(promptBytes))
	m.ReqBytes.Store(int64(bodyBytes))
}

func (m *CallMetrics) attempt(n int) {
	if m == nil {
		return
	}
	m.Attempts.Add(1)
	if n > 0 {
		m.Retries.Add(1)
	}
}

func (m *CallMetrics) firstByte(since time.Time) {
	if m == nil || !m.fbSet.CompareAndSwap(false, true) {
		return
	}
	m.FirstByteMS.Store(time.Since(since).Milliseconds())
}

func (m *CallMetrics) upstream(since time.Time, respBytes int) {
	if m == nil {
		return
	}
	m.UpstreamMS.Add(time.Since(since).Milliseconds())
	m.RespBytes.Add(int64(respBytes))
}

func (m *CallMetrics) retryWait(d time.Duration) {
	if m == nil {
		return
	}
	m.RetryWaitMS.Add(d.Milliseconds())
}
