package main

import (
	"context"
	"strings"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
)

// noteEcho records history-echo removals for the request and the process
// counters, and logs one metadata-only line ("echo_stripped", no content).
func (s *Server) noteEcho(ctx context.Context, st apiconv.EchoStats) {
	if st.Stripped == 0 {
		return
	}
	s.stats.echoStripped.Add(int64(st.Stripped))
	s.stats.echoFabricated.Add(int64(st.Fabricated))
	if ri := reqFrom(ctx); ri != nil {
		ri.echoStripped.Add(int64(st.Stripped))
		ri.echoFabricated.Add(int64(st.Fabricated))
	}
	s.rlog(ctx).Warn("echo_stripped", "blocks", st.Stripped, "fabricated_tool_results", st.Fabricated)
}

// noteRefusal logs "upstream_refusal" (no content) when the reply is Gemini's
// generic refusal sentence. The reply itself is passed through unchanged.
func (s *Server) noteRefusal(ctx context.Context, text string) {
	if !apiconv.IsUpstreamRefusal(text) {
		return
	}
	s.stats.upstreamRefusal.Add(1)
	if ri := reqFrom(ctx); ri != nil {
		ri.refusal.Store(true)
	}
	s.rlog(ctx).Warn("upstream_refusal", "len", len(text))
}

// finishText post-processes a complete model reply: tool calls are extracted first
// (so a real call is never affected), then the remaining text is cleaned of
// history echoes. The refusal marker is evaluated on the raw reply.
func (s *Server) finishText(ctx context.Context, raw string, toolsActive bool, declared []apiconv.DeclaredTool) (string, []apiconv.ToolCall) {
	s.noteRefusal(ctx, raw)
	text, calls := raw, []apiconv.ToolCall(nil)
	if toolsActive && text != "" {
		var rep apiconv.ParseReport
		text, calls, rep = apiconv.ParseToolCallsWith(text, declared)
		s.noteParse(ctx, rep)
		if len(declared) > 0 && text != "" {
			recText, recCalls, recCount := apiconv.RecoverUnconvertedToolCalls(text, declared, len(calls) > 0)
			if recCount > 0 {
				text = recText
				calls = append(calls, recCalls...)
				s.stats.toolCallUnconverted.Add(int64(recCount))
				s.rlog(ctx).Warn("tool_call_unconverted", "count", recCount)
			}
		}
	}
	if !s.cfg.DisableEchoFilter {
		var st apiconv.EchoStats
		text, st = apiconv.StripEcho(text)
		s.noteEcho(ctx, st)
	}
	return text, calls
}

// googleResponse builds a generateContent reply with echo filtering and markers.
func (s *Server) googleResponse(ctx context.Context, model, prompt, text string, hasTools bool, declared []apiconv.DeclaredTool) any {
	s.noteRefusal(ctx, text)
	resp, st, rep := apiconv.GoogleResponseWith(model, prompt, text, hasTools, !s.cfg.DisableEchoFilter, declared)
	s.noteEcho(ctx, st)
	s.noteParse(ctx, rep)
	return resp
}

// echoEmitter wraps a delta emitter of a tool-free stream with the echo filter.
// finish() must be called once generation ended: it flushes held-back text and
// records the markers. acc holds the text emitted so far (for the refusal check).
func (s *Server) echoEmitter(ctx context.Context, emitRaw func(string), acc *strings.Builder) (emit func(string), finish func()) {
	var f apiconv.EchoFilter
	if s.cfg.DisableEchoFilter {
		emit = emitRaw
		finish = func() { s.noteRefusal(ctx, acc.String()) }
		return emit, finish
	}
	emit = func(d string) {
		if out := f.Push(d); out != "" {
			emitRaw(out)
		}
	}
	finish = func() {
		if out := f.Flush(); out != "" {
			emitRaw(out)
		}
		s.noteEcho(ctx, f.Stats)
		s.noteRefusal(ctx, acc.String())
	}
	return emit, finish
}

// noteParse logs and counts tool-call parsing events (no values, key names only):
// "tool_call_unparsed" for a tool-intent fence that stayed text, "tool_name_inferred"
// for a call whose name was recovered from the declared tools.
func (s *Server) noteParse(ctx context.Context, rep apiconv.ParseReport) {
	for _, u := range rep.Unparsed {
		s.stats.toolCallUnparsed.Add(1)
		s.rlog(ctx).Warn("tool_call_unparsed", "tag", u.Tag, "keys", safeKeyList(u.Keys),
			"declared_tools", u.Declared, "reason", u.Reason)
	}
	if rep.Repaired > 0 {
		s.rlog(ctx).Warn("tool_call_json_repaired", "blocks", rep.Repaired)
	}
	if rep.DroppedAfter > 0 {
		s.rlog(ctx).Warn("text_after_tool_call_dropped", "bytes", rep.DroppedAfter)
	}
	for _, name := range rep.Inferred {
		s.stats.toolNameInferred.Add(1)
		s.rlog(ctx).Info("tool_name_inferred", "tool", safeKeyList([]string{name}))
	}
	if rep.CodeRefRemoved > 0 {
		s.markInternalToolDetected(ctx, "exécution de code")
	}
}

func (s *Server) markInternalToolDetected(ctx context.Context, name string) {
	if ri := reqFrom(ctx); ri != nil {
		ri.setInternalTool(name)
		if !ri.internalTool.Swap(true) {
			s.stats.internalToolDetected.Add(1)
		}
		return
	}
	s.stats.internalToolDetected.Add(1)
}

// safeKeyList joins key names for logging, replacing anything that is not a plain
// identifier and capping the list, so model-controlled text cannot reach the log.
func safeKeyList(keys []string) string {
	out := make([]string, 0, len(keys))
	for i, k := range keys {
		if i == 40 {
			out = append(out, "...")
			break
		}
		if !safeNameRe.MatchString(k) {
			k = "(unsafe)"
		}
		out = append(out, k)
	}
	return strings.Join(out, ",")
}
