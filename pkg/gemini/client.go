// Package gemini is a reusable client for Google's Gemini web backend
// (the BardChatUi batchexecute endpoints). It handles cookie/SAPISIDHASH auth,
// dynamic build-label and model-catalog resolution, cookie rotation, image
// uploads, and blocking or streaming text generation.
//
// A typical consumer builds a Config, calls New, optionally resolves the build
// label / auth / model list at startup, and then calls Generate or
// GenerateStream:
//
//	c, err := gemini.New(gemini.Config{CookieFile: "cookie.txt"}, logger)
//	c.CheckAuth(ctx)
//	c.ResolveModels(ctx)
//	m, _ := c.ResolveModelOrDefault("gemini-3.5-flash")
//	text, err := c.Generate(ctx, m.Params("Hello"))
package gemini

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/n0madic/go-gemini-web2api/pkg/util"
)

// geminiBaseURL is the public Gemini host serving the BardChatUi batchexecute endpoint.
const geminiBaseURL = "https://gemini.google.com"

// userAgent mirrors a desktop Chrome UA, as the reference does.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

// codeArtifactRe strips internal code-execution artifacts that Gemini embeds in
// streamed text (e.g. ```python?code_reference&code_event_index=0 ... ```).
var codeArtifactRe = regexp.MustCompile("(?s)```(?:python|javascript|text)\\?code_(?:reference|stdout)&code_event_index=\\d+\\n.*?```\\n?")

// cardContentRe strips googleusercontent card-content reference URLs.
var cardContentRe = regexp.MustCompile(`http://googleusercontent\.com/card_content/\d+\n?`)

// ErrEmptyResponse indicates Gemini returned a 200 with no extractable text,
// typically because the prompt exceeded the web endpoint's size limit. Generate
// returns it (unwrapped) so callers can detect this case with errors.Is.
var ErrEmptyResponse = errors.New("empty response from Gemini (prompt may exceed the web endpoint size limit)")

// buildLabelRe extracts the build label (bl) from WIZ_global_data on the Gemini page.
var buildLabelRe = regexp.MustCompile(`"cfb2h":"(boq_[^"]+)"`)

// snlm0eRe captures the SNlM0e (XSRF/"at") token, which the Gemini page embeds
// only for a signed-in session: its presence proves the cookie authenticates, and
// its value is required as the "at" form field on authenticated StreamGenerate calls.
var snlm0eRe = regexp.MustCompile(`"SNlM0e":"([^"]+)"`)

// maxResponseBytes caps a blocking Generate response body (D04).
const maxResponseBytes = 32 << 20 // 32 MiB

// ErrResponseTooLarge is returned when the upstream response exceeds maxResponseBytes.
var ErrResponseTooLarge = errors.New("gemini response exceeds the 32 MiB size limit")

// ErrEarlyStop is used as a cancellation cause when the proxy intentionally stops
// reading a runaway upstream stream after receiving complete tool call(s).
var ErrEarlyStop = errors.New("early_stop")

// ErrEarlyStopAborted is used as a cancellation cause when the proxy stops reading
// a runaway upstream stream because a safety cap was exceeded without a tool call.
var ErrEarlyStopAborted = errors.New("early_stop_aborted")

// ErrInternalToolRetry is used as a cancellation cause when the proxy aborts a
// stream upon detecting an internal Google tool so it can retry once (B2).
var ErrInternalToolRetry = errors.New("internal_tool_retry")

// readLimited reads at most max bytes, returning ErrResponseTooLarge when the
// body is longer.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, ErrResponseTooLarge
	}
	return b, nil
}

// Client talks to the Gemini web backend. It is safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
	log  *slog.Logger

	// imageHTTP fetches remote image_url images through a destination-filtering
	// dialer; imageDest is that filter (overridable in tests).
	imageHTTP *http.Client
	imageDest func(ip net.IP, port string) bool

	// cookie file cache, keyed by file mtime to avoid re-reading every request.
	cookieMu     sync.Mutex
	cookieStr    string
	cookieSAPI   string
	cookieMtime  time.Time
	cookieLoaded bool
	cookieGen    uint64    // bumped whenever the cookie jar changes (rotation, capture, file rewrite)
	rotatedAt    time.Time // last successful __Secure-1PSIDTS rotation (guarded by cookieMu)
	psidtsAt     time.Time // last real change of __Secure-1PSIDTS (persisted; guarded by cookieMu)
	psidtsHash   string    // truncated SHA-256 of __Secure-1PSIDTS matching psidtsAt
	rot          rotState  // rotation attempt tracking (guarded by cookieMu)
	block        blockState
	captureMu    sync.Mutex // serialises Set-Cookie capture (read-modify-write of the jar)

	// Cached /app page (shared by the build-label scrape and the auth check so a
	// startup costs one Google call instead of two).
	appMu   sync.Mutex
	appBody []byte
	appAt   time.Time

	// Build label, optionally auto-resolved from the Gemini page. It is only
	// refreshed when the page is loaded (startup without a recent state, session
	// reload): there is no TTL any more (AUDIT-13 A6).
	blMu        sync.Mutex
	bl          string
	blAuto      bool
	blAt        time.Time
	blResolving bool

	// XSRF "at" token (SNlM0e), scraped from the signed-in page. Required on
	// authenticated StreamGenerate requests; anonymous requests omit it.
	atMu     sync.Mutex
	atToken  string
	atAt     time.Time // when atToken was obtained (page scrape or restored state)
	reauthAt time.Time // last session reload triggered by a generation error
	auth     authCheck // outcome of the last /app auth check (guarded by atMu)

	// Model registry, populated from the listModels RPC (or the static fallback
	// set). modelOrder preserves a stable listing order.
	modelMu              sync.Mutex
	models               map[string]*AvailableModel
	modelOrder           []string
	modelsSource         string               // "dynamic" | "fallback" | "static" | "persisted"
	catalogAt            time.Time            // last successful listModels fetch (or restored state)
	catalogTriedAt       time.Time            // last listModels attempt (success or not)
	catalogAfterCookieAt time.Time            // last listModels attempt after a cookie refresh (K14 P3)
	catalogWarnAt        time.Time            // last "model list refresh failed" WARN (guarded by modelMu)
	absentWarnAt         map[string]time.Time // last "model absent from the last catalogue" WARN per model
	// withdrawn holds models that a later catalogue no longer lists (observed when
	// the web usage limit is reached). In memory only (guarded by modelMu).
	withdrawn map[string]WithdrawnModel
	// suspectCheckAt is the last catalogue check triggered by a suspect reply.
	suspectCheckAt time.Time
	// sessionDegraded is set when listModels shrinks to <=1 model while >=2 were known.
	sessionDegraded      bool
	sessionDegradedAt    time.Time
	sessionAccountStatus int // body[14] from degraded listModels response (0 = absent)

	// Session state file (AUDIT-13 A5).
	stateMu         sync.Mutex
	stateSavedAt    time.Time
	stateMtime      time.Time
	autoRefreshPref atomic.Int32 // -1 unset, 0 false, 1 true (K18 T4)

	// Content-free journal of generations (served.go) & token ledger (tokens.go).
	calls  callJournal
	tokens tokenLedger
}

