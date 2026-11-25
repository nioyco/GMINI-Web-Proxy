package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func k9Client(t *testing.T, rt http.RoundTripper) (*Config, *gemini.Client) {
	t.Helper()
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Description: "4 Hydrogen"},
		{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"},
	})
	client.SetTransport(rt)
	return cfg, client
}

const k9ChatToolsBody = `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"go"}],` +
	`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}},` +
	`{"type":"function","function":{"name":"write_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}}}]}`

// (a) Complete tool_call + forbidden marker + 100 KiB: client receives the tool call
// in < 1s after the marker and upstream reading is cancelled.
func TestK9_EarlyStopOnMarkerCancelsUpstream(t *testing.T) {
	pr, pw := io.Pipe()
	var upstreamCancelled atomic.Bool
	var markerSentAt atomic.Int64
	var extraFramesWritten atomic.Int64

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"main.go\"}}\n```\n"
			if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			markerSentAt.Store(time.Now().UnixNano())
			acc += "[Tool result for read_file]: package main // SECRET_MARKER_PAYLOAD\n"
			if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
				return
			}
			// Try to drip 100 KiB slowly; upstream cancellation must cut it short.
			chunk := strings.Repeat("Z", 2048)
			for i := 0; i < 50; i++ {
				select {
				case <-r.Context().Done():
					upstreamCancelled.Store(true)
					return
				case <-time.After(25 * time.Millisecond):
					acc += chunk
					if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
						upstreamCancelled.Store(true)
						return
					}
					extraFramesWritten.Add(1)
				}
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	elapsedSinceMarker := time.Since(time.Unix(0, markerSentAt.Load()))
	assertProgressStream(t, c)

	if elapsedSinceMarker >= time.Second {
		t.Fatalf("client waited %v after marker, want < 1s", elapsedSinceMarker)
	}
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if last.FinishReason == nil || *last.FinishReason != "tool_calls" || len(last.Delta.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", c.dataFrames)
	}
	if last.Delta.ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool name = %q", last.Delta.ToolCalls[0].Function.Name)
	}

	// Wait briefly for the upstream goroutine to observe ctx cancellation / closed pipe.
	deadline := time.Now().Add(time.Second)
	for !upstreamCancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !upstreamCancelled.Load() {
		t.Errorf("upstream read was not cancelled (extraFramesWritten=%d)", extraFramesWritten.Load())
	}
	if extraFramesWritten.Load() >= 40 {
		t.Errorf("upstream kept reading too long: extraFramesWritten=%d", extraFramesWritten.Load())
	}

	// Status counter & metadata-only log check.
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal(stRec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Counters["early_stop"] != 1 || st.Counters["early_stop_marker"] != 1 {
		t.Errorf("unexpected counters: %+v", st.Counters)
	}
	// Successful early stop after tool call must not record stream_interrupted error.
	if calls := client.RecentCalls(); len(calls) == 0 || calls[0].Error != "" {
		t.Errorf("expected clean call record after tool delivery, got %+v", calls)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "early_stop") || !strings.Contains(logs, "reason=marker") {
		t.Errorf("missing early_stop log line: %s", logs)
	}
	if strings.Contains(logs, "SECRET_MARKER_PAYLOAD") || strings.Contains(logs, "main.go") {
		t.Errorf("content leaked into log: %s", logs)
	}
}

// (b) Three consecutive tool_calls without forbidden marker: all three are delivered.
func TestK9_ThreeConsecutiveToolCallsDelivered(t *testing.T) {
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"1.go\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
			acc += "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"2.go\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
			acc += "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"3.go\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.SSEKeepaliveSec = 1
	cfg.MaxHeldBytes = 64 * 1024
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 3 {
		t.Fatalf("got %d tool calls, want 3: %v", len(last.Delta.ToolCalls), c.dataFrames)
	}
}

