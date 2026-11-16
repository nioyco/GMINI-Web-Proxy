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
	"sync"
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
	// streamEvents is the streaming generation used by the progressive stream+tools
	// chat path (P9); nil keeps the buffered path. A seam for tests.
	streamEvents func(ctx context.Context, p gemini.GenParams, h gemini.StreamHandlers) error
	// keepaliveOverride, when > 0, replaces the cfg-derived keepalive interval
	// (sub-second values for tests). Never set in production.
	keepaliveOverride time.Duration
	// heldDurationOverride and heldNoteOverride override the K9 120 s held cap
	// and 30 s progress note period in tests. Never set in production.
	heldDurationOverride time.Duration
	heldNoteOverride     time.Duration
	statusNoteOverride   time.Duration
	// uiToken protects the API of the local management page (ui.go); empty: page off.
	uiToken string

	started time.Time
	active  activeSet
	stats   statsCounters
	ignored ignoredStats

	suspectWarnMu sync.Mutex
	suspectWarn   map[string]suspectWarnState
	warnNow       func() time.Time

	autoRefresh *cookieAutoRefresher
}

type suspectWarnState struct {
	verdict string
	at      time.Time
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
	apiconv.SetToolPromptRules(!cfg.DisableToolPromptRules)
	apiconv.SetMarkerNeutralization(!cfg.DisableMarkerNeutralization)
	apiconv.SetDropTextAfterToolCall(!cfg.KeepTextAfterToolCall)
	srv := &Server{cfg: cfg, client: client, log: logger, generate: client.Generate, started: time.Now()}
	srv.autoRefresh = newCookieAutoRefresher(cfg, client, logger)
	if cfg.UIEnabled {
		srv.uiToken = newUIToken()
	}
	if cfg.StreamToolsProgress {
		srv.streamEvents = client.GenerateStreamEvents
	}
	return srv
}

// handler builds the routed, middleware-wrapped HTTP handler. Health and model
// listings are open; generation endpoints are wrapped in the auth middleware.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.Handle("GET /_status", s.withAuth(s.handleStatus))
	mux.Handle("POST /_models/refresh", s.withAuth(s.handleModelsRefresh))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1beta/models", s.handleModelsGoogle)
	mux.Handle("POST /v1/chat/completions", s.withAuth(s.handleChat))
	mux.Handle("POST /v1/responses", s.withAuth(s.handleResponses))
	mux.Handle("POST /v1/messages", s.withAuth(s.handleMessages))
	mux.Handle("POST /v1/messages/count_tokens", s.withAuth(s.handleCountTokens))
	mux.Handle("POST /v1beta/models/{model}", s.withAuth(s.handleGoogleGenerate))
	mux.HandleFunc("GET /_ui", s.handleUI)
	mux.HandleFunc("GET /_ui/api/state", s.handleUIState)
	mux.HandleFunc("POST /_ui/api/models/refresh", s.handleUIRefresh)
	mux.HandleFunc("POST /_ui/api/cookie-refresh/toggle", s.handleUICookieRefreshToggle)
	mux.HandleFunc("POST /_ui/api/antigravity/launch", s.handleUILaunch)
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