// New builds a Client with an HTTP client honouring the configured timeout and
// proxy. Any Config field left at its zero value is filled with the matching
// Default* constant (see Config.applyDefaults), so gemini.Config{} — optionally
// with just a cookie — yields a working client. A nil logger defaults to
// slog.Default().
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	cfg.applyDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(pu)
	}
	c := &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout:   time.Duration(cfg.RequestTimeout) * time.Second,
			Transport: transport,
		},
		log:       logger,
		bl:        cfg.GeminiBL,
		blAuto:    cfg.GeminiBLAuto,
		imageDest: destAllowed,
	}
	c.http.Transport = &googleTransport{base: transport, c: c}
	c.imageHTTP = c.newImageHTTPClient()
	c.autoRefreshPref.Store(-1)
	c.loadCalls()
	c.loadTokens()
	return c, nil
}

// GenParams carries everything needed for a single generation request.
type GenParams struct {
	Prompt string
	Images []InputImage    // images to attach (uploaded just before generation)
	Model  *AvailableModel // selected model (drives the selection headers)
	// Level and Mode are the web "thinking level" (inner[80]: 1 low, 4 medium,
	// 2 high) and "mode" (inner[79], e.g. 3 = Pro) codes. 0 = not sent (default:
	// the body is byte-identical to the historical one). EXPERIMENTAL.
	Level int
	Mode  int
}

// InputImage is an image to attach to a generation request. Exactly one of Data
// (inline bytes already decoded from base64) or URL (a remote http(s) image to
// fetch at upload time) is populated.
type InputImage struct {
	Data     []byte
	URL      string
	Filename string
}

// buildBody constructs the urlencoded request body for StreamGenerate. The Gemini
// backend expects f.req to be [null, "<inner-json-string>"] where the inner payload
// is a sparse 69-element array with specific indices populated. Attached images
// (already uploaded) are referenced at inner[0][3]. The model is selected via HTTP
// headers (see AvailableModel.headers), not the payload; inner[17] is a constant.
//
// It also returns the per-request UUID (inner[59]); the caller must echo it in the
// x-goog-ext-525005358-jspb selection header so the two stay in sync.
func (c *Client) buildBody(p GenParams, images []uploadedImage) (body, reqUUID string, err error) {
	reqUUID = strings.ToUpper(uuid4())
	inner := make([]any, 69)
	inner[0] = []any{p.Prompt, 0, nil, imageRefsField(images), nil, nil, 0}
	for _, bc := range bodyConstants {
		inner[bc.Index] = bc.Value
	}
	inner[59] = reqUUID
	if c.cfg.TemporaryChat {
		inner[45] = 1
	}
	if p.Level > 0 || p.Mode > 0 {
		inner = append(inner, make([]any, 81-len(inner))...)
		if p.Mode > 0 {
			inner[79] = p.Mode
		}
		if p.Level > 0 {
			inner[80] = p.Level
		}
	}

	innerJSON, err := util.MarshalNoEscape(inner)
	if err != nil {
		return "", "", err
	}
	outerJSON, err := util.MarshalNoEscape([]any{nil, string(innerJSON)})
	if err != nil {
		return "", "", err
	}

	vals := url.Values{}
	vals.Set("f.req", string(outerJSON))
	if at := c.currentAt(); at != "" {
		vals.Set("at", at)
	}
	return vals.Encode(), reqUUID, nil
}

// reqIDCounter yields the upstream _reqid values. It starts at a random offset
// and increments atomically, so concurrent requests never share an id (the old
// time.Now().Unix()%1e6 collided for every request within the same second).
var reqIDCounter atomic.Uint64

func init() {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		reqIDCounter.Store(uint64(binary.BigEndian.Uint32(b[:])))
	}
}

// nextReqID returns a 6-digit, process-unique (until 1e6 requests wrap) id.
func nextReqID() uint64 {
	return reqIDCounter.Add(1) % 1000000
}

// streamURL builds the StreamGenerate URL with a per-request id.
func (c *Client) streamURL() string {
	reqid := nextReqID()
	prefix := c.accountPrefix()
	return fmt.Sprintf(
		"%s%s/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"+
			"?bl=%s&hl=en&_reqid=%d&rt=c",
		geminiBaseURL, prefix, url.QueryEscape(c.CurrentBL()), reqid)
}

// CurrentBL returns the active build label. It is a pure accessor: it never
// triggers a Google call (AUDIT-13 A4/A6).
func (c *Client) CurrentBL() string {
	c.blMu.Lock()
	defer c.blMu.Unlock()
	return c.bl
}

// ResolveBuildLabel synchronously resolves the build label (used at startup when
// no recent state was restored). It is a no-op when auto-resolution is disabled.
// The /app page it loads is cached briefly and reused by CheckAuth.
func (c *Client) ResolveBuildLabel(ctx context.Context) {
	if !c.blAuto {
		return
	}
	c.blMu.Lock()
	if c.blResolving {
		c.blMu.Unlock()
		return
	}
	c.blResolving = true
	c.blMu.Unlock()
	bl := c.fetchBuildLabel(ctx)
	c.blMu.Lock()
	if bl != "" {
		c.bl, c.blAt = bl, time.Now()
	}
	c.blResolving = false
	c.blMu.Unlock()
	if bl != "" {
		c.log.Info("resolved gemini build label", "bl", bl)
	} else {
		c.log.Warn("could not resolve gemini build label; using fallback", "fallback", c.CurrentBL())
	}
}

