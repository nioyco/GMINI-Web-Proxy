package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

const progressBody = `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"hi"}],` +
	`"tools":[{"type":"function","function":{"name":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]%s}`

type progressFixture struct {
	url     string
	srv     *Server
	params  chan gemini.GenParams
	blocked chan struct{}
}

func progressServer(t *testing.T, stream func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error) *progressFixture {
	t.Helper()
	cfg := testConfig()
	cfg.SSEKeepaliveSec = 1
	cfg.StreamToolsProgress, cfg.StreamThoughts = true, true
	cfg.ThinkingModes = map[string]int{"proid": 3}
	srv := newServer(cfg, mustTestClient(t, cfg), testLogger())
	srv.keepaliveOverride = 50 * time.Millisecond
	f := &progressFixture{srv: srv, params: make(chan gemini.GenParams, 4)}
	srv.streamEvents = func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error {
		f.params <- p
		return stream(ctx, p, h)
	}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

type chunkView struct {
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			Reasoning *string `json:"reasoning_content"`
			ToolCalls []struct {
				Function struct{ Name, Arguments string }
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct{ Message string } `json:"error"`
}

func views(t *testing.T, c *sseCapture) []chunkView {
	t.Helper()
	var out []chunkView
	for _, f := range c.dataFrames {
		var v chunkView
		if err := json.Unmarshal([]byte(f), &v); err != nil {
			t.Fatalf("bad frame %q: %v", f, err)
		}
		out = append(out, v)
	}
	return out
}

// assertProgressStream checks the SSE envelope of a progressive stream.
func assertProgressStream(t *testing.T, c *sseCapture) {
	t.Helper()
	if c.status != http.StatusOK || c.contentType != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q", c.status, c.contentType)
	}
	if !c.done || !strings.HasSuffix(c.raw, "data: [DONE]\n\n") {
		t.Fatalf("missing terminating [DONE]; raw=%q", c.raw)
	}
	if !strings.HasPrefix(c.raw, ": keepalive\n\n") {
		t.Errorf("the stream must start with a keepalive comment")
	}
}

func body(extra string) string { return strings.Replace(progressBody, "%s", extra, 1) }

// Thoughts, then text, go out before the generation ends; the tool call is held
// back and arrives alone in the final chunk.
func TestProgressiveToolStream(t *testing.T) {
	release := make(chan struct{})
	f := progressServer(t, func(ctx context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Thought("Je réfléchis. ")
		h.Text("Je lance la commande.\n\n")
		h.Text("```tool_call\n{\"name\": \"run\", ")
		<-release
		h.Text("\"arguments\": {\"cmd\": \"ls\"}}\n```")
		return nil
	})
	go func() { time.Sleep(200 * time.Millisecond); close(release) }()
	c := readSSE(t, context.Background(), f.url, body(""))
	assertProgressStream(t, c)
	vs := views(t, c)
	if len(vs) != 3 {
		t.Fatalf("frames = %d, want 3 (thought, text, final): %v", len(vs), c.dataFrames)
	}
	if r := vs[0].Choices[0].Delta.Reasoning; r == nil || *r != "Je réfléchis. " {
		t.Errorf("first frame must be the thought: %s", c.dataFrames[0])
	}
	if ct := vs[1].Choices[0].Delta.Content; ct == nil || *ct != "Je lance la commande.\n\n" {
		t.Errorf("second frame must be the text: %s", c.dataFrames[1])
	}
	last := vs[2].Choices[0]
	if last.FinishReason == nil || *last.FinishReason != "tool_calls" || len(last.Delta.ToolCalls) != 1 ||
		last.Delta.ToolCalls[0].Function.Name != "run" || last.Delta.Content != nil {
		t.Errorf("final frame: %s", c.dataFrames[2])
	}
	if strings.Contains(c.dataFrames[1], "tool_call") {
		t.Error("the tool call block leaked into the forwarded text")
	}
	if c.comments < 2 {
		t.Errorf("keepalives = %d, want the initial one plus at least one during the silence", c.comments)
	}
}

// A plain answer (no tool call) streams entirely, then a stop chunk.
func TestProgressivePlainAnswer(t *testing.T) {
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("Il est ")
		h.Text("8h07 à Chicago.")
		return nil
	})
	c := readSSE(t, context.Background(), f.url, body(""))
	assertProgressStream(t, c)
	vs := views(t, c)
	var sb strings.Builder
	for _, v := range vs {
		if ct := v.Choices[0].Delta.Content; ct != nil {
			sb.WriteString(*ct)
		}
	}
	if sb.String() != "Il est 8h07 à Chicago." {
		t.Errorf("text = %q", sb.String())
	}
	if fr := vs[len(vs)-1].Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("last frame must stop: %s", c.dataFrames[len(c.dataFrames)-1])
	}
}