// (c) Endless held text without tool call: cut at 64 KiB cap with clear message.
func TestK9_EndlessTextWithoutToolCallCutAtSizeCap(t *testing.T) {
	var upstreamCancelled atomic.Bool
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			// Unclosed fence simulates runaway held output without any complete tool call.
			acc := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"x.txt\",\"content\":\""
			chunk := strings.Repeat("a", 2048)
			for i := 0; i < 60; i++ { // up to 120 KiB, should stop shortly after 64 KiB
				select {
				case <-r.Context().Done():
					upstreamCancelled.Store(true)
					return
				default:
				}
				acc += chunk
				if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
					upstreamCancelled.Store(true)
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.SSEKeepaliveSec = 1
	cfg.MaxHeldBytes = 64 * 1024
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if last.Delta.Content == nil || !strings.Contains(*last.Delta.Content, "GMINI : la réponse de Gemini s'emballe") ||
		!strings.Contains(*last.Delta.Content, "demande interrompue") {
		t.Fatalf("expected clear runaway interruption message, got %v", c.dataFrames)
	}
	if len(last.Delta.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(last.Delta.ToolCalls))
	}

	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop"] != 1 || st.Counters["early_stop_size"] != 1 {
		t.Errorf("expected early_stop_size=1, got %+v", st.Counters)
	}

	// Subcase: endless fabricated tool results (dropped by EchoFilter, no tool call).
	_, client2 := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := ""
			line := "[Tool result for read_file]: " + strings.Repeat("b", 1024) + "\n\n"
			for i := 0; i < 90; i++ {
				select {
				case <-r.Context().Done():
					return
				default:
				}
				acc += line
				if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	s2 := newServer(cfg, client2, testLogger())
	s2.keepaliveOverride = 50 * time.Millisecond
	ts2 := httptest.NewServer(s2.handler())
	defer ts2.Close()

	c2 := readSSE(t, context.Background(), ts2.URL, k9ChatToolsBody)
	assertProgressStream(t, c2)
	vs2 := views(t, c2)
	last2 := vs2[len(vs2)-1].Choices[0]
	if last2.Delta.Content == nil || !strings.Contains(*last2.Delta.Content, "GMINI : la réponse de Gemini s'emballe") {
		t.Fatalf("expected runaway message on endless echo-dropped text, got %v", c2.dataFrames)
	}
}

// (d) Normal long response (50 KiB write_file): NOT cut under the default 64 KiB cap.
func TestK9_LongWriteFile50KBNotCut(t *testing.T) {
	payload := strings.Repeat("x", 50*1024)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"big.txt\",\"content\":\""
			pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
			for off := 0; off < len(payload); off += 4096 {
				end := off + 4096
				if end > len(payload) {
					end = len(payload)
				}
				acc += payload[off:end]
				pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
			}
			acc += "\"}}\n```"
			pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.SSEKeepaliveSec = 1
	cfg.MaxHeldBytes = 64 * 1024
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("expected 1 write_file call, got %v", c.dataFrames)
	}
	if len(last.Delta.ToolCalls[0].Function.Arguments) < 50*1024 {
		t.Errorf("arguments truncated: len=%d", len(last.Delta.ToolCalls[0].Function.Arguments))
	}
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop"] != 0 {
		t.Errorf("expected early_stop=0 for 50 KiB write_file, got %+v", st.Counters)
	}
}

// Normal long response (> 64 KiB, e.g. 80 KiB write_file): NOT cut under default 256 KiB cap.
func TestK9_LongWriteFile80KBNotCut(t *testing.T) {
	payload := strings.Repeat("y", 80*1024)
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"huge.txt\",\"content\":\""
			pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
			for off := 0; off < len(payload); off += 8192 {
				end := off + 8192
				if end > len(payload) {
					end = len(payload)
				}
				acc += payload[off:end]
				pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
			}
			acc += "\"}}\n```"
			pw.Write([]byte(fakeStreamServed(acc, "", "e6fa609c3fa255c0", "3.1 Pro")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("expected 1 write_file call, got %v", c.dataFrames)
	}
	if len(last.Delta.ToolCalls[0].Function.Arguments) < 80*1024 {
		t.Errorf("arguments truncated: len=%d", len(last.Delta.ToolCalls[0].Function.Arguments))
	}
}

// Duration cap: with complete tool call -> delivers it; without -> clear message.
func TestK9_DurationCapWithAndWithoutToolCall(t *testing.T) {
	t.Run("with complete tool call", func(t *testing.T) {
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				acc := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\nsome rambling without marker"
				pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen")))
				<-r.Context().Done()
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 40 * time.Millisecond
		s.heldDurationOverride = 120 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		vs := views(t, c)
		last := vs[len(vs)-1].Choices[0]
		if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "read_file" {
			t.Fatalf("expected read_file delivered on duration cap, got %v", c.dataFrames)
		}
		stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
		var st struct {
			Counters map[string]int64 `json:"counters"`
		}
		json.Unmarshal(stRec.Body.Bytes(), &st)
		if st.Counters["early_stop_duration"] != 1 {
			t.Errorf("expected early_stop_duration=1, got %+v", st.Counters)
		}
	})

	t.Run("without tool call", func(t *testing.T) {
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				acc := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\",\"content\":\""
				pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen")))
				chunk := strings.Repeat("still_streaming_", 8)
				for {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(15 * time.Millisecond):
						acc += chunk
						if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
							return
						}
					}
				}
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 40 * time.Millisecond
		s.heldDurationOverride = 120 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, c)
		vs := views(t, c)
		last := vs[len(vs)-1].Choices[0]
		if last.Delta.Content == nil || !strings.Contains(*last.Delta.Content, "GMINI : la réponse de Gemini s'emballe") {
			t.Fatalf("expected runaway message on duration cap, got %v", c.dataFrames)
		}
	})
}

