package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
	"github.com/n0madic/go-gemini-web2api/pkg/util"
)

// maxBodyBytes caps the request body size accepted from clients.
const maxBodyBytes = 20 << 20 // 20 MiB

// googleModelRe extracts the model name from /v1beta/models/{model}:method paths.
var googleModelRe = regexp.MustCompile(`/v1beta/models/([^:?]+)`)

// Server routes HTTP requests to the Gemini client.
type Server struct {
	cfg    *Config
	client *gemini.Client
	log    *slog.Logger
	// generate is the blocking generation call used by the stream+tools keepalive
	// path. It defaults to client.Generate and exists as a seam for tests.
	generate func(ctx context.Context, p gemini.GenParams) (string, error)
	// keepaliveOverride, when > 0, replaces the cfg-derived keepalive interval
	// (sub-second values for tests). Never set in production.
	keepaliveOverride time.Duration

	started time.Time
	active  activeSet
	stats   statsCounters
	ignored ignoredStats
}

// remoteImagesMsg is the 400 message for http(s) image URLs while they are disabled.
const remoteImagesMsg = "remote image URLs are disabled; send the image as a data: URI (or set GEMINI_ALLOW_REMOTE_IMAGES=1 on the proxy)"

// hasRemoteImage reports whether any image must be fetched from a remote URL.
func hasRemoteImage(images []gemini.InputImage) bool {
	for _, im := range images {
		if len(im.Data) == 0 && im.URL != "" {
			return true
		}
	}
	return false
}

// rlog returns the logger tagged with the request id carried by ctx.
func (s *Server) rlog(ctx context.Context) *slog.Logger {
	if ri := reqFrom(ctx); ri != nil {
		return s.log.With("request_id", ri.id)
	}
	return s.log
}

// keepaliveInterval returns the SSE keepalive period; 0 means disabled.
func (s *Server) keepaliveInterval() time.Duration {
	if s.cfg.SSEKeepaliveSec <= 0 {
		return 0
	}
	if s.keepaliveOverride > 0 {
		return s.keepaliveOverride
	}
	return time.Duration(s.cfg.SSEKeepaliveSec) * time.Second
}

func newServer(cfg *Config, client *gemini.Client, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, client: client, log: logger, generate: client.Generate, started: time.Now()}
}

// handler builds the routed, middleware-wrapped HTTP handler. Health and model
// listings are open; generation endpoints are wrapped in the auth middleware.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.Handle("GET /_status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1beta/models", s.handleModelsGoogle)
	mux.Handle("POST /v1/chat/completions", s.withAuth(s.handleChat))
	mux.Handle("POST /v1/responses", s.withAuth(s.handleResponses))
	mux.Handle("POST /v1/messages", s.withAuth(s.handleMessages))
	mux.Handle("POST /v1/messages/count_tokens", s.withAuth(s.handleCountTokens))
	mux.Handle("POST /v1beta/models/{model}", s.withAuth(s.handleGoogleGenerate))
	mux.HandleFunc("/", s.handleNotFound)
	return s.withTracking(s.withHostCheck(s.withCORS(mux)))
}

// withAuth enforces the optional API-key gate around a generation handler.
func (s *Server) withAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			sendError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	sendError(w, http.StatusNotFound, "not found")
}

// authorized reports whether the request may access generation endpoints.
func (s *Server) authorized(r *http.Request) bool {
	if len(s.cfg.APIKeys) == 0 {
		return true
	}
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	if key == "" {
		key = r.Header.Get("x-goog-api-key")
	}
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	for _, allowed := range s.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(allowed), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

// ─── Simple endpoints ────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	sendJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: version,
		Models:  s.client.ModelNames(),
	})
}

// handleModels serves the model list in OpenAI format by default, or Anthropic
// format when the request carries an anthropic-version header (Claude Code etc.).
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("anthropic-version") != "" {
		s.handleModelsAnthropic(w)
		return
	}
	s.handleModelsOpenAI(w)
}

