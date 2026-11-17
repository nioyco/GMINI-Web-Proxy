package apiconv

import (
	"strings"
	"testing"
)

// feed pushes s rune by rune and returns everything released.
func feed(g *ProgressGate, s string) string {
	var out strings.Builder
	for _, r := range s {
		out.WriteString(g.Push(string(r)))
	}
	return out.String()
}

func TestProgressGate(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text flows", "Bonjour, voici la réponse.", "Bonjour, voici la réponse."},
		{"tool_call fence is held", "Je regarde.\n\n```tool_call\n{\"name\":\"x\",\"arguments\":{}}\n```", "Je regarde.\n\n"},
		{"untagged fence is held", "Voici :\n```\n{\"name\":\"x\",\"arguments\":{}}\n```", "Voici :\n"},
		{"json fence is held", "A\n```json\n{\"name\":\"x\",\"arguments\":{}}\n```", "A\n"},
		{"only a tool call", "```tool_call\n{\"name\":\"x\"}\n```", ""},
		{"bare JSON is held", "{\"name\":\"x\",\"arguments\":{}}", ""},
		{"ordinary code flows", "Code :\n```go\nfmt.Println(1)\n```\nFin.", "Code :\n```go\nfmt.Println(1)\n```\nFin."},
		{"tool call after code", "```bash\nls\n```\n```tool_call\n{\"name\":\"x\"}\n```", "```bash\nls\n```\n"},
		{"exotic non-tool tag flows", "```foo?a=1&b=2:bar baz\necho 1\n```\n```tool_call\n{\"name\":\"x\"}\n```", "```foo?a=1&b=2:bar baz\necho 1\n```\n"},
		{"four-backtick block flows", "````bash?x=1\n```inner```\n````\n```tool_call\n{\"name\":\"x\"}\n```", "````bash?x=1\n```inner```\n````\n"},
		{"inline triple backticks flow", "Utilisez (` ```tool_call ... ``` `) ici.\n```tool_call\n{\"name\":\"x\"}\n```", "Utilisez (` ```tool_call ... ``` `) ici.\n"},
		{"inline backticks flow", "Utilisez `ls` puis ``x``.", "Utilisez `ls` puis ``x``."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var g ProgressGate
			if got := feed(&g, c.in); got != c.want {
				t.Errorf("released %q, want %q", got, c.want)
			}
			var whole ProgressGate
			if got := whole.Push(c.in); got != c.want {
				t.Errorf("single push released %q, want %q", got, c.want)
			}
		})
	}
}

// Nothing released may ever belong to a tool call: the final parse of the whole
// reply must still find the call, and the released text is a prefix of the reply.
func TestProgressGateNeverLeaksToolCall(t *testing.T) {
	reply := "Je crée le fichier.\n\n```tool_call\n{\"name\": \"write\", \"arguments\": {\"c\": \"# T\\n```go\\nx\\n```\\n\"}}\n```"
	var g ProgressGate
	sent := feed(&g, reply)
	if strings.Contains(sent, "tool_call") || strings.Contains(sent, "write") {
		t.Fatalf("tool call leaked: %q", sent)
	}
	text, calls, _ := ParseToolCallsWith(reply, []DeclaredTool{{Name: "write", Props: map[string]bool{"c": true}}})
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	rest, ok := g.Rest(text)
	if !ok || rest != "" {
		t.Errorf("rest = %q ok=%v, want empty/true (sent %q, final %q)", rest, ok, sent, text)
	}
}

func TestProgressGateRest(t *testing.T) {
	var g ProgressGate
	g.Push("Début.\n```tool_call\n{}\n```\nSuite.")
	if rest, ok := g.Rest("Début.\n\nSuite."); !ok || rest != "\nSuite." {
		t.Errorf("rest = %q ok=%v", rest, ok)
	}
	var h ProgressGate
	h.Push("Texte envoyé.")
	if rest, ok := h.Rest("Autre chose."); ok || rest != "" {
		t.Errorf("mismatch must give empty/false, got %q %v", rest, ok)
	}
	var e ProgressGate
	if rest, ok := e.Rest("Tout à la fin."); !ok || rest != "Tout à la fin." {
		t.Errorf("nothing sent: rest = %q ok=%v", rest, ok)
	}
}

func TestEffort(t *testing.T) {
	for in, want := range map[string]int{"low": 1, "Medium": 4, "HIGH": 2, "": 0, "max": 0} {
		if got := EffortLevel(in); got != want {
			t.Errorf("EffortLevel(%q) = %d, want %d", in, got, want)
		}
	}
	if n, e := SplitModelEffort("gemini-detailed-research@high"); n != "gemini-detailed-research" || e != "high" {
		t.Errorf("split = %q %q", n, e)
	}
	if n, e := SplitModelEffort("model@unknown"); n != "model@unknown" || e != "" {
		t.Errorf("unknown suffix must be kept: %q %q", n, e)
	}
}
