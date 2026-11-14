package apiconv

import (
	"encoding/json"
	"regexp"
	"strings"
)

// fenceOpenRe matches what follows an opening ``` : an optional language tag and
// the end of the line. Group 1 is the tag.
var fenceOpenRe = regexp.MustCompile("^[ \\t]*([\\w-]*)[ \\t]*\\r?\\n")

// fenceBlock is a fenced block located in the model output. All offsets are byte
// offsets into the scanned text: [start,end) is the whole block (opening fence to
// the end of the closing fence, or to the end of the text when it is unterminated),
// [bodyStart,bodyEnd) is the body between the fences.
type fenceBlock struct {
	start, end         int
	tag                string
	bodyStart, bodyEnd int
}

// findFences locates the fenced blocks of text. The model is asked to wrap tool
// calls in ```tool_call ... ```, but in practice it varies the fence language, omits
// the closing fence, or puts it on the same line as the body; whether a block is a
// tool call is decided later by parsing its body, so ordinary code blocks are only
// located, never altered.
//
// A tool-call argument can itself contain ``` (a Markdown or code file being
// written), so the closing fence is NOT the first ``` after the opening: when the
// body starts with a JSON object or array, the JSON value(s) are decoded first
// (encoding/json knows about strings) and the closing fence is searched only after
// the end of the last value. A body that is not JSON keeps the plain rule (the
// first ``` closes the block), as does a body whose JSON is invalid or truncated.
func findFences(text string) []fenceBlock {
	var out []fenceBlock
	pos := 0
	for pos < len(text) {
		i := strings.Index(text[pos:], "```")
		if i < 0 {
			break
		}
		i += pos
		after := i + 3
		m := fenceOpenRe.FindStringSubmatchIndex(text[after:])
		if m == nil {
			pos = i + 1
			continue
		}
		b := fenceBlock{
			start:     i,
			tag:       text[after+m[2] : after+m[3]],
			bodyStart: after + m[1],
		}
		b.bodyEnd, b.end = fenceClose(text, b.bodyStart)
		out = append(out, b)
		pos = b.end
	}
	return out
}

// fenceClose returns the end of the body and the end of the block for a block whose
// body starts at bodyStart (see findFences).
func fenceClose(text string, bodyStart int) (bodyEnd, end int) {
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
		if strings.HasSuffix(rest, "```") {
			rest = rest[:len(rest)-3]
			bodyEnd = bodyStart + len(rest)
		}
		fixed, _ := repairJSONStrings(rest)
		if closed, ok := closeJSON(fixed); ok && json.Valid([]byte(closed)) {
			return bodyEnd, len(text)
		}
	}
	if c := strings.Index(text[searchFrom:], "```"); c >= 0 {
		return searchFrom + c, searchFrom + c + 3
	}
	return len(text), len(text)
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
