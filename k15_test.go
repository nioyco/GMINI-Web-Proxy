package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
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

// K15 Q2: code_reference followed by tool_call is stripped, counted in
// internal_tool_detected, and displayed in in-flight state as "exécution de code".
// When code_reference appears alone (no tool_call), it is left intact in text and
// internal_tool_detected is not incremented.
func TestK15_Q2_CodeReferenceHandling(t *testing.T) {
	t.Run("followed by tool_call: stripped, counted, shown in-flight", func(t *testing.T) {
		pr, pw := io.Pipe()
		codeRefArrived := make(chan struct{})
		proceed := make(chan struct{})

		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			go func() {
				defer pw.Close()
				part1 := "\n\n```bash?code_reference&code_event_index=1\ngit rev-parse --short HEAD\n```\n"
				_, _ = pw.Write([]byte(fakeStreamServed(part1, "", "e6fa609c3fa255c0", "3.1 Pro")))
				close(codeRefArrived)
				<-proceed
				part2 := part1 + "\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```\n"
				_, _ = pw.Write([]byte(fakeStreamServed(part2, "", "e6fa609c3fa255c0", "3.1 Pro")))
			}()
			return &http.Response{StatusCode: 200, Body: pr}, nil
		}))
		cfg := testConfig()
		cfg.UIEnabled = true
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		var sseRes *sseCapture
		done := make(chan struct{})
		go func() {
			sseRes = readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
			close(done)
		}()

		<-codeRefArrived
		// Wait briefly for runStreamTools to process the event into in-flight state.
		var gotInternal string
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			stRec := uiDo(s.handler(), "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
			var payload struct {
				InFlight []InFlightRequest `json:"in_flight"`
			}
			_ = json.Unmarshal(stRec.Body.Bytes(), &payload)
			if len(payload.InFlight) > 0 && payload.InFlight[0].InternalTool != "" {
				gotInternal = payload.InFlight[0].InternalTool
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		close(proceed)
		<-done

		if gotInternal != "exécution de code" {
			t.Errorf("in-flight internal_tool = %q, want %q", gotInternal, "exécution de code")
		}
		assertProgressStream(t, sseRes)
		joined := strings.Join(sseRes.dataFrames, "\n")
		if strings.Contains(joined, "code_reference") || strings.Contains(joined, "rev-parse") {
			t.Errorf("code_reference block leaked to client: %s", joined)
		}
		if !strings.Contains(joined, `"name":"read_file"`) {
			t.Errorf("tool_call missing: %s", joined)
		}
		stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
		var st struct {
			Counters map[string]int64 `json:"counters"`
		}
		_ = json.Unmarshal(stRec.Body.Bytes(), &st)
		if st.Counters["internal_tool_detected"] != 1 {
			t.Errorf("internal_tool_detected = %d, want 1", st.Counters["internal_tool_detected"])
		}
	})

	t.Run("alone without tool_call: kept in text and counter stays 0", func(t *testing.T) {
		reply := "Voici :\n```bash?code_reference&code_event_index=1\ngit status\n```\nFin."
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

		sseRes := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, sseRes)
		joined := strings.Join(sseRes.dataFrames, "\n")
		if !strings.Contains(joined, "code_reference") || !strings.Contains(joined, "git status") {
			t.Errorf("standalone code_reference should be preserved, got %s", joined)
		}
		stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
		var st struct {
			Counters map[string]int64 `json:"counters"`
		}
		_ = json.Unmarshal(stRec.Body.Bytes(), &st)
		if st.Counters["internal_tool_detected"] != 0 {
			t.Errorf("internal_tool_detected = %d, want 0 when alone", st.Counters["internal_tool_detected"])
		}
	})

	t.Run("retry mode cancels and retries once on code_reference", func(t *testing.T) {
		var calls atomic.Int32
		_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			if n == 1 {
				pr, pw := io.Pipe()
				go func() {
					defer pw.Close()
					_, _ = pw.Write([]byte(fakeStreamServed("```bash?code_reference&code_event_index=1\nls\n```\n", "", "e6fa609c3fa255c0", "3.1 Pro")))
					<-r.Context().Done()
				}()
				return &http.Response{StatusCode: 200, Body: pr}, nil
			}
			body := fakeStreamServed("```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"ok.go\"}}\n```", "", "e6fa609c3fa255c0", "3.1 Pro")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		cfg := testConfig()
		cfg.StreamToolsProgress = true
		cfg.StreamThoughts = true
		cfg.SSEKeepaliveSec = 1
		cfg.InternalToolAction = "retry"
		s := newServer(cfg, client, testLogger())
		s.keepaliveOverride = 50 * time.Millisecond
		ts := httptest.NewServer(s.handler())
		defer ts.Close()

		sseRes := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
		assertProgressStream(t, sseRes)
		if calls.Load() != 2 {
			t.Fatalf("calls = %d, want 2", calls.Load())
		}
		joined := strings.Join(sseRes.dataFrames, "\n")
		if !strings.Contains(joined, `"name":"read_file"`) {
			t.Errorf("retry did not produce tool_call: %s", joined)
		}
	})
}

