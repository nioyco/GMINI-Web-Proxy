package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// listModelsRPC is the batchexecute RPC id that returns the account-specific
// model catalog (and tier/capability flags).
const listModelsRPC = "otAQ7b"

// DefaultModelsRefreshInterval is the default spacing of the periodic catalog
// refresh (listModels). The catalog is fetched at startup (unless a recent copy
// was restored), then at most once per interval or on explicit request
// (AUDIT-13 A2). It used to be refreshed at every cookie-rotation tick.
const DefaultModelsRefreshInterval = 24 * time.Hour

// catalogRetryAfter spaces catalog attempts after a failed or skipped one,
// catalogManualMinInterval spaces explicit refresh requests, and
// catalogAfterCookieMinInterval caps optional post-cookie refreshes to 1/6h.
var (
	catalogRetryAfter             = time.Hour
	catalogManualMinInterval      = 5 * time.Minute
	catalogAfterCookieMinInterval = 6 * time.Hour
)

// genericModelHeader is the x-goog-ext-525001261-jspb value used on the model
// listing RPC (no specific model id, just the [4] suffix the web client sends).
const genericModelHeader = "[1,null,null,null,null,null,null,null,[4]]"

// AvailableModel is a model exposed to the account, as returned by the listModels
// RPC (or the static fallback). Capacity/CapacityField encode the account tier and
// are sent in the model-selection header on each generation request.
type AvailableModel struct {
	Name          string // public slug, e.g. gemini-3.5-flash
	ModelID       string // hex id used in the selection header (empty for fallback)
	DisplayName   string // short label from the RPC, e.g. "Fast"
	Description   string
	Capacity      int
	CapacityField int
	// Read from the catalogue entry as Google sends it (may be empty/zero when the
	// layout changes): the menu label ([11], e.g. "Detailed research"), the
	// internal family code ([8], e.g. "Carbon4p0") and the web "mode" code ([17]).
	MenuLabel      string `json:",omitempty"`
	Family         string `json:",omitempty"`
	ModeCode       int    `json:",omitempty"`
	Locked         bool   `json:",omitempty"`
	ThinkingLevels []int  `json:",omitempty"`
}

// AutoModelHexID is the hex ModelID of Google's Auto router ("gemini-auto").
const AutoModelHexID = "8c46e95b1a07cecc"

// IsAutoModel reports whether a model targets Gemini's Auto router, which may
// serve responses under another backend model ID without indicating a downgrade.
func IsAutoModel(modelID, name, displayName string) bool {
	if modelID != AutoModelHexID {
		return false
	}
	return name == "gemini-auto" || strings.EqualFold(strings.TrimSpace(displayName), "Auto")
}

// ConfirmedNameEntry stores the last served name Google reported for a ModelID
// along with the otAQ7b catalog signature at confirmation time, so a true
// product replacement (e.g. 3.1 Pro -> Neon / Neon4p0) can invalidate stale
// history while a lagging otAQ7b label (e.g. Carbon -> 4 Argon) keeps the
// confirmed served name.
type ConfirmedNameEntry struct {
	ServedName     string    `json:"served_name,omitempty"`
	CatalogDisplay string    `json:"catalog_display,omitempty"`
	CatalogFamily  string    `json:"catalog_family,omitempty"`
	ConfirmedAt    time.Time `json:"confirmed_at,omitempty"`
	Invalidated    bool      `json:"invalidated,omitempty"`
	InvalidatedAt  time.Time `json:"invalidated_at,omitempty"`
}

var canonicalAliasesByHex = map[string][]string{
	"8c46e95b1a07cecc": {"gemini-auto", "gemini-quick-answers", "gemini-3.5-flash", "gemini-3.5-flash-lite"},
	"56fdd199312815e2": {"gemini-everyday-help", "gemini-4-hydrogen", "gemini-all-around-help"},
	"e6fa609c3fa255c0": {"gemini-deeper-analysis", "gemini-detailed-research", "gemini-3.1-pro", "gemini-4-neon"},
	"95a4c0abcba24364": {"gemini-code-complex-work", "gemini-code-&-complex-work", "gemini-most-complex-work", "gemini-4-argon", "gemini-4-carbon"},
}

var aliasToHex = func() map[string]string {
	m := make(map[string]string, 24)
	for hexID, aliases := range canonicalAliasesByHex {
		for _, a := range aliases {
			m[a] = hexID
		}
	}
	return m
}()

// Params builds the base GenParams selecting this model. Callers then attach
// images before calling Generate/GenerateStream.
func (m *AvailableModel) Params(prompt string) GenParams {
	return GenParams{Prompt: prompt, Model: m}
}

// selectionHeader builds the x-goog-ext-525001261-jspb value that selects this
// model. For capacity_field 13 the capacity is preceded by an extra null slot
// (mirrors the reference web client).
func (m *AvailableModel) selectionHeader() string {
	tail := strconv.Itoa(m.Capacity)
	if m.CapacityField == 13 {
		tail = "null," + tail
	}
	return fmt.Sprintf("[1,null,null,null,%q,null,null,0,[4],null,null,%s]", m.ModelID, tail)
}

// headers returns the four HTTP headers that select this model on a
// StreamGenerate request. reqUUID must equal inner[59] of the request body.
func (m *AvailableModel) headers(reqUUID string) map[string]string {
	return map[string]string{
		"x-goog-ext-525001261-jspb": m.selectionHeader(),
		"x-goog-ext-73010989-jspb":  "[0]",
		"x-goog-ext-73010990-jspb":  "[0]",
		"x-goog-ext-525005358-jspb": fmt.Sprintf("[%q,1]", reqUUID),
	}
}

