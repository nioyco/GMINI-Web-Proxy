package main

import (
	"context"
	"strings"
	"testing"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

func TestIsUpstreamGlitch(t *testing.T) {
	yes := []string{
		"I can't use Workspace because required Gmail settings are off. Turn on these settings to continue.",
		`<GenerateWidget component_placeholder_id="GenerateWidget_c_7b_r_56_0" type="artifact"/>` + "\n",
		"I'm having a hard time fulfilling your request. Can I help you with something else instead?",
	}
	no := []string{"", "Voici le rapport.", "```tool_call\n{\"name\":\"a\",\"arguments\":{}}\n```",
		"Le texte <GenerateWidget x/> est cité ici dans une phrase."}
	for _, s := range yes {
		if !apiconv.IsUpstreamGlitch(s) {
			t.Errorf("not detected: %q", s)
		}
	}
	for _, s := range no {
		if apiconv.IsUpstreamGlitch(s) {
			t.Errorf("wrongly detected: %q", s)
		}
	}
}

// Real replies of 2026-11-14: a canned non-answer never reaches the client; the
// request is retried once; a second non-answer is explained in clear terms.
func TestGlitchRetriedOnce(t *testing.T) {
	const ws = "I can't use Workspace because required Gmail settings are off. Turn on these settings to continue."
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text(ws)
		return nil
	})
	calls := 0
	f.srv.generate = func(context.Context, gemini.GenParams) (string, error) {
		calls++
		return "Voici la vraie réponse.", nil
	}
	c := readSSE(t, context.Background(), f.url, body(""))
	all := strings.Join(c.dataFrames, "\n")
	if calls != 1 || strings.Contains(all, "Workspace") || !strings.Contains(all, "Voici la vraie réponse.") {
		t.Fatalf("calls=%d frames=%v", calls, c.dataFrames)
	}

	g := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text(`<GenerateWidget component_placeholder_id="x" type="artifact"/>`)
		return nil
	})
	g.srv.generate = func(context.Context, gemini.GenParams) (string, error) {
		return `<GenerateWidget component_placeholder_id="y" type="artifact"/>`, nil
	}
	c = readSSE(t, context.Background(), g.url, body(""))
	all = strings.Join(c.dataFrames, "\n")
	if strings.Contains(all, "GenerateWidget") || !strings.Contains(all, "GMINI") {
		t.Fatalf("frames=%v", c.dataFrames)
	}
}

// A short normal answer is delivered in full although its start was held back.
func TestShortAnswerStillDelivered(t *testing.T) {
	f := progressServer(t, func(_ context.Context, _ gemini.GenParams, h gemini.StreamHandlers) error {
		h.Text("Il est ")
		h.Text("8h07.")
		return nil
	})
	c := readSSE(t, context.Background(), f.url, body(""))
	var sb strings.Builder
	for _, v := range views(t, c) {
		if v.Choices[0].Delta.Content != nil {
			sb.WriteString(*v.Choices[0].Delta.Content)
		}
	}
	if sb.String() != "Il est 8h07." {
		t.Fatalf("text=%q", sb.String())
	}
}
