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

// repairJSONStringsAndQuotes repairs invalid escapes, raw control chars, and
// unambiguous unescaped double quotes inside JSON string values (such as HTML/Vue
// attributes class="card" inside a write_file Content field). It returns the
// repaired string, sorted insertion offsets, and whether any byte was added.
func repairJSONStringsAndQuotes(s string) (string, []int, bool) {
	var sb strings.Builder
	var ins []int
	var stack []byte
	var expectKey []bool
	inStr := false
	curIsKey := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if !inStr {
			switch c {
			case '"':
				curIsKey = len(stack) > 0 && stack[len(stack)-1] == '}' && len(expectKey) > 0 && expectKey[len(expectKey)-1]
				inStr = true
			case '{':
				stack = append(stack, '}')
				expectKey = append(expectKey, true)
			case '[':
				stack = append(stack, ']')
				expectKey = append(expectKey, false)
			case ':':
				if len(expectKey) > 0 {
					expectKey[len(expectKey)-1] = false
				}
			case ',':
				if len(stack) > 0 && stack[len(stack)-1] == '}' && len(expectKey) > 0 {
					expectKey[len(expectKey)-1] = true
				}
			case '}', ']':
				if len(stack) > 0 && stack[len(stack)-1] == c {
					stack = stack[:len(stack)-1]
					expectKey = expectKey[:len(expectKey)-1]
				}
			}
			sb.WriteByte(c)
			continue
		}

		switch {
		case c == '"':
			if isClosingJSONQuote(s, i, curIsKey, stack) {
				inStr = false
				sb.WriteByte(c)
			} else {
				ins = append(ins, sb.Len())
				sb.WriteString(`\"`)
			}
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
		return s, nil, false
	}
	return sb.String(), ins, true
}

func skipJSONSpaceAt(s string, i int) int {
	for i < len(s) && isJSONSpace(s[i]) {
		i++
	}
	return i
}

// looksLikeJSONObjectKeyColon reports whether s starts (after optional whitespace)
// with a short single-line JSON string key followed by ":".
func looksLikeJSONObjectKeyColon(s string) bool {
	i := skipJSONSpaceAt(s, 0)
	if i >= len(s) || s[i] != '"' {
		return false
	}
	i++
	keyStart := i
	for i < len(s) {
		c := s[i]
		if c == '\n' || c == '\r' || i-keyStart > 64 {
			return false
		}
		if c == '\\' {
			i += 2
			continue
		}
		if c == '"' {
			j := skipJSONSpaceAt(s, i+1)
			return j < len(s) && s[j] == ':'
		}
		i++
	}
	return false
}

// closesValidObjectChain checks whether a closing '}' or ']' at index j closes
// the active JSON container chain cleanly (reaching EOF, next object '{', or a
// valid ", \"key\":" sibling).
func closesValidObjectChain(s string, j int, stack []byte) bool {
	if len(stack) == 0 {
		return true
	}
	tmp := append([]byte(nil), stack...)
	k := j
	for k < len(s) && len(tmp) > 0 {
		k = skipJSONSpaceAt(s, k)
		if k >= len(s) {
			break
		}
		c := s[k]
		if c == tmp[len(tmp)-1] {
			tmp = tmp[:len(tmp)-1]
			k++
			continue
		}
		if c == ',' {
			if tmp[len(tmp)-1] == '}' {
				return looksLikeJSONObjectKeyColon(s[k+1:])
			}
			return true
		}
		return false
	}
	if len(tmp) > 0 {
		return false
	}
	k = skipJSONSpaceAt(s, k)
	return k >= len(s) || s[k] == '{' || s[k] == '['
}

func isClosingJSONQuote(s string, quoteIdx int, curIsKey bool, stack []byte) bool {
	j := skipJSONSpaceAt(s, quoteIdx+1)
	if curIsKey {
		return j < len(s) && s[j] == ':'
	}
	if j >= len(s) {
		return true
	}
	switch s[j] {
	case ',':
		if len(stack) > 0 && stack[len(stack)-1] == '}' {
			return looksLikeJSONObjectKeyColon(s[j+1:])
		}
		return true
	case '}', ']':
		return closesValidObjectChain(s, j, stack)
	default:
		return false
	}
}

// originalOffset maps an offset in the repaired text back to the original text.
func originalOffset(off int, ins []int) int {
	return off - sort.SearchInts(ins, off)
}

func readAllJSON(s string) (vals []json.RawMessage, complete bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		vals = append(vals, raw)
	}
	rest := strings.TrimSpace(s[int(dec.InputOffset()):])
	return vals, rest == ""
}

// jsonValuesReport decodes consecutive JSON values of body. When strict decoding
// fails or stops short before EOF, tolerant string/quote/brace repairs are tried.
// partialTrailing is true when a valid prefix decoded but trailing non-empty text
// remained unparseable.
func jsonValuesReport(body string) (vals []json.RawMessage, repaired bool, partialTrailing bool) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return nil, false, false
	}
	if v, ok := readAllJSON(body); len(v) > 0 && ok {
		return v, false, false
	}
	bestVals, bestRepaired := []json.RawMessage(nil), false
	if v, ok := readAllJSON(body); len(v) > 0 {
		bestVals = v
		_ = ok
	}

	fixed, ins := repairJSONStrings(body)
	if len(ins) > 0 {
		if v, ok := readAllJSON(fixed); len(v) > 0 && ok {
			return v, true, false
		} else if len(v) > len(bestVals) {
			bestVals, bestRepaired = v, true
		}
	}
	// A single value whose closing brackets are missing.
	if closed, ok := closeJSON(fixed); ok && json.Valid([]byte(closed)) {
		return []json.RawMessage{json.RawMessage(closed)}, true, false
	}
	// Targeted repair for unescaped inner double quotes inside string values
	// (combined with string escape/newline repair across corrected boundaries).
	if qFixed, _, qChanged := repairJSONStringsAndQuotes(body); qChanged {
		if v, ok := readAllJSON(qFixed); len(v) > 0 && ok {
			return v, true, false
		} else if len(v) > len(bestVals) {
			bestVals, bestRepaired = v, true
		}
		if closed, ok := closeJSON(qFixed); ok && json.Valid([]byte(closed)) {
			return []json.RawMessage{json.RawMessage(closed)}, true, false
		}
	}
	if len(bestVals) > 0 {
		return bestVals, bestRepaired, true
	}
	return nil, false, false
}

// jsonValues decodes the consecutive JSON values of body. When the strict decoder
// finds none or stops short, the repaired body is tried; repaired reports that this was needed.
func jsonValues(body string) (vals []json.RawMessage, repaired bool) {
	vals, repaired, _ = jsonValuesReport(body)
	return vals, repaired
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