func (s *Server) handleModelsOpenAI(w http.ResponseWriter) {
	sendJSON(w, http.StatusOK, apiconv.OpenAIModelList(s.client.ListModels()))
}

func (s *Server) handleModelsGoogle(w http.ResponseWriter, _ *http.Request) {
	sendJSON(w, http.StatusOK, apiconv.GoogleModelList(s.client.ListModels()))
}

func (s *Server) handleModelsAnthropic(w http.ResponseWriter) {
	sendJSON(w, http.StatusOK, apiconv.AnthropicModelList(s.client.ListModels()))
}

// ─── OpenAI Chat Completions ─────────────────────────────────────────────────

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		sendError(w, bodyErrorStatus(err), "invalid body: "+err.Error())
		return
	}
	var req apiconv.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	m, err := s.client.ResolveModel(req.Model)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	prompt, images, toolsActive := req.Prompt()
	s.noteRequestParams(r.Context(), "chat", body, len(images))
	if len(images) > 0 && !s.client.ImagesSupported() {
		sendError(w, http.StatusBadRequest, apiconv.ImagesNeedAuthMsg)
		return
	}
	if hasRemoteImage(images) && !s.cfg.Gemini.AllowRemoteImages {
		sendError(w, http.StatusBadRequest, remoteImagesMsg)
		return
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		sendError(w, http.StatusBadRequest, "empty prompt")
		return
	}

	setReqMeta(r.Context(), m.Name, req.Stream)
	cid := apiconv.ID("chatcmpl-", 12)
	p := m.Params(prompt)
	p.Images = images

	// True streaming path (no active tools): forward deltas as they arrive.
	if req.Stream && !toolsActive {
		s.streamChat(r.Context(), w, cid, m.Name, p)
		return
	}

	// Stream + tools: the full response is still needed to parse tool calls, but
	// the SSE stream is opened immediately and kept alive with comment frames so
	// the client does not see a silent connection during long generations.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 {
		s.streamChatTools(r.Context(), w, cid, m.Name, p, interval)
		return
	}

	// Blocking path (also used for tool calling, which needs the full response).
	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive)

	if req.Stream {
		// Tools + stream: emit one chunk carrying the full message, then DONE.
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		s.sseTail(r.Context(), sse, sse.data(apiconv.ChatChunkComplete(cid, m.Name, text, toolCalls)), sse.done())
		return
	}

	sendJSON(w, http.StatusOK, apiconv.ChatCompletion(cid, m.Name, prompt, text, toolCalls))
}

// streamChat performs true token streaming for a chat completion.
func (s *Server) streamChat(ctx context.Context, w http.ResponseWriter, cid, model string, p gemini.GenParams) {
	created := time.Now().Unix()
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sse *sseStream
	noFlush := false
	var acc strings.Builder

	emitRaw := func(text string) {
		if sse == nil {
			var ok bool
			if sse, ok = openSSE(w); !ok {
				noFlush = true
				cancel()
				return
			}
		}
		acc.WriteString(text)
		if err := sse.data(apiconv.ChatChunk(cid, created, model, text)); err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel() // client is gone: stop pulling tokens
		}
	}
	emit, finish := s.echoEmitter(ctx, emitRaw, &acc)

	err := s.client.GenerateStream(gctx, p, emit)
	finish()
	if sse == nil {
		// Nothing was streamed yet, so a normal HTTP error status is still possible.
		// No deltas and no error means the upstream produced no content.
		switch {
		case noFlush:
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		default:
			sendError(w, http.StatusBadGateway, gemini.ErrEmptyResponse.Error())
		}
		return
	}
	if sse.err != nil {
		return // write already failed and was logged
	}
	if err != nil {
		// The stream was interrupted after deltas were already sent; the status is
		// committed, so surface the failure in-band as an error frame instead of a
		// clean "stop" chunk that would read as a complete response.
		s.rlog(ctx).Warn("stream interrupted", "err", err)
		s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+err.Error())), sse.done())
		return
	}
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkStop(cid, created, model)), sse.done())
}

