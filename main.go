package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

func main() {
	configPath := flag.String("config", "", "path to a .env file to load (default: $GEMINI_ENV_FILE or ./.env)")
	cookieFile := flag.String("cookie-file", "", "path to a cookie file (overrides GEMINI_COOKIE_FILE)")
	listenAddr := flag.String("listen", "", "address to listen on, e.g. 127.0.0.1:8081 or :8081 (overrides GEMINI_LISTEN)")
	flag.Parse()

	cfg := loadConfig(*configPath)
	if *cookieFile != "" {
		cfg.Gemini.CookieFile = *cookieFile
	}
	if *listenAddr != "" {
		cfg.Listen = normalizeListen(*listenAddr)
	}
	logger := newLogger(cfg.LogRequests, cfg.LogFormat)
	if err := validateStartup(cfg); err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}
	if len(cfg.APIKeys) == 0 {
		logger.Warn("running WITHOUT authentication (GEMINI_ALLOW_NO_AUTH=1)")
	}
	if !isLoopbackBind(cfg.Listen) {
		logger.Warn("listening on a non-loopback address: other machines can reach this proxy",
			"listen", cfg.Listen)
	}

	client, err := gemini.New(cfg.Gemini, logger)
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	// Resolve the build label, verify cookie auth, and fetch the account model
	// list up front (bounded so a slow or unreachable Gemini doesn't block startup
	// indefinitely).
	startCtx, startCancel := context.WithTimeout(context.Background(), 15*time.Second)
	client.ResolveBuildLabel(startCtx)
	authStatus := client.CheckAuth(startCtx)

	// Auto-refresh keeps the cookie's __Secure-1PSIDTS from expiring: a file-backed
	// cookie is rewritten in place, an inline cookie is rotated in memory.
	refresh := authStatus == "authenticated" && (cfg.Gemini.Cookie != "" || cfg.Gemini.CookieFile != "") && cfg.CookieRefresh > 0
	if refresh {
		// Rotate once up front so the model list is fetched against the freshest
		// session; the periodic loop then only ticks on its interval.
		client.StartupRotate(startCtx) // skipped when the token is recent
		authStatus = fmt.Sprintf("authenticated (auto-refresh %dm)", cfg.CookieRefresh)
	}
	client.ResolveModels(startCtx)
	startCancel()

	srv := newServer(cfg, client, logger)
	httpServer := newHTTPServer(cfg, srv.handler())

	printBanner(cfg, client, client.CurrentBL(), authStatus)
	// Open-proxy guard: a non-loopback bind with no API keys exposes the
	// authenticated Google session to anyone who can reach the address.
	if len(cfg.APIKeys) == 0 && !isLoopbackBind(cfg.Listen) {
		logger.Warn("OPEN PROXY: listening on a non-loopback address with no API keys — "+
			"anyone who can reach this address can use your Google session",
			"listen", cfg.Listen, "fix", "set GEMINI_API_KEYS, or bind to 127.0.0.1")
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if refresh {
		go client.CookieRefreshLoop(ctx, time.Duration(cfg.CookieRefresh)*time.Minute)
	}
	// The catalog is refreshed at most once per GEMINI_MODELS_REFRESH_HOURS, on its
	// own timer (never chained to a rotation tick).
	go client.CatalogRefreshLoop(ctx, time.Duration(cfg.ModelsRefreshHours)*time.Hour)
	// Whenever a cookie is configured, watch the session: while it is not signed in
	// (Google block at startup, stale cookie, session lost after a generation
	// error) it is re-checked sparingly; while signed in, no call is made.
	if cfg.Gemini.Cookie != "" || cfg.Gemini.CookieFile != "" {
		var startRotation sync.Once
		go client.AuthRecoveryLoop(ctx, func() {
			if !refresh && cfg.CookieRefresh > 0 {
				startRotation.Do(func() {
					go client.CookieRefreshLoop(ctx, time.Duration(cfg.CookieRefresh)*time.Minute)
				})
			}
		})
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("server error", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown error", "err", err)
		}
	}
}