// An empty or failed stream before anything was forwarded falls back to the
// buffered path (which retries); a failure after text was forwarded is reported.
func TestProgressiveFallbackAndError(t *testing.T) {
	f := progressServer(t, func(context.Context, gemini.GenParams, gemini.StreamHandlers) error { return nil })
	f.srv.generate = func(context.Context, gemini.GenParams) (string, error) {
		return "```tool_call\n{\"name\": \"run\", \"arguments\": {\"cmd\": \"pwd\"}}\n```", nil
	}
	c := readSSE(t, context.Background(), f.url, body(""))
	assertProgressStream(t, c)
	vs := views(t, c)
	if len(vs) != 1 || len(vs[0].Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("fallback must give one complete chunk: %v", c.dataFrames)
	}

	g := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("Début de réponse")
		return errors.New("connection reset")
	})
	g.srv.generate = func(context.Context, gemini.GenParams) (string, error) {
		t.Error("no fallback once text was forwarded")
		return "", nil
	}
	c = readSSE(t, context.Background(), g.url, body(""))
	vs = views(t, c)
	if last := vs[len(vs)-1]; last.Error == nil || !strings.Contains(last.Error.Message, "connection reset") {
		t.Errorf("want an in-band error, got %v", c.dataFrames)
	}
}

// Effort: model@suffix wins over reasoning_effort, which wins over the default;
// none set sends nothing; the mode code comes from the model id.
func TestEffortSelection(t *testing.T) {
	ok := func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error { h.Text("ok"); return nil }
	f := progressServer(t, ok)
	check := func(reqBody string, level, mode int) {
		t.Helper()
		readSSE(t, context.Background(), f.url, reqBody)
		if p := <-f.params; p.Level != level || p.Mode != mode {
			t.Errorf("level=%d mode=%d, want %d/%d", p.Level, p.Mode, level, mode)
		}
	}
	check(body(""), 0, 0)
	check(body(`,"reasoning_effort":"high"`), 2, 3)
	check(strings.Replace(body(`,"reasoning_effort":"high"`), "gemini-3.1-pro", "gemini-3.1-pro@low", 1), 1, 3)
	f.srv.cfg.DefaultThinkingLevel = "medium"
	check(body(""), 4, 3)
	check(strings.Replace(body(""), "gemini-3.1-pro", "gemini-3.5-flash", 1), 4, 0) // no mode code known for this id
}

func TestPromptTooLarge(t *testing.T) {
	f := progressServer(t, func(context.Context, gemini.GenParams, gemini.StreamHandlers) error {
		t.Error("an oversized prompt must not reach Gemini")
		return nil
	})
	f.srv.cfg.MaxPromptBytes = 50
	resp, err := http.Post(f.url+"/v1/chat/completions", "application/json", strings.NewReader(body("")))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// While a long tool call is being written, progress notes go out as
// reasoning_content and nothing of the call leaks into the text.
func TestProgressHintsDuringLongToolCall(t *testing.T) {
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("```tool_call\n{\"name\": \"run\", \"arguments\": {\"cmd\": \"")
		for i := 0; i < 6; i++ {
			h.Text(strings.Repeat("x", 400))
			time.Sleep(40 * time.Millisecond)
		}
		h.Text("\"}}\n```")
		return nil
	})
	c := readSSE(t, context.Background(), f.url, body(""))
	assertProgressStream(t, c)
	vs := views(t, c)
	hints := 0
	for _, v := range vs[:len(vs)-1] {
		d := v.Choices[0].Delta
		if d.Content != nil {
			t.Errorf("no text may be forwarded during a tool call: %q", *d.Content)
		}
		if d.Reasoning != nil && strings.Contains(*d.Reasoning, "appel d'outil") {
			hints++
		}
	}
	if hints == 0 {
		t.Errorf("want at least one progress note; frames: %v", c.dataFrames)
	}
	if last := vs[len(vs)-1].Choices[0]; len(last.Delta.ToolCalls) != 1 || len(last.Delta.ToolCalls[0].Function.Arguments) < 2400 {
		t.Errorf("final frame must carry the whole tool call")
	}
}

// The web mode code comes from the catalogue; the built-in table is only a
// fallback and an explicit GEMINI_THINKING_MODE_CODES wins.
func TestApplyEffortModeCodeSource(t *testing.T) {
	cfg := testConfig()
	cfg.ThinkingModes = map[string]int{"idA": 3}
	s := newTestServer(t, cfg)
	cat := &gemini.AvailableModel{Name: "a", ModelID: "idA", ModeCode: 8}
	old := &gemini.AvailableModel{Name: "a", ModelID: "idA"}
	var p gemini.GenParams
	s.applyEffort(&p, cat, "low", "")
	if p.Level == 0 || p.Mode != 8 {
		t.Errorf("catalogue code not used: %+v", p)
	}
	p = gemini.GenParams{}
	s.applyEffort(&p, old, "low", "")
	if p.Mode != 3 {
		t.Errorf("fallback table not used: %+v", p)
	}
	cfg.ThinkingModesExplicit = true
	p = gemini.GenParams{}
	s.applyEffort(&p, cat, "low", "")
	if p.Mode != 3 {
		t.Errorf("explicit table should win: %+v", p)
	}
}
