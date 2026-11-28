package apiconv

import (
	"regexp"
	"strings"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
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

// IsCompaction reports whether text is wrapped in the model's compaction
// marker (<summary>…</summary>). Compactions are legitimate text-only answers
// and must never be treated as a skipped tool call.
func IsCompaction(text string) bool {
	t := text
	return strings.Contains(t, "<summary>") || strings.Contains(t, "</summary>")
}

// IsUpstreamRefusal reports whether text is Gemini web's generic refusal
// sentence (or an obvious variant). Only short replies qualify, so a long answer
// merely quoting the phrase is not counted.
func IsUpstreamRefusal(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && len(t) <= 300 && refusalRe.MatchString(t)
}

var glitchRe = regexp.MustCompile(`(?i)can['’]t use workspace because|turn on these settings to continue|permission\s+to\s+turn\s+on\s+the\s+[^.\n]{1,40}\s+app\b|autorisation\s+d['’]activer\s+l['’]application\b`)
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
	toolRefusalEN        = regexp.MustCompile(`(?i)(none of (those|these|the)\b[^.\n]*(tools|commands)\b[^.\n]*(declared|available)|(?:the\s+)?(?:local\s+)?(?:file(?:\s+access)?\s+)?tools?\b[^.\n]*(?:are|is)\s+not\s+(?:available|declared|accessible|exposed)|only\s+external\s+connectors\b[^.\n]*are\s+declared|lack local (file|shell|filesystem|command|tool) access|do not have (?:direct )?access to (?:your|the|any) (?:local )?(?:environment|terminal|git repositor(?:y|ies)|filesystem|files|folders|directory|tools)|(?:is|are) not exposed in my available tools|no (local )?tools are declared or available|(?:tool\s+execution\s+is\s+disabled|tools\s+are\s+disabled\s+for\s+this\s+turn)|(?:reached|hit|exceeded|exhausted|depleted|capped|reset(?:s|ting)?)\b[^.\n]*(?:tool[^.\n]*call[^.\n]*limit|action\s+limit|maximum[^.\n]*tool|tool[^.\n]*quota)|(?:tool[^.\n]*call[^.\n]*limit|action\s+limit|maximum[^.\n]*tool[^.\n]*calls?|tool[^.\n]*quota)[^.\n]*(?:reached|hit|exceeded|exhausted|depleted|met|blocking)|(?:zero|no|\x60?0\x60?)\s+remaining\s+tool\s+calls?|(?:\x60?0\x60?|zero|no)\s+tool\s+calls?\s+remaining|\x600\x60\s+tool\s+calls?|\b0\s+tool\s+calls?\s+(?:left|remaining)|out\s+of\s+tool\s+calls?|\x600\s+remaining\x60|\b0\s+remaining(?:\s+(?:appels?(?:\s+d['’]outils?)?|tools?|calls?|actions?|restants?)|\s*\))|(?:reply|send|type)\s+(?:(?:simply|just)\s+)?[\x60"*\s\x{00a0}\x{202f}]{0,8}continue[\x60"*\s\x{00a0}\x{202f}]{0,8}\s+to\s+\p{L}+)`)
	toolRefusalFR        = regexp.MustCompile(`(?i)(aucun\b[^.\n]*(outil|des outils)\b[^.\n]*(n['’]est|ne sont)\s*(déclaré|disponible)|(?:les\s+)?outils(?:\s+d['’]accès\s+aux\s+fichiers\s+locaux)?\b[^.\n]*(?:ne\s+sont\s+pas|n['’]est\s+pas)\s*(?:disponibles?|déclarés?|accessibles?|exposés?)|seuls\s+des\s+connecteurs\s+externes\b[^.\n]*sont\s+déclarés|n['’]existent? pas dans mes outils actifs|n['’]ai pas (?:d['’])?accès(?:\s+direct)?\s+(?:à\s+(?:votre|l['’]|mon|ces|aux|vos)?|aux)\s*(?:environnement\s+local|terminal|dépôts?(?:\s+git)?|système\s+de\s+fichiers|dossiers|fichiers|outils)(?:\s*locaux)?|(?:ne\s+sont\s+pas\s+exposés?|n['’]est\s+pas\s+exposé)\s+dans\s+mes\s+outils\s+disponibles|aucun outil local n['’]est disponible|(?:l['’])?exécution\s+d['’]outils\s+est\s+désactivée|(?:atteint|dépassé|épuisé|saturé|excédé|bloqué|réinitialiser(?:a)?)\b[^.\n]*(?:limite|quota|nombre\s+maximal|plafond|budget)[^.\n]*(?:appels?|actions?|outils?)|(?:limite|quota|nombre\s+maximal|plafond|budget)[^.\n]*(?:appels?(?:\s+d['’]outils?)?|d['’]actions?|d['’]outils?)[^.\n]*(?:épuisé|atteint|dépassé|saturé|excédé|bloqué|terminé|fini|réinitialis|à\s+zéro|nul|:\s*0\b)|(?:\x60?0\x60?|zéro|aucun)\s+appels?(?:\s+d['’]outils?)?\s+restants?|plus\s+d['’]appels?\s+d['’]outils?|(?:à\s+)?zéro\s*\(\s*0\s+restants?\s*\)|(?:\b0|\x600\x60)\s+restants?\b|(?:envoie|envoyez|tapez?|réponds?|répondez|écris?|écrivez|dites?|dis)\s+(?:simplement\s+)?[\x60«»"*\s\x{00a0}\x{202f}]{0,8}continue(?:r)?[\x60«»"*\s\x{00a0}\x{202f}]{0,8}\s+pour\s+\p{L}+|(?:dès\s+que\s+vous\s+(?:me\s+)?donnez\s+le\s+feu\s+vert|donnez[- ]moi\s+le\s+feu\s+vert\s+pour\s+(?:démarrer|lancer|commencer)|prêt\s+à\s+(?:démarrer|lancer)\s+l['’]implémentation\s+dès\s+que)|relance(?:r|z)?\s+une\s+nouvelle\s+session)`)
	postStallHandoffRe   = regexp.MustCompile(`(?i)(état(?:\s+(?:exact|actuel))?\s+d['’]avancement|étapes\s+restantes|fichiers\s+(?:et\s+étapes\s+)?restant(?:s|es)?|dernières\s+actions\s+à\s+exécuter|prochaines\s+étapes|remaining\s+steps|files\s+remaining)`)
	thoughtToolRefusalRe = regexp.MustCompile(`(?i)(file\s+tools\s+are\s+not\s+available|(?:local\s+|file\s+|command\s+)?tools\s+are\s+(?:not\s+available|unavailable)|no\s+(?:local\s+|file\s+|command\s+)?tools\s+(?:are\s+)?(?:available|declared)|simulated\s+[^.\n]*(?:report|content|counter-review|review|audit|response|output)|drafted\s+a\s+simulated|without\s+(?:any\s+|access\s+to\s+|using\s+|calling\s+)?(?:file\s+|local\s+)?tools?\b)`)
	proxyInterruptedRe   = regexp.MustCompile(`(?i)^GMINI\s*:\s*(Gemini\s+(s['’]est\s+figé|a\s+renvoyé\s+une\s+erreur\s+interne|a\s+refusé\s+ou\s+simulé|a\s+affirmé\s+deux\s+fois|a\s+répondu\s+deux\s+fois|a\s+tenté\s+deux\s+fois|n['’]a\s+envoyé\s+aucune\s+réponse)|la\s+réponse\s+de\s+Gemini\s+s['’]emballe)`)
)

