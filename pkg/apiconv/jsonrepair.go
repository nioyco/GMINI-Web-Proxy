package apiconv

import (
	"encoding/json"
	"sort"
	"strings"
	"sync/atomic"
)

// Tolerant reading of the JSON a model writes in a tool call.
//
// Gemini web sometimes writes JSON that a strict decoder rejects although its
// meaning is clear: a LaTeX backslash inside a string ("$\Lambda$" → invalid
// escape \L), or a raw line break inside a string. repairJSONStrings fixes only
// those two cases, and only inside string literals: an invalid escape gets its
// backslash doubled (the text keeps its backslash), a raw control character is
// written as its escape. Valid JSON is returned unchanged.

// repairJSONStrings returns the repaired text and the positions (in the repaired
// text) of every byte that was inserted, in increasing order.
func repairJSONStrings(s string) (string, []int) {
	var sb strings.Builder
	var ins []int
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			sb.WriteByte(c)
			continue
		}
		switch {
		case c == '"':
			inStr = false
			sb.WriteByte(c)
		case c == '\\':
			valid := false
			if i+1 < len(s) {
				switch n := s[i+1]; n {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					valid = true
				case 'u':
					valid = i+5 < len(s) && isHex4(s[i+2:i+6])
				}
			}
			if valid {
				sb.WriteByte(c)
				sb.WriteByte(s[i+1])
				i++
			} else {
				ins = append(ins, sb.Len())
				sb.WriteString(`\\`)
			}
		case c == '\n', c == '\r', c == '\t':
			ins = append(ins, sb.Len())
			sb.WriteByte('\\')
			sb.WriteByte(map[byte]byte{'\n': 'n', '\r': 'r', '\t': 't'}[c])
		default:
			sb.WriteByte(c)
		}
	}
	if len(ins) == 0 {
		return s, nil
	}
	return sb.String(), ins
}

func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return len(s) == 4
}

// closeJSON appends the single closing bracket a JSON value lacks at its end ("}"
// or "]"), as happens when a model miscounts the braces of a nested tool call. It
// refuses (ok false) when the text ends inside a string or right after a comma or
// colon, or when more than one bracket is missing — the value is then truly cut —
// or when nothing is missing.
func closeJSON(s string) (string, bool) {
	var stack []byte
	inStr := false
	last := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			last = c
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return s, false
			}
			stack = stack[:len(stack)-1]
		}
		if !isJSONSpace(c) {
			last = c
		}
	}
	// Exactly one missing bracket is a miscount; more means the value was cut
	// while still being written (arguments incomplete): never guessed.
	if inStr || len(stack) != 1 || last == ',' || last == ':' {
		return s, false
	}
	out := []byte(strings.TrimRight(s, " \t\r\n"))
	for i := len(stack) - 1; i >= 0; i-- {
		out = append(out, stack[i])
	}
	return string(out), true
}

// originalOffset maps an offset in the repaired text back to the original text.
func originalOffset(off int, ins []int) int {
	return off - sort.SearchInts(ins, off)
}

// jsonValues decodes the consecutive JSON values of body. When the strict decoder
// finds none, the repaired body is tried; repaired reports that this was needed.
func jsonValues(body string) (vals []json.RawMessage, repaired bool) {
	read := func(s string) []json.RawMessage {
		dec := json.NewDecoder(strings.NewReader(s))
		var out []json.RawMessage
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				break
			}
			out = append(out, raw)
		}
		return out
	}
	if vals = read(body); len(vals) > 0 {
		return vals, false
	}
	fixed, ins := repairJSONStrings(body)
	if len(ins) > 0 {
		if vals = read(fixed); len(vals) > 0 {
			return vals, true
		}
	}
	// A single value whose closing brackets are missing.
	if closed, ok := closeJSON(fixed); ok && json.Valid([]byte(closed)) {
		return []json.RawMessage{json.RawMessage(closed)}, true
	}
	return nil, false
}

// dropAfterToolCall removes the text a model writes after its first tool call
// (default on). The tool prompt asks for the tool_call block(s) only; what follows
// is noise (a simulated result, leaked reasoning) that clients would display.
var dropAfterToolCall atomic.Bool

func init() { dropAfterToolCall.Store(true) }

// SetDropTextAfterToolCall switches the removal of text following a tool call.
func SetDropTextAfterToolCall(on bool) { dropAfterToolCall.Store(on) }

// DropTextAfterToolCall reports whether text following a tool call is removed.
func DropTextAfterToolCall() bool { return dropAfterToolCall.Load() }
