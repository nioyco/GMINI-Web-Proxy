package main

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

const version = "0.1.0"

// defaultListen is the default bind address (localhost only) used when GEMINI_LISTEN
// is unset.
const defaultListen = "127.0.0.1:8081"

// Config holds all runtime settings. It is populated entirely from environment
// variables (optionally seeded from a .env file); there is no JSON config file.
// The upstream Gemini settings live in the embedded gemini.Config; the rest are
// the proxy's own network/CLI concerns.
type Config struct {
	Listen         string // bind address, e.g. 127.0.0.1:8081 or :8081
	LogRequests    bool   // log each request at Info level
	LogFormat      string // "text" (default) or "json" (GEMINI_LOG_FORMAT)
	CookieRotation bool   // GEMINI_COOKIE_ROTATION: off (default) or on
	CookieRefresh  int    // minutes between __Secure-1PSIDTS rotations (0 = off)
	// CookieAutoRefreshMin sets the token-age interval in minutes (default 90 if
	// unset or <=0) used once auto-refresh is turned on via the UI toggle; it
	// never enables auto-refresh on startup. CookieRefreshCmd is the external
	// command executed without a shell.
	CookieAutoRefreshMin int
	CookieRefreshCmd     string
	APIKeys              []string // allowed client keys; empty means open access
	// SSEKeepaliveSec is the interval (seconds) between ": keepalive" SSE comments
	// sent while a stream+tools request waits for the blocking generation (0 = off,
	// which restores the legacy "single chunk after generation" behaviour).
	SSEKeepaliveSec int
	// ShutdownGraceSec is how long in-flight requests may finish on SIGINT/SIGTERM
	// before being aborted (GEMINI_SHUTDOWN_GRACE_SEC, default 30).
	ShutdownGraceSec int
	// CORSOrigins lists origins allowed to make cross-origin browser calls
	// (GEMINI_CORS_ORIGINS, comma-separated). Empty = no CORS headers at all.
	CORSOrigins []string
	// AllowNoAuth lets the proxy start with no API key (GEMINI_ALLOW_NO_AUTH=1).
	AllowNoAuth bool
	// DisableEchoFilter turns the history-echo output filter off
	// (GEMINI_STRIP_HISTORY_ECHO=0|false|off; emergency switch, default: filter on).
	DisableEchoFilter bool
	// DisableToolPromptRules removes the extra tool-prompt rules added in P6/P10
	// (GEMINI_TOOL_PROMPT_NOECHO_RULES=0|false|off), restoring the original prompt.
	DisableToolPromptRules bool
	// ToolPromptReminder appends a short 3-line reminder just before the last
	// user request when tools are active (GEMINI_TOOL_PROMPT_REMINDER, default true).
	ToolPromptReminder bool
	// InternalToolAction controls how the proxy reacts when an internal Google tool
	// is detected (GEMINI_INTERNAL_TOOL_ACTION = notify | retry | off; default notify).
	InternalToolAction string
	// CannedErrorRetry controls whether Gemini's canned internal-error replies are
	// retried once identically (GEMINI_CANNED_ERROR_RETRY = off | once; default off),
	// and CannedErrorRetrySec bounds that retry when neither text nor thought arrives
	// (GEMINI_CANNED_ERROR_RETRY_SEC, default 25).
	CannedErrorRetry    string
	CannedErrorRetrySec int
	// DisableMarkerNeutralization stops escaping history markers ([User]:, ...) found
	// in input contents (GEMINI_NEUTRALIZE_HISTORY_MARKERS=0|false|off; default: on).
	DisableMarkerNeutralization bool
	// KeepTextAfterToolCall keeps the text a model writes after a tool call
	// (GEMINI_DROP_TEXT_AFTER_TOOL_CALL=0; default: that text is dropped).
	KeepTextAfterToolCall bool
	// UIEnabled serves the local management page at /_ui (GEMINI_UI, default on;
	// loopback clients only). AntigravityModelsFile is the Antigravity custom
	// models file shown there (GEMINI_ANTIGRAVITY_MODELS_FILE; "off" hides it).
	UIEnabled bool
	// WarnSuspectFallback adds a note (as reasoning text) to a streamed reply that
	// probably came from another model (GEMINI_WARN_SUSPECT_FALLBACK, default on),
	// at most once per WarnSuspectCooldownMin minutes per model unless the verdict
	// changes (GEMINI_WARN_SUSPECT_COOLDOWN_MIN, default 10; <0 disables rate limit).
	WarnSuspectFallback    bool
	WarnSuspectCooldownMin int
	AntigravityModelsFile  string
	// StreamToolsProgress forwards text and thoughts as they arrive on the
	// stream+tools chat path instead of one final chunk (GEMINI_STREAM_TOOLS_PROGRESS,
	// default on; 0 restores the buffered behaviour).
	StreamToolsProgress bool
	// StreamThoughts forwards the model's thinking text as reasoning_content
	// (GEMINI_STREAM_THOUGHTS, default on).
	StreamThoughts bool
	// SilenceWarningSec warns in stream thoughts if thoughts have arrived but no
	// answer text follows for this duration (GEMINI_SILENCE_WARN_SEC, default 45).
	SilenceWarningSec int
	// NoContentSec cancels an open upstream stream after N seconds when no
	// thought, text, or body[2]["7"] status frame arrived (GEMINI_NO_CONTENT_SEC,
	// default 0 = disabled; emits a wait note instead).
	NoContentSec int
	// MaxHeldBytes and MaxHeldSec cap a retained stream+tools response when no
	// runaway marker appears (GEMINI_MAX_HELD_BYTES default 256 KiB, GEMINI_MAX_HELD_SEC
	// default 120 s; 0 disables via -1).
	MaxHeldBytes int
	MaxHeldSec   int
	// DefaultThinkingLevel is the effort applied when a request names none
	// (GEMINI_THINKING_LEVEL = low | medium | high; empty = send nothing). EXPERIMENTAL.
	DefaultThinkingLevel string
	// ThinkingModes maps a model id (hex) to the web "mode" code sent with a
	// thinking level (GEMINI_THINKING_MODE_CODES, "id=code,id=code"). EXPERIMENTAL.
	ThinkingModes         map[string]int
	ThinkingModesExplicit bool
	// MaxPromptBytes refuses (413) a prompt larger than this (GEMINI_MAX_PROMPT_BYTES,
	// 0 = no limit). PromptWarnKB highlights prompt sizes in orange above N KiB on
	// GMINI Control (GEMINI_PROMPT_WARN_KB, default 300; 0 disables via -1).
	MaxPromptBytes int
	PromptWarnKB   int
	// AllowedHosts are extra Host header values accepted beside the loopback names
	// (GEMINI_ALLOWED_HOSTS, comma-separated).
	AllowedHosts []string
	// ModelsRefreshHours is kept for backward compatibility (deprecated: periodic
	// background catalog refresh was removed in K14 P3; default 0).
	ModelsRefreshHours int
	// CatalogRefreshAfterCookie optionally refreshes the catalog right after a
	// successful cookie refresh, at most once per 6 hours
	// (GEMINI_CATALOG_REFRESH_AFTER_COOKIE, default off = false; K14 P3).
	CatalogRefreshAfterCookie bool
	// AuthRecovery enables the background /app recovery polling loop when not
	// authenticated (GEMINI_AUTH_RECOVERY, default off = false; K14 P4).
	AuthRecovery bool
	// IdleQuietMin refuses background Google calls when no generation request or
	// page action happened in the last N minutes (GEMINI_IDLE_QUIET_MIN, default 10).
	IdleQuietMin int
	// BlockMaxBackoffMin is the top step of the anti-bot backoff
	// (GEMINI_BLOCK_MAX_BACKOFF_MIN, default 60).
	BlockMaxBackoffMin int
	// StateFile / StateMaxAgeMin: persisted session state (GEMINI_STATE_FILE,
	// default "<cookie file>.state.json", "off" to disable; always off for an
	// inline cookie) and its reuse threshold (GEMINI_STATE_MAX_AGE_MIN, default 1440 = 24h).
	StateMaxAgeMin int
	Gemini         gemini.Config
}

