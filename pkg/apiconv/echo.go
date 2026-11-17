package apiconv

import (
	"regexp"
	"strings"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

// History-echo filter.
//
// The prompt flattens the conversation into text using the labels
// "[System instruction]:", "[User]:", "[Assistant]:" and "[Tool result for X]:".
// Gemini web tends to imitate that format and copies it into its own output (even
// fabricating tool results). EchoFilter removes those artefacts from the text
// returned to clients, in streaming (Push/Flush) and non-streaming (StripEcho)
// modes with identical behaviour.
//
// Rules (line oriented; lines inside a ``` code fence are never touched unless a
// dropped block is already in progress):
//   - a line starting with "[Tool result for ...]:" starts a dropped block: that
//     line and the following lines up to the next blank line (or next label) are
//     removed; the event is counted as a fabricated tool result;
//   - same for "[User]:" and "[System instruction]:" (no fabricated count);
//   - a line starting with "[Assistant]:" only loses its label (the rest may be
//     the genuine answer);
//   - a "[Tool result for call_<hex>]:" marker in the middle of a line (the
//     proxy-generated call id shape) cuts the rest of the line and starts a block;
//   - any other bracket text (mid-sentence labels, normal text) is preserved.
//
// Real tool calls must be extracted (ParseToolCalls / ParseGoogleFunctionCalls)
// BEFORE filtering the remaining text, so they are never affected.

// EchoStats reports what a filter removed. It never carries content.
type EchoStats struct {
	Stripped   int // label lines / blocks removed
	Fabricated int // of which fabricated tool results
}

// Add accumulates o into s.
func (s *EchoStats) Add(o EchoStats) { s.Stripped += o.Stripped; s.Fabricated += o.Fabricated }

var echoLabelRe = regexp.MustCompile(`^[ \t]*\[(Assistant|User|System instruction|Tool result for [^\]\n]{0,100})\]:[ \t]?`)

const midToolMarker = "[Tool result for call_"

var midToolRe = regexp.MustCompile(`\[Tool result for call_[0-9a-fA-F]{6,}\]:`)

// EchoFilter is an incremental filter. The zero value is ready to use.
type EchoFilter struct {
	Stats EchoStats

	buf      string // unprocessed text of the current line
	started  bool   // the current line start was already classified (mid-line)
	dropping bool   // inside a dropped block (until a blank line)
	dropRest bool   // discarding the remainder of the current line
	inFence  bool
}

// Push feeds a text delta and returns the text that may be emitted now.
func (f *EchoFilter) Push(s string) string {
	f.buf += s
	return f.process(false)
}

// Flush returns the held-back remainder at end of stream.
func (f *EchoFilter) Flush() string { return f.process(true) }

// StripEcho filters a complete text.
func StripEcho(text string) (string, EchoStats) {
	var f EchoFilter
	out := f.Push(text) + f.Flush()
	return out, f.Stats
}

func (f *EchoFilter) process(final bool) string {
	var out strings.Builder
	for f.buf != "" {
		nl := strings.IndexByte(f.buf, '\n')

		if f.dropRest {
			if nl < 0 {
				f.buf = ""
				break
			}
			f.buf = f.buf[nl+1:]
			f.dropRest, f.started = false, false
			continue
		}

		if !f.started {
			line := f.buf
			if nl >= 0 {
				line = f.buf[:nl]
			} else if !final && f.mayBeLabel(line) {
				break // hold until the line start can be classified
			}
			if strings.TrimSpace(line) == "" {
				// blank (or whitespace-only final) line: ends a dropped block
				if nl >= 0 {
					f.buf = f.buf[nl+1:]
				} else {
					f.buf = ""
				}
				if f.dropping {
					f.dropping = false
				} else if nl >= 0 {
					out.WriteString(line + "\n")
				} else {
					out.WriteString(line)
				}
				continue
			}
			if !(f.inFence && !f.dropping) {
				if m := echoLabelRe.FindStringSubmatch(line); m != nil {
					f.buf = f.buf[len(m[0]):]
					f.Stats.Stripped++
					if m[1] == "Assistant" {
						f.dropping = false
						if strings.HasPrefix(f.buf, "\n") {
							f.buf = f.buf[1:] // label-only line: drop the line itself
						} else {
							f.started = true
						}
						continue
					}
					if strings.HasPrefix(m[1], "Tool result") {
						f.Stats.Fabricated++
					}
					f.dropping, f.dropRest = true, true
					continue
				}
			}
			if f.dropping {
				f.dropRest = true // non-label line inside a dropped block
				continue
			}
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
				f.inFence = !f.inFence
			}
			f.started = true
			continue
		}

		// Mid-line text, not dropping.
		end := nl
		if end < 0 {
			end = len(f.buf)
		}
		if !f.inFence {
			if loc := midToolRe.FindStringIndex(f.buf[:end]); loc != nil {
				out.WriteString(f.buf[:loc[0]])
				f.Stats.Stripped++
				f.Stats.Fabricated++
				f.buf = f.buf[loc[1]:]
				f.dropping, f.dropRest = true, true
				continue
			}
		}
		if nl >= 0 {
			out.WriteString(f.buf[:nl+1])
			f.buf = f.buf[nl+1:]
			f.started = false
			continue
		}
		// Partial line: emit all but a possible trailing marker prefix.
		if final {
			out.WriteString(f.buf)
			f.buf = ""
			break
		}
		hold := len(f.buf)
		if !f.inFence {
			hold = markerPrefixStart(f.buf)
		}
		out.WriteString(f.buf[:hold])
		f.buf = f.buf[hold:]
		break
	}
	if final {
		f.started, f.dropRest = false, false
	}
	return out.String()
}