// ─── Registry accessors (on Client) ──────────────────────────────────────────

// ResolveModelAlias checks whether name (after stripping any @effort suffix)
// is a legacy/canonical alias for an active model sharing the same hex ModelID.
// It returns (activeName, activeModelID, true) only when name itself is not a
// direct active key in the catalog and an active model with the target ModelID
// exists.
func (c *Client) ResolveModelAlias(name string) (targetName string, targetModelID string, isAlias bool) {
	base, _, _ := strings.Cut(strings.TrimSpace(name), "@")
	if base == "" {
		return "", "", false
	}
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if _, direct := c.models[base]; direct {
		return "", "", false
	}
	m, ok := c.resolveAliasLocked(base)
	if !ok {
		return "", "", false
	}
	return m.Name, m.ModelID, true
}

func (c *Client) resolveAliasLocked(base string) (*AvailableModel, bool) {
	targetHex := aliasToHex[base]
	if targetHex == "" {
		if wd, ok := c.withdrawn[base]; ok {
			targetHex = wd.Model.ModelID
		}
	}
	if targetHex == "" {
		return nil, false
	}
	for _, n := range c.modelOrder {
		if m := c.models[n]; m != nil && m.ModelID == targetHex {
			return m, true
		}
	}
	return nil, false
}

// ResolveModel looks up a model by name, substituting the configured default for
// an empty name.
func (c *Client) ResolveModel(name string) (*AvailableModel, error) {
	if name == "" {
		if m := c.defaultModel(); m != nil {
			if m.ModelID == "" && c.ModelsSourceLabel() == "fallback" {
				if km, ok := knownFallbackModels[m.Name]; ok {
					cp := km
					return &cp, nil
				}
			}
			return m, nil
		}
		return nil, errors.New("no models available")
	}
	c.modelMu.Lock()
	m, ok := c.models[name]
	src := c.modelsSource
	if ok {
		c.modelMu.Unlock()
		if m.ModelID == "" && src == "fallback" {
			if km, ok := knownFallbackModels[name]; ok {
				cp := km
				return &cp, nil
			}
		}
		return m, nil
	}
	if aliasModel, aliased := c.resolveAliasLocked(name); aliased {
		logPair := name + "->" + aliasModel.Name
		shouldLog := false
		if c.aliasWarnAt == nil {
			c.aliasWarnAt = map[string]time.Time{}
		}
		if last, seen := c.aliasWarnAt[logPair]; !seen || time.Since(last) >= absentModelWarnEvery {
			c.aliasWarnAt[logPair] = time.Now()
			shouldLog = true
		}
		cp := *aliasModel
		c.modelMu.Unlock()
		if shouldLog {
			c.log.Info("model_alias_resolved", "asked", name, "resolved", cp.Name, "model_id", cp.ModelID)
		}
		return &cp, nil
	}
	wd, was := c.withdrawn[name]
	shouldWarn := false
	if was {
		if c.absentWarnAt == nil {
			c.absentWarnAt = map[string]time.Time{}
		}
		if last, seen := c.absentWarnAt[name]; !seen || time.Since(last) >= absentModelWarnEvery {
			c.absentWarnAt[name] = time.Now()
			shouldWarn = true
		}
	}
	c.modelMu.Unlock()
	if was {
		// The catalogue Google returns can shrink without the model being
		// refused (2026-11-15: one model listed while the web page offered
		// four and the usage was at 1 %). The request is sent with the last
		// known id; the reply itself says which model answered.
		if shouldWarn {
			c.log.Warn("model absent from the last catalogue; sending the request with its last known id",
				"model", name, "model_id", wd.Model.ModelID, "absent_since", wd.Since.Format("15:04"))
		}
		m := wd.Model
		return &m, nil
	}
	if src == "fallback" {
		if km, ok := knownFallbackModels[name]; ok {
			cp := km
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("unknown model: %q", name)
}

// ResolveModelOrDefault resolves a model name, falling back to the configured
// default (then the first registered model) when the name is unknown — used for
// foreign ids like "claude-*" from Claude Code.
func (c *Client) ResolveModelOrDefault(name string) *AvailableModel {
	if m, err := c.ResolveModel(name); err == nil {
		return m
	}
	return c.defaultModel()
}

// defaultModel returns the configured default model, or the first registered
// model when the default is absent (e.g. an account whose dynamic list does not
// include the configured default name).
func (c *Client) defaultModel() *AvailableModel {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if m, ok := c.models[c.cfg.DefaultModel]; ok {
		return m
	}
	if len(c.modelOrder) > 0 {
		return c.models[c.modelOrder[0]]
	}
	return nil
}

// ListModels returns the registered models in listing order. It is a pure
// accessor: it never triggers a Google call (AUDIT-13 A4), so /v1/models and the
// diagnostic routes stay strictly local.
func (c *Client) ListModels() []*AvailableModel {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	out := make([]*AvailableModel, 0, len(c.modelOrder))
	for _, name := range c.modelOrder {
		if m, ok := c.models[name]; ok {
			out = append(out, m)
		}
	}
	return out
}

// ModelNames returns the registered model names in listing order (pure accessor,
// no Google call).
func (c *Client) ModelNames() []string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return append([]string(nil), c.modelOrder...)
}

// ModelsSourceLabel reports where the current list came from: "dynamic" (RPC),
// "fallback" (static), "static" (set via SetModels), or "" before the registry is
// populated.
func (c *Client) ModelsSourceLabel() string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return c.modelsSource
}

// SetModels installs an explicit model catalog, bypassing the network. The source
// label is reported as "static". This lets a consumer seed the registry offline
// (e.g. in tests) using the public API.
func (c *Client) SetModels(models []*AvailableModel) {
	c.storeModels(models, "static")
}