// streamChatTools serves a stream=true request with active tools: SSE is opened
// up front and kept alive while the blocking generation runs (see awaitGenerate),
// then the usual single complete chunk and [DONE] follow.
func (s *Server) streamChatTools(ctx context.Context, w http.ResponseWriter, cid, model string, p gemini.GenParams, interval time.Duration) {
	sse, ok := openSSE(w)
	if !ok {
		sendError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	res := s.awaitGenerate(ctx, sse, p, interval)
	switch res.out {
	case kaGone:
		return
	case kaShutdown:
		s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
		return
	}
	if res.err != nil {
		// Headers are already sent: report in-band, in the same shape as streamChat.
		s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+res.err.Error())), sse.done())
		return
	}
	text, toolCalls := s.finishText(ctx, res.text, true)
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, text, toolCalls)), sse.done())
}

// ─── Shared keepalive helper ─────────────────────────────────────────────────

// kaOutcome tells how awaitGenerate ended.
type kaOutcome int

const (
	kaDone     kaOutcome = iota // generation finished (res.err may carry an upstream error)
	kaGone                      // client went away or an SSE write failed: write nothing more
	kaShutdown                  // request aborted by graceful-shutdown expiry
)

type kaResult struct {
	text string
	err  error
	out  kaOutcome
}

// awaitGenerate runs the blocking generation (s.generate) while writing an SSE
// ": keepalive" comment immediately and then every `interval` on an already-open
// stream. It is shared by every route that streams with active tools
// (/v1/chat/completions, /v1/responses, /v1/messages, /v1beta/...), so they all
// behave identically. Write errors end the wait (kaGone) and cancel the
// generation. Only this goroutine writes to the response; the generation goroutine
// reports through a buffered channel and so can always exit.
func (s *Server) awaitGenerate(ctx context.Context, sse *sseStream, p gemini.GenParams, interval time.Duration) kaResult {
	genCtx, cancel := context.WithCancel(ctx)
	defer cancel() // Generate never outlives the handler

	done := make(chan kaResult, 1)
	go func() {
		text, err := s.generate(genCtx, p)
		done <- kaResult{text: text, err: err}
	}()

	start := time.Now()
	elapsed := func() time.Duration { return time.Since(start).Round(time.Millisecond) }
	keepalive := func() bool {
		if err := sse.comment("keepalive"); err != nil {
			s.rlog(ctx).Warn("sse keepalive write failed", "err", err, "elapsed", elapsed())
			return false
		}
		addKeepalive(ctx)
		return true
	}
	cancelled := func() kaResult {
		if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
			return kaResult{out: kaShutdown}
		}
		s.rlog(ctx).Warn("stream+tools cancelled by client", "err", ctx.Err(), "elapsed", elapsed())
		return kaResult{out: kaGone}
	}

	// First comment right away so the client sees bytes after the headers.
	if !keepalive() {
		return kaResult{out: kaGone}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return cancelled()
		case <-ticker.C:
			if !keepalive() {
				return kaResult{out: kaGone}
			}
		case res := <-done:
			if res.err != nil && ctx.Err() != nil {
				return cancelled()
			}
			if res.err != nil {
				s.rlog(ctx).Warn("stream+tools upstream error after SSE open", "err", res.err, "elapsed", elapsed())
			}
			return res
		}
	}
}

// sseTail logs the first write error among the final-frame writes, if any.
func (s *Server) sseTail(ctx context.Context, _ *sseStream, errs ...error) {
	for _, err := range errs {
		if err != nil {
			s.rlog(ctx).Warn("sse write failed", "err", err)
			return
		}
	}
}

