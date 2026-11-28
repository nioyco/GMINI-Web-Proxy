package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/apiconv"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
	"github.com/nioyco/GMINI-Web-Proxy/pkg/util"
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
	// heldDurationOverride, heldStallOverride, and heldNoteOverride override the
	// K9 120 s held cap, 60 s stall cap, and 30 s progress note period in tests.
	// Never set in production.
	heldDurationOverride           time.Duration
	heldStallOverride              time.Duration
	heldStallFastOverride          time.Duration
	stallRetryBackoffOverride      time.Duration
	heldNoteOverride               time.Duration
	statusNoteOverride             time.Duration
	cannedRetryTimeoutOverride     time.Duration
	noContentTimeoutOverride       time.Duration
	firstAttemptNoContentOverride  time.Duration
	nonFinalStallNoContentOverride time.Duration
	finalNoContentRetryOverride    time.Duration
	noContentNoteOverride          time.Duration
	initialWaitNoteOverride        time.Duration
	initialSilenceTimeoutOverride  time.Duration
	codeRefWaitOverride            time.Duration
	// uiToken protects the API of the local management page (ui.go); empty: page off.
	uiToken string

	started    time.Time
	active     activeSet
	flightHist inFlightHistory
	stats      statsCounters
	ignored    ignoredStats

	suspectWarnMu sync.Mutex
	suspectWarn   map[string]suspectWarnState
	warnNow       func() time.Time

	failMemMu sync.Mutex
	failMem   map[string][]time.Time

	staleWarnMu sync.Mutex
	staleWarnAt time.Time

	codeRefMu    sync.Mutex
	codeRefStats []codeRefStat

	cfgMu sync.RWMutex

	autoRefresh *cookieAutoRefresher
	agPatch     agPatchManager

	// diag is the anonymized diagnostics ring buffer (K37/K38). It stores only
	// metadata (never secrets, cookies, keys, or prompt/response content) and is
	// persisted to cfg.DiagFile so past events survive a restart.
	diag *diagRing
	// diagExportBusy serializes /_ui/api/diagnostics/export runs.
	diagExportBusy atomic.Bool

	// quotaPause is the K41 global premium-model pause (see quotapause.go).
	quotaPause quotaPauseTracker

	// toolsDB is the K46 anonymized tool-call & incident database (see tools_db.go).
	toolsDB *toolsDB
}

func (s *Server) cfgSnapshot() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return *s.cfg
}

func (s *Server) cannedErrorRetryMode() string {
	s.cfgMu.RLock()
	v := s.cfg.CannedErrorRetry
	s.cfgMu.RUnlock()
	return normalizeCannedErrorRetry(v)
}

func (s *Server) noContentSecSetting() int {
	s.cfgMu.RLock()
	v := s.cfg.NoContentSec
	s.cfgMu.RUnlock()
	return nonNegative(v, 0)
}

type codeRefStat struct {
	at      time.Time
	model   string
	pauseMS int64 // -1 when no tool_call followed
}

func (s *Server) codeRefWaitThreshold() time.Duration {
	if s.codeRefWaitOverride > 0 {
		return s.codeRefWaitOverride
	}
	return 3 * time.Second
}

func (s *Server) recordCodeRefStat(model string, pauseMS int64) {
	if model == "" {
		return
	}
	now := s.currentNow()
	cutoff := now.Add(-5 * time.Hour)
	s.codeRefMu.Lock()
	defer s.codeRefMu.Unlock()
	kept := s.codeRefStats[:0]
	for _, st := range s.codeRefStats {
		if !st.at.Before(cutoff) {
			kept = append(kept, st)
		}
	}
	s.codeRefStats = append(kept, codeRefStat{at: now, model: model, pauseMS: pauseMS})
}