// CatalogRefreshLoop is retained for API compatibility; periodic background
// catalog refreshes were removed in K14 P3 (catalog refreshes now happen only on
// explicit click, at startup when no <7d catalog exists, or optionally after a
// successful cookie refresh). It waits for ctx cancellation without calling Google.
func (c *Client) CatalogRefreshLoop(ctx context.Context, interval time.Duration) {
	<-ctx.Done()
}

// catalogDueIn returns how long until the next periodic catalog refresh is due.
func (c *Client) catalogDueIn(interval time.Duration) time.Duration {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	now := nowFn().Round(0)
	due := now
	if !c.catalogAt.IsZero() {
		due = c.catalogAt.Add(interval)
	}
	if !c.catalogTriedAt.IsZero() {
		if retry := c.catalogTriedAt.Add(catalogRetryAfter); retry.After(due) {
			due = retry
		}
	}
	if d := due.Sub(now); d > 0 {
		return d
	}
	return 0
}

func (c *Client) markCatalogTried() {
	c.modelMu.Lock()
	c.catalogTriedAt = nowFn().Round(0)
	c.modelMu.Unlock()
}

// ErrCatalogRefreshRefused is returned by RefreshModels when the refresh was not
// attempted (no session, block in force, or a request made too recently).
var ErrCatalogRefreshRefused = errors.New("catalog refresh not attempted")

// ErrCatalogDegraded is returned when listModels shrinks to <=1 model while at
// least 2 real models are already known: the previous good catalog is kept and
// the session is marked weakened until a full catalog is received again.
var ErrCatalogDegraded = errors.New("degraded catalog rejected: session weakened (refresh cookie)")

// CatalogHealthStatus summarizes whether listModels recently returned a
// degraded (single-model) catalog indicating a weakened Google session.
type CatalogHealthStatus struct {
	SessionDegraded bool
	Since           time.Time
	AccountStatus   int
}

// CatalogHealth reports the current catalog degradation state (no Google call).
func (c *Client) CatalogHealth() CatalogHealthStatus {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	return CatalogHealthStatus{
		SessionDegraded: c.sessionDegraded,
		Since:           c.sessionDegradedAt,
		AccountStatus:   c.sessionAccountStatus,
	}
}

// CatalogRefreshNextAt returns when the next manual RefreshModels call is
// allowed (considering the 5-min minimum interval and any gating family block
// backoff), or the zero time if one may run now.
func (c *Client) CatalogRefreshNextAt() time.Time {
	var due time.Time
	now := nowFn().Round(0)
	c.modelMu.Lock()
	if !c.catalogTriedAt.IsZero() {
		due = c.catalogTriedAt.Add(catalogManualMinInterval)
	}
	c.modelMu.Unlock()
	bs := c.GoogleBlockStatus()
	for _, fam := range gatingFamilies(FamilyBatch) {
		if fb, ok := bs.Families[fam]; ok && fb.Active && fb.BackoffUntil.After(due) {
			due = fb.BackoffUntil
		}
	}
	if !due.After(now) {
		return time.Time{}
	}
	return due
}

// RefreshModels refreshes the model catalog on explicit request (one listModels
// call). It refuses — without any Google call — when the session is not
// authenticated, when a gemini.google.com block is in force, or when the last
// attempt is younger than catalogManualMinInterval.
func (c *Client) RefreshModels(ctx context.Context) error {
	if !c.Authenticated() {
		return fmt.Errorf("%w: session is not authenticated", ErrCatalogRefreshRefused)
	}
	now := nowFn().Round(0)
	c.modelMu.Lock()
	recent := !c.catalogTriedAt.IsZero() && now.Sub(c.catalogTriedAt.Round(0)) < catalogManualMinInterval
	c.modelMu.Unlock()
	if recent {
		return fmt.Errorf("%w: last attempt less than %s ago", ErrCatalogRefreshRefused, catalogManualMinInterval)
	}
	c.RecordUserActivity()
	if !c.AcquireBackground(FamilyBatch, "catalog_manual") {
		return fmt.Errorf("%w: google block backoff in force", ErrCatalogRefreshRefused)
	}
	if err := c.fetchModels(ctx); err != nil {
		return err
	}
	c.log.Info("gemini model list refreshed on request", "count", len(c.ModelNames()), "models", c.modelSummaryString())
	c.maybeRefreshBuildLabel(ctx, "build_label_models_refresh")
	return nil
}

// RefreshCatalogAfterCookie optionally refreshes the catalog right after a
// successful external cookie refresh, at most once per 6 hours (K14 P3).
func (c *Client) RefreshCatalogAfterCookie(ctx context.Context) error {
	if !c.Authenticated() {
		return fmt.Errorf("%w: session is not authenticated", ErrCatalogRefreshRefused)
	}
	now := nowFn().Round(0)
	c.modelMu.Lock()
	recent := !c.catalogAfterCookieAt.IsZero() && now.Sub(c.catalogAfterCookieAt.Round(0)) < catalogAfterCookieMinInterval
	c.modelMu.Unlock()
	if recent {
		return fmt.Errorf("%w: last after-cookie refresh less than %s ago", ErrCatalogRefreshRefused, catalogAfterCookieMinInterval)
	}
	if !c.AcquireBackground(FamilyBatch, "catalog_after_cookie") {
		return fmt.Errorf("%w: background guard refused", ErrCatalogRefreshRefused)
	}
	c.modelMu.Lock()
	c.catalogAfterCookieAt = now
	c.modelMu.Unlock()
	if err := c.fetchModels(ctx); err != nil {
		return err
	}
	c.log.Info("gemini model list refreshed after cookie refresh", "count", len(c.ModelNames()), "models", c.modelSummaryString())
	return nil
}

