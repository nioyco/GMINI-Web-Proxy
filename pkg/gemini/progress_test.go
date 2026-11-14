package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
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