// mayBeLabel reports whether a partial line start could still become a label.
func (f *EchoFilter) mayBeLabel(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if t == "" {
		return true
	}
	if t[0] == '`' {
		return len(t) < 3 && strings.Trim(t, "`") == "" // maybe a fence opener
	}
	if t[0] != '[' {
		return false
	}
	i := strings.IndexByte(t, ']')
	switch {
	case i < 0:
		return len(t) < 120
	case i == len(t)-1: // ":" not received yet
		return true
	}
	return t[i+1] == ':' && i+2 == len(t) // optional trailing space not received yet
}

// markerPrefixStart returns the index from which s must be held back because the
// tail could be the beginning of (or an incomplete) mid-line tool-result marker.
func markerPrefixStart(s string) int {
	if i := strings.LastIndex(s, midToolMarker); i >= 0 && !strings.Contains(s[i:], "]:") {
		return i
	}
	for i := len(s) - 1; i >= 0 && i >= len(s)-len(midToolMarker); i-- {
		if s[i] == '[' && strings.HasPrefix(midToolMarker, s[i:]) {
			return i
		}
	}
	return len(s)
}

var refusalRe = regexp.MustCompile(`(?i)i['’]m having a hard time (fulfilling|completing|processing) (your|that|this) request|i['’]m (just )?a language model and (can['’]t|cannot) help with that|can i help you with something else instead`)

// IsUpstreamRefusal reports whether text is Gemini web's generic refusal
// sentence (or an obvious variant). Only short replies qualify, so a long answer
// merely quoting the phrase is not counted.
func IsUpstreamRefusal(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && len(t) <= 300 && refusalRe.MatchString(t)
}

var glitchRe = regexp.MustCompile(`(?i)can['’]t use workspace because|turn on these settings to continue`)
var widgetOnlyRe = regexp.MustCompile(`^(\s*<GenerateWidget\b[^>]*/>\s*)+$`)

// IsUpstreamGlitch reports whether text is a canned non-answer of Gemini web:
// the "can't use Workspace" sentence or a reply made only of widget
// placeholders. Generic canned errors ("I'm having a hard time fulfilling your
// request...") are handled by IsUpstreamCannedError. Only short replies qualify.
func IsUpstreamGlitch(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 400 {
		return false
	}
	return glitchRe.MatchString(t) || widgetOnlyRe.MatchString(t)
}

