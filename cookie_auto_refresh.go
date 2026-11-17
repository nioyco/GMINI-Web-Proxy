package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

const (
	defaultAutoRefreshActivityWindow = 90 * time.Minute
	defaultAutoRefreshMinSpacing     = 60 * time.Minute
	defaultManualRefreshMinSpacing   = 10 * time.Minute
	defaultAutoRefreshExecTimeout    = 60 * time.Second
	defaultAutoRefreshWaitTimeout    = 45 * time.Second
	defaultAutoRefreshPollInterval   = time.Minute
	autoRefreshMaxConsecutiveFails   = 2
)

// cookieAutoRefreshStatus is the JSON snapshot exposed in /_status and /_ui/api/state.
// It never exposes the configured command string or any output/paths.
type cookieAutoRefreshStatus struct {
	Configured          bool    `json:"configured"`
	Enabled             bool    `json:"enabled"`
	IntervalMin         int     `json:"interval_min"`
	Running             bool    `json:"running"`
	Paused              bool    `json:"paused"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTrigger         string  `json:"last_trigger,omitempty"`
	LastAttemptAt       *string `json:"last_attempt_at,omitempty"`
	LastFinishAt        *string `json:"last_finish_at,omitempty"`
	LastDurationMS      *int64  `json:"last_duration_ms,omitempty"`
	LastExitCode        *int    `json:"last_exit_code,omitempty"`
	LastOK              *bool   `json:"last_ok,omitempty"`
	NextRefreshAt       *string `json:"next_refresh_at,omitempty"`
	NextRefreshInSec    *int64  `json:"next_refresh_in_sec,omitempty"`
	ManualCooldownSec   int64   `json:"manual_cooldown_sec,omitempty"`
}

// cookieAutoRefresher runs an external cookie-refresh command (no shell) only
// when useful, with strict guardrails: no concurrent runs, >=60m spacing,
// never during a Google block, 60s exec timeout, 45s on-demand wait cap, and
// automatic pause after 2 consecutive failures until cookie.txt changes.
type cookieAutoRefresher struct {
	client *gemini.Client
	log    *slog.Logger

	cmdArgs            []string
	catalogAfterCookie bool
	activityWindow     time.Duration
	minSpacing         time.Duration
	manualMinSpacing   time.Duration
	execTimeout        time.Duration
	waitTimeout        time.Duration
	pollInterval       time.Duration
	nowFn              func() time.Time

	mu                  sync.Mutex
	enabled             bool
	interval            time.Duration
	startedAt           time.Time
	lastActivityAt      time.Time
	lastLaunchAt        time.Time
	lastFinishAt        time.Time
	lastDuration        time.Duration
	lastExitCode        int
	lastOK              *bool
	lastTrigger         string
	consecutiveFailures int
	paused              bool
	pausedAtGen         uint64
	running             bool
	runDone             chan struct{}
}

func newCookieAutoRefresher(cfg *Config, client *gemini.Client, log *slog.Logger) *cookieAutoRefresher {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var args []string
	if cmd := strings.TrimSpace(cfg.CookieRefreshCmd); cmd != "" {
		parsed, err := splitCommandArgs(cmd)
		if err != nil {
			log.Warn("cookie auto-refresh command ignored: invalid quoting", "err", err)
		} else {
			args = parsed
		}
	}
	intervalMin := cfg.CookieAutoRefreshMin
	if intervalMin <= 0 {
		intervalMin = 90
	}
	interval := time.Duration(intervalMin) * time.Minute
	now := time.Now().Round(0)
	return &cookieAutoRefresher{
		client:             client,
		log:                log,
		cmdArgs:            args,
		catalogAfterCookie: cfg.CatalogRefreshAfterCookie,
		activityWindow:     defaultAutoRefreshActivityWindow,
		minSpacing:         defaultAutoRefreshMinSpacing,
		manualMinSpacing:   defaultManualRefreshMinSpacing,
		execTimeout:        defaultAutoRefreshExecTimeout,
		waitTimeout:        defaultAutoRefreshWaitTimeout,
		pollInterval:       defaultAutoRefreshPollInterval,
		nowFn:              func() time.Time { return time.Now().Round(0) },
		enabled:            false,
		interval:           interval,
		startedAt:          now,
	}
}

// splitCommandArgs splits a command line into executable and arguments without
// invoking a shell (supports single/double quotes and backslash escapes).
func splitCommandArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inToken := false
	var quote rune

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			if quote == '"' && r == '\\' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
				continue
			}
			cur.WriteRune(r)
			continue
		}
		if unicode.IsSpace(r) {
			if inToken {
				args = append(args, cur.String())
				cur.Reset()
				inToken = false
			}
			continue
		}
		inToken = true
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == '\\' && i+1 < len(runes) {
			i++
			cur.WriteRune(runes[i])
			continue
		}
		cur.WriteRune(r)
	}
	if quote != 0 {
		return nil, errors.New("unclosed quote in command")
	}
	if inToken {
		args = append(args, cur.String())
	}
	return args, nil
}

// Loop periodically checks whether a background refresh is needed.
func (r *cookieAutoRefresher) Loop(ctx context.Context) {
	t := time.NewTicker(r.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick()
		}
	}
}

func (r *cookieAutoRefresher) tokenAge() (time.Duration, string) {
	rot := r.client.RotationStatus()
	return rot.TokenAge, rot.TokenAgeSource
}

func (r *cookieAutoRefresher) tokenDueLocked(now time.Time) bool {
	if r.interval <= 0 {
		return false
	}
	age, src := r.tokenAge()
	if src != "unknown" {
		return age >= r.interval
	}
	ref := r.startedAt
	if !r.lastFinishAt.IsZero() {
		ref = r.lastFinishAt
	}
	return now.Sub(ref) >= r.interval
}

func (r *cookieAutoRefresher) canLaunchLocked(now time.Time) (bool, string) {
	if len(r.cmdArgs) == 0 {
		return false, "not_configured"
	}
	if !r.enabled || r.interval <= 0 {
		return false, "disabled"
	}
	if r.paused {
		if r.client.CookieGeneration() != r.pausedAtGen {
			r.paused = false
			r.consecutiveFailures = 0
		} else {
			return false, "paused_failures"
		}
	}
	if !r.lastLaunchAt.IsZero() && now.Round(0).Sub(r.lastLaunchAt.Round(0)) < r.minSpacing {
		return false, "min_spacing"
	}
	return true, ""
}

func (r *cookieAutoRefresher) startRunLocked(now time.Time, trigger string) chan struct{} {
	r.running = true
	r.lastLaunchAt = now
	r.lastTrigger = trigger
	done := make(chan struct{})
	r.runDone = done
	go r.run(trigger, done)
	return done
}

// Tick runs a background refresh when the proxy was recently active and the
// token has reached the configured age.
func (r *cookieAutoRefresher) Tick() {
	r.mu.Lock()
	if !r.enabled {
		r.mu.Unlock()
		return
	}
	now := r.nowFn().Round(0)
	activeRecently := !r.lastActivityAt.IsZero() && now.Sub(r.lastActivityAt.Round(0)) <= r.activityWindow
	if !activeRecently || r.running || !r.tokenDueLocked(now) {
		r.mu.Unlock()
		return
	}
	if ok, _ := r.canLaunchLocked(now); !ok {
		r.mu.Unlock()
		return
	}
	if !r.client.AcquireBackground(gemini.FamilyApp, "cookie_auto_refresh_background") {
		r.mu.Unlock()
		return
	}
	r.startRunLocked(now, "background")
	r.mu.Unlock()
}

// PrepareRequest records request activity and, if auto-refresh was explicitly
// enabled via the UI toggle, the proxy was idle, and the token is older than
// the configured interval, runs a synchronous refresh (capped at 45 s).
func (r *cookieAutoRefresher) PrepareRequest(ctx context.Context, onWait func()) {
	r.client.RecordUserActivity()
	r.mu.Lock()
	now := r.nowFn().Round(0)
	wasIdle := r.lastActivityAt.IsZero() || now.Sub(r.lastActivityAt.Round(0)) > r.activityWindow
	r.lastActivityAt = now
	if !wasIdle || !r.enabled {
		r.mu.Unlock()
		return
	}
	var waitCh chan struct{}
	if r.running {
		if r.tokenDueLocked(now) {
			waitCh = r.runDone
		}
	} else if r.tokenDueLocked(now) {
		if ok, _ := r.canLaunchLocked(now); ok && r.client.AcquireBackground(gemini.FamilyApp, "cookie_auto_refresh_on_demand") {
			waitCh = r.startRunLocked(now, "on_demand")
		}
	}
	waitCap := r.waitTimeout
	r.mu.Unlock()

	if waitCh == nil {
		return
	}
	if onWait != nil {
		onWait()
	}
	timer := time.NewTimer(waitCap)
	defer timer.Stop()
	select {
	case <-waitCh:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// TriggerEarly launches an early refresh when Gemini serves another model
// (at most once per minSpacing = 60 min) if auto-refresh is enabled.
func (r *cookieAutoRefresher) TriggerEarly() {
	r.client.RecordUserActivity()
	r.mu.Lock()
	now := r.nowFn()
	if !r.enabled || r.running {
		r.mu.Unlock()
		return
	}
	if ok, _ := r.canLaunchLocked(now); !ok {
		r.mu.Unlock()
		return
	}
	if !r.client.AcquireBackground(gemini.FamilyApp, "cookie_auto_refresh_other_model") {
		r.mu.Unlock()
		return
	}
	r.startRunLocked(now, "other_model")
	r.mu.Unlock()
}

func (r *cookieAutoRefresher) run(trigger string, done chan struct{}) {
	defer close(done)

	start := r.nowFn().Round(0)
	execCtx, cancel := context.WithTimeout(context.Background(), r.execTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, r.cmdArgs[0], r.cmdArgs[1:]...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	dur := r.nowFn().Sub(start)
	if dur < 0 {
		dur = 0
	}

	exitCode := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	if err == nil {
		r.client.ReloadCookieFromDisk()
		r.client.UnsuspendRotation()
		if r.catalogAfterCookie {
			catCtx, catCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = r.client.RefreshCatalogAfterCookie(catCtx)
			catCancel()
		}
	}

	r.mu.Lock()
	r.running = false
	r.runDone = nil
	r.lastFinishAt = r.nowFn().Round(0)
	r.lastDuration = dur
	r.lastExitCode = exitCode
	ok := err == nil
	r.lastOK = &ok
	if ok {
		r.consecutiveFailures = 0
		r.paused = false
	} else if trigger != "manual" {
		r.consecutiveFailures++
		if r.consecutiveFailures >= autoRefreshMaxConsecutiveFails {
			r.paused = true
			r.pausedAtGen = r.client.CookieGeneration()
		}
	}
	failures := r.consecutiveFailures
	paused := r.paused
	r.mu.Unlock()

	if ok {
		r.log.Info("cookie_auto_refresh",
			"trigger", trigger,
			"ok", true,
			"exit_code", exitCode,
			"duration_ms", dur.Milliseconds())
	} else {
		r.log.Warn("cookie_auto_refresh",
			"trigger", trigger,
			"ok", false,
			"exit_code", exitCode,
			"duration_ms", dur.Milliseconds(),
			"consecutive_failures", failures,
			"paused", paused)
	}
}

// RunManual launches a manual cookie refresh ("manual") if all guardrails allow
// it: command configured, not already running, no active Google block, no
// generation requests in flight, and >=10 min since any previous refresh.
func (r *cookieAutoRefresher) RunManual(activeInFlight int) (cookieAutoRefreshStatus, int, string) {
	r.mu.Lock()
	now := r.nowFn().Round(0)
	if len(r.cmdArgs) == 0 {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "not_configured"
	}
	if r.running {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "already_running"
	}
	if r.client.GoogleBlockStatus().Active {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "google_blocked"
	}
	if activeInFlight > 0 {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "requests_in_flight"
	}
	if !r.lastLaunchAt.IsZero() && r.manualMinSpacing > 0 && now.Sub(r.lastLaunchAt.Round(0)) < r.manualMinSpacing {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "cooldown"
	}
	r.client.RecordUserActivity()
	if !r.client.AcquireBackground(gemini.FamilyApp, "cookie_auto_refresh_manual") {
		r.mu.Unlock()
		return r.Status(), http.StatusConflict, "google_blocked"
	}
	r.startRunLocked(now, "manual")
	r.mu.Unlock()
	return r.Status(), http.StatusOK, ""
}

// Toggle flips or sets the enabled state for the lifetime of this process
// (never persisted to disk or restored across restarts; used by the UI switch).
func (r *cookieAutoRefresher) Toggle(on *bool) cookieAutoRefreshStatus {
	return r.ToggleFrom(on, "ui")
}

// ToggleFrom flips or sets the enabled state and logs each transition with source.
func (r *cookieAutoRefresher) ToggleFrom(on *bool, source string) cookieAutoRefreshStatus {
	if source == "" {
		source = "ui"
	}
	var (
		changed     bool
		enabled     bool
		intervalMin int
	)
	r.mu.Lock()
	if len(r.cmdArgs) > 0 {
		prev := r.enabled
		if on != nil {
			r.enabled = *on
		} else {
			r.enabled = !r.enabled
		}
		if r.enabled {
			if r.interval <= 0 {
				r.interval = 90 * time.Minute
			}
			r.paused = false
			r.consecutiveFailures = 0
		}
		changed = r.enabled != prev
		enabled = r.enabled
		intervalMin = int(r.interval / time.Minute)
	}
	r.mu.Unlock()
	if changed {
		r.log.Info("cookie_auto_refresh_toggle",
			"enabled", enabled,
			"source", source,
			"interval_min", intervalMin)
	}
	return r.Status()
}

// Status returns a content-free snapshot of the auto-refresh state.
func (r *cookieAutoRefresher) Status() cookieAutoRefreshStatus {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.nowFn()
	if r.paused && r.client.CookieGeneration() != r.pausedAtGen {
		r.paused = false
		r.consecutiveFailures = 0
	}

	st := cookieAutoRefreshStatus{
		Configured:          len(r.cmdArgs) > 0,
		Enabled:             r.enabled,
		IntervalMin:         int(r.interval / time.Minute),
		Running:             r.running,
		Paused:              r.paused,
		ConsecutiveFailures: r.consecutiveFailures,
		LastTrigger:         r.lastTrigger,
		LastAttemptAt:       rfc3339Ptr(r.lastLaunchAt),
		LastFinishAt:        rfc3339Ptr(r.lastFinishAt),
		LastOK:              r.lastOK,
	}
	if r.lastOK != nil {
		ms := r.lastDuration.Milliseconds()
		ec := r.lastExitCode
		st.LastDurationMS = &ms
		st.LastExitCode = &ec
	}
	if !r.lastLaunchAt.IsZero() && r.manualMinSpacing > 0 {
		if rem := r.manualMinSpacing - now.Round(0).Sub(r.lastLaunchAt.Round(0)); rem > 0 {
			st.ManualCooldownSec = int64(rem.Seconds() + 0.999)
		}
	}
	if st.Configured && st.Enabled && !st.Paused && !st.Running && r.interval > 0 {
		var rem time.Duration
		age, src := r.tokenAge()
		if src != "unknown" {
			rem = r.interval - age
		} else {
			ref := r.startedAt
			if !r.lastFinishAt.IsZero() {
				ref = r.lastFinishAt
			}
			rem = r.interval - now.Sub(ref)
		}
		if !r.lastLaunchAt.IsZero() {
			if sp := r.minSpacing - now.Sub(r.lastLaunchAt); sp > rem {
				rem = sp
			}
		}
		if rem < 0 {
			rem = 0
		}
		nextAt := now.Add(rem)
		sec := int64(rem.Seconds())
		st.NextRefreshAt = rfc3339Ptr(nextAt)
		st.NextRefreshInSec = &sec
	}
	return st
}