func TestK9_HeldStallRetrySucceeds(t *testing.T) {
	const k9RunCmdBody = `{"model":"gemini-everyday-help","stream":true,"messages":[{"role":"user","content":"go"}],` +
		`"tools":[{"type":"function","function":{"name":"run_command","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`
	var attempts atomic.Int32
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			if n == 1 {
				acc := "```tool_call\n{\"name\": \"run_command\", \"arguments" // 47 bytes
				if len(acc) != 47 {
					t.Errorf("fixture len=%d, want 47", len(acc))
				}
				pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
				time.Sleep(25 * time.Millisecond)
				// Heartbeat {"37":[0]} arrives while text stays frozen.
				pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
				<-r.Context().Done()
				return
			}
			acc := "```tool_call\n{\"name\":\"run_command\",\"arguments\":{\"command\":\"echo ok\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 20 * time.Millisecond
	s.heldStallOverride = 80 * time.Millisecond
	s.heldDurationOverride = 250 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9RunCmdBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "run_command" {
		t.Fatalf("expected run_command delivered after stall retry, got %v", c.dataFrames)
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop_stall"] != 1 || st.Counters["held_stall_retries"] != 1 || st.Counters["early_stop_duration"] != 0 {
		t.Errorf("unexpected counters: %+v", st.Counters)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "reason=stall") || !strings.Contains(logs, "held_stall_retry") {
		t.Errorf("expected stall & held_stall_retry in logs: %s", logs)
	}
}

func TestK9_HeldStallExhaustedReportsBytes(t *testing.T) {
	var attempts atomic.Int32
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\": \"run_command\", \"arguments" // 47 bytes
			pw.Write([]byte(fakeStreamServed(acc, "", "56fdd199312815e2", "4 Hydrogen")))
			time.Sleep(20 * time.Millisecond)
			pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
			<-r.Context().Done()
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 20 * time.Millisecond
	s.heldStallOverride = 70 * time.Millisecond
	s.heldDurationOverride = 200 * time.Millisecond
	s.stallRetryBackoffOverride = 5 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if last.Delta.Content == nil {
		t.Fatalf("expected error content on exhausted stall, got %v", c.dataFrames)
	}
	got := *last.Delta.Content
	if !strings.Contains(got, "47 o reçus sans suite") {
		t.Errorf("expected exact byte count '47 o reçus sans suite', got %q", got)
	}
	if strings.Contains(got, "s'emballe") || strings.Contains(got, "1 Ko sans fin") {
		t.Errorf("must not report runaway 's'emballe (1 Ko sans fin)', got %q", got)
	}
	if attempts.Load() != 4 {
		t.Errorf("expected 4 attempts (1 initial + 3 retries), got %d", attempts.Load())
	}
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop_stall"] != 4 || st.Counters["held_stall_retries"] != 3 || st.Counters["early_stop_duration"] != 0 {
		t.Errorf("unexpected counters: %+v", st.Counters)
	}
}