func (c *Client) modelSummaryString() string {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	parts := make([]string, 0, len(c.modelOrder))
	for _, name := range c.modelOrder {
		m := c.models[name]
		if m == nil {
			continue
		}
		desc := strings.TrimSpace(m.Description)
		if desc == "" {
			desc = strings.TrimSpace(m.DisplayName)
		}
		parts = append(parts, m.Name+"("+m.ModelID+"="+desc+")")
	}
	return strings.Join(parts, ", ")
}

func catalogIdentityChanged(cn ConfirmedNameEntry, prev *AvailableModel, next *AvailableModel) bool {
	if next == nil {
		return false
	}
	if cn.CatalogFamily != "" && next.Family != "" && !strings.EqualFold(cn.CatalogFamily, next.Family) {
		return true
	}
	if cn.CatalogDisplay != "" && next.DisplayName != "" && !strings.EqualFold(cn.CatalogDisplay, next.DisplayName) {
		return true
	}
	if prev != nil {
		if prev.Family != "" && next.Family != "" && !strings.EqualFold(prev.Family, next.Family) {
			return true
		}
		if prev.DisplayName != "" && next.DisplayName != "" && !strings.EqualFold(prev.DisplayName, next.DisplayName) {
			return true
		}
		if prev.Family == "" && next.Family != "" && prev.Name != "" && prev.Name != next.Name {
			return true
		}
	}
	return false
}

// storeModels atomically replaces the registry with the given models, preserving
// their order and recording the source and fetch time.
func (c *Client) storeModels(models []*AvailableModel, source string) {
	c.modelMu.Lock()
	prevByID := make(map[string]*AvailableModel, len(c.models))
	for _, old := range c.models {
		if old != nil && old.ModelID != "" {
			prevByID[old.ModelID] = old
		}
	}
	reg := make(map[string]*AvailableModel, len(models))
	activeByID := make(map[string]*AvailableModel, len(models))
	order := make([]string, 0, len(models))
	for _, m := range models {
		if m == nil {
			continue
		}
		if _, dup := reg[m.Name]; dup {
			continue
		}
		cp := *m
		if cp.ThinkingLevels != nil {
			cp.ThinkingLevels = append([]int(nil), cp.ThinkingLevels...)
		}
		if (source == "dynamic" || source == "persisted") && cp.ModelID != "" {
			if cn, hasCN := c.confirmedNames[cp.ModelID]; hasCN {
				if cn.Invalidated {
					if cp.DisplayName != "" {
						cn.CatalogDisplay = cp.DisplayName
					}
					if cp.Family != "" {
						cn.CatalogFamily = cp.Family
					}
					c.confirmedNames[cp.ModelID] = cn
				} else if cn.ServedName != "" {
					if catalogIdentityChanged(cn, prevByID[cp.ModelID], &cp) {
						c.confirmedNames[cp.ModelID] = ConfirmedNameEntry{
							CatalogDisplay: cp.DisplayName,
							CatalogFamily:  cp.Family,
							Invalidated:    true,
							InvalidatedAt:  nowFn().Round(0),
						}
					} else {
						if cp.Description != cn.ServedName {
							cp.Description = cn.ServedName
						}
						if cn.CatalogDisplay == "" && cp.DisplayName != "" {
							cn.CatalogDisplay = cp.DisplayName
						}
						if cn.CatalogFamily == "" && cp.Family != "" {
							cn.CatalogFamily = cp.Family
						}
						c.confirmedNames[cp.ModelID] = cn
					}
				}
			}
		}
		reg[cp.Name] = &cp
		if cp.ModelID != "" && activeByID[cp.ModelID] == nil {
			activeByID[cp.ModelID] = &cp
		}
		order = append(order, cp.Name)
	}
	// A model that disappears from a catalogue fetched from Google is remembered
	// as withdrawn unless the same ModelID was simply renamed to a new slug.
	if source == "dynamic" {
		for name, old := range c.models {
			if _, still := reg[name]; !still && old.ModelID != "" && activeByID[old.ModelID] == nil {
				if c.withdrawn == nil {
					c.withdrawn = map[string]WithdrawnModel{}
				}
				c.withdrawn[name] = WithdrawnModel{Model: *old, Since: time.Now()}
			}
		}
	}
	for name, w := range c.withdrawn {
		if _, listed := reg[name]; listed || (w.Model.ModelID != "" && activeByID[w.Model.ModelID] != nil) {
			delete(c.withdrawn, name)
		}
	}
	c.models = reg
	c.modelOrder = order
	c.modelsSource = source
	c.modelMu.Unlock()
}

// syncConfirmedServedName records the real backend name Google confirmed for
// modelID (inner[39]/inner[42]) and updates the in-memory catalog description
// when it differs from otAQ7b (e.g. "4 Carbon" -> "4 Argon").
func (c *Client) syncConfirmedServedName(modelID, servedName string) (prevDesc string, changed bool) {
	return c.syncConfirmedServedNameAt(modelID, servedName, nowFn().Round(0))
}