// handleModelsRefresh refreshes the model catalog on explicit request (one
// listModels call at most; refused without any Google call when the session is
// not authenticated, a block is in force, or the last attempt is too recent).
func (s *Server) handleModelsRefresh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := s.client.RefreshModels(ctx)
	resp := map[string]any{"refreshed": err == nil, "models_count": len(s.client.ModelNames()),
		"models_source": s.client.ModelsSourceLabel(), "session_degraded": s.client.CatalogHealth().SessionDegraded}
	if next := s.client.CatalogRefreshNextAt(); !next.IsZero() {
		if rem := int64(time.Until(next).Seconds() + 0.999); rem > 0 {
			resp["cooldown_sec"] = rem
			resp["next_refresh_at"] = next.UTC().Format(time.RFC3339)
		}
	}
	status := http.StatusOK
	switch {
	case errors.Is(err, gemini.ErrCatalogRefreshRefused):
		status = http.StatusConflict
		resp["error"] = err.Error()
	case errors.Is(err, gemini.ErrCatalogDegraded):
		status = http.StatusConflict
		resp["error"] = err.Error()
	case err != nil:
		status = http.StatusBadGateway
		resp["error"] = err.Error()
	}
	sendJSON(w, status, resp)
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

	modelName, effort := apiconv.SplitModelEffort(req.Model)
	m, err := s.client.ResolveModel(modelName)
	if err != nil {
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}

	prompt, images, toolsActive := req.Prompt()
	s.noteRequestParams(r.Context(), "chat", body, len(images))
	if max := s.cfg.MaxPromptBytes; max > 0 && len(prompt) > max {
		s.rlog(r.Context()).Warn("prompt_too_large", "prompt_bytes", len(prompt), "max", max)
		sendError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"prompt too large for the Gemini web endpoint: %d bytes (limit %d, GEMINI_MAX_PROMPT_BYTES); reduce the context", len(prompt), max))
		return
	}
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
	if s.autoRefresh != nil {
		s.autoRefresh.PrepareRequest(r.Context(), func() {
			if req.Stream {
				if sse, ok := openSSE(w); ok {
					if s.cfg.StreamThoughts {
						_ = sse.data(apiconv.ChatChunkReasoning(cid, time.Now().Unix(), m.Name, "GMINI : actualisation de la session…\n"))
					} else {
						_ = sse.comment("GMINI : actualisation de la session…")
					}
				}
			}
		})
	}
	s.applyEffort(&p, m, effort, req.ReasoningEffort)

	// True streaming path (no active tools): forward deltas as they arrive.
	if req.Stream && !toolsActive {
		s.streamChat(r.Context(), w, cid, m.Name, p)
		return
	}

	// Stream + tools, progressive (P9): text and thoughts go out as they arrive,
	// tool calls are extracted at the end.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 && s.streamEvents != nil {
		s.streamChatToolsProgress(r.Context(), w, cid, m.Name, p, interval, req.DeclaredTools())
		return
	}

	// Stream + tools: the full response is still needed to parse tool calls, but
	// the SSE stream is opened immediately and kept alive with comment frames so
	// the client does not see a silent connection during long generations.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 {
		s.streamChatTools(r.Context(), w, cid, m.Name, p, interval, req.DeclaredTools())
		return
	}

	// Blocking path (also used for tool calling, which needs the full response).
	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())

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
func (s *Server) streamChatTools(ctx context.Context, w http.ResponseWriter, cid, model string, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool) {
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
	text, toolCalls := s.finishText(ctx, res.text, true, declared)
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, text, toolCalls)), sse.done())
}

// applyEffort sets the experimental thinking level (and the model's mode code)
// on p. The effort comes from the "model@effort" suffix, else the request's
// reasoning_effort, else GEMINI_THINKING_LEVEL. Nothing is sent when none is set.
func (s *Server) applyEffort(p *gemini.GenParams, m *gemini.AvailableModel, suffix, requested string) {
	effort := suffix
	if effort == "" {
		effort = requested
	}
	if apiconv.EffortLevel(effort) == 0 {
		effort = s.cfg.DefaultThinkingLevel
	}
	level := apiconv.EffortLevel(effort)
	if level == 0 || m == nil || m.ModelID == "" {
		return
	}
	p.Level = level
	// The mode code comes from the catalogue when Google provides it; the
	// configured table (GEMINI_THINKING_MODE_CODES) overrides it.
	p.Mode = m.ModeCode
	if v, ok := s.cfg.ThinkingModes[m.ModelID]; ok && (p.Mode == 0 || s.cfg.ThinkingModesExplicit) {
		p.Mode = v
	}
}