// Progress note every 30s (overridden in test) & in-flight received vs sent bytes (K7/K9).
func TestK9_HeldProgressNoteAndInFlightBytes(t *testing.T) {
	release := make(chan struct{})
	inFlightChecked := make(chan InFlightRequest, 1)

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			acc := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen")))
			time.Sleep(30 * time.Millisecond)
			acc += strings.Repeat("prose ", 350) // ~2.1 KiB held after complete tool call, no marker
			pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen")))
			<-release
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 40 * time.Millisecond
	s.heldNoteOverride = 60 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	go func() {
		time.Sleep(160 * time.Millisecond)
		stRec := uiDo(s.handler(), "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
		var st struct {
			InFlight []InFlightRequest `json:"in_flight"`
		}
		if err := json.Unmarshal(stRec.Body.Bytes(), &st); err == nil && len(st.InFlight) > 0 {
			inFlightChecked <- st.InFlight[0]
		}
		close(release)
	}()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	if !strings.Contains(c.raw, "appel d'outil reçu, Gemini continue d'écrire :") {
		t.Fatalf("missing 30s held progress note in SSE stream: %s", c.raw)
	}
	select {
	case inf := <-inFlightChecked:
		if inf.TextBytes < 2000 || inf.SentBytes != 0 {
			t.Errorf("unexpected in-flight received/sent bytes: text=%d sent=%d", inf.TextBytes, inf.SentBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("did not observe in-flight snapshot")
	}
}

// Early stop on other formats (/v1/responses, /v1/messages, /v1beta/...:streamGenerateContent).
func TestK9_EarlyStopOtherFormats(t *testing.T) {
	cases := []struct {
		name, path, body, reply, wantTool string
	}{
		{
			name:     "responses",
			path:     "/v1/responses",
			body:     responsesToolsBody,
			reply:    "```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}\n```\n[Tool result for get_weather]: 20C\n",
			wantTool: "get_weather",
		},
		{
			name:     "messages",
			path:     "/v1/messages",
			body:     messagesToolsBody,
			reply:    "```tool_call\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}\n```\n[Assistant]: sunny\n",
			wantTool: "get_weather",
		},
		{
			name:     "google",
			path:     "/v1beta/models/gemini-3.1-pro:streamGenerateContent?alt=sse",
			body:     googleToolsBody,
			reply:    "```function_call\n{\"name\":\"get_weather\",\"args\":{\"city\":\"Paris\"}}\n```\n[User]: thanks\n",
			wantTool: "get_weather",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCancelled atomic.Bool
			_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					acc := tc.reply
					pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen")))
					for i := 0; i < 40; i++ {
						select {
						case <-r.Context().Done():
							upstreamCancelled.Store(true)
							return
						case <-time.After(25 * time.Millisecond):
							acc += strings.Repeat("X", 2048)
							if _, err := pw.Write([]byte(fakeStreamServed(acc, "", "cf41b0e0dd7d53e5", "4 Hydrogen"))); err != nil {
								upstreamCancelled.Store(true)
								return
							}
						}
					}
				}()
				return &http.Response{StatusCode: 200, Body: pr}, nil
			}))
			cfg := testConfig()
			cfg.StreamToolsProgress = true
			cfg.SSEKeepaliveSec = 1
			s := newServer(cfg, client, testLogger())
			s.keepaliveOverride = 40 * time.Millisecond
			ts := httptest.NewServer(s.handler())
			defer ts.Close()

			start := time.Now()
			resp, raw := postRaw(t, context.Background(), ts.URL, tc.path, tc.body)
			if resp.StatusCode != 200 {
				t.Fatalf("status=%d raw=%s", resp.StatusCode, raw)
			}
			if time.Since(start) >= time.Second {
				t.Fatalf("took too long (%v)", time.Since(start))
			}
			if !strings.Contains(raw, tc.wantTool) {
				t.Errorf("missing tool %q in %s", tc.wantTool, raw)
			}
			deadline := time.Now().Add(time.Second)
			for !upstreamCancelled.Load() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !upstreamCancelled.Load() {
				t.Errorf("upstream read not cancelled for format %s", tc.name)
			}
		})
	}
}

