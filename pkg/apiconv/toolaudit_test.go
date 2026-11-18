package apiconv

import (
	"encoding/json"
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
	trace2302 := "Voici le fichier `/Users/nioyco/Documents/CONTRE_REVUE_INDEPENDANTE.md` entièrement mis à jour avec les preuves exactes :\n\n```markdown\n# Contre-revue\n```"
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