// loadConfig seeds env from a .env file, then reads all settings. The .env path
// is resolved in priority order: the explicit envFile argument (the -config
// flag), then $GEMINI_ENV_FILE, then ./.env.
func loadConfig(envFile string) *Config {
	if envFile == "" {
		envFile = os.Getenv("GEMINI_ENV_FILE")
	}
	if envFile == "" {
		envFile = ".env"
	}
	loadDotenv(envFile)

	cfg := &Config{
		Listen:               normalizeListen(getEnv("GEMINI_LISTEN", defaultListen)),
		LogRequests:          getEnvBool("GEMINI_LOG_REQUESTS", false),
		LogFormat:            strings.ToLower(strings.TrimSpace(getEnv("GEMINI_LOG_FORMAT", "text"))),
		CookieRotation:       getEnvBool("GEMINI_COOKIE_ROTATION", false),
		CookieRefresh:        getEnvInt("GEMINI_COOKIE_REFRESH_MIN", 9),
		CookieAutoRefreshMin: positive(getEnvInt("GEMINI_COOKIE_AUTO_REFRESH_MIN", 90), 90),
		CookieRefreshCmd:     strings.TrimSpace(getEnv("GEMINI_COOKIE_REFRESH_CMD", "")),
		APIKeys:              splitKeys(getEnv("GEMINI_API_KEYS", "")),

		SSEKeepaliveSec:             getEnvInt("GEMINI_SSE_KEEPALIVE_SEC", 15),
		ShutdownGraceSec:            shutdownGrace(getEnvInt("GEMINI_SHUTDOWN_GRACE_SEC", defaultShutdownGraceSec)),
		AllowNoAuth:                 getEnvBool("GEMINI_ALLOW_NO_AUTH", false),
		DisableEchoFilter:           !getEnvBool("GEMINI_STRIP_HISTORY_ECHO", true),
		DisableToolPromptRules:      !getEnvBool("GEMINI_TOOL_PROMPT_NOECHO_RULES", true),
		ToolPromptReminder:          getEnvBool("GEMINI_TOOL_PROMPT_REMINDER", true),
		InternalToolAction:          normalizeInternalToolAction(getEnv("GEMINI_INTERNAL_TOOL_ACTION", "notify")),
		CannedErrorRetry:            normalizeCannedErrorRetry(getEnv("GEMINI_CANNED_ERROR_RETRY", "off")),
		CannedErrorRetrySec:         positive(getEnvInt("GEMINI_CANNED_ERROR_RETRY_SEC", 25), 25),
		DisableMarkerNeutralization: !getEnvBool("GEMINI_NEUTRALIZE_HISTORY_MARKERS", true),
		KeepTextAfterToolCall:       !getEnvBool("GEMINI_DROP_TEXT_AFTER_TOOL_CALL", true),
		UIEnabled:                   getEnvBool("GEMINI_UI", true),
		WarnSuspectFallback:         getEnvBool("GEMINI_WARN_SUSPECT_FALLBACK", true),
		WarnSuspectCooldownMin:      getEnvInt("GEMINI_WARN_SUSPECT_COOLDOWN_MIN", 10),
		AntigravityModelsFile:       getEnv("GEMINI_ANTIGRAVITY_MODELS_FILE", ""),
		StreamToolsProgress:         getEnvBool("GEMINI_STREAM_TOOLS_PROGRESS", true),
		StreamThoughts:              getEnvBool("GEMINI_STREAM_THOUGHTS", true),
		SilenceWarningSec:           nonNegative(getEnvInt("GEMINI_SILENCE_WARN_SEC", 45), 45),
		NoContentSec:                nonNegative(getEnvInt("GEMINI_NO_CONTENT_SEC", 0), 0),
		MaxHeldBytes:                capWithDisable(getEnvInt("GEMINI_MAX_HELD_BYTES", 256*1024), 256*1024),
		MaxHeldSec:                  capWithDisable(getEnvInt("GEMINI_MAX_HELD_SEC", 120), 120),
		DefaultThinkingLevel:        strings.ToLower(strings.TrimSpace(getEnv("GEMINI_THINKING_LEVEL", ""))),
		ThinkingModes:               parseModeCodes(getEnv("GEMINI_THINKING_MODE_CODES", defaultThinkingModeCodes)),
		ThinkingModesExplicit:       strings.TrimSpace(getEnv("GEMINI_THINKING_MODE_CODES", "")) != "",
		MaxPromptBytes:              getEnvInt("GEMINI_MAX_PROMPT_BYTES", 0),
		PromptWarnKB:                capWithDisable(getEnvInt("GEMINI_PROMPT_WARN_KB", 300), 300),
		CORSOrigins:                 splitKeys(getEnv("GEMINI_CORS_ORIGINS", "")),
		AllowedHosts:                splitKeys(getEnv("GEMINI_ALLOWED_HOSTS", "")),
		ModelsRefreshHours:          nonNegative(getEnvInt("GEMINI_MODELS_REFRESH_HOURS", 0), 0),
		CatalogRefreshAfterCookie:   getEnvBool("GEMINI_CATALOG_REFRESH_AFTER_COOKIE", false),
		AuthRecovery:                getEnvBool("GEMINI_AUTH_RECOVERY", false),
		IdleQuietMin:                nonNegative(getEnvInt("GEMINI_IDLE_QUIET_MIN", 10), 10),
		BlockMaxBackoffMin:          positive(getEnvInt("GEMINI_BLOCK_MAX_BACKOFF_MIN", 60), 60),
		StateMaxAgeMin:              positive(getEnvInt("GEMINI_STATE_MAX_AGE_MIN", 1440), 1440),
		Gemini: gemini.Config{
			LogParams:      getEnvBool("GEMINI_LOG_REQUESTS", false),
			RetryAttempts:  getEnvInt("GEMINI_RETRY_ATTEMPTS", 3),
			RetryDelaySec:  getEnvInt("GEMINI_RETRY_DELAY_SEC", 2),
			RequestTimeout: getEnvInt("GEMINI_REQUEST_TIMEOUT_SEC", 180),
			AuthUser:       getEnv("GEMINI_AUTH_USER", ""),
			DefaultModel:   getEnv("GEMINI_DEFAULT_MODEL", "gemini-3.5-flash"),
			Cookie:         getEnv("GEMINI_COOKIE", ""),
			CookieFile:     getEnv("GEMINI_COOKIE_FILE", ""),
			Proxy:          getEnv("GEMINI_PROXY", ""),

			AllowRemoteImages:  getEnvBool("GEMINI_ALLOW_REMOTE_IMAGES", false),
			TraceDir:           strings.TrimSpace(getEnv("GEMINI_TRACE_DIR", "")),
			TraceMaxFiles:      getEnvInt("GEMINI_TRACE_MAX_FILES", 0),
			TraceRaw:           getEnvBool("GEMINI_TRACE_RAW", false),
			RotateSuspendAfter: rotateSuspendEnv(),
			TemporaryChat:      getEnvBool("GEMINI_TEMPORARY_CHAT", false),
			CookieRotation:     getEnvBool("GEMINI_COOKIE_ROTATION", false),
		},
	}

	cfg.Gemini.BlockMaxBackoff = time.Duration(cfg.BlockMaxBackoffMin) * time.Minute
	if cfg.IdleQuietMin == 0 {
		cfg.Gemini.IdleQuiet = -time.Minute
	} else {
		cfg.Gemini.IdleQuiet = time.Duration(cfg.IdleQuietMin) * time.Minute
	}
	cfg.Gemini.StateMaxAge = time.Duration(cfg.StateMaxAgeMin) * time.Minute
	cfg.Gemini.StateFile = stateFilePath(getEnv("GEMINI_STATE_FILE", ""), cfg.Gemini.CookieFile, cfg.Gemini.Cookie)
	cfg.Gemini.CallsFile = callsFilePath(getEnv("GEMINI_CALLS_FILE", ""), cfg.Gemini.StateFile)

	// GEMINI_BL: empty or "auto" → auto-resolve from the Gemini page (with the
	// baked-in default as fallback); an explicit value disables auto-resolution.
	bl := strings.TrimSpace(getEnv("GEMINI_BL", ""))
	cfg.Gemini.GeminiBLAuto = bl == "" || strings.EqualFold(bl, "auto")
	if cfg.Gemini.GeminiBLAuto {
		cfg.Gemini.GeminiBL = gemini.DefaultBuildLabel
	} else {
		cfg.Gemini.GeminiBL = bl
	}
	return cfg
}

