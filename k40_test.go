package main

// K40 test suite: anonymized diagnostics "reason" and "tool names" fields,
// the GEMINI_ENFORCE_TOOL_CALLS silent-skip guard, and the IsCompaction helper.
// Everything is deterministic and offline.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

func TestK40_ActivityReason(t *testing.T) {
	cases := []struct {
		outcome string
		status  int
		want    string
	}{
		{"cancelled", 0, "annulée (arrêt proxy ou client)"},
		{"error", 500, "HTTP 500"},
		{"error", 429, "HTTP 429"},
		{"tool_calls_ok", 200, ""},
		{"text_only", 200, ""},
	}
	for _, c := range cases {
		if got := activityReason(c.outcome, c.status); got != c.want {
			t.Errorf("activityReason(%q, %d) = %q, want %q", c.outcome, c.status, got, c.want)
		}
	}
}

func TestK40_SanitizeToolNames(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := sanitizeToolNames([]string{" read_file ", "", "run_command", long, "edit_file", "write_file"})
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5 (empty dropped): %v", len(got), got)
	}
	if got[0] != "read_file" {
		t.Errorf("got[0] = %q, want read_file", got[0])
	}
	if len(got[2]) != 64 {
		t.Errorf("long name not bounded: len=%d", len(got[2]))
	}

	// Bounded to 16 entries.
	many := make([]string, 30)
	for i := range many {
		many[i] = "t" + string(rune('a'+i%26))
	}
	if got := sanitizeToolNames(many); len(got) != 16 {
		t.Errorf("bound = %d, want 16", len(got))
	}

	if got := sanitizeToolNames(nil); got != nil {
		t.Errorf("nil input = %v, want nil", got)
	}
}

func TestK40_IsCompaction(t *testing.T) {
	if !apiconv.IsCompaction("<summary>état</summary>") {
		t.Error("summary marker not detected")
	}
	if !apiconv.IsCompaction("avant\n</summary>") {
		t.Error("closing marker not detected")
	}
	if apiconv.IsCompaction("texte normal") {
		t.Error("plain text wrongly flagged as compaction")
	}
}

func TestK40_SilentSkipGuard(t *testing.T) {
	declared := []apiconv.DeclaredTool{{Name: "run_command"}}
	base := gemini.GenParams{} // Model == nil -> isSilentModelDowngrade short-circuits

	mk := func(enforce bool) *Server {
		cfg := testConfig()
		cfg.EnforceToolCalls = enforce
		return newTestServer(t, cfg)
	}

	t.Run("off", func(t *testing.T) {
		s := mk(false)
		out := streamToolsResult{rawText: "Voici ce qui reste à faire.", thoughtText: ""}
		g := s.evaluateAttemptGuards(context.Background(), base, out, declared, 0, false, false)
		if g.isSilentSkip {
			t.Fatal("silent skip triggered while GEMINI_ENFORCE_TOOL_CALLS=off")
		}
	})

	t.Run("on_text_only", func(t *testing.T) {
		s := mk(true)
		out := streamToolsResult{rawText: "Voici un résumé de l'avancement.", thoughtText: ""}
		g := s.evaluateAttemptGuards(context.Background(), base, out, declared, 0, false, false)
		if !g.isSilentSkip {
			t.Fatal("expected silent skip for text-only reply with declared tools")
		}
	})

	t.Run("on_with_tool_call", func(t *testing.T) {
		s := mk(true)
		out := streamToolsResult{rawText: "```tool_call\n{\"name\":\"run_command\",\"arguments\":{}}\n```"}
		g := s.evaluateAttemptGuards(context.Background(), base, out, declared, 0, false, false)
		if g.isSilentSkip {
			t.Fatal("silent skip triggered despite a tool_call block")
		}
	})

	t.Run("on_compaction", func(t *testing.T) {
		s := mk(true)
		out := streamToolsResult{rawText: "<summary>état compacté</summary>"}
		g := s.evaluateAttemptGuards(context.Background(), base, out, declared, 0, false, false)
		if g.isSilentSkip {
			t.Fatal("silent skip triggered for a compaction")
		}
	})
}

func TestK40_IncidentReasonPersistedAndSanitized(t *testing.T) {
	cfg := testConfig()
	s := newTestServer(t, cfg)
	now := time.Now()

	s.recordDiagIncident(context.Background(), "tool_refusal", "reported", "refus/simulation d'outils\navec saut de ligne", false)

	resp := s.diag.query(diagQuery{Kind: "incident", Window: "all", Limit: 10}, now)
	if resp.Total != 1 {
		t.Fatalf("incident total = %d, want 1", resp.Total)
	}
	it := resp.Items[0]
	if it.Type != "tool_refusal" || it.Detail != "reported" {
		t.Errorf("incident type/detail = %q/%q, want tool_refusal/reported", it.Type, it.Detail)
	}
	if it.Reason != "refus/simulation d'outils avec saut de ligne" {
		t.Errorf("reason = %q, want newline collapsed", it.Reason)
	}
	if strings.Contains(it.Reason, "\n") {
		t.Errorf("reason still contains newline: %q", it.Reason)
	}
}