func (c *Client) syncConfirmedServedNameAt(modelID, servedName string, confirmedAt time.Time) (prevDesc string, changed bool) {
	modelID = strings.TrimSpace(modelID)
	servedName = strings.TrimSpace(servedName)
	if modelID == "" || servedName == "" {
		return "", false
	}
	if confirmedAt.IsZero() {
		confirmedAt = nowFn().Round(0)
	}
	c.modelMu.Lock()
	if c.confirmedNames == nil {
		c.confirmedNames = map[string]ConfirmedNameEntry{}
	}
	prev := c.confirmedNames[modelID]
	catDisplay, catFamily := prev.CatalogDisplay, prev.CatalogFamily
	for _, m := range c.models {
		if m.ModelID == modelID {
			if m.DisplayName != "" {
				catDisplay = m.DisplayName
			}
			if m.Family != "" {
				catFamily = m.Family
			}
			break
		}
	}
	if catDisplay == "" && catFamily == "" {
		for _, w := range c.withdrawn {
			if w.Model.ModelID == modelID {
				catDisplay = w.Model.DisplayName
				catFamily = w.Model.Family
				break
			}
		}
	}
	c.confirmedNames[modelID] = ConfirmedNameEntry{
		ServedName:     servedName,
		CatalogDisplay: catDisplay,
		CatalogFamily:  catFamily,
		ConfirmedAt:    confirmedAt,
		Invalidated:    false,
	}
	for _, m := range c.models {
		if m.ModelID == modelID && m.Description != servedName {
			prevDesc = m.Description
			m.Description = servedName
			changed = true
		}
	}
	c.modelMu.Unlock()
	return prevDesc, changed
}

// WithdrawnModel is a model that was in the catalogue and no longer is.
type WithdrawnModel struct {
	Model AvailableModel
	Since time.Time
}

// WithdrawnModels lists the models dropped by a later catalogue, by name.
func (c *Client) WithdrawnModels() []WithdrawnModel {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	out := make([]WithdrawnModel, 0, len(c.withdrawn))
	for _, w := range c.withdrawn {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.Name < out[j].Model.Name })
	return out
}

// ─── Fetching ────────────────────────────────────────────────────────────────

// ResolveModels populates the registry at startup (synchronous) and logs the
// result once. It installs the dynamic list when available, otherwise the static
// fallback set. When the configured default model is not in the resolved list
// (e.g. the account renamed or dropped it), it warns and names the substitute, so
// a model rename is observable rather than silent.
func (c *Client) ResolveModels(ctx context.Context) {
	c.fetchModels(ctx)
	c.modelMu.Lock()
	count, source := len(c.modelOrder), c.modelsSource
	_, haveDefault := c.models[c.cfg.DefaultModel]
	var substitute string
	if !haveDefault && len(c.modelOrder) > 0 {
		substitute = c.modelOrder[0]
	}
	c.modelMu.Unlock()
	c.log.Info("resolved gemini model list", "count", count, "source", source)
	if c.cfg.DefaultModel != "" && !haveDefault && substitute != "" {
		c.log.Warn("configured default model not in account list; using first available instead",
			"configured", c.cfg.DefaultModel, "using", substitute,
			"hint", "set GEMINI_DEFAULT_MODEL to a name from /v1/models")
	}
}

// fetchModels queries the dynamic model list and replaces the registry. On
// failure it keeps an existing good list rather than downgrading; only when the
// registry is still empty (startup) does it install the fallback set. Routine
// refreshes log at Debug; the one-time startup summary is logged by ResolveModels.
func (c *Client) fetchModels(ctx context.Context) error {
	c.markCatalogTried()
	models, accountStatus, err := c.dynamicModelsWithMeta(ctx)
	if err == nil {
		c.modelMu.Lock()
		prevCount := len(c.modelOrder)
		prevSource := c.modelsSource
		c.modelMu.Unlock()
		if len(models) <= 1 && prevCount >= 2 && (prevSource == "dynamic" || prevSource == "persisted") {
			c.modelMu.Lock()
			c.sessionDegraded = true
			c.sessionDegradedAt = time.Now()
			c.sessionAccountStatus = accountStatus
			c.modelMu.Unlock()
			if c.catalogWarnDue() {
				c.log.Warn("degraded model catalog rejected; session weakened (refresh cookie)",
					"received", len(models), "kept", prevCount, "account_status", accountStatus)
			}
			return ErrCatalogDegraded
		}
		c.modelMu.Lock()
		wasDegraded := c.sessionDegraded && len(models) >= 2
		if len(models) >= 2 {
			c.sessionDegraded = false
			c.sessionDegradedAt = time.Time{}
			c.sessionAccountStatus = 0
		}
		c.modelMu.Unlock()
		c.storeModels(models, "dynamic")
		c.modelMu.Lock()
		c.catalogAt = nowFn().Round(0)
		c.modelMu.Unlock()
		c.saveState()
		if wasDegraded {
			c.log.Info("full model catalog restored; session weakened state cleared", "count", len(models))
		} else {
			c.log.Debug("refreshed gemini model list", "count", len(models), "source", "dynamic")
		}
		return nil
	}

	c.modelMu.Lock()
	have := len(c.modelOrder) > 0
	c.modelMu.Unlock()
	if have {
		// A transient refresh failure must not downgrade a good dynamic list. It is
		// logged at WARN (AUDIT-13 O-02), at most once per catalogWarnEvery.
		if c.catalogWarnDue() {
			c.log.Warn("model list refresh failed; keeping current list", "err", cleanErr(err))
		}
		return err
	}
	if c.currentAt() == "" {
		c.log.Warn("no authenticated session for model list; using fallback model list")
	} else {
		c.log.Warn("dynamic model list unavailable; using fallback model list", "err", err)
	}
	c.storeModels(fallbackModels(c.cfg.DefaultModel), "fallback")
	return err
}

// catalogWarnEvery rate-limits the "model list refresh failed" WARN, and
// absentModelWarnEvery rate-limits the "model absent from the last catalogue"
// WARN per model (vars so tests can shrink them).
var (
	catalogWarnEvery     = time.Hour
	absentModelWarnEvery = time.Hour
)