func (s *Server) codeRefStats5h() map[string][]int64 {
	now := s.currentNow()
	cutoff := now.Add(-5 * time.Hour)
	s.codeRefMu.Lock()
	defer s.codeRefMu.Unlock()
	out := map[string][]int64{}
	for _, st := range s.codeRefStats {
		if !st.at.Before(cutoff) {
			out[st.model] = append(out[st.model], st.pauseMS)
		}
	}
	return out
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
	if client != nil {
		client.SetBinaryTag(currentBinaryTag())
	}
	apiconv.SetToolPromptRules(!cfg.DisableToolPromptRules)
	apiconv.SetToolPromptReminder(cfg.ToolPromptReminder)
	apiconv.SetNoInternalCodeExecRule(cfg.NoInternalCodeExecRule)
	apiconv.SetMarkerNeutralization(!cfg.DisableMarkerNeutralization)
	apiconv.SetDropTextAfterToolCall(!cfg.KeepTextAfterToolCall)
	srv := &Server{
		cfg:        cfg,
		client:     client,
		log:        logger,
		generate:   client.Generate,
		started:    time.Now(),
		flightHist: inFlightHistory{path: cfg.FlightsFile},
		diag:       newDiagRing(diagRingCapacity, cfg.DiagFile),
		toolsDB:    newToolsDB(defaultToolsDBPath(), toolsDBMaxEntries),
	}
	srv.flightHist.load()
	srv.diag.load()
	srv.diag.seed(client.RecentCalls())
	srv.autoRefresh = newCookieAutoRefresher(cfg, client, logger)
	if srv.autoRefresh != nil {
		srv.autoRefresh.onSuccess = func() {
			srv.resumeStaleTokenQuotaPause(context.Background(), "cookie_refresh_stale_token")
			if len(client.ListModels()) <= 1 {
				go func() {
					rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer rcancel()
					if err := client.RefreshModels(rctx); err != nil {
						logger.Warn("catalog auto-recovery after cookie refresh failed", "err", err)
					} else {
						logger.Info("catalog auto-recovered after cookie refresh", "models", len(client.ListModels()))
					}
				}()
			}
		}
	}
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
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.Handle("GET /_status", s.withAuth(s.handleStatus))
	mux.Handle("POST /_models/refresh", s.withAuth(s.handleModelsRefresh))
	mux.Handle("POST /_models/resume", s.withAuth(s.handleModelsResume))
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1beta/models", s.handleModelsGoogle)
	mux.Handle("POST /v1/chat/completions", s.withAuth(s.handleChat))
	mux.Handle("POST /v1/responses", s.withAuth(s.handleResponses))
	mux.Handle("POST /v1/messages", s.withAuth(s.handleMessages))
	mux.Handle("POST /v1/messages/count_tokens", s.withAuth(s.handleCountTokens))
	mux.Handle("POST /v1beta/models/{model}", s.withAuth(s.handleGoogleGenerate))
	mux.HandleFunc("GET /_ui", s.handleUI)
	mux.HandleFunc("GET /_ui/api/state", s.handleUIState)
	mux.HandleFunc("GET /_ui/api/diagnostics", s.handleUIDiagnostics)
	mux.HandleFunc("POST /_ui/api/diagnostics/export", s.handleUIDiagnosticsExport)
	mux.HandleFunc("GET /_ui/api/tools-db", s.handleUIToolsDB)
	mux.HandleFunc("GET /_ui/api/tools-db/export", s.handleUIToolsDBExport)
	mux.HandleFunc("POST /_ui/api/models/refresh", s.handleUIRefresh)
	mux.HandleFunc("POST /_ui/api/quota/resume", s.handleUIQuotaResume)
	mux.HandleFunc("POST /_ui/api/cookie-refresh/toggle", s.handleUICookieRefreshToggle)
	mux.HandleFunc("POST /_ui/api/cookie-refresh/run", s.handleUICookieRefreshRun)
	mux.HandleFunc("POST /_ui/api/cookies/refresh", s.handleUICookieRefreshRun)
	mux.HandleFunc("POST /_ui/api/temporary-chat/toggle", s.handleUITemporaryChatToggle)
	mux.HandleFunc("POST /_ui/api/guardrails/set", s.handleUIGuardrailsSet)
	mux.HandleFunc("POST /_ui/api/config/reload", s.handleUIConfigReload)
	mux.HandleFunc("POST /_ui/api/antigravity/launch", s.handleUILaunch)
	mux.HandleFunc("POST /_ui/api/antigravity/sync-models", s.handleUIAntigravitySync)
	mux.HandleFunc("POST /_ui/api/antigravity/patch", s.handleUIAntigravityPatch)
	mux.HandleFunc("GET /_ui/api/antigravity/patch/status", s.handleUIAntigravityPatchStatus)
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
	s.client.RecordUserActivity()
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
		s.recordUnknownModel(r.Context(), modelName, err)
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.quotaPreflight(w, r, m, flavorOpenAI) {
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
	setReqConvID(r.Context(), convFingerprintFromPrompt(prompt))
	setReqInput(r.Context(), len(prompt), utf8.RuneCountInString(prompt), len(images))
	setReqToolFacts(r.Context(), apiconv.ExtractConversationToolFacts(req.Messages))
	s.client.RecordUserActivity()
	cid := apiconv.ID("chatcmpl-", 12)
	p := m.Params(prompt)
	p.Images = images
	if s.autoRefresh != nil {
		s.autoRefresh.PrepareRequest(r.Context(), func() {
			if req.Stream {
				if sse, ok := openSSE(w); ok {
					if s.cfg.StreamThoughts {
						_ = sse.data(apiconv.ChatChunkReasoning(cid, time.Now().Unix(), m.Name, "["+s.currentNow().Format("15:04:05")+"] (pas le modèle — attente technique) GMINI : actualisation de la session…\n"))
					} else {
						_ = sse.comment("GMINI : actualisation de la session…")
					}
				}
			}
		})
	}
	s.client.EnsureAuthOnRequest(r.Context())
	s.prepareStaleTokenAlert(r.Context())
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
	if s.quotaAfterGenerate(r.Context(), p) {
		s.sendQuotaPause429(w, flavorOpenAI)
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())
	if text != "" || len(toolCalls) > 0 {
		s.recordModelSuccess(m.Name)
	}

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
			if ri := reqFrom(ctx); ri != nil {
				if note := ri.takeStaleStreamNote(); note != "" && s.cfg.StreamThoughts {
					_ = sse.data(apiconv.ChatChunkReasoning(cid, created, model, "\n"+note+"\n"))
				}
			}
		}
		acc.WriteString(text)
		if err := sse.data(apiconv.ChatChunk(cid, created, model, text)); err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel() // client is gone: stop pulling tokens
		}
	}
	emit, finish := s.echoEmitter(ctx, emitRaw, &acc)

	var hasCandidate, hasThought bool
	guard := s.newQuotaGuard(ctx, p, func() bool { return sse != nil }, cancel)
	err := s.client.GenerateStreamEvents(gctx, p, guard.wrap(gemini.StreamHandlers{
		Text:      emit,
		Thought:   func(string) { hasThought = true },
		Candidate: func() { hasCandidate = true },
	}))
	finish()
	if sse == nil && guard.isTripped() {
		s.sendQuotaPause429(w, flavorOpenAI)
		return
	}
	if sse == nil {
		// Nothing was streamed yet, so a normal HTTP error status is still possible.
		// If Google closed the stream with no candidate, text, or thought, return
		// an explicit explanation in text without retrying (K16 B2).
		switch {
		case noFlush:
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		case !hasCandidate && !hasThought:
			var ok bool
			if sse, ok = openSSE(w); !ok {
				sendError(w, http.StatusInternalServerError, "streaming unsupported")
				return
			}
			if ri := reqFrom(ctx); ri != nil {
				if note := ri.takeStaleStreamNote(); note != "" && s.cfg.StreamThoughts {
					_ = sse.data(apiconv.ChatChunkReasoning(cid, created, model, "\n"+note+"\n"))
				}
			}
			msg := s.recordEmptyReply(ctx, model, p)
			s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
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
	if acc.Len() > 0 {
		s.recordModelSuccess(model)
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
	if ri := reqFrom(ctx); ri != nil {
		if note := ri.takeStaleStreamNote(); note != "" && s.cfg.StreamThoughts {
			if err := sse.data(apiconv.ChatChunkReasoning(cid, time.Now().Unix(), model, "\n"+note+"\n")); err != nil {
				return
			}
		}
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
	if m == nil || m.ModelID == "" {
		return
	}
	// Auto (8c46e95b1a07cecc) exposes 0 thinking levels in Google's otAQ7b menu
	// (observed in 20261119T010707: f.81 omitted). Never emit inner[80] for Auto
	// even when GEMINI_THINKING_LEVEL, reasoning_effort, or @level is supplied.
	if gemini.IsAutoModel(m.ModelID, m.Name, m.DisplayName) {
		return
	}
	effort := suffix
	if effort == "" {
		effort = requested
	}
	if apiconv.EffortLevel(effort) == 0 {
		effort = s.cfg.DefaultThinkingLevel
	}
	level := apiconv.EffortLevel(effort)
	if level == 0 {
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
type attemptGuards struct {
	isCannedErr        bool
	isGlitch           bool
	isToolRefusal      bool
	isFakeClaim        bool
	fakeClaimKind      string
	isPostStallHandoff bool
	isDowngrade        bool
	isSilentSkip       bool
}

func zeroExtractedCalls(raw string, declared []apiconv.DeclaredTool) bool {
	if len(declared) == 0 {
		return false
	}
	_, calls, _ := apiconv.ParseToolCallsWith(strings.TrimSpace(raw), declared)
	return len(calls) == 0
}

func isWeakerServedModel(servedID, servedName string) bool {
	if servedID == gemini.AutoModelHexID {
		return true
	}
	low := strings.ToLower(strings.TrimSpace(servedName))
	if low == "" {
		return false
	}
	return strings.Contains(low, "flash") || strings.Contains(low, "lite") || strings.Contains(low, "fast") || strings.Contains(low, "quick")
}

func isRequestedHigherModel(m *gemini.AvailableModel) bool {
	if m == nil || m.ModelID == "" {
		return false
	}
	if m.ModelID == gemini.AutoModelHexID || gemini.IsAutoModel(m.ModelID, m.Name, m.DisplayName) {
		return false
	}
	desc := strings.ToLower(m.Name + " " + m.DisplayName + " " + m.Description)
	return !strings.Contains(desc, "flash") && !strings.Contains(desc, "lite") && !strings.Contains(desc, "fast") && !strings.Contains(desc, "quick")
}

func (s *Server) isSilentModelDowngrade(ctx context.Context, p gemini.GenParams, out streamToolsResult) bool {
	if !isRequestedHigherModel(p.Model) {
		return false
	}
	servedID, servedName, verdict := out.servedID, out.servedName, out.servedVerdict
	if verdict == "" || servedID == "" {
		if ri := reqFrom(ctx); ri != nil {
			if rec, found := s.client.CallByRequest(ri.id); found {
				if servedID == "" {
					servedID = rec.ServedID
				}
				if servedName == "" {
					servedName = rec.ServedName
				}
				if verdict == "" {
					verdict = rec.Identity
				}
			}
		}
	}
	if verdict != "other_model" && verdict != "name_only" {
		return false
	}
	if isWeakerServedModel(servedID, servedName) {
		return true
	}
	if servedID != "" && s.client != nil {
		for _, m := range s.client.ListModels() {
			if m.ModelID == servedID && isWeakerServedModel(m.ModelID, m.Name+" "+m.DisplayName+" "+m.Description) {
				return true
			}
		}
	}
	return false
}

func (s *Server) evaluateAttemptGuards(ctx context.Context, p gemini.GenParams, out streamToolsResult, declared []apiconv.DeclaredTool, stallRetriesUsed int, postStallHandoffRetried, modelDowngradeRetried bool) attemptGuards {
	var g attemptGuards
	if out.released || out.earlyStopReason != "" {
		return g
	}
	g.isCannedErr = apiconv.IsUpstreamCannedErrorWithMarker(out.rawText, out.errorMarker)
	if g.isCannedErr {
		return g
	}
	g.isGlitch = apiconv.IsUpstreamGlitch(out.rawText)
	if len(declared) == 0 {
		return g
	}
	g.isToolRefusal = apiconv.IsUpstreamToolRefusal(out.rawText) || apiconv.IsThoughtToolRefusalOrSimulation(out.thoughtText, out.rawText)
	toolFacts := reqToolFacts(ctx)
	if !g.isToolRefusal {
		g.fakeClaimKind, g.isFakeClaim = apiconv.DetectUnverifiedActionClaim(out.rawText, declared, toolFacts.SinceLastUser)
	}
	if g.isGlitch || g.isToolRefusal || g.isFakeClaim {
		return g
	}
	wantHandoff := stallRetriesUsed > 0 && !postStallHandoffRetried && apiconv.IsPostStallHandoffSummary(out.rawText)
	wantDowngrade := !modelDowngradeRetried && s.isSilentModelDowngrade(ctx, p, out)
	if (wantHandoff || wantDowngrade) && zeroExtractedCalls(out.rawText, declared) {
		if wantHandoff {
			g.isPostStallHandoff = true
		} else {
			g.isDowngrade = true
		}
	}
	if !g.isGlitch && !g.isToolRefusal && !g.isFakeClaim && !g.isPostStallHandoff && !g.isDowngrade &&
		s.cfg.EnforceToolCalls && zeroExtractedCalls(out.rawText, declared) && !apiconv.IsCompaction(out.rawText) {
		g.isSilentSkip = true
	}
	return g
}

func (s *Server) streamChatToolsProgress(ctx context.Context, w http.ResponseWriter, cid, model string, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool) {
	sse, ok := openSSE(w)
	if !ok {
		sendError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	maxRetries := s.maxStallRetries()
	curParams := p
	created := time.Now().Unix()
	stallRetriesUsed := 0
	postStallHandoffRetried := false
	modelDowngradeRetried := false
	internalToolRetried := false
	toolUnparsedRetried := false

	runAttempt := func(rp gemini.GenParams, isRetry, allowFast bool, cannedTimeout, maxNoContent time.Duration) (streamToolsResult, apiconv.ProgressGate, apiconv.EchoFilter, bool) {
		var g apiconv.ProgressGate
		var e apiconv.EchoFilter
		var initSilence time.Duration
		if !isRetry && rp.Mode == 1 {
			initSilence = s.initialSilenceTimeout()
		}
		out := s.runStreamTools(ctx, sse, rp, interval, declared, &streamToolsHooks{
			progressive:           true,
			noInternalRetry:       internalToolRetried,
			isRetry:               isRetry,
			cannedRetryTimeout:    cannedTimeout,
			maxNoContentTimeout:   maxNoContent,
			initialSilenceTimeout: initSilence,
			cid:                   cid,
			created:               created,
			model:                 model,
			gate:                  &g,
			echo:                  &e,
		}, allowFast)
		return out, g, e, !out.aborted
	}

	firstCap := time.Duration(0)
	if maxRetries > 0 {
		firstCap = s.firstAttemptNoContentCap()
	}
	res, gate, echo, ok := runAttempt(curParams, false, maxRetries > 0, 0, firstCap)
	if !ok {
		return
	}
	if res.earlyStopReason == "initial_silence" && res.transmittedBytes == 0 {
		if !s.waitRetryBackoff(ctx) {
			return
		}
		retryParams := curParams
		stepdown := ""
		if retryParams.Mode == 1 && retryParams.Level == 4 {
			retryParams.Level = 2
			stepdown = "4->2"
		}
		retryParams.Prompt += initialSilenceAntiInternalNote
		timeoutSec := int(s.initialSilenceTimeout().Round(time.Second).Seconds())
		attrs := []any{
			"timeout_sec", timeoutSec,
			"heartbeats", res.heartbeats,
			"temp_chat", retryParams.TemporaryChat,
			"mode", retryParams.Mode,
		}
		if stepdown != "" {
			attrs = append(attrs, "level_stepdown", stepdown)
		}
		s.rlog(ctx).Warn("initial_silence_retry", attrs...)
		s.recordDiagIncident(ctx, "initial_silence_retry", "retry", "aucune réponse Google après délai", true)
		if s.cfg.StreamThoughts {
			note := fmt.Sprintf("Google n'a envoyé aucune réponse après %d s : nouvelle tentative automatique…", timeoutSec)
			if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), note))); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return
			}
		}
		curParams = retryParams
		maxNoContent := time.Duration(0)
		if maxRetries > 0 {
			maxNoContent = s.stallRetryNoContentCap(1, maxRetries)
		}
		res, gate, echo, ok = runAttempt(curParams, true, maxRetries > 0, 0, maxNoContent)
		if !ok {
			return
		}
	}
	allNoContent := res.noContentTimedOut
	for attempt := 1; (res.earlyStopReason == "stall" || res.noContentTimedOut) && res.transmittedBytes == 0 && attempt <= maxRetries; attempt++ {
		allNoContent = allNoContent && res.noContentTimedOut
		nextParams, ok := s.prepareStallRetry(ctx, sse, cid, created, model, true, curParams, res, attempt, maxRetries)
		if !ok {
			return
		}
		stallRetriesUsed++
		if res.noContentTimedOut {
			s.recordDiagIncident(ctx, "no_content_timeout", "retry", "aucun contenu (tentative non-finale)", true)
		} else {
			s.recordDiagIncident(ctx, "stall", "retry", "aucun premier texte (stall)", true)
		}
		curParams = nextParams
		noContentCap := s.stallRetryNoContentCap(attempt, maxRetries)
		if attempt == maxRetries && allNoContent {
			noContentCap = s.finalNoContentCap()
		}
		res, gate, echo, ok = runAttempt(curParams, true, attempt < maxRetries, 0, noContentCap)
		if !ok {
			return
		}
	}
	if res.earlyStopMsg != "" {
		s.markEarlyStopTerminalError(ctx, res)
		s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, res.earlyStopMsg), nil)), sse.done())
		return
	}
	if res.noContentTimedOut {
		s.finishNoContentTimeout(ctx, sse, cid, model, curParams, res)
		return
	}
	finishCommonNonStream := func(out streamToolsResult, rp gemini.GenParams) bool {
		if out.earlyStopMsg != "" {
			s.markEarlyStopTerminalError(ctx, out)
			s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, out.earlyStopMsg), nil)), sse.done())
			return true
		}
		if out.noContentTimedOut {
			s.finishNoContentTimeout(ctx, sse, cid, model, rp, out)
			return true
		}
		if out.earlyStopReason == "" && !out.internalToolRetry && (out.genErr != nil || out.rawText == "") {
			genErr := out.genErr
			if genErr == nil {
				genErr = gemini.ErrEmptyResponse
			}
			s.recordDiagIncident(ctx, "error", "upstream_error", "erreur amont", false)
			s.markTerminalError(ctx, "Erreur amont", "upstream_error")
			s.setErrorContext(ctx, "upstream/google", "finishCommonNonStream", "upstream_error", genErr.Error())
			s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+genErr.Error())), sse.done())
			return true
		}
		return false
	}

	finishTerminalNonAnswer := func(out streamToolsResult, rp gemini.GenParams, afterLabel string) bool {
		if out.earlyStopReason != "" || out.released {
			return false
		}
		if apiconv.IsUpstreamCannedErrorWithMarker(out.rawText, out.errorMarker) {
			s.recordModelFailure(model)
			s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(out.rawText), "after", afterLabel, "action", "reported")
			s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini (réponse type)", false)
			s.markTerminalError(ctx, "Erreur interne", "canned_error")
			s.setErrorContext(ctx, "guard/upstream", "finishTerminalNonAnswer", "upstream_canned_error", "Erreur interne Google répétée après tentative de récupération")
			s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, cannedErrorMessage(model, rp)), nil)), sse.done())
			return true
		}
		if len(declared) > 0 && (apiconv.IsUpstreamToolRefusal(out.rawText) || apiconv.IsThoughtToolRefusalOrSimulation(out.thoughtText, out.rawText)) {
			s.rlog(ctx).Warn("upstream_tool_refusal", "bytes", len(out.rawText), "action", "reported")
			s.recordDiagIncident(ctx, "tool_refusal", "reported", "refus/simulation d'outils sans appel local", false)
			s.markTerminalError(ctx, "Refus outils", "tool_refusal")
			s.setErrorContext(ctx, "guard/tools", "finishTerminalNonAnswer", "tool_refusal", "Refus ou simulation d'outils locaux sans appel tool_call")
			msg := "GMINI : Gemini a refusé ou simulé deux fois sans appeler les outils locaux. Relancez la demande ou ouvrez une nouvelle conversation."
			s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, msg), nil)), sse.done())
			return true
		}
		if apiconv.IsUpstreamGlitch(out.rawText) {
			s.rlog(ctx).Warn("upstream_glitch", "bytes", len(out.rawText), "action", "reported")
			s.recordDiagIncident(ctx, "glitch", "reported", "message de service / extension Google", false)
			s.markTerminalError(ctx, "Glitch Google", "glitch")
			s.setErrorContext(ctx, "guard/glitch", "finishTerminalNonAnswer", "upstream_glitch", "Message de service / extension Google au lieu de la réponse")
			s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, glitchReportMessage(out.rawText)), nil)), sse.done())
			return true
		}
		return false
	}

	// K53: handle internalToolRetry inside any recovery branch so an intentional
	// internal-tool cut (rawText == "") never turns into ErrEmptyResponse.
	resolveInternalToolRetry := func(out streamToolsResult, g apiconv.ProgressGate, e apiconv.EchoFilter, rp gemini.GenParams) (streamToolsResult, apiconv.ProgressGate, apiconv.EchoFilter, gemini.GenParams, bool) {
		if !out.internalToolRetry || internalToolRetried {
			return out, g, e, rp, true
		}
		internalToolRetried = true
		if s.cfg.StreamThoughts {
			if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a tenté d'utiliser un outil interne : nouvelle tentative avec rappel renforcé…"))); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return out, g, e, rp, false
			}
		}
		itParams := internalToolRetryParams(rp)
		if itParams.Mode == 1 && itParams.Level == 4 {
			itParams.Level = 2
			s.rlog(ctx).Warn("internal_tool_retry_stepdown", "level_stepdown", "4->2", "temp_chat", itParams.TemporaryChat)
		}
		maxNoContent := time.Duration(0)
		if maxRetries > 0 {
			maxNoContent = s.stallRetryNoContentCap(1, maxRetries)
		}
		itRes, itGate, itEcho, ok := runAttempt(itParams, true, false, 0, maxNoContent)
		if !ok {
			return itRes, itGate, itEcho, itParams, false
		}
		if (itRes.earlyStopReason == "stall" || itRes.noContentTimedOut) && itRes.transmittedBytes == 0 && maxRetries > 0 {
			if !s.waitRetryBackoff(ctx) {
				return itRes, itGate, itEcho, itParams, false
			}
			stallParams := itParams
			stepdown := ""
			if stallParams.Mode == 1 && stallParams.Level >= 2 {
				stepdown = fmt.Sprintf("%d->1", stallParams.Level)
				stallParams.Level = 1
			}
			if itRes.noContentTimedOut {
				attrs := []any{"after", "internal_tool", "attempt", 1, "max_retries", 1, "elapsed_sec", int(itRes.noContentElapsed.Round(time.Second).Seconds()), "temp_chat", stallParams.TemporaryChat}
				if stepdown != "" {
					attrs = append(attrs, "level_stepdown", stepdown)
				}
				s.rlog(ctx).Warn("no_content_timeout", attrs...)
				s.recordDiagIncident(ctx, "no_content_timeout", "retry", "aucun contenu après outil interne ; nouvelle tentative", true)
			} else {
				s.stats.heldStallRetries.Add(1)
				attrs := []any{"after", "internal_tool", "attempt", 1, "max_retries", 1, "held_bytes", itRes.heldBytes, "temp_chat", stallParams.TemporaryChat}
				if stepdown != "" {
					attrs = append(attrs, "level_stepdown", stepdown)
				}
				s.rlog(ctx).Warn("held_stall_retry", attrs...)
				s.recordDiagIncident(ctx, "stall", "retry", "blocage après outil interne ; nouvelle tentative", true)
			}
			if s.cfg.StreamThoughts {
				waitNote := "Gemini s'est figé après l'outil interne : nouvelle tentative…"
				if itRes.noContentTimedOut {
					waitNote = "Aucun contenu reçu après l'outil interne : nouvelle tentative…"
				}
				if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), waitNote))); err != nil {
					s.rlog(ctx).Warn("sse write failed", "err", err)
					return itRes, itGate, itEcho, stallParams, false
				}
			}
			itParams = stallParams
			itRes, itGate, itEcho, ok = runAttempt(itParams, true, false, 0, 0)
			if !ok {
				return itRes, itGate, itEcho, itParams, false
			}
		}
		return itRes, itGate, itEcho, itParams, true
	}

	if res.internalToolRetry {
		internalToolRetried = true
		if s.cfg.StreamThoughts {
			if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a tenté d'utiliser un outil interne : nouvelle tentative avec rappel renforcé…"))); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return
			}
		}
		retryParams := internalToolRetryParams(curParams)
		if retryParams.Mode == 1 && retryParams.Level == 4 {
			retryParams.Level = 2
			s.rlog(ctx).Warn("internal_tool_retry_stepdown", "level_stepdown", "4->2", "temp_chat", retryParams.TemporaryChat)
		}
		if s.streamEvents != nil {
			maxNoContent := time.Duration(0)
			if maxRetries > 0 {
				maxNoContent = s.stallRetryNoContentCap(1, maxRetries)
			}
			rres, gate2, echo2, ok := runAttempt(retryParams, true, false, 0, maxNoContent)
			if !ok {
				return
			}
			// G2/K52: 1 retry allowed if the corrective attempt stalls or hits capped no_content_timeout with 0 transmitted bytes.
			if (rres.earlyStopReason == "stall" || rres.noContentTimedOut) && rres.transmittedBytes == 0 && maxRetries > 0 {
				if !s.waitRetryBackoff(ctx) {
					return
				}
				stallParams := retryParams
				stepdown := ""
				if stallParams.Level >= 2 {
					stepdown = fmt.Sprintf("%d->1", stallParams.Level)
					stallParams.Level = 1
				}
				if rres.noContentTimedOut {
					attrs := []any{"after", "internal_tool", "attempt", 1, "max_retries", 1, "elapsed_sec", int(rres.noContentElapsed.Round(time.Second).Seconds()), "temp_chat", stallParams.TemporaryChat}
					if stepdown != "" {
						attrs = append(attrs, "level_stepdown", stepdown)
					}
					s.rlog(ctx).Warn("no_content_timeout", attrs...)
					s.recordDiagIncident(ctx, "no_content_timeout", "retry", "aucun contenu après outil interne ; nouvelle tentative", true)
				} else {
					s.stats.heldStallRetries.Add(1)
					attrs := []any{"after", "internal_tool", "attempt", 1, "max_retries", 1, "held_bytes", rres.heldBytes, "temp_chat", stallParams.TemporaryChat}
					if stepdown != "" {
						attrs = append(attrs, "level_stepdown", stepdown)
					}
					s.rlog(ctx).Warn("held_stall_retry", attrs...)
					s.recordDiagIncident(ctx, "stall", "retry", "blocage après outil interne ; nouvelle tentative", true)
				}
				if s.cfg.StreamThoughts {
					waitNote := "Gemini s'est figé après l'outil interne : nouvelle tentative…"
					if rres.noContentTimedOut {
						waitNote = "Aucun contenu reçu après l'outil interne : nouvelle tentative…"
					}
					if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), waitNote))); err != nil {
						s.rlog(ctx).Warn("sse write failed", "err", err)
						return
					}
				}
				retryParams = stallParams
				rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
				if !ok {
					return
				}
			}
			// G2: Ultimate retry with level->1 stepdown if internal_tool_retry returns a short upstream glitch.
			if rres.earlyStopReason == "" && !rres.released && apiconv.IsUpstreamGlitch(rres.rawText) && retryParams.Level >= 2 {
				if !s.waitRetryBackoff(ctx) {
					return
				}
				glitchParams := glitchRetryParams(retryParams)
				stepdown := fmt.Sprintf("%d->1", glitchParams.Level)
				glitchParams.Level = 1
				s.rlog(ctx).Warn("upstream_glitch", "bytes", len(rres.rawText), "after", "internal_tool", "action", "retry_once", "level_stepdown", stepdown, "temp_chat", glitchParams.TemporaryChat)
				s.recordDiagIncident(ctx, "glitch", "retry", "message de service après outil interne ; nouvelle tentative", true)
				if s.cfg.StreamThoughts {
					if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a renvoyé un message de service après outil interne : nouvelle tentative…"))); err != nil {
						s.rlog(ctx).Warn("sse write failed", "err", err)
						return
					}
				}
				retryParams = glitchParams
				rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
				if !ok {
					return
				}
			}
			// K54: 1 retry with 2->1 stepdown if internal_tool_retry returns a canned error.
			if rres.earlyStopReason == "" && !rres.released && apiconv.IsUpstreamCannedErrorWithMarker(rres.rawText, rres.errorMarker) && retryParams.Level >= 2 {
				if !s.waitRetryBackoff(ctx) {
					return
				}
				cannedParams, _ := prepareRecoveryParams(retryParams)
				cannedParams.Level = 1
				retryTimeout := s.cannedErrorRetryTimeoutFor(len(cannedParams.Prompt))
				s.stats.cannedErrorRetries.Add(1)
				s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "after", "internal_tool", "action", "retry_once", "level_stepdown", "2->1", "timeout_sec", int(retryTimeout.Seconds()), "temp_chat", cannedParams.TemporaryChat)
				s.recordDiagIncident(ctx, "canned_error", "retry", "erreur interne après outil interne ; nouvelle tentative", true)
				if s.cfg.StreamThoughts {
					if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a renvoyé une erreur interne après l'outil interne : nouvelle tentative…"))); err != nil {
						s.rlog(ctx).Warn("sse write failed", "err", err)
						return
					}
				}
				retryParams = cannedParams
				rres, gate2, echo2, ok = runAttempt(retryParams, true, false, retryTimeout, 0)
				if !ok {
					return
				}
			}
			// K54: 1 retry with 2->1 stepdown if internal_tool_retry returns a silent empty reply.
			if rres.earlyStopReason == "" && !rres.noContentTimedOut && !rres.internalToolRetry && rres.genErr == nil && rres.rawText == "" && retryParams.Mode == 1 && retryParams.Level >= 2 {
				if !s.waitRetryBackoff(ctx) {
					return
				}
				emptyParams := retryParams
				stepdown := fmt.Sprintf("%d->1", emptyParams.Level)
				emptyParams.Level = 1
				s.rlog(ctx).Warn("empty_reply_retry", "after", "internal_tool", "level_stepdown", stepdown, "temp_chat", emptyParams.TemporaryChat)
				s.recordDiagIncident(ctx, "empty_reply", "retry", "réponse vide silencieuse après outil interne ; nouvelle tentative", true)
				if s.cfg.StreamThoughts {
					if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Réponse vide silencieuse après l'outil interne : nouvelle tentative (niveau réduit)…"))); err != nil {
						s.rlog(ctx).Warn("sse write failed", "err", err)
						return
					}
				}
				retryParams = emptyParams
				rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
				if !ok {
					return
				}
			}
			if finishCommonNonStream(rres, retryParams) {
				return
			}
			// K54: Do not terminate on tool_refusal right after internal_tool_retry;
			// hand off to the unified guards so toolRefusalRetryParams (1->2) can recover.
			isRefusalAfterInternal := rres.earlyStopReason == "" && !rres.released && len(declared) > 0 &&
				(apiconv.IsUpstreamToolRefusal(rres.rawText) || apiconv.IsThoughtToolRefusalOrSimulation(rres.thoughtText, rres.rawText))
			if !isRefusalAfterInternal && finishTerminalNonAnswer(rres, retryParams, "internal_tool") {
				return
			}
			// K52: hand off to the unified post-attempt guards (unverified claim,
			// tool_unparsed, silent_skip, tool_refusal) and final finishStreamProgress.
			res, gate, echo, curParams = rres, gate2, echo2, retryParams
		} else {
			bres := s.awaitGenerate(ctx, sse, retryParams, interval)
			switch {
			case bres.out == kaGone:
			case bres.out == kaShutdown:
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			case bres.err != nil:
				s.markTerminalError(ctx, "Erreur amont", "upstream_error")
				s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+bres.err.Error())), sse.done())
			default:
				if !res.warnEmitted {
					if note := s.maybeSuspectWarning(ctx, model); note != "" {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, note)); err != nil {
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
	if res.emptyReply {
		msg := s.recordEmptyReply(ctx, model, p)
		s.markTerminalError(ctx, "Réponse vide", "empty_reply")
		s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
		return
	}
	if res.earlyStopReason == "" && (res.genErr != nil || res.rawText == "") {
		if !res.modelForwarded && (res.genErr == nil || gemini.Retryable(res.genErr)) && !errors.Is(res.genErr, gemini.ErrSessionAuth) {
			// Nothing from the model reached the client yet (only GMINI wait notes): let the buffered path retry.
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
					if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, note)); err != nil {
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
		s.recordDiagIncident(ctx, "error", "upstream_error", "erreur amont", false)
		s.markTerminalError(ctx, "Erreur amont", "upstream_error")
		s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+genErr.Error())), sse.done())
		return
	}

	prepareGuardRetry := func(base gemini.GenParams, g attemptGuards, bytes int, afterLabel string) (gemini.GenParams, bool) {
		if !s.waitRetryBackoff(ctx) {
			return base, false
		}
		var retryParams gemini.GenParams
		var steppedDown bool
		var waitMsg string
		switch {
		case g.isPostStallHandoff:
			postStallHandoffRetried = true
			retryParams = postStallHandoffRetryParams(base)
			steppedDown = base.Mode == 1 && base.Level == 4 && retryParams.Level == 2
			attrs := []any{"bytes", bytes, "action", "retry_once", "temp_chat", retryParams.TemporaryChat, "mode", retryParams.Mode}
			if afterLabel != "" {
				attrs = append(attrs, "after", afterLabel)
			}
			if steppedDown {
				attrs = append(attrs, "level_stepdown", "4->2")
			}
			s.rlog(ctx).Warn("post_stall_handoff_retry", attrs...)
			s.recordDiagIncident(ctx, "retry", "post_stall_handoff", "bilan d'étapes au lieu d'un appel d'outil", true)
			waitMsg = "Gemini a renvoyé un bilan d'étapes au lieu d'un appel d'outil : nouvelle tentative directe…"
		case g.isDowngrade:
			modelDowngradeRetried = true
			retryParams = base
			retryParams.TemporaryChat = true
			attrs := []any{"model", model, "action", "retry_once", "temp_chat", true}
			if afterLabel != "" {
				attrs = append(attrs, "after", afterLabel)
			}
			s.rlog(ctx).Warn("served_model_doubt_retry", attrs...)
			s.recordDiagIncident(ctx, "served_model_doubt", "retry", "modèle réduit servi sans appel d'outil", true)
			waitMsg = "Google a servi un modèle réduit sans appel d'outil : nouvelle tentative avec le modèle demandé…"
		case g.isSilentSkip:
			retryParams, steppedDown = prepareRecoveryParams(base)
			retryParams = silentSkipRetryParams(retryParams)
			waitMsg = "Gemini a répondu en texte sans appeler d'outil : nouvelle tentative avec consigne corrective…"
			attrs := []any{"bytes", bytes, "silent_skip", true, "action", "retry_once", "temp_chat", retryParams.TemporaryChat}
			if afterLabel != "" {
				attrs = append(attrs, "after", afterLabel)
			}
			if steppedDown {
				attrs = append(attrs, "level_stepdown", "4->2")
			}
			s.rlog(ctx).Warn("silent_skip", attrs...)
			s.recordDiagIncident(ctx, "silent_skip", "retry", "texte seul malgré outils déclarés (aucun appel)", true)
		default:
			retryParams, steppedDown = prepareRecoveryParams(base)
			stepdownLabel := ""
			if steppedDown {
				stepdownLabel = "4->2"
			}
			switch {
			case g.isFakeClaim:
				retryParams = unverifiedActionRetryParams(retryParams)
				waitMsg = "Gemini affirme avoir agi sur le disque sans appeler d'outil : nouvelle tentative avec consigne corrective…"
			case g.isToolRefusal:
				prevLevel := retryParams.Level
				retryParams = toolRefusalRetryParams(retryParams)
				if retryParams.Mode == 1 && prevLevel == 1 && retryParams.Level == 2 {
					stepdownLabel = "1->2"
				}
				waitMsg = "Gemini affirme ne pas avoir d'outils locaux : nouvelle tentative avec consigne corrective…"
			default:
				retryParams = glitchRetryParams(retryParams)
				waitMsg = "Gemini a renvoyé un message de service au lieu de traiter la demande : nouvelle tentative…"
			}
			attrs := []any{"bytes", bytes, "tool_refusal", g.isToolRefusal, "unverified_claim", g.fakeClaimKind, "action", "retry_once", "temp_chat", retryParams.TemporaryChat}
			if afterLabel != "" {
				attrs = append(attrs, "after", afterLabel)
			}
			if stepdownLabel != "" {
				attrs = append(attrs, "level_stepdown", stepdownLabel)
			}
			s.rlog(ctx).Warn("upstream_glitch", attrs...)
			switch {
			case g.isToolRefusal:
				s.recordDiagIncident(ctx, "tool_refusal", "retry", "refus d'outils ; nouvelle tentative corrective", true)
			case g.isFakeClaim:
				s.recordDiagIncident(ctx, "tool_refusal", "unverified_claim_retry", "affirmation d'action sans appel d'outil", true)
			default:
				s.recordDiagIncident(ctx, "glitch", "retry", "message de service ; nouvelle tentative", true)
			}
		}
		if s.cfg.StreamThoughts {
			if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), waitMsg))); err != nil {
				s.rlog(ctx).Warn("sse write failed", "err", err)
				return base, false
			}
		}
		return retryParams, true
	}

	if res.earlyStopReason == "" {
		g := s.evaluateAttemptGuards(ctx, curParams, res, declared, stallRetriesUsed, postStallHandoffRetried, modelDowngradeRetried)
		if g.isCannedErr {
			if s.cannedErrorRetryMode() != "once" {
				s.recordModelFailure(model)
				s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(res.rawText), "action", "off")
				s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini (retry désactivé)", false)
				s.markTerminalError(ctx, "Erreur interne", "canned_error")
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, curParams), nil)), sse.done())
				return
			}
			if s.shouldSkipCannedRetry(model) {
				s.recordModelFailure(model)
				s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(res.rawText), "action", "skipped_recent_failures")
				s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini (retry ignoré)", false)
				s.markTerminalError(ctx, "Erreur interne", "canned_error")
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, curParams), nil)), sse.done())
				return
			}
			if !s.waitRetryBackoff(ctx) {
				return
			}
			cannedParams, steppedDown := prepareRecoveryParams(curParams)
			s.stats.cannedErrorRetries.Add(1)
			retryTimeout := s.cannedErrorRetryTimeoutFor(len(cannedParams.Prompt))
			attrs := []any{"bytes", len(res.rawText), "action", "retry_once", "timeout_sec", int(retryTimeout.Seconds()), "temp_chat", cannedParams.TemporaryChat}
			if steppedDown {
				attrs = append(attrs, "level_stepdown", "4->2")
			}
			s.rlog(ctx).Warn("upstream_canned_error", attrs...)
			s.recordDiagIncident(ctx, "canned_error", "retry_once", "erreur interne ; nouvelle tentative à l'identique", true)
			if s.cfg.StreamThoughts {
				if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a renvoyé une erreur interne : nouvelle tentative à l'identique…"))); err != nil {
					s.rlog(ctx).Warn("sse write failed", "err", err)
					return
				}
			}
			if s.streamEvents != nil {
				rres, gate2, echo2, ok := runAttempt(cannedParams, true, false, retryTimeout, 0)
				if !ok {
					return
				}
				rres, gate2, echo2, cannedParams, ok = resolveInternalToolRetry(rres, gate2, echo2, cannedParams)
				if !ok {
					return
				}
				if rres.earlyStopMsg != "" {
					s.markEarlyStopTerminalError(ctx, rres)
					s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, rres.earlyStopMsg, nil)), sse.done())
					return
				}
				if !rres.cannedRetryTimedOut && !rres.noContentTimedOut && rres.earlyStopReason == "" && !rres.released &&
					apiconv.IsUpstreamCannedErrorWithMarker(rres.rawText, rres.errorMarker) && cannedParams.Level >= 2 {
					if !s.waitRetryBackoff(ctx) {
						return
					}
					stepdown := fmt.Sprintf("%d->1", cannedParams.Level)
					cannedParams.Level = 1
					retryTimeout = s.cannedErrorRetryTimeoutFor(len(cannedParams.Prompt))
					s.stats.cannedErrorRetries.Add(1)
					s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "action", "retry_2to1", "level_stepdown", stepdown, "timeout_sec", int(retryTimeout.Seconds()), "temp_chat", cannedParams.TemporaryChat)
					s.recordDiagIncident(ctx, "canned_error", "retry_2to1", "erreur interne persistante ; nouvelle tentative niveau 1", true)
					if s.cfg.StreamThoughts {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a renvoyé une seconde erreur interne : ultime tentative (niveau 1)…"))); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
					rres, gate2, echo2, ok = runAttempt(cannedParams, true, false, retryTimeout, 0)
					if !ok {
						return
					}
					rres, gate2, echo2, cannedParams, ok = resolveInternalToolRetry(rres, gate2, echo2, cannedParams)
					if !ok {
						return
					}
					if rres.earlyStopMsg != "" {
						s.markEarlyStopTerminalError(ctx, rres)
						s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, rres.earlyStopMsg), nil)), sse.done())
						return
					}
				}
				if rres.cannedRetryTimedOut || rres.noContentTimedOut || (rres.earlyStopReason == "" && (rres.genErr != nil || rres.rawText == "" || (!rres.released && apiconv.IsUpstreamCannedErrorWithMarker(rres.rawText, rres.errorMarker)))) {
					s.recordModelFailure(model)
					s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "timed_out", rres.cannedRetryTimedOut || rres.noContentTimedOut, "action", "reported")
					s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini après nouvelle tentative", false)
					if rres.cannedRetryTimedOut || rres.noContentTimedOut {
						s.markTerminalError(ctx, "Timeout", "canned_retry_timeout")
					} else {
						s.markTerminalError(ctx, "Erreur interne", "canned_error")
					}
					s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, appendResumeHint(ctx, cannedErrorMessage(model, cannedParams)), nil)), sse.done())
					return
				}
				if rres.earlyStopReason == "" && !rres.released {
					g2 := s.evaluateAttemptGuards(ctx, cannedParams, rres, declared, stallRetriesUsed, postStallHandoffRetried, modelDowngradeRetried)
					if g2.isGlitch || g2.isToolRefusal || g2.isFakeClaim || g2.isPostStallHandoff || g2.isDowngrade || g2.isSilentSkip {
						rp2, ok := prepareGuardRetry(cannedParams, g2, len(rres.rawText), "canned_error")
						if !ok {
							return
						}
						rres, gate2, echo2, ok = runAttempt(rp2, true, false, 0, 0)
						if !ok {
							return
						}
						rres, gate2, echo2, rp2, ok = resolveInternalToolRetry(rres, gate2, echo2, rp2)
						if !ok {
							return
						}
						if finishCommonNonStream(rres, rp2) || finishTerminalNonAnswer(rres, rp2, "canned_error") {
							return
						}
					}
				}
				s.finishStreamProgress(ctx, sse, cid, model, declared, rres, &gate2, &echo2)
				return
			}
			retryCtx, retryCancel := context.WithTimeout(ctx, retryTimeout)
			bres := s.awaitGenerate(retryCtx, sse, cannedParams, interval)
			retryCancel()
			switch {
			case bres.out == kaGone:
				if ctx.Err() == nil && errors.Is(retryCtx.Err(), context.DeadlineExceeded) {
					s.recordModelFailure(model)
					s.rlog(ctx).Warn("upstream_canned_error", "timed_out", true, "action", "reported")
					s.markTerminalError(ctx, "Timeout", "canned_retry_timeout")
					s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, cannedParams), nil)), sse.done())
				}
			case bres.out == kaShutdown:
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			case bres.err != nil:
				if ctx.Err() == nil && errors.Is(bres.err, context.DeadlineExceeded) {
					s.recordDiagIncident(ctx, "timeout", "canned_retry", "nouvelle tentative erreur interne expirée", false)
					s.recordModelFailure(model)
					s.rlog(ctx).Warn("upstream_canned_error", "timed_out", true, "action", "reported")
					s.markTerminalError(ctx, "Timeout", "canned_retry_timeout")
					s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, cannedParams), nil)), sse.done())
					return
				}
				s.markTerminalError(ctx, "Erreur amont", "upstream_error")
				s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+bres.err.Error())), sse.done())
			case s.isCannedRetryText(ctx, bres.text):
				s.recordModelFailure(model)
				s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(bres.text), "action", "reported")
				s.markTerminalError(ctx, "Erreur interne", "canned_error")
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, cannedParams), nil)), sse.done())
			default:
				if !res.warnEmitted {
					if note := s.maybeSuspectWarning(ctx, model); note != "" {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, note)); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
				}
				text, toolCalls := s.finishText(ctx, bres.text, true, declared)
				if text != "" || len(toolCalls) > 0 {
					s.recordModelSuccess(model)
				}
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, text, toolCalls)), sse.done())
			}
			return
		}
		if g.isGlitch || g.isToolRefusal || g.isFakeClaim || g.isPostStallHandoff || g.isDowngrade || g.isSilentSkip {
			retryParams, ok := prepareGuardRetry(curParams, g, len(res.rawText), "")
			if !ok {
				return
			}
			// Run the K19/K20/K33/K34 retry via runStreamTools when available so thought
			// simulations, stalls, chained canned errors, post-glitch refusals, handoffs,
			// and silent downgrades are caught.
			if s.streamEvents != nil {
				rres, gate2, echo2, ok := runAttempt(retryParams, true, maxRetries > 0, 0, 0)
				if !ok {
					return
				}
				rres, gate2, echo2, retryParams, ok = resolveInternalToolRetry(rres, gate2, echo2, retryParams)
				if !ok {
					return
				}
				if rres.earlyStopReason == "stall" && rres.transmittedBytes == 0 && maxRetries > 0 {
					nextParams, ok := s.prepareStallRetry(ctx, sse, cid, created, model, true, retryParams, rres, 1, 1)
					if !ok {
						return
					}
					stallRetriesUsed++
					retryParams = nextParams
					rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
					if !ok {
						return
					}
					rres, gate2, echo2, retryParams, ok = resolveInternalToolRetry(rres, gate2, echo2, retryParams)
					if !ok {
						return
					}
				}
				// K53: Silent empty reply (rawText == "" && genErr == nil) on level >= 2 after tool_refusal/glitch -> 1 retry with 2->1 stepdown.
				if rres.earlyStopReason == "" && !rres.noContentTimedOut && !rres.internalToolRetry && rres.genErr == nil && rres.rawText == "" && retryParams.Mode == 1 && retryParams.Level >= 2 {
					if !s.waitRetryBackoff(ctx) {
						return
					}
					afterLabel := "glitch"
					if g.isToolRefusal {
						afterLabel = "tool_refusal"
					}
					emptyParams := retryParams
					stepdown := fmt.Sprintf("%d->1", emptyParams.Level)
					emptyParams.Level = 1
					s.rlog(ctx).Warn("empty_reply_retry", "after", afterLabel, "level_stepdown", stepdown, "temp_chat", emptyParams.TemporaryChat)
					s.recordDiagIncident(ctx, "empty_reply", "retry", "réponse vide silencieuse après "+afterLabel+" ; nouvelle tentative", true)
					if s.cfg.StreamThoughts {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Réponse vide silencieuse de Google : nouvelle tentative (niveau réduit)…"))); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
					retryParams = emptyParams
					rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
					if !ok {
						return
					}
					rres, gate2, echo2, retryParams, ok = resolveInternalToolRetry(rres, gate2, echo2, retryParams)
					if !ok {
						return
					}
				}
				if finishCommonNonStream(rres, retryParams) {
					return
				}
				if rres.earlyStopReason == "" && !rres.released && apiconv.IsUpstreamCannedErrorWithMarker(rres.rawText, rres.errorMarker) {
					afterLabel := "tool_refusal"
					if g.isGlitch && !g.isToolRefusal {
						afterLabel = "glitch"
					}
					if s.cannedErrorRetryMode() != "once" || s.shouldSkipCannedRetry(model) {
						s.recordModelFailure(model)
						s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "after", afterLabel, "action", "reported")
						s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini après "+afterLabel, false)
						s.markTerminalError(ctx, "Erreur interne", "canned_error")
						s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, retryParams), nil)), sse.done())
						return
					}
					if !s.waitRetryBackoff(ctx) {
						return
					}
					cannedParams, _ := prepareRecoveryParams(retryParams)
					s.stats.cannedErrorRetries.Add(1)
					retryTimeout := s.cannedErrorRetryTimeoutFor(len(cannedParams.Prompt))
					s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "after", afterLabel, "action", "retry_once", "timeout_sec", int(retryTimeout.Seconds()), "temp_chat", cannedParams.TemporaryChat)
					s.recordDiagIncident(ctx, "canned_error", "retry_once", "erreur interne après "+afterLabel+" ; nouvelle tentative", true)
					if s.cfg.StreamThoughts {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini a renvoyé une erreur interne : nouvelle tentative…"))); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
					rres, gate2, echo2, ok = runAttempt(cannedParams, true, false, retryTimeout, 0)
					if !ok {
						return
					}
					rres, gate2, echo2, cannedParams, ok = resolveInternalToolRetry(rres, gate2, echo2, cannedParams)
					if !ok {
						return
					}
					if rres.earlyStopMsg != "" {
						s.markEarlyStopTerminalError(ctx, rres)
						s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, rres.earlyStopMsg, nil)), sse.done())
						return
					}
					if rres.cannedRetryTimedOut || rres.noContentTimedOut || (rres.earlyStopReason == "" && (rres.genErr != nil || rres.rawText == "" || (!rres.released && apiconv.IsUpstreamCannedErrorWithMarker(rres.rawText, rres.errorMarker)))) {
						s.recordModelFailure(model)
						s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(rres.rawText), "timed_out", rres.cannedRetryTimedOut || rres.noContentTimedOut, "after", afterLabel, "action", "reported")
						s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini après "+afterLabel, false)
						if rres.cannedRetryTimedOut || rres.noContentTimedOut {
							s.markTerminalError(ctx, "Timeout", "canned_retry_timeout")
						} else {
							s.markTerminalError(ctx, "Erreur interne", "canned_error")
						}
						s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, cannedParams), nil)), sse.done())
						return
					}
				}
				if g.isGlitch && !g.isToolRefusal && rres.earlyStopReason == "" && !rres.released && len(declared) > 0 &&
					(apiconv.IsUpstreamToolRefusal(rres.rawText) || apiconv.IsThoughtToolRefusalOrSimulation(rres.thoughtText, rres.rawText)) {
					if !s.waitRetryBackoff(ctx) {
						return
					}
					refusalParams, steppedDown2 := prepareRecoveryParams(retryParams)
					prevLevel2 := refusalParams.Level
					refusalParams = toolRefusalRetryParams(refusalParams)
					rattrs := []any{"bytes", len(rres.rawText), "after", "glitch", "action", "retry_once", "temp_chat", refusalParams.TemporaryChat}
					if steppedDown2 {
						rattrs = append(rattrs, "level_stepdown", "4->2")
					} else if refusalParams.Mode == 1 && prevLevel2 == 1 && refusalParams.Level == 2 {
						rattrs = append(rattrs, "level_stepdown", "1->2")
					}
					s.rlog(ctx).Warn("upstream_tool_refusal", rattrs...)
					s.recordDiagIncident(ctx, "tool_refusal", "retry", "refus d'outils après glitch ; nouvelle tentative corrective", true)
					if s.cfg.StreamThoughts {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Gemini affirme ne pas avoir d'outils locaux : nouvelle tentative avec consigne corrective…"))); err != nil {
							s.rlog(ctx).Warn("sse write failed", "err", err)
							return
						}
					}
					retryParams = refusalParams
					rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
					if !ok {
						return
					}
					rres, gate2, echo2, retryParams, ok = resolveInternalToolRetry(rres, gate2, echo2, retryParams)
					if !ok {
						return
					}
					if finishCommonNonStream(rres, retryParams) || finishTerminalNonAnswer(rres, retryParams, "glitch_tool_refusal") {
						return
					}
				}
				// K34: Catch a post-stall handoff or silent Flash-Lite downgrade on retry output too (max 1 each).
				if rres.earlyStopReason == "" && !rres.released {
					gNext := s.evaluateAttemptGuards(ctx, retryParams, rres, declared, stallRetriesUsed, postStallHandoffRetried, modelDowngradeRetried)
					if gNext.isPostStallHandoff || gNext.isDowngrade {
						rpNext, ok := prepareGuardRetry(retryParams, gNext, len(rres.rawText), "retry")
						if !ok {
							return
						}
						retryParams = rpNext
						rres, gate2, echo2, ok = runAttempt(retryParams, true, false, 0, 0)
						if !ok {
							return
						}
						if finishCommonNonStream(rres, retryParams) {
							return
						}
					}
				}
				if finishTerminalNonAnswer(rres, retryParams, "retry") {
					return
				}
				s.finishStreamProgress(ctx, sse, cid, model, declared, rres, &gate2, &echo2)
				return
			}
			bres := s.awaitGenerate(ctx, sse, retryParams, interval)
			switch {
			case bres.out == kaGone:
			case bres.out == kaShutdown:
				s.sseTail(ctx, sse, sse.data(errorObj("server shutting down")), sse.done())
			case bres.err != nil:
				s.markTerminalError(ctx, "Erreur amont", "upstream_error")
				s.sseTail(ctx, sse, sse.data(errorObj("upstream error: "+bres.err.Error())), sse.done())
			case s.isCannedRetryText(ctx, bres.text):
				s.recordModelFailure(model)
				s.rlog(ctx).Warn("upstream_canned_error", "bytes", len(bres.text), "after", "tool_refusal", "action", "reported")
				s.recordDiagIncident(ctx, "canned_error", "reported", "erreur interne Gemini après tool_refusal", false)
				s.markTerminalError(ctx, "Erreur interne", "canned_error")
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, cannedErrorMessage(model, retryParams), nil)), sse.done())
			case len(declared) > 0 && apiconv.IsUpstreamToolRefusal(bres.text):
				s.rlog(ctx).Warn("upstream_tool_refusal", "bytes", len(bres.text), "action", "reported")
				s.recordDiagIncident(ctx, "tool_refusal", "reported", "refus/simulation d'outils sans appel local", false)
				s.markTerminalError(ctx, "Refus outils", "tool_refusal")
				msg := "GMINI : Gemini a refusé ou simulé sans appeler les outils locaux. Relancez la demande ou ouvrez une nouvelle conversation."
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
			case apiconv.IsUpstreamGlitch(bres.text):
				s.rlog(ctx).Warn("upstream_glitch", "bytes", len(bres.text), "action", "reported")
				s.recordDiagIncident(ctx, "glitch", "reported", "message de service / extension Google", false)
				s.markTerminalError(ctx, "Glitch Google", "glitch")
				s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, glitchReportMessage(bres.text), nil)), sse.done())
			default:
				if !res.warnEmitted {
					if note := s.maybeSuspectWarning(ctx, model); note != "" {
						if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, note)); err != nil {
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

	if res.earlyStopReason == "" && !res.released && res.transmittedBytes == 0 && len(declared) > 0 && !toolUnparsedRetried {
		_, parsedCalls, parseRep := apiconv.ParseToolCallsWith(strings.TrimSpace(res.rawText), declared)
		if len(parsedCalls) == 0 && len(parseRep.Unparsed) > 0 {
			toolUnparsedRetried = true
			reason := parseRep.Unparsed[0].Reason
			s.rlog(ctx).Warn("tool_unparsed_retry", "unparsed_count", len(parseRep.Unparsed), "reason", reason, "bytes", len(res.rawText))
			s.recordDiagIncident(ctx, "tool_unparsed", "retry", "bloc tool_call JSON invalide ("+reason+") ; nouvelle tentative", true)
			if s.cfg.StreamThoughts {
				_ = sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), "Bloc tool_call JSON invalide : nouvelle tentative avec format strict…")))
			}
			retryParams := toolUnparsedRetryParams(curParams)
			var ok bool
			res, gate, echo, ok = runAttempt(retryParams, true, false, 0, 0)
			if !ok {
				return
			}
			res, gate, echo, retryParams, ok = resolveInternalToolRetry(res, gate, echo, retryParams)
			if !ok {
				return
			}
			if finishCommonNonStream(res, retryParams) || finishTerminalNonAnswer(res, retryParams, "tool_unparsed") {
				return
			}
		}
	}

	s.finishStreamProgress(ctx, sse, cid, model, declared, res, &gate, &echo)
}