func TestK9_ConfigEnvCapsAndUIHTML(t *testing.T) {
	t.Setenv("GEMINI_MAX_HELD_BYTES", "32768")
	t.Setenv("GEMINI_MAX_HELD_SEC", "90")
	t.Setenv("GEMINI_HELD_STALL_SEC", "45")
	t.Setenv("GEMINI_HELD_STALL_FAST_SEC", "22")
	t.Setenv("GEMINI_HELD_STALL_RETRIES", "9")
	cfg := loadConfig("/nonexistent/.env")
	if cfg.MaxHeldBytes != 32768 || cfg.MaxHeldSec != 90 || cfg.HeldStallSec != 45 || cfg.HeldStallFastSec != 22 || cfg.HeldStallRetries != 5 {
		t.Fatalf("got MaxHeldBytes=%d MaxHeldSec=%d HeldStallSec=%d HeldStallFastSec=%d HeldStallRetries=%d",
			cfg.MaxHeldBytes, cfg.MaxHeldSec, cfg.HeldStallSec, cfg.HeldStallFastSec, cfg.HeldStallRetries)
	}
	cfg.UIEnabled = true
	sOn := newTestServer(t, cfg)
	stOn := uiDo(sOn.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var stBody struct {
		HeldStallSec        int `json:"held_stall_sec"`
		HeldStallFastSec    int `json:"held_stall_fast_sec"`
		HeldStallRetriesMax int `json:"held_stall_retries_max"`
	}
	if err := json.Unmarshal(stOn.Body.Bytes(), &stBody); err != nil {
		t.Fatal(err)
	}
	if stBody.HeldStallSec != 45 || stBody.HeldStallFastSec != 22 || stBody.HeldStallRetriesMax != 5 {
		t.Fatalf("/_status stall fields = %+v", stBody)
	}

	t.Setenv("GEMINI_MAX_HELD_BYTES", "0")
	t.Setenv("GEMINI_MAX_HELD_SEC", "0")
	t.Setenv("GEMINI_HELD_STALL_SEC", "0")
	t.Setenv("GEMINI_HELD_STALL_FAST_SEC", "0")
	t.Setenv("GEMINI_HELD_STALL_RETRIES", "0")
	cfgOff := loadConfig("/nonexistent/.env")
	s := newTestServer(t, cfgOff)
	if s.maxHeldBytes() != 0 || s.maxHeldDuration() != 0 || s.heldStallDuration() != 0 || s.heldStallFastDuration() != 0 || s.maxStallRetries() != 0 {
		t.Fatalf("expected 0 when disabled, got bytes=%d dur=%v stall=%v fast=%v retries=%d",
			s.maxHeldBytes(), s.maxHeldDuration(), s.heldStallDuration(), s.heldStallFastDuration(), s.maxStallRetries())
	}

	_, h := uiServer(t)
	w := uiDo(h, "GET", "/_ui", "127.0.0.1:5000", "")
	if !strings.Contains(w.Body.String(), "Arrêts anticipés") || !strings.Contains(w.Body.String(), "Transmis") {
		t.Errorf("ui.html missing K9 labels")
	}
}

// Reproduces 1dc3f58617569dd2: prompt > 300 KiB (initial wait note + keepalives)
// followed by an open ```tool_call write_file of 1232 B (> earlyHold=400 B) that
// stalls with sent_bytes=0; held_stall_retry must still fire and recover on attempt 2.
func TestK9_HeldStallRetryLargeOpenWriteFile1232Bytes(t *testing.T) {
	largeBody, _ := json.Marshal(map[string]any{
		"model":  "gemini-everyday-help",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "write file " + strings.Repeat("A", 320*1024)},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "write_file",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]string{"type": "string"},
							"content": map[string]string{"type": "string"},
						},
					},
				},
			},
		},
	})

	prefix := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"out.txt\",\"content\":\""
	stalled1232 := prefix + strings.Repeat("x", 1232-len(prefix))
	if len(stalled1232) != 1232 {
		t.Fatalf("fixture len=%d, want 1232", len(stalled1232))
	}

	var attempts atomic.Int32
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		n := attempts.Add(1)
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			if n == 1 {
				// Delay first frame so keepalives and the >300 KiB initial wait note are emitted first.
				time.Sleep(35 * time.Millisecond)
				pw.Write([]byte(fakeStreamServed(stalled1232, "", "56fdd199312815e2", "4 Hydrogen")))
				time.Sleep(25 * time.Millisecond)
				pw.Write([]byte("36\n[[\"wrb.fr\",null,\"[{\\\"37\\\":[0]}]\"]]\n"))
				<-r.Context().Done()
				return
			}
			okReply := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"out.txt\",\"content\":\"ok\"}}\n```\n"
			pw.Write([]byte(fakeStreamServed(okReply, "", "56fdd199312815e2", "4 Hydrogen")))
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 10 * time.Millisecond
	s.initialWaitNoteOverride = 15 * time.Millisecond
	s.heldStallOverride = 80 * time.Millisecond
	s.heldDurationOverride = 250 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, string(largeBody))
	assertProgressStream(t, c)
	if !strings.Contains(c.raw, "Demande envoyée à") {
		t.Errorf("expected initial wait note for >300 KiB prompt in SSE stream")
	}
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("expected write_file delivered after held_stall_retry, got %v", c.dataFrames)
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop_stall"] != 1 || st.Counters["held_stall_retries"] != 1 || st.Counters["early_stop_duration"] != 0 {
		t.Errorf("unexpected counters: %+v", st.Counters)
	}
	logs := logBuf.String()
	for _, want := range []string{"reason=stall", "held_bytes=1232", "sent_bytes=0", "held_stall_retry"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected %q in logs: %s", want, logs)
		}
	}
}

