package gemini

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Who answered? Google never states which model served a web reply in a field
// the proxy knows, and it may silently serve another model than the one asked
// for (observed 2026-11-14: once the web usage limit is reached, replies come
// back fast and without thoughts). This file keeps a content-free journal of
// generations and derives two signals from it:
//
//   - identity: catalogue model ids found anywhere in the raw response. If the
//     reply names the requested id it is "confirmed"; if it names another
//     catalogue id only, "other_model"; otherwise "not_stated".
//   - suspect: the model normally streams thoughts (learnt from earlier calls)
//     and this reply carried none.
//
// Both are indications, not proof. Nothing here stores prompt or reply text.

// CallRecord is one generation, described by sizes, timings and ids only.
type CallRecord struct {
	Time           string `json:"time"`
	RequestID      string `json:"request_id,omitempty"`
	BinaryTag      string `json:"binary_tag,omitempty"`
	Model          string `json:"model"`
	ModelID        string `json:"model_id,omitempty"`
	Level          int    `json:"thinking_level"`
	Mode           int    `json:"mode"`
	Stream         bool   `json:"stream"`
	DurationMS     int64  `json:"duration_ms"`
	FirstTextMS    int64  `json:"first_text_ms"`    // -1: unknown / none
	FirstThoughtMS int64  `json:"first_thought_ms"` // -1: unknown / none
	Frames         int    `json:"frames"`
	PromptBytes    int    `json:"prompt_bytes"`
	ImageCount     int    `json:"image_count,omitempty"`
	TextBytes      int    `json:"text_bytes"`
	ThoughtBytes   int    `json:"thought_bytes"`
	GoogleModel    string `json:"google_model,omitempty"`
	InputTokens    int    `json:"input_tokens,omitempty"`
	ThoughtTokens  int    `json:"thought_tokens,omitempty"`
	TextTokens     int    `json:"text_tokens,omitempty"`
	TotalTokens    int    `json:"total_tokens,omitempty"`
	// ServedID / ServedName: the model Google names in the reply itself (response
	// body positions 39 and 42, located in real traces on 2026-11-14).
	ServedID     string   `json:"served_id,omitempty"`
	ServedName   string   `json:"served_name,omitempty"`
	SeenModelIDs []string `json:"seen_model_ids"`  // catalogue ids present in the raw reply
	OtherHexIDs  int      `json:"other_hex_ids"`   // 16-hex tokens that are not catalogue ids
	Identity     string   `json:"identity"`        // confirmed | other_model | name_only | canned_error | not_stated
	Suspect      string   `json:"suspect"`         // "" | no_thoughts
	Error        string   `json:"error,omitempty"` // error kind, never content
}

// hexIDRe matches a bare 16-hex token (the shape of a model id). A leading "_"
// is excluded: conversation and reply ids (c_…, r_…, rc_…) have that shape too.
var hexIDRe = regexp.MustCompile(`(?:^|[^0-9A-Za-z_])([0-9a-f]{16})(?:[^0-9A-Za-z]|$)`)

// idScan collects 16-hex tokens of a raw response (bounded).
type idScan struct {
	seen                 map[string]struct{}
	servedID, servedName string
	errorMarker          bool
	cannedError          bool
}

// Positions of the error marker, model id, and model name in the response body.
const (
	errorMarkerIndex = 31
	servedIDIndex    = 39
	servedNameIndex  = 42
)

// frameMetaFromLine reads the model id, name, and inner[31] error flag Google
// states in a response line.
func frameMetaFromLine(line string) (id, name string, errorMarker bool) {
	if !strings.Contains(line, `"wrb.fr"`) {
		return "", "", false
	}
	var arr []any
	if json.Unmarshal([]byte(line), &arr) != nil {
		return "", "", false
	}
	first, ok := indexSlice(arr, 0)
	if !ok {
		return "", "", false
	}
	innerStr, ok := indexString(first, 2)
	if !ok {
		return "", "", false
	}
	var body []any
	if json.Unmarshal([]byte(innerStr), &body) != nil {
		return "", "", false
	}
	id, _ = indexAny(body, servedIDIndex).(string)
	name, _ = indexAny(body, servedNameIndex).(string)
	if b, ok := indexAny(body, errorMarkerIndex).(bool); ok && b {
		errorMarker = true
	}
	if !hexIDOnlyRe.MatchString(id) {
		id = ""
	}
	if len(name) > 60 {
		name = ""
	}
	return id, name, errorMarker
}

var hexIDOnlyRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

var (
	cannedWidgetRe = regexp.MustCompile(`^(\s*<GenerateWidget\b[^>]*/>\s*)+$`)
	cannedGlitchRe = regexp.MustCompile(`(?i)can['’]t use workspace because|turn on these settings to continue`)
	cannedErrorRe  = regexp.MustCompile(`(?i)^(?:i\s+seem\s+to\s+be\s+encountering\s+an\s+error|i\s+encountered\s+an\s+error(?:\s+doing\s+what\s+you\s+asked)?|i['’]?m\s+(?:sorry,?\s+)?(?:having\s+trouble|encountering\s+an?\s+error)|i['’]?m\s+having\s+a\s+hard\s+time\s+(?:fulfilling|completing|processing)\s+(?:your|that|this)\s+request|(?:sorry,?\s+)?something\s+went\s+wrong|an\s+error\s+occurred)\b(?:\s*[.,!]\s*(?:can\s+i\s+try\s+something\s+else\s+for\s+you|can\s+i\s+help\s+you\s+with\s+something\s+else(?:\s+instead)?|could\s+you\s+try\s+again(?:\s+later|\s+in\s+a\s+moment)?|please\s+try\s+(?:your\s+request\s+)?again(?:\s+later|\s+in\s+a\s+moment)?|try\s+something\s+else))?[.!?]?$|^(?:je\s+ne\s+suis\s+qu['’]une\s+ia\s+basée\s+sur\s+du\s+texte\.?\s*je\s+ne\s+peux\s+pas\s+vous\s+aider\s+à\s+ce\s+sujet\.?|ce\s+que\s+vous\s+demandez\s+va\s+au-delà\s+de\s+ce\s+que\s+permet\s+ma\s+programmation\.?\s*je\s+génère\s+juste\s+du\s+texte\.?)$`)
)

// IsCannedErrorReply reports whether text is a short canned upstream error:
// primarily when Google flagged the response with inner[31]=true on a short,
// non-fenced, non-widget/workspace reply, with known EN/FR canned phrases as
// fallback when frame metadata is unavailable.
func IsCannedErrorReply(text string, hasErrorMarker bool) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 250 {
		return false
	}
	if strings.Contains(t, "```") {
		return false
	}
	if cannedWidgetRe.MatchString(t) || cannedGlitchRe.MatchString(t) {
		return false
	}
	if hasErrorMarker {
		return true
	}
	return cannedErrorRe.MatchString(t)
}

func (s *idScan) add(line string) {
	if s.servedID == "" || !s.errorMarker {
		id, name, errMarker := frameMetaFromLine(line)
		if s.servedID == "" && id != "" {
			s.servedID, s.servedName = id, name
		}
		if errMarker {
			s.errorMarker = true
		}
	}
	if len(s.seen) >= 64 {
		return
	}
	for _, m := range hexIDRe.FindAllStringSubmatch(line, 32) {
		id := m[1]
		if !strings.ContainsAny(id, "abcdef") || !strings.ContainsAny(id, "0123456789") {
			continue // all-digit or all-letter runs are not model ids
		}
		if s.seen == nil {
			s.seen = map[string]struct{}{}
		}
		s.seen[id] = struct{}{}
	}
}

// classify splits the scanned tokens into catalogue ids and others, and returns
// the identity verdict for the requested id.
func (s *idScan) classify(requested string, catalogue map[string]struct{}) (seen []string, other int, verdict string) {
	seen = []string{}
	for id := range s.seen {
		if _, ok := catalogue[id]; ok {
			seen = append(seen, id)
		} else {
			other++
		}
	}
	sort.Strings(seen)
	verdict = "not_stated"
	for _, id := range seen {
		if id == requested {
			return seen, other, "confirmed"
		}
	}
	if len(seen) > 0 && requested != "" {
		verdict = "other_model"
	}
	return seen, other, verdict
}

