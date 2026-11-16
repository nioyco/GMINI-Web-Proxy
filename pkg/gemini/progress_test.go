package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// wrbLine builds a streamed "wrb.fr" line whose first candidate carries text and,
// when thought is not empty, a thinking text at candidate[thoughtIndex][0][0].
func wrbLine(text, thought string) string {
	cand := make([]any, thoughtIndex+1)
	cand[0] = "rc_" + strings.Repeat("x", 60) // padding: real lines are long
	cand[1] = []any{text}
	if thought != "" {
		cand[thoughtIndex] = []any{[]any{thought}}
	}
	inner := []any{nil, nil, nil, nil, []any{cand}}
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + strings.Repeat(" ", 200) + "\n"
}

func TestParseFrameTextThoughtAndSizes(t *testing.T) {
	fr, ok := parseFrame(wrbLine("Bonjour", "Je réfléchis"))
	if !ok || fr.text != "Bonjour" || fr.thought != "Je réfléchis" {
		t.Fatalf("frame = %+v ok=%v", fr, ok)
	}
	if fr.sizes[1] != len("Bonjour") || fr.sizes[thoughtIndex] != len("Je réfléchis") {
		t.Errorf("sizes = %v", fr.sizes)
	}
	if _, ok := parseFrame("not a frame"); ok {
		t.Error("garbage must not parse")
	}
}

func TestGenerateStreamEventsSeparatesThoughts(t *testing.T) {
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1}, nil)
	body := wrbLine("", "Analyse") + wrbLine("", "Analyse de la demande") + wrbLine("Il est", "Analyse de la demande") + wrbLine("Il est 8h.", "Analyse de la demande")
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	var text, thought strings.Builder
	order := ""
	err := c.GenerateStreamEvents(context.Background(), GenParams{Prompt: "h"}, StreamHandlers{
		Text:    func(d string) { text.WriteString(d); order += "T" },
		Thought: func(d string) { thought.WriteString(d); order += "R" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Il est 8h." || thought.String() != "Analyse de la demande" {
		t.Errorf("text=%q thought=%q", text.String(), thought.String())
	}
	if order != "RRTT" {
		t.Errorf("order = %q, want thoughts before text", order)
	}
}

func innerOf(t *testing.T, body string) []any {
	t.Helper()
	vals, err := url.ParseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	var outer []any
	if err := json.Unmarshal([]byte(vals.Get("f.req")), &outer); err != nil {
		t.Fatal(err)
	}
	var inner []any
	if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
		t.Fatal(err)
	}
	return inner
}

// Without a level the body is the historical 69-element one; with a level the
// experimental slots 79 (mode) and 80 (level) are filled.
func TestBuildBodyThinkingLevel(t *testing.T) {
	c, _ := New(Config{GeminiBL: "bl"}, nil)
	body, _, _ := c.buildBody(GenParams{Prompt: "p"}, nil)
	if n := len(innerOf(t, body)); n != 69 {
		t.Fatalf("default body has %d elements, want 69", n)
	}
	body, _, _ = c.buildBody(GenParams{Prompt: "p", Level: 4, Mode: 3}, nil)
	in := innerOf(t, body)
	if len(in) != 81 || in[79] != float64(3) || in[80] != float64(4) {
		t.Fatalf("len=%d [79]=%v [80]=%v", len(in), in[79], in[80])
	}
	body, _, _ = c.buildBody(GenParams{Prompt: "p", Level: 1}, nil)
	if in = innerOf(t, body); in[79] != nil || in[80] != float64(1) {
		t.Fatalf("level only: [79]=%v [80]=%v", in[79], in[80])
	}
}

// Tracing is off by default; when a directory is set, one private file per
// generation holds the prompt and the reply, never the cookie or the token.
func TestContentTrace(t *testing.T) {
	dir := t.TempDir() + "/traces"
	run := func(traceDir string) {
		c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1, Cookie: "SAPISID=SECRETSAPI; __Secure-1PSID=SECRETPSID", TraceDir: traceDir, TraceMaxFiles: 2}, nil)
		c.setAt("SECRET_AT_TOKEN")
		body := wrbLine("Il est 8h.", "Analyse")
		c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}))
		ctx := WithMetrics(context.Background(), &CallMetrics{ID: "req1"})
		for i := 0; i < 3; i++ {
			if err := c.GenerateStreamEvents(ctx, GenParams{Prompt: "PROMPT_MARKER"}, StreamHandlers{}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Millisecond)
		}
	}
	run("")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("no trace directory may be created when tracing is off")
	}
	run(dir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("want 2 files kept (max), got %d err=%v", len(entries), err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", st.Mode().Perm())
	}
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
		}
		data, _ := os.ReadFile(dir + "/" + e.Name())
		s := string(data)
		if !strings.Contains(s, "PROMPT_MARKER") || !strings.Contains(s, "Il est 8h.") || !strings.Contains(s, "Analyse") {
			t.Errorf("trace must hold prompt, reply and thought: %s", s)
		}
		for _, secret := range []string{"SECRETSAPI", "SECRETPSID", "SECRET_AT_TOKEN"} {
			if strings.Contains(s, secret) {
				t.Errorf("trace leaks %s", secret)
			}
		}
	}
}

