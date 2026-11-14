package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// fakeStream builds a StreamGenerate body: one frame, with or without thoughts
// (thoughts sit at index 37 of the first candidate).
func fakeStream(text, thought string) string {
	cand := make([]any, 38)
	cand[0] = "rc_x"
	cand[1] = []any{text}
	if thought != "" {
		cand[37] = []any{[]any{thought}}
	}
	ib, _ := json.Marshal([]any{nil, nil, nil, nil, []any{cand}})
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line) + "\n"
}

// A model that usually streams thoughts and suddenly does not: the reply carries
// a warning (as reasoning text) and the answer itself is untouched.
func TestWarnSuspectFallbackInStream(t *testing.T) {
	cfg := testConfig()
	cfg.StreamToolsProgress, cfg.StreamThoughts, cfg.WarnSuspectFallback = true, true, true
	cfg.SSEKeepaliveSec = 1
	client, err := gemini.New(cfg.Gemini, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	client.SetModels(testModels())
	thought := "je réfléchis"
	client.SetTransport(rtFn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fakeStream("Réponse.", thought)))}, nil
	}))
	s := newServer(cfg, client, testLogger())
	body := `{"model":"gemini-3.1-pro","stream":true,"messages":[{"role":"user","content":"x"}],
	 "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}}]}`
	call := func() string {
		rec := post(s, "/v1/chat/completions", body)
		if rec.Code != 200 {
			t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	for i := 0; i < 2; i++ {
		if out := call(); strings.Contains(out, "GMINI") {
			t.Fatalf("warning on a normal reply: %s", out)
		}
	}
	thought = ""
	out := call()
	if !strings.Contains(out, "GMINI") || !strings.Contains(out, "reasoning_content") || !strings.Contains(out, "Réponse.") {
		t.Fatalf("no warning on a reply without thoughts: %s", out)
	}
	cfg.WarnSuspectFallback = false
	if out := call(); strings.Contains(out, "GMINI") {
		t.Fatalf("warning although disabled")
	}
}
