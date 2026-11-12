package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripEcho(t *testing.T) {
	cases := []struct {
		name, in, want string
		stripped, fab  int
	}{
		{"plain", "Bonjour, voici la réponse.", "Bonjour, voici la réponse.", 0, 0},
		{"brackets normal", "Voir [1] et la liste [a, b] ou arr[0]: ok", "Voir [1] et la liste [a, b] ou arr[0]: ok", 0, 0},
		{"label mid sentence", "Le format est [Assistant]: suivi du texte.", "Le format est [Assistant]: suivi du texte.", 0, 0},
		{"code block keeps labels", "Exemple:\n```\n[Assistant]: hi\n[Tool result for x]: 1\n```\nfin", "Exemple:\n```\n[Assistant]: hi\n[Tool result for x]: 1\n```\nfin", 0, 0},
		{"assistant label only", "[Assistant]: Voici la réponse", "Voici la réponse", 1, 0},
		{"tool result block", "Avant\n[Tool result for call_ab12cd34]: {\"a\":1}\nsuite json\n\nAprès", "Avant\nAprès", 1, 1},
		{"user and system blocks", "[User]: question\n\n[System instruction]: règles\n\nRéponse", "Réponse", 2, 0},
		{"midline call marker", "ok {\"x\":1} [Tool result for call_0123abcd]: {\"y\":2}\nfin", "ok {\"x\":1} ", 1, 1},
		{"stray label lines", "[Assistant]:\n\n[Tool result for call_fca76f45]: चयन: {\"output\":\"x\"}\n\n[Tool result for call_fca76f45]: चयन: {\"output\":\"x\"}\n\nRéponse finale", "\nRéponse finale", 3, 2},
	}
	for _, c := range cases {
		got, st := StripEcho(c.in)
		if got != c.want || st.Stripped != c.stripped || st.Fabricated != c.fab {
			t.Errorf("%s: got %q %+v; want %q (%d,%d)", c.name, got, st, c.want, c.stripped, c.fab)
		}
	}
}

// Streaming in arbitrary chunk sizes must give the same result as one shot.
func TestEchoFilterChunkingEquivalence(t *testing.T) {
	inputs := []string{
		"Avant\n[Tool result for call_ab12cd34]: {\"a\":1}\nsuite\n\nAprès [1] fin\n",
		"[Assistant]: Salut\n```go\n[User]: x\n```\n[User]: q\n\nok",
		"texte [Tool result for call_0123abcd]: {\"y\":2}\nfin [Tool result for no",
		"  \n[Assistant]:\n[Tool result for call_fca76f45]: x",
	}
	for _, in := range inputs {
		want, wst := StripEcho(in)
		for size := 1; size <= 7; size++ {
			var f EchoFilter
			var sb strings.Builder
			for i := 0; i < len(in); i += size {
				j := i + size
				if j > len(in) {
					j = len(in)
				}
				sb.WriteString(f.Push(in[i:j]))
			}
			sb.WriteString(f.Flush())
			if sb.String() != want || f.Stats != wst {
				t.Errorf("size %d on %q: got %q %+v want %q %+v", size, in, sb.String(), f.Stats, want, wst)
			}
		}
	}
}

// Regression: multi-turn history with tool results, simulated model echoing it.
func TestEchoRegressionFromHistory(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "system", Content: jsonString("Tu es un agent.")},
			{Role: "user", Content: jsonString("Liste le dossier")},
			{Role: "assistant", Content: jsonString(""), ToolCalls: []ToolCall{{ID: "call_fca76f45", Type: "function", Function: ToolCallFunction{Name: "run", Arguments: `{"cmd":"ls"}`}}}},
			{Role: "tool", ToolCallID: "call_fca76f45", Content: jsonString("total 16")},
		},
		Tools: []Tool{{Type: "function", Function: &ToolFunction{Name: "run", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	prompt, _, _ := req.Prompt()
	if !strings.Contains(prompt, "[Tool result for") {
		t.Fatalf("history not flattened as expected: %s", prompt)
	}
	simulated := "[Assistant]:\n\n[Tool result for call_fca76f45]: चयन: {\"output\":\"total 16\"}\n\n" +
		"Voici le résultat.\n```tool_call\n{\"name\":\"run\",\"arguments\":{\"cmd\":\"pwd\"}}\n```"
	text, calls := ParseToolCalls(simulated)
	if len(calls) != 1 || calls[0].Function.Name != "run" {
		t.Fatalf("real tool call must survive: %+v", calls)
	}
	clean, st := StripEcho(text)
	if strings.Contains(clean, "[Tool result") || strings.Contains(clean, "[Assistant]") || strings.Contains(clean, "चयन") {
		t.Errorf("echo not stripped: %q", clean)
	}
	if !strings.Contains(clean, "Voici le résultat.") {
		t.Errorf("legit text lost: %q", clean)
	}
	if st.Fabricated != 1 {
		t.Errorf("fabricated = %d, want 1", st.Fabricated)
	}
}

func TestGoogleResponseExStripsEchoKeepsCall(t *testing.T) {
	text := "[Tool result for call_ab12cd34]: {\"a\":1}\n\nTexte\n```function_call\n{\"name\":\"f\",\"args\":{}}\n```"
	_, st := GoogleResponseEx("m", "p", text, true)
	if st.Fabricated != 1 {
		t.Fatalf("stats %+v", st)
	}
	b, _ := json.Marshal(GoogleResponse("m", "p", text, true))
	if strings.Contains(string(b), "Tool result") || !strings.Contains(string(b), `"name":"f"`) {
		t.Errorf("unexpected: %s", b)
	}
}

func TestToolPromptsForbidEcho(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{{Role: "user", Content: jsonString("x")}},
		Tools:    []Tool{{Type: "function", Function: &ToolFunction{Name: "run", Parameters: json.RawMessage(`{}`)}}},
	}
	p, _, _ := req.Prompt()
	g := buildGoogleToolPrompt([]googleToolDef{{Name: "f"}})
	for name, s := range map[string]string{"openai": p, "google": g} {
		for _, want := range []string{"NEVER write them", "NEVER repeat or replay", "simulate a tool result"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s prompt missing %q", name, want)
			}
		}
	}
}
