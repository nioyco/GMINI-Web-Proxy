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
	resp, st := GoogleResponseEx("m", "p", text, true)
	if st.Fabricated != 1 {
		t.Fatalf("stats %+v", st)
	}
	b, _ := json.Marshal(resp)
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

// R7: a tool turn is rendered in the flattened history as the assistant call
// (fenced, with name and arguments) followed by the labelled result.
func TestToolTurnRenderedInHistory(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: jsonString("liste")},
			{Role: "assistant", Content: jsonString(""), ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "run", Arguments: `{"cmd":"ls"}`}}}},
			{Role: "tool", ToolCallID: "call_1", Content: jsonString("a.txt")},
		},
		Tools: []Tool{{Type: "function", Function: &ToolFunction{Name: "run", Parameters: json.RawMessage(`{}`)}}},
	}
	p, _, _ := req.Prompt()
	call := strings.Index(p, "```tool_call\n{\"name\": \"run\", \"arguments\": {\"cmd\":\"ls\"}}")
	res := strings.Index(p, "[Tool result for run]: a.txt")
	if call < 0 || res < 0 || call > res {
		t.Errorf("tool turn not rendered in order (call=%d result=%d):\n%s", call, res, p)
	}
}

func TestN1_IsUpstreamCannedError(t *testing.T) {
	yes := []string{
		"I seem to be encountering an error. Can I try something else for you?",
		"I'm having a hard time fulfilling your request. Can I help you with something else instead?",
		"I encountered an error doing what you asked. Could you try again?",
		"Sorry, something went wrong. Please try your request again.",
		"Je ne suis qu'une IA basée sur du texte. Je ne peux pas vous aider à ce sujet.",
		"Je ne suis qu’une IA basée sur du texte. Je ne peux pas vous aider à ce sujet.",
		"Ce que vous demandez va au-delà de ce que permet ma programmation. Je génère juste du texte.",
	}
	for _, s := range yes {
		if !IsUpstreamCannedError(s) {
			t.Errorf("IsUpstreamCannedError(%q) = false, want true", s)
		}
		if IsUpstreamGlitch(s) {
			t.Errorf("IsUpstreamGlitch(%q) = true, want false", s)
		}
	}
	no := []string{
		"",
		"OK.",
		"I encountered an error doing what you asked because the file was locked, here is the fix.",
		"Sorry, something went wrong in test_foo.go line 42: unexpected EOF.",
		"I'm having a hard time fulfilling your request because the directory is missing.",
		"```text\nSorry, something went wrong. Please try your request again.\n```",
		"I encountered an error doing what you asked. " + strings.Repeat("x", 220),
	}
	for _, s := range no {
		if IsUpstreamCannedError(s) {
			t.Errorf("IsUpstreamCannedError(%q) = true, want false", s)
		}
	}

	// K15 Q4: inner[31]=true catches short non-fenced canned errors regardless of wording,
	// while never flagging long replies, fenced replies, or widgets.
	if !IsUpstreamCannedErrorWithMarker("Erreur générique inconnue.", true) {
		t.Errorf("short reply with errorMarker=true must match")
	}
	if IsUpstreamCannedErrorWithMarker(strings.Repeat("a", 260), true) {
		t.Errorf("long reply (>250 bytes) with errorMarker=true must not match")
	}
	if IsUpstreamCannedErrorWithMarker("```markdown\nhello\n```", true) {
		t.Errorf("fenced reply with errorMarker=true must not match")
	}
	if IsUpstreamCannedErrorWithMarker(`<GenerateWidget component_placeholder_id="x" type="artifact"/>`, true) {
		t.Errorf("widget glitch with errorMarker=true must stay glitch, not canned error")
	}
}