// streamChatToolsProgress serves a stream=true chat request with active tools
// progressively: thinking text is forwarded as reasoning_content, answer text is
// forwarded until a block that may be a tool call begins (see apiconv.ProgressGate),
// keepalive comments fill the silences, and the tool calls plus any held-back text
// follow in the final chunk. If the stream fails or stays empty before anything was
// forwarded, the buffered path (with its retries) takes over on the same response.
func (s *Server) streamChatToolsProgress(ctx context.Context, w http.ResponseWriter, cid, model string, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool) {
	sse, ok := openSSE(w)
	if !ok {
		sendError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	var gate apiconv.ProgressGate
	var echo apiconv.EchoFilter
	res := s.runStreamTools(ctx, sse, p, interval, declared, &streamToolsHooks{
		progressive: true,
		cid:         cid,
		created:     time.Now().Unix(),
		model:       model,
		gate:        &gate,
		echo:        &echo,
	})
	if res.aborted {
		return
	}
	if res.earlyStopMsg != "" {
		s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, res.earlyStopMsg, nil)), sse.done())
		return
	}
	if res.earlyStopReason == "" && (res.genErr != nil || res.rawText == "") {
		if !res.forwarded && (res.genErr == nil || gemini.Retryable(res.genErr)) && !errors.Is(res.genErr, gemini.ErrSessionAuth) {
			// Nothing reached the client yet: let the buffered path retry.
			s.rlog(ctx).Warn("progressive stream gave nothing; falling back to the buffered path", "err", res.genErr)
			bres := s.awaitGenerate(ctx, sse, p, interval)
			switch {
			case bres.out == kaGone:
			case bres.out == kaShutdown:
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			case bres.err != nil:
				s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+bres.err.Error())), sse.done())
			default:
				if note := s.maybeSuspectWarning(ctx, model); note != "" {
					if err := sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, note)); err != nil {
						s.rlog(ctx).Warn("sse write failed", "err", err)
						return
					}
				}
				text, toolCalls := s.finishText(ctx, bres.text, true, declared)
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, text, toolCalls)), sse.done())
			}
			return
		}
		genErr := res.genErr
		if genErr == nil {
			genErr = gemini.ErrEmptyResponse
		}
		s.rlog(ctx).Warn("stream+tools upstream error after SSE open", "err", genErr, "forwarded", res.forwarded)
		s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+genErr.Error())), sse.done())
		return
	}

	if res.earlyStopReason == "" {
		isGlitch := !res.released && apiconv.IsUpstreamGlitch(res.rawText)
		isToolRefusal := !res.released && len(declared) > 0 && apiconv.IsUpstreamToolRefusal(res.rawText)
		if isGlitch || isToolRefusal {
			// Gemini sent a canned non-answer (generic refusal, "can't use Workspace",
			// a bare widget placeholder, or claimed local tools are not declared/accessible).
			// Nothing of it reached the client: try once more.
			s.rlog(ctx).Warn("upstream_glitch", "bytes", len(res.rawText), "tool_refusal", isToolRefusal, "action", "retry_once")
			if s.cfg.StreamThoughts {
				retryMsg := "\nGemini a renvoyé un message de service au lieu de traiter la demande : nouvelle tentative…\n"
				if isToolRefusal {
					retryMsg = "\nGemini affirme ne pas avoir d'outils locaux : nouvelle tentative avec consigne corrective…\n"
				}
				if err := sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, retryMsg)); err != nil {
					s.rlog(ctx).Warn("sse write failed", "err", err)
					return
				}
			}
			retryParams := glitchRetryParams(p)
			if isToolRefusal {
				retryParams = toolRefusalRetryParams(p)
			}
			bres := s.awaitGenerate(ctx, sse, retryParams, interval)
			switch {
			case bres.out == kaGone:
			case bres.out == kaShutdown:
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			case bres.err != nil:
				s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+bres.err.Error())), sse.done())
			case isToolRefusal && apiconv.IsUpstreamToolRefusal(bres.text):
				s.rlog(ctx).Warn("upstream_tool_refusal", "bytes", len(bres.text), "action", "reported")
				msg := "GMINI : Gemini a affirmé deux fois qu'aucun outil local n'est déclaré ou disponible (« " +
					strings.TrimSpace(bres.text) + " »). Relancez la demande ou ouvrez une nouvelle conversation."
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
			case apiconv.IsUpstreamGlitch(bres.text):
				s.rlog(ctx).Warn("upstream_glitch", "bytes", len(bres.text), "action", "reported")
				msg := "GMINI : Gemini a répondu deux fois par un message de service au lieu de traiter la demande (« " +
					strings.TrimSpace(bres.text) + " »). Reformulez la demande en donnant le chemin complet du dossier ou du fichier, puis relancez."
				if strings.Contains(bres.text, "<GenerateWidget") {
					msg = "GMINI : Gemini a tenté deux fois d'ouvrir un de ses propres outils d'affichage au lieu de répondre. Relancez la demande, ou ouvrez une nouvelle conversation."
				}
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
			default:
				if !res.warnEmitted {
					if note := s.maybeSuspectWarning(ctx, model); note != "" {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, note)); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
				}
				text, toolCalls := s.finishText(ctx, bres.text, true, declared)
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, text, toolCalls)), sse.done())
			}
			return
		}
	}

	ri := reqFrom(ctx)
	sentBytes := res.transmittedBytes
	if !res.released {
		if res.early != "" {
			if err := sse.data(apiconv.ChatChunk(cid, res.created, model, res.early)); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return
			}
			sentBytes += len(res.early)
			ri.setSent(sentBytes)
		}
	}
	if !s.cfg.DisableEchoFilter {
		if out := gate.Push(echo.Flush()); out != "" {
			if err := sse.data(apiconv.ChatChunk(cid, res.created, model, out)); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return
			}
			sentBytes += len(out)
			ri.setSent(sentBytes)
		}
	}
	// Say so when another model than the one asked for probably answered.
	if !res.warnEmitted {
		if note := s.maybeSuspectWarning(ctx, model); note != "" {
			if err := sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, note)); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return
			}
		}
	}
	text, toolCalls := s.finishText(ctx, strings.TrimSpace(res.rawText), true, declared)
	rest, ok := gate.Rest(text)
	if !ok {
		s.rlog(ctx).Warn("progress_text_mismatch", "sent_bytes", len(gate.Sent()), "final_bytes", len(text))
	}
	ri.setSent(sentBytes + len(rest))
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, rest, toolCalls)), sse.done())
}

