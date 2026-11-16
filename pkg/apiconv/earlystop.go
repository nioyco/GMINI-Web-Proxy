package apiconv

import (
	"encoding/json"
	"strings"
)

// runawayMarkers are conversation-flattening labels that Gemini sometimes writes
// after emitting a valid tool call, simulating the rest of the turn on its own.
var runawayMarkers = []string{
	"[Tool result for",
	"[User]:",
	"[Assistant]:",
	"[System instruction]:",
}

// ToolScanResult summarizes the complete valid tool calls seen so far in an
// in-progress streamed reply and whether a forbidden runaway marker appeared
// after at least one complete tool call.
type ToolScanResult struct {
	CompleteCalls int
	LastCallEnd   int
	CutAt         int
	HasMarker     bool
}

// findRunawayMarker returns the byte offset of the earliest runaway marker in s,
// or -1 when none is present.
func findRunawayMarker(s string) int {
	best := -1
	for _, m := range runawayMarkers {
		if i := strings.Index(s, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// isClosedFence reports whether fb has an explicit closing ``` fence in text.
func isClosedFence(text string, fb fenceBlock) bool {
	return fb.end > fb.bodyEnd && fb.end <= len(text) && strings.HasPrefix(text[fb.bodyEnd:fb.end], "```")
}

// ScanToolCalls inspects an in-progress streamed model output for complete, valid
// tool call blocks and for runaway markers appearing after at least one complete
// tool call. Markers inside an open tool-call/JSON fence (such as a write_file
// payload) are ignored until that fence closes.
func ScanToolCalls(text string, declared []DeclaredTool) ToolScanResult {
	var res ToolScanResult
	names := declaredNameSet(declared)
	fences := findFences(text)
	scanFrom := 0

	for _, fb := range fences {
		if res.CompleteCalls > 0 && fb.start > scanFrom {
			if m := findRunawayMarker(text[scanFrom:fb.start]); m >= 0 {
				res.CutAt = scanFrom + m
				res.HasMarker = true
				return res
			}
		}

		tag := strings.ToLower(fb.tag)
		if !isClosedFence(text, fb) {
			// An unclosed fence that may carry JSON / a tool call (e.g. write_file
			// writing a markdown file containing "[User]:") must not trigger inside
			// its body while still streaming.
			bodyLead := strings.TrimLeft(text[fb.bodyStart:], " \t\r\n")
			if mayHoldToolCall(tag) && (toolFenceTags[tag] || bodyLead == "" || bodyLead[0] == '{' || bodyLead[0] == '[') {
				return res
			}
			if res.CompleteCalls > 0 {
				if m := findRunawayMarker(text[fb.bodyStart:]); m >= 0 {
					res.CutAt = fb.bodyStart + m
					res.HasMarker = true
				}
			}
			return res
		}

		body := strings.TrimSpace(text[fb.bodyStart:fb.bodyEnd])
		var rep ParseReport
		var blockCalls []ToolCall
		if toolFenceTags[tag] {
			blockCalls = decodeIntentBlock(body, tag, declared, &rep)
		} else {
			blockCalls = decodeToolCalls(body, false)
		}
		if len(blockCalls) > 0 && len(rejectUndeclared(blockCalls, names)) == 0 {
			res.CompleteCalls += len(blockCalls)
			res.LastCallEnd = fb.end
			scanFrom = fb.end
			continue
		}

		if res.CompleteCalls > 0 {
			if m := findRunawayMarker(text[fb.start:fb.end]); m >= 0 {
				res.CutAt = fb.start + m
				res.HasMarker = true
				return res
			}
		}
		scanFrom = fb.end
	}

	if res.CompleteCalls > 0 && scanFrom < len(text) {
		if m := findRunawayMarker(text[scanFrom:]); m >= 0 {
			res.CutAt = scanFrom + m
			res.HasMarker = true
			return res
		}
	}

	// Bare JSON fallback (when the response starts with { or [ without fences).
	if res.CompleteCalls == 0 && len(fences) == 0 {
		lead := strings.TrimLeft(text, " \t\r\n")
		if lead != "" && (lead[0] == '{' || lead[0] == '[') {
			offset := len(text) - len(lead)
			dec := json.NewDecoder(strings.NewReader(lead))
			for {
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					break
				}
				calls := toolCallsFromJSON(raw, false)
				if len(calls) == 0 || len(rejectUndeclared(calls, names)) > 0 {
					break
				}
				res.CompleteCalls += len(calls)
				res.LastCallEnd = offset + int(dec.InputOffset())
			}
			if res.CompleteCalls > 0 && res.LastCallEnd < len(text) {
				remLead := strings.TrimLeft(text[res.LastCallEnd:], " \t\r\n")
				// An incomplete trailing bare JSON object/array is still streaming
				// (e.g. a second bare write_file whose content quotes a marker).
				isStreamingJSON := remLead != "" && (remLead[0] == '{' || (remLead[0] == '[' && findRunawayMarker(remLead) != 0))
				if !isStreamingJSON {
					if m := findRunawayMarker(text[res.LastCallEnd:]); m >= 0 {
						res.CutAt = res.LastCallEnd + m
						res.HasMarker = true
					}
				}
			}
		}
	}

	return res
}