func TestSanitizeAssistantHistory(t *testing.T) {
	canned := "I seem to be encountering an error. Can I try something else for you?"
	gotCanned := SanitizeAssistantHistory(canned)
	if strings.Contains(gotCanned, "encountering an error") || !strings.Contains(gotCanned, "transient error") {
		t.Errorf("canned error not neutralized: %q", gotCanned)
	}

	notice := "GMINI : Gemini s'est figé en cours d'appel d'outil (537 o reçus sans suite) ; demande interrompue."
	gotNotice := SanitizeAssistantHistory(notice)
	if strings.Contains(gotNotice, "GMINI :") || !strings.Contains(gotNotice, "transient error") {
		t.Errorf("GMINI notice not neutralized: %q", gotNotice)
	}

	refusalWithRecap := "Le quota d'appels d'outils pour ce tour est épuisé (`0` appel restant). Voici l'état exact atteint :\n\n### Fichiers déjà créés\n- `routes.go`"
	gotRefusal := SanitizeAssistantHistory(refusalWithRecap)
	if strings.Contains(gotRefusal, "quota d'appels d'outils") || strings.Contains(gotRefusal, "0` appel restant") {
		t.Errorf("refusal line leaked after sanitize: %q", gotRefusal)
	}
	if !strings.Contains(gotRefusal, "NO quota or call limit") || !strings.Contains(gotRefusal, "### Fichiers déjà créés\n- `routes.go`") {
		t.Errorf("sanitize did not preserve factual recap or add corrective note: %q", gotRefusal)
	}

	refusalOnly := "Le quota d'appels d'outils est épuisé (`0` appel restant)."
	gotOnly := SanitizeAssistantHistory(refusalOnly)
	if gotOnly != "[Previous turn was interrupted before calling tools; all declared tools are active with NO quota or call limit.]" {
		t.Errorf("refusal-only sanitize = %q", gotOnly)
	}

	normal := "J'ai terminé l'analyse. " + strings.Repeat("Détail technique. ", 80) + "\nLe mot quota apparaît ici loin du début."
	if got := SanitizeAssistantHistory(normal); got != normal {
		t.Errorf("normal assistant turn modified: got len=%d want len=%d", len(got), len(normal))
	}
}