var (
	toolRefusalEN        = regexp.MustCompile(`(?i)(none of (those|these|the)\b[^.\n]*(tools|commands)\b[^.\n]*(declared|available)|lack local (file|shell|filesystem|command) access|do not have access to (your|the|any) local (filesystem|files|folders|directory)|no (local )?tools are declared or available|tool[^.\n]*call[^.\n]*limit|action limit|maximum[^.\n]*tool|tool[^.\n]*quota)`)
	toolRefusalFR        = regexp.MustCompile(`(?i)(aucun\b[^.\n]*(outil|des outils)\b[^.\n]*(n['’]est|ne sont)\s*(déclaré|disponible)|n['’]existent? pas dans mes outils actifs|n['’]ai pas accès (à ces|aux|à vos|aux dossiers|aux fichiers)\s*(dossiers|fichiers)?\s*locaux|aucun outil local n['’]est disponible|limite[^.\n]*(appels d['’]outils|d['’]actions|quota d['’]outils)|quota d['’]outils)`)
	thoughtToolRefusalRe = regexp.MustCompile(`(?i)(file\s+tools\s+are\s+not\s+available|(?:local\s+|file\s+|command\s+)?tools\s+are\s+(?:not\s+available|unavailable)|no\s+(?:local\s+|file\s+|command\s+)?tools\s+(?:are\s+)?(?:available|declared)|simulated\s+[^.\n]*(?:report|content|counter-review|review|audit|response|output)|drafted\s+a\s+simulated|without\s+(?:any\s+|access\s+to\s+|using\s+|calling\s+)?(?:file\s+|local\s+)?tools?\b)`)
)

// IsUpstreamToolRefusal reports whether text is an upstream non-answer claiming
// that local tools are undeclared, unavailable, or at an invented limit/quota,
// without emitting any tool call block.
func IsUpstreamToolRefusal(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || len(t) > 2000 {
		return false
	}
	if strings.Contains(t, "```tool_call") || strings.Contains(t, "```function_call") {
		return false
	}
	return toolRefusalEN.MatchString(t) || toolRefusalFR.MatchString(t)
}

// IsThoughtToolRefusalOrSimulation reports whether thought explicitly states
// that local tools are unavailable or that the reply is simulated, while text
// contains no tool_call block.
func IsThoughtToolRefusalOrSimulation(thought, text string) bool {
	th := strings.TrimSpace(thought)
	if th == "" {
		return false
	}
	if strings.Contains(text, "```tool_call") || strings.Contains(text, "```function_call") {
		return false
	}
	return thoughtToolRefusalRe.MatchString(th) || toolRefusalEN.MatchString(th) || toolRefusalFR.MatchString(th)
}

var internalToolRe = regexp.MustCompile(`(?i)(expand_skills|expand_tools|retriever:expand_skills|<GenerateWidget\b|trigger(ed|ing)?\s+artifact\s+generation|artifact\s+generation\s+using)`)

// IsInternalToolSignal reports whether text or thought indicates that Gemini
// tried to use its built-in internal tools (expand_skills, internal artifact, widget).
func IsInternalToolSignal(text string) bool {
	return internalToolRe.MatchString(text)
}

var internalToolStatusRe = regexp.MustCompile(`(?i)(trigger(ed|ing)?\s+artifact\s+generation|searching\s+the\s+web|google\s+(search|drive|docs)|drive_mcp|docs_agent|context_service|expand_skills)`)

// IsInternalToolStatus reports whether a body[2]["7"] progress status string
// indicates an internal Google tool is running.
func IsInternalToolStatus(status string) bool {
	return internalToolStatusRe.MatchString(status)
}

// IsUpstreamCannedError reports whether Gemini returned a short canned
// internal-error non-answer (B6/N1, e.g. "I seem to be encountering an error. Can I try something else for you?").
func IsUpstreamCannedError(text string) bool {
	return gemini.IsCannedErrorReply(text, false)
}

// IsUpstreamCannedErrorWithMarker reports whether Gemini returned a short canned
// error, using Google's inner[31]=true error marker as the primary criterion and
// known EN/FR canned phrases as fallback.
func IsUpstreamCannedErrorWithMarker(text string, hasErrorMarker bool) bool {
	return gemini.IsCannedErrorReply(text, hasErrorMarker)
}
