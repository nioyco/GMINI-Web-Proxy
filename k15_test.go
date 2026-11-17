package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// K15 Q1: An ordinary exotic-tag fence before tool_call blocks in runStreamTools
// (K9 progressive path) must stream out as text without being held back, and all
// following tool calls must be converted.
func TestK15_Q1_StreamToolsExoticFenceNotHeldAndCallsConverted(t *testing.T) {
	exotic := strings.Repeat("a", 420)
	reply := "```foo?a=1&b=2:bar\n" + exotic + "\n```\n\n" +
		"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"1.go\"}}\n```\n" +
		"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"2.go\"}}\n```\n" +
		"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"3.go\"}}\n```\n" +
		"```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"4.go\"}}\n```\n"

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		body := fakeStreamServed(reply, "", "e6fa609c3fa255c0", "3.1 Pro")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	joined := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(joined, "foo?a=1&b=2:bar") {
		t.Errorf("exotic fence should have streamed progressively, got %s", joined)
	}
	if n := strings.Count(joined, `"name":"read_file"`); n != 4 {
		t.Errorf("converted tool_calls = %d, want 4 (%s)", n, joined)
	}
}