// catalogWarnDue reports whether the refresh-failure WARN may be emitted now.
func (c *Client) catalogWarnDue() bool {
	c.modelMu.Lock()
	defer c.modelMu.Unlock()
	if !c.catalogWarnAt.IsZero() && time.Since(c.catalogWarnAt) < catalogWarnEvery {
		return false
	}
	c.catalogWarnAt = time.Now()
	return true
}

// dynamicModels fetches and parses the account's model catalog via the
// listModels RPC. It requires an authenticated session.
func (c *Client) dynamicModels(ctx context.Context) ([]*AvailableModel, error) {
	models, _, err := c.dynamicModelsWithMeta(ctx)
	return models, err
}

func (c *Client) dynamicModelsWithMeta(ctx context.Context) ([]*AvailableModel, int, error) {
	if c.currentAt() == "" {
		return nil, 0, errors.New("session is not authenticated")
	}
	raw, err := c.batchExecute(ctx, listModelsRPC, "[]")
	if err != nil {
		return nil, 0, err
	}
	inner, ok := parseBatchResponse(raw, listModelsRPC)
	if !ok {
		return nil, 0, fmt.Errorf("%s: wrb.fr entry not found in response", listModelsRPC)
	}
	return parseModelsWithMeta(inner)
}

// knownFallbackModels holds known Gemini web slugs (current and legacy aliases)
// with their model ids and mode codes so requests remain usable in fallback mode
// when the startup catalog call fails or is blocked (N8).
var knownFallbackModels = map[string]AvailableModel{
	"gemini-auto": {
		Name: "gemini-auto", ModelID: "8c46e95b1a07cecc",
		DisplayName: "Auto", Description: "Auto",
		Capacity: 2, CapacityField: 12, ModeCode: 6,
	},
	"gemini-quick-answers": {
		Name: "gemini-quick-answers", ModelID: "8c46e95b1a07cecc",
		DisplayName: "Auto", Description: "Auto",
		Capacity: 2, CapacityField: 12, ModeCode: 6,
	},
	"gemini-3.5-flash": {
		Name: "gemini-3.5-flash", ModelID: "8c46e95b1a07cecc",
		DisplayName: "Auto", Description: "Auto",
		Capacity: 2, CapacityField: 12, ModeCode: 6,
	},
	"gemini-3.5-flash-lite": {
		Name: "gemini-3.5-flash-lite", ModelID: "8c46e95b1a07cecc",
		DisplayName: "Auto", Description: "Auto",
		Capacity: 2, CapacityField: 12, ModeCode: 6,
	},
	"gemini-everyday-help": {
		Name: "gemini-everyday-help", ModelID: "56fdd199312815e2",
		DisplayName: "4 Hydrogen", Description: "4 Hydrogen",
		Capacity: 2, CapacityField: 12, ModeCode: 1,
	},
	"gemini-4-hydrogen": {
		Name: "gemini-4-hydrogen", ModelID: "56fdd199312815e2",
		DisplayName: "4 Hydrogen", Description: "4 Hydrogen",
		Capacity: 2, CapacityField: 12, ModeCode: 1,
	},
	"gemini-all-around-help": {
		Name: "gemini-all-around-help", ModelID: "56fdd199312815e2",
		DisplayName: "4 Hydrogen", Description: "4 Hydrogen",
		Capacity: 2, CapacityField: 12, ModeCode: 1,
	},
	"gemini-deeper-analysis": {
		Name: "gemini-deeper-analysis", ModelID: "e6fa609c3fa255c0",
		DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0",
		Capacity: 2, CapacityField: 12, ModeCode: 3,
	},
	"gemini-detailed-research": {
		Name: "gemini-detailed-research", ModelID: "e6fa609c3fa255c0",
		DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0",
		Capacity: 2, CapacityField: 12, ModeCode: 3,
	},
	"gemini-3.1-pro": {
		Name: "gemini-3.1-pro", ModelID: "e6fa609c3fa255c0",
		DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0",
		Capacity: 2, CapacityField: 12, ModeCode: 3,
	},
	"gemini-4-neon": {
		Name: "gemini-4-neon", ModelID: "e6fa609c3fa255c0",
		DisplayName: "Neon", Description: "4 Neon", Family: "Neon4p0",
		Capacity: 2, CapacityField: 12, ModeCode: 3,
	},
	"gemini-code-complex-work": {
		Name: "gemini-code-complex-work", ModelID: "95a4c0abcba24364",
		DisplayName: "Code & Analysis", Description: "4 Argon",
		Capacity: 2, CapacityField: 12, ModeCode: 8,
	},
	"gemini-code-&-complex-work": {
		Name: "gemini-code-&-complex-work", ModelID: "95a4c0abcba24364",
		DisplayName: "Code & Analysis", Description: "4 Argon",
		Capacity: 2, CapacityField: 12, ModeCode: 8,
	},
	"gemini-most-complex-work": {
		Name: "gemini-most-complex-work", ModelID: "95a4c0abcba24364",
		DisplayName: "Code & Analysis", Description: "4 Argon",
		Capacity: 2, CapacityField: 12, ModeCode: 8,
	},
	"gemini-4-argon": {
		Name: "gemini-4-argon", ModelID: "95a4c0abcba24364",
		DisplayName: "Code & Analysis", Description: "4 Argon",
		Capacity: 2, CapacityField: 12, ModeCode: 8,
	},
	"gemini-4-carbon": {
		Name: "gemini-4-carbon", ModelID: "95a4c0abcba24364",
		DisplayName: "Code & Analysis", Description: "4 Argon",
		Capacity: 2, CapacityField: 12, ModeCode: 8,
	},
}

