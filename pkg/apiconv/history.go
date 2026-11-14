package apiconv

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
)

// Input hardening for the flattened history (AUDIT-13 T-03, T-04).
//
// The history is flattened into one prompt with line-start markers ([User]:,
// [Assistant]:, [System instruction]:, [Tool result for X]:). Contents supplied by
// the client (a file read by a tool, a tool result, a user paste) may contain the
// same markers at the start of a line and forge a turn. They are neutralized by
// prefixing the marker's "[" with a backslash; existing backslashes already in front
// of such a marker get one more, so the transformation is reversible by removing one
// backslash before a "[" marker (see unneutralizeMarkers).

// markerNeutralization switches the neutralization of history markers inside input
// contents. Default on. The server config is expected to expose it (suggested
// variable GEMINI_NEUTRALIZE_HISTORY_MARKERS=0 to turn it off); config.go is not
// touched by this package.
var markerNeutralization atomic.Bool

func init() { markerNeutralization.Store(true) }

// SetMarkerNeutralization enables or disables the neutralization of history markers
// in input contents.
func SetMarkerNeutralization(on bool) { markerNeutralization.Store(on) }

// MarkerNeutralization reports whether history markers are neutralized.
func MarkerNeutralization() bool { return markerNeutralization.Load() }

// markerLineRe matches a history marker at the start of a line (start of text, after
// \n or after \r), allowing leading blanks as the output echo filter does. Groups:
// 1 line start, 2 blanks, 3 existing backslashes, 4 the marker text after "[".
var markerLineRe = regexp.MustCompile(`(?m)(^|\r)([ \t]*)(\\*)\[(System instruction\]:|User\]:|Assistant\]:|Tool result for )`)

// neutralizeMarkers escapes the history markers found at the start of a line in an
// input content. It is a no-op when the switch is off.
func neutralizeMarkers(s string) string {
	if !MarkerNeutralization() || !strings.Contains(s, "[") {
		return s
	}
	return markerLineRe.ReplaceAllString(s, `${1}${2}${3}\[${4}`)
}

var escapedMarkerRe = regexp.MustCompile(`(?m)(^|\r)([ \t]*)\\(\\*)\[(System instruction\]:|User\]:|Assistant\]:|Tool result for )`)

// unneutralizeMarkers is the inverse of neutralizeMarkers (used by tests and by
// anyone reading a prompt back).
func unneutralizeMarkers(s string) string {
	return escapedMarkerRe.ReplaceAllString(s, `${1}${2}${3}[${4}`)
}

// jsonQuote renders s as a JSON string literal without HTML escaping.
func jsonQuote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// safeLabel makes a tool name or call id safe inside "[Tool result for X]:" : it
// cannot contain a line break or the closing bracket.
func safeLabel(s string) string {
	if !strings.ContainsAny(s, "\r\n]") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == ']' {
			return '_'
		}
		return r
	}, s)
}

// sanitizeToolName keeps a model-supplied tool name loggable: plain identifier
// characters only, capped at 64 bytes.
func sanitizeToolName(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s) && sb.Len() < 64; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == ':', c == '-':
			sb.WriteByte(c)
		default:
			sb.WriteByte('?')
		}
	}
	return sb.String()
}

// toolNameIndex maps tool_call ids to function names from the assistant messages
// seen so far, so a later tool-result message that carries only tool_call_id can be
// labelled with the function name.
type toolNameIndex map[string]string

func (ix toolNameIndex) addCalls(calls []ToolCall) {
	for _, c := range calls {
		if c.ID != "" && c.Function.Name != "" {
			ix[c.ID] = c.Function.Name
		}
	}
}

// toolResultLabel picks the label of a tool-result message: its own name, else the
// name of the call it answers (via tool_call_id), else the id itself.
func (ix toolNameIndex) toolResultLabel(m Message) string {
	if m.Name != "" {
		return m.Name
	}
	if n, ok := ix[m.ToolCallID]; ok && m.ToolCallID != "" {
		return n
	}
	return m.ToolCallID
}

// declaredNameSet returns the set of declared tool names, or nil when none are
// declared (validation off, legacy behaviour).
func declaredNameSet(declared []DeclaredTool) map[string]bool {
	if len(declared) == 0 {
		return nil
	}
	set := make(map[string]bool, len(declared))
	for _, d := range declared {
		set[d.Name] = true
	}
	return set
}

// rejectUndeclared reports the names of calls not in set (nil set accepts all).
func rejectUndeclared(calls []ToolCall, set map[string]bool) []string {
	if set == nil {
		return nil
	}
	var bad []string
	for _, c := range calls {
		if !set[c.Function.Name] {
			bad = append(bad, c.Function.Name)
		}
	}
	return bad
}

// noteUndeclared records a refused call in the report (sanitized name, no values).
func noteUndeclared(rep *ParseReport, tag string, declared int, names []string) {
	for _, n := range names {
		rep.Unparsed = append(rep.Unparsed, Unparsed{
			Tag: tag, Declared: declared,
			Reason: "tool name not declared: " + sanitizeToolName(n),
		})
	}
}