// defaultThinkingModeCodes are the web "mode" codes observed in the real catalog
// (model id → mode): Pro = 3, Flash-Lite = 6, Hydrogen = 1. They are only sent
// together with a thinking level. Since the catalogue itself carries the code
// (AvailableModel.ModeCode), this table is only a fallback for a catalogue
// restored from an older state file; setting the variable makes it authoritative.
const defaultThinkingModeCodes = "e6fa609c3fa255c0=3,8c46e95b1a07cecc=6,56fdd199312815e2=1"

// parseModeCodes parses "id=code,id=code" (invalid entries are skipped).
func parseModeCodes(s string) map[string]int {
	out := map[string]int{}
	for _, kv := range splitKeys(s) {
		k, v, ok := strings.Cut(kv, "=")
		if n, err := strconv.Atoi(strings.TrimSpace(v)); ok && err == nil && n > 0 && strings.TrimSpace(k) != "" {
			out[strings.TrimSpace(k)] = n
		}
	}
	return out
}

// normalizeListen turns a listen value into a "host:port" bind address. A bare
// numeric port ("8081") or a port-only ":8081" is bound to loopback
// (127.0.0.1:port), never to all interfaces. An explicit host (including
// 0.0.0.0) is kept as given; main warns when it is not loopback. An empty value
// yields the default.
func normalizeListen(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultListen
	}
	if !strings.Contains(s, ":") {
		if _, err := strconv.Atoi(s); err == nil {
			return "127.0.0.1:" + s
		}
		return s
	}
	if strings.HasPrefix(s, ":") {
		return "127.0.0.1" + s
	}
	return s
}