type streamToolsHooks struct {
	progressive bool
	cid         string
	created     int64
	model       string
	gate        *apiconv.ProgressGate
	echo        *apiconv.EchoFilter
}

type streamToolsResult struct {
	rawText          string
	earlyStopReason  string // "" | "marker" | "size" | "duration"
	earlyStopMsg     string
	genErr           error
	forwarded        bool
	released         bool
	early            string
	warnEmitted      bool
	aborted          bool // SSE write failed or client cancelled
	created          int64
	transmittedBytes int
}

// runStreamTools drives s.streamEvents for all stream+tools routes with keepalives,
// in-flight tracking, K8 silence detection, and K9 early-stop / runaway caps.
func (s *Server) runStreamTools(ctx context.Context, sse *sseStream, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool, hooks *streamToolsHooks) streamToolsResult {
	var res streamToolsResult
	if hooks != nil {
		res.created = hooks.created
	}
	genCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type event struct {
		text       string
		thought    string
		status     string
		servedID   string
		servedName string
	}
	events := make(chan event, 256)
	done := make(chan error, 1)
	push := func(e event) {
		select {
		case events <- e:
		case <-genCtx.Done():
		}
	}
	ri := reqFrom(ctx)
	h := gemini.StreamHandlers{
		Text:   func(d string) { push(event{text: d}) },
		Status: func(st string) { push(event{status: st}) },
		Identity: func(servedID, servedName string) {
			push(event{servedID: servedID, servedName: servedName})
		},
		Progress: func(textBytes, thoughtBytes int) {
			if ri != nil {
				ri.setProgress(thoughtBytes, textBytes)
			}
		},
	}
	if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
		h.Thought = func(d string) { push(event{thought: d}) }
	}
	go func() { done <- s.streamEvents(genCtx, p, h) }()

	var raw strings.Builder
	filter := func(d string) string {
		if hooks == nil || hooks.echo == nil || s.cfg.DisableEchoFilter {
			return d
		}
		return hooks.echo.Push(d)
	}
	start, lastWrite := time.Now(), time.Now()
	const earlyHold = 400
	var early strings.Builder
	thoughtBytes := 0
	lastFrameTime := start
	silenceWarned := false
	silenceThreshold := time.Duration(s.silenceWarningSec()) * time.Second
	maxHeldBytes := s.maxHeldBytes()
	maxHeldDur := s.maxHeldDuration()
	heldNoteEvery := s.heldNoteInterval()
	statusNoteEvery := s.statusNoteInterval()

	rawAtLastUsefulTx := 0
	var heldSince, lastHeldNoteAt, lastStatusAt time.Time
	var lastStatusText string
	completeToolCalls, lastCompleteEnd := 0, 0
	lastHeldNoteBytes := 0

	write := func(err error) bool {
		if err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel(err)
			res.aborted = true
			return false
		}
		lastWrite = time.Now()
		return true
	}
	keepalive := func() bool {
		if err := sse.comment("keepalive"); err != nil {
			s.rlog(ctx).Warn("sse keepalive write failed", "err", err)
			cancel(err)
			res.aborted = true
			return false
		}
		addKeepalive(ctx)
		lastWrite = time.Now()
		return true
	}
	cancelled := func() {
		res.aborted = true
		if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
			if hooks != nil && hooks.progressive {
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			}
			return
		}
		s.rlog(ctx).Warn("stream+tools cancelled by client", "err", ctx.Err(), "elapsed", time.Since(start).Round(time.Millisecond))
	}
	triggerCap := func(reason string, heldBytes int) {
		res.earlyStopReason = reason
		s.noteEarlyStop(ctx, reason, completeToolCalls, raw.Len(), heldBytes, res.transmittedBytes, time.Since(start))
		if completeToolCalls > 0 && lastCompleteEnd > 0 {
			cancel(gemini.ErrEarlyStop)
			res.rawText = raw.String()[:lastCompleteEnd]
		} else {
			cancel(gemini.ErrEarlyStopAborted)
			res.earlyStopMsg = fmt.Sprintf("GMINI : la réponse de Gemini s'emballe (%d Ko sans fin) ; demande interrompue.", koCount(heldBytes))
		}
	}

	handle := func(e event) bool {
		if apiconv.IsInternalToolSignal(e.thought) || apiconv.IsInternalToolSignal(e.text) {
			if ri != nil && !ri.internalTool.Swap(true) {
				s.stats.internalToolDetected.Add(1)
			}
		}
		if e.servedID != "" {
			verdict := "confirmed"
			if p.Model != nil && p.Model.ModelID != "" && e.servedID != p.Model.ModelID {
				verdict = "other_model"
				if p.Model.Description != "" && e.servedName == p.Model.Description {
					verdict = "name_only"
				}
			}
			if ri != nil {
				ri.setServed(e.servedID, e.servedName, verdict)
			}
			if verdict == "other_model" && s.autoRefresh != nil {
				s.autoRefresh.TriggerEarly()
			}
			if hooks != nil && hooks.progressive && !res.warnEmitted && (verdict == "other_model" || verdict == "name_only") {
				if note := s.maybeSuspectWarningFor(ctx, hooks.model, verdict, e.servedID); note != "" {
					res.warnEmitted = true
					if !write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, note))) {
						return false
					}
				}
			}
			return true
		}
		if e.status != "" {
			lastFrameTime = time.Now()
			silenceWarned = false
			if ri != nil {
				ri.setStatusNote("Google : " + e.status)
			}
			if e.status != lastStatusText && hooks != nil && hooks.progressive && s.cfg.StreamThoughts &&
				(lastStatusAt.IsZero() || time.Since(lastStatusAt) >= statusNoteEvery) {
				lastStatusText = e.status
				lastStatusAt = time.Now()
				res.forwarded = true
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, "\nGoogle : "+e.status+"\n")))
			}
			return true
		}
		if e.thought != "" {
			thoughtBytes += len(e.thought)
			lastFrameTime = time.Now()
			if ri != nil {
				ri.setReceived(thoughtBytes, raw.Len())
			}
			if hooks != nil && hooks.progressive {
				res.forwarded = true
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, e.thought)))
			}
			return true
		}
		lastFrameTime = time.Now()
		if silenceWarned && ri != nil {
			ri.setStatusNote("")
		}
		prevRawLen := raw.Len()
		raw.WriteString(e.text)
		if ri != nil {
			ri.setReceived(thoughtBytes, raw.Len())
		}

		scan := apiconv.ScanToolCalls(raw.String(), declared)
		if scan.CompleteCalls > 0 && completeToolCalls == 0 {
			lastHeldNoteAt = time.Now()
		}
		completeToolCalls, lastCompleteEnd = scan.CompleteCalls, scan.LastCallEnd
		if scan.HasMarker {
			heldBytes := raw.Len() - rawAtLastUsefulTx
			res.earlyStopReason = "marker"
			res.rawText = raw.String()[:scan.CutAt]
			s.noteEarlyStop(ctx, "marker", completeToolCalls, raw.Len(), heldBytes, res.transmittedBytes, time.Since(start))
			cancel(gemini.ErrEarlyStop)
			if hooks != nil && hooks.progressive && hooks.gate != nil {
				cutInDelta := scan.CutAt - prevRawLen
				if cutInDelta < 0 {
					cutInDelta = 0
				}
				if cutInDelta > len(e.text) {
					cutInDelta = len(e.text)
				}
				if cutInDelta > 0 {
					out := hooks.gate.Push(filter(e.text[:cutInDelta]))
					if !res.released {
						early.WriteString(out)
					} else if out != "" {
						res.forwarded = true
						res.transmittedBytes += len(out)
						ri.setSent(res.transmittedBytes)
						if !write(sse.data(apiconv.ChatChunk(hooks.cid, hooks.created, hooks.model, out))) {
							return false
						}
					}
				}
			}
			return false
		}

		if hooks != nil && hooks.progressive && hooks.gate != nil {
			out := hooks.gate.Push(filter(e.text))
			if !res.released {
				// The first few hundred bytes are held back: a canned non-answer of
				// Gemini is short, and holding it lets the request be retried instead.
				early.WriteString(out)
				if raw.Len() >= earlyHold {
					res.released, out = true, early.String()
				} else {
					out = ""
				}
			}
			if res.released && out != "" {
				res.forwarded = true
				rawAtLastUsefulTx = raw.Len()
				heldSince = time.Time{}
				res.transmittedBytes += len(out)
				ri.setSent(res.transmittedBytes)
				if !write(sse.data(apiconv.ChatChunk(hooks.cid, hooks.created, hooks.model, out))) {
					return false
				}
			}
		}

		heldBytes := raw.Len() - rawAtLastUsefulTx
		if heldBytes > 0 && heldSince.IsZero() {
			heldSince = time.Now()
		}
		if maxHeldBytes > 0 && heldBytes > maxHeldBytes {
			triggerCap("size", heldBytes)
			return false
		}
		return true
	}

	if !keepalive() { // bytes right after the headers
		return res
	}
	tick := interval / 4
	if tick <= 0 {
		tick = interval
	}
	if maxHeldDur > 0 && maxHeldDur/4 > 0 && maxHeldDur/4 < tick {
		tick = maxHeldDur / 4
	}
	if heldNoteEvery > 0 && heldNoteEvery/4 > 0 && heldNoteEvery/4 < tick {
		tick = heldNoteEvery / 4
	}
	if tick < 5*time.Millisecond {
		tick = 5 * time.Millisecond
	}

	// While a (possibly long) tool call is being written nothing can be forwarded:
	// a short progress note goes out as reasoning_content instead, so the client
	// shows activity and its inactivity timer is reset by real data.
	hintEvery, lastHeld := interval/3, 0
	hint := func() bool {
		if completeToolCalls > 0 {
			if raw.Len() <= lastCompleteEnd || raw.Len() == lastHeldNoteBytes || heldNoteEvery <= 0 || time.Since(lastHeldNoteAt) < heldNoteEvery {
				return true
			}
			lastHeldNoteAt = time.Now()
			lastHeldNoteBytes = raw.Len()
			held := raw.Len() - rawAtLastUsefulTx
			msg := fmt.Sprintf("appel d'outil reçu, Gemini continue d'écrire : %d Ko", koCount(held))
			if ri != nil {
				ri.setStatusNote(msg)
			}
			if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
				res.forwarded = true
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, msg+"\n")))
			}
			return true
		}
		if hooks == nil || !hooks.progressive || hooks.gate == nil || !s.cfg.StreamThoughts {
			return true
		}
		held := hooks.gate.Held()
		if held < 512 || held == lastHeld || time.Since(lastWrite) < hintEvery {
			return true
		}
		lastHeld = held
		note := fmt.Sprintf("Préparation d'un appel d'outil… %d Ko reçus (%s)\n", (held+512)/1024, time.Since(start).Round(time.Second))
		res.forwarded = true
		return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, note)))
	}
	// If thoughts have arrived but no answer text follows for SilenceWarningSec,
	// warn the user that Gemini may be executing an internal tool.
	checkSilence := func() bool {
		if silenceWarned {
			return true
		}
		if thoughtBytes > 0 && raw.Len() == 0 {
			age := time.Since(lastFrameTime)
			if age >= silenceThreshold {
				silenceWarned = true
				sec := int(age.Round(time.Second).Seconds())
				note := fmt.Sprintf("\nGemini ne répond plus depuis %d s ; il utilise peut-être un outil interne (recherche web ou génération d'artifact).\n", sec)
				if ri != nil {
					ri.setStatusNote(fmt.Sprintf("Gemini ne répond plus depuis %d s ; il utilise peut-être un outil interne", sec))
				}
				s.rlog(ctx).Warn("upstream_silence_after_thoughts", "silence_sec", sec, "thought_bytes", thoughtBytes)
				if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
					res.forwarded = true
					return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, note)))
				}
			}
		}
		return true
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			cancelled()
			return res
		case <-ticker.C:
			heldBytes := raw.Len() - rawAtLastUsefulTx
			if maxHeldDur > 0 && heldBytes > 0 && !heldSince.IsZero() && time.Since(heldSince) >= maxHeldDur {
				triggerCap("duration", heldBytes)
				break loop
			}
			if !hint() {
				return res
			}
			if !checkSilence() {
				return res
			}
			if time.Since(lastWrite) >= interval && !keepalive() {
				return res
			}
		case e := <-events:
			if !handle(e) {
				if res.aborted {
					return res
				}
				break loop
			}
		case res.genErr = <-done:
			for { // the generation is over: drain what it queued
				select {
				case e := <-events:
					if !handle(e) {
						if res.aborted {
							return res
						}
						break loop
					}
					continue
				default:
				}
				break
			}
			break loop
		}
	}
	if ctx.Err() != nil {
		cancelled()
		return res
	}
	if res.earlyStopReason == "" {
		res.rawText = raw.String()
	}
	res.early = early.String()
	return res
}