// ─── OpenAI Responses API ────────────────────────────────────────────────────

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		sendError(w, bodyErrorStatus(err), "invalid body: "+err.Error())
		return
	}
	var req apiconv.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	m, err := s.client.ResolveModel(req.Model)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	prompt, images, toolsActive := req.Prompt()
	s.noteRequestParams(r.Context(), "responses", body, len(images))
	if len(images) > 0 && !s.client.ImagesSupported() {
		sendError(w, http.StatusBadRequest, apiconv.ImagesNeedAuthMsg)
		return
	}
	if hasRemoteImage(images) && !s.cfg.Gemini.AllowRemoteImages {
		sendError(w, http.StatusBadRequest, remoteImagesMsg)
		return
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		sendError(w, http.StatusBadRequest, "empty input")
		return
	}

	setReqMeta(r.Context(), m.Name, req.Stream)
	p := m.Params(prompt)
	p.Images = images
	rid := apiconv.ID("resp_", 16)
	mid := apiconv.ID("msg_", 12)

	// Stream + tools: same keepalive behaviour as chat completions.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 {
		ctx := r.Context()
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		res := s.awaitGenerate(ctx, sse, p, interval)
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.event("error", responsesStreamError("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.event("error", responsesStreamError("upstream error: "+res.err.Error())))
		default:
			text, toolCalls := s.finishText(ctx, res.text, true)
			s.writeResponsesStream(ctx, sse, rid, mid, m.Name, prompt, text, toolCalls)
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive)

	if req.Stream {
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		s.writeResponsesStream(r.Context(), sse, rid, mid, m.Name, prompt, text, toolCalls)
		return
	}

	sendJSON(w, http.StatusOK, apiconv.ResponseObject(rid, mid, m.Name, prompt, text, toolCalls))
}

// responsesStreamError is the in-band error event payload for /v1/responses.
func responsesStreamError(msg string) any {
	return map[string]any{"type": "error", "message": msg}
}

// writeResponsesStream replays a complete Responses API result as SSE events.
func (s *Server) writeResponsesStream(ctx context.Context, sse *sseStream, rid, mid, model, prompt, text string, toolCalls []apiconv.ToolCall) {
	errs := []error{sse.event("response.created", apiconv.ResponseCreated(rid, model))}
	for _, tc := range toolCalls {
		errs = append(errs, sse.event("response.function_call_arguments.done", apiconv.ResponseFunctionCallDone(tc)))
	}
	if text != "" || len(toolCalls) == 0 {
		errs = append(errs, sse.event("response.output_text.done", apiconv.ResponseOutputTextDone(mid, 0, text)))
	}
	errs = append(errs, sse.event("response.completed", apiconv.ResponseCompleted(rid, mid, model, prompt, text, toolCalls)))
	s.sseTail(ctx, sse, errs...)
}

// ─── Google native API ───────────────────────────────────────────────────────

func (s *Server) handleGoogleGenerate(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		sendError(w, bodyErrorStatus(err), "invalid body: "+err.Error())
		return
	}
	name := parseGoogleModel(r.URL.Path)
	if name == "" {
		sendError(w, http.StatusBadRequest, "model not specified in path")
		return
	}
	m, err := s.client.ResolveModel(name)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req apiconv.GenerateContentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	prompt, images, hasTools := req.Prompt()
	s.noteRequestParams(r.Context(), "google", body, len(images))
	if len(images) > 0 && !s.client.ImagesSupported() {
		sendError(w, http.StatusBadRequest, apiconv.ImagesNeedAuthMsg)
		return
	}
	if hasRemoteImage(images) && !s.cfg.Gemini.AllowRemoteImages {
		sendError(w, http.StatusBadRequest, remoteImagesMsg)
		return
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		sendError(w, http.StatusBadRequest, "empty content")
		return
	}
	stream := strings.Contains(r.URL.Path, ":streamGenerateContent")
	setReqMeta(r.Context(), m.Name, stream)
	p := m.Params(prompt)
	p.Images = images

	// Incremental streaming (no tools): emit a chunk per delta, then a final
	// chunk with finishReason and usage.
	if stream && !hasTools {
		s.streamGoogle(r.Context(), w, m.Name, p)
		return
	}

	// Stream + tools: keepalive while the blocking generation runs.
	if interval := s.keepaliveInterval(); stream && hasTools && interval > 0 {
		ctx := r.Context()
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		res := s.awaitGenerate(ctx, sse, p, interval)
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+res.err.Error())))
		default:
			s.sseTail(ctx, sse, sse.data(s.googleResponse(ctx, m.Name, prompt, res.text, hasTools)))
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	response := s.googleResponse(r.Context(), m.Name, prompt, text, hasTools)

	if stream {
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		s.sseTail(r.Context(), sse, sse.data(response))
		return
	}
	sendJSON(w, http.StatusOK, response)
}

