package gemini

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// GeminiImageTokens is the fixed Gemini per-image input token budget (258 tokens/image).
	GeminiImageTokens = 258
	// GeminiDefaultContextWindow is the 1M-token context window of Gemini 2.5 / 3.x models.
	GeminiDefaultContextWindow = 1048576
)

// EstimateTokensForBytes estimates tokens from UTF-8 byte length (~4 chars/token, rounded up).
func EstimateTokensForBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// EstimateTokensForText estimates tokens from a UTF-8 string's rune count (~4 chars/token).
func EstimateTokensForText(s string) int {
	if s == "" {
		return 0
	}
	return (utf8.RuneCountInString(s) + 3) / 4
}

// ModelContextWindow returns the context window size (in tokens) for a Gemini model name.
func ModelContextWindow(modelName string) int {
	_ = modelName
	return GeminiDefaultContextWindow
}

// ResolveEffectiveModel returns Google's stated model name (if any) and the
// effective model label used for token attribution (falling back to askedModel).
func ResolveEffectiveModel(askedModel, servedID, servedName string, catalogNames map[string]string) (googleModel, effectiveModel string, googleIndicated bool) {
	if s := strings.TrimSpace(servedName); s != "" {
		return s, s, true
	}
	if s := strings.TrimSpace(servedID); s != "" {
		if catalogNames != nil {
			if name := strings.TrimSpace(catalogNames[s]); name != "" {
				return name, name, true
			}
		}
		return s, s, true
	}
	asked := strings.TrimSpace(askedModel)
	if asked == "" {
		asked = "inconnu"
	}
	return "", asked, false
}

// PopulateCallTokens fills token estimation and Google model fields on a CallRecord
// when missing, preserving already computed rune-based counts if present.
func PopulateCallTokens(r *CallRecord, catalogNames map[string]string) {
	if r == nil {
		return
	}
	if r.GoogleModel == "" {
		gModel, _, indicated := ResolveEffectiveModel(r.Model, r.ServedID, r.ServedName, catalogNames)
		if indicated {
			r.GoogleModel = gModel
		}
	}
	if r.InputTokens <= 0 && (r.PromptBytes > 0 || r.ImageCount > 0) {
		r.InputTokens = EstimateTokensForBytes(r.PromptBytes) + r.ImageCount*GeminiImageTokens
	}
	if r.ThoughtTokens <= 0 && r.ThoughtBytes > 0 {
		r.ThoughtTokens = EstimateTokensForBytes(r.ThoughtBytes)
	}
	if r.TextTokens <= 0 && r.TextBytes > 0 {
		r.TextTokens = EstimateTokensForBytes(r.TextBytes)
	}
	r.TotalTokens = r.InputTokens + r.ThoughtTokens + r.TextTokens
}