// awaitStreamOrGenerate runs runStreamTools when progressive streaming is enabled
// (giving non-chat routes early-stop, runaway caps and in-flight tracking) and
// falls back to awaitGenerate otherwise.
func (s *Server) awaitStreamOrGenerate(ctx context.Context, sse *sseStream, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool) kaResult {
	if s.streamEvents == nil {
		return s.awaitGenerate(ctx, sse, p, interval)
	}
	res := s.runStreamTools(ctx, sse, p, interval, declared, nil)
	if res.aborted {
		if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
			return kaResult{out: kaShutdown}
		}
		return kaResult{out: kaGone}
	}
	if res.earlyStopMsg != "" {
		return kaResult{text: res.earlyStopMsg}
	}
	if res.earlyStopReason != "" {
		return kaResult{text: res.rawText}
	}
	if res.genErr != nil || res.rawText == "" {
		if res.genErr == nil || (gemini.Retryable(res.genErr) && !errors.Is(res.genErr, gemini.ErrSessionAuth)) {
			return s.awaitGenerate(ctx, sse, p, interval)
		}
		return kaResult{err: res.genErr}
	}
	return kaResult{text: res.rawText}
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

// glitchRetryNote is appended to the prompt on the single retry after a canned
// upstream non-answer so Google does not receive an identical second request.
const glitchRetryNote = "\n\n[Retry instruction: Do not use Google Drive, Gmail, Workspace, or any built-in Google extension or widget. All workspace files are local on the user's machine and can only be accessed using the listed tools. Reply now either with a ```tool_call block or plain text.]"

func glitchRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += glitchRetryNote
	return p
}

const toolRefusalRetryNote = "\n\n[Retry instruction: The listed tools are NOT built-in model features. You invoke them by outputting a ```tool_call block in your reply; the user's system executes it locally and returns the result. Never reply that tools are not declared or that you lack local access: output a ```tool_call block now or reply in plain text.]"

func toolRefusalRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += toolRefusalRetryNote
	return p
}

func (s *Server) warnCooldown() time.Duration {
	if s.cfg.WarnSuspectCooldownMin < 0 {
		return 0
	}
	if s.cfg.WarnSuspectCooldownMin == 0 {
		return 10 * time.Minute
	}
	return time.Duration(s.cfg.WarnSuspectCooldownMin) * time.Minute
}

func (s *Server) silenceWarningSec() int {
	if s.cfg.SilenceWarningSec > 0 {
		return s.cfg.SilenceWarningSec
	}
	return 45
}

func (s *Server) maxHeldBytes() int {
	if s.cfg.MaxHeldBytes < 0 {
		return 0
	}
	if s.cfg.MaxHeldBytes == 0 {
		return 256 * 1024
	}
	return s.cfg.MaxHeldBytes
}

func (s *Server) maxHeldDuration() time.Duration {
	if s.heldDurationOverride > 0 {
		return s.heldDurationOverride
	}
	if s.cfg.MaxHeldSec < 0 {
		return 0
	}
	if s.cfg.MaxHeldSec == 0 {
		return 120 * time.Second
	}
	return time.Duration(s.cfg.MaxHeldSec) * time.Second
}