func (s *Server) finishStreamProgress(ctx context.Context, sse *sseStream, cid, model string, declared []apiconv.DeclaredTool, res streamToolsResult, gate *apiconv.ProgressGate, echo *apiconv.EchoFilter) {
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
	text, toolCalls, parseRep := s.finishTextWithReport(ctx, strings.TrimSpace(res.rawText), true, declared)
	if len(toolCalls) > 0 && len(parseRep.Unparsed) > 0 && s.cfg.StreamThoughts {
		_ = sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, proxyWaitNoteWithTitle(s.currentNow(), "Lot tool_call partiel", fmt.Sprintf("%d bloc(s) tool_call malformé(s) ignoré(s) ; %d appel(s) valide(s) transmis.", len(parseRep.Unparsed), len(toolCalls)))))
	}
	if len(toolCalls) > 0 && s.cfg.SplitReadWriteBatch {
		if kept, deferred := apiconv.SplitPrematureReadWriteBatch(toolCalls); deferred > 0 {
			toolCalls = kept
			s.stats.readWriteBatchSplit.Add(1)
			s.rlog(ctx).Warn("read_write_batch_split", "deferred_writes", deferred, "kept_calls", len(kept))
			if s.cfg.StreamThoughts {
				_ = sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, proxyWaitNoteWithTitle(s.currentNow(), "Écriture prématurée retenue", "GMINI a retenu write_file car des lectures (read_file) sont demandées dans le même tour : le modèle rédigera après réception des fichiers lus.")))
			}
		}
	}
	if ri != nil {
		ri.setEmittedTools(apiconv.SummarizeToolCalls(toolCalls))
	}
	if text != "" || len(toolCalls) > 0 {
		s.recordModelSuccess(model)
	}
	rest, ok := gate.Rest(text)
	if !ok {
		s.rlog(ctx).Warn("progress_text_mismatch", "sent_bytes", len(gate.Sent()), "final_bytes", len(text))
	}
	if len(toolCalls) == 0 && len(declared) > 0 {
		if res.released && (apiconv.IsUpstreamToolRefusal(res.rawText) || apiconv.IsThoughtToolRefusalOrSimulation(res.thoughtText, res.rawText)) {
			s.rlog(ctx).Warn("upstream_tool_refusal_released", "bytes", len(res.rawText), "sent_bytes", res.transmittedBytes)
			s.recordDiagIncident(ctx, "tool_refusal_released", "released", "refus relayé à l'aval", true)
			s.markTerminalError(ctx, "Refus outils", "tool_refusal_released")
		}
		toolFacts := reqToolFacts(ctx)
		if kind, suspect := apiconv.DetectUnverifiedActionClaim(text, declared, toolFacts.SinceLastUser); suspect {
			s.stats.unverifiedClaim.Add(1)
			if ri != nil {
				ri.setUnverifiedClaim(kind)
			}
			s.rlog(ctx).Warn("unverified_action_claim", "kind", kind, "bytes", len(text), "released", res.released)
			rest += unverifiedActionWarning(kind)
		}
		if s.cfg.ToolAuditNote && s.cfg.StreamThoughts {
			summary := "Depuis votre dernière demande : " + toolFacts.SinceLastUser.FormatSummary() + "."
			_ = sse.data(apiconv.ChatChunkReasoning(cid, res.created, model, proxyWaitNoteWithTitle(s.currentNow(), "Bilan vérifié des outils", summary)))
		}
	}
	ri.setSent(sentBytes + len(rest))
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, rest, toolCalls)), sse.done())
}