// TokenRecord is one completed request's content-free token usage estimate,
// attributed to Google's indicated model when stated and persisted as JSONL.
type TokenRecord struct {
	Time            string `json:"time"`
	RequestID       string `json:"request_id,omitempty"`
	AskedModel      string `json:"asked_model"`
	ModelID         string `json:"model_id,omitempty"`
	ServedID        string `json:"served_id,omitempty"`
	ServedName      string `json:"served_name,omitempty"`
	Identity        string `json:"identity,omitempty"`
	GoogleModel     string `json:"google_model,omitempty"`
	EffectiveModel  string `json:"effective_model"`
	GoogleIndicated bool   `json:"google_indicated"`
	PromptBytes     int    `json:"prompt_bytes"`
	ImageCount      int    `json:"image_count,omitempty"`
	ThoughtBytes    int    `json:"thought_bytes"`
	TextBytes       int    `json:"text_bytes"`
	InputTokens     int    `json:"input_tokens"`
	ThoughtTokens   int    `json:"thought_tokens"`
	TextTokens      int    `json:"text_tokens"`
	TotalTokens     int    `json:"total_tokens"`
	ContextWindow   int    `json:"context_window,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	Error           string `json:"error,omitempty"`
}

// TokenRecordFromCall builds a TokenRecord from a CallRecord using Google's
// indicated model and request byte/rune figures.
func TokenRecordFromCall(r CallRecord, catalogNames map[string]string) TokenRecord {
	PopulateCallTokens(&r, catalogNames)
	askedLabel := r.Model
	if catalogNames != nil && r.ModelID != "" {
		if n := strings.TrimSpace(catalogNames[r.ModelID]); n != "" {
			askedLabel = n
		}
	}
	gModel, effModel, indicated := ResolveEffectiveModel(askedLabel, r.ServedID, r.ServedName, catalogNames)
	if gModel == "" && r.GoogleModel != "" {
		gModel, effModel, indicated = r.GoogleModel, r.GoogleModel, true
	}
	return TokenRecord{
		Time:            r.Time,
		RequestID:       r.RequestID,
		AskedModel:      r.Model,
		ModelID:         r.ModelID,
		ServedID:        r.ServedID,
		ServedName:      r.ServedName,
		Identity:        r.Identity,
		GoogleModel:     gModel,
		EffectiveModel:  effModel,
		GoogleIndicated: indicated,
		PromptBytes:     r.PromptBytes,
		ImageCount:      r.ImageCount,
		ThoughtBytes:    r.ThoughtBytes,
		TextBytes:       r.TextBytes,
		InputTokens:     r.InputTokens,
		ThoughtTokens:   r.ThoughtTokens,
		TextTokens:      r.TextTokens,
		TotalTokens:     r.TotalTokens,
		ContextWindow:   ModelContextWindow(effModel),
		DurationMS:      r.DurationMS,
		Error:           r.Error,
	}
}

// TokenTotals summarizes token usage over one time window.
type TokenTotals struct {
	Requests      int   `json:"requests"`
	InputTokens   int64 `json:"input_tokens"`
	ThoughtTokens int64 `json:"thought_tokens"`
	TextTokens    int64 `json:"text_tokens"`
	TotalTokens   int64 `json:"total_tokens"`
	MaxInput      int   `json:"max_input_tokens"`
}

// TokenModelSummary summarizes estimated tokens attributed to one model across 5h, 12h, and all saved history.
type TokenModelSummary struct {
	EffectiveModel  string      `json:"effective_model"`
	GoogleIndicated bool        `json:"google_indicated"`
	ServedID        string      `json:"served_id,omitempty"`
	ContextWindow   int         `json:"context_window"`
	Window5h        TokenTotals `json:"window_5h"`
	Window12h       TokenTotals `json:"window_12h"`
	AllSaved        TokenTotals `json:"all_saved"`
}

// TokenUsageSnapshot is the UI-ready aggregate of past persisted token usage.
type TokenUsageSnapshot struct {
	Rule          string              `json:"rule"`
	Persisted     bool                `json:"persisted"`
	FileName      string              `json:"file_name,omitempty"`
	RecordsCount  int                 `json:"records_count"`
	Since         string              `json:"since,omitempty"`
	Totals5h      TokenTotals         `json:"totals_5h"`
	Totals12h     TokenTotals         `json:"totals_12h"`
	TotalsAll     TokenTotals         `json:"totals_all"`
	ByGoogleModel []TokenModelSummary `json:"by_google_model"`
}

const tokenRecordKeep = 5000

type tokenLedger struct {
	mu   sync.Mutex
	path string
	recs []TokenRecord
	seen map[string]struct{}
}

func tokenRecordKey(r TokenRecord) string {
	return r.Time + "|" + r.RequestID
}

// loadTokens reads TokensFile at startup and backfills any past calls from calls.jsonl
// that are not yet stored in TokensFile so historical usage is preserved.
func (c *Client) loadTokens() {
	path := c.cfg.TokensFile
	catalogNames := c.catalogNamesByID()

	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()
	c.tokens.path = path
	if c.tokens.seen == nil {
		c.tokens.seen = map[string]struct{}{}
	}
	if path != "" {
		if f, err := os.Open(path); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				var tr TokenRecord
				if json.Unmarshal(sc.Bytes(), &tr) == nil && (tr.EffectiveModel != "" || tr.AskedModel != "") {
					if tr.ContextWindow == 0 {
						tr.ContextWindow = ModelContextWindow(tr.EffectiveModel)
					}
					c.tokens.recs = append(c.tokens.recs, tr)
					c.tokens.seen[tokenRecordKey(tr)] = struct{}{}
				}
			}
			_ = f.Close()
		}
	}

	// Backfill any past requests loaded from calls.jsonl that are not yet in tokens.jsonl.
	c.calls.mu.Lock()
	pastCalls := make([]CallRecord, len(c.calls.recs))
	copy(pastCalls, c.calls.recs)
	c.calls.mu.Unlock()

	var missing []TokenRecord
	for _, r := range pastCalls {
		tr := TokenRecordFromCall(r, catalogNames)
		key := tokenRecordKey(tr)
		if _, ok := c.tokens.seen[key]; ok {
			continue
		}
		c.tokens.seen[key] = struct{}{}
		c.tokens.recs = append(c.tokens.recs, tr)
		missing = append(missing, tr)
	}
	if len(missing) > 0 && path != "" {
		c.appendTokenLinesLocked(missing)
	}
	if len(c.tokens.recs) > tokenRecordKeep {
		c.tokens.recs = c.tokens.recs[len(c.tokens.recs)-tokenRecordKeep:]
	}
}

func (c *Client) recordTokenCall(r CallRecord, catalogNames map[string]string) {
	tr := TokenRecordFromCall(r, catalogNames)
	key := tokenRecordKey(tr)

	c.tokens.mu.Lock()
	defer c.tokens.mu.Unlock()
	if c.tokens.seen == nil {
		c.tokens.seen = map[string]struct{}{}
	}
	if _, ok := c.tokens.seen[key]; ok {
		return
	}
	c.tokens.seen[key] = struct{}{}
	c.tokens.recs = append(c.tokens.recs, tr)
	if len(c.tokens.recs) > tokenRecordKeep {
		c.tokens.recs = c.tokens.recs[len(c.tokens.recs)-tokenRecordKeep:]
	}
	if c.tokens.path != "" {
		c.appendTokenLinesLocked([]TokenRecord{tr})
	}
}

func (c *Client) appendTokenLinesLocked(trs []TokenRecord) {
	if c.tokens.path == "" || len(trs) == 0 {
		return
	}
	f, err := os.OpenFile(c.tokens.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		c.log.Warn("token ledger write failed", "path", c.tokens.path, "err", err)
		return
	}
	defer f.Close()
	for _, tr := range trs {
		line, err := json.Marshal(tr)
		if err != nil {
			continue
		}
		_, _ = f.Write(append(line, '\n'))
	}
}

func addTokenTotals(t *TokenTotals, tr TokenRecord) {
	t.Requests++
	t.InputTokens += int64(tr.InputTokens)
	t.ThoughtTokens += int64(tr.ThoughtTokens)
	t.TextTokens += int64(tr.TextTokens)
	t.TotalTokens += int64(tr.TotalTokens)
	if tr.InputTokens > t.MaxInput {
		t.MaxInput = tr.InputTokens
	}
}

// TokenUsageSummary aggregates estimated token usage across 5h, 12h, and all
// saved past requests, grouped by the model indicated by Google.
func (c *Client) TokenUsageSummary(now time.Time) TokenUsageSnapshot {
	if now.IsZero() {
		now = time.Now()
	}
	cut5 := now.Add(-5 * time.Hour)
	cut12 := now.Add(-12 * time.Hour)
	byID := c.catalogNamesByID()

	c.tokens.mu.Lock()
	recs := make([]TokenRecord, len(c.tokens.recs))
	copy(recs, c.tokens.recs)
	path := c.tokens.path
	c.tokens.mu.Unlock()

	type groupKey struct {
		model     string
		indicated bool
	}
	groups := map[groupKey]*TokenModelSummary{}
	var totals5, totals12, totalsAll TokenTotals
	earliest := ""

	for _, tr := range recs {
		if earliest == "" || (tr.Time != "" && tr.Time < earliest) {
			earliest = tr.Time
		}
		effModel, indicated := tr.EffectiveModel, tr.GoogleIndicated
		if !indicated && tr.ServedID != "" && byID[tr.ServedID] != "" {
			effModel, indicated = byID[tr.ServedID], true
		} else if tr.GoogleIndicated && tr.ServedName == "" && tr.ServedID != "" && effModel == tr.ServedID && byID[tr.ServedID] != "" {
			effModel = byID[tr.ServedID]
		} else if !indicated && tr.ModelID != "" && byID[tr.ModelID] != "" && effModel == tr.AskedModel {
			effModel = byID[tr.ModelID]
		}
		k := groupKey{model: effModel, indicated: indicated}
		g := groups[k]
		if g == nil {
			cw := tr.ContextWindow
			if cw == 0 {
				cw = ModelContextWindow(effModel)
			}
			g = &TokenModelSummary{
				EffectiveModel:  effModel,
				GoogleIndicated: indicated,
				ServedID:        tr.ServedID,
				ContextWindow:   cw,
			}
			groups[k] = g
		} else if g.ServedID == "" && tr.ServedID != "" {
			g.ServedID = tr.ServedID
		}

		addTokenTotals(&g.AllSaved, tr)
		addTokenTotals(&totalsAll, tr)
		if t, err := time.Parse(time.RFC3339, tr.Time); err == nil {
			if !t.Before(cut12) {
				addTokenTotals(&g.Window12h, tr)
				addTokenTotals(&totals12, tr)
			}
			if !t.Before(cut5) {
				addTokenTotals(&g.Window5h, tr)
				addTokenTotals(&totals5, tr)
			}
		}
	}

	list := make([]TokenModelSummary, 0, len(groups))
	for _, g := range groups {
		list = append(list, *g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Window5h.TotalTokens != list[j].Window5h.TotalTokens {
			return list[i].Window5h.TotalTokens > list[j].Window5h.TotalTokens
		}
		if list[i].AllSaved.TotalTokens != list[j].AllSaved.TotalTokens {
			return list[i].AllSaved.TotalTokens > list[j].AllSaved.TotalTokens
		}
		if list[i].GoogleIndicated != list[j].GoogleIndicated {
			return list[i].GoogleIndicated
		}
		return list[i].EffectiveModel < list[j].EffectiveModel
	})

	fileName := ""
	if path != "" {
		fileName = filepath.Base(path)
	}
	return TokenUsageSnapshot{
		Rule:          "~1 token / 4 caractères (entrée + pensées + réponse) + 258 tok/image · imputé au modèle indiqué par Google",
		Persisted:     path != "",
		FileName:      fileName,
		RecordsCount:  len(recs),
		Since:         earliest,
		Totals5h:      totals5,
		Totals12h:     totals12,
		TotalsAll:     totalsAll,
		ByGoogleModel: list,
	}
}