func (s *Server) heldNoteInterval() time.Duration {
	if s.heldNoteOverride > 0 {
		return s.heldNoteOverride
	}
	return 30 * time.Second
}

func (s *Server) statusNoteInterval() time.Duration {
	if s.statusNoteOverride > 0 {
		return s.statusNoteOverride
	}
	return 5 * time.Second
}

func koCount(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	k := (bytes + 512) / 1024
	if k < 1 {
		return 1
	}
	return k
}

func (s *Server) noteEarlyStop(ctx context.Context, reason string, toolCalls, rawBytes, heldBytes, sentBytes int, elapsed time.Duration) {
	s.stats.earlyStop.Add(1)
	switch reason {
	case "marker":
		s.stats.earlyStopMarker.Add(1)
	case "size":
		s.stats.earlyStopSize.Add(1)
	case "duration":
		s.stats.earlyStopDuration.Add(1)
	}
	s.rlog(ctx).Warn("early_stop",
		"reason", reason,
		"tool_calls", toolCalls,
		"raw_bytes", rawBytes,
		"held_bytes", heldBytes,
		"sent_bytes", sentBytes,
		"elapsed_ms", elapsed.Milliseconds(),
	)
}

// maybeSuspectWarningFor checks and emits a warning for a given verdict (in-flight
// or finalized). Warnings are rate-limited per model (default 10 min) unless
// the verdict changes.
func (s *Server) maybeSuspectWarningFor(ctx context.Context, model, verdict, servedID string) string {
	if !s.cfg.WarnSuspectFallback || !s.cfg.StreamThoughts {
		return ""
	}
	var note string
	switch {
	case verdict == "name_only":
		note = "\n⚠ GMINI : Google annonce le nom du modèle demandé (" + model + ") sous un identifiant différent"
		if servedID != "" {
			note += " (" + servedID + ")"
		}
		note += " : modèle réel non garanti ; actualiser le cookie via le raccourci 7 et vérifier GMINI Control.\n"
	case verdict == "other_model" || strings.HasPrefix(verdict, "suspect:"):
		note = "\n⚠ GMINI : cette réponse ne vient probablement pas du modèle demandé (" + model +
			"). Google sert un autre modèle quand la limite d'utilisation du site Gemini est atteinte. Vérifiez la page GMINI Control avant de vous fier au résultat.\n"
	default:
		s.suspectWarnMu.Lock()
		if verdict == "confirmed" && s.suspectWarn != nil {
			delete(s.suspectWarn, model)
		}
		s.suspectWarnMu.Unlock()
		return ""
	}

	nowFn := time.Now
	if s.warnNow != nil {
		nowFn = s.warnNow
	}
	now := nowFn()
	cooldown := s.warnCooldown()

	s.suspectWarnMu.Lock()
	defer s.suspectWarnMu.Unlock()
	if s.suspectWarn == nil {
		s.suspectWarn = map[string]suspectWarnState{}
	}
	if prev, ok := s.suspectWarn[model]; ok && prev.verdict == verdict && cooldown > 0 && now.Sub(prev.at) < cooldown {
		return ""
	}
	s.suspectWarn[model] = suspectWarnState{verdict: verdict, at: now}
	return note
}