// K15 Q3: Safety net in finishText logs tool_call_unconverted count=N (no content),
// increments /_status counter, shows it on the UI page, and converts the calls.
func TestK15_Q3_SafetyNetLogsStatusAndUI(t *testing.T) {
	var logs bytes.Buffer
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		reply := "Intro : ```tool_call{\"name\":\"read_file\",\"arguments\":{\"path\":\"SECRET_PATH.go\"}}```"
		body := fakeStreamServed(reply, "", "e6fa609c3fa255c0", "3.1 Pro")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	s := newServer(cfg, client, slog.New(slog.NewTextHandler(&logs, nil)))
	s.keepaliveOverride = 50 * time.Millisecond

	decl := []apiconv.DeclaredTool{{Name: "read_file", Props: map[string]bool{"path": true}}}
	// Exercise finishText directly and HTTP end-to-end.
	text, calls := s.finishText(context.Background(), "Intro : ```tool_call{\"name\":\"read_file\",\"arguments\":{\"path\":\"SECRET_PATH.go\"}}```", true, decl)
	if len(calls) != 1 || calls[0].Function.Name != "read_file" || text != "Intro :" {
		t.Fatalf("finishText got text=%q calls=%+v", text, calls)
	}
	logStr := logs.String()
	if !strings.Contains(logStr, "tool_call_unconverted") || !strings.Contains(logStr, "count=1") {
		t.Errorf("missing tool_call_unconverted log: %s", logStr)
	}
	if strings.Contains(logStr, "SECRET_PATH") {
		t.Errorf("log must never leak argument content: %s", logStr)
	}

	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		Counters map[string]int64 `json:"counters"`
	}
	_ = json.Unmarshal(stRec.Body.Bytes(), &st)
	if st.Counters["tool_call_unconverted"] != 1 {
		t.Errorf("tool_call_unconverted = %d, want 1", st.Counters["tool_call_unconverted"])
	}
	uiRec := uiDo(s.handler(), "GET", "/_ui", "127.0.0.1:5000", "")
	if !strings.Contains(uiRec.Body.String(), "tool_call_unconverted") {
		t.Errorf("UI page missing tool_call_unconverted tile")
	}
}

func fakeStreamWithErrorMarker(text, servedID, servedName string, errMarker bool) string {
	body := make([]any, 43)
	body[4] = []any{[]any{"rc_k15", []any{text}}}
	if errMarker {
		body[31] = true
	}
	body[39], body[42] = servedID, servedName
	ib, _ := json.Marshal(body)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

// K15 Q4: inner[31]=true drives IsUpstreamCannedErrorWithMarker even for an
// arbitrary short phrasing or the French canned-error sentence.
func TestK15_Q4_ErrorMarkerDrivesCannedErrorDetection(t *testing.T) {
	_, client := k9Client(t, rtFn(func(r *http.Request) (*http.Response, error) {
		body := fakeStreamWithErrorMarker("Réponse générique inédite.", "e6fa609c3fa255c0", "3.1 Pro", true)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	cfg := testConfig()
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	cfg.CannedErrorRetry = "off"
	s := newServer(cfg, client, testLogger())
	s.keepaliveOverride = 50 * time.Millisecond
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	c := readSSE(t, context.Background(), ts.URL, k9ChatToolsBody)
	assertProgressStream(t, c)
	joined := strings.Join(c.dataFrames, "\n")
	if !strings.Contains(joined, "Gemini a renvoyé une erreur interne") {
		t.Errorf("expected canned error wrapper from inner[31]=true, got %s", joined)
	}
}

// K15 Q5: On a non-tool stream (like essai_minimal_proxy.py) receiving a canned
// error with inner[31]=true, journal, /_status, /_ui/api/state and /_ui report
// identity="canned_error" with the served model name, never "confirmed".
func TestK15_Q5_CannedErrorVerdictInStatusAndUI(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := testConfig()
	cfg.UIEnabled = true
	cfg.StreamToolsProgress = true
	cfg.StreamThoughts = true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels([]*gemini.AvailableModel{
		{Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"},
	})
	client.SetTransport(rtFn(func(r *http.Request) (*http.Response, error) {
		body := fakeStreamWithErrorMarker("Je ne suis qu'une IA basée sur du texte. Je ne peux pas vous aider à ce sujet.", "e6fa609c3fa255c0", "3.1 Pro", true)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	s := newServer(cfg, client, logger)
	ts := httptest.NewServer(s.handler())
	defer ts.Close()

	body := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"Réponds OK"}]}`
	_ = readSSE(t, context.Background(), ts.URL, body)

	if !strings.Contains(logs.String(), "identity=canned_error") || strings.Contains(logs.String(), "identity=confirmed") {
		t.Errorf("log must show identity=canned_error, never confirmed: %s", logs.String())
	}

	stRec := uiDo(s.handler(), "GET", "/_status", "127.0.0.1:5000", "")
	var st struct {
		ModelIdentities []struct {
			Model       string `json:"model"`
			LastVerdict string `json:"last_verdict"`
			ServedName  string `json:"served_name"`
		} `json:"model_identities"`
	}
	_ = json.Unmarshal(stRec.Body.Bytes(), &st)
	if len(st.ModelIdentities) != 1 || st.ModelIdentities[0].LastVerdict != "canned_error" || st.ModelIdentities[0].ServedName != "3.1 Pro" {
		t.Fatalf("/_status model_identities = %+v", st.ModelIdentities)
	}

	uiStateRec := uiDo(s.handler(), "GET", "/_ui/api/state", "127.0.0.1:5000", s.uiToken)
	var state struct {
		Calls []struct {
			Identity   string `json:"identity"`
			ServedName string `json:"served_name"`
		} `json:"calls"`
	}
	_ = json.Unmarshal(uiStateRec.Body.Bytes(), &state)
	if len(state.Calls) != 1 || state.Calls[0].Identity != "canned_error" || state.Calls[0].ServedName != "3.1 Pro" {
		t.Fatalf("/_ui/api/state calls = %+v", state.Calls)
	}

	uiRec := uiDo(s.handler(), "GET", "/_ui", "127.0.0.1:5000", "")
	if !strings.Contains(uiRec.Body.String(), `identity==="canned_error"`) {
		t.Errorf("ui.html missing canned_error rendering")
	}
}
