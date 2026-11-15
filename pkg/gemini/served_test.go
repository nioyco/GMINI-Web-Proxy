package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIDScanClassify(t *testing.T) {
	cat := map[string]struct{}{"e6fa609c3fa255c0": {}, "8c46e95b1a07cecc": {}}
	cases := []struct {
		raw, req, want string
		seen           int
	}{
		{`[["wrb.fr",null,"[null,[\"c_1a2b3c4d5e6f7a8b\",\"r_aa11bb22cc33dd44\"],\"e6fa609c3fa255c0\"]"]]`, "e6fa609c3fa255c0", "confirmed", 1},
		{`x \"8c46e95b1a07cecc\" y`, "e6fa609c3fa255c0", "other_model", 1},
		{`\"c_1a2b3c4d5e6f7a8b\" 1794621774798620 \"abcdefabcdefabcd\"`, "e6fa609c3fa255c0", "not_stated", 0},
		{`\"0123456789abcdef\"`, "e6fa609c3fa255c0", "not_stated", 0},
	}
	for i, c := range cases {
		s := &idScan{}
		s.add(c.raw)
		seen, _, v := s.classify(c.req, cat)
		if v != c.want || len(seen) != c.seen {
			t.Errorf("case %d: verdict=%s seen=%v", i, v, seen)
		}
	}
	s := &idScan{}
	s.add(`\"0123456789abcdef\"`)
	if _, other, _ := s.classify("x", cat); other != 1 {
		t.Errorf("unknown hex id not counted: %d", other)
	}
}

func TestCallJournalSuspectAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "calls.jsonl")
	c, err := New(Config{CallsFile: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &AvailableModel{Name: "pro", ModelID: "e6fa609c3fa255c0"}
	rec := func(thought int) CallRecord {
		r := newCallRecord(&CallMetrics{ID: "r"}, GenParams{Prompt: "secret prompt", Model: m}, time.Now())
		r.Stream, r.TextBytes, r.ThoughtBytes = true, 100, thought
		return r
	}
	c.recordCall(rec(0), &idScan{}) // nothing learnt yet: not suspect
	c.recordCall(rec(500), &idScan{})
	c.recordCall(rec(700), &idScan{})
	c.recordCall(rec(0), &idScan{}) // model usually thinks: suspect
	got := c.RecentCalls()
	if len(got) != 4 || got[0].Suspect != "no_thoughts" || got[3].Suspect != "" || got[1].Suspect != "" {
		t.Fatalf("records=%+v", got)
	}
	data, _ := os.ReadFile(path)
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("journal mode=%v", st.Mode().Perm())
	}
	if len(data) == 0 || containsStr(string(data), "secret prompt") {
		t.Errorf("journal empty or leaks content")
	}
	// A new process learns from the file.
	c2, _ := New(Config{CallsFile: path}, nil)
	if n := len(c2.RecentCalls()); n != 4 {
		t.Fatalf("reloaded %d records", n)
	}
	c2.recordCall(rec(0), &idScan{})
	if c2.RecentCalls()[0].Suspect != "no_thoughts" {
		t.Errorf("suspicion not kept across restart")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Layout read in the masked capture of 2026-11-14: [8] family code, [11] menu
// label, [17] web mode code. Missing or odd values must not break parsing.
func TestParseModelsCatalogueExtras(t *testing.T) {
	inner := `[null,null,null,null,null,null,null,null,null,null,null,null,null,null,null,
	 [["95a4c0abcba24364","Carbon","4 Carbon",[2],2,null,null,true,"Carbon4p0",4,"Carbon","Most complex work","4 Carbon",null,null,true,"Carbon4p0",8],
	  ["e6fa609c3fa255c0","Pro","3.1 Pro",[2],2,null,["95a4c0abcba24364"],false,"",4,"Pro","Detailed research","3.1 Pro",null,null,false,"","x"],
	  ["8c46e95b1a07cecc","Flash-Lite","3.5 Flash-Lite"]],[8,0,38],[19]]`
	ms, err := parseModels(inner)
	if err != nil || len(ms) != 3 {
		t.Fatalf("err=%v n=%d", err, len(ms))
	}
	if ms[0].Family != "Carbon4p0" || ms[0].ModeCode != 8 || ms[0].MenuLabel != "Most complex work" || ms[0].Description != "4 Carbon" {
		t.Errorf("carbon=%+v", ms[0])
	}
	if ms[1].ModeCode != 0 || ms[1].Family != "" || ms[2].ModeCode != 0 || ms[2].MenuLabel != "" {
		t.Errorf("tolerant parsing failed: %+v %+v", ms[1], ms[2])
	}
}

// A model dropped by a newer catalogue (web usage limit reached, 2026-11-14) is
// remembered, stays callable with its last known id, and is forgotten when it returns.
func TestWithdrawnModels(t *testing.T) {
	c, _ := New(Config{}, nil)
	pro := &AvailableModel{Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"}
	lite := &AvailableModel{Name: "gemini-quick-answers", ModelID: "8c46e95b1a07cecc", Description: "3.5 Flash-Lite"}
	c.storeModels([]*AvailableModel{lite, pro}, "dynamic")
	c.storeModels([]*AvailableModel{lite}, "dynamic")
	if w := c.WithdrawnModels(); len(w) != 1 || w[0].Model.Name != pro.Name {
		t.Fatalf("withdrawn=%+v", w)
	}
	// Still callable with its last known id: the catalogue shrank on 2026-11-15
	// while the web page still offered every model.
	got, err := c.ResolveModel(pro.Name)
	if err != nil || got == nil || got.ModelID != pro.ModelID {
		t.Fatalf("withdrawn model must stay callable: got=%+v err=%v", got, err)
	}
	if _, err := c.ResolveModel("never-seen"); err == nil || containsStr(err.Error(), "no longer") {
		t.Fatalf("err=%v", err)
	}
	c.storeModels([]*AvailableModel{lite, pro}, "dynamic")
	if len(c.WithdrawnModels()) != 0 {
		t.Errorf("model came back but is still withdrawn")
	}
	if _, err := c.ResolveModel(pro.Name); err != nil {
		t.Errorf("err=%v", err)
	}
}

func TestCallByRequest(t *testing.T) {
	c, _ := New(Config{}, nil)
	r := newCallRecord(&CallMetrics{ID: "abc"}, GenParams{Model: &AvailableModel{Name: "m"}}, time.Now())
	c.recordCall(r, &idScan{})
	if got, ok := c.CallByRequest("abc"); !ok || got.Model != "m" {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
	if _, ok := c.CallByRequest("zzz"); ok {
		t.Fatal("unknown id found")
	}
}

// Response body positions 39 (model id) and 42 (model name), as found in the
// real traces of 2026-11-14.
func servedLine(id, name string) string {
	body := make([]any, 43)
	body[4] = []any{[]any{"rc_x", []any{"texte"}}}
	body[39], body[42] = id, name
	ib, _ := json.Marshal(body)
	line, _ := json.Marshal([]any{[]any{"wrb.fr", nil, string(ib)}})
	return string(line)
}

func TestServedModelFromReply(t *testing.T) {
	c, _ := New(Config{}, nil)
	pro := &AvailableModel{Name: "pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"}
	c.storeModels([]*AvailableModel{pro, {Name: "lite", ModelID: "8c46e95b1a07cecc"}}, "dynamic")
	rec := func(line string, thought int) CallRecord {
		s := &idScan{}
		s.add(line)
		r := newCallRecord(&CallMetrics{ID: line[len(line)-8:]}, GenParams{Model: pro}, time.Now())
		r.Stream, r.TextBytes, r.ThoughtBytes = true, 50, thought
		c.recordCall(r, s)
		return c.RecentCalls()[0]
	}
	rec(servedLine("e6fa609c3fa255c0", "3.1 Pro"), 300)
	rec(servedLine("e6fa609c3fa255c0", "3.1 Pro"), 300)
	// Right model, no thoughts (short answer): confirmed, never suspect.
	if r := rec(servedLine("e6fa609c3fa255c0", "3.1 Pro"), 0); r.Identity != "confirmed" || r.Suspect != "" || r.ServedName != "3.1 Pro" {
		t.Fatalf("r=%+v", r)
	}
	// Another model named by Google.
	if r := rec(servedLine("8c46e95b1a07cecc", "3.5 Flash-Lite"), 0); r.Identity != "other_model" || r.ServedName != "3.5 Flash-Lite" {
		t.Fatalf("r=%+v", r)
	}
	// The asked model's name with another id (seen on 4 Carbon, 2026-11-15).
	if r := rec(servedLine("cf41b0e0dd7d53e5", "3.1 Pro"), 200); r.Identity != "name_only" {
		t.Fatalf("r=%+v", r)
	}
	// Nothing stated and no thoughts: suspect.
	if r := rec(servedLine("", ""), 0); r.Identity != "not_stated" || r.Suspect != "no_thoughts" {
		t.Fatalf("r=%+v", r)
	}
}

func catalogWithStatusAndNames(t *testing.T, status int, names ...string) string {
	t.Helper()
	raw := make([]any, 0, len(names))
	for i, n := range names {
		ml := make([]any, 12)
		ml[0] = "id_" + n
		ml[1] = n
		ml[11] = n
		_ = i
		raw = append(raw, ml)
	}
	body := make([]any, 18)
	if status > 0 {
		body[14] = status
	}
	body[15] = raw
	ib, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal([]any{[]any{"wrb.fr", listModelsRPC, string(ib)}})
	if err != nil {
		t.Fatal(err)
	}
	return ")]}'\n" + string(outer)
}

// TestDegradedCatalogRejectedAndRecovered proves R&D-14 R3 (C3):
//   - a 1-model listModels reply must NOT replace a >=2-model catalog in memory or on disk;
//   - rejection holds with body[14]=1016 AND when body[14] is absent;
//   - sessionDegraded stays active (without polluting withdrawn) until >=2 models return.
func TestDegradedCatalogRejectedAndRecovered(t *testing.T) {
	dir := t.TempDir()
	cookiePath := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(cookiePath, []byte("SAPISID=s; __Secure-1PSIDTS=ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := cookiePath + ".state.json"

	var reply string
	var logs bytes.Buffer
	c, err := New(Config{CookieFile: cookiePath, StateFile: statePath}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp200(r, reply), nil
	}))
	c.setAt("tok")
	ctx := context.Background()

	// 1. Seed a 4-model dynamic catalog -> persisted on disk.
	reply = catalogWithStatusAndNames(t, 0, "3.5 Flash", "3.5 Flash-Lite", "3.1 Pro", "4 Carbon")
	if err := c.fetchModels(ctx); err != nil {
		t.Fatalf("initial fetchModels: %v", err)
	}
	if len(c.ModelNames()) != 4 {
		t.Fatalf("got %d models, want 4", len(c.ModelNames()))
	}
	diskBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Partial drop (4 -> 3) still updates and tracks withdrawn model (5bf479e).
	reply = catalogWithStatusAndNames(t, 0, "3.5 Flash", "3.5 Flash-Lite", "3.1 Pro")
	if err := c.fetchModels(ctx); err != nil {
		t.Fatalf("partial shrink fetchModels: %v", err)
	}
	if len(c.ModelNames()) != 3 || len(c.WithdrawnModels()) != 1 {
		t.Fatalf("models=%d withdrawn=%d, want 3 / 1", len(c.ModelNames()), len(c.WithdrawnModels()))
	}
	if _, err := c.ResolveModel("gemini-4-carbon"); err != nil {
		t.Fatalf("withdrawn model must stay callable: %v", err)
	}

	// 3. Degraded reply (1 model, body[14]=1016): rejected, catalog & state unchanged.
	diskBefore, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	reply = catalogWithStatusAndNames(t, 1016, "3.5 Flash")
	if err := c.fetchModels(ctx); !errors.Is(err, ErrCatalogDegraded) {
		t.Fatalf("err = %v, want ErrCatalogDegraded", err)
	}
	if len(c.ModelNames()) != 3 {
		t.Fatalf("catalog overwritten: got %v, want 3 models", c.ModelNames())
	}
	if len(c.WithdrawnModels()) != 1 {
		t.Fatalf("withdrawn polluted by rejected 1-model catalog: %+v", c.WithdrawnModels())
	}
	diskAfter, _ := os.ReadFile(statePath)
	if string(diskAfter) != string(diskBefore) {
		t.Error("on-disk state was modified by rejected degraded catalog")
	}
	h := c.CatalogHealth()
	if !h.SessionDegraded || h.AccountStatus != 1016 {
		t.Errorf("CatalogHealth = %+v", h)
	}
	if !strings.Contains(logs.String(), "account_status=1016") {
		t.Errorf("expected account_status=1016 in WARN log:\n%s", logs.String())
	}

	// 4. Even without body[14], a 1-model reply must STILL be rejected.
	oldWarn := catalogWarnEvery
	catalogWarnEvery = 0
	t.Cleanup(func() { catalogWarnEvery = oldWarn })
	reply = catalogWithStatusAndNames(t, 0, "3.5 Flash")
	if err := c.fetchModels(ctx); !errors.Is(err, ErrCatalogDegraded) {
		t.Fatalf("err without body[14] = %v, want ErrCatalogDegraded", err)
	}
	if len(c.ModelNames()) != 3 || !c.CatalogHealth().SessionDegraded {
		t.Fatal("must reject 1-model catalog regardless of body[14]")
	}

	// 5. Receiving a full catalog again clears SessionDegraded.
	reply = catalogWithStatusAndNames(t, 0, "3.5 Flash", "3.5 Flash-Lite", "3.1 Pro", "4 Carbon")
	if err := c.fetchModels(ctx); err != nil {
		t.Fatalf("recovery fetchModels: %v", err)
	}
	h = c.CatalogHealth()
	if h.SessionDegraded || h.AccountStatus != 0 || len(c.ModelNames()) != 4 || len(c.WithdrawnModels()) != 0 {
		t.Errorf("not recovered cleanly: health=%+v models=%d withdrawn=%d", h, len(c.ModelNames()), len(c.WithdrawnModels()))
	}
}

// K2: A call without a position-39 served id and frames==0 must never be marked
// "confirmed" solely because the requested hex id appeared elsewhere in raw text.
func TestNoPosition39WithZeroFramesNeverConfirmed(t *testing.T) {
	c, _ := New(Config{}, nil)
	pro := &AvailableModel{Name: "pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"}
	c.storeModels([]*AvailableModel{pro}, "dynamic")

	scan := &idScan{}
	scan.add(`{"echo":"e6fa609c3fa255c0"}`)
	r := newCallRecord(&CallMetrics{ID: "z0"}, GenParams{Model: pro}, time.Now())
	r.TextBytes = 50
	c.recordCall(r, scan)

	got := c.RecentCalls()[0]
	if got.Identity != "not_stated" || got.ServedID != "" || got.Frames != 0 {
		t.Fatalf("got=%+v, want Identity=not_stated ServedID='' Frames=0", got)
	}
}

// K2: Non-progressive Generate must parse each line separately so position 39/42
// and frame counts are recorded just like GenerateStreamEvents.
func TestGenerateScansFramesPosition39(t *testing.T) {
	c, _ := New(Config{RetryAttempts: 1, RequestTimeout: 5}, nil)
	pro := &AvailableModel{Name: "pro", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"}
	c.storeModels([]*AvailableModel{pro, {Name: "lite", ModelID: "cf41b0e0dd7d53e5", Description: "3.5 Flash-Lite"}}, "dynamic")

	var replyLine string
	c.SetTransport(rtFunc(func(*http.Request) (*http.Response, error) {
		body := ")]}'\n\n123\n" + replyLine + "\n"
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))

	replyLine = servedLine("e6fa609c3fa255c0", "3.1 Pro")
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "hi", Model: pro}); err != nil {
		t.Fatalf("Generate 1: %v", err)
	}
	r1 := c.RecentCalls()[0]
	if r1.Frames == 0 || r1.ServedID != "e6fa609c3fa255c0" || r1.ServedName != "3.1 Pro" || r1.Identity != "confirmed" {
		t.Fatalf("r1=%+v", r1)
	}

	replyLine = servedLine("cf41b0e0dd7d53e5", "3.1 Pro")
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "hi", Model: pro}); err != nil {
		t.Fatalf("Generate 2: %v", err)
	}
	r2 := c.RecentCalls()[0]
	if r2.Frames == 0 || r2.ServedID != "cf41b0e0dd7d53e5" || r2.ServedName != "3.1 Pro" || r2.Identity != "name_only" {
		t.Fatalf("r2=%+v", r2)
	}
}