func normalizeInternalToolAction(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "retry", "off", "notify":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "notify"
	}
}

func normalizeCannedErrorRetry(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), "once") {
		return "once"
	}
	return "off"
}

// defaultShutdownGraceSec is the default graceful-shutdown window.
const defaultShutdownGraceSec = 30

// shutdownGrace maps a negative setting back to the default; 0 means "abort
// immediately".
func shutdownGrace(n int) int {
	if n < 0 {
		return defaultShutdownGraceSec
	}
	return n
}

// stateFilePath resolves GEMINI_STATE_FILE: "off"/"none"/"0" disables it; an
// explicit path is used as is; empty defaults to "<cookie file>.state.json" next
// to the cookie file. It is always disabled for an inline cookie (whose session
// lives in memory only) and when no cookie file is configured.
func stateFilePath(setting, cookieFile, inlineCookie string) string {
	setting = strings.TrimSpace(setting)
	switch strings.ToLower(setting) {
	case "off", "none", "0", "false", "no":
		return ""
	}
	if inlineCookie != "" || cookieFile == "" {
		return ""
	}
	if setting != "" {
		return setting
	}
	return cookieFile + ".state.json"
}

// positive maps a setting below 1 back to def.
func positive(n, def int) int {
	if n < 1 {
		return def
	}
	return n
}

