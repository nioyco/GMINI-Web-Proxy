package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestK25_SlugifySanitizationAndPreservation(t *testing.T) {
	cases := map[string]string{
		"Code & complex work": "gemini-code-complex-work",
		"3.1 Flash-Lite":      "gemini-3.1-flash-lite",
		"Deeper analysis":     "gemini-deeper-analysis",
		"Auto":                "gemini-auto",
		"  A & B / C -- D  ":  "gemini-a-b-c-d",
	}
	for in, want := range cases {
		if got := slugifyModelName(in); got != want {
			t.Errorf("slugifyModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestK25_ParseModelsWithMetaBody24Defensive(t *testing.T) {
	entry := func(id, label, desc, versioned, family string, mode int) []any {
		e := make([]any, 18)
		e[0], e[1], e[2], e[8], e[11], e[17] = id, label, desc, family, versioned, float64(mode)
		return e
	}
	menuItem := func(id string, mode int, family string, locked bool, levels []any) []any {
		meta := make([]any, 15)
		meta[0] = id
		meta[1] = float64(mode)
		meta[8] = family
		if locked {
			meta[14] = []any{"upgrade_required"}
		}
		if levels != nil {
			return []any{meta, levels}
		}
		return []any{meta}
	}

	t.Run("with_body24_locked_and_thinking_levels", func(t *testing.T) {
		body := make([]any, 25)
		body[15] = []any{
			entry("8c46e95b1a07cecc", "Auto", "Auto", "Auto", "", 6),
			entry("e6fa609c3fa255c0", "Neon", "4 Neon", "Deeper analysis", "Neon4p0", 3),
			entry("95a4c0abcba24364", "Code & Analysis", "4 Carbon", "Code & complex work", "Carbon4p0", 8),
		}
		body[24] = []any{
			nil,
			[]any{
				menuItem("8c46e95b1a07cecc", 6, "", false, nil),
				menuItem("e6fa609c3fa255c0", 3, "Neon4p0", false, []any{float64(1), float64(4), float64(2)}),
				menuItem("95a4c0abcba24364", 8, "Carbon4p0", true, []any{[]any{float64(1)}, []any{float64(2)}}),
			},
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		models, _, err := parseModelsWithMeta(string(raw))
		if err != nil {
			t.Fatalf("parseModelsWithMeta: %v", err)
		}
		if len(models) != 3 {
			t.Fatalf("got %d models, want 3", len(models))
		}
		byName := map[string]*AvailableModel{}
		for _, m := range models {
			byName[m.Name] = m
		}
		neon := byName["gemini-deeper-analysis"]
		if neon == nil {
			t.Fatalf("missing gemini-deeper-analysis in %+v", models)
		}
		if neon.Locked {
			t.Errorf("Neon Locked = true, want false")
		}
		if !slices.Equal(neon.ThinkingLevels, []int{1, 4, 2}) {
			t.Errorf("Neon ThinkingLevels = %v, want [1 4 2]", neon.ThinkingLevels)
		}
		carb := byName["gemini-code-complex-work"]
		if carb == nil {
			t.Fatalf("missing sanitized gemini-code-complex-work in %+v", models)
		}
		if !carb.Locked {
			t.Errorf("Carbon4p0 Locked = false, want true")
		}
		if !slices.Equal(carb.ThinkingLevels, []int{1, 2}) {
			t.Errorf("Carbon4p0 ThinkingLevels = %v, want [1 2]", carb.ThinkingLevels)
		}
	})

	t.Run("with_absent_or_malformed_body24_never_fails", func(t *testing.T) {
		body := make([]any, 25)
		body[15] = []any{
			entry("e6fa609c3fa255c0", "Neon", "4 Neon", "Deeper analysis", "Neon4p0", 3),
		}
		body[24] = "unexpected-shape"
		raw, _ := json.Marshal(body)
		models, _, err := parseModelsWithMeta(string(raw))
		if err != nil {
			t.Fatalf("malformed body[24] returned error: %v", err)
		}
		if len(models) != 1 || models[0].Name != "gemini-deeper-analysis" || models[0].Locked {
			t.Fatalf("unexpected parsed model: %+v", models)
		}

		// Absent body[24] (len < 25)
		shortBody := body[:18]
		rawShort, _ := json.Marshal(shortBody)
		modelsShort, _, err := parseModelsWithMeta(string(rawShort))
		if err != nil {
			t.Fatalf("absent body[24] returned error: %v", err)
		}
		if len(modelsShort) != 1 || modelsShort[0].Name != "gemini-deeper-analysis" {
			t.Fatalf("unexpected short parsed model: %+v", modelsShort)
		}
	})
}

func TestK25_AliasesAndSlugRenameNoWithdrawn(t *testing.T) {
	var logs bytes.Buffer
	c, err := New(testConfig(), slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}

	// Start with old slugs, then receive a dynamic catalog with the new slugs for the same hex ModelIDs.
	c.storeModels([]*AvailableModel{
		{Name: "gemini-quick-answers", ModelID: "8c46e95b1a07cecc", DisplayName: "Fast", Description: "3.5 Flash", ModeCode: 6},
		{Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0", DisplayName: "Thinking with 3.1 Pro", Description: "3.1 Pro", Family: "Pro3p1", ModeCode: 3},
		{Name: "gemini-most-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Argon", Family: "Argon4p0", ModeCode: 8},
		{Name: "gemini-code-&-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Argon", Family: "Argon4p0", ModeCode: 8},
	}, "dynamic")
	// Seed a stale withdrawn entry sharing an active ModelID to verify pruning on next storeModels.
	c.modelMu.Lock()
	if c.withdrawn == nil {
		c.withdrawn = map[string]WithdrawnModel{}
	}
	c.withdrawn["gemini-legacy-temp"] = WithdrawnModel{
		Model: AvailableModel{Name: "gemini-legacy-temp", ModelID: "e6fa609c3fa255c0"},
		Since: time.Now(),
	}
	c.modelMu.Unlock()

	c.storeModels([]*AvailableModel{
		{Name: "gemini-auto", ModelID: "8c46e95b1a07cecc", DisplayName: "Auto", Description: "Auto", ModeCode: 6},
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", DisplayName: "4 Hydrogen", Description: "4 Hydrogen", ModeCode: 1},
		{Name: "gemini-deeper-analysis", ModelID: "e6fa609c3fa255c0", DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0", ModeCode: 3},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Argon", Family: "Argon4p0", ModeCode: 8},
	}, "dynamic")

	if wd := c.WithdrawnModels(); len(wd) != 0 {
		t.Fatalf("WithdrawnModels after slug rename = %+v, want empty", wd)
	}

	aliasCases := []struct {
		asked string
		want  string
		id    string
	}{
		{"gemini-detailed-research", "gemini-deeper-analysis", "e6fa609c3fa255c0"},
		{"gemini-3.1-pro", "gemini-deeper-analysis", "e6fa609c3fa255c0"},
		{"gemini-most-complex-work", "gemini-code-complex-work", "95a4c0abcba24364"},
		{"gemini-code-&-complex-work", "gemini-code-complex-work", "95a4c0abcba24364"},
		{"gemini-quick-answers", "gemini-auto", "8c46e95b1a07cecc"},
		{"gemini-3.5-flash", "gemini-auto", "8c46e95b1a07cecc"},
	}
	for _, tc := range aliasCases {
		target, id, isAlias := c.ResolveModelAlias(tc.asked)
		if !isAlias || target != tc.want || id != tc.id {
			t.Errorf("ResolveModelAlias(%q) = (%q, %q, %v), want (%q, %q, true)", tc.asked, target, id, isAlias, tc.want, tc.id)
		}
		resolved, err := c.ResolveModel(tc.asked)
		if err != nil {
			t.Fatalf("ResolveModel(%q): %v", tc.asked, err)
		}
		if resolved.Name != tc.want || resolved.ModelID != tc.id {
			t.Errorf("ResolveModel(%q) = (%q, %q), want (%q, %q)", tc.asked, resolved.Name, resolved.ModelID, tc.want, tc.id)
		}
	}

	logOut := logs.String()
	if !strings.Contains(logOut, "model_alias_resolved") {
		t.Errorf("expected model_alias_resolved log, got: %s", logOut)
	}
	if strings.Contains(logOut, "absent from the last catalogue") {
		t.Errorf("unexpected absent-model warning for active alias: %s", logOut)
	}
}

func TestK25_KnownFallbackModelsCurrentAndLegacy(t *testing.T) {
	c, err := New(Config{DefaultModel: "gemini-deeper-analysis"}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate fallback initialization without network calls.
	c.storeModels(fallbackModels(c.cfg.DefaultModel), "fallback")

	if m, err := c.ResolveModel("gemini-deeper-analysis"); err != nil || m.ModelID != "e6fa609c3fa255c0" || m.Description != "4 Neon" || m.Family != "Neon4p0" {
		t.Fatalf("ResolveModel(gemini-deeper-analysis) = (%+v, %v)", m, err)
	}
	if m, err := c.ResolveModel("gemini-auto"); err != nil || m.ModelID != "8c46e95b1a07cecc" || m.DisplayName != "Auto" || m.Description != "Auto" {
		t.Fatalf("ResolveModel(gemini-auto) = (%+v, %v)", m, err)
	}
	for _, slug := range []string{"gemini-detailed-research", "gemini-most-complex-work", "gemini-code-complex-work", "gemini-code-&-complex-work", "gemini-quick-answers"} {
		if _, err := c.ResolveModel(slug); err != nil {
			t.Errorf("fallback ResolveModel(%q) failed: %v", slug, err)
		}
	}
}

func TestK25_ConfirmedNamesSmartPrecedenceAndRestart(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	statePath := filepath.Join(dir, "cookie.txt.state.json")
	callsPath := filepath.Join(dir, "cookie.txt.calls.jsonl")
	if err := os.WriteFile(cookiePath, []byte("__Secure-1PSID=fake; __Secure-1PSIDTS=fakets"), 0o600); err != nil {
		t.Fatal(err)
	}

	c1, err := New(Config{
		CookieFile: cookiePath,
		StateFile:  statePath,
		CallsFile:  callsPath,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: initial catalog had 3.1 Pro and 4 Carbon; confirmed calls recorded both in calls.jsonl.
	oldAt := time.Now().UTC().Add(-10 * time.Minute)
	c1.storeModels([]*AvailableModel{
		{Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0", DisplayName: "Thinking with 3.1 Pro", Description: "3.1 Pro", Family: "Pro3p1", ModeCode: 3},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Carbon", Family: "Carbon4p0", ModeCode: 8},
	}, "dynamic")
	c1.modelMu.Lock()
	c1.catalogAt = oldAt
	c1.modelMu.Unlock()

	recPro := newCallRecord(&CallMetrics{ID: "req-old-pro"}, GenParams{
		Prompt: "hi",
		Model:  &AvailableModel{Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0"},
	}, oldAt)
	recPro.Time = oldAt.Format(time.RFC3339)
	recPro.TextBytes = 50
	c1.recordCall(recPro, &idScan{servedID: "e6fa609c3fa255c0", servedName: "3.1 Pro"})

	recArgon := newCallRecord(&CallMetrics{ID: "req-argon"}, GenParams{
		Prompt: "hi",
		Model:  &AvailableModel{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364"},
	}, oldAt)
	recArgon.Time = oldAt.Format(time.RFC3339)
	recArgon.TextBytes = 50
	c1.recordCall(recArgon, &idScan{servedID: "95a4c0abcba24364", servedName: "4 Argon"})

	// Step 2: Google upgrades e6fa609c3fa255c0 in otAQ7b to Neon / Neon4p0 ("4 Neon"),
	// while 95a4c0abcba24364 still reports Code & Analysis / Carbon4p0 ("4 Carbon") in otAQ7b.
	c1.storeModels([]*AvailableModel{
		{Name: "gemini-deeper-analysis", ModelID: "e6fa609c3fa255c0", DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0", ModeCode: 3},
		{Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364", DisplayName: "Code & Analysis", Description: "4 Carbon", Family: "Carbon4p0", ModeCode: 8},
	}, "dynamic")
	c1.modelMu.Lock()
	c1.catalogAt = time.Now().UTC()
	c1.modelMu.Unlock()
	c1.saveState()

	neon, err := c1.ResolveModel("gemini-deeper-analysis")
	if err != nil {
		t.Fatal(err)
	}
	if neon.Description != "4 Neon" {
		t.Fatalf("Neon Description after catalog upgrade = %q, want %q", neon.Description, "4 Neon")
	}
	argon, err := c1.ResolveModel("gemini-code-complex-work")
	if err != nil {
		t.Fatal(err)
	}
	if argon.Description != "4 Argon" {
		t.Fatalf("Argon Description when catalog unchanged = %q, want %q", argon.Description, "4 Argon")
	}

	// Step 3: Restart client BEFORE any 4 Neon call occurs:
	// loadCalls() replays old "3.1 Pro" from calls.jsonl, then RestoreState() must restore "4 Neon"
	// (because InvalidatedAt > old call timestamp) while keeping "4 Argon" for 95a4c0abcba24364!
	c2, err := New(Config{
		CookieFile: cookiePath,
		StateFile:  statePath,
		CallsFile:  callsPath,
	}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c2.RestoreState()

	neon2, err := c2.ResolveModel("gemini-deeper-analysis")
	if err != nil {
		t.Fatal(err)
	}
	if neon2.Description != "4 Neon" {
		t.Fatalf("restarted Neon Description = %q, want %q (old 3.1 Pro must not resurrect)", neon2.Description, "4 Neon")
	}
	argon2, err := c2.ResolveModel("gemini-code-complex-work")
	if err != nil {
		t.Fatal(err)
	}
	if argon2.Description != "4 Argon" {
		t.Fatalf("restarted Argon Description = %q, want %q", argon2.Description, "4 Argon")
	}
}

func TestK25_RefreshModelsLogsModelsSummary(t *testing.T) {
	body := make([]any, 25)
	entry := func(id, label, desc, versioned, family string) []any {
		e := make([]any, 18)
		e[0], e[1], e[2], e[8], e[11] = id, label, desc, family, versioned
		return e
	}
	body[15] = []any{
		entry("8c46e95b1a07cecc", "Auto", "Auto", "Auto", ""),
		entry("e6fa609c3fa255c0", "Neon", "4 Neon", "Deeper analysis", "Neon4p0"),
	}
	inner, _ := json.Marshal(body)
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", listModelsRPC, string(inner)}})
	payload := ")]}'\n" + string(outer)

	var logs bytes.Buffer
	c, err := New(Config{
		Cookie:    "SAPISID=fake; __Secure-1PSIDTS=fakets",
		GeminiBL:  "test_bl",
		IdleQuiet: 0,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp200(r, payload), nil
	}))
	c.setAt("tok")

	if err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	logOut := logs.String()
	if !strings.Contains(logOut, "models=") ||
		!strings.Contains(logOut, "gemini-auto(8c46e95b1a07cecc=Auto)") ||
		!strings.Contains(logOut, "gemini-deeper-analysis(e6fa609c3fa255c0=4 Neon)") {
		t.Fatalf("RefreshModels log missing models summary: %s", logOut)
	}
}

func TestK25_AutoRoutedReplyNotDoubt(t *testing.T) {
	var logs bytes.Buffer
	c, err := New(Config{DefaultModel: "gemini-auto"}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	autoModel := &AvailableModel{
		Name: "gemini-auto", ModelID: "8c46e95b1a07cecc",
		DisplayName: "Auto", Description: "Auto", ModeCode: 6,
	}
	c.SetModels([]*AvailableModel{
		autoModel,
		{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", DisplayName: "4 Hydrogen", Description: "4 Hydrogen", ModeCode: 1},
	})

	rec := newCallRecord(&CallMetrics{ID: "req-auto-1"}, GenParams{Prompt: "hello", Model: autoModel}, time.Now())
	rec.Stream = true
	rec.TextBytes = 42
	scan := &idScan{servedID: "56fdd199312815e2", servedName: "4 Hydrogen"}
	scan.add("56fdd199312815e2")
	c.recordCall(rec, scan)

	calls := c.RecentCalls()
	if len(calls) != 1 {
		t.Fatalf("calls len = %d, want 1", len(calls))
	}
	if calls[0].Identity != "routed_auto" || calls[0].ServedID != "56fdd199312815e2" || calls[0].ServedName != "4 Hydrogen" {
		t.Fatalf("call record = %+v, want routed_auto with 56fdd199312815e2 / 4 Hydrogen", calls[0])
	}
	ids := c.ModelIdentities()
	if len(ids) != 1 || ids[0].LastVerdict != "routed_auto" || !ids[0].MismatchSince.IsZero() {
		t.Fatalf("ModelIdentities = %+v, want LastVerdict=routed_auto and zero MismatchSince", ids)
	}
	logOut := logs.String()
	if strings.Contains(logOut, "served_model_doubt") {
		t.Errorf("unexpected served_model_doubt log for Auto model: %s", logOut)
	}
	if !strings.Contains(logOut, "served_model") || !strings.Contains(logOut, "routed_auto") {
		t.Errorf("expected served_model routed_auto log, got: %s", logOut)
	}
}

func TestK25_ParseFrameAndWrbLineABMultiCandidate(t *testing.T) {
	makeABLine := func(textA, thoughtA, textB, thoughtB string) string {
		makeCand := func(rc, text, thought string) []any {
			c := make([]any, 38)
			c[0] = rc
			if text != "" {
				c[1] = []any{text}
			} else {
				c[1] = []any{}
			}
			if thought != "" {
				c[37] = []any{[]any{thought}}
			}
			return c
		}
		inner2 := make([]any, 5)
		inner2[4] = []any{
			makeCand("rc_a", textA, thoughtA),
			makeCand("rc_b", textB, thoughtB),
		}
		innerRaw, err := json.Marshal(inner2)
		if err != nil {
			t.Fatal(err)
		}
		outerRaw, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(innerRaw)}})
		if err != nil {
			t.Fatal(err)
		}
		line := string(outerRaw)
		if len(line) < 200 {
			t.Fatalf("expected valid wrb.fr line len >= 200, got %d", len(line))
		}
		return line
	}

	t.Run("choice_b_longer_does_not_override_choice_a", func(t *testing.T) {
		line := makeABLine(
			"Choix A court",
			"Pensée A",
			"Choix B beaucoup plus long qui ne doit pas écraser ni se mêler au Choix A",
			"Pensée B",
		)
		fr, ok := parseFrame(line)
		if !ok {
			t.Fatalf("parseFrame returned ok=false")
		}
		if fr.text != "Choix A court" {
			t.Errorf("fr.text = %q, want %q", fr.text, "Choix A court")
		}
		if fr.thought != "Pensée A" {
			t.Errorf("fr.thought = %q, want %q", fr.thought, "Pensée A")
		}
		got := parseWrbLine(line)
		if !slices.Equal(got, []string{"Choix A court"}) {
			t.Errorf("parseWrbLine = %v, want [\"Choix A court\"]", got)
		}
	})

	t.Run("empty_choice_a_stays_empty_even_when_choice_b_has_text", func(t *testing.T) {
		line := makeABLine(
			"",
			"Pensée A",
			"Choix B plus long qui dépasse largement pour atteindre une trame réaliste de réponse",
			"Pensée B",
		)
		fr, ok := parseFrame(line)
		if !ok {
			t.Fatalf("parseFrame returned ok=false")
		}
		if fr.text != "" {
			t.Errorf("fr.text = %q, want empty", fr.text)
		}
		if fr.thought != "Pensée A" {
			t.Errorf("fr.thought = %q, want %q", fr.thought, "Pensée A")
		}
		if got := parseWrbLine(line); len(got) != 0 {
			t.Errorf("parseWrbLine = %v, want empty slice", got)
		}
	})
}

func TestK25_EnrichModelsFromMenuReal20261119Shape(t *testing.T) {
	catEntry := func(id, label, desc, family, versioned string, mode int) []any {
		e := make([]any, 18)
		e[0], e[1], e[2], e[8], e[11], e[17] = id, label, desc, family, versioned, float64(mode)
		return e
	}
	menuEntry := func(mode int, family any, locked bool, levels []any, extra1 any, group []int) []any {
		hLen := 16
		if extra1 == nil {
			hLen = 17
		}
		h := make([]any, hLen)
		h[0] = ""
		h[1] = float64(mode)
		h[8] = family
		if locked {
			h[14] = []any{float64(1), "gemini-4-carbon"}
		}
		if extra1 == nil {
			h[15] = float64(2)
		} else {
			h[15] = float64(1)
		}
		grp := make([]any, len(group))
		for i, g := range group {
			grp[i] = float64(g)
		}
		return []any{h, levels, extra1, grp}
	}

	lvlsBasMoyenEleve := []any{
		[]any{float64(1), "Bas", "Fast and efficient", false},
		[]any{float64(4), "Moyen", "Équilibré", false},
		[]any{float64(2), "Élevé", "Très poussé", false},
	}
	lvlsStandard := []any{
		[]any{float64(1), "Standard", "Idéal pour la plupart des questions", false},
	}
	lvlsBas := []any{
		[]any{float64(1), "Bas", "Rapide et efficace", false},
	}

	body := make([]any, 25)
	body[15] = []any{
		catEntry("8c46e95b1a07cecc", "Auto", "Adapts to your needs", "", "Auto", 6),
		catEntry("56fdd199312815e2", "4 Hydrogen", "4 Hydrogen", "", "Everyday help", 1),
		catEntry("e6fa609c3fa255c0", "Neon", "4 Neon", "Neon4p0", "Deeper analysis", 3),
		catEntry("95a4c0abcba24364", "Carbon", "Carbon", "", "Code & complex work", 8),
	}
	body[24] = []any{
		nil,
		[]any{
			menuEntry(6, nil, false, nil, float64(1), []int{1}),
			menuEntry(1, nil, false, lvlsBasMoyenEleve, nil, []int{1}),
			menuEntry(1, nil, false, lvlsStandard, float64(1), []int{2}),
			menuEntry(3, "Neon4p0", false, lvlsBasMoyenEleve, nil, []int{1}),
			menuEntry(3, "Neon4p0", false, lvlsBas, float64(1), []int{2}),
			menuEntry(8, nil, true, lvlsStandard, nil, []int{1}),
			menuEntry(8, nil, true, lvlsStandard, float64(1), []int{2}),
		},
	}

	innerBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	models, err := parseModels(string(innerBytes))
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	byName := make(map[string]*AvailableModel, len(models))
	for _, m := range models {
		byName[m.Name] = m
	}

	if m := byName["gemini-auto"]; m == nil || m.Locked || len(m.ThinkingLevels) != 0 {
		t.Errorf("gemini-auto = %+v, want Locked=false and 0 ThinkingLevels", m)
	}
	if m := byName["gemini-everyday-help"]; m == nil || m.Locked || !slices.Equal(m.ThinkingLevels, []int{1, 4, 2}) {
		t.Errorf("gemini-everyday-help = %+v, want Locked=false and ThinkingLevels=[1 4 2]", m)
	}
	if m := byName["gemini-deeper-analysis"]; m == nil || m.Locked || !slices.Equal(m.ThinkingLevels, []int{1, 4, 2}) {
		t.Errorf("gemini-deeper-analysis = %+v, want Locked=false and ThinkingLevels=[1 4 2]", m)
	}
	if m := byName["gemini-code-complex-work"]; m == nil || !m.Locked || !slices.Equal(m.ThinkingLevels, []int{1}) {
		t.Errorf("gemini-code-complex-work = %+v, want Locked=true and ThinkingLevels=[1]", m)
	}
}