// streamGoogle performs incremental streaming for a tool-free Google native request.
func (s *Server) streamGoogle(ctx context.Context, w http.ResponseWriter, model string, p gemini.GenParams) {
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sse *sseStream
	noFlush := false
	var full strings.Builder

	emitRaw := func(delta string) {
		if sse == nil {
			var ok bool
			if sse, ok = openSSE(w); !ok {
				noFlush = true
				cancel()
				return
			}
		}
		full.WriteString(delta)
		if err := sse.data(apiconv.GoogleStreamChunk(model, delta)); err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel()
		}
	}
	emit, finish := s.echoEmitter(ctx, emitRaw, &full)

	err := s.client.GenerateStream(gctx, p, emit)
	finish()
	if sse == nil {
		switch {
		case noFlush:
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		default:
			sendError(w, http.StatusBadGateway, gemini.ErrEmptyResponse.Error())
		}
		return
	}
	if sse.err != nil {
		return
	}
	if err != nil {
		// Interrupted after deltas: emit a final frame with a non-STOP finishReason
		// so the client can tell the generation did not complete normally.
		s.rlog(ctx).Warn("stream interrupted", "err", err)
		s.sseTail(ctx, sse, sse.data(apiconv.GoogleStreamError(model, p.Prompt, full.String())))
		return
	}
	s.sseTail(ctx, sse, sse.data(apiconv.GoogleStreamFinal(model, p.Prompt, full.String())))
}

