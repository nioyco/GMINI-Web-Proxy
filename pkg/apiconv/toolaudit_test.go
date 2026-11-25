package apiconv

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestK20_ExtractConversationToolFacts(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"First task"`)},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "read_file", Arguments: `{"Path":"/a/b.go"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"ok"`)},
		{Role: "user", Content: json.RawMessage(`"Second task"`)},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "c2", Type: "function", Function: ToolCallFunction{Name: "run_command", Arguments: `{"CommandLine":"go test ./..."}`}},
			{ID: "c3", Type: "function", Function: ToolCallFunction{Name: "write_file", Arguments: `{"Path":"/tmp/CONTRE_REVUE_INDEPENDANTE.md"}`}},
		}},
		{Role: "tool", ToolCallID: "c2", Content: json.RawMessage(`"ok"`)},
		{Role: "tool", ToolCallID: "c3", Content: json.RawMessage(`"ok"`)},
	}

	facts := ExtractConversationToolFacts(msgs)
	if facts.Total.ReadCount != 1 || facts.Total.CommandCount != 1 || facts.Total.WriteCount != 1 {
		t.Fatalf("Total = %+v", facts.Total)
	}
	if facts.SinceLastUser.ReadCount != 0 || facts.SinceLastUser.CommandCount != 1 || facts.SinceLastUser.WriteCount != 1 {
		t.Fatalf("SinceLastUser = %+v", facts.SinceLastUser)
	}
	if len(facts.SinceLastUser.WriteFiles) != 1 || facts.SinceLastUser.WriteFiles[0] != "CONTRE_REVUE_INDEPENDANTE.md" {
		t.Fatalf("WriteFiles = %v", facts.SinceLastUser.WriteFiles)
	}
}

func TestK20_DetectUnverifiedActionClaim(t *testing.T) {
	declared := []DeclaredTool{
		{Name: "read_file", Props: map[string]bool{"Path": true}},
		{Name: "write_file", Props: map[string]bool{"Path": true, "Content": true}},
		{Name: "run_command", Props: map[string]bool{"CommandLine": true}},
	}
	none := ToolTurnSummary{}
	withWrite := ToolTurnSummary{WriteCount: 1}
	withCmd := ToolTurnSummary{CommandCount: 1}

	// Real trace 23:02:02
	trace2302 := "Voici le fichier `/Users/demo/Documents/CONTRE_REVUE_INDEPENDANTE.md` entièrement mis à jour avec les preuves exactes :\n\n```markdown\n# Contre-revue\n```"
	if !IsEarlyFakeWriteOpener(trace2302, declared, none) {
		t.Error("expected IsEarlyFakeWriteOpener=true on trace 23:02:02")
	}
	if kind, ok := DetectUnverifiedActionClaim(trace2302, declared, none); !ok || kind != "write_without_tool" {
		t.Errorf("trace 23:02:02 got (%q, %v), want (write_without_tool, true)", kind, ok)
	}
	// If write_file DID run since last user message -> no false positive!
	if _, ok := DetectUnverifiedActionClaim(trace2302, declared, withWrite); ok {
		t.Error("false positive when write_file actually ran")
	}

	// Real trace 23:25:54
	trace2325 := "C'est fait ! J'ai terminé la lecture des derniers fichiers et j'ai bien exécuté l'écriture sur le disque. Le livrable de contre-revue est maintenant parfaitement à jour sur votre machine !"
	if kind, ok := DetectUnverifiedActionClaim(trace2325, declared, none); !ok || kind != "write_without_tool" {
		t.Errorf("trace 23:25:54 got (%q, %v), want (write_without_tool, true)", kind, ok)
	}

	// Fake command claim
	fakeCmd := "J'ai bien lancé les tests `go test` et tout est vert."
	if kind, ok := DetectUnverifiedActionClaim(fakeCmd, declared, none); !ok || kind != "command_without_tool" {
		t.Errorf("fakeCmd got (%q, %v), want (command_without_tool, true)", kind, ok)
	}
	if _, ok := DetectUnverifiedActionClaim(fakeCmd, declared, withCmd); ok {
		t.Error("false positive when run_command actually ran")
	}

	// Hypothetical / conditional phrasing must NOT trigger
	conditional := "Je peux mettre à jour le fichier `/tmp/foo.md` si tu veux."
	if _, ok := DetectUnverifiedActionClaim(conditional, declared, none); ok {
		t.Error("false positive on conditional sentence")
	}
}

func TestK20_SplitPrematureReadWriteBatch(t *testing.T) {
	// Real trace 23:07:03: read_file + write_file together -> defer write_file
	batch2307 := []ToolCall{
		{ID: "1", Function: ToolCallFunction{Name: "read_file", Arguments: `{"Path":"a.go"}`}},
		{ID: "2", Function: ToolCallFunction{Name: "read_file", Arguments: `{"Path":"b.js"}`}},
		{ID: "3", Function: ToolCallFunction{Name: "write_file", Arguments: `{"Path":"CONTRE_REVUE.md"}`}},
	}
	kept, deferred := SplitPrematureReadWriteBatch(batch2307)
	if deferred != 1 || len(kept) != 2 || kept[0].Function.Name != "read_file" || kept[1].Function.Name != "read_file" {
		t.Fatalf("23:07:03 split = kept:%+v deferred:%d", kept, deferred)
	}

	// Real trace 23:53:36: run_command + edit_file + read_file -> must stay untouched
	batch2353 := []ToolCall{
		{ID: "1", Function: ToolCallFunction{Name: "run_command", Arguments: `{"CommandLine":"git diff"}`}},
		{ID: "2", Function: ToolCallFunction{Name: "edit_file", Arguments: `{"Path":"server.go"}`}},
		{ID: "3", Function: ToolCallFunction{Name: "read_file", Arguments: `{"Path":"adr.md"}`}},
	}
	kept2, deferred2 := SplitPrematureReadWriteBatch(batch2353)
	if deferred2 != 0 || len(kept2) != 3 {
		t.Fatalf("23:53:36 edit batch should not split: kept:%d deferred:%d", len(kept2), deferred2)
	}
}

// K36: a reply that merely QUOTES code ("file created" in a fence or in inline
// backticks) must not be mistaken for an unverified action claim; a real prose
// claim outside code must still be caught.
func TestK36_DetectUnverifiedActionClaimIgnoresCode(t *testing.T) {
	declared := []DeclaredTool{
		{Name: "read_file", Props: map[string]bool{"Path": true}},
		{Name: "write_file", Props: map[string]bool{"Path": true, "Content": true}},
		{Name: "run_command", Props: map[string]bool{"CommandLine": true}},
	}
	none := ToolTurnSummary{}

	// (1) true positive: the claim is plain prose, no code span involved.
	for _, prose := range []string{
		"Le fichier a été créé sur le disque.",
		"J'ai créé le fichier `a.go` sur le disque.",
	} {
		if kind, ok := DetectUnverifiedActionClaim(prose, declared, none); !ok || kind != "write_without_tool" {
			t.Errorf("%q got (%q, %v), want (write_without_tool, true)", prose, kind, ok)
		}
	}

	// (2) false positive: the claim only appears inside a fenced code block and
	// the surrounding prose carries no other claim.
	for _, fenced := range []string{
		"Voici le message d'erreur exact du build :\n```\nfile created\n```\nRien d'autre à signaler.",
		"Le journal affichait :\n```text\nle fichier a été créé sur le disque\nJ'ai créé le fichier a.go\n```\nFin du journal.",
		"Le journal montrait :\n```go\n// I have created the file\n```\nRien d'autre.",
	} {
		if kind, ok := DetectUnverifiedActionClaim(fenced, declared, none); ok {
			t.Errorf("fenced claim wrongly flagged as (%q, true): %q", kind, fenced)
		}
	}

	// (3) false positive: inline backtick span only.
	for _, inline := range []string{
		"Le texte `the file created` vient du journal.",
		"Le libellé `j'ai créé le fichier` est celui de l'outil.",
		"Il affichait `I have created the file` à l'écran.",
	} {
		if kind, ok := DetectUnverifiedActionClaim(inline, declared, none); ok {
			t.Errorf("inline-code claim wrongly flagged as (%q, true): %q", kind, inline)
		}
	}

	// (4) command claims: outside code -> flagged; same sentence inside a code
	// block -> not flagged.
	cmdProse := "I ran the tests."
	if kind, ok := DetectUnverifiedActionClaim(cmdProse, declared, none); !ok || kind != "command_without_tool" {
		t.Errorf("cmdProse got (%q, %v), want (command_without_tool, true)", kind, ok)
	}
	cmdFenced := "Extrait du journal :\n```\nI ran the tests.\n```\nVoilà."
	if kind, ok := DetectUnverifiedActionClaim(cmdFenced, declared, none); ok {
		t.Errorf("fenced command claim wrongly flagged as (%q, true): %q", kind, cmdFenced)
	}
}

// K36: stripCodeSpans drops fences and inline spans (delimiters included) and
// keeps the prose; an unterminated fence drops the rest.
func TestK36_StripCodeSpans(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no_code", "Le fichier a été créé.", "Le fichier a été créé."},
		{"fenced", "avant\n```go\nfile created\n```\naprès", "avant\n\naprès"},
		{"fenced_no_lang", "a```b\nc```d", "ad"},
		{"inline", "avant `the file created` après", "avant  après"},
		{"unterminated_fence", "avant\n```\nfile created", "avant\n"},
		{"unterminated_inline", "avant `the file created", "avant "},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := stripCodeSpans(tc.in); got != tc.want {
			t.Errorf("%s: stripCodeSpans(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	if got := stripCodeSpans("```\nfile created\n```"); strings.TrimSpace(got) != "" {
		t.Errorf("fence-only input must strip to whitespace, got %q", got)
	}
}
