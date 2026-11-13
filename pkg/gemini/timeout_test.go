package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// redirectRT sends every request to the test server, keeping its context (so the
// http.Client timeout still applies).
func redirectRT(t *testing.T, srv *httptest.Server) http.RoundTripper {
	u, _ := url.Parse(srv.URL)
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host = u.Scheme, u.Host
		return http.DefaultTransport.RoundTrip(r2)
	})
}

// slowStream writes one cumulative line immediately, then another after gap.
func slowStream(gap time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Write([]byte(wrbBody("first part")))
		f.Flush()
		select {
		case <-time.After(gap):
		case <-r.Context().Done():
			return
		}
		w.Write([]byte(wrbBody("first part and the rest")))
	}
}

// GEMINI_REQUEST_TIMEOUT_SEC is the TOTAL http.Client timeout, body read included:
// a stream that outlives it is cut, one that fits is delivered whole.
func TestRequestTimeoutIsTotalTimeoutOnStreams(t *testing.T) {
	srv := httptest.NewServer(slowStream(1600 * time.Millisecond))
	defer srv.Close()

	short, _ := New(Config{RequestTimeout: 1, RetryAttempts: 1, GeminiBL: "bl"}, nil)
	short.SetTransport(redirectRT(t, srv))
	var got strings.Builder
	err := short.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(d string) { got.WriteString(d) })
	if err == nil {
		t.Errorf("1s total timeout must cut a 1.6s stream (got %q)", got.String())
	}

	long, _ := New(Config{RequestTimeout: 5, RetryAttempts: 1, GeminiBL: "bl"}, nil)
	long.SetTransport(redirectRT(t, srv))
	got.Reset()
	if err := long.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(d string) { got.WriteString(d) }); err != nil || got.String() != "first part and the rest" {
		t.Errorf("5s timeout: err=%v got=%q", err, got.String())
	}
}

// No other time cap hides below the configured total timeout.
func TestNoHiddenTimeoutBelowRequestTimeout(t *testing.T) {
	c, _ := New(Config{RequestTimeout: 1800}, nil)
	if c.http.Timeout != 1800*time.Second {
		t.Errorf("client timeout = %v", c.http.Timeout)
	}
	tr := c.http.Transport.(*googleTransport).base.(*http.Transport)
	if tr.ResponseHeaderTimeout != 0 || tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("transport caps: header=%v idle=%v (idle only bounds unused connections)", tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
	}
}

// Retries happen only before any byte is delivered: a stream interrupted after
// its first line is never replayed.
func TestStreamNeverRetriesAfterFirstByte(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(wrbBody("partial")))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // abort mid-stream
	}))
	defer srv.Close()
	c, _ := New(Config{RequestTimeout: 5, RetryAttempts: 3, RetryDelaySec: 0, GeminiBL: "bl"}, nil)
	c.SetTransport(redirectRT(t, srv))
	var got strings.Builder
	err := c.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(d string) { got.WriteString(d) })
	if err == nil || got.String() != "partial" || calls.Load() != 1 {
		t.Errorf("err=%v got=%q calls=%d", err, got.String(), calls.Load())
	}
}
