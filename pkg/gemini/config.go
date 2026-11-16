package gemini

import "time"

// DefaultBuildLabel is the fallback Gemini web build label used when auto-resolution
// is disabled or fails. It is date-stamped and may go stale.
const DefaultBuildLabel = "boq_assistant-bard-web-server_20260529.02_p0"

// Defaults applied by New to any Config field left at its zero value, so a
// consumer can pass gemini.Config{} (optionally with just a cookie) and get a
// working client.
const (
	// DefaultModel is used when Config.DefaultModel is empty.
	DefaultModel = "gemini-3.5-flash"
	// DefaultRequestTimeout is the per-request HTTP timeout (seconds) used when
	// Config.RequestTimeout is zero.
	DefaultRequestTimeout = 180
	// DefaultRetryAttempts is the blocking-generate retry count used when
	// Config.RetryAttempts is zero. It must be at least 1 or Generate makes no
	// request at all.
	DefaultRetryAttempts = 3
	// DefaultRetryDelaySec is the delay between retries (seconds) used when
	// Config.RetryDelaySec is zero.
	DefaultRetryDelaySec = 2
)

// Config holds the upstream settings the Gemini web client needs. A consumer
// populates it directly; the proxy's main package builds it from environment
// variables. All fields are plain values with no smart defaults except GeminiBL,
// which falls back to DefaultBuildLabel inside New when left empty.
type Config struct {
	Proxy          string // optional HTTP/HTTPS proxy URL
	RequestTimeout int    // per-request timeout, seconds
	LogParams      bool   // log the upstream parameters sent to Gemini (no content), set from GEMINI_LOG_REQUESTS
	GeminiBL       string // build label (bl) sent to the backend
	GeminiBLAuto   bool   // auto-resolve the build label from the Gemini page (defaults on when GeminiBL is unset)
	AuthUser       string // /u/N account index for non-default Google accounts
	DefaultModel   string // model used when a request names none
	Cookie         string // inline cookie string
	CookieFile     string // path to a cookie file (raw "k=v; …" string)
	RetryAttempts  int    // blocking-generate retry attempts
	RetryDelaySec  int    // delay between retries, seconds
	// AllowRemoteImages permits fetching http(s) image URLs (default false). Fetches
	// refuse loopback/private/link-local destinations, limit redirects and size.
	AllowRemoteImages bool
	// TraceDir, when set, makes the client write one JSON file per generation with
	// the FULL prompt and reply (GEMINI_TRACE_DIR). Off by default: the files hold
	// conversation content. Never contains cookies, tokens or keys. 0700 dir, 0600 files.
	TraceDir string
	// TraceMaxFiles caps the number of trace files kept (oldest removed first).
	TraceMaxFiles int
	// TraceRaw also stores the raw response lines in each trace file (GEMINI_TRACE_RAW).
	TraceRaw bool
	// CallsFile is the content-free journal of generations (JSONL, 0600). Empty:
	// the journal lives in memory only.
	CallsFile string

	// BlockMaxBackoff is the top step of the anti-bot backoff (AUDIT-13 A9). Zero
	// keeps the built-in schedule (60 min at the top); the proxy sets it from
	// GEMINI_BLOCK_MAX_BACKOFF_MIN.
	BlockMaxBackoff time.Duration

	// StateFile persists the XSRF token, build label and catalog with their
	// timestamps (0600, atomic) so a restart need not load /app (AUDIT-13 A5).
	// Empty disables it; it is always disabled for an inline Cookie.
	StateFile string
	// StateMaxAge is how recent a persisted token must be to be reused at startup
	// (default DefaultStateMaxAge).
	StateMaxAge time.Duration

	// RotateSuspendAfter suspends RotateCookies calls after N consecutive
	// no_new_token responses until the cookie changes (R&D-14 R6).
	// Zero uses DefaultRotateSuspendAfter (3); negative disables suspension.
	RotateSuspendAfter int

	// TemporaryChat sets inner[45] = 1 in StreamGenerate payloads when true
	// (GEMINI_TEMPORARY_CHAT, default false; R&D-14 R2).
	TemporaryChat bool

	// CookieRotation enables startup and periodic RotateCookies calls
	// (GEMINI_COOKIE_ROTATION=on; default false = off, K14 P2).
	CookieRotation bool
}

// DefaultRotateSuspendAfter is the default consecutive no_new_token threshold.
const DefaultRotateSuspendAfter = 3

// applyDefaults fills any field left at its zero value with the corresponding
// Default* constant, so gemini.Config{} produces a usable client. When no explicit
// GeminiBL is given it also turns GeminiBLAuto on, so the date-stamped fallback
// label is kept current by auto-resolution rather than going stale; setting an
// explicit GeminiBL pins it and leaves auto-resolution off.
func (c *Config) applyDefaults() {
	if c.GeminiBL == "" {
		c.GeminiBL = DefaultBuildLabel
		c.GeminiBLAuto = true
	}
	// The numeric timing/retry fields treat any value below 1 as "unset": 0 is the
	// zero-value sentinel, and a negative value is a misconfiguration that would
	// otherwise misbehave — a negative RequestTimeout makes http.Client time out
	// immediately, and a negative RetryAttempts makes Generate's loop run zero times
	// and wrap a nil error. Clamp all of them to their defaults.
	if c.RequestTimeout < 1 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.RetryAttempts < 1 {
		c.RetryAttempts = DefaultRetryAttempts
	}
	if c.RetryDelaySec < 1 {
		c.RetryDelaySec = DefaultRetryDelaySec
	}
	if c.DefaultModel == "" {
		c.DefaultModel = DefaultModel
	}
	if c.StateMaxAge <= 0 {
		c.StateMaxAge = DefaultStateMaxAge
	}
	if c.RotateSuspendAfter == 0 {
		c.RotateSuspendAfter = DefaultRotateSuspendAfter
	}
}
