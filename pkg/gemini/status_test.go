package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func statusRT(status int, body string, calls *atomic.Int32) rtFunc {
	return func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

// AUDIT-13 A8 (F-02): the generation status is read; typed errors; no retry on
// 4xx; the size-limit hint is reserved for the empty 200.
func TestGenerateStatusClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		withAt    bool
		wantKind  error
		wantCalls int32
	}{
		{"401", 401, true, ErrSessionAuth, 1},
		{"403", 403, false, ErrSessionAuth, 1},
		{"400 with token", 400, true, ErrSessionAuth, 1},
		{"400 without token", 400, false, ErrUpstreamRejected, 1},
		{"404", 404, true, ErrUpstreamRejected, 1},
		{"500", 500, true, ErrUpstreamServer, 3},
		{"503", 503, false, ErrUpstreamServer, 3},
		{"200 empty", 200, true, ErrEmptyResponse, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			body := "<html>SECRETBODY</html>"
			c, logs := blockClient(t, statusRT(tc.status, body, &calls))
			if tc.withAt {
				c.setAt("tok")
			}
			_, err := c.Generate(context.Background(), GenParams{Prompt: "p"})
			if !errors.Is(err, tc.wantKind) {
				t.Fatalf("err = %v, want %v", err, tc.wantKind)
			}
			if calls.Load() != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
			if tc.status != 200 && strings.Contains(err.Error(), "size limit") {
				t.Errorf("size-limit hint on HTTP %d: %v", tc.status, err)
			}
			var se *UpstreamStatusError
			if tc.status != 200 && (!errors.As(err, &se) || se.Status != tc.status) {
				t.Errorf("no status in %v", err)
			}
			l := logs.String()
			if strings.Contains(l, "SECRETBODY") || strings.Contains(err.Error(), "SECRETBODY") || strings.Contains(l, "tok") {
				t.Errorf("content leaked: %s", l)
			}
			if tc.status != 200 && !strings.Contains(l, "upstream_status") {
				t.Errorf("no upstream_status log: %s", l)
			}
		})
	}
}

func TestGenerateStreamStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   error
	}{{401, ErrSessionAuth}, {500, ErrUpstreamServer}, {404, ErrUpstreamRejected}} {
		var calls atomic.Int32
		c, _ := blockClient(t, statusRT(tc.status, "x", &calls))
		c.setAt("tok")
		emitted := 0
		err := c.GenerateStream(context.Background(), GenParams{Prompt: "p"}, func(string) { emitted++ })
		if !errors.Is(err, tc.kind) || calls.Load() != 1 || emitted != 0 {
			t.Errorf("HTTP %d: err=%v calls=%d emitted=%d", tc.status, err, calls.Load(), emitted)
		}
	}
}