// finishNoContentTimeout records a model failure, logs the no-content timeout
// with heartbeat count, and terminates the progressive SSE stream cleanly.
func (s *Server) finishNoContentTimeout(ctx context.Context, sse *sseStream, cid, model string, p gemini.GenParams, res streamToolsResult) {
	s.recordModelFailure(model)
	sec := int(res.noContentElapsed.Round(time.Second).Seconds())
	if nc := s.noContentSecSetting(); sec < 1 && nc > 0 {
		sec = nc
	}
	s.rlog(ctx).Warn("upstream_no_content_timeout", "elapsed_sec", sec, "heartbeats", res.heartbeats)
	s.recordDiagIncident(ctx, "no_content_timeout", fmt.Sprintf("elapsed_sec=%d", sec), fmt.Sprintf("aucun contenu après %d s", sec), false)
	s.markTerminalError(ctx, "Timeout", "no_content_timeout")
	s.setErrorContext(ctx, "stream/watchdog", "finishNoContentTimeout", "no_content_timeout", fmt.Sprintf("Aucun contenu reçu de Google après %d s (%d battements)", sec, res.heartbeats))
	msg := appendResumeHint(ctx, noContentTimeoutMessage(model, p, sec))
	s.sseTail(ctx, sse, sse.data(apiconv.ChatChunkComplete(cid, model, msg, nil)), sse.done())
}