// shutdownServer stops accepting connections and waits up to grace for in-flight
// requests (SSE streams included) to finish. When the grace expires, requests still
// active are cancelled explicitly (logged as aborted_on_shutdown), given a short
// window to flush their final frame, and any remaining connection is closed.
func shutdownServer(httpServer *http.Server, srv *Server, grace time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	err := httpServer.Shutdown(ctx)
	if err == nil {
		return
	}
	n := srv.AbortActive()
	logger.Warn("shutdown grace expired; active requests aborted", "grace", grace, "aborted", n)
	ctx2, cancel2 := context.WithTimeout(context.Background(), abortFlushWait)
	defer cancel2()
	if err := httpServer.Shutdown(ctx2); err != nil {
		logger.Error("shutdown error", "err", err)
		_ = httpServer.Close()
	}
}

// abortFlushWait is how long aborted handlers get to write their final frame.
const abortFlushWait = 2 * time.Second

// HTTP server timeouts. WriteTimeout is intentionally unset: SSE streams must
// stay open for the full generation. The others bound slow or idle peers without
// affecting an in-flight streamed response (the request body is read up front,
// before any streaming begins).
const (
	httpReadHeaderTimeout = 30 * time.Second
	httpReadTimeout       = 120 * time.Second // bound slow request-body uploads (e.g. inline images)
	httpIdleTimeout       = 120 * time.Second // bound idle keep-alive connections
)

// newHTTPServer builds the proxy's HTTP server with timeouts tuned for SSE.
func newHTTPServer(cfg *Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// No WriteTimeout: SSE streams must stay open for the full generation.
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}

// newLogger returns a slog text logger writing to stderr. When verbose is false
// the level is raised to Warn, suppressing per-request Info logs while still
// surfacing retries (Warn) and failures (Error).
func newLogger(verbose bool, format string) *slog.Logger {
	return newLoggerTo(os.Stderr, verbose, format)
}

// newLoggerTo builds the logger on an arbitrary writer. format "json" selects the
// JSON handler (GEMINI_LOG_FORMAT=json); anything else keeps the default text one.
func newLoggerTo(w io.Writer, verbose bool, format string) *slog.Logger {
	level := slog.LevelWarn
	if verbose {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// printBanner writes the startup summary, mirroring the reference CLI output.
func printBanner(cfg *Config, client *gemini.Client, bl, authStatus string) {
	cookie := "none (anonymous)"
	switch {
	case cfg.Gemini.Cookie != "":
		cookie = "yes (inline)"
	case cfg.Gemini.CookieFile != "":
		cookie = "yes (" + cfg.Gemini.CookieFile + ")"
	}
	// Append the upstream auth result when a cookie is configured.
	if authStatus != "" && authStatus != "anonymous" {
		cookie += " — " + authStatus
	}
	proxy := cfg.Gemini.Proxy
	if proxy == "" {
		proxy = "none (uses system env HTTP_PROXY/HTTPS_PROXY)"
	}
	auth := "open"
	if len(cfg.APIKeys) > 0 {
		auth = fmt.Sprintf("required (%d key(s))", len(cfg.APIKeys))
	}
	if cfg.Gemini.GeminiBLAuto {
		bl += " (auto)"
	}
	names := client.ModelNames()
	models := fmt.Sprintf("%s (%d, %s)", strings.Join(names, ", "), len(names), client.ModelsSourceLabel())

	fmt.Fprintf(os.Stdout, "gemini-web2api v%s\n", version)
	fmt.Fprintf(os.Stdout, "  Listening:   %s\n", bindDisplay(cfg.Listen))
	fmt.Fprintf(os.Stdout, "  Base URL:    %s/v1\n", clientBaseURL(cfg.Listen))
	fmt.Fprintf(os.Stdout, "  Models:      %s\n", models)
	fmt.Fprintf(os.Stdout, "  Build:       %s\n", bl)
	fmt.Fprintf(os.Stdout, "  Cookie:      %s\n", cookie)
	fmt.Fprintf(os.Stdout, "  Proxy:       %s\n", proxy)
	fmt.Fprintf(os.Stdout, "  Client auth: %s\n", auth)
	fmt.Fprintf(os.Stdout, "  Retry:       %dx / %ds\n\n", cfg.Gemini.RetryAttempts, cfg.Gemini.RetryDelaySec)
}

// isLoopbackBind reports whether the bind address is loopback-only (safe to leave
// unauthenticated). A wildcard host (":port") counts as non-loopback.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// bindDisplay renders a bind address with an explicit host, showing a wildcard
// (0.0.0.0) for an omitted host so ":8081" reads as "0.0.0.0:8081".
func bindDisplay(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, port)
}

// clientBaseURL renders a URL a local client can use, mapping a wildcard/empty
// host to localhost.
func clientBaseURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}