func parseGoogleModel(path string) string {
	if m := googleModelRe.FindStringSubmatch(path); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// ─── Anthropic Messages API ──────────────────────────────────────────────────

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		sendAnthropicError(w, bodyErrorStatus(err), "invalid body: "+err.Error())
		return
	}
	var req apiconv.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendAnthropicError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	m := s.client.ResolveModelOrDefault(req.Model)
	prompt, images, toolsActive := req.Prompt()
	s.noteRequestParams(r.Context(), "anthropic", body, len(images))
	if len(images) > 0 && !s.client.ImagesSupported() {
		sendAnthropicError(w, http.StatusBadRequest, apiconv.ImagesNeedAuthMsg)
		return
	}
	if hasRemoteImage(images) && !s.cfg.Gemini.AllowRemoteImages {
		sendAnthropicError(w, http.StatusBadRequest, remoteImagesMsg)
		return
	}
	if strings.TrimSpace(prompt) == "" && len(images) == 0 {
		sendAnthropicError(w, http.StatusBadRequest, "empty prompt")
		return
	}

	setReqMeta(r.Context(), m.Name, req.Stream)
	p := m.Params(prompt)
	p.Images = images
	msgID := apiconv.ID("msg_", 24)

	// True streaming (no active tools): forward text deltas as they arrive.
	if req.Stream && !toolsActive {
		s.streamMessages(r.Context(), w, msgID, m.Name, p)
		return
	}

	// Stream + tools: keepalive while the blocking generation runs.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 {
		ctx := r.Context()
		sse, ok := openSSE(w)
		if !ok {
			sendAnthropicError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		res := s.awaitGenerate(ctx, sse, p, interval)
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.event("error", apiconv.AnthropicError("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.event("error", apiconv.AnthropicError("upstream error: "+res.err.Error())))
		default:
			text, toolCalls := s.finishText(ctx, res.text, true)
			s.streamMessagesFull(ctx, sse, msgID, m.Name, prompt, text, toolCalls)
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendAnthropicError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive)

	if req.Stream {
		// Tools + stream (keepalive disabled): replay the full message as SSE events.
		sse, ok := openSSE(w)
		if !ok {
			sendAnthropicError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		s.streamMessagesFull(r.Context(), sse, msgID, m.Name, prompt, text, toolCalls)
		return
	}
	sendJSON(w, http.StatusOK, apiconv.AnthropicResponse(m.Name, prompt, text, toolCalls))
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(w, r)
	if err != nil {
		sendAnthropicError(w, bodyErrorStatus(err), "invalid body: "+err.Error())
		return
	}
	var req apiconv.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		sendAnthropicError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	prompt, _, _ := req.Prompt()
	sendJSON(w, http.StatusOK, apiconv.AnthropicCountTokens(prompt))
}

// streamMessages performs true token streaming for a tool-free message request.
func (s *Server) streamMessages(ctx context.Context, w http.ResponseWriter, msgID, model string, p gemini.GenParams) {
	inputTokens := apiconv.ApproxTokens(p.Prompt)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sse *sseStream
	noFlush := false
	var acc strings.Builder

	open := func() bool {
		var ok bool
		if sse, ok = openSSE(w); !ok {
			noFlush = true
			cancel()
			return false
		}
		sse.event("message_start", apiconv.AnthropicMessageStart(msgID, model, inputTokens))
		sse.event("content_block_start", apiconv.AnthropicTextBlockStart(0))
		return true
	}

	emitRaw := func(text string) {
		if sse == nil && !open() {
			return
		}
		acc.WriteString(text)
		if err := sse.event("content_block_delta", apiconv.AnthropicTextDelta(0, text)); err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel()
		}
	}
	emit, finish := s.echoEmitter(ctx, emitRaw, &acc)

	err := s.client.GenerateStream(gctx, p, emit)
	finish()
	if sse == nil {
		// Nothing was streamed yet, so a normal HTTP error status is still possible.
		// No deltas and no error means the upstream produced no content.
		switch {
		case noFlush:
			sendAnthropicError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendAnthropicError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		default:
			sendAnthropicError(w, http.StatusBadGateway, gemini.ErrEmptyResponse.Error())
		}
		return
	}
	if sse.err != nil {
		s.rlog(ctx).Warn("sse write failed", "err", sse.err)
		return
	}
	if err != nil {
		// Interrupted after deltas: close the open block and emit an Anthropic
		// `error` event instead of a clean message_stop, so the partial response is
		// not presented as complete.
		s.rlog(ctx).Warn("stream interrupted", "err", err)
		s.sseTail(ctx, sse,
			sse.event("content_block_stop", apiconv.AnthropicBlockStop(0)),
			sse.event("error", apiconv.AnthropicError("upstream error: "+err.Error())))
		return
	}
	s.sseTail(ctx, sse,
		sse.event("content_block_stop", apiconv.AnthropicBlockStop(0)),
		sse.event("message_delta", apiconv.AnthropicMessageDelta("end_turn", apiconv.ApproxTokens(acc.String()))),
		sse.event("message_stop", apiconv.AnthropicMessageStop()))
}

// streamMessagesFull replays an already-computed message as an SSE sequence on an
// already-open stream, used when tools are present (the tool calls need the full
// text first). It mirrors the block layout of apiconv.AnthropicResponse: a text
// block (when there is text), then a tool_use block per call, or a single empty
// text block when there is neither.
func (s *Server) streamMessagesFull(ctx context.Context, sse *sseStream, msgID, model, prompt, text string, toolCalls []apiconv.ToolCall) {
	errs := []error{sse.event("message_start", apiconv.AnthropicMessageStart(msgID, model, apiconv.ApproxTokens(prompt)))}

	idx := 0
	emitTextBlock := func(t string) {
		errs = append(errs,
			sse.event("content_block_start", apiconv.AnthropicTextBlockStart(idx)),
			sse.event("content_block_delta", apiconv.AnthropicTextDelta(idx, t)),
			sse.event("content_block_stop", apiconv.AnthropicBlockStop(idx)))
		idx++
	}
	if text != "" {
		emitTextBlock(text)
	}
	for _, tc := range toolCalls {
		errs = append(errs,
			sse.event("content_block_start", apiconv.AnthropicToolUseBlockStart(idx, tc)),
			sse.event("content_block_delta", apiconv.AnthropicToolUseDelta(idx, tc)),
			sse.event("content_block_stop", apiconv.AnthropicBlockStop(idx)))
		idx++
	}
	if text == "" && len(toolCalls) == 0 {
		emitTextBlock("")
	}

	stop := "end_turn"
	if len(toolCalls) > 0 {
		stop = "tool_use"
	}
	errs = append(errs,
		sse.event("message_delta", apiconv.AnthropicMessageDelta(stop, apiconv.ApproxTokens(text))),
		sse.event("message_stop", apiconv.AnthropicMessageStop()))
	s.sseTail(ctx, sse, errs...)
}

// sendAnthropicError writes an Anthropic-shaped error envelope.
func sendAnthropicError(w http.ResponseWriter, status int, msg string) {
	sendJSON(w, status, apiconv.AnthropicError(msg))
}

// ─── Output types (proxy-level) ──────────────────────────────────────────────

// errorResponse is the OpenAI-style error envelope.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
}