const (
	callLogKeep     = 1000 // records kept in memory and served to the page
	callFileMax     = 3000 // lines before the journal file is compacted
	callFileCompact = 1500
)

// ModelIdentityStatus tracks the latest identity verdict for one model and
// since when the served id has differed (cleared upon a confirmed reply).
type ModelIdentityStatus struct {
	Model         string
	ModelID       string
	LastVerdict   string
	MismatchSince time.Time
	ServedID      string
	ServedName    string
	LastTime      time.Time
}

// callJournal is the in-memory journal, optionally mirrored to a JSONL file.
type callJournal struct {
	mu      sync.Mutex
	recs    []CallRecord
	byModel map[string]ModelIdentityStatus
	path    string
	lines   int
}

// loadCalls reads the journal file (if any). Called once at startup.
func (c *Client) loadCalls() {
	path := c.cfg.CallsFile
	if path == "" {
		return
	}
	type confirmedCall struct {
		id, name string
		at       time.Time
	}
	var confirmedPairs []confirmedCall
	func() {
		c.calls.mu.Lock()
		defer c.calls.mu.Unlock()
		c.calls.path = path
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			var r CallRecord
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.Model != "" {
				PopulateCallTokens(&r, nil)
				c.calls.recs = append(c.calls.recs, r)
				c.calls.lines++
				t, _ := time.Parse(time.RFC3339, r.Time)
				c.calls.updateModelIdentity(r, t)
				if r.Identity == "confirmed" && r.ServedID != "" && r.ServedName != "" {
					confirmedPairs = append(confirmedPairs, confirmedCall{id: r.ServedID, name: r.ServedName, at: t})
				}
				if len(c.calls.recs) > callFileCompact {
					c.calls.recs = c.calls.recs[len(c.calls.recs)-callLogKeep:]
				}
			}
		}
		if len(c.calls.recs) > callLogKeep {
			c.calls.recs = c.calls.recs[len(c.calls.recs)-callLogKeep:]
		}
	}()
	for _, pair := range confirmedPairs {
		c.syncConfirmedServedNameAt(pair.id, pair.name, pair.at)
	}
}

func (c *Client) catalogNamesByID() map[string]string {
	byID := map[string]string{}
	for _, m := range c.ListModels() {
		if m.ModelID == "" {
			continue
		}
		label := m.Description
		if label == "" {
			label = m.DisplayName
		}
		if label != "" {
			byID[m.ModelID] = label
		}
	}
	for _, w := range c.WithdrawnModels() {
		if w.Model.ModelID == "" || byID[w.Model.ModelID] != "" {
			continue
		}
		label := w.Model.Description
		if label == "" {
			label = w.Model.DisplayName
		}
		if label != "" {
			byID[w.Model.ModelID] = label
		}
	}
	return byID
}

// RecentCalls returns the newest generations first (content-free).
func (c *Client) RecentCalls() []CallRecord {
	byID := c.catalogNamesByID()
	c.calls.mu.Lock()
	defer c.calls.mu.Unlock()
	out := make([]CallRecord, len(c.calls.recs))
	for i, r := range c.calls.recs {
		PopulateCallTokens(&r, byID)
		out[len(out)-1-i] = r
	}
	return out
}

// thinksUsually reports whether model has streamed thoughts in at least two of
// its earlier successful calls. Caller holds calls.mu.
func (l *callJournal) thinksUsually(model string) bool {
	n := 0
	for _, r := range l.recs {
		if r.Model == model && r.Error == "" && r.ThoughtBytes > 0 {
			n++
			if n >= 2 {
				return true
			}
		}
	}
	return false
}