type streamToolsHooks struct {
	progressive           bool
	noInternalRetry       bool
	isRetry               bool
	cannedRetryTimeout    time.Duration
	maxNoContentTimeout   time.Duration
	initialSilenceTimeout time.Duration
	cid                   string
	created               int64
	model                 string
	gate                  *apiconv.ProgressGate
	echo                  *apiconv.EchoFilter
}

type streamToolsResult struct {
	rawText             string
	thoughtText         string
	earlyStopReason     string // "" | "marker" | "size" | "duration" | "stall" | "initial_silence"
	earlyStopMsg        string
	fastStall           bool
	internalToolRetry   bool
	internalToolName    string
	cannedRetryTimedOut bool
	noContentTimedOut   bool
	noContentElapsed    time.Duration
	heartbeats          int
	emptyReply          bool
	genErr              error
	forwarded           bool
	modelForwarded      bool // true only once real model thought or text was sent (excludes GMINI wait notes)
	released            bool
	early               string
	warnEmitted         bool
	errorMarker         bool
	aborted             bool // SSE write failed or client cancelled
	created             int64
	transmittedBytes    int
	heldBytes           int
	servedID            string
	servedName          string
	servedVerdict       string
}

// runStreamTools drives s.streamEvents for all stream+tools routes with keepalives,
// in-flight tracking, K8 silence detection, and K9 early-stop / runaway caps.
func (s *Server) runStreamTools(ctx context.Context, sse *sseStream, p gemini.GenParams, interval time.Duration, declared []apiconv.DeclaredTool, hooks *streamToolsHooks, allowFastStall bool) streamToolsResult {
	var res streamToolsResult
	if hooks != nil {
		res.created = hooks.created
	}
	genCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type event struct {
		opened       bool
		heartbeat    bool
		candidate    bool
		text         string
		thought      string
		status       string
		internalTool string
		body24Tools  []string
		limitBanner  string
		quotaWarning string
		servedID     string
		servedName   string
		errorMarker  bool
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
	if ri != nil && p.Level > 0 {
		ri.setThinkingLevel(p.Level)
	}
	h := gemini.StreamHandlers{
		Opened:       func() { push(event{opened: true}) },
		Heartbeat:    func() { push(event{heartbeat: true}) },
		Candidate:    func() { push(event{candidate: true}) },
		Text:         func(d string) { push(event{text: d}) },
		Status:       func(st string) { push(event{status: st}) },
		InternalTool: func(name string) { push(event{internalTool: name}) },
		Body24Tools:  func(names []string) { push(event{body24Tools: names}) },
		LimitBanner:  func(msg string) { push(event{limitBanner: msg}) },
		QuotaWarning: func(msg string) { push(event{quotaWarning: msg}) },
		Identity: func(servedID, servedName string) {
			push(event{servedID: servedID, servedName: servedName})
		},
		ErrorMarker: func() { push(event{errorMarker: true}) },
		Progress: func(textBytes, thoughtBytes int) {
			if ri != nil {
				ri.setProgress(thoughtBytes, textBytes)
			}
		},
	}
	if (hooks != nil && hooks.progressive && s.cfg.StreamThoughts) || len(declared) > 0 {
		h.Thought = func(d string) { push(event{thought: d}) }
	}
	go func() { done <- s.streamEvents(genCtx, p, h) }()

	var raw strings.Builder
	var thoughtBuilder strings.Builder
	filter := func(d string) string {
		if hooks == nil || hooks.echo == nil || s.cfg.DisableEchoFilter {
			return d
		}
		return hooks.echo.Push(d)
	}
	start, lastWrite := time.Now(), time.Now()
	earlyHold := 400
	if len(declared) > 0 {
		earlyHold = 4500
		if hooks != nil && hooks.isRetry {
			earlyHold = 6500
		}
	}
	var early strings.Builder
	thoughtBytes := 0
	lastFrameTime := start
	silenceWarned := false
	silenceThreshold := time.Duration(s.silenceWarningSec()) * time.Second
	maxHeldBytes := s.maxHeldBytes()
	maxHeldDur := s.maxHeldDuration()
	heldStallDur, heldStallFastDur := s.effectiveHeldStallDurations(len(p.Prompt))
	heldNoteEvery := s.heldNoteInterval()
	statusNoteEvery := s.statusNoteInterval()

	rawAtLastUsefulTx := 0
	heldGrowthBytes := 0
	var heldSince, lastHeldGrowthAt, lastHeldNoteAt, lastStatusAt time.Time
	var lastStatusText string
	completeToolCalls, lastCompleteEnd := 0, 0
	lastHeldNoteBytes := 0
	internalToolNotified := false
	quotaWarned := false
	codeRefHandled := false
	var codeRefDetectedAt time.Time
	codeRefWaitNoted := false
	codeRefPauseMS := int64(-1)
	codeRefWaitThresh := s.codeRefWaitThreshold()
	statModelName := ""
	if p.Model != nil && p.Model.Name != "" {
		statModelName = p.Model.Name
	} else if hooks != nil {
		statModelName = hooks.model
	}
	defer func() {
		if !codeRefDetectedAt.IsZero() {
			s.recordCodeRefStat(statModelName, codeRefPauseMS)
		}
	}()
	body24Tools := map[string]bool{}
	var streamOpenedAt, lastNoContentNoteAt time.Time
	hasContent := false
	hasCandidate := false
	initialWaitNoteSent := false
	heartbeats := 0
	noContentTimeout := s.noContentTimeout()
	if hooks != nil && hooks.maxNoContentTimeout > 0 && noContentTimeout > 0 && hooks.maxNoContentTimeout < noContentTimeout {
		noContentTimeout = hooks.maxNoContentTimeout
	}
	if hooks != nil && hooks.cannedRetryTimeout > 0 {
		noContentTimeout = 0
	}
	noContentNoteWait := s.noContentNoteInterval()
	initialWaitNoteWait := s.initialWaitNoteDelay()

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
	emitWaitNote := func(elapsed time.Duration) bool {
		sec := int(elapsed.Round(time.Second).Seconds())
		note := fmt.Sprintf("Google n'a encore envoyé aucune pensée ni réponse (%d s d'attente ; %d battement(s) reçu(s)).", sec, heartbeats)
		if ri != nil {
			ri.setStatusNote(note)
		}
		if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
			res.forwarded = true
			return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), note))))
		}
		return true
	}
	emitInitialWaitNote := func() bool {
		modelName := ""
		if hooks != nil {
			modelName = hooks.model
		}
		title := fmt.Sprintf("Demande envoyée à %s (%d Ko), en attente de Google…", modelDisplayLabel(modelName, p), koCount(len(p.Prompt)))
		if ri != nil {
			ri.setStatusNote(title)
		}
		if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
			res.forwarded = true
			return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNoteWithTitle(s.currentNow(), title, "En attente de la première réponse de Google."))))
		}
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
		if completeToolCalls == 0 && !res.released && res.transmittedBytes == 0 && len(declared) > 0 {
			if rawJSON, toolName, ok := apiconv.BuildCommandToolCallFromCodeRef(raw.String(), declared, body24Tools); ok {
				res.earlyStopReason = "code_ref_fastpath"
				res.rawText = rawJSON
				if !codeRefDetectedAt.IsZero() && codeRefPauseMS < 0 {
					codeRefPauseMS = time.Since(codeRefDetectedAt).Milliseconds()
				}
				s.stats.codeRefFastpath.Add(1)
				s.rlog(ctx).Info("code_ref_fastpath", "tool", toolName, "reason", reason, "elapsed", time.Since(start).Round(time.Millisecond))
				cancel(gemini.ErrEarlyStop)
				return
			}
		}
		res.earlyStopReason = reason
		res.heldBytes = heldBytes
		if reason == "stall" && res.released && res.transmittedBytes > 0 && completeToolCalls == 0 {
			// Part of the answer is already on the client's side, so no silent
			// retry is possible (it would duplicate the text). Emit an explicit
			// diagnostic: the GMINI notice below is the client-facing marker.
			s.recordDiagIncident(ctx, "stall_after_release", "released", "stall après libération du refus", false)
			s.rlog(ctx).Warn("stall_after_release",
				"sent_bytes", res.transmittedBytes,
				"held_bytes", heldBytes,
				"complete_tool_calls", completeToolCalls)
		}
		s.noteEarlyStop(ctx, reason, completeToolCalls, raw.Len(), heldBytes, res.transmittedBytes, time.Since(start))
		if completeToolCalls > 0 && lastCompleteEnd > 0 {
			cancel(gemini.ErrEarlyStop)
			res.rawText = raw.String()[:lastCompleteEnd]
		} else {
			res.rawText = raw.String()
			cancel(gemini.ErrEarlyStopAborted)
			if reason == "stall" {
				res.earlyStopMsg = fmt.Sprintf("GMINI : Gemini s'est figé en cours d'appel d'outil (%s reçus sans suite) ; demande interrompue.", formatByteCount(heldBytes))
			} else {
				res.earlyStopMsg = fmt.Sprintf("GMINI : la réponse de Gemini s'emballe (%d Ko sans fin) ; demande interrompue.", koCount(heldBytes))
			}
		}
	}

	tryCodeRefFastpath := func() bool {
		if !s.cfg.CodeRefFastpath || res.released || res.transmittedBytes != 0 || completeToolCalls != 0 || len(declared) == 0 {
			return false
		}
		rawJSON, toolName, ok := apiconv.BuildCommandToolCallFromCodeRef(raw.String(), declared, body24Tools)
		if !ok {
			return false
		}
		res.earlyStopReason = "code_ref_fastpath"
		res.rawText = rawJSON
		if !codeRefDetectedAt.IsZero() && codeRefPauseMS < 0 {
			codeRefPauseMS = time.Since(codeRefDetectedAt).Milliseconds()
		}
		s.stats.codeRefFastpath.Add(1)
		s.rlog(ctx).Info("code_ref_fastpath", "tool", toolName, "elapsed", time.Since(start).Round(time.Millisecond))
		cancel(gemini.ErrEarlyStop)
		return true
	}

	onInternalTool := func(name string) bool {
		s.markInternalToolDetected(ctx, name)
		action := normalizeInternalToolAction(s.cfg.InternalToolAction)
		switch action {
		case "off":
			return true
		case "retry":
			if len(declared) > 0 && !res.released && res.transmittedBytes == 0 && completeToolCalls == 0 && (hooks == nil || !hooks.noInternalRetry) {
				res.internalToolRetry = true
				res.internalToolName = name
				s.rlog(ctx).Warn("internal_tool_retry", "tool", name)
				cancel(gemini.ErrEarlyStop)
				return false
			}
			// Already transmitted text/tool_call (or no local tools): cannot retry, fall back to notify.
		}
		if !internalToolNotified {
			internalToolNotified = true
			if ri != nil {
				ri.setStatusNote("Gemini lance un outil interne (" + name + ") ; la réponse peut tarder")
			}
			if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
				res.forwarded = true
				if !write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), "Gemini lance un outil interne ; la réponse peut tarder.")))) {
					return false
				}
			}
		}
		return true
	}

	// quotaTrip activates the global quota pause (K41). It returns false when the
	// stream must stop: nothing useful reached the client yet, so the upstream is
	// cancelled (no served_model_doubt retry) and the client gets the pause notice.
	quotaTrip := func(servedID, servedName, reason, banner string) bool {
		if !s.triggerQuotaPause(ctx, p.Model, servedID, servedName, reason, banner) {
			return true
		}
		if res.modelForwarded || res.transmittedBytes > 0 || res.earlyStopMsg != "" {
			return true
		}
		res.earlyStopReason = "quota_pause"
		res.earlyStopMsg = quotaPauseMessage(s.quotaPauseSnapshot())
		s.markTerminalError(ctx, "Pause quota", "quota_pause")
		s.setErrorContext(ctx, "upstream/google", "quotaTrip", "quota_fallback_pause", "Repli modèle ou bannière limite détecté en cours de flux")
		cancel(gemini.ErrEarlyStopAborted)
		return false
	}

	handle := func(e event) bool {
		if e.opened {
			if streamOpenedAt.IsZero() {
				streamOpenedAt = time.Now()
			}
			return true
		}
		if e.candidate {
			hasCandidate = true
			return true
		}
		if e.heartbeat {
			heartbeats++
			res.heartbeats = heartbeats
			if streamOpenedAt.IsZero() {
				streamOpenedAt = time.Now()
			}
			if noContentTimeout == 0 && !hasContent && (lastNoContentNoteAt.IsZero() || time.Since(lastNoContentNoteAt) >= noContentNoteWait/2) {
				lastNoContentNoteAt = time.Now()
				return emitWaitNote(time.Since(streamOpenedAt))
			}
			return true
		}
		if len(e.body24Tools) > 0 {
			for _, name := range e.body24Tools {
				body24Tools[name] = true
			}
			if tryCodeRefFastpath() {
				return false
			}
			return true
		}
		if e.internalTool != "" {
			return onInternalTool(e.internalTool)
		}
		if e.limitBanner != "" {
			if ri != nil {
				ri.setLimitBanner(e.limitBanner)
				if !ri.limitBannerHit.Swap(true) {
					s.stats.limitBannerDetected.Add(1)
					modelName := ""
					if p.Model != nil {
						modelName = p.Model.Name
					}
					banner := cleanLimitBanner(e.limitBanner)
					s.rlog(ctx).Warn("limit_banner_detected", "model", modelName, "banner", banner)
					s.recordDiagIncident(ctx, "limit_banner", "banner", "bannière de limite Google : "+banner, false)
				}
			} else {
				s.stats.limitBannerDetected.Add(1)
			}
			return quotaTrip("", "", "limit_banner", e.limitBanner)
		}
		if e.quotaWarning != "" {
			if ri != nil {
				ri.setStatusNote("⚠ " + e.quotaWarning)
			}
			if !quotaWarned {
				quotaWarned = true
				modelName := statModelName
				if p.Model != nil && p.Model.Name != "" {
					modelName = p.Model.Name
				}
				s.rlog(ctx).Warn("quota_warning_detected", "model", modelName, "warning", e.quotaWarning)
				s.recordDiagIncident(ctx, "quota_warning", "warning_90pct", e.quotaWarning, true)
			}
			return true
		}
		if apiconv.IsInternalToolSignal(e.thought) || apiconv.IsInternalToolSignal(e.text) {
			if !onInternalTool("artifact/skills") {
				return false
			}
		}
		if e.errorMarker {
			res.errorMarker = true
			return true
		}
		if e.servedID != "" {
			verdict := "confirmed"
			if p.Model != nil && p.Model.ModelID != "" && e.servedID != p.Model.ModelID {
				if gemini.IsAutoModel(p.Model.ModelID, p.Model.Name, p.Model.DisplayName) {
					verdict = "routed_auto"
				} else {
					verdict = "other_model"
					if p.Model.Description != "" && e.servedName == p.Model.Description {
						verdict = "name_only"
					}
				}
			}
			res.servedID = e.servedID
			res.servedName = e.servedName
			res.servedVerdict = verdict
			if ri != nil {
				ri.setServed(e.servedID, e.servedName, verdict)
			}
			if verdict == "other_model" && s.autoRefresh != nil {
				s.autoRefresh.TriggerEarly()
			}
			if verdict == "other_model" && !quotaTrip(e.servedID, e.servedName, "other_model", "") {
				return false
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
			isIntTool := apiconv.IsInternalToolStatus(e.status)
			if !isIntTool {
				hasContent = true
			}
			lastFrameTime = time.Now()
			silenceWarned = false
			if ri != nil {
				ri.setStatusNote("Google : " + e.status)
			}
			if isIntTool {
				if !onInternalTool(e.status) {
					return false
				}
			}
			if e.status != lastStatusText && hooks != nil && hooks.progressive && s.cfg.StreamThoughts &&
				(lastStatusAt.IsZero() || time.Since(lastStatusAt) >= statusNoteEvery) {
				lastStatusText = e.status
				lastStatusAt = time.Now()
				res.forwarded = true
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), "Google : "+e.status))))
			}
			return true
		}
		if e.thought != "" {
			thoughtBuilder.WriteString(e.thought)
			if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
				hasContent = true
				thoughtBytes += len(e.thought)
				lastFrameTime = time.Now()
				if ri != nil {
					ri.setReceived(thoughtBytes, raw.Len())
				}
				res.forwarded = true
				res.modelForwarded = true
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, e.thought)))
			}
			return true
		}
		hasContent = true
		lastFrameTime = time.Now()
		if ri != nil {
			ri.setStatusNote("")
		}
		prevRawLen := raw.Len()
		raw.WriteString(e.text)
		if ri != nil {
			ri.setReceived(thoughtBytes, raw.Len())
		}

		scan := apiconv.ScanToolCalls(raw.String(), declared)
		if scan.HasCodeRef && codeRefDetectedAt.IsZero() {
			codeRefDetectedAt = time.Now()
		}
		if scan.CompleteCalls > 0 && !codeRefDetectedAt.IsZero() && codeRefPauseMS < 0 {
			codeRefPauseMS = time.Since(codeRefDetectedAt).Milliseconds()
		}
		if scan.CompleteCalls == 0 && tryCodeRefFastpath() {
			return false
		}
		if len(declared) > 0 && scan.HasCodeRef && !codeRefHandled {
			codeRefHandled = true
			action := normalizeInternalToolAction(s.cfg.InternalToolAction)
			if action == "retry" && !res.released && res.transmittedBytes == 0 && scan.CompleteCalls == 0 && (hooks == nil || !hooks.noInternalRetry) {
				if !onInternalTool("exécution de code") {
					return false
				}
			} else if action != "off" {
				if ri != nil {
					ri.setInternalTool("exécution de code")
					if scan.CompleteCalls == 0 {
						ri.setStatusNote("Gemini lance un outil interne (exécution de code) ; la réponse peut tarder")
					}
				}
				if !internalToolNotified {
					internalToolNotified = true
					if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
						res.forwarded = true
						if !write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), "Gemini lance un outil interne ; la réponse peut tarder.")))) {
							return false
						}
					}
				}
			}
		}
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
				toolFacts := reqToolFacts(ctx)
				holdForRefusal := len(declared) > 0 &&
					(apiconv.IsThoughtToolRefusalOrSimulation(thoughtBuilder.String(), raw.String()) ||
						apiconv.IsUpstreamToolRefusal(raw.String()) ||
						(raw.Len() <= 16384 && apiconv.IsEarlyFakeWriteOpener(raw.String(), declared, toolFacts.SinceLastUser)))
				// Only mark released when early actually contains text to emit and no
				// fence/tool_call block is currently held open (e.g. response opened with
				// ```tool_call or intro + open tool_call); keeping released=false preserves
				// stall/refusal/code-ref retries.
				if raw.Len() >= earlyHold && early.Len() > 0 && hooks.gate.Held() == 0 && !holdForRefusal {
					res.released, out = true, early.String()
				} else {
					out = ""
				}
			}
			if res.released && out != "" {
				res.forwarded = true
				res.modelForwarded = true
				rawAtLastUsefulTx = raw.Len()
				heldSince = time.Time{}
				lastHeldGrowthAt = time.Time{}
				heldGrowthBytes = 0
				res.transmittedBytes += len(out)
				ri.setSent(res.transmittedBytes)
				if !write(sse.data(apiconv.ChatChunk(hooks.cid, hooks.created, hooks.model, out))) {
					return false
				}
			}
		}

		heldBytes := raw.Len() - rawAtLastUsefulTx
		if heldBytes > 0 {
			if heldSince.IsZero() {
				heldSince = time.Now()
				lastHeldGrowthAt = heldSince
			} else if raw.Len() > prevRawLen {
				heldGrowthBytes += raw.Len() - prevRawLen
				lastHeldGrowthAt = time.Now()
			}
		}
		if maxHeldBytes > 0 && heldBytes > maxHeldBytes {
			triggerCap("size", heldBytes)
			return false
		}
		return true
	}

	preLoopOK := keepalive() // bytes right after the headers
	if preLoopOK && ri != nil {
		if note := ri.takeStaleStreamNote(); note != "" {
			if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
				preLoopOK = write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, "\n"+note+"\n")))
			} else {
				preLoopOK = write(sse.comment(note))
			}
		}
	}
	tick := interval / 4
	if tick <= 0 {
		tick = interval
	}
	if maxHeldDur > 0 && maxHeldDur/4 > 0 && maxHeldDur/4 < tick {
		tick = maxHeldDur / 4
	}
	if heldStallDur > 0 && heldStallDur/4 > 0 && heldStallDur/4 < tick {
		tick = heldStallDur / 4
	}
	if allowFastStall && heldStallFastDur > 0 && heldStallFastDur/4 > 0 && heldStallFastDur/4 < tick {
		tick = heldStallFastDur / 4
	}
	if heldNoteEvery > 0 && heldNoteEvery/4 > 0 && heldNoteEvery/4 < tick {
		tick = heldNoteEvery / 4
	}
	if hooks != nil && hooks.cannedRetryTimeout > 0 && hooks.cannedRetryTimeout/4 > 0 && hooks.cannedRetryTimeout/4 < tick {
		tick = hooks.cannedRetryTimeout / 4
	}
	if hooks != nil && hooks.initialSilenceTimeout > 0 && hooks.initialSilenceTimeout/4 > 0 && hooks.initialSilenceTimeout/4 < tick {
		tick = hooks.initialSilenceTimeout / 4
	}
	if noContentTimeout > 0 && noContentTimeout/4 > 0 && noContentTimeout/4 < tick {
		tick = noContentTimeout / 4
	}
	if noContentNoteWait > 0 && noContentNoteWait/4 > 0 && noContentNoteWait/4 < tick {
		tick = noContentNoteWait / 4
	}
	if initialWaitNoteWait > 0 && initialWaitNoteWait/4 > 0 && initialWaitNoteWait/4 < tick {
		tick = initialWaitNoteWait / 4
	}
	if codeRefWaitThresh > 0 && codeRefWaitThresh/4 > 0 && codeRefWaitThresh/4 < tick {
		tick = codeRefWaitThresh / 4
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
				return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), msg))))
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
		note := fmt.Sprintf("Préparation d'un appel d'outil… %d Ko reçus (%s)", (held+512)/1024, time.Since(start).Round(time.Second))
		res.forwarded = true
		return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), note))))
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
				note := fmt.Sprintf("Gemini ne répond plus depuis %d s ; il utilise peut-être un outil interne (recherche web ou génération d'artifact).", sec)
				if ri != nil {
					ri.setStatusNote(fmt.Sprintf("Gemini ne répond plus depuis %d s ; il utilise peut-être un outil interne", sec))
				}
				s.rlog(ctx).Warn("upstream_silence_after_thoughts", "silence_sec", sec, "thought_bytes", thoughtBytes)
				if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
					res.forwarded = true
					return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), note))))
				}
			}
		}
		return true
	}
	checkCodeRefWait := func() bool {
		if codeRefWaitNoted || codeRefDetectedAt.IsZero() || completeToolCalls > 0 {
			return true
		}
		if time.Since(codeRefDetectedAt) < codeRefWaitThresh {
			return true
		}
		codeRefWaitNoted = true
		note := "Gemini tente son outil interne d'exécution de code ; l'appel d'outil suit en général sous 10 à 60 s."
		if ri != nil {
			ri.setStatusNote(note)
		}
		if hooks != nil && hooks.progressive && s.cfg.StreamThoughts {
			res.forwarded = true
			return write(sse.data(apiconv.ChatChunkReasoning(hooks.cid, hooks.created, hooks.model, proxyWaitNote(s.currentNow(), note))))
		}
		return true
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	gotDone := false
	if preLoopOK {
	loop:
		for {
			select {
			case <-ctx.Done():
				cancel(context.Cause(ctx))
				break loop
			case <-ticker.C:
				if hooks != nil && hooks.cannedRetryTimeout > 0 && thoughtBytes == 0 && raw.Len() == 0 && s.cannedErrorRetryExceeded(time.Since(start), len(p.Prompt), heartbeats) {
					res.cannedRetryTimedOut = true
					cancel(errCannedRetryTimeout)
					break loop
				}
				if hooks != nil && hooks.initialSilenceTimeout > 0 && raw.Len() == 0 && thoughtBytes == 0 && thoughtBuilder.Len() == 0 && !res.released && time.Since(start) >= hooks.initialSilenceTimeout {
					res.earlyStopReason = "initial_silence"
					cancel(errInitialSilence)
					break loop
				}
				if !hasContent && !streamOpenedAt.IsZero() {
					elapsed := time.Since(streamOpenedAt)
					if noContentTimeout > 0 && elapsed >= noContentTimeout {
						for draining := true; draining; {
							select {
							case ev := <-events:
								if ev.heartbeat {
									heartbeats++
								}
							default:
								draining = false
							}
						}
						res.noContentTimedOut = true
						res.noContentElapsed = elapsed
						res.heartbeats = heartbeats
						cancel(errNoContentTimeout)
						break loop
					}
					if !initialWaitNoteSent && initialWaitNoteWait > 0 && elapsed >= initialWaitNoteWait {
						initialWaitNoteSent = true
						lastNoContentNoteAt = time.Now()
						if !emitInitialWaitNote() {
							break loop
						}
					} else if noContentNoteWait > 0 && elapsed >= noContentNoteWait &&
						(lastNoContentNoteAt.IsZero() || time.Since(lastNoContentNoteAt) >= noContentNoteWait) {
						initialWaitNoteSent = true
						lastNoContentNoteAt = time.Now()
						if !emitWaitNote(elapsed) {
							break loop
						}
					}
				}
				heldBytes := raw.Len() - rawAtLastUsefulTx
				if completeToolCalls == 0 && heldStallDur > 0 && heldBytes > 0 && !lastHeldGrowthAt.IsZero() {
					stallLimit := heldStallDur
					fastStallUsed := false
					if allowFastStall && heldStallFastDur > 0 && heldStallFastDur < heldStallDur &&
						res.transmittedBytes == 0 && thoughtBytes == 0 && strings.Count(raw.String(), "```")%2 == 1 {
						stallLimit = heldStallFastDur
						fastStallUsed = true
					}
					if time.Since(lastHeldGrowthAt) >= stallLimit {
						res.fastStall = fastStallUsed
						triggerCap("stall", heldBytes)
						break loop
					}
				}
				if maxHeldDur > 0 && heldBytes > 0 && !heldSince.IsZero() && time.Since(heldSince) >= maxHeldDur {
					// A genuine duration runaway requires either a captured tool call
					// holding trailing text, or active held growth (not a small frozen opener).
					activeGrowth := completeToolCalls > 0 ||
						(heldGrowthBytes > 0 && (heldStallDur <= 0 || time.Since(lastHeldGrowthAt) < heldStallDur) && time.Since(lastHeldGrowthAt) < maxHeldDur/2) ||
						heldBytes >= 1024
					if activeGrowth {
						triggerCap("duration", heldBytes)
						break loop
					}
				}
				if !hint() {
					break loop
				}
				if !checkSilence() {
					break loop
				}
				if !checkCodeRefWait() {
					break loop
				}
				if time.Since(lastWrite) >= interval && !keepalive() {
					break loop
				}
			case e := <-events:
				if !handle(e) {
					break loop
				}
			case res.genErr = <-done:
				gotDone = true
				for { // the generation is over: drain what it queued
					select {
					case e := <-events:
						if !handle(e) {
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
	}
	if !gotDone && genCtx.Err() != nil {
		// Bounded drain so streamEvents/streamOnce finishes its defer
		// (met.upstream, recordCall, served_model, client_cancelled) before
		// cancelled() and request_end run. Never wait on <-ctx.Done() here:
		// on client cancel ctx is already done and would exit in 0 ms.
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-done:
			gotDone = true
		case <-timer.C:
		}
		timer.Stop()
	}
	if ctx.Err() != nil {
		cancelled()
		return res
	}
	if res.aborted {
		return res
	}
	res.thoughtText = thoughtBuilder.String()
	if res.earlyStopReason == "" {
		res.rawText = raw.String()
		if res.genErr == nil && !res.aborted && !res.noContentTimedOut && !res.cannedRetryTimedOut &&
			!res.internalToolRetry && !hasCandidate && raw.Len() == 0 && thoughtBytes == 0 {
			res.emptyReply = true
		}
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
	maxRetries := s.maxStallRetries()
	curParams := p
	res := s.runStreamTools(ctx, sse, curParams, interval, declared, nil, maxRetries > 0)
	if res.aborted {
		if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
			return kaResult{out: kaShutdown}
		}
		return kaResult{out: kaGone}
	}
	for attempt := 1; (res.earlyStopReason == "stall" || (attempt > 1 && res.noContentTimedOut)) && res.transmittedBytes == 0 && attempt <= maxRetries; attempt++ {
		nextParams, ok := s.prepareStallRetry(ctx, sse, "", 0, "", false, curParams, res, attempt, maxRetries)
		if !ok {
			if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
				return kaResult{out: kaShutdown}
			}
			return kaResult{out: kaGone}
		}
		if res.noContentTimedOut {
			s.recordDiagIncident(ctx, "no_content_timeout", "retry", "aucun contenu (tentative non-finale)", true)
		} else {
			s.recordDiagIncident(ctx, "stall", "retry", "aucun premier texte (stall)", true)
		}
		curParams = nextParams
		var hooks *streamToolsHooks
		if maxCap := s.stallRetryNoContentCap(attempt, maxRetries); maxCap > 0 {
			hooks = &streamToolsHooks{maxNoContentTimeout: maxCap}
		}
		res = s.runStreamTools(ctx, sse, curParams, interval, declared, hooks, attempt < maxRetries)
		if res.aborted {
			if errors.Is(context.Cause(ctx), errAbortedOnShutdown) {
				return kaResult{out: kaShutdown}
			}
			return kaResult{out: kaGone}
		}
	}
	if res.earlyStopMsg != "" {
		s.markEarlyStopTerminalError(ctx, res)
		return kaResult{text: res.earlyStopMsg}
	}
	if res.earlyStopReason != "" {
		return kaResult{text: res.rawText}
	}
	if res.noContentTimedOut {
		if p.Model != nil {
			s.recordModelFailure(p.Model.Name)
		}
		sec := int(res.noContentElapsed.Round(time.Second).Seconds())
		if nc := s.noContentSecSetting(); sec < 1 && nc > 0 {
			sec = nc
		}
		s.rlog(ctx).Warn("upstream_no_content_timeout", "elapsed_sec", sec, "heartbeats", res.heartbeats)
		s.recordDiagIncident(ctx, "no_content_timeout", fmt.Sprintf("elapsed_sec=%d", sec), fmt.Sprintf("aucun contenu après %d s", sec), false)
		s.markTerminalError(ctx, "Timeout", "no_content_timeout")
		s.setErrorContext(ctx, "stream/watchdog", "finishNoContentTimeout", "no_content_timeout", fmt.Sprintf("Aucun contenu reçu de Google après %d s (%d battements)", sec, res.heartbeats))
		return kaResult{text: noContentTimeoutMessage("", p, sec)}
	}
	if res.internalToolRetry {
		return s.awaitGenerate(ctx, sse, internalToolRetryParams(p), interval)
	}
	if res.emptyReply {
		model := ""
		if p.Model != nil {
			model = p.Model.Name
		}
		return kaResult{text: s.recordEmptyReply(ctx, model, p)}
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
const glitchRetryNote = "\n\n[Retry instruction: Use ONLY the declared local tools via a ```tool_call JSON block, or reply directly in plain text. Never invoke any built-in assistant feature, web lookup, internal tool, widget, external connector, or any third-party/partner app, and never prefix tool names with any namespace.]"

func glitchRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += glitchRetryNote
	return p
}

const toolRefusalRetryNote = "\n\n[Retry instruction: The listed tools are NOT built-in model features. You invoke them by outputting a ```tool_call block in your reply; the user's system executes it locally and returns the result. There is NO tool call limit or quota per turn or per cycle. NEVER reply that tools are not declared, unavailable, or at a tool/action limit or quota, NEVER ask the user to reply \"Continue\" or give a green light mid-task, and NEVER simulate or invent a report without calling the tools first: output the next ```tool_call block immediately or reply in plain text.]"

func toolRefusalRetryParams(p gemini.GenParams) gemini.GenParams {
	if p.Mode == 1 && p.Level == 1 {
		p.Level = 2
	}
	p.Prompt += toolRefusalRetryNote
	return p
}

const silentSkipRetryNote = "\n\n[Retry instruction: You replied with plain text but did NOT emit any ```tool_call block, while the task still requires an action. Do NOT summarize progress or list remaining steps. Emit the next concrete ```tool_call block(s) now, or clearly state that the task is fully complete.]"

func silentSkipRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += silentSkipRetryNote
	return p
}

const unverifiedActionRetryNote = "\n\n[Retry instruction: You wrote a plain text reply claiming you updated/saved a file on disk or executed a command, WITHOUT emitting any tool_call block. Nothing was actually written or executed on disk! Output the real ```tool_call block (write_file / edit_file / run_command) now instead of pasting the file content in chat.]"

func unverifiedActionRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += unverifiedActionRetryNote
	return p
}

func unverifiedActionWarning(kind string) string {
	switch kind {
	case "command_without_tool":
		return "\n\n> ⚠ **GMINI (vérification mécanique)** : le modèle affirme avoir exécuté une commande ou des tests, mais **aucun appel `run_command` n'a été transmis par le proxy** depuis votre dernière demande."
	default:
		return "\n\n> ⚠ **GMINI (vérification mécanique)** : le modèle affirme avoir mis à jour ou écrit un fichier sur le disque, mais **aucun appel `write_file` / `edit_file` n'a été transmis par le proxy** depuis votre dernière demande. Vérifiez le disque avant de valider."
	}
}

func glitchReportMessage(text string) string {
	if strings.Contains(text, "<GenerateWidget") {
		return "GMINI : Gemini a tenté deux fois d'ouvrir un de ses propres outils d'affichage au lieu de répondre. Relancez la demande, ou ouvrez une nouvelle conversation."
	}
	if low := strings.ToLower(text); strings.Contains(low, "permission to turn on") || strings.Contains(low, "autorisation d'activer") || strings.Contains(low, "autorisation d’activer") {
		return "GMINI : Gemini a tenté deux fois d'activer une application ou extension tierce au lieu de répondre. Relancez la demande, ou ouvrez une nouvelle conversation."
	}
	return "GMINI : Gemini a répondu deux fois par un message de service au lieu de traiter la demande (« " +
		strings.TrimSpace(text) + " »). Reformulez la demande en donnant le chemin complet du dossier ou du fichier, puis relancez."
}

const postStallHandoffRetryNote = "\n\n[Retry instruction: Do NOT write a status summary, progress checklist, or list of remaining steps/files. A previous tool call was interrupted before completing; emit ONLY the next concrete ```tool_call block(s) immediately.]"

const initialSilenceAntiInternalNote = "\n\n[Retry instruction: Use ONLY the declared local tools via a ```tool_call JSON block. Never invoke any built-in assistant feature, web lookup, internal tool, widget, or external connector, and never prefix tool names with any namespace.]"

const toolUnparsedRetryNote = "\n\n[Retry instruction: Your previous ```tool_call block was not valid JSON and could not be parsed. Output a strictly valid JSON ```tool_call block: escape all newlines as \\n and quotes as \\\" inside string values, or split large edits into smaller calls.]"

func toolUnparsedRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += toolUnparsedRetryNote
	return p
}

func postStallHandoffRetryParams(p gemini.GenParams) gemini.GenParams {
	p, _ = prepareRecoveryParams(p)
	if p.Model == nil || !gemini.IsAutoModel(p.Model.ModelID, p.Model.Name, p.Model.DisplayName) {
		p.Mode = 1
		if p.Level == 4 {
			p.Level = 2
		}
	}
	p.Prompt += postStallHandoffRetryNote
	return p
}

const stallWriteFileRetryHint = "\n\n[Retry hint: Your previous response froze mid-tool_call. Emit ONE concise write_file call at a time instead of one large block (or for large Vue/HTML/TS files, use run_command with cat <<'EOF' > <path> if a command tool is declared).]"

const internalToolRetryNote = "\n\n[CRITICAL REMINDER: Use ONLY the declared local tools by emitting a ```tool_call JSON block immediately, or reply directly in plain text. Never invoke any built-in assistant feature, web lookup, internal tool, widget, external connector, or any third-party/partner app, and never prefix tool names with any namespace.]"

func internalToolRetryParams(p gemini.GenParams) gemini.GenParams {
	p.Prompt += internalToolRetryNote
	return p
}

var (
	errCannedRetryTimeout = errors.New("canned_retry_timeout")
	errNoContentTimeout   = errors.New("no_content_timeout")
	errInitialSilence     = errors.New("initial_silence")
)

func (s *Server) initialSilenceTimeout() time.Duration {
	if s.initialSilenceTimeoutOverride < 0 {
		return 0
	}
	if s.initialSilenceTimeoutOverride > 0 {
		return s.initialSilenceTimeoutOverride
	}
	return 25 * time.Second
}

func (s *Server) noContentTimeout() time.Duration {
	if s.noContentTimeoutOverride > 0 {
		return s.noContentTimeoutOverride
	}
	nc := s.noContentSecSetting()
	if nc <= 0 {
		return 0
	}
	return time.Duration(nc) * time.Second
}

func (s *Server) noContentNoteInterval() time.Duration {
	if s.noContentNoteOverride > 0 {
		return s.noContentNoteOverride
	}
	s.cfgMu.RLock()
	wn := s.cfg.WaitNoteSec
	s.cfgMu.RUnlock()
	if wn < 0 {
		return 0
	}
	if wn == 0 {
		return 15 * time.Second
	}
	return time.Duration(wn) * time.Second
}

func (s *Server) initialWaitNoteDelay() time.Duration {
	if s.initialWaitNoteOverride > 0 {
		return s.initialWaitNoteOverride
	}
	return 2 * time.Second
}

func noContentTimeoutMessage(model string, p gemini.GenParams, sec int) string {
	return fmt.Sprintf("GMINI : %s ne répond pas (Google n'envoie que des signaux d'attente depuis %d s). Réessayez plus tard ou changez de modèle.", modelDisplayLabel(model, p), sec)
}

func shortWaitTitle(msg string) string {
	switch {
	case strings.Contains(msg, "aucune pensée ni réponse"):
		return "En attente de Google…"
	case strings.Contains(msg, "outil interne : nouvelle tentative"):
		return "Nouvelle tentative (outil interne)…"
	case strings.Contains(msg, "erreur interne : nouvelle tentative"):
		return "Nouvelle tentative (erreur interne)…"
	case strings.Contains(msg, "outil interne d'exécution de code"):
		return "Outil interne d'exécution de code…"
	case strings.Contains(msg, "lance un outil interne"):
		return "Outil interne Google en cours…"
	case strings.Contains(msg, "appel d'outil reçu"):
		return "Appel d'outil reçu, suite en cours…"
	case strings.Contains(msg, "Préparation d'un appel d'outil"):
		return "Préparation d'un appel d'outil…"
	case strings.Contains(msg, "aucune réponse après"):
		return "Nouvelle tentative (silence initial)…"
	case strings.Contains(msg, "ne répond plus"):
		return "Silence Google (outil interne probable)…"
	}
	s := strings.TrimSpace(msg)
	if idx := strings.IndexAny(s, ";.\n"); idx > 0 && idx <= 56 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 56 {
		return s[:53] + "…"
	}
	if s == "" {
		return "Attente technique…"
	}
	return s
}

// proxyWaitNote formats a timestamped technical note with a bold first-line
// header so Antigravity displays the live step on the thinking-box title and
// clearly distinguishes proxy notes from the model's thoughts.
func proxyWaitNote(now time.Time, msg string) string {
	return proxyWaitNoteWithTitle(now, shortWaitTitle(msg), msg)
}

func proxyWaitNoteWithTitle(now time.Time, title, msg string) string {
	if title == "" {
		title = "Attente technique…"
	}
	return fmt.Sprintf("\n**[%s] GMINI — %s**\nGMINI (pas le modèle — attente technique) : %s\n", now.Format("15:04:05"), title, msg)
}

// Canned-error retry budget for very large prompts. A retried answer on a
// >=200 KiB prompt has a first token around 125 s at p90 (measured upstream), so
// the default 55 s budget would abort the retry before the first text arrives.
const (
	cannedErrorLargePromptBytes   = 200 * 1024
	cannedErrorLargePromptTimeout = 120 * time.Second
)

func (s *Server) cannedErrorRetryTimeout() time.Duration {
	if s.cannedRetryTimeoutOverride > 0 {
		return s.cannedRetryTimeoutOverride
	}
	s.cfgMu.RLock()
	sec := s.cfg.CannedErrorRetrySec
	s.cfgMu.RUnlock()
	return time.Duration(positive(sec, 55)) * time.Second
}

// cannedErrorRetryTimeoutFor returns the retry timeout to arm for a prompt of
// promptBytes bytes. The explicit override always wins (no extension); otherwise
// a large prompt gets the extended budget when it is longer than the base one.
func (s *Server) cannedErrorRetryTimeoutFor(promptBytes int) time.Duration {
	if s.cannedRetryTimeoutOverride > 0 {
		return s.cannedRetryTimeoutOverride
	}
	base := s.cannedErrorRetryTimeout()
	if promptBytes >= cannedErrorLargePromptBytes && cannedErrorLargePromptTimeout > base {
		return cannedErrorLargePromptTimeout
	}
	return base
}

// cannedErrorRetryExceeded reports whether elapsed exhausts the canned-error
// retry budget. The extended large-prompt budget only applies while the upstream
// stream is still alive (heartbeats > 0): a dead stream keeps the base budget so
// it is cut fast instead of hanging for two minutes.
func (s *Server) cannedErrorRetryExceeded(elapsed time.Duration, promptBytes, heartbeats int) bool {
	if s.cannedRetryTimeoutOverride > 0 {
		return elapsed >= s.cannedRetryTimeoutOverride
	}
	budget := s.cannedErrorRetryTimeout()
	if promptBytes >= cannedErrorLargePromptBytes && cannedErrorLargePromptTimeout > budget {
		if heartbeats > 0 {
			budget = cannedErrorLargePromptTimeout
		} else if 70*time.Second > budget {
			budget = 70 * time.Second
		}
	}
	return elapsed >= budget
}

func modelDisplayLabel(model string, p gemini.GenParams) string {
	if p.Model != nil && p.Model.Description != "" {
		return p.Model.Description
	}
	if p.Model != nil && p.Model.DisplayName != "" {
		return p.Model.DisplayName
	}
	return model
}

func cannedErrorMessage(model string, p gemini.GenParams) string {
	return fmt.Sprintf("GMINI : Gemini a renvoyé une erreur interne sur %s (pas une réponse du modèle). Réessayez, ou changez de modèle.", modelDisplayLabel(model, p))
}

func emptyReplyMessage(model string, p gemini.GenParams) string {
	return fmt.Sprintf("GMINI : Google a renvoyé une réponse vide sur %s (prompt de %d Ko). Réessayez, raccourcissez la conversation ou changez de modèle.",
		modelDisplayLabel(model, p), koCount(len(p.Prompt)))
}

func (s *Server) recordEmptyReply(ctx context.Context, model string, p gemini.GenParams) string {
	s.stats.upstreamEmptyReply.Add(1)
	s.recordModelFailure(model)
	s.rlog(ctx).Warn("upstream_empty_reply",
		"model", model,
		"prompt_bytes", len(p.Prompt),
		"prompt_kb", koCount(len(p.Prompt)))
	return emptyReplyMessage(model, p)
}

func (s *Server) isCannedRetryText(ctx context.Context, text string) bool {
	if apiconv.IsUpstreamCannedError(text) {
		return true
	}
	if ri := reqFrom(ctx); ri != nil {
		if rec, ok := s.client.CallByRequest(ri.id); ok && rec.Identity == "canned_error" {
			return true
		}
	}
	return false
}

const (
	modelFailureWindow    = 15 * time.Minute
	modelFailureThreshold = 2
)

func (s *Server) currentNow() time.Time {
	if s.warnNow != nil {
		return s.warnNow()
	}
	return time.Now()
}

func pruneFailureTimes(ts []time.Time, now time.Time) []time.Time {
	cutoff := now.Add(-modelFailureWindow)
	keep := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	return keep
}

func (s *Server) recordModelFailure(model string) {
	if model == "" {
		return
	}
	now := s.currentNow()
	s.failMemMu.Lock()
	defer s.failMemMu.Unlock()
	if s.failMem == nil {
		s.failMem = map[string][]time.Time{}
	}
	list := pruneFailureTimes(s.failMem[model], now)
	list = append(list, now)
	s.failMem[model] = list
}

func (s *Server) recordModelSuccess(model string) {
	if model == "" {
		return
	}
	s.failMemMu.Lock()
	delete(s.failMem, model)
	s.failMemMu.Unlock()
}

func (s *Server) shouldSkipCannedRetry(model string) bool {
	if model == "" {
		return false
	}
	now := s.currentNow()
	s.failMemMu.Lock()
	defer s.failMemMu.Unlock()
	list := pruneFailureTimes(s.failMem[model], now)
	if len(list) == 0 {
		delete(s.failMem, model)
		return false
	}
	s.failMem[model] = list
	return len(list) >= modelFailureThreshold
}

type modelFailureSummary struct {
	Model        string
	Count        int
	Since        time.Time
	Unresponsive bool
}

func (s *Server) modelFailures() []modelFailureSummary {
	now := s.currentNow()
	s.failMemMu.Lock()
	defer s.failMemMu.Unlock()
	var out []modelFailureSummary
	for model, raw := range s.failMem {
		list := pruneFailureTimes(raw, now)
		if len(list) == 0 {
			delete(s.failMem, model)
			continue
		}
		s.failMem[model] = list
		out = append(out, modelFailureSummary{
			Model:        model,
			Count:        len(list),
			Since:        list[0],
			Unresponsive: len(list) >= modelFailureThreshold,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

const (
	staleTokenWarnThreshold = 120 * time.Minute
	staleTokenWarnCooldown  = 30 * time.Minute
)

func (s *Server) autoRefreshEnabled() bool {
	return s.autoRefresh != nil && s.autoRefresh.IsEnabled()
}

// prepareStaleTokenAlert warns the user in the stream (at most once per 30 min)
// and in "Demandes en cours" when auto-refresh is off and the token exceeds
// 120 min, without launching Chrome or delaying the request (K16 A3).
func (s *Server) prepareStaleTokenAlert(ctx context.Context) {
	if s.autoRefreshEnabled() {
		return
	}
	rot := s.client.RotationStatus()
	if rot.TokenAgeSource == "unknown" || rot.TokenAge <= staleTokenWarnThreshold {
		return
	}
	mins := int(rot.TokenAge / time.Minute)
	alert := fmt.Sprintf("GMINI : jeton de %d min — risque de repli sur un autre modèle ; actualisez le cookie depuis GMINI Console-Web-UI", mins)
	var streamNote string
	now := s.currentNow()
	s.staleWarnMu.Lock()
	if s.staleWarnAt.IsZero() || now.Sub(s.staleWarnAt) >= staleTokenWarnCooldown {
		s.staleWarnAt = now
		streamNote = alert
	}
	s.staleWarnMu.Unlock()
	if ri := reqFrom(ctx); ri != nil {
		ri.setStaleTokenAlert(alert, streamNote)
	}
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

func (s *Server) promptWarnKB() int {
	if s.cfg.PromptWarnKB < 0 {
		return 0
	}
	if s.cfg.PromptWarnKB == 0 {
		return 300
	}
	return s.cfg.PromptWarnKB
}

func (s *Server) maxHeldBytes() int {
	v := s.cfgSnapshot().MaxHeldBytes
	if v < 0 {
		return 0
	}
	if v == 0 {
		return 256 * 1024
	}
	return v
}

func (s *Server) maxHeldDuration() time.Duration {
	if s.heldDurationOverride > 0 {
		return s.heldDurationOverride
	}
	v := s.cfgSnapshot().MaxHeldSec
	if v < 0 {
		return 0
	}
	if v == 0 {
		return 120 * time.Second
	}
	return time.Duration(v) * time.Second
}

func (s *Server) heldStallDuration() time.Duration {
	if s.heldStallOverride > 0 {
		return s.heldStallOverride
	}
	v := s.cfgSnapshot().HeldStallSec
	if v < 0 {
		return 0
	}
	if v == 0 {
		return 60 * time.Second
	}
	return time.Duration(v) * time.Second
}

func (s *Server) heldStallFastDuration() time.Duration {
	if s.heldStallFastOverride > 0 {
		return s.heldStallFastOverride
	}
	v := s.cfgSnapshot().HeldStallFastSec
	if v < 0 {
		return 0
	}
	if v == 0 {
		return 25 * time.Second
	}
	return time.Duration(v) * time.Second
}

const largePromptStallThresholdBytes = 250 * 1024

func (s *Server) effectiveHeldStallDurations(promptBytes int) (stall, fast time.Duration) {
	stall = s.heldStallDuration()
	fast = s.heldStallFastDuration()
	if promptBytes >= largePromptStallThresholdBytes {
		if s.heldStallOverride <= 0 && stall > 0 && stall < 60*time.Second {
			stall = 60 * time.Second
		}
		if s.heldStallFastOverride <= 0 && fast > 0 && fast < 30*time.Second {
			fast = 30 * time.Second
		}
	}
	return stall, fast
}

func (s *Server) maxStallRetries() int {
	v := s.cfgSnapshot().HeldStallRetries
	if v < 0 {
		return 0
	}
	if v == 0 {
		return 3
	}
	if v > 5 {
		return 5
	}
	return v
}

func (s *Server) stallRetryBackoff() time.Duration {
	if s.stallRetryBackoffOverride < 0 {
		return 0
	}
	if s.stallRetryBackoffOverride > 0 {
		return s.stallRetryBackoffOverride
	}
	return 750*time.Millisecond + time.Duration(rand.Intn(751))*time.Millisecond
}

func (s *Server) waitRetryBackoff(ctx context.Context) bool {
	if delay := s.stallRetryBackoff(); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	} else if ctx.Err() != nil {
		return false
	}
	return true
}

// prepareRecoveryParams steps down level=4 to level=2 (the highest-reliability
// thinking level) on recovery retries across all model modes, preserving
// p.TemporaryChat as configured.
func prepareRecoveryParams(p gemini.GenParams) (gemini.GenParams, bool) {
	if p.Level == 4 {
		p.Level = 2
		return p, true
	}
	return p, false
}

const (
	firstAttemptNoContentTimeout       = 75 * time.Second
	nonFinalStallRetryNoContentTimeout = 75 * time.Second
	finalNoContentRetryCap             = 120 * time.Second
)

func (s *Server) firstAttemptNoContentCap() time.Duration {
	if s.firstAttemptNoContentOverride > 0 {
		return s.firstAttemptNoContentOverride
	}
	return firstAttemptNoContentTimeout
}

func (s *Server) finalNoContentCap() time.Duration {
	if s.finalNoContentRetryOverride > 0 {
		return s.finalNoContentRetryOverride
	}
	return finalNoContentRetryCap
}

func (s *Server) nonFinalStallNoContentCap() time.Duration {
	if s.nonFinalStallNoContentOverride > 0 {
		return s.nonFinalStallNoContentOverride
	}
	return nonFinalStallRetryNoContentTimeout
}

// stallRetryNoContentCap returns the no-content timeout cap for a stall retry
// attempt: non-final attempts (attempt < maxRetries) are capped so subsequent
// retries (e.g. 2/2) can still run, while the final attempt keeps the full
// noContentTimeout (0 = no cap).
func (s *Server) stallRetryNoContentCap(attempt, maxRetries int) time.Duration {
	if attempt >= maxRetries {
		return 0
	}
	return s.nonFinalStallNoContentCap()
}

func appendResumeHint(ctx context.Context, msg string) string {
	ri := reqFrom(ctx)
	if ri == nil {
		return msg
	}
	ri.inFlightMu.RLock()
	priorCalls := 0
	for _, c := range ri.toolFacts.Total.Counts {
		priorCalls += c
	}
	priorCalls += len(ri.emittedTools)
	ri.inFlightMu.RUnlock()
	if priorCalls <= 0 {
		return msg
	}
	return msg + fmt.Sprintf("\n\n_Contexte de reprise GMINI : %d appel(s) d'outil déjà exécuté(s) dans cette conversation ; envoyez « Continue » pour reprendre à l'étape suivante sans repartir de zéro._", priorCalls)
}

func (s *Server) recordUnknownModel(ctx context.Context, modelName string, err error) {
	setReqModel(ctx, modelName)
	s.markTerminalError(ctx, "Modèle inconnu (HTTP 400)", "unknown_model")
	s.setErrorContext(ctx, "proxy/catalog", "ResolveModel", "unknown_model", err.Error()+" — actualisez le catalogue dans Modèles")
	s.recordDiagIncident(ctx, "unknown_model", "http_400", err.Error(), false)
}

func (s *Server) markTerminalError(ctx context.Context, state, reason string) {
	if ri := reqFrom(ctx); ri != nil {
		ri.setTerminalError(state, reason)
	}
}

func (s *Server) setErrorContext(ctx context.Context, service, fn, code, detail string) {
	if ri := reqFrom(ctx); ri != nil {
		ri.setErrorContext(service, fn, code, detail)
	}
}

func (s *Server) markEarlyStopTerminalError(ctx context.Context, res streamToolsResult) {
	switch res.earlyStopReason {
	case "quota_pause":
		s.markTerminalError(ctx, "Pause quota", "quota_pause")
		s.setErrorContext(ctx, "upstream/google", "quotaTrip", "quota_fallback_pause", "Pause quota déclenchée en cours de flux")
	case "stall":
		s.markTerminalError(ctx, "Blocage outil", "stall")
		s.setErrorContext(ctx, "stream/watchdog", "runStreamTools", "early_stop:stall", res.earlyStopMsg)
		s.recordDiagIncident(ctx, "stall", "terminal", res.earlyStopMsg, false)
	default:
		s.markTerminalError(ctx, "Réponse interrompue", res.earlyStopReason)
		s.setErrorContext(ctx, "stream/watchdog", "runStreamTools", "early_stop:runaway", res.earlyStopMsg)
		s.recordDiagIncident(ctx, "runaway", "terminal", res.earlyStopMsg, false)
	}
}

func (s *Server) prepareStallRetry(ctx context.Context, sse *sseStream, cid string, created int64, model string, progressive bool, p gemini.GenParams, res streamToolsResult, attempt, maxRetries int) (gemini.GenParams, bool) {
	if !s.waitRetryBackoff(ctx) {
		return p, false
	}

	retryParams := p
	stepdown := ""
	if attempt == 1 && retryParams.Level > 2 {
		stepdown = fmt.Sprintf("%d->2", retryParams.Level)
		retryParams.Level = 2
	} else if attempt == 1 && retryParams.Level == 2 {
		stepdown = "2->1"
		retryParams.Level = 1
	} else if attempt >= 2 && retryParams.Level > 1 {
		stepdown = fmt.Sprintf("%d->1", retryParams.Level)
		retryParams.Level = 1
	}
	hintAdded := false
	isWriteStall := strings.Contains(res.rawText, "write_file") || strings.Contains(res.rawText, "edit_file")
	if attempt >= 1 && isWriteStall && !strings.Contains(retryParams.Prompt, "[Retry hint:") {
		retryParams.Prompt += stallWriteFileRetryHint
		hintAdded = true
	}

	s.stats.heldStallRetries.Add(1)
	attrs := []any{
		"attempt", attempt,
		"max_retries", maxRetries,
		"held_bytes", res.heldBytes,
		"fast_stall", res.fastStall,
		"temp_chat", retryParams.TemporaryChat,
		"model_forwarded", res.modelForwarded,
		"released", res.released,
	}
	if stepdown != "" {
		attrs = append(attrs, "level_stepdown", stepdown)
	}
	if hintAdded {
		attrs = append(attrs, "retry_hint", true)
	}
	s.rlog(ctx).Info("held_stall_retry", attrs...)

	if progressive && s.cfg.StreamThoughts {
		var note string
		if res.noContentTimedOut {
			sec := int(res.noContentElapsed.Round(time.Second).Seconds())
			note = fmt.Sprintf("Google n'a envoyé aucun contenu (%d s d'attente ; %d battement(s)) : nouvelle tentative automatique (%d/%d)…", sec, res.heartbeats, attempt, maxRetries)
		} else {
			note = fmt.Sprintf("Gemini s'est figé en cours d'appel d'outil (%s reçus sans suite) : nouvelle tentative automatique (%d/%d)…", formatByteCount(res.heldBytes), attempt, maxRetries)
		}
		if err := sse.data(apiconv.ChatChunkReasoning(cid, created, model, proxyWaitNote(s.currentNow(), note))); err != nil {
			s.rlog(ctx).Warn("sse write failed", "err", err)
			return p, false
		}
	}
	return retryParams, true
}

func formatByteCount(bytes int) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d o", bytes)
	}
	return fmt.Sprintf("%d Ko", koCount(bytes))
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
	case "stall":
		s.stats.earlyStopStall.Add(1)
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
		note += " : modèle réel non garanti ; actualiser le cookie via le raccourci 7 et vérifier GMINI Console-Web-UI.\n"
	case verdict == "other_model" || strings.HasPrefix(verdict, "suspect:"):
		note = "\n⚠ GMINI : cette réponse ne vient probablement pas du modèle demandé (" + model +
			"). Google sert un autre modèle quand la limite d'utilisation du site Gemini est atteinte. Vérifiez la page GMINI Console-Web-UI avant de vous fier au résultat.\n"
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
			} else if s.quotaAfterGenerate(ctx, p) {
				res.text = quotaPauseMessage(s.quotaPauseSnapshot())
				s.markTerminalError(ctx, "Pause quota", "quota_pause")
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
		s.recordUnknownModel(r.Context(), req.Model, err)
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.quotaPreflight(w, r, m, flavorOpenAI) {
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
	setReqConvID(r.Context(), convFingerprintFromPrompt(prompt))
	setReqInput(r.Context(), len(prompt), utf8.RuneCountInString(prompt), len(images))
	s.client.RecordUserActivity()
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
	s.client.EnsureAuthOnRequest(r.Context())
	s.prepareStaleTokenAlert(r.Context())

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
	if s.quotaAfterGenerate(r.Context(), p) {
		s.sendQuotaPause429(w, flavorOpenAI)
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())
	if text != "" || len(toolCalls) > 0 {
		s.recordModelSuccess(m.Name)
	}

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
		s.recordUnknownModel(r.Context(), name, err)
		sendError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.quotaPreflight(w, r, m, flavorGoogle) {
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
	setReqConvID(r.Context(), convFingerprintFromPrompt(prompt))
	setReqInput(r.Context(), len(prompt), utf8.RuneCountInString(prompt), len(images))
	s.client.RecordUserActivity()
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
	s.client.EnsureAuthOnRequest(r.Context())
	s.prepareStaleTokenAlert(r.Context())

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
	if s.quotaAfterGenerate(r.Context(), p) {
		s.sendQuotaPause429(w, flavorGoogle)
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
			if ri := reqFrom(ctx); ri != nil {
				if note := ri.takeStaleStreamNote(); note != "" {
					_ = sse.comment(note)
				}
			}
		}
		full.WriteString(delta)
		if err := sse.data(apiconv.GoogleStreamChunk(model, delta)); err != nil {
			s.rlog(ctx).Warn("sse write failed; aborting generation", "err", err)
			cancel()
		}
	}
	emit, finish := s.echoEmitter(ctx, emitRaw, &full)

	var hasCandidate, hasThought bool
	guard := s.newQuotaGuard(ctx, p, func() bool { return sse != nil }, cancel)
	err := s.client.GenerateStreamEvents(gctx, p, guard.wrap(gemini.StreamHandlers{
		Text:      emit,
		Thought:   func(string) { hasThought = true },
		Candidate: func() { hasCandidate = true },
	}))
	finish()
	if sse == nil && guard.isTripped() {
		s.sendQuotaPause429(w, flavorGoogle)
		return
	}
	if sse == nil {
		switch {
		case noFlush:
			sendError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		case !hasCandidate && !hasThought:
			var ok bool
			if sse, ok = openSSE(w); !ok {
				sendError(w, http.StatusInternalServerError, "streaming unsupported")
				return
			}
			if ri := reqFrom(ctx); ri != nil {
				if note := ri.takeStaleStreamNote(); note != "" {
					_ = sse.comment(note)
				}
			}
			msg := s.recordEmptyReply(ctx, model, p)
			s.sseTail(ctx, sse, sse.data(apiconv.GoogleStreamFinal(model, p.Prompt, msg)))
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
	if s.quotaPreflight(w, r, m, flavorAnthropic) {
		return
	}
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
	setReqConvID(r.Context(), convFingerprintFromPrompt(prompt))
	setReqInput(r.Context(), len(prompt), utf8.RuneCountInString(prompt), len(images))
	s.client.RecordUserActivity()
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
	s.client.EnsureAuthOnRequest(r.Context())
	s.prepareStaleTokenAlert(r.Context())

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
	if s.quotaAfterGenerate(r.Context(), p) {
		s.sendQuotaPause429(w, flavorAnthropic)
		return
	}
	text, toolCalls := s.finishText(r.Context(), text, toolsActive, req.DeclaredTools())
	if text != "" || len(toolCalls) > 0 {
		s.recordModelSuccess(m.Name)
	}

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

	var hasCandidate, hasThought bool
	guard := s.newQuotaGuard(ctx, p, func() bool { return sse != nil }, cancel)
	err := s.client.GenerateStreamEvents(gctx, p, guard.wrap(gemini.StreamHandlers{
		Text:      emit,
		Thought:   func(string) { hasThought = true },
		Candidate: func() { hasCandidate = true },
	}))
	finish()
	if sse == nil && guard.isTripped() {
		s.sendQuotaPause429(w, flavorAnthropic)
		return
	}
	if sse == nil {
		// Nothing was streamed yet, so a normal HTTP error status is still possible.
		switch {
		case noFlush:
			sendAnthropicError(w, http.StatusInternalServerError, "streaming unsupported")
		case err != nil:
			sendAnthropicError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		case !hasCandidate && !hasThought:
			var ok bool
			if sse, ok = openSSE(w); !ok {
				sendAnthropicError(w, http.StatusInternalServerError, "streaming unsupported")
				return
			}
			msg := s.recordEmptyReply(ctx, model, p)
			s.streamMessagesFull(ctx, sse, msgID, model, p.Prompt, msg, nil)
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
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
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

// openSSE writes SSE response headers and returns the stream. If the response
// headers were already sent on this request (e.g. during an on-demand cookie
// refresh wait note), it reuses the open stream without calling WriteHeader twice.
func openSSE(w http.ResponseWriter) (*sseStream, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	if sw, ok := w.(*statusWriter); ok && sw.status != 0 {
		return &sseStream{w: w, f: f}, true
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
