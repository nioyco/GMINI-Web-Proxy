package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Full-content tracing (opt-in, GEMINI_TRACE_DIR). One JSON file per generation
// holds the prompt sent to Gemini and the reply received, so a failed or strange
// exchange can be analysed afterwards. The files contain conversation content and
// are therefore private (directory 0700, files 0600) and never written unless the
// directory is configured. They never contain cookies, the XSRF token or API keys.

// DefaultTraceMaxFiles is the number of trace files kept when TraceMaxFiles is unset.
const DefaultTraceMaxFiles = 200

type traceResult struct {
	Stream      bool
	Text        string
	Thought     string
	Err         error
	Start       time.Time
	Frames      int
	FirstTextMS int64
	LastFrameMS int64
	Raw         []string // raw response lines (only stored when TraceRaw is on)
}

// traceRawMax caps the raw response kept per trace file.
const traceRawMax = 4 << 20

type traceFile struct {
	Time          string `json:"time"`
	RequestID     string `json:"request_id"`
	Model         string `json:"model"`
	ModelID       string `json:"model_id"`
	Level         int    `json:"thinking_level"`
	Mode          int    `json:"mode"`
	Stream        bool   `json:"stream"`
	DurationMS    int64  `json:"duration_ms"`
	Frames        int    `json:"frames"`
	FirstTextMS   int64  `json:"first_text_ms"`
	LastFrameMS   int64  `json:"last_frame_ms"`
	Error         string `json:"error,omitempty"`
	PromptBytes   int    `json:"prompt_bytes"`
	ResponseBytes int    `json:"response_bytes"`
	ThoughtBytes  int    `json:"thought_bytes"`
	Prompt        string `json:"prompt"`
	Response      string `json:"response"`
	Thought       string `json:"thought,omitempty"`
	// RawFrames is the unparsed response (GEMINI_TRACE_RAW=1): it lets a change in
	// Google's response layout be studied offline. It may hold conversation ids.
	RawFrames []string `json:"raw_frames,omitempty"`
}

var traceMu sync.Mutex

// TraceEnabled reports whether full-content tracing is on.
func (c *Client) TraceEnabled() bool { return c.cfg.TraceDir != "" }

// writeTrace stores one exchange. Failures are logged once per call and never
// affect the request.
func (c *Client) writeTrace(met *CallMetrics, p GenParams, r traceResult) {
	dir := c.cfg.TraceDir
	if dir == "" {
		return
	}
	tf := traceFile{
		Time: r.Start.Format(time.RFC3339), Level: p.Level, Mode: p.Mode, Stream: r.Stream,
		DurationMS: time.Since(r.Start).Milliseconds(), Frames: r.Frames,
		FirstTextMS: r.FirstTextMS, LastFrameMS: r.LastFrameMS,
		PromptBytes: len(p.Prompt), ResponseBytes: len(r.Text), ThoughtBytes: len(r.Thought),
		Prompt: p.Prompt, Response: r.Text, Thought: r.Thought,
	}
	if c.cfg.TraceRaw {
		tf.RawFrames = r.Raw
	}
	if met != nil {
		tf.RequestID = met.ID
	}
	if p.Model != nil {
		tf.Model, tf.ModelID = p.Model.Name, p.Model.ModelID
	}
	if r.Err != nil {
		tf.Error = cleanErr(r.Err).Error()
	}
	data, err := json.MarshalIndent(tf, "", " ")
	if err != nil {
		return
	}
	traceMu.Lock()
	defer traceMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.log.Warn("trace not written", "err", err)
		return
	}
	_ = os.Chmod(dir, 0o700)
	id := tf.RequestID
	if id == "" {
		id = "noid"
	}
	name := r.Start.Format("20060102T150405.000") + "-" + id + ".json"
	if err := writeFileAtomic(filepath.Join(dir, name), data, 0o600); err != nil {
		c.log.Warn("trace not written", "err", err)
		return
	}
	c.pruneTraces(dir)
}

// pruneTraces keeps the newest TraceMaxFiles trace files. Caller holds traceMu.
func (c *Client) pruneTraces(dir string) {
	max := c.cfg.TraceMaxFiles
	if max <= 0 {
		max = DefaultTraceMaxFiles
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && len(e.Name()) > 20 && e.Name()[8] == 'T' {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // names start with the timestamp
	for len(names) > max {
		_ = os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