func TestK9_FastStallOnlyWhenNoThoughtsAndOpenFence(t *testing.T) {
	t.Run("no_thoughts_open_fence", func(t *testing.T) {
		var attempts atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			n := attempts.Add(1)
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				if n == 1 {
					pw.Write([]byte(fakeStreamServed("```tool_call\n{\"name\":\"read_file\",", "", "56fdd199312815e2", "4 Hydrogen")))
					<-r.Context().Done()
					return
				}
				ok := "```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n"
				pw.Write([]byte(fakeStreamServed(ok, "", "56fdd199312815e2", "4 Hydrogen")))
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))

		var logBuf bytes.Buffer
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
		s.keepaliveOverride = 15 * time.Millisecond
		s.heldStallFastOverride = 55 * time.Millisecond
		s.heldStallOverride = 500 * time.Millisecond
		s.heldDurationOverride = 800 * time.Millisecond
		s.stallRetryBackoffOverride = 5 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		start := time.Now()
		c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		elapsed := time.Since(start)
		assertProgressStream(t, c)
		if elapsed >= 350*time.Millisecond {
			t.Fatalf("fast stall did not trigger quickly: elapsed=%v", elapsed)
		}
		vs := views(t, c)
		last := vs[len(vs)-1].Choices[0]
		if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "read_file" {
			t.Fatalf("expected read_file after fast stall retry, got %v", c.dataFrames)
		}
		if attempts.Load() != 2 || !strings.Contains(logBuf.String(), "fast_stall=true") {
			t.Fatalf("attempts=%d logs=%s", attempts.Load(), logBuf.String())
		}
	})

	t.Run("with_thoughts_4neon_waits_full_stall", func(t *testing.T) {
		const neonBody = `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"go"}],` +
			`"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`
		var attempts atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			attempts.Add(1)
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				part1 := "```tool_call\n{\"name\":\"read_file\","
				pw.Write([]byte(fakeStreamServed(part1, "Je réfléchis avant d'agir.", "e6fa609c3fa255c0", "4 Neon")))
				time.Sleep(130 * time.Millisecond) // > heldStallFastOverride (50ms), < heldStallOverride (350ms)
				full := part1 + "\"arguments\":{\"path\":\"a.go\"}}\n```\n"
				pw.Write([]byte(fakeStreamServed(full, "Je réfléchis avant d'agir.", "e6fa609c3fa255c0", "4 Neon")))
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))

		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 15 * time.Millisecond
		s.heldStallFastOverride = 50 * time.Millisecond
		s.heldStallOverride = 350 * time.Millisecond
		s.heldDurationOverride = 600 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		c := readSSE(t, context.Background(), ts.URL, neonBody)
		assertProgressStream(t, c)
		vs := views(t, c)
		last := vs[len(vs)-1].Choices[0]
		if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "read_file" {
			t.Fatalf("expected read_file without premature cut, got %v", c.dataFrames)
		}
		if attempts.Load() != 1 || s.stats.heldStallRetries.Load() != 0 || s.stats.earlyStopStall.Load() != 0 {
			t.Fatalf("expected single attempt without fast stall cut: attempts=%d retries=%d stalls=%d",
				attempts.Load(), s.stats.heldStallRetries.Load(), s.stats.earlyStopStall.Load())
		}
	})
}