// IsUpstreamToolRefusal reports whether text is an upstream non-answer claiming
// that local tools are undeclared, unavailable, disabled, or at an invented
// limit/quota, without emitting any tool call block. The whole reply is scanned
// (not only its head and tail) so a refusal written mid-message in a long recap
// is still caught.
func IsUpstreamToolRefusal(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.Contains(t, "```tool_call") || strings.Contains(t, "```function_call") {
		return false
	}
	if strings.Contains(t, "<summary>") || strings.Contains(t, "</summary>") {
		return false
	}
	return toolRefusalEN.MatchString(t) || toolRefusalFR.MatchString(t)
}

// IsPostStallHandoffSummary reports whether a 0-tool-call reply is framed as a
// progress/remaining-steps handoff checklist rather than executing the next tool.
// Compactions wrapped in <summary>...</summary> are never flagged.
func IsPostStallHandoffSummary(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if strings.Contains(t, "```tool_call") || strings.Contains(t, "```function_call") {
		return false
	}
	if strings.Contains(t, "<summary>") || strings.Contains(t, "</summary>") {
		return false
	}
	return postStallHandoffRe.MatchString(t)
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

// SanitizeAssistantHistory neutralizes prior assistant turns that were transient
// upstream canned errors, GMINI interruption notices, or false tool-quota/refusal
// replies so they never poison subsequent turns in the same conversation.
// For tool refusals followed by a factual status recap, only the refusal line(s)
// are replaced so the model keeps awareness of already-completed work.
func SanitizeAssistantHistory(content string) string {
	t := strings.TrimSpace(content)
	if t == "" {
		return content
	}
	if IsUpstreamCannedError(t) || IsUpstreamGlitch(t) || proxyInterruptedRe.MatchString(t) {
		return "[Previous turn failed with a transient error — ignored. Continue the task normally using the declared tools.]"
	}
	if IsUpstreamToolRefusal(t) {
		return stripPoisonedHistoryLines(content, "[Previous turn was interrupted before calling tools; all declared tools are active with NO quota or call limit.]", false)
	}
	if IsPostStallHandoffSummary(t) {
		return stripPoisonedHistoryLines(content, "[Previous turn stopped with a partial status checklist instead of calling tools; all declared tools are active — execute the remaining tool calls directly.]", true)
	}
	return content
}

func stripPoisonedHistoryLines(content, note string, dropHandoffLines bool) string {
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if toolRefusalFR.MatchString(line) || toolRefusalEN.MatchString(line) {
			continue
		}
		if dropHandoffLines && postStallHandoffRe.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	rest := strings.TrimSpace(strings.Join(kept, "\n"))
	if rest == "" {
		return note
	}
	return note + "\n" + rest
}