// fetchAppPage GETs the Gemini /app page (with the cookie when present); it is the
// shared source for scraping the build label and the SNlM0e XSRF token. The body is
// capped at 4MB.
func (c *Client) fetchAppPage(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiBaseURL+c.accountPrefix()+"/app", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if cookie, _ := c.loadCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("gemini /app page: http %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// appPageMaxAge is how long a fetched /app page may be reused by the build-label
// scrape and the auth check (startup dedupe).
const appPageMaxAge = 30 * time.Second

// fetchAppPageCached returns the /app page, reusing a copy fetched less than
// maxAge ago. While a block backoff is in force it does not touch the network.
func (c *Client) fetchAppPageCached(ctx context.Context, maxAge time.Duration) ([]byte, error) {
	c.appMu.Lock()
	defer c.appMu.Unlock()
	if c.appBody != nil && time.Since(c.appAt) < maxAge {
		return c.appBody, nil
	}
	if c.blockedNow(FamilyApp) {
		return nil, &BlockedError{Family: FamilyApp, Host: "gemini.google.com", Path: "/app"}
	}
	body, err := c.fetchAppPage(ctx)
	if err != nil {
		return nil, err
	}
	c.appBody, c.appAt = body, time.Now()
	return body, nil
}

func (c *Client) invalidateAppPage() {
	c.appMu.Lock()
	c.appBody, c.appAt = nil, time.Time{}
	c.appMu.Unlock()
}

// applyBuildLabel stores the build label found in an /app page, when
// auto-resolution is on.
func (c *Client) applyBuildLabel(html []byte) {
	if !c.blAuto {
		return
	}
	if m := buildLabelRe.FindSubmatch(html); m != nil {
		c.blMu.Lock()
		c.bl, c.blAt = string(m[1]), time.Now()
		c.blMu.Unlock()
	}
}

// fetchBuildLabel scrapes the current build label from the Gemini app page.
func (c *Client) fetchBuildLabel(ctx context.Context) string {
	html, err := c.fetchAppPageCached(ctx, appPageMaxAge)
	if err != nil {
		c.log.Warn("build label fetch failed", "err", err)
		return ""
	}
	if m := buildLabelRe.FindSubmatch(html); m != nil {
		return string(m[1])
	}
	return ""
}

// accountPrefix returns the /u/N path segment for non-default Google accounts.
func (c *Client) accountPrefix() string {
	if c.cfg.AuthUser == "" {
		return ""
	}
	return "/u/" + c.cfg.AuthUser
}

// Outcomes of an /app auth check (authCheck.result).
const (
	authOK          = "authenticated"
	authAnonymous   = "anonymous"
	authBlocked     = "blocked"
	authFetchFailed = "fetch_failed"
	authNoToken     = "not_authenticated" // page served without a session token
)

// authCheck records the last /app auth check: when, its outcome, and the cookie
// generation it was made with (so a dead cookie is not re-checked until it
// changes).
type authCheck struct {
	at     time.Time
	result string
	gen    uint64
}

func (c *Client) recordAuthCheck(result string, gen uint64) {
	c.atMu.Lock()
	c.auth = authCheck{at: time.Now(), result: result, gen: gen}
	c.atMu.Unlock()
}

func (c *Client) lastAuthCheck() authCheck {
	c.atMu.Lock()
	defer c.atMu.Unlock()
	return c.auth
}

// CheckAuth reports whether the configured cookie establishes a signed-in Gemini
// session, detected via the SNlM0e token (issued only to authenticated sessions).
// It returns a short human-readable status and logs a warning when a cookie is
// present but does not authenticate. The token value itself is never logged.
func (c *Client) CheckAuth(ctx context.Context) string {
	cookie, _ := c.loadCookie()
	gen := c.cookieGeneration()
	if cookie == "" {
		c.recordAuthCheck(authAnonymous, gen)
		return "anonymous"
	}
	html, err := c.fetchAppPageCached(ctx, appPageMaxAge)
	if err != nil {
		c.log.Warn("cookie auth check failed", "err", err)
		if errors.Is(err, ErrGoogleBlocked) {
			c.recordAuthCheck(authBlocked, gen)
			return "unknown (blocked by Google)"
		}
		c.recordAuthCheck(authFetchFailed, gen)
		return "unknown (fetch failed)"
	}
	c.applyBuildLabel(html)
	if m := snlm0eRe.FindSubmatch(html); m != nil {
		c.setAt(string(m[1])) // authenticated requests must echo this XSRF token back
		c.recordAuthCheck(authOK, gen)
		c.saveState()
		c.log.Info("gemini cookie authenticated")
		return "authenticated"
	}
	c.setAt("")
	c.recordAuthCheck(authNoToken, gen)
	c.saveState()
	c.log.Warn("gemini cookie is NOT authenticating — requests run anonymously",
		"hint", "the session needs __Secure-1PSIDTS (it expires; re-export the cookie)")
	return "NOT authenticated"
}

// currentAt returns the XSRF "at" token to send with StreamGenerate: the SNlM0e
// value scraped from the signed-in page (at startup, or after a session error of a
// generation), or restored from the state file. Empty for anonymous sessions,
// which need no token.
func (c *Client) currentAt() string {
	c.atMu.Lock()
	defer c.atMu.Unlock()
	return c.atToken
}

// setAt caches the scraped SNlM0e XSRF token.
func (c *Client) setAt(token string) {
	c.atMu.Lock()
	c.atToken = token
	c.atAt = time.Time{}
	if token != "" {
		c.atAt = time.Now()
	}
	c.atMu.Unlock()
}

// Authenticated reports whether the cookie established a signed-in session (an
// XSRF "at" token is held). It exposes no secret.
func (c *Client) Authenticated() bool {
	_, _ = c.loadCookie()
	return c.currentAt() != ""
}

// LastCookieRotation returns the time of the last successful cookie rotation, or
// the zero time if none happened in this process.
func (c *Client) LastCookieRotation() time.Time {
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	return c.rotatedAt
}

// DefaultModelName returns the name of the model used when a request names none.
func (c *Client) DefaultModelName() string {
	if m := c.defaultModel(); m != nil {
		return m.Name
	}
	return c.cfg.DefaultModel
}

// ImagesSupported reports whether image attachments can be used. They require an
// authenticated session (a non-empty XSRF "at" token); anonymous sessions — and
// sessions whose cookie no longer authenticates — cannot attach images.
func (c *Client) ImagesSupported() bool {
	return c.currentAt() != ""
}

// reauthMinInterval is the minimum spacing of session reloads triggered by a
// generation error (a var so tests can shrink it). It bounds the /app loads a
// systematic 400/401 could cause to one per interval.
var reauthMinInterval = 10 * time.Minute

// reloadSession reloads the /app page once after a session error of a
// generation, to pick up a fresh XSRF token and build label (AUDIT-13 A6). It
// returns true when a new token was obtained, i.e. one new attempt is
// worthwhile. It makes no call when no cookie is configured, when the previous
// reload is younger than reauthMinInterval, or while a gemini.google.com block
// is in force.
func (c *Client) reloadSession(ctx context.Context) bool {
	if cookie, _ := c.loadCookie(); cookie == "" {
		return false
	}
	c.atMu.Lock()
	recent := !c.reauthAt.IsZero() && time.Since(c.reauthAt) < reauthMinInterval
	if !recent {
		c.reauthAt = time.Now()
	}
	c.atMu.Unlock()
	if recent {
		c.log.Warn("session reload skipped: previous reload too recent", "min_interval", reauthMinInterval)
		return false
	}
	if !c.AcquireBackground(FamilyApp, "session_reload") {
		c.log.Warn("session reload skipped: google block backoff in force")
		return false
	}
	c.invalidateAppPage()
	status := c.CheckAuth(ctx)
	if status != "authenticated" {
		c.log.Warn("session reload after an authentication error did not restore the session", "status", status)
		return false
	}
	c.log.Info("gemini session reloaded after an authentication error")
	return true
}

// newRequest assembles a POST request with all required headers and auth. When a
// model with a known id is supplied, the four model-selection headers are set
// (reqUUID must match inner[59] of the body). A nil model or an empty model id
// (the fallback/anonymous case) omits them so the request uses the account default.
func (c *Client) newRequest(ctx context.Context, body string, model *AvailableModel, reqUUID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.streamURL(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	prefix := c.accountPrefix()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", geminiBaseURL)
	req.Header.Set("Referer", geminiBaseURL+prefix+"/app")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("User-Agent", userAgent)
	if prefix != "" {
		req.Header.Set("X-Goog-AuthUser", c.cfg.AuthUser)
	}
	if model != nil && model.ModelID != "" {
		for k, v := range model.headers(reqUUID) {
			req.Header.Set(k, v)
		}
	}

	cookie, sapisid := c.loadCookie()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if sapisid != "" {
		req.Header.Set("Authorization", makeSapisidHash(sapisid))
	}
	return req, nil
}

// Typed upstream status errors (AUDIT-13 A8). Use errors.Is; the concrete type
// is *UpstreamStatusError, which carries the HTTP status.
var (
	// ErrSessionAuth: Gemini refused the session — HTTP 401/403, or HTTP 400 on a
	// request that carried an XSRF "at" token (the observed symptom of an expired
	// token; see statusError). Never retried as is.
	ErrSessionAuth = errors.New("gemini refused the session (authentication: cookie or XSRF token)")
	// ErrUpstreamServer: HTTP 5xx from Gemini (retried like a transport error).
	ErrUpstreamServer = errors.New("gemini server error")
	// ErrUpstreamRejected: any other non-2xx status (other 4xx, unexpected 3xx).
	// Never retried.
	ErrUpstreamRejected = errors.New("gemini rejected the request")
)

// UpstreamStatusError is a non-2xx answer of a generation call. It holds the
// status only (never the body, which may echo the request).
type UpstreamStatusError struct {
	Status int
	Kind   error // ErrSessionAuth | ErrUpstreamServer | ErrUpstreamRejected
}

func (e *UpstreamStatusError) Error() string { return fmt.Sprintf("%v (http %d)", e.Kind, e.Status) }
func (e *UpstreamStatusError) Unwrap() error { return e.Kind }

// statusError classifies the HTTP status of a generation response; nil for 2xx.
//
// Choice for 400: the web endpoint answers 400 when the "at" XSRF token no longer
// matches the session (LU in the reference clients; never OBSERVÉ here). A 400 on
// a request that carried a token is therefore treated as a session error, which
// allows ONE /app reload + ONE new attempt (rate-limited, see reloadSession). A
// 400 without a token cannot be a token problem and is a plain rejection.
func statusError(status int, hadAt bool) error {
	var kind error
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = ErrSessionAuth
	case status == http.StatusBadRequest && hadAt:
		kind = ErrSessionAuth
	case status >= 500:
		kind = ErrUpstreamServer
	default:
		kind = ErrUpstreamRejected
	}
	return &UpstreamStatusError{Status: status, Kind: kind}
}

// statusKind names the class of err for logs.
func statusKind(err error) string {
	switch {
	case errors.Is(err, ErrSessionAuth):
		return "session_auth"
	case errors.Is(err, ErrUpstreamServer):
		return "server"
	}
	return "rejected"
}

// retryable reports whether a failed attempt may be retried as is: transport
// errors, read errors, 5xx and empty 200 answers. Never a 4xx, a block or a
// session error.
// Retryable reports whether a failed generation may be attempted again.
func Retryable(err error) bool { return retryable(err) }

func retryable(err error) bool {
	var se *UpstreamStatusError
	if errors.As(err, &se) {
		return se.Kind == ErrUpstreamServer
	}
	return !errors.Is(err, ErrGoogleBlocked) && !errors.Is(err, ErrResponseTooLarge)
}

// checkGenerateStatus turns a non-2xx generation response into a typed error,
// draining and closing its body and logging one metadata-only line.
func (c *Client) checkGenerateStatus(resp *http.Response, hadAt bool, attempt int) error {
	err := statusError(resp.StatusCode, hadAt)
	if err == nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	c.log.Warn("upstream_status", "status", resp.StatusCode, "kind", statusKind(err),
		"attempt", attempt+1, "at_present", hadAt)
	return err
}

// Generate sends a prompt and returns the final response text (blocking). A
// transport error, a 5xx or an empty 200 is retried up to RetryAttempts; a 4xx,
// a block or a cancellation is not. A session error (401/403, or 400 with a
// token) triggers at most one session reload followed by one new attempt.
func (c *Client) Generate(ctx context.Context, p GenParams) (string, error) {
	c.beginUserActivity()
	defer c.endUserActivity()
	images := c.uploadImages(ctx, p.Images)
	met := metricsFrom(ctx)
	start := time.Now()
	text, err := c.generateAttempts(ctx, p, images, met, start, c.cfg.RetryAttempts, true)
	if errors.Is(err, ErrSessionAuth) && c.reloadSession(ctx) {
		text, err = c.generateAttempts(ctx, p, images, met, start, 1, false)
	}
	return text, err
}

// generateAttempts runs up to attempts blocking generations. first marks the
// initial call (request metrics and parameter log are recorded once).
func (c *Client) generateAttempts(ctx context.Context, p GenParams, images []uploadedImage, met *CallMetrics, start time.Time, attempts int, first bool) (string, error) {
	_, _ = c.loadCookie()
	// The body carries the XSRF token: rebuild it for each series of attempts so a
	// reloaded token is used.
	body, reqUUID, err := c.buildBody(p, images)
	if err != nil {
		return "", err
	}
	hadAt := c.currentAt() != ""
	if first {
		met.request(len(p.Prompt), len(body))
		c.logUpstreamParams(p)
	}
	if ctx.Err() != nil { // cancelled while uploading images: do not call Gemini
		return "", c.clientCancelled(ctx, met, start)
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := c.newRequest(ctx, body, p.Model, reqUUID)
		if err != nil {
			return "", err
		}
		n := attempt
		if !first {
			n = 1 // a post-reload attempt counts as a retry
		}
		met.attempt(n)
		t0 := time.Now()
		resp, err := c.http.Do(req)
		err = cleanErr(err)
		if err == nil {
			met.firstByte(t0)
			if err = c.checkGenerateStatus(resp, hadAt, attempt); err != nil {
				met.upstream(t0, 0)
			} else {
				raw, readErr := readLimited(resp.Body, maxResponseBytes)
				resp.Body.Close()
				met.upstream(t0, len(raw))
				if errors.Is(readErr, ErrResponseTooLarge) {
					return "", readErr // deterministic: retrying would not help
				}
				switch {
				case readErr != nil:
					err = readErr
				default:
					// A 200 without text is treated as a (retryable) failure rather than a
					// silent empty answer; only this case carries the size-limit hint.
					if text := extractResponseText(string(raw)); text != "" {
						scan := &idScan{}
						shape := newFrameShape()
						elapsed := time.Since(t0)
						var rawLines []string
						for _, line := range strings.Split(string(raw), "\n") {
							if line == "" {
								continue
							}
							rawLines = append(rawLines, line)
							scan.add(line)
							if isHeartbeatFrame(line) {
								met.heartbeat()
							}
							if fr, ok := parseFrame(line); ok {
								shape.add(fr, elapsed)
							}
						}
						c.writeTrace(met, p, traceResult{
							Text: text, Start: t0,
							Frames: shape.frames, FirstTextMS: shape.firstTextMS, LastFrameMS: shape.lastFrameMS,
							Raw: rawLines,
						})
						rec := newCallRecord(met, p, t0)
						rec.Frames = shape.frames
						rec.TextBytes, rec.ThoughtBytes = len(text), shape.thought
						rec.TextTokens = EstimateTokensForText(text)
						rec.ThoughtTokens = EstimateTokensForBytes(shape.thought)
						rec.FirstTextMS, rec.FirstThoughtMS = shape.firstTextMS, shape.firstThoughtMS
						scan.cannedError = IsCannedErrorReply(text, scan.errorMarker)
						c.recordCall(rec, scan)
						return text, nil
					}
					err = ErrEmptyResponse
				}
			}
		} else {
			met.upstream(t0, 0) // transport error: time spent before failing
		}
		lastErr = err
		if errors.Is(err, ErrGoogleBlocked) {
			// Retrying only feeds the anti-bot: fail fast, the backoff takes over.
			return "", fmt.Errorf("gemini request failed: %w", err)
		}
		if ctx.Err() != nil {
			// The caller is gone: never retry, never sleep RetryDelaySec.
			return "", c.clientCancelled(ctx, met, start)
		}
		if !retryable(err) {
			return "", fmt.Errorf("gemini request failed: %w", err)
		}
		if attempt < attempts-1 {
			c.log.Warn("retrying gemini request",
				"attempt", attempt+1, "max", attempts, "err", err)
			wait := time.Duration(c.cfg.RetryDelaySec) * time.Second
			w0 := time.Now()
			select {
			case <-ctx.Done():
				return "", c.clientCancelled(ctx, met, start)
			case <-time.After(wait):
			}
			met.retryWait(time.Since(w0))
		}
	}
	// ErrEmptyResponse is already self-descriptive; don't double-wrap it.
	if errors.Is(lastErr, ErrEmptyResponse) {
		return "", ErrEmptyResponse
	}
	return "", fmt.Errorf("gemini request failed: %w", lastErr)
}

// clientCancelled logs one metadata-only "client_cancelled" WARN line for a
// generation abandoned because ctx ended (client disconnect, deadline or shutdown
// abort) and returns the context error.
func (c *Client) clientCancelled(ctx context.Context, met *CallMetrics, start time.Time) error {
	attrs := []any{"elapsed_ms", time.Since(start).Milliseconds(), "cause", context.Cause(ctx)}
	if met != nil {
		attrs = append(attrs,
			"upstream_attempts", met.Attempts.Load(), "upstream_retries", met.Retries.Load(),
			"first_byte_ms", met.FirstByteMS.Load(), "upstream_ms", met.UpstreamMS.Load(),
			"upstream_req_bytes", met.ReqBytes.Load(), "upstream_resp_bytes", met.RespBytes.Load())
	}
	c.log.Warn("client_cancelled", attrs...)
	return ctx.Err()
}

// StreamHandlers receives the incremental output of a streamed generation. Either
// handler may be nil.
type StreamHandlers struct {
	Text         func(delta string)                // answer text
	Thought      func(delta string)                // model "thinking" text, when the backend streams it
	Status       func(text string)                 // Google progress status from body[2]["7"] (K11)
	InternalTool func(name string)                 // internal Google tool detected in body[11]/body[24][0] (B1)
	Body24Tools  func(names []string)              // all tool names in body[24][0], including local tools (K17 S3)
	LimitBanner  func(msg string)                  // explicit Google quota/limit banner from body[52] (B1)
	Identity     func(servedID, servedName string) // reported as soon as position 39 is read
	ErrorMarker  func()                            // reported when inner[31]=true is read
	Opened       func()                            // upstream HTTP 200 stream opened (N4)
	Heartbeat    func()                            // Google wait heartbeat frame [{"37":[n]}] received (N4)
	Candidate    func()                            // at least one candidate frame inner[4] was received (K16 B2)
	Progress     func(textBytes, thoughtBytes int) // incremental bytes received and heartbeat
}

// GenerateStream sends a prompt and invokes emit for each incremental text delta.
// Gemini sends cumulative text per chunk, so deltas are computed against prevText.
// A session error answered before any byte triggers at most one session reload
// followed by one new attempt; nothing is retried once streaming has begun.
func (c *Client) GenerateStream(ctx context.Context, p GenParams, emit func(delta string)) error {
	return c.GenerateStreamEvents(ctx, p, StreamHandlers{Text: emit})
}

// GenerateStreamEvents is GenerateStream with separate handlers for the answer
// text and the thinking text.
func (c *Client) GenerateStreamEvents(ctx context.Context, p GenParams, h StreamHandlers) error {
	c.beginUserActivity()
	defer c.endUserActivity()
	images := c.uploadImages(ctx, p.Images)
	err := c.streamOnce(ctx, p, images, h, 0)
	if errors.Is(err, ErrSessionAuth) && c.reloadSession(ctx) {
		err = c.streamOnce(ctx, p, images, h, 1)
	}
	return err
}

// streamOnce performs one streaming generation attempt (n: 0 first, 1 after a
// session reload).
func (c *Client) streamOnce(ctx context.Context, p GenParams, images []uploadedImage, h StreamHandlers, n int) error {
	_, _ = c.loadCookie()
	body, reqUUID, err := c.buildBody(p, images)
	if err != nil {
		return err
	}
	hadAt := c.currentAt() != ""
	req, err := c.newRequest(ctx, body, p.Model, reqUUID)
	if err != nil {
		return err
	}
	met := metricsFrom(ctx)
	if n == 0 {
		met.request(len(p.Prompt), len(body))
		c.logUpstreamParams(p)
	}
	met.attempt(n)
	t0 := time.Now()
	respBytes := 0
	shape := newFrameShape()
	prevText, prevThought := "", ""
	var runErr error
	scan := &idScan{}
	var rawLines []string
	var rawTimesMS []int64
	rawBytes := 0
	sent := false
	defer func() {
		met.upstream(t0, respBytes)
		c.logFrameShape(shape)
		c.writeTrace(met, p, traceResult{Stream: true, Text: prevText, Thought: prevThought, Err: runErr,
			Start: t0, Frames: shape.frames, FirstTextMS: shape.firstTextMS, LastFrameMS: shape.lastFrameMS, Raw: rawLines, RawTimesMS: rawTimesMS})
		if !sent {
			return // nothing was asked of Gemini (cancelled before the call)
		}
		rec := newCallRecord(met, p, t0)
		rec.Stream, rec.Frames = true, shape.frames
		rec.FirstTextMS, rec.FirstThoughtMS = shape.firstTextMS, shape.firstThoughtMS
		rec.TextBytes, rec.ThoughtBytes = len(prevText), len(prevThought)
		rec.TextTokens, rec.ThoughtTokens = EstimateTokensForText(prevText), EstimateTokensForText(prevThought)
		if runErr != nil {
			if errors.Is(runErr, ErrEarlyStopAborted) {
				rec.Error = "early_stop"
			} else if errors.Is(runErr, ErrInternalToolRetry) {
				rec.Error = "internal_tool_retry"
			} else {
				rec.Error = "stream_interrupted"
			}
		} else if prevText == "" {
			rec.Error = "empty"
		} else {
			scan.cannedError = IsCannedErrorReply(prevText, scan.errorMarker)
		}
		c.recordCall(rec, scan)
	}()
	earlyCause := func() (error, bool) {
		cause := context.Cause(ctx)
		if errors.Is(cause, ErrEarlyStop) {
			return nil, true
		}
		if errors.Is(cause, ErrEarlyStopAborted) {
			return ErrEarlyStopAborted, true
		}
		if errors.Is(cause, ErrInternalToolRetry) {
			return ErrInternalToolRetry, true
		}
		return nil, false
	}
	if ctx.Err() != nil {
		if err, ok := earlyCause(); ok {
			runErr = err
			return err
		}
		return c.clientCancelled(ctx, met, t0)
	}
	sent = true
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			if cerr, ok := earlyCause(); ok {
				runErr = cerr
				return cerr
			}
			runErr = c.clientCancelled(ctx, met, t0)
			return runErr
		}
		runErr = fmt.Errorf("gemini request failed: %w", cleanErr(err))
		return runErr
	}
	if err := c.checkGenerateStatus(resp, hadAt, n); err != nil {
		runErr = fmt.Errorf("gemini request failed: %w", err)
		return runErr
	}
	defer resp.Body.Close()
	if h.Opened != nil {
		h.Opened()
	}

	// ReadString grows its buffer as needed, unlike bufio.Scanner which caps at 64KB;
	// batchexecute lines routinely exceed that.
	reader := bufio.NewReader(resp.Body)
	emitted, thoughtEmitted := false, false
	identEmitted, errMarkerEmitted, candidateEmitted := false, false, false
	for {
		if ctx.Err() != nil {
			if cerr, ok := earlyCause(); ok {
				runErr = cerr
				return cerr
			}
			runErr = c.clientCancelled(ctx, met, t0)
			return runErr
		}
		line, readErr := reader.ReadString('\n')
		if line != "" {
			respBytes += len(line)
			met.firstByte(t0)
			scan.add(line)
			if scan.servedID != "" && !identEmitted {
				identEmitted = true
				if h.Identity != nil {
					h.Identity(scan.servedID, scan.servedName)
				}
			}
			if scan.errorMarker && !errMarkerEmitted {
				errMarkerEmitted = true
				if h.ErrorMarker != nil {
					h.ErrorMarker()
				}
			}
			if c.cfg.TraceRaw && c.cfg.TraceDir != "" && rawBytes < traceRawMax {
				rawBytes += len(line)
				rawLines = append(rawLines, line)
				rawTimesMS = append(rawTimesMS, time.Since(t0).Milliseconds())
			}
			if isHeartbeatFrame(line) {
				met.heartbeat()
				if h.Heartbeat != nil {
					h.Heartbeat()
				}
			}
			fr, ok := parseFrame(line)
			if ok {
				shape.add(fr, time.Since(t0))
				if fr.hasCandidate && !candidateEmitted {
					candidateEmitted = true
					if h.Candidate != nil {
						h.Candidate()
					}
				}
				if h.Progress != nil {
					h.Progress(len(prevText), len(prevThought))
				}
			}
			if ok && fr.status != "" && h.Status != nil {
				h.Status(fr.status)
			}
			if ok && fr.internalTool != "" && h.InternalTool != nil {
				h.InternalTool(fr.internalTool)
			}
			if ok && len(fr.body24Tools) > 0 && h.Body24Tools != nil {
				h.Body24Tools(fr.body24Tools)
			}
			if ok && fr.limitBanner != "" && h.LimitBanner != nil {
				h.LimitBanner(fr.limitBanner)
			}
			if ok && len(fr.thought) > len(prevThought) {
				delta := runeDelta(prevThought, fr.thought)
				prevThought = fr.thought
				if !thoughtEmitted {
					delta = strings.TrimLeft(delta, " \t\r\n")
				}
				if delta != "" && h.Thought != nil {
					thoughtEmitted = true
					h.Thought(delta)
				}
			}
			if ok && fr.text != "" && len(fr.text) > len(prevText) {
				// Whitespace at a chunk boundary is content: only artifacts are removed,
				// and only the very first emitted text is left-trimmed.
				delta := cleanGeminiDelta(runeDelta(prevText, fr.text))
				prevText = fr.text
				if !emitted {
					delta = strings.TrimLeft(delta, " \t\r\n")
				}
				if delta != "" && h.Text != nil {
					emitted = true
					h.Text(delta)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				if cerr, ok := earlyCause(); ok {
					runErr = cerr
					return cerr
				}
				runErr = c.clientCancelled(ctx, met, t0)
				return runErr
			}
			runErr = readErr
			return readErr
		}
	}
}

// runeDelta returns the part of cur that follows the longest rune-aligned prefix
// it shares with prev. Gemini streams cumulative snapshots, so cur is normally a
// byte-prefix extension of prev and the delta is simply its tail; but if a
// snapshot revises earlier text (so prev is not a byte-prefix of cur), slicing at
// len(prev) could land in the middle of a multi-byte rune. Diffing on rune
// boundaries keeps every emitted delta valid UTF-8.
func runeDelta(prev, cur string) string {
	if strings.HasPrefix(cur, prev) {
		// Fast path: prev ends on a rune boundary within cur, so the tail is safe.
		return cur[len(prev):]
	}
	i := 0
	for i < len(prev) && i < len(cur) {
		rp, size := utf8.DecodeRuneInString(prev[i:])
		rc, _ := utf8.DecodeRuneInString(cur[i:])
		if rp != rc {
			break
		}
		i += size
	}
	return cur[i:]
}

// extractResponseText parses a full StreamGenerate response and returns the final
// text. Gemini streams cumulative snapshots, so the longest candidate is the
// complete answer (more robust than picking the last non-empty one).
func extractResponseText(raw string) string {
	longest := ""
	for _, line := range strings.Split(raw, "\n") {
		for _, t := range parseWrbLine(line) {
			if len(t) > len(longest) {
				longest = t
			}
		}
	}
	return cleanGeminiText(longest)
}

// latestText returns the longest text candidate from a single streamed line.
func latestText(line string) (string, bool) {
	texts := parseWrbLine(line)
	best := ""
	for _, t := range texts {
		if len(t) > len(best) {
			best = t
		}
	}
	return best, best != ""
}

// isHeartbeatFrame reports whether a streamed line is a Google wait-heartbeat
// frame of the form [["wrb.fr",null,"[{\"37\":[n]}]"]].
func isHeartbeatFrame(line string) bool {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) > 256 || !strings.Contains(line, "37") {
		return false
	}
	var arr []any
	if json.Unmarshal([]byte(line), &arr) != nil {
		return false
	}
	first, ok := indexSlice(arr, 0)
	if !ok {
		return false
	}
	innerStr, ok := indexString(first, 2)
	if !ok || len(innerStr) < 2 {
		return false
	}
	var inner []any
	if json.Unmarshal([]byte(innerStr), &inner) != nil {
		return false
	}
	for _, elem := range inner {
		if m, ok := elem.(map[string]any); ok {
			if _, has37 := m["37"]; has37 {
				return true
			}
		}
	}
	return false
}