func TestK9_HeldStallTwoRetriesWithTempChatAndLevelStepdown(t *testing.T) {
	largeBody, _ := json.Marshal(map[string]any{
		"model":  "gemini-everyday-help",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "write file " + strings.Repeat("Z", 310*1024)},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "write_file",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path":    map[string]string{"type": "string"},
							"content": map[string]string{"type": "string"},
						},
					},
				},
			},
		},
	})

	type wireSlots struct {
		tempChat any
		mode     any
		level    any
	}
	seenSlots := make(chan wireSlots, 4)
	var attempts atomic.Int32

	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		rawBody, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(rawBody))
		var outer []any
		var inner []any
		if err := json.Unmarshal([]byte(vals.Get("f.req")), &outer); err == nil && len(outer) > 1 {
			if s, ok := outer[1].(string); ok {
				_ = json.Unmarshal([]byte(s), &inner)
			}
		}
		var ws wireSlots
		if len(inner) > 45 {
			ws.tempChat = inner[45]
		}
		if len(inner) > 80 {
			ws.mode, ws.level = inner[79], inner[80]
		}
		seenSlots <- ws

		n := attempts.Add(1)
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			switch n {
			case 1:
				pw.Write([]byte(fakeStreamServed("```tool_call\n{\"name\":\"write_file\",", "", "56fdd199312815e2", "4 Hydrogen")))
				<-r.Context().Done()
			case 2:
				pw.Write([]byte(fakeStreamServed("```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\",", "", "56fdd199312815e2", "4 Hydrogen")))
				<-r.Context().Done()
			default:
				ok := "```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\",\"content\":\"ok\"}}\n```\n"
				pw.Write([]byte(fakeStreamServed(ok, "", "56fdd199312815e2", "4 Hydrogen")))
			}
		}()
		return &http.Response{StatusCode: 200, Body: pr}, nil
	}))

	var logBuf bytes.Buffer
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.DefaultThinkingLevel = "medium"
	cfg.ThinkingModes = parseModeCodes(defaultThinkingModeCodes)
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logBuf, nil)))
	s.keepaliveOverride = 15 * time.Millisecond
	s.heldStallFastOverride = 50 * time.Millisecond
	s.heldStallOverride = 90 * time.Millisecond
	s.heldDurationOverride = 400 * time.Millisecond
	s.stallRetryBackoffOverride = 5 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, string(largeBody))
	assertProgressStream(t, c)
	vs := views(t, c)
	last := vs[len(vs)-1].Choices[0]
	if len(last.Delta.ToolCalls) != 1 || last.Delta.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("expected write_file delivered on attempt 3, got %v", c.dataFrames)
	}
	if attempts.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts.Load())
	}
	close(seenSlots)
	var got []wireSlots
	for ws := range seenSlots {
		got = append(got, ws)
	}
	if len(got) != 3 {
		t.Fatalf("captured %d upstream payloads, want 3", len(got))
	}
	if got[0].tempChat != nil || got[0].mode != float64(1) || got[0].level != float64(4) {
		t.Errorf("attempt 1 slots = %+v, want tempChat=nil mode=1 level=4", got[0])
	}
	if got[1].tempChat != nil || got[1].mode != float64(1) || got[1].level != float64(2) {
		t.Errorf("attempt 2 slots = %+v, want tempChat=nil mode=1 level=2 (stepdown 4->2 on 1st retry)", got[1])
	}
	if got[2].tempChat != nil || got[2].mode != float64(1) || got[2].level != float64(1) {
		t.Errorf("attempt 3 slots = %+v, want tempChat=nil mode=1 level=1 (stepdown 2->1)", got[2])
	}
	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["early_stop_stall"] != 2 || st.Counters["held_stall_retries"] != 2 {
		t.Errorf("unexpected counters: %+v", st.Counters)
	}
	logs := logBuf.String()
	for _, want := range []string{"attempt=1", "attempt=2", "max_retries=3", "fast_stall=true", "temp_chat=false", "level_stepdown=4->2", "level_stepdown=2->1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("missing %q in logs: %s", want, logs)
		}
	}
}
