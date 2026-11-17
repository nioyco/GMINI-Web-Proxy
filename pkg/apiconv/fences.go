package apiconv

import (
	"encoding/json"
	"strings"
)

// fenceBlock is a fenced block located in the model output. All offsets are byte
// offsets into the scanned text: [start,end) is the whole block (opening fence to
// the end of the closing fence, or to the end of the text when it is unterminated),
// [bodyStart,bodyEnd) is the body between the fences.
type fenceBlock struct {
	start, end         int
	tag                string
	bodyStart, bodyEnd int
}

// findFences locates the fenced blocks of text. An opening fence is three or more
// backticks followed by any info-string text up to the end of the line (except an
// inline span containing another ``` on the same line). The model is asked to wrap
// tool calls in ```tool_call ... ```, but in practice it varies the fence language,
// omits the closing fence, or puts it on the same line as the body; whether a block
// is a tool call is decided later by parsing its body, so ordinary code blocks are
// only located, never altered.
//
// A tool-call argument can itself contain ``` (a Markdown or code file being
// written), so the closing fence is NOT the first ``` after the opening: when the
// body starts with a JSON object or array, the JSON value(s) are decoded first
// (encoding/json knows about strings) and the closing fence is searched only after
// the end of the last value. A body that is not JSON keeps the plain rule (the
// first matching closing fence closes the block), as does a body whose JSON is
// invalid or truncated.
func findFences(text string) []fenceBlock {
	var out []fenceBlock
	pos := 0
	for pos < len(text) {
		i := strings.Index(text[pos:], "```")
		if i < 0 {
			break
		}
		i += pos
		openLen := 3
		for i+openLen < len(text) && text[i+openLen] == '`' {
			openLen++
		}
		after := i + openLen
		nl := strings.IndexByte(text[after:], '\n')
		if nl < 0 {
			pos = after
			continue
		}
		rawTag := strings.TrimSuffix(text[after:after+nl], "\r")
		if end, ok := inlineFenceEnd(rawTag); ok {
			pos = after + end
			continue
		}
		b := fenceBlock{
			start:     i,
			tag:       strings.TrimSpace(rawTag),
			bodyStart: after + nl + 1,
		}
		b.bodyEnd, b.end = fenceClose(text, b.bodyStart, openLen)
		out = append(out, b)
		pos = b.end
	}
	return out
}

// fenceClose returns the end of the body and the end of the block for a block whose
// body starts at bodyStart with an opening fence of openLen backticks.
func fenceClose(text string, bodyStart, openLen int) (bodyEnd, end int) {
	searchFrom := bodyStart
	// The JSON may need the tolerant reading (see repairJSONStrings): decode the
	// repaired text and map the end of the last value back to the original.
	src, ins := text[bodyStart:], []int(nil)
	if lead := strings.TrimLeft(src, " \t\r\n"); lead != "" && (lead[0] == '{' || lead[0] == '[') {
		src, ins = repairJSONStrings(src)
	}
	dec := json.NewDecoder(strings.NewReader(src))
	for {
		q := int(dec.InputOffset())
		for q < len(src) && isJSONSpace(src[q]) {
			q++
		}
		if q >= len(src) || (src[q] != '{' && src[q] != '[') {
			break
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			break
		}
		searchFrom = bodyStart + originalOffset(int(dec.InputOffset()), ins)
	}
	if lead := strings.TrimLeft(src, " \t\r\n"); searchFrom == bodyStart && lead != "" && lead[0] == '{' {
		// No complete JSON value: the reply may be ONE tool call whose closing
		// brackets are missing (see closeJSON), possibly followed by a last fence.
		// The block then runs to the end, whatever ``` its strings contain.
		rest := strings.TrimRight(text[bodyStart:], " \t\r\n")
		bodyEnd = bodyStart + len(rest)
		if trimmed, ok := trimTrailingFence(rest, openLen); ok {
			rest = trimmed
			bodyEnd = bodyStart + len(rest)
		}
		fixed, _ := repairJSONStrings(rest)
		if closed, ok := closeJSON(fixed); ok && json.Valid([]byte(closed)) {
			return bodyEnd, len(text)
		}
	}
	remLead := strings.TrimLeft(text[searchFrom:], " \t\r\n")
	if c, closeLen := findClosingFence(text, searchFrom, remLead != "" && (remLead[0] == '{' || remLead[0] == '['), openLen); c >= 0 {
		return c, c + closeLen
	}
	return len(text), len(text)
}

func trimTrailingFence(s string, minLen int) (string, bool) {
	n := 0
	for n < len(s) && s[len(s)-1-n] == '`' {
		n++
	}
	if n >= minLen {
		return s[:len(s)-n], true
	}
	return s, false
}

// findClosingFence locates the next run of at least minLen backticks at or after
// searchFrom and returns its byte offset and length. When jsonBody is true and
// searchFrom starts inside an incomplete JSON value, backticks inside a JSON
// string literal are skipped so an embedded Markdown code block in a write_file
// payload never prematurely closes the fence.
func findClosingFence(text string, searchFrom int, jsonBody bool, minLen int) (int, int) {
	if minLen < 3 {
		minLen = 3
	}
	countRun := func(i int) int {
		n := 0
		for i+n < len(text) && text[i+n] == '`' {
			n++
		}
		return n
	}
	if !jsonBody {
		for i := searchFrom; i < len(text); i++ {
			if text[i] == '`' {
				n := countRun(i)
				if n >= minLen {
					return i, n
				}
				i += n - 1
			}
		}
		return -1, 0
	}
	inStr := false
	for i := searchFrom; i < len(text); i++ {
		c := text[i]
		if inStr {
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '`' {
			n := countRun(i)
			if n >= minLen {
				return i, n
			}
			i += n - 1
		}
	}
	return -1, 0
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// IsCodeReferenceTag reports whether a fence info string marks Gemini's
// internal code-execution reference/output block.
func IsCodeReferenceTag(tag string) bool {
	low := strings.ToLower(tag)
	return strings.Contains(low, "code_reference") || strings.Contains(low, "code_event_index")
}

func inlineFenceEnd(rawTag string) (int, bool) {
	idx := strings.Index(rawTag, "```")
	if idx < 0 {
		return 0, false
	}
	end := idx + 3
	for end < len(rawTag) && rawTag[end] == '`' {
		end++
	}
	return end, true
}
