package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// WrbBody builds a batchexecute-shaped body whose model text is text (test helper).
func wrbBody(text string) string {
	inner := []any{nil, nil, nil, nil, []any{[]any{"rc_1", []any{text}}}, nil, nil, nil, nil, nil, nil, nil, "padding-padding-padding-padding"}
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return ")]}'\n\n" + string(line) + strings.Repeat(" ", 200) + "\n"
}

func TestMetricsGenerateWithRetry(t *testing.T) {
	c, err := New(Config{RetryAttempts: 3, RetryDelaySec: 0, GeminiBL: "bl"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	body := wrbBody("hello there")
	c.SetTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("garbage"))}, nil // empty -> retry
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	var m CallMetrics
	out, err := c.Generate(WithMetrics(context.Background(), &m), GenParams{Prompt: "bonjour"})
	if err != nil || out != "hello there" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if m.Attempts.Load() != 2 || m.Retries.Load() != 1 {
		t.Errorf("attempts=%d retries=%d", m.Attempts.Load(), m.Retries.Load())
	}
	if m.PromptBytes.Load() != 7 || m.ReqBytes.Load() <= 7 {
		t.Errorf("prompt=%d req=%d", m.PromptBytes.Load(), m.ReqBytes.Load())
	}
	if m.RespBytes.Load() != int64(len("garbage")+len(body)) {
		t.Errorf("resp bytes = %d", m.RespBytes.Load())
	}
	if !m.fbSet.Load() {
		t.Error("first byte not recorded")
	}
}

func TestMetricsStreamAndNilSafe(t *testing.T) {
	c, _ := New(Config{RetryAttempts: 1, GeminiBL: "bl"}, nil)
	body := wrbBody("streamed text")
	c.SetTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	var got strings.Builder
	var m CallMetrics
	if err := c.GenerateStream(WithMetrics(context.Background(), &m), GenParams{Prompt: "p"}, func(d string) { got.WriteString(d) }); err != nil {
		t.Fatal(err)
	}
	if got.String() != "streamed text" || m.Attempts.Load() != 1 || m.RespBytes.Load() != int64(len(body)) || !m.fbSet.Load() {
		t.Errorf("got=%q attempts=%d resp=%d", got.String(), m.Attempts.Load(), m.RespBytes.Load())
	}
	// no metrics in ctx: must not panic
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
}