// wrbInner2 decodes a batchexecute "wrb.fr" response line and returns its
// parsed inner JSON array (body). Status frames in body[2] can be shorter than
// 200 bytes, so only a minimal length guard is applied.
func wrbInner2(line string) []any {
	if !strings.Contains(line, `"wrb.fr"`) || len(line) < 40 {
		return nil
	}
	var arr []any
	if err := json.Unmarshal([]byte(line), &arr); err != nil {
		return nil
	}
	first, ok := indexSlice(arr, 0)
	if !ok {
		return nil
	}
	innerStr, ok := indexString(first, 2)
	if !ok || len(innerStr) < 2 {
		return nil
	}
	var inner2 any
	if err := json.Unmarshal([]byte(innerStr), &inner2); err != nil {
		return nil
	}
	inner2Slice, _ := inner2.([]any)
	return inner2Slice
}

// wrbCandidates decodes a batchexecute "wrb.fr" response line and returns its
// candidate list at inner2[4].
func wrbCandidates(line string) []any {
	inner2Slice := wrbInner2(line)
	if len(inner2Slice) <= 4 {
		return nil
	}
	parts, _ := inner2Slice[4].([]any)
	return parts
}

// parseWrbLine extracts the text strings of a "wrb.fr" response line: the model
// text is nested at inner2[4][*][1][*].
func parseWrbLine(line string) []string {
	var out []string
	for _, part := range wrbCandidates(line) {
		ps, ok := part.([]any)
		if !ok || len(ps) <= 1 {
			continue
		}
		texts, ok := ps[1].([]any)
		if !ok {
			continue
		}
		for _, t := range texts {
			if s, ok := t.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// thoughtIndex is where the first candidate is expected to carry the model's
// thinking text (candidate[37][0][0]). HYPOTHESIS, not yet observed on this
// account: the frame-shape diagnostic (logFrameShape) reports, per candidate
// index, how many string bytes were seen, so the real location can be confirmed
// from one logged request without recording any content.
const thoughtIndex = 37

// frame is the useful content of one streamed line.
type frame struct {
	text         string
	thought      string
	hasCandidate bool        // true when inner2[4] carries >=1 candidate (K16 B2)
	status       string      // Google progress status from body[2]["7"] (K11)
	internalTool string      // internal Google tool from body[11] or body[24][0] (B1)
	body24Tools  []string    // all tool names in body[24][0] (K17 S3)
	limitBanner  string      // Google limit banner from body[52] (B1)
	sizes        map[int]int // first candidate: index → string bytes in that subtree
	thoughts     []int       // sizes of the strings found under the thinking slot, in order
}

// googleInternalTools lists tool/extension identifiers emitted by Gemini's own
// server-side tools in body[24][0] (as opposed to user-declared local tools).
var googleInternalTools = map[string]bool{
	"google":                true,
	"drive":                 true,
	"drive_mcp":             true,
	"docs_agent":            true,
	"context_service_agent": true,
	"retriever":             true,
	"gmail":                 true,
	"calendar":              true,
	"workspace":             true,
	"youtube":               true,
	"maps":                  true,
	"flights":               true,
	"hotels":                true,
}

// parseBody11Extension extracts the invoked Google extension name from body[11]
// (e.g. [[null, ["google", [null, null, "Google Search", ...], 5]]]).
func parseBody11Extension(inner2 []any) string {
	if len(inner2) <= 11 {
		return ""
	}
	cards, ok := inner2[11].([]any)
	if !ok {
		return ""
	}
	for _, card := range cards {
		entry, ok := card.([]any)
		if !ok || len(entry) < 2 {
			continue
		}
		ext, ok := entry[1].([]any)
		if !ok || len(ext) == 0 {
			continue
		}
		id, _ := ext[0].(string)
		if meta, ok := indexAny(ext, 1).([]any); ok {
			if label, ok := indexAny(meta, 2).(string); ok && strings.TrimSpace(label) != "" {
				return strings.TrimSpace(label)
			}
		}
		if strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// parseBody24ToolCalls extracts the first internal Google tool call from
// body[24][0] (e.g. [["retriever", "expand_skills", 2]]). Local tools declared
// by the client (such as run_command, read_file, manage_task) are ignored.
func parseBody24ToolCalls(inner2 []any) string {
	if len(inner2) <= 24 {
		return ""
	}
	b24, ok := inner2[24].([]any)
	if !ok || len(b24) == 0 {
		return ""
	}
	calls, ok := b24[0].([]any)
	if !ok {
		return ""
	}
	for _, c := range calls {
		entry, ok := c.([]any)
		if !ok || len(entry) == 0 {
			continue
		}
		name, _ := entry[0].(string)
		name = strings.TrimSpace(name)
		if name == "" || !googleInternalTools[strings.ToLower(name)] {
			continue
		}
		if action, ok := indexAny(entry, 1).(string); ok && strings.TrimSpace(action) != "" {
			return name + ":" + strings.TrimSpace(action)
		}
		return name
	}
	return ""
}

// parseBody24AllToolNames returns all tool names present in body[24][0],
// including user-declared local tools such as run_command (K17 S3).
func parseBody24AllToolNames(inner2 []any) []string {
	if len(inner2) <= 24 {
		return nil
	}
	b24, ok := inner2[24].([]any)
	if !ok || len(b24) == 0 {
		return nil
	}
	calls, ok := b24[0].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, c := range calls {
		entry, ok := c.([]any)
		if !ok || len(entry) == 0 {
			continue
		}
		if name, ok := entry[0].(string); ok {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// parseBody52Limit extracts Google's explicit quota/limit banner from body[52]
// (e.g. [["Limit reached. Continuing with Flash-Lite."], ...]).
func parseBody52Limit(inner2 []any) string {
	if len(inner2) <= 52 {
		return ""
	}
	b52, ok := inner2[52].([]any)
	if !ok || len(b52) == 0 {
		return ""
	}
	first, ok := b52[0].([]any)
	if !ok || len(first) == 0 {
		return ""
	}
	if msg, ok := first[0].(string); ok {
		return strings.TrimSpace(msg)
	}
	return ""
}

// parseBody2Status extracts Google's internal tool progress text from body[2]["7"]
// (e.g. {"7":[null,null,null,null,null,["Analyzing user requests"]]}). Any
// unexpected shape is ignored without error.
func parseBody2Status(inner2 []any) string {
	if len(inner2) <= 2 {
		return ""
	}
	m, ok := inner2[2].(map[string]any)
	if !ok {
		return ""
	}
	raw7, ok := m["7"]
	if !ok || raw7 == nil {
		return ""
	}
	strs := collectStatusStrings(raw7, 0, nil)
	if len(strs) == 0 {
		return ""
	}
	joined := strings.Join(strs, " — ")
	if len(joined) > 240 {
		joined = joined[:240]
	}
	return joined
}

func collectStatusStrings(v any, depth int, out []string) []string {
	if depth > 4 || len(out) >= 4 {
		return out
	}
	switch x := v.(type) {
	case string:
		if s := strings.TrimSpace(x); s != "" {
			out = append(out, s)
		}
	case []any:
		for _, e := range x {
			out = collectStatusStrings(e, depth+1, out)
			if len(out) >= 4 {
				break
			}
		}
	}
	return out
}

// parseFrame extracts the answer text, the thinking text, any body[2] progress
// status, and the size profile of the first candidate of a streamed line.
func parseFrame(line string) (frame, bool) {
	inner2 := wrbInner2(line)
	if len(inner2) == 0 {
		return frame{}, false
	}
	status := parseBody2Status(inner2)
	internalTool := parseBody11Extension(inner2)
	if t24 := parseBody24ToolCalls(inner2); t24 != "" && internalTool == "" {
		internalTool = t24
	}
	body24Tools := parseBody24AllToolNames(inner2)
	limitBanner := parseBody52Limit(inner2)
	var cands []any
	if len(inner2) > 4 {
		cands, _ = inner2[4].([]any)
	}
	if len(cands) == 0 && status == "" && internalTool == "" && limitBanner == "" && len(body24Tools) == 0 {
		return frame{}, false
	}
	var fr frame
	fr.hasCandidate = len(cands) > 0
	fr.status = status
	fr.internalTool = internalTool
	fr.body24Tools = body24Tools
	fr.limitBanner = limitBanner
	for _, part := range cands {
		ps, ok := part.([]any)
		if !ok {
			continue
		}
		if texts, ok := indexAny(ps, 1).([]any); ok {
			for _, t := range texts {
				if s, ok := t.(string); ok && len(s) > len(fr.text) {
					fr.text = s
				}
			}
		}
		if fr.sizes == nil {
			fr.sizes = map[int]int{}
			for i, v := range ps {
				if n := stringBytes(v, 0); n > 0 {
					fr.sizes[i] = n
				}
			}
			if l1, ok := indexAny(ps, thoughtIndex).([]any); ok {
				if l2, ok := indexAny(l1, 0).([]any); ok {
					fr.thought, _ = indexAny(l2, 0).(string)
				}
				fr.thoughts = leafSizes(l1, 0, nil)
			}
		}
	}
	return fr, true
}

// stringBytes sums the lengths of the strings found in v (bounded depth).
func stringBytes(v any, depth int) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		if depth > 8 {
			return 0
		}
		n := 0
		for _, e := range x {
			n += stringBytes(e, depth+1)
		}
		return n
	}
	return 0
}

// leafSizes lists the sizes of the strings found in v, depth first (at most 16).
func leafSizes(v any, depth int, out []int) []int {
	if len(out) >= 16 || depth > 8 {
		return out
	}
	switch x := v.(type) {
	case string:
		return append(out, len(x))
	case []any:
		for _, e := range x {
			out = leafSizes(e, depth+1, out)
		}
	}
	return out
}

// frameShape accumulates a content-free profile of a streamed response: number of
// frames and, per index of the first candidate, the largest string volume seen.
type frameShape struct {
	frames  int
	text    int
	thought int
	max     map[int]int
	// Arrival times (ms since the request was sent; -1: never) and the profile of
	// the thinking slot in the last frame that carried one.
	firstThoughtMS, firstTextMS, lastFrameMS int64
	thoughts                                 []int
	times                                    []int64 // arrival of each frame (first 24)
}

func newFrameShape() *frameShape {
	return &frameShape{max: map[int]int{}, firstThoughtMS: -1, firstTextMS: -1}
}

func (s *frameShape) add(fr frame, at time.Duration) {
	ms := at.Milliseconds()
	s.frames++
	s.lastFrameMS = ms
	if len(s.times) < 24 {
		s.times = append(s.times, ms)
	}
	if fr.thought != "" && s.firstThoughtMS < 0 {
		s.firstThoughtMS = ms
	}
	if fr.text != "" && s.firstTextMS < 0 {
		s.firstTextMS = ms
	}
	if len(fr.thoughts) > 0 {
		s.thoughts = fr.thoughts
	}
	if len(fr.text) > s.text {
		s.text = len(fr.text)
	}
	if len(fr.thought) > s.thought {
		s.thought = len(fr.thought)
	}
	for i, n := range fr.sizes {
		if n > s.max[i] {
			s.max[i] = n
		}
	}
}

// logFrameShape emits one "upstream_frame_shape" line (sizes only, never content)
// when parameter logging is on.
func (c *Client) logFrameShape(s *frameShape) {
	if !c.cfg.LogParams || s.frames == 0 {
		return
	}
	idx := make([]int, 0, len(s.max))
	for i := range s.max {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	parts := make([]string, 0, len(idx))
	for _, i := range idx {
		parts = append(parts, fmt.Sprintf("%d=%d", i, s.max[i]))
	}
	c.log.Info("upstream_frame_shape", "frames", s.frames, "text_bytes", s.text,
		"thought_bytes", s.thought, "thought_index", thoughtIndex,
		"candidate_string_bytes", strings.Join(parts, ","),
		"first_thought_ms", s.firstThoughtMS, "first_text_ms", s.firstTextMS, "last_frame_ms", s.lastFrameMS,
		"frame_times_ms", fmt.Sprint(s.times), "thought_leaf_bytes", fmt.Sprint(s.thoughts))
}

// indexSlice returns arr[i] as a []any if possible.
func indexSlice(arr []any, i int) ([]any, bool) {
	if i < 0 || i >= len(arr) {
		return nil, false
	}
	s, ok := arr[i].([]any)
	return s, ok
}

// indexString returns arr[i] as a string if possible.
func indexString(arr []any, i int) (string, bool) {
	if i < 0 || i >= len(arr) {
		return "", false
	}
	s, ok := arr[i].(string)
	return s, ok
}

// cleanGeminiText removes code-execution and card-content artifacts and trims
// surrounding whitespace.
func cleanGeminiText(text string) string {
	text = codeArtifactRe.ReplaceAllString(text, "")
	text = cardContentRe.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// cleanGeminiDelta removes code-execution and card-content artifacts from a stream
// delta without trimming it (see GenerateStream).
func cleanGeminiDelta(text string) string {
	text = codeArtifactRe.ReplaceAllString(text, "")
	return cardContentRe.ReplaceAllString(text, "")
}

// ─── Cookie / SAPISIDHASH auth ───────────────────────────────────────────────

// loadCookie resolves the cookie string and its SAPISID (inline Cookie takes
// precedence over the cookie file). The SAPISID is parsed from the cookie string
// itself. An inline cookie is rotated in memory, so a rotated value (cached in
// cookieStr) is preferred over the original config value once available.
func (c *Client) loadCookie() (cookie, sapisid string) {
	if c.cfg.Cookie != "" {
		c.cookieMu.Lock()
		cached, cachedSapi := c.cookieStr, c.cookieSAPI
		c.cookieMu.Unlock()
		if cached != "" {
			return cached, cachedSapi
		}
		return c.cfg.Cookie, cookieValue(c.cfg.Cookie, cookieSAPISID)
	}
	if c.cfg.CookieFile == "" {
		return "", ""
	}
	cookie, sapisid, changed := c.loadCookieFile()
	c.maybeReloadExternalState(cookie, changed)
	return cookie, sapisid
}

// loadCookieFile reads the cookie file, caching the parsed result by mtime so
// the file is not re-read and re-parsed on every request.
func (c *Client) loadCookieFile() (cookie, sapisid string, changed bool) {
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()

	info, err := os.Stat(c.cfg.CookieFile)
	if err != nil {
		c.log.Error("cookie stat failed", "path", c.cfg.CookieFile, "err", err)
		return c.cookieStr, c.cookieSAPI, false // fall back to last good value
	}
	if c.cookieLoaded && info.ModTime().Equal(c.cookieMtime) {
		return c.cookieStr, c.cookieSAPI, false
	}

	data, err := os.ReadFile(c.cfg.CookieFile)
	if err != nil {
		c.log.Error("cookie load failed", "path", c.cfg.CookieFile, "err", err)
		return c.cookieStr, c.cookieSAPI, false
	}
	cookie = strings.TrimSpace(string(data))
	sapisid = cookieValue(cookie, cookieSAPISID)
	newHash := psidtsHash(cookie)
	if c.cookieLoaded && cookie != c.cookieStr {
		changed = true
		c.cookieGen++ // the file was rewritten (e.g. a re-exported cookie)
		c.rot.noNewTokenStreak = 0
		c.rot.suspended = false
		if newHash != c.psidtsHash {
			c.psidtsHash = newHash
			c.rotatedAt = time.Time{}
			if newHash != "" {
				c.psidtsAt = time.Now()
			} else {
				c.psidtsAt = time.Time{}
			}
		}
	} else if !c.cookieLoaded && c.psidtsHash == "" && newHash != "" {
		c.psidtsHash = newHash
	}
	c.cookieStr, c.cookieSAPI = cookie, sapisid
	c.cookieMtime, c.cookieLoaded = info.ModTime(), true
	return cookie, sapisid, changed
}

// cookieGeneration returns the cookie-jar generation (bumped on every change).
func (c *Client) cookieGeneration() uint64 {
	c.cookieMu.Lock()
	defer c.cookieMu.Unlock()
	return c.cookieGen
}

// makeSapisidHash builds the SAPISIDHASH Authorization header value.
func makeSapisidHash(sapisid string) string {
	ts := time.Now().Unix()
	sum := sha1.Sum(fmt.Appendf(nil, "%d %s %s", ts, sapisid, geminiBaseURL))
	return fmt.Sprintf("SAPISIDHASH %d_%x", ts, sum)
}