// nonNegative maps a negative setting back to def.
func nonNegative(n, def int) int {
	if n < 0 {
		return def
	}
	return n
}

// capWithDisable maps 0 to -1 (explicitly disabled) and negative values to def.
func capWithDisable(n, def int) int {
	if n == 0 {
		return -1
	}
	if n < 0 {
		return def
	}
	return n
}

// validateStartup refuses an unauthenticated configuration unless explicitly
// allowed with GEMINI_ALLOW_NO_AUTH=1.
func validateStartup(cfg *Config) error {
	if len(cfg.APIKeys) == 0 && !cfg.AllowNoAuth {
		return errors.New("GEMINI_API_KEYS is empty: refusing to start without an API key. " +
			"Set GEMINI_API_KEYS=<key>, or set GEMINI_ALLOW_NO_AUTH=1 to explicitly run without authentication")
	}
	return nil
}

// loadDotenv parses a simple KEY=VALUE file and sets variables that are not
// already present in the process environment (standard dotenv precedence).
func loadDotenv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // missing .env is not an error
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = unquote(val)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

// unquote removes a single matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return def
}

// splitKeys parses a comma-separated list into a trimmed, non-empty slice.
func splitKeys(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// callsFilePath returns the journal of generations: the explicit value ("off"
// disables it), else a file beside the state file, else none.
func callsFilePath(explicit, stateFile string) string {
	switch v := strings.TrimSpace(explicit); {
	case strings.EqualFold(v, "off"):
		return ""
	case v != "":
		return v
	case stateFile != "":
		return strings.TrimSuffix(stateFile, ".state.json") + ".calls.jsonl"
	}
	return ""
}

func rotateSuspendEnv() int {
	v := getEnvInt("GEMINI_COOKIE_ROTATE_SUSPEND_AFTER", gemini.DefaultRotateSuspendAfter)
	if v <= 0 {
		return -1 // 0 explicitly disables rotation suspension
	}
	return v
}
