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