// fallbackModels returns the model set used when the dynamic list cannot be
// fetched (anonymous session, an expired cookie, or an RPC failure). In that mode
// there are no real model ids to select anything other than the backend's default
// (Flash) model, so listing additional names would be fictitious — the catalog is
// honestly just the configured default, and requesting any other name returns 400.
func fallbackModels(defaultModel string) []*AvailableModel {
	if defaultModel == "" {
		defaultModel = "gemini-3.5-flash"
	}
	return []*AvailableModel{{
		Name:          defaultModel,
		Description:   "Default model (the only model available without an authenticated cookie)",
		Capacity:      1,
		CapacityField: 12,
	}}
}

// ─── batchexecute + parsing ──────────────────────────────────────────────────

// batchExecute POSTs a single batchexecute RPC and returns the raw response
// body. It reuses the same cookie / SAPISIDHASH / build-label / at-token
// machinery as StreamGenerate. The call is authenticated; an anonymous session
// (no "at" token) will not receive account data.
func (c *Client) batchExecute(ctx context.Context, rpcid, payload string) (string, error) {
	reqid := time.Now().Unix() % 1000000
	prefix := c.accountPrefix()
	q := url.Values{}
	q.Set("rpcids", rpcid)
	q.Set("source-path", "/app")
	q.Set("bl", c.CurrentBL())
	q.Set("hl", "en")
	q.Set("_reqid", strconv.FormatInt(reqid, 10))
	q.Set("rt", "c")
	u := geminiBaseURL + prefix + "/_/BardChatUi/data/batchexecute?" + q.Encode()

	freq, err := json.Marshal([]any{[]any{[]any{rpcid, payload, nil, "generic"}}})
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("f.req", string(freq))
	if at := c.currentAt(); at != "" {
		form.Set("at", at)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", geminiBaseURL)
	req.Header.Set("Referer", geminiBaseURL+prefix+"/app")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("x-goog-ext-525001261-jspb", genericModelHeader)
	if prefix != "" {
		req.Header.Set("X-Goog-AuthUser", c.cfg.AuthUser)
	}
	cookie, sapisid := c.loadCookie()
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if sapisid != "" {
		req.Header.Set("Authorization", makeSapisidHash(sapisid))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", cleanErr(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("batchexecute %s: http %d", rpcid, resp.StatusCode)
	}
	return string(body), nil
}

// parseBatchResponse strips the )]}' XSSI guard, decodes the consecutive JSON
// values of a batchexecute response, and returns the inner JSON payload of the
// ["wrb.fr","<rpcid>",...] entry.
func parseBatchResponse(raw, rpcid string) (string, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, ")]}'")
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var v any
		if err := dec.Decode(&v); err != nil {
			break // EOF or an unreadable length token; stop scanning
		}
		if s, ok := searchWrb(v, rpcid); ok {
			return s, true
		}
	}
	return "", false
}

// searchWrb recursively looks for a ["wrb.fr","<rpcid>","<inner-json>",…] entry
// and returns the inner JSON string.
func searchWrb(v any, rpcid string) (string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return "", false
	}
	if len(arr) >= 3 {
		if a0, _ := arr[0].(string); a0 == "wrb.fr" {
			if a1, _ := arr[1].(string); a1 == rpcid {
				if s, ok := arr[2].(string); ok {
					return s, true
				}
			}
		}
	}
	for _, child := range arr {
		if s, ok := searchWrb(child, rpcid); ok {
			return s, true
		}
	}
	return "", false
}

// parseModels parses the inner JSON of the listModels RPC into the available
// model list. body[15] is the model array; body[16]/body[17] are the tier and
// capability flags fed to computeCapacity.
func parseModels(innerJSON string) ([]*AvailableModel, error) {
	out, _, err := parseModelsWithMeta(innerJSON)
	return out, err
}

func parseModelsWithMeta(innerJSON string) ([]*AvailableModel, int, error) {
	var body []any
	if err := json.Unmarshal([]byte(innerJSON), &body); err != nil {
		return nil, 0, err
	}
	accountStatus := accountStatusAt(body)
	tier := floatList(body, 16)
	capFlags := floatList(body, 17)
	capacity, capField := computeCapacity(tier, capFlags)

	rawModels := anyList(body, 15)
	out := make([]*AvailableModel, 0, len(rawModels))
	seen := make(map[string]bool)
	for _, rm := range rawModels {
		ml, ok := rm.([]any)
		if !ok {
			continue
		}
		id := strAt(ml, 0)
		if id == "" {
			continue
		}
		name := modelSlug(strAt(ml, 11), strAt(ml, 1), id)
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, &AvailableModel{
			Name:          name,
			ModelID:       id,
			DisplayName:   strAt(ml, 1),
			Description:   strAt(ml, 2),
			MenuLabel:     strAt(ml, 11),
			Family:        strAt(ml, 8),
			ModeCode:      intAt(ml, 17),
			Capacity:      capacity,
			CapacityField: capField,
		})
	}
	if len(out) == 0 {
		return nil, accountStatus, errors.New("no models in listModels response")
	}
	enrichModelsFromMenu(out, body)
	return out, accountStatus, nil
}

type menuCatalogEntry struct {
	id     string
	mode   int
	family string
	locked bool
	levels []int
}

