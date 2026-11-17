package apiconv

import "strings"

// Progressive output for the stream+tools path (P9).
//
// With tools active the full reply is needed to extract the tool calls, so the
// reply used to be sent in one piece at the end. ProgressGate lets the text that
// cannot be part of a tool call go out as it arrives: everything up to the first
// fence that may hold a tool call (an intent tag, "json" or no tag), or nothing at
// all when the reply starts like bare JSON. Ordinary code blocks (```go, ```bash…)
// are forwarded. Whatever is held back is delivered at the end, after parsing.

// ProgressGate is fed the reply deltas in order. The zero value is ready to use.
type ProgressGate struct {
	acc       string
	sent      int  // bytes of acc already released
	scan      int  // where the fence scan resumes
	inCode    bool // inside an ordinary code block
	inCodeLen int  // backtick count of the open code block
	closed    bool // a possible tool call was met: nothing more is released
}

// mayHoldToolCall reports whether a fence with this tag can carry a tool call.
func mayHoldToolCall(tag string) bool {
	return tag == "" || tag == "json" || toolFenceTags[tag]
}

// Push adds a delta and returns the text that can be released now.
func (g *ProgressGate) Push(delta string) string {
	g.acc += delta
	if g.closed {
		return ""
	}
	if g.sent == 0 {
		lead := strings.TrimLeft(g.acc, " \t\r\n")
		if lead == "" {
			return ""
		}
		if lead[0] == '{' || lead[0] == '[' {
			g.closed = true // may be a bare JSON tool call
			return ""
		}
	}
	safe := len(g.acc)
	for {
		i := strings.Index(g.acc[g.scan:], "```")
		if i < 0 {
			// Keep a trailing run of backticks: it may be the start of a fence.
			for safe > g.scan && safe > 0 && g.acc[safe-1] == '`' && len(g.acc)-safe < 2 {
				safe--
			}
			g.scan = safe
			break
		}
		i += g.scan
		fenceLen := 3
		for i+fenceLen < len(g.acc) && g.acc[i+fenceLen] == '`' {
			fenceLen++
		}
		if g.inCode {
			if i+fenceLen == len(g.acc) && fenceLen < g.inCodeLen {
				safe, g.scan = i, i
				break
			}
			if fenceLen < g.inCodeLen {
				g.scan = i + fenceLen
				continue
			}
			g.inCode = false
			g.inCodeLen = 0
			g.scan = i + fenceLen
			continue
		}
		nl := strings.IndexByte(g.acc[i+fenceLen:], '\n')
		if nl < 0 {
			safe, g.scan = i, i // tag line incomplete: hold from the fence
			break
		}
		rawTag := strings.TrimSuffix(g.acc[i+fenceLen:i+fenceLen+nl], "\r")
		if end, ok := inlineFenceEnd(rawTag); ok {
			g.scan = i + fenceLen + end
			continue
		}
		tag := strings.ToLower(strings.TrimSpace(rawTag))
		if mayHoldToolCall(tag) || IsCodeReferenceTag(tag) {
			g.closed = true
			safe = i
			break
		}
		g.inCode = true
		g.inCodeLen = fenceLen
		g.scan = i + fenceLen + nl + 1
	}
	if safe <= g.sent {
		return ""
	}
	out := g.acc[g.sent:safe]
	g.sent = safe
	return out
}

// Held returns how many bytes are currently held back (a possible tool call being
// written, or a fence whose tag is not known yet).
func (g *ProgressGate) Held() int { return len(g.acc) - g.sent }

// Sent returns the text released so far.
func (g *ProgressGate) Sent() string { return g.acc[:g.sent] }

// Rest returns what remains to be sent once the final text (tool calls removed,
// cleaned) is known. ok is false when the final text does not continue the text
// already released (it was altered by post-processing): nothing more is sent then.
func (g *ProgressGate) Rest(final string) (rest string, ok bool) {
	sent := g.Sent()
	switch {
	case sent == "":
		return final, true
	case strings.HasPrefix(final, sent):
		return final[len(sent):], true
	case strings.HasPrefix(sent, final):
		return "", true // only trailing whitespace was released in addition
	}
	if st := strings.TrimRight(sent, " \t\r\n"); strings.HasPrefix(final, st) {
		return strings.TrimLeft(final[len(st):], " \t\r\n"), true
	}
	return "", false
}

// EffortLevel maps an OpenAI-style reasoning effort onto the Gemini web thinking
// level code (catalog values: low=1, medium=4, high=2). 0 = unknown / not set.
func EffortLevel(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "minimal", "bas":
		return 1
	case "medium", "moyen":
		return 4
	case "high", "eleve", "élevé":
		return 2
	}
	return 0
}

// SplitModelEffort splits "model@effort" into its parts ("model", "effort"). A
// name without a recognised effort suffix is returned unchanged.
func SplitModelEffort(model string) (name, effort string) {
	if i := strings.LastIndexByte(model, '@'); i > 0 && EffortLevel(model[i+1:]) > 0 {
		return model[:i], model[i+1:]
	}
	return model, ""
}