func TestMessagesAndGoogleContentsSanitizeHistoryAndAntiQuotaReminder(t *testing.T) {
	refusal := "Le quota d'appels d'outils pour ce tour est épuisé (`0` appel restant). Voici le résumé :\n### État\n- `app.go` créé"
	gminiNotice := "GMINI : Gemini s'est figé en cours d'appel d'outil (1 Ko reçus sans suite) ; demande interrompue."
	canned := "I seem to be encountering an error. Can I try something else for you?"

	req := ChatRequest{
		Messages: []Message{
			{Role: "user", Content: jsonString("continue")},
			{Role: "assistant", Content: jsonString(canned)},
			{Role: "assistant", Content: jsonString(gminiNotice)},
			{Role: "assistant", Content: jsonString(refusal)},
			{Role: "user", Content: jsonString("fais la suite")},
		},
		Tools: []Tool{{Type: "function", Function: &ToolFunction{Name: "write_file", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	}
	prompt, _, active := req.Prompt()
	if !active {
		t.Fatal("expected toolsActive=true")
	}
	for _, bad := range []string{"encountering an error", "GMINI : Gemini s'est figé", "quota d'appels d'outils", "`0` appel restant"} {
		if strings.Contains(prompt, bad) {
			t.Errorf("OpenAI prompt still contains poisoned history %q", bad)
		}
	}
	if !strings.Contains(prompt, "- `app.go` créé") {
		t.Errorf("OpenAI prompt dropped factual recap from refusal turn")
	}
	if !strings.Contains(prompt, "NO tool call limit or quota per turn") {
		t.Errorf("OpenAI prompt missing anti-quota reminder at end")
	}

	// tool_choice="none" keeps normal assistant content untouched and omits the reminder.
	reqNone := ChatRequest{
		Messages: []Message{
			{Role: "assistant", Content: jsonString(refusal)},
			{Role: "user", Content: jsonString("résume")},
		},
		Tools:      req.Tools,
		ToolChoice: json.RawMessage(`"none"`),
	}
	pNone, _, activeNone := reqNone.Prompt()
	if activeNone || strings.Contains(pNone, "NO tool call limit or quota per turn") || !strings.Contains(pNone, "quota d'appels d'outils") {
		t.Errorf("tool_choice=none behavior unexpected: active=%v", activeNone)
	}

	gReq := GenerateContentRequest{
		Contents: []GoogleContent{
			{Role: "user", Parts: []GooglePart{{Text: "go"}}},
			{Role: "model", Parts: []GooglePart{{Text: gminiNotice}}},
			{Role: "model", Parts: []GooglePart{{Text: refusal}}},
			{Role: "user", Parts: []GooglePart{{Text: "suite"}}},
		},
		Tools: []GoogleTool{{FunctionDeclarations: []GoogleFunctionDecl{{Name: "write_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}},
	}
	gPrompt, _, gActive := gReq.Prompt()
	if !gActive {
		t.Fatal("expected google toolsActive=true")
	}
	for _, bad := range []string{"GMINI : Gemini s'est figé", "quota d'appels d'outils", "`0` appel restant"} {
		if strings.Contains(gPrompt, bad) {
			t.Errorf("Google prompt still contains poisoned history %q", bad)
		}
	}
	if !strings.Contains(gPrompt, "- `app.go` créé") || !strings.Contains(gPrompt, "NO tool call limit or quota per turn") {
		t.Errorf("Google prompt missing preserved recap or anti-quota reminder")
	}
}

func TestK32_GlitchCanvaAndHistorySanitization(t *testing.T) {
	canvaReplies := []string{
		"Sure. First, I'll need your permission to turn on the Canva app.",
		"I need your permission to turn on the Spotify app.",
		"J'ai besoin de votre autorisation d'activer l'application Canva.",
		"Merci de donner votre autorisation d’activer l'application partenaire.",
	}
	for _, s := range canvaReplies {
		if !IsUpstreamGlitch(s) {
			t.Errorf("IsUpstreamGlitch(%q) = false, want true", s)
		}
		got := SanitizeAssistantHistory(s)
		if strings.Contains(got, "permission") || strings.Contains(got, "autorisation") || strings.Contains(got, "Canva") || !strings.Contains(got, "transient error") {
			t.Errorf("SanitizeAssistantHistory(%q) = %q, expected transient error replacement", s, got)
		}
	}

	longProse := strings.Repeat("Documentation normale. ", 20) + "Mention permission to turn on the Canva app in docs."
	if IsUpstreamGlitch(longProse) {
		t.Errorf("long prose (>400B) should not trigger IsUpstreamGlitch")
	}

	tailRefusalHistory := "### Fichiers déjà créés\n- `pkg/a.go` ok\n\n" +
		strings.Repeat("Ligne de bilan technique.\n", 75) +
		"\nEnvoie simplement `continue` pour poursuivre."
	sanitized := SanitizeAssistantHistory(tailRefusalHistory)
	if strings.Contains(sanitized, "Envoie simplement") {
		t.Errorf("tail refusal line not stripped by SanitizeAssistantHistory: %q", sanitized)
	}
	if !strings.Contains(sanitized, "NO quota or call limit") || !strings.Contains(sanitized, "- `pkg/a.go` ok") {
		t.Errorf("SanitizeAssistantHistory lost recap or corrective note: %q", sanitized)
	}
}

func TestK32_IsUpstreamToolRefusalHeadTailAndZeroFalsePositives(t *testing.T) {
	yes := []struct {
		name string
		text string
	}{
		{"mixed_0_remaining_fr", "Statut : `0 remaining` appels d'outils pour ce tour."},
		{"a_zero_0_restants", "Le budget d'outils est à zéro (0 restants) pour ce tour."},
		{"0_restants_bare", "Il y a 0 restants pour cette requête."},
		{"tail_continue_fr_2000B", strings.Repeat("État vérifié dans le module.\n", 68) + "Envoie simplement `continue` pour poursuivre."},
		{"tail_relance_session_2920B", strings.Repeat("Analyse des fichiers terminée.\n", 94) + "Merci de relancer une nouvelle session."},
		{"tail_continue_en_2000B", strings.Repeat("Verified report section.\n", 80) + "Reply `continue` to proceed with the remaining edits."},
		{"tail_send_continue_en", strings.Repeat("Checklist line completed.\n", 75) + "Send \"continue\" to finish the task."},
	}
	for _, tc := range yes {
		if !IsUpstreamToolRefusal(tc.text) {
			t.Errorf("%s (len=%d): expected IsUpstreamToolRefusal=true", tc.name, len(tc.text))
		}
	}

	no := []struct {
		name string
		text string
	}{
		{
			name: "summary_compaction_with_keywords",
			text: "<summary>\n# Previous Session Summary\nFixed quota d'appels d'outils épuisé and `0 remaining` appels d'outils.\nAlso discussed relancez une nouvelle session and Envoie simplement `continue` pour poursuivre.\n</summary>",
		},
		{
			name: "final_report_with_0_remaining_warnings_and_action_limit",
			text: strings.Repeat("## Section vérifiée\nTout a été écrit via write_file.\n", 35) +
				"Tests OK: 0 remaining warnings, 0 remaining errors.\n" +
				"Remarque sur le quota d'appels d'outils, action limit et tool quota dans la doc. Verdict : Commit possible.",
		},
	}
	for _, tc := range no {
		if IsUpstreamToolRefusal(tc.text) {
			t.Errorf("%s (len=%d): false positive in IsUpstreamToolRefusal", tc.name, len(tc.text))
		}
	}
}

func TestK34_LongAndExtendedToolRefusalsAndHandoffSummary(t *testing.T) {
	long22KBHead := "L'exécution d'outils est désactivée pour ce tour (0 appel d'outil restant).\n" +
		strings.Repeat("- fichier inspecté et documenté dans le bilan.\n", 480)
	if len(long22KBHead) <= 20000 {
		t.Fatalf("expected >20KB fixture, got %d", len(long22KBHead))
	}
	long22KBTail := strings.Repeat("Ligne de résumé factuel vérifié.\n", 660) +
		"\nEnvoie « Continue » pour lancer le prochain lot."

	yesRefusal := []struct {
		name string
		text string
	}{
		{"head_22KB_disabled_tools_28b9d77f", long22KBHead},
		{"tail_22KB_continue_pour_lancer", long22KBTail},
		{"no_access_outils_locaux_79aa7eb5", "Je n'ai pas accès aux outils locaux dans cette session."},
		{"execution_outils_desactivee", "Exécution d'outils est désactivée pour ce tour (0 appel restant)."},
		{"en_tools_disabled", "Tool execution is disabled for this turn with 0 tool calls remaining."},
		{"pour_lancer_2f989f3b", strings.Repeat("État validé.\n", 180) + "Envoie « Continue » pour lancer la suite."},
		{"pour_ecrire_utf8_verb", "Envoie simplement `continue` pour écrire les tests."},
		{"en_reply_continue_to_launch", "Reply `continue` to launch the next batch."},
	}
	for _, tc := range yesRefusal {
		if !IsUpstreamToolRefusal(tc.text) {
			t.Errorf("%s (len=%d): expected IsUpstreamToolRefusal=true", tc.name, len(tc.text))
		}
	}

	summaryCompaction := "<summary>\nL'exécution d'outils est désactivée (0 appel d'outil restant).\n### Étapes restantes\n- Tester\n</summary>\n" +
		strings.Repeat("Context line.\n", 1500)
	if IsUpstreamToolRefusal(summaryCompaction) || IsPostStallHandoffSummary(summaryCompaction) {
		t.Errorf("<summary> compaction must never trigger refusal or handoff")
	}

	handoffYes := []string{
		"### État exact d'avancement\n- `routes.go` écrit\n### Étapes restantes\n- Écrire `main.go`",
		"Voici les fichiers et étapes restants à traiter :",
		"Dernières actions à exécuter avant livraison :",
		"### Prochaines étapes\n1. Créer `App.vue`",
		"Remaining steps:\n- Update router.ts",
		"Files remaining to create: index.ts, style.css",
	}
	for _, s := range handoffYes {
		if !IsPostStallHandoffSummary(s) {
			t.Errorf("expected IsPostStallHandoffSummary=true for %q", s)
		}
	}
	handoffNo := []string{
		"",
		"### État d'avancement\n```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\",\"content\":\"ok\"}}\n```",
		"J'ai terminé l'analyse complète. Verdict : Commit possible.",
	}
	for _, s := range handoffNo {
		if IsPostStallHandoffSummary(s) {
			t.Errorf("false positive IsPostStallHandoffSummary for %q", s)
		}
	}

	sanLong := SanitizeAssistantHistory(long22KBHead)
	if strings.Contains(sanLong, "exécution d'outils est désactivée") || !strings.Contains(sanLong, "NO quota or call limit") {
		t.Errorf("22KB refusal not sanitized: %q", sanLong[:200])
	}
	sanAccess := SanitizeAssistantHistory("Je n'ai pas accès aux outils locaux.")
	if strings.Contains(sanAccess, "pas accès aux outils locaux") || !strings.Contains(sanAccess, "NO quota or call limit") {
		t.Errorf("outils locaux refusal not sanitized: %q", sanAccess)
	}
	sanHandoff := SanitizeAssistantHistory("### État d'avancement\n- `a.go` créé\n### Étapes restantes\n- écrire `b.go`")
	if strings.Contains(sanHandoff, "État d'avancement") || strings.Contains(sanHandoff, "Étapes restantes") ||
		!strings.Contains(sanHandoff, "- `a.go` créé") || !strings.Contains(sanHandoff, "partial status checklist") {
		t.Errorf("handoff not sanitized cleanly: %q", sanHandoff)
	}
}

// K36: a refusal written in the MIDDLE of a long reply (far from the first and
// last 1500 bytes) must still be detected now that the whole text is scanned.
func TestK36_IsUpstreamToolRefusalMidMessage(t *testing.T) {
	mid := strings.Repeat("Ligne factuelle vérifiée.\n", 90) +
		"Je n'ai pas accès aux outils locaux dans cette session." +
		strings.Repeat("Fin de bilan factuel.\n", 90)
	if len(mid) < 3500 || len(mid) > 5000 {
		t.Fatalf("fixture len=%d, want ~4000", len(mid))
	}
	pos := strings.Index(mid, "Je n'ai pas accès")
	if pos < 1500 || len(mid)-pos < 1500 {
		t.Fatalf("refusal at %d is not mid-message (len=%d)", pos, len(mid))
	}
	if !IsUpstreamToolRefusal(mid) {
		t.Errorf("mid-message refusal (len=%d, at byte %d) not detected", len(mid), pos)
	}

	// An English mid-message refusal too.
	midEN := strings.Repeat("Verified factual line.\n", 90) +
		"I do not have access to your local filesystem in this session." +
		strings.Repeat("End of factual report.\n", 90)
	if !IsUpstreamToolRefusal(midEN) {
		t.Errorf("mid-message EN refusal (len=%d) not detected", len(midEN))
	}

	// No new false positive on the existing "no" shapes now that the whole text
	// is scanned: summary compaction and a final report mentioning the vocabulary.
	stillNo := []struct {
		name string
		text string
	}{
		{
			name: "summary_compaction_with_keywords",
			text: "<summary>\n# Previous Session Summary\nFixed quota d'appels d'outils épuisé and `0 remaining` appels d'outils.\nAlso discussed relancez une nouvelle session and Envoie simplement `continue` pour poursuivre.\n</summary>",
		},
		{
			name: "final_report_with_0_remaining_warnings_and_action_limit",
			text: strings.Repeat("## Section vérifiée\nTout a été écrit via write_file.\n", 35) +
				"Tests OK: 0 remaining warnings, 0 remaining errors.\n" +
				"Remarque sur le quota d'appels d'outils, action limit et tool quota dans la doc. Verdict : Commit possible.",
		},
		{
			name: "long_report_mentioning_limits_without_claim",
			text: strings.Repeat("La doc rappelle la limite d'appels d'outils configurable côté proxy.\n", 60) +
				"Tout est conforme.",
		},
	}
	for _, tc := range stillNo {
		if IsUpstreamToolRefusal(tc.text) {
			t.Errorf("%s (len=%d): new false positive with full-text scan", tc.name, len(tc.text))
		}
	}

	// A tool_call block still wins over any refusal wording.
	if IsUpstreamToolRefusal("Je n'ai pas accès aux outils locaux.\n```tool_call\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```") {
		t.Error("a reply carrying a tool_call block must never be a refusal")
	}
}

func TestK60_IsUpstreamToolRefusalContinueAndGreenLight(t *testing.T) {
	shouldMatch := []string{
		"Répondez simplement **« continue »** pour lancer l'écriture.",
		"Envoie simplement **«\u00a0Continue\u00a0»** pour que je poursuive.",
		"Je peux démarrer l'implémentation dès que vous me donnez le feu vert.",
	}
	for _, s := range shouldMatch {
		if !IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = false, want true", s)
		}
	}
	shouldNotMatch := []string{
		"Répondez simplement « continue » pour lancer.\n```tool_call\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"a.go\"}}\n```",
		"<summary>Répondez simplement « continue » pour lancer l'implémentation.</summary>",
	}
	for _, s := range shouldNotMatch {
		if IsUpstreamToolRefusal(s) {
			t.Errorf("IsUpstreamToolRefusal(%q) = true, want false", s)
		}
	}
}
