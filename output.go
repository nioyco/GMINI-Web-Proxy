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
func (s *Server) finishText(ctx context.Context, raw string, toolsActive bool) (string, []apiconv.ToolCall) {
	s.noteRefusal(ctx, raw)
	text, calls := raw, []apiconv.ToolCall(nil)
	if toolsActive && text != "" {
		text, calls = apiconv.ParseToolCalls(text)
	}
	text, st := apiconv.StripEcho(text)
	s.noteEcho(ctx, st)
	return text, calls
}

// googleResponse builds a generateContent reply with echo filtering and markers.
func (s *Server) googleResponse(ctx context.Context, model, prompt, text string, hasTools bool) any {
	s.noteRefusal(ctx, text)
	resp, st := apiconv.GoogleResponseEx(model, prompt, text, hasTools)
	s.noteEcho(ctx, st)
	return resp
}

// echoEmitter wraps a delta emitter of a tool-free stream with the echo filter.
// finish() must be called once generation ended: it flushes held-back text and
// records the markers. acc holds the text emitted so far (for the refusal check).
func (s *Server) echoEmitter(ctx context.Context, emitRaw func(string), acc *strings.Builder) (emit func(string), finish func()) {
	var f apiconv.EchoFilter
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