// recordCall finalises and stores one record, and logs it.
func (c *Client) recordCall(r CallRecord, scan *idScan) {
	if r.BinaryTag == "" {
		r.BinaryTag = c.BinaryTag()
	}
	catalogue := map[string]struct{}{}
	askedName := ""
	askedDisplayName := ""
	for _, m := range c.ListModels() {
		if m.ModelID != "" {
			catalogue[m.ModelID] = struct{}{}
		}
		if m.Name == r.Model {
			askedName = m.Description
			askedDisplayName = m.DisplayName
		}
	}
	for _, w := range c.WithdrawnModels() {
		if w.Model.Name == r.Model {
			if askedName == "" {
				askedName = w.Model.Description
			}
			if askedDisplayName == "" {
				askedDisplayName = w.Model.DisplayName
			}
		}
	}
	r.SeenModelIDs, r.OtherHexIDs, r.Identity = scan.classify(r.ModelID, catalogue)
	if scan.servedID != "" {
		// Google names the model in the reply: that is the verdict.
		r.ServedID, r.ServedName = scan.servedID, scan.servedName
		r.Identity = "confirmed"
		if r.ModelID != "" && r.ServedID != r.ModelID {
			if IsAutoModel(r.ModelID, r.Model, askedDisplayName) {
				r.Identity = "routed_auto"
			} else {
				r.Identity = "other_model"
				// Seen 2026-11-15 on "4 Carbon": Google's reply carried the asked
				// model's name with another id. Which of the two is right is unknown.
				if askedName != "" && r.ServedName == askedName {
					r.Identity = "name_only"
				}
			}
		}
	} else if r.Frames == 0 && r.Identity == "confirmed" {
		// Without any parsed wrb.fr frame or position-39 id, seeing the requested
		// hex id elsewhere in the raw payload must not claim "confirmed".
		r.Identity = "not_stated"
	}
	if r.Error == "" && scan != nil && scan.cannedError {
		// "confirmed" requires both the right model id and a genuine reply: a
		// canned upstream error (even bearing the requested model id) gets its own
		// verdict while keeping ServedID / ServedName.
		r.Identity = "canned_error"
	}

	byID := c.catalogNamesByID()
	c.calls.mu.Lock()
	// The absence of thoughts only counts when Google did not name the model: a
	// short answer of the right model can come without thoughts (seen 2026-11-14).
	if r.Identity == "not_stated" && r.Error == "" && r.Stream && r.TextBytes > 0 && r.ThoughtBytes == 0 && c.calls.thinksUsually(r.Model) {
		r.Suspect = "no_thoughts"
	}
	PopulateCallTokens(&r, byID)
	c.calls.recs = append(c.calls.recs, r)
	if len(c.calls.recs) > callLogKeep {
		c.calls.recs = c.calls.recs[len(c.calls.recs)-callLogKeep:]
	}
	c.calls.updateModelIdentity(r, time.Now())
	c.appendCallLocked(r)
	c.calls.mu.Unlock()
	c.recordTokenCall(r, byID)

	// No catalogue check here any more: on 2026-11-15 that extra call returned a
	// one-model catalogue while every model was still offered on the web page,
	// and it made the proxy refuse the others.
	if r.Suspect != "" || r.Identity == "other_model" || r.Identity == "name_only" {
		c.log.Warn("served_model_doubt", "request_id", r.RequestID, "model", r.Model, "model_id", r.ModelID,
			"identity", r.Identity, "served_id", r.ServedID, "served_name", r.ServedName,
			"seen_model_ids", strings.Join(r.SeenModelIDs, ","), "suspect", r.Suspect,
			"first_text_ms", r.FirstTextMS, "thought_bytes", r.ThoughtBytes)
	} else {
		if r.Identity == "confirmed" && r.ServedID != "" && r.ServedName != "" {
			recAt, _ := time.Parse(time.RFC3339, r.Time)
			if prevDesc, changed := c.syncConfirmedServedNameAt(r.ServedID, r.ServedName, recAt); changed {
				c.log.Info("served_name_changed", "request_id", r.RequestID, "model", r.Model, "model_id", r.ModelID,
					"catalog_name", prevDesc, "served_name", r.ServedName, "synced", true)
				c.saveState()
			}
		}
		if c.cfg.LogParams || r.Identity == "canned_error" || r.Identity == "routed_auto" {
			c.log.Info("served_model", "request_id", r.RequestID, "model", r.Model, "identity", r.Identity,
				"served_id", r.ServedID, "served_name", r.ServedName,
				"seen_model_ids", strings.Join(r.SeenModelIDs, ","), "other_hex_ids", r.OtherHexIDs)
		}
	}
}