func wrbStatusLine(body2 any) string {
	inner := []any{nil, []any{"c_1", "r_1"}, body2}
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

func TestParseFrameBody2StatusAndTolerant(t *testing.T) {
	// Observed structure: <200 bytes, body[2]["7"] = [null,...,["Analyzing user requests"]]
	line := wrbStatusLine(map[string]any{
		"7":  []any{nil, nil, nil, nil, nil, []any{"Analyzing user requests"}},
		"44": true,
	})
	if len(line) >= 200 {
		t.Fatalf("expected short status line <200 bytes, got %d", len(line))
	}
	fr, ok := parseFrame(line)
	if !ok || fr.status != "Analyzing user requests" {
		t.Fatalf("parseFrame status = (%+v, %v), want status='Analyzing user requests'", fr, ok)
	}

	// Unexpected shapes must be ignored without error.
	for _, bad := range []any{
		nil,
		123,
		"plain string",
		map[string]any{"7": 42},
		map[string]any{"7": map[string]any{"x": "y"}},
		map[string]any{"7": []any{nil, nil}},
		map[string]any{"18": "r_1"},
	} {
		if fr, ok := parseFrame(wrbStatusLine(bad)); ok && fr.status != "" {
			t.Errorf("bad body[2]=%#v should not produce status, got %q", bad, fr.status)
		}
	}
}

func TestGenerateStreamEventsEmitsStatus(t *testing.T) {
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1}, nil)
	body := wrbStatusLine(map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Analyzing user requests"}}}) +
		wrbStatusLine(map[string]any{"7": []any{nil, nil, nil, nil, nil, []any{"Fulfilling request"}}}) +
		wrbLine("Done.", "Thinking...")
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	var statuses []string
	var gotText strings.Builder
	err := c.GenerateStreamEvents(context.Background(), GenParams{Prompt: "h"}, StreamHandlers{
		Status: func(s string) { statuses = append(statuses, s) },
		Text:   func(d string) { gotText.WriteString(d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != "Analyzing user requests" || statuses[1] != "Fulfilling request" {
		t.Fatalf("statuses = %v", statuses)
	}
	if gotText.String() != "Done." {
		t.Fatalf("text = %q", gotText.String())
	}
}

func wrbBodyLine(inner []any) string {
	ib, _ := json.Marshal(inner)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + strings.Repeat(" ", 60) + "\n"
}

func TestParseFrameBody11Body24Body52AndTolerant(t *testing.T) {
	// body[11]: Google Search extension card
	inner11 := make([]any, 12)
	inner11[11] = []any{[]any{nil, []any{"google", []any{nil, nil, "Google Search"}, 5}}}
	fr, ok := parseFrame(wrbBodyLine(inner11))
	if !ok || fr.internalTool != "Google Search" {
		t.Fatalf("body[11] got (%+v, %v), want internalTool='Google Search'", fr, ok)
	}

	// body[24][0]: internal Google tool vs local tool
	inner24 := make([]any, 25)
	inner24[24] = []any{[]any{
		[]any{"run_command", nil, 2},
		[]any{"manage_task", "status", 2},
		[]any{"retriever", "expand_skills", 2},
	}}
	fr, ok = parseFrame(wrbBodyLine(inner24))
	if !ok || fr.internalTool != "retriever:expand_skills" {
		t.Fatalf("body[24][0] got (%+v, %v), want internalTool='retriever:expand_skills'", fr, ok)
	}

	// body[24][0] with ONLY local tools must NOT trigger internalTool
	innerLocal := make([]any, 25)
	innerLocal[24] = []any{[]any{
		[]any{"run_command", nil, 2},
		[]any{"read_file", nil, 2},
		[]any{"manage_task", "status", 2},
	}}
	if fr, ok := parseFrame(wrbBodyLine(innerLocal)); ok && fr.internalTool != "" {
		t.Fatalf("local tools triggered internalTool: %+v", fr)
	}

	// body[52]: limit banner
	inner52 := make([]any, 53)
	inner52[52] = []any{[]any{"Limit reached. Continuing with Flash-Lite."}}
	fr, ok = parseFrame(wrbBodyLine(inner52))
	if !ok || fr.limitBanner != "Limit reached. Continuing with Flash-Lite." {
		t.Fatalf("body[52] got (%+v, %v), want limitBanner", fr, ok)
	}

	// Unexpected shapes must not panic or misreport
	for _, bad := range [][]any{
		make([]any, 12),
		{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "not-a-list"},
		make([]any, 25),
		make([]any, 53),
	} {
		if len(bad) > 24 && bad[24] == nil {
			bad[24] = "not-a-list"
		}
		if len(bad) > 52 && bad[52] == nil {
			bad[52] = []any{"not-a-nested-list"}
		}
		fr, ok := parseFrame(wrbBodyLine(bad))
		if ok && (fr.internalTool != "" || fr.limitBanner != "") {
			t.Errorf("unexpected shape produced %+v", fr)
		}
	}
}

func TestTraceRawFrameTimesMS(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1, TraceDir: dir, TraceRaw: true}, nil)
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			_, _ = pw.Write([]byte(wrbLine("", "T1")))
			time.Sleep(15 * time.Millisecond)
			_, _ = pw.Write([]byte(wrbLine("Hi", "T1")))
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: pr, Request: r}, nil
	}))
	if err := c.GenerateStreamEvents(context.Background(), GenParams{Prompt: "h"}, StreamHandlers{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 trace file, got %d (err=%v)", len(entries), err)
	}
	b, err := os.ReadFile(dir + "/" + entries[0].Name())
	if err != nil {
		t.Fatal(err)
	}
	var tf traceFile
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatal(err)
	}
	if len(tf.RawFrames) != 2 || len(tf.RawFrameTimesM) != len(tf.RawFrames) {
		t.Fatalf("raw_frames=%d raw_frame_times_ms=%v", len(tf.RawFrames), tf.RawFrameTimesM)
	}
	if tf.RawFrameTimesM[1] < tf.RawFrameTimesM[0] {
		t.Errorf("timestamps not monotonic: %v", tf.RawFrameTimesM)
	}
}