// maybeSuspectWarning returns a reasoning warning note when the current call's
// served model identity is doubtful (name_only, other_model or no_thoughts).
// Warnings are rate-limited per model (default 10 min) unless the verdict changes.
func (s *Server) maybeSuspectWarning(ctx context.Context, model string) string {
	ri := reqFrom(ctx)
	if ri == nil {
		return ""
	}
	rec, found := s.client.CallByRequest(ri.id)
	if !found {
		return ""
	}
	verdict := rec.Identity
	if verdict == "other_model" && s.autoRefresh != nil {
		s.autoRefresh.TriggerEarly()
	}
	if verdict == "not_stated" && rec.Suspect != "" {
		verdict = "suspect:" + rec.Suspect
	}
	return s.maybeSuspectWarningFor(ctx, model, verdict, rec.ServedID)
}

func (s *Server) inFlightRequests() []InFlightRequest {
	return s.active.inFlight()
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
	if s.autoRefresh != nil {
		s.autoRefresh.PrepareRequest(r.Context(), func() {
			if req.Stream {
				if sse, ok := openSSE(w); ok {
					_ = sse.comment("GMINI : actualisation de la session…")
				}
			}
		})
	}

	// Stream + tools: same keepalive behaviour as chat completions.
	if interval := s.keepaliveInterval(); req.Stream && toolsActive && interval > 0 {
		ctx := r.Context()
		sse, ok := openSSE(w)
		if !ok {
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}
		res := s.awaitStreamOrGenerate(ctx, sse, p, interval, req.DeclaredTools())
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.event("error", responsesStreamError("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.event("error", responsesStreamError("upstream error: "+res.err.Error())))
		default:
			text, toolCalls := s.finishText(ctx, res.text, true, req.DeclaredTools())
			s.writeResponsesStream(ctx, sse, rid, mid, m.Name, prompt, text, toolCalls)
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())

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
	if s.autoRefresh != nil {
		s.autoRefresh.PrepareRequest(r.Context(), func() {
			if stream {
				if sse, ok := openSSE(w); ok {
					_ = sse.comment("GMINI : actualisation de la session…")
				}
			}
		})
	}

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
		res := s.awaitStreamOrGenerate(ctx, sse, p, interval, req.DeclaredTools())
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+res.err.Error())))
		default:
			s.sseTail(ctx, sse, sse.data(s.googleResponse(ctx, m.Name, prompt, res.text, hasTools, req.DeclaredTools())))
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	response := s.googleResponse(r.Context(), m.Name, prompt, text, hasTools, req.DeclaredTools())

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
	if s.autoRefresh != nil {
		s.autoRefresh.PrepareRequest(r.Context(), func() {
			if req.Stream {
				if sse, ok := openSSE(w); ok {
					_ = sse.comment("GMINI : actualisation de la session…")
				}
			}
		})
	}

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
		res := s.awaitStreamOrGenerate(ctx, sse, p, interval, req.DeclaredTools())
		switch {
		case res.out == kaGone:
		case res.out == kaShutdown:
			s.sseTail(ctx, sse, sse.event("error", apiconv.AnthropicError("server shutting down")))
		case res.err != nil:
			s.sseTail(ctx, sse, sse.event("error", apiconv.AnthropicError("upstream error: "+res.err.Error())))
		default:
			text, toolCalls := s.finishText(ctx, res.text, true, req.DeclaredTools())
			s.streamMessagesFull(ctx, sse, msgID, m.Name, prompt, text, toolCalls)
		}
		return
	}

	text, err := s.client.Generate(r.Context(), p)
	if err != nil {
		sendAnthropicError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())

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