// appendCallLocked mirrors r to the journal file (0600). Caller holds calls.mu.
func (c *Client) appendCallLocked(r CallRecord) {
	l := &c.calls
	if l.path == "" {
		return
	}
	line, err := json.Marshal(r)
	if err != nil {
		return
	}
	if l.lines >= callFileMax {
		var b strings.Builder
		keep := l.recs
		if len(keep) > callFileCompact {
			keep = keep[len(keep)-callFileCompact:]
		}
		for _, k := range keep {
			if kb, err := json.Marshal(k); err == nil {
				b.Write(kb)
				b.WriteByte('\n')
			}
		}
		if writeFileAtomic(l.path, []byte(b.String()), 0o600) == nil {
			l.lines = len(keep)
		}
		return // r is already part of keep
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err == nil {
		l.lines++
	}
}

func newCallRecord(met *CallMetrics, p GenParams, start time.Time) CallRecord {
	r := CallRecord{Time: start.Format(time.RFC3339), Level: p.Level, Mode: p.Mode,
		PromptBytes: len(p.Prompt), ImageCount: len(p.Images),
		InputTokens:    EstimateTokensForText(p.Prompt) + len(p.Images)*GeminiImageTokens,
		FirstTextMS:    -1,
		FirstThoughtMS: -1,
		DurationMS:     time.Since(start).Milliseconds()}
	if met != nil {
		r.RequestID = met.ID
	}
	if p.Model != nil {
		r.Model, r.ModelID = p.Model.Name, p.Model.ModelID
	}
	return r
}

// ModelIdentities returns the per-model identity verdict summary, sorted by
// model name (no Google call).
func (c *Client) ModelIdentities() []ModelIdentityStatus {
	c.calls.mu.Lock()
	defer c.calls.mu.Unlock()
	if len(c.calls.byModel) == 0 {
		return nil
	}
	out := make([]ModelIdentityStatus, 0, len(c.calls.byModel))
	for _, st := range c.calls.byModel {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// updateModelIdentity updates per-model identity mismatch tracking. Caller holds mu.
func (l *callJournal) updateModelIdentity(r CallRecord, at time.Time) {
	if r.Model == "" || r.Error != "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	if l.byModel == nil {
		l.byModel = map[string]ModelIdentityStatus{}
	}
	prev := l.byModel[r.Model]
	st := ModelIdentityStatus{
		Model:       r.Model,
		ModelID:     r.ModelID,
		LastVerdict: r.Identity,
		ServedID:    r.ServedID,
		ServedName:  r.ServedName,
		LastTime:    at,
	}
	switch r.Identity {
	case "confirmed", "routed_auto":
		st.MismatchSince = time.Time{}
	case "name_only", "other_model":
		if !prev.MismatchSince.IsZero() {
			st.MismatchSince = prev.MismatchSince
		} else {
			st.MismatchSince = at
		}
	default:
		st.MismatchSince = prev.MismatchSince
	}
	l.byModel[r.Model] = st
}

// CallByRequest returns the newest journal record of a proxy request id.
func (c *Client) CallByRequest(id string) (CallRecord, bool) {
	if id == "" {
		return CallRecord{}, false
	}
	c.calls.mu.Lock()
	defer c.calls.mu.Unlock()
	for i := len(c.calls.recs) - 1; i >= 0; i-- {
		if c.calls.recs[i].RequestID == id {
			return c.calls.recs[i], true
		}
	}
	return CallRecord{}, false
}
