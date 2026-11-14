package gemini

import (
	"os"
	"path/filepath"
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
// remembered, refused with an explicit message, and forgotten when it returns.
func TestWithdrawnModels(t *testing.T) {
	c, _ := New(Config{}, nil)
	pro := &AvailableModel{Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0", Description: "3.1 Pro"}
	lite := &AvailableModel{Name: "gemini-quick-answers", ModelID: "8c46e95b1a07cecc", Description: "3.5 Flash-Lite"}
	c.storeModels([]*AvailableModel{lite, pro}, "dynamic")
	c.storeModels([]*AvailableModel{lite}, "dynamic")
	if w := c.WithdrawnModels(); len(w) != 1 || w[0].Model.Name != pro.Name {
		t.Fatalf("withdrawn=%+v", w)
	}
	_, err := c.ResolveModel(pro.Name)
	if err == nil || !containsStr(err.Error(), "no longer in the catalogue") || !containsStr(err.Error(), "3.1 Pro") {
		t.Fatalf("err=%v", err)
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