// healthResponse is returned by GET /.
type healthResponse struct {
	Status  string   `json:"status"`
	Version string   `json:"version"`
	Models  []string `json:"models"`
}

// ─── HTTP / SSE helpers ──────────────────────────────────────────────────────

// readBody reads the request body, capping it at maxBodyBytes. It uses
// http.MaxBytesReader so an over-limit body surfaces as an explicit
// *http.MaxBytesError (mapped to 413 by bodyErrorStatus) rather than being
// silently truncated into a confusing JSON parse error.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
}

// bodyErrorStatus maps a readBody error onto an HTTP status: 413 when the body
// exceeded maxBodyBytes, 400 otherwise.
func bodyErrorStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// errorObj wraps a message in the OpenAI error envelope.
func errorObj(msg string) errorResponse {
	return errorResponse{Error: errorDetail{Message: msg}}
}

// sendError writes a JSON error envelope with the given status code.
func sendError(w http.ResponseWriter, status int, msg string) {
	sendJSON(w, status, errorObj(msg))
}

// sendJSON writes a JSON response with the given status code.
func sendJSON(w http.ResponseWriter, status int, v any) {
	b, err := util.MarshalNoEscape(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// sseStream wraps an open SSE response with a sticky error: after the first
// failed write every later write is a no-op returning that error, so callers can
// issue a sequence of writes and inspect the outcome once.
type sseStream struct {
	w   http.ResponseWriter
	f   http.Flusher
	err error
}

// openSSE writes SSE response headers and returns the stream.
func openSSE(w http.ResponseWriter) (*sseStream, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseStream{w: w, f: f}, true
}

func (s *sseStream) do(fn func() error) error {
	if s.err != nil {
		return s.err
	}
	s.err = fn()
	return s.err
}

func (s *sseStream) data(v any) error { return s.do(func() error { return writeSSEData(s.w, s.f, v) }) }
func (s *sseStream) event(name string, v any) error {
	return s.do(func() error { return writeSSEEvent(s.w, s.f, name, v) })
}
func (s *sseStream) comment(text string) error {
	return s.do(func() error { return writeSSEComment(s.w, s.f, text) })
}
func (s *sseStream) done() error { return s.do(func() error { return writeSSEDone(s.w, s.f) }) }

// writeSSEData writes a `data: <json>` SSE frame.
func writeSSEData(w http.ResponseWriter, f http.Flusher, v any) error {
	b, err := util.MarshalNoEscape(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// writeSSEComment writes an SSE comment line (": <text>") and flushes. Comments
// are ignored by compliant SSE parsers but still put bytes on the wire.
func writeSSEComment(w http.ResponseWriter, f http.Flusher, text string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", text); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// writeSSEEvent writes a named SSE event with a JSON payload.
func writeSSEEvent(w http.ResponseWriter, f http.Flusher, event string, v any) error {
	b, err := util.MarshalNoEscape(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// writeSSEDone writes the terminating [DONE] frame.
func writeSSEDone(w http.ResponseWriter, f http.Flusher) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	f.Flush()
	return nil
}