func enrichModelsFromMenu(models []*AvailableModel, body []any) {
	menuOuter := anyList(body, 24)
	if len(menuOuter) < 2 {
		return
	}
	rawMenu, ok := menuOuter[1].([]any)
	if !ok || len(rawMenu) == 0 {
		return
	}
	entries := make([]menuCatalogEntry, 0, len(rawMenu))
	for _, item := range rawMenu {
		row, ok := item.([]any)
		if !ok || len(row) == 0 {
			continue
		}
		meta, ok := row[0].([]any)
		if !ok {
			continue
		}
		var levels []int
		if len(row) > 1 {
			if rawLevels, ok := row[1].([]any); ok {
				for _, lv := range rawLevels {
					switch val := lv.(type) {
					case float64:
						if val > 0 && val <= 100 && val == float64(int(val)) {
							levels = append(levels, int(val))
						}
					case []any:
						if f, ok := indexAny(val, 0).(float64); ok && f > 0 && f <= 100 && f == float64(int(f)) {
							levels = append(levels, int(f))
						}
					}
				}
			}
		}
		entries = append(entries, menuCatalogEntry{
			id:     strings.TrimSpace(strAt(meta, 0)),
			mode:   intAt(meta, 1),
			family: strings.TrimSpace(strAt(meta, 8)),
			locked: indexAny(meta, 14) != nil,
			levels: levels,
		})
	}
	if len(entries) == 0 {
		return
	}
	for _, m := range models {
		if m == nil {
			continue
		}
		var matched []menuCatalogEntry
		if m.Family != "" {
			for _, e := range entries {
				if e.family != "" && strings.EqualFold(e.family, m.Family) {
					matched = append(matched, e)
				}
			}
		}
		if len(matched) == 0 && m.ModelID != "" {
			for _, e := range entries {
				if e.id != "" && e.id == m.ModelID {
					matched = append(matched, e)
				}
			}
		}
		if len(matched) == 0 && m.ModeCode > 0 {
			for _, e := range entries {
				if e.mode == m.ModeCode && (m.Family == "" || e.family == "" || strings.EqualFold(e.family, m.Family)) {
					matched = append(matched, e)
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		allLocked := true
		var bestLevels []int
		for _, e := range matched {
			if !e.locked {
				allLocked = false
				if len(e.levels) > len(bestLevels) {
					bestLevels = e.levels
				}
			}
		}
		if len(bestLevels) == 0 {
			for _, e := range matched {
				if len(e.levels) > len(bestLevels) {
					bestLevels = e.levels
				}
			}
		}
		m.Locked = allLocked
		if len(bestLevels) > 0 {
			seenLv := map[int]bool{}
			merged := make([]int, 0, len(bestLevels))
			for _, lv := range bestLevels {
				if !seenLv[lv] {
					seenLv[lv] = true
					merged = append(merged, lv)
				}
			}
			m.ThinkingLevels = merged
		}
	}
}

// computeCapacity maps the account tier/capability flags onto Gemini's
// (capacity, capacity_field) pair (ported from HanaokaYuzu's compute_capacity).
func computeCapacity(tier, capFlags []float64) (capacity, capacityField int) {
	switch {
	case slices.Contains(tier, 21):
		return 1, 13
	case slices.Contains(tier, 22):
		return 2, 13
	case slices.Contains(capFlags, 115):
		return 4, 12 // Plus
	case slices.Contains(tier, 16) || slices.Contains(capFlags, 106):
		return 3, 12 // Pro (uncommon)
	case slices.Contains(tier, 8) || (!slices.Contains(capFlags, 106) && slices.Contains(capFlags, 19)):
		return 2, 12 // Pro
	default:
		return 1, 12 // Free
	}
}

// modelSlug derives the public model name: from the versioned name ([11]) when
// present, else the short label ([1]), else gemini-<id>.
func modelSlug(versioned, label, id string) string {
	if s := slugifyModelName(versioned); s != "" && s != "gemini-" {
		return s
	}
	if s := slugifyModelName(label); s != "" && s != "gemini-" {
		return s
	}
	return "gemini-" + id
}

// slugifyModelName turns a versioned model name (e.g. "3.1 Flash-Lite" or
// "Code & complex work") into a clean slug ("gemini-3.1-flash-lite",
// "gemini-code-complex-work"): lowercased, dots and single hyphens preserved,
// any sequence outside [a-z0-9.] collapsed to a single hyphen and trimmed.
func slugifyModelName(versioned string) string {
	var b strings.Builder
	b.Grow(len(versioned) + len("gemini-"))
	lastDash := true // avoid leading '-'
	for _, r := range strings.ToLower(versioned) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		return ""
	}
	return "gemini-" + s
}

// ─── Small JSON-array accessors ──────────────────────────────────────────────

// floatList returns arr[i] as a []float64 (non-numeric elements skipped).
func floatList(arr []any, i int) []float64 {
	l, ok := indexAny(arr, i).([]any)
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(l))
	for _, x := range l {
		if f, ok := x.(float64); ok {
			out = append(out, f)
		}
	}
	return out
}

// anyList returns arr[i] as a []any (nil when absent or not an array).
func anyList(arr []any, i int) []any {
	l, _ := indexAny(arr, i).([]any)
	return l
}

// strAt returns arr[i] as a string (empty when absent or not a string).
func strAt(arr []any, i int) string {
	s, _ := indexAny(arr, i).(string)
	return s
}

// indexAny returns arr[i] or nil when out of range.
func indexAny(arr []any, i int) any {
	if i < 0 || i >= len(arr) {
		return nil
	}
	return arr[i]
}

// intAt returns arr[i] as a small positive integer, or 0.
func intAt(arr []any, i int) int {
	f, ok := indexAny(arr, i).(float64)
	if !ok || f < 0 || f > 1000 || f != float64(int(f)) {
		return 0
	}
	return int(f)
}

// accountStatusAt reads body[14] from listModels without intAt's <=1000 cap.
func accountStatusAt(arr []any) int {
	f, ok := indexAny(arr, 14).(float64)
	if !ok || f < 0 || f > 1_000_000 || f != float64(int(f)) {
		return 0
	}
	return int(f)
}
