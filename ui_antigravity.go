package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

// Antigravity copies installed on this Mac, for the management page: which are
// running, their version, whether they accept the custom models of this proxy
// (the Enabler patch) and relay keepalives, plus safe dry-run/apply model sync
// and Enabler clone patching.

type agApp struct {
	Path            string `json:"path"`
	Name            string `json:"name"`
	Version         string `json:"version"`
	Kind            string `json:"kind"` // "official" | "clone"
	CustomModels    bool   `json:"custom_models"`
	Relay           bool   `json:"relay"`
	Running         bool   `json:"running"`
	PID             int    `json:"pid,omitempty"`
	IsLatest        bool   `json:"is_latest,omitempty"`
	HasPatchedClone bool   `json:"has_patched_clone,omitempty"`
	TargetCloneName string `json:"target_clone_name,omitempty"`
	Patchable       bool   `json:"patchable,omitempty"`
}

type agDMG struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	SizeMB  int64  `json:"size_mb"`
	ModTime string `json:"mod_time"`
}

type agProbe struct {
	mtime               time.Time
	version             string
	customModels, relay bool
}

var (
	agMu    sync.Mutex
	agCache = map[string]agProbe{}

	// Test-overridable OS / environment hooks (no real machine side effects in unit tests).
	agDirsFn         = defaultAgDirs
	runningAppsFn    = defaultRunningApps
	nodeBinFn        = defaultNodeBins
	userHomeFn       = os.UserHomeDir
	enablerRepoFn    = defaultEnablerRepo
	codesignVerifyFn = defaultCodesignVerify
)

func agDirs() []string {
	return agDirsFn()
}

func defaultAgDirs() []string {
	dirs := []string{"/Applications"}
	if home, err := userHomeFn(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return dirs
}

func defaultNodeBins() (nodePath, npmPath string, err error) {
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin"}
	findBin := func(name string) string {
		if p, lookErr := exec.LookPath(name); lookErr == nil {
			return p
		}
		for _, dir := range extra {
			cand := filepath.Join(dir, name)
			if info, statErr := os.Stat(cand); statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				return cand
			}
		}
		return ""
	}
	nodePath = findBin("node")
	npmPath = findBin("npm")
	if nodePath == "" || npmPath == "" {
		return "", "", fmt.Errorf("node/npm introuvable (node=%q, npm=%q)", nodePath, npmPath)
	}
	return nodePath, npmPath, nil
}

func defaultEnablerRepo(cfg *Config) string {
	var candidates []string
	if cfg != nil && strings.TrimSpace(cfg.EnablerRepo) != "" {
		candidates = append(candidates, strings.TrimSpace(cfg.EnablerRepo))
	}
	if env := strings.TrimSpace(os.Getenv("ENABLER_REPO")); env != "" {
		candidates = append(candidates, env)
	}
	if home, err := userHomeFn(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, "Developer", "antigravity-add-model", ".claude", "worktrees", "feat-p1-p5a-324a48"),
			filepath.Join(home, "Developer", "antigravity-add-model"),
		)
	}
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if fileExists(filepath.Join(dir, "scripts", "deploy.mjs")) &&
			fileExists(filepath.Join(dir, "scripts", "macos-clone.mjs")) {
			return dir
		}
	}
	return ""
}

func defaultCodesignVerify(ctx context.Context, appPath string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", appPath)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// compareSemver compares dotted/numeric version strings segment by segment
// (e.g. "2.33.10" > "2.33.0" > "2.32.2" > "2.32.1").
func compareSemver(a, b string) int {
	sa := parseSemverSegments(a)
	sb := parseSemverSegments(b)
	n := len(sa)
	if len(sb) > n {
		n = len(sb)
	}
	for i := 0; i < n; i++ {
		var va, vb int
		if i < len(sa) {
			va = sa[i]
		}
		if i < len(sb) {
			vb = sb[i]
		}
		if va > vb {
			return 1
		}
		if va < vb {
			return -1
		}
	}
	return 0
}

func parseSemverSegments(s string) []int {
	var out []int
	cur := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			if cur < 0 {
				cur = 0
			}
			cur = cur*10 + int(c-'0')
		} else if cur >= 0 {
			out = append(out, cur)
			cur = -1
		}
	}
	if cur >= 0 {
		out = append(out, cur)
	}
	return out
}

// probeApp reads the version and looks for the two markers in the app archive.
// The result is cached per archive modification time (the archive is large).
func probeApp(path string) agProbe {
	asar := filepath.Join(path, "Contents", "Resources", "app.asar")
	st, err := os.Stat(asar)
	if err != nil {
		return agProbe{}
	}
	agMu.Lock()
	if p, ok := agCache[path]; ok && p.mtime.Equal(st.ModTime()) {
		agMu.Unlock()
		return p
	}
	agMu.Unlock()
	p := agProbe{mtime: st.ModTime()}
	if out, err := exec.Command("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw",
		filepath.Join(path, "Contents", "Info.plist")).Output(); err == nil {
		p.version = strings.TrimSpace(string(out))
	}
	if st.Size() < 1<<30 {
		if data, err := os.ReadFile(asar); err == nil {
			p.relay = bytes.Contains(data, []byte("relayKeepaliveMs"))
			p.customModels = p.relay || bytes.Contains(data, []byte("custom_models.json"))
		}
	}
	agMu.Lock()
	agCache[path] = p
	agMu.Unlock()
	return p
}

func runningApps() map[string]int {
	return runningAppsFn()
}

// defaultRunningApps maps an .app path to the pid of its main process.
func defaultRunningApps() map[string]int {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,command=").Output()
	res := map[string]int{}
	if err != nil {
		return res
	}
	const marker = ".app/Contents/MacOS/Antigravity"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, marker)
		if i < 0 || strings.Contains(line, "Helper") {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 || sp > i {
			continue
		}
		rest := line[i+len(marker):]
		if rest != "" && rest[0] != ' ' {
			continue // e.g. ".../MacOS/Antigravity Helper"
		}
		pid := 0
		for _, c := range line[:sp] {
			if c < '0' || c > '9' {
				pid = -1
				break
			}
			pid = pid*10 + int(c-'0')
		}
		if pid > 0 {
			res[strings.TrimSpace(line[sp:i])+".app"] = pid
		}
	}
	return res
}

func listAntigravity() []agApp {
	apps := []agApp{}
	if runtime.GOOS != "darwin" {
		return apps
	}
	running := runningAppsFn()
	seenPaths := map[string]bool{}
	for _, dir := range agDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, "Antigravity") || !strings.HasSuffix(n, ".app") {
				continue
			}
			path := filepath.Join(dir, n)
			if seenPaths[path] {
				continue
			}
			seenPaths[path] = true
			p := probeApp(path)
			pid, on := running[path]
			kind := "official"
			if p.customModels || p.relay || strings.Contains(strings.ToLower(n), "add-model") {
				kind = "clone"
			}
			apps = append(apps, agApp{
				Path:         path,
				Name:         strings.TrimSuffix(n, ".app"),
				Version:      p.version,
				Kind:         kind,
				CustomModels: p.customModels,
				Relay:        p.relay,
				Running:      on,
				PID:          pid,
			})
		}
	}

	maxOfficialVer := ""
	var patchedCloneVers []string
	for _, a := range apps {
		if a.Kind == "official" && a.Version != "" {
			if maxOfficialVer == "" || compareSemver(a.Version, maxOfficialVer) > 0 {
				maxOfficialVer = a.Version
			}
		}
		if a.Kind == "clone" && a.CustomModels && a.Relay && a.Version != "" {
			patchedCloneVers = append(patchedCloneVers, a.Version)
		}
	}
	for i := range apps {
		if apps[i].Kind != "official" {
			continue
		}
		if apps[i].Version != "" {
			apps[i].IsLatest = maxOfficialVer != "" && compareSemver(apps[i].Version, maxOfficialVer) == 0
			apps[i].TargetCloneName = fmt.Sprintf("Antigravity (add-model %s P1P5a).app", apps[i].Version)
			apps[i].Patchable = true
			for _, cv := range patchedCloneVers {
				if compareSemver(cv, apps[i].Version) == 0 {
					apps[i].HasPatchedClone = true
					break
				}
			}
		}
	}

	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Relay != apps[j].Relay {
			return apps[i].Relay
		}
		if cmp := compareSemver(apps[i].Version, apps[j].Version); cmp != 0 {
			return cmp > 0
		}
		if apps[i].Kind != apps[j].Kind {
			return apps[i].Kind == "clone"
		}
		return apps[i].Name < apps[j].Name
	})
	return apps
}

func listAntigravityDMGs() []agDMG {
	out := []agDMG{}
	if runtime.GOOS != "darwin" {
		return out
	}
	home, err := userHomeFn()
	if err != nil || home == "" {
		return out
	}
	dlDir := filepath.Join(home, "Downloads")
	entries, err := os.ReadDir(dlDir)
	if err != nil {
		return out
	}
	type dmgItem struct {
		dmg agDMG
		mod time.Time
	}
	var items []dmgItem
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "Antigravity") || !strings.HasSuffix(strings.ToLower(n), ".dmg") {
			continue
		}
		full := filepath.Join(dlDir, n)
		info, statErr := os.Stat(full)
		if statErr != nil || info.IsDir() {
			continue
		}
		items = append(items, dmgItem{
			dmg: agDMG{
				Path:    full,
				Name:    n,
				SizeMB:  info.Size() / (1024 * 1024),
				ModTime: info.ModTime().UTC().Format(time.RFC3339),
			},
			mod: info.ModTime(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].mod.After(items[j].mod)
	})
	for _, it := range items {
		out = append(out, it.dmg)
	}
	return out
}

// handleUILaunch opens one of the scanned Antigravity copies. It refuses when a
// copy is already running (one instance at a time: they share their settings).
func (s *Server) handleUILaunch(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		sendError(w, http.StatusBadRequest, "invalid request")
		return
	}
	apps := listAntigravity()
	var target *agApp
	for i := range apps {
		if apps[i].Running {
			sendJSON(w, http.StatusConflict, map[string]any{"launched": false, "error": "already_running", "name": apps[i].Name})
			return
		}
		if apps[i].Path == req.Path {
			target = &apps[i]
		}
	}
	if target == nil {
		sendError(w, http.StatusBadRequest, "unknown application")
		return
	}
	if err := exec.Command("/usr/bin/open", "-na", target.Path).Run(); err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{"launched": false, "error": "open_failed"})
		return
	}
	s.log.Info("antigravity launched from the management page", "app", target.Name, "version", target.Version)
	sendJSON(w, http.StatusOK, map[string]any{"launched": true, "name": target.Name})
}

// ─── Sync custom_models.json ─────────────────────────────────────────────────

type agSyncReq struct {
	Mode           string `json:"mode"` // "dry_run" (default) | "apply"
	WithLevels     *bool  `json:"with_levels"`
	RenameExisting *bool  `json:"rename_existing"`
	KeepLegacy     *bool  `json:"keep_legacy"`
	MigrateAliases *bool  `json:"migrate_aliases"`
	Deduplicate    *bool  `json:"deduplicate"`
}

type agSyncChange struct {
	DisplayName string `json:"display_name"`
	Model       string `json:"model"`
	FromModel   string `json:"from_model,omitempty"`
	FromDisplay string `json:"from_display,omitempty"`
}

type agSyncResult struct {
	OK           bool           `json:"ok"`
	Mode         string         `json:"mode"`
	Added        []agSyncChange `json:"added"`
	Renamed      []agSyncChange `json:"renamed"`
	Migrated     []agSyncChange `json:"migrated"`
	Removed      []agSyncChange `json:"removed"`
	KeptLegacy   []agSyncChange `json:"kept_legacy"`
	Unchanged    []agSyncChange `json:"unchanged"`
	BackupPath   string         `json:"backup_path,omitempty"`
	EntriesTotal int            `json:"entries_total"`
	Summary      string         `json:"summary"`
}

type agRawMeta struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	External    string  `json:"externalModelName"`
	APIURL      string  `json:"apiUrl"`
	Description string  `json:"description"`
	Timeout     float64 `json:"timeout"`
	Relay       float64 `json:"relayKeepaliveMs"`
}

type agDocEntry struct {
	key  string // used only when container is object map
	raw  json.RawMessage
	meta agRawMeta
}

func inspectAGEntry(raw json.RawMessage) (agRawMeta, bool) {
	var m agRawMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return agRawMeta{}, false
	}
	return m, true
}

func effectiveAGExt(m agRawMeta) string {
	if strings.TrimSpace(m.External) != "" {
		return strings.TrimSpace(m.External)
	}
	if i := strings.LastIndex(m.Name, "/"); i >= 0 && i+1 < len(m.Name) {
		return strings.TrimSpace(m.Name[i+1:])
	}
	return strings.TrimSpace(m.Name)
}

func isGminiAPIURL(apiURL, port string) bool {
	if port == "" {
		return false
	}
	return strings.Contains(apiURL, "127.0.0.1:"+port) || strings.Contains(apiURL, "localhost:"+port)
}

func safeCustomSlug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastDash := true
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' {
			b.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

func agDisplayLabel(m *gemini.AvailableModel) string {
	if m == nil {
		return ""
	}
	if gemini.IsAutoModel(m.ModelID, m.Name, m.DisplayName) {
		return "Gemini Auto"
	}
	desc := strings.TrimSpace(m.Description)
	lbl := ""
	if desc != "" && utf8.RuneCountInString(desc) <= 28 && len(strings.Fields(desc)) < 4 {
		lbl = desc
	} else {
		lbl = strings.TrimSpace(m.DisplayName)
		if lbl == "" {
			lbl = desc
		}
		if lbl == "" {
			lbl = m.Name
		}
	}
	if !strings.HasPrefix(strings.ToLower(lbl), "gemini") {
		lbl = "Gemini " + lbl
	}
	return lbl
}

type agWantedEntry struct {
	Ext         string
	Name        string
	DisplayName string
	Description string
}

func buildWantedAGEntries(catalog []*gemini.AvailableModel, withLevels bool) []agWantedEntry {
	efforts := []struct{ suffix, title string }{
		{"low", "Low"},
		{"medium", "Medium"},
		{"high", "High"},
	}
	out := make([]agWantedEntry, 0, len(catalog)*3)
	for _, m := range catalog {
		if m == nil || m.Name == "" {
			continue
		}
		label := agDisplayLabel(m)
		safe := safeCustomSlug(m.Name)
		if safe == "" {
			safe = safeCustomSlug(m.ModelID)
		}
		if !withLevels || gemini.IsAutoModel(m.ModelID, m.Name, m.DisplayName) {
			out = append(out, agWantedEntry{
				Ext:         m.Name,
				Name:        "models/custom/" + safe,
				DisplayName: label + " (GMINI-Proxy)",
				Description: m.Name + " — catalogue réel Gemini Web via le proxy GMINI",
			})
			continue
		}
		for _, ef := range efforts {
			out = append(out, agWantedEntry{
				Ext:         m.Name + "@" + ef.suffix,
				Name:        "models/custom/" + safe + "-" + ef.suffix,
				DisplayName: label + " (GMINI-Proxy) (" + ef.title + ")",
				Description: m.Name + "@" + ef.suffix + " — catalogue réel Gemini Web via le proxy GMINI",
			})
		}
	}
	return out
}

func pickGminiTemplate(entries []agDocEntry, port string) (json.RawMessage, bool) {
	var gmini []agDocEntry
	for _, e := range entries {
		if isGminiAPIURL(e.meta.APIURL, port) {
			gmini = append(gmini, e)
		}
	}
	if len(gmini) == 0 {
		return nil, false
	}
	notKeepalive := func(d agDocEntry) bool {
		return !strings.Contains(strings.ToLower(d.meta.DisplayName), "keepalive test")
	}
	for _, targetBase := range []string{"gemini-deeper-analysis", "gemini-detailed-research"} {
		for _, e := range gmini {
			base, _, _ := strings.Cut(effectiveAGExt(e.meta), "@")
			if base == targetBase && notKeepalive(e) {
				return e.raw, true
			}
		}
	}
	for _, e := range gmini {
		if notKeepalive(e) {
			return e.raw, true
		}
	}
	return gmini[0].raw, true
}

func patchRawEntry(base json.RawMessage, name, ext, displayName, description string, ensureTimings bool) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(base, &obj); err != nil {
		return nil, err
	}
	setStr := func(k, v string) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		obj[k] = b
		return nil
	}
	if name != "" {
		if err := setStr("name", name); err != nil {
			return nil, err
		}
	}
	if ext != "" {
		if err := setStr("externalModelName", ext); err != nil {
			return nil, err
		}
	}
	if displayName != "" {
		if err := setStr("displayName", displayName); err != nil {
			return nil, err
		}
	}
	if description != "" {
		if err := setStr("description", description); err != nil {
			return nil, err
		}
	}
	if ensureTimings {
		meta, _ := inspectAGEntry(base)
		if meta.Timeout <= 0 {
			obj["timeout"] = json.RawMessage("1800000")
		}
		if meta.Relay <= 0 {
			obj["relayKeepaliveMs"] = json.RawMessage("15000")
		}
	}
	return json.Marshal(obj)
}

func nonGminiFingerprint(entries []agDocEntry, port string) ([]string, error) {
	var out []string
	for _, e := range entries {
		if isGminiAPIURL(e.meta.APIURL, port) {
			continue
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, e.raw); err != nil {
			return nil, err
		}
		out = append(out, e.key+"\x00"+buf.String())
	}
	return out, nil
}

func parseAGModelsDoc(data []byte) (rootMap map[string]json.RawMessage, isMap bool, entries []agDocEntry, err error) {
	if err = json.Unmarshal(data, &rootMap); err != nil {
		return nil, false, nil, err
	}
	modelsRaw, ok := rootMap["models"]
	if !ok {
		return nil, false, nil, errors.New("champ models absent")
	}
	var arr []json.RawMessage
	if errArr := json.Unmarshal(modelsRaw, &arr); errArr == nil {
		entries = make([]agDocEntry, 0, len(arr))
		for _, r := range arr {
			meta, ok := inspectAGEntry(r)
			if !ok {
				return nil, false, nil, errors.New("entrée models invalide")
			}
			entries = append(entries, agDocEntry{raw: r, meta: meta})
		}
		return rootMap, false, entries, nil
	}
	var m map[string]json.RawMessage
	if errMap := json.Unmarshal(modelsRaw, &m); errMap == nil {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			ni, ei := strconv.Atoi(keys[i])
			nj, ej := strconv.Atoi(keys[j])
			if ei == nil && ej == nil {
				return ni < nj
			}
			if ei == nil || ej == nil {
				return ei == nil
			}
			return keys[i] < keys[j]
		})
		entries = make([]agDocEntry, 0, len(keys))
		for _, k := range keys {
			r := m[k]
			meta, ok := inspectAGEntry(r)
			if !ok {
				return nil, false, nil, errors.New("entrée models map invalide")
			}
			entries = append(entries, agDocEntry{key: k, raw: r, meta: meta})
		}
		return rootMap, true, entries, nil
	}
	return nil, false, nil, errors.New("format models non reconnu")
}

func backupAntigravityModelsFile(agPath string, data []byte, now time.Time) (string, error) {
	backupsDir := filepath.Join(filepath.Dir(agPath), "backups")
	if err := os.MkdirAll(backupsDir, 0o700); err != nil {
		return "", err
	}
	ts := now.Format("20060102-150405")
	bakPath := filepath.Join(backupsDir, "custom_models.json.bak."+ts)
	for n := 2; fileExists(bakPath); n++ {
		bakPath = filepath.Join(backupsDir, fmt.Sprintf("custom_models.json.bak.%s.%d", ts, n))
	}
	if err := gemini.WriteFileAtomic(bakPath, data, 0o600); err != nil {
		return "", err
	}
	return bakPath, nil
}

func (s *Server) syncAntigravityModels(req agSyncReq) (agSyncResult, int, error) {
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "dry_run"
	}
	if mode != "dry_run" && mode != "apply" {
		return agSyncResult{}, http.StatusBadRequest, errors.New("mode invalide (dry_run ou apply attendu)")
	}
	withLevels := req.WithLevels == nil || *req.WithLevels
	renameExisting := req.RenameExisting != nil && *req.RenameExisting
	keepLegacy := req.KeepLegacy == nil || *req.KeepLegacy
	migrateAliases := req.MigrateAliases != nil && *req.MigrateAliases
	deduplicate := req.Deduplicate != nil && *req.Deduplicate

	if mode == "apply" {
		if running := runningAppsFn(); len(running) > 0 {
			return agSyncResult{}, http.StatusConflict, errors.New("antigravity_running")
		}
	}

	agPath := s.antigravityModelsFile()
	if agPath == "" {
		return agSyncResult{}, http.StatusBadRequest, errors.New("custom_models.json désactivé (GEMINI_ANTIGRAVITY_MODELS_FILE=off)")
	}
	rawBytes, err := os.ReadFile(agPath)
	if err != nil {
		return agSyncResult{}, http.StatusNotFound, fmt.Errorf("impossible de lire custom_models.json (%w)", err)
	}
	rootMap, isMap, entries, err := parseAGModelsDoc(rawBytes)
	if err != nil {
		return agSyncResult{}, http.StatusBadRequest, fmt.Errorf("custom_models.json illisible (%w)", err)
	}
	_, port, _ := net.SplitHostPort(s.cfg.Listen)
	if port == "" {
		port = "8081"
	}
	templateRaw, hasTemplate := pickGminiTemplate(entries, port)
	if !hasTemplate {
		return agSyncResult{}, http.StatusBadRequest, errors.New("aucune entrée GMINI existante à cloner dans custom_models.json")
	}
	beforeNonGmini, err := nonGminiFingerprint(entries, port)
	if err != nil {
		return agSyncResult{}, http.StatusInternalServerError, err
	}

	catalog := s.client.ListModels()
	if len(catalog) == 0 {
		return agSyncResult{}, http.StatusBadRequest, errors.New("catalogue proxy vide")
	}
	wanted := buildWantedAGEntries(catalog, withLevels)
	wantedByExt := make(map[string]agWantedEntry, len(wanted))
	wantedDisplays := make(map[string]bool, len(wanted))
	for _, w := range wanted {
		wantedByExt[w.Ext] = w
		wantedDisplays[w.DisplayName] = true
	}

	res := agSyncResult{
		OK:         true,
		Mode:       mode,
		Added:      []agSyncChange{},
		Renamed:    []agSyncChange{},
		Migrated:   []agSyncChange{},
		Removed:    []agSyncChange{},
		KeptLegacy: []agSyncChange{},
		Unchanged:  []agSyncChange{},
	}

	// Track existing exts and direct-wanted custom names so legacy duplicates sharing a `name` never survive or win.
	existingExts := map[string]bool{}
	directNames := map[string]bool{}
	seenDirectExt := map[string]bool{}
	for _, e := range entries {
		if isGminiAPIURL(e.meta.APIURL, port) {
			ext := effectiveAGExt(e.meta)
			existingExts[ext] = true
			if _, ok := wantedByExt[ext]; ok && !seenDirectExt[ext] {
				seenDirectExt[ext] = true
				if e.meta.Name != "" {
					directNames[e.meta.Name] = true
				}
			}
		}
	}

	outEntries := make([]agDocEntry, 0, len(entries)+len(wanted))
	seenWanted := map[string]bool{}
	seenNames := map[string]bool{}
	seenDisplays := map[string]bool{}
	seenExtraDisplays := map[string]bool{}
	for _, e := range entries {
		if !isGminiAPIURL(e.meta.APIURL, port) {
			outEntries = append(outEntries, e)
			continue
		}
		ext := effectiveAGExt(e.meta)
		if w, directWanted := wantedByExt[ext]; directWanted {
			finalDisp := e.meta.DisplayName
			if renameExisting {
				finalDisp = w.DisplayName
			}
			dupName := e.meta.Name != "" && seenNames[e.meta.Name]
			dupDisp := finalDisp != "" && seenDisplays[finalDisp]
			if deduplicate && (seenWanted[w.Ext] || dupName || dupDisp) {
				res.Removed = append(res.Removed, agSyncChange{
					DisplayName: e.meta.DisplayName,
					Model:       ext,
				})
				continue
			}
			seenWanted[w.Ext] = true
			if e.meta.Name != "" {
				seenNames[e.meta.Name] = true
			}
			if finalDisp != "" {
				seenDisplays[finalDisp] = true
			}
			updatedRaw := e.raw
			if renameExisting && e.meta.DisplayName != w.DisplayName {
				patched, perr := patchRawEntry(e.raw, "", "", w.DisplayName, "", false)
				if perr != nil {
					return agSyncResult{}, http.StatusInternalServerError, perr
				}
				updatedRaw = patched
				res.Renamed = append(res.Renamed, agSyncChange{
					DisplayName: w.DisplayName,
					Model:       w.Ext,
					FromDisplay: e.meta.DisplayName,
				})
			} else {
				res.Unchanged = append(res.Unchanged, agSyncChange{
					DisplayName: e.meta.DisplayName,
					Model:       ext,
				})
			}
			newMeta, _ := inspectAGEntry(updatedRaw)
			outEntries = append(outEntries, agDocEntry{key: e.key, raw: updatedRaw, meta: newMeta})
			continue
		}

		// Legacy or non-level entry.
		base, effort, hasEffort := strings.Cut(ext, "@")
		nameOccupied := deduplicate && e.meta.Name != "" && (directNames[e.meta.Name] || seenNames[e.meta.Name])
		if migrateAliases && !nameOccupied {
			if targetBase, targetID, isAlias := s.client.ResolveModelAlias(base); isAlias {
				targetExt := targetBase
				if withLevels && !gemini.IsAutoModel(targetID, targetBase, "") && hasEffort && effort != "" {
					targetExt = targetBase + "@" + effort
				}
				if w, ok := wantedByExt[targetExt]; ok && !existingExts[targetExt] && !seenWanted[targetExt] &&
					(!deduplicate || (!directNames[w.Name] && !seenNames[w.Name])) {
					migratedRaw, perr := patchRawEntry(e.raw, w.Name, w.Ext, w.DisplayName, w.Description, true)
					if perr != nil {
						return agSyncResult{}, http.StatusInternalServerError, perr
					}
					seenWanted[targetExt] = true
					existingExts[targetExt] = true
					seenNames[w.Name] = true
					seenDisplays[w.DisplayName] = true
					newMeta, _ := inspectAGEntry(migratedRaw)
					outEntries = append(outEntries, agDocEntry{key: e.key, raw: migratedRaw, meta: newMeta})
					res.Migrated = append(res.Migrated, agSyncChange{
						DisplayName: w.DisplayName,
						Model:       w.Ext,
						FromModel:   ext,
						FromDisplay: e.meta.DisplayName,
					})
					continue
				}
			}
		}

		if deduplicate {
			// Remove entries that collide with a wanted/seen displayName or name (fixes broken Argon submenu & Auto #2),
			// or obsolete Auto/bare-base variants when --levels is enabled.
			isAutoVariant := withLevels && hasEffort && effort != "" && gemini.IsAutoModel(gemini.AutoModelHexID, base, "")
			isBareWhenLevels := withLevels && !hasEffort && !gemini.IsAutoModel(gemini.AutoModelHexID, base, "") && wantedByExt[base+"@medium"].Ext != ""
			collidesDisplay := e.meta.DisplayName != "" && (wantedDisplays[e.meta.DisplayName] || seenDisplays[e.meta.DisplayName] || seenExtraDisplays[e.meta.DisplayName])
			collidesName := e.meta.Name != "" && (directNames[e.meta.Name] || seenNames[e.meta.Name])
			if collidesDisplay || collidesName || isAutoVariant || isBareWhenLevels {
				res.Removed = append(res.Removed, agSyncChange{
					DisplayName: e.meta.DisplayName,
					Model:       ext,
				})
				continue
			}
		}

		if keepLegacy {
			if e.meta.Name != "" {
				seenNames[e.meta.Name] = true
			}
			if e.meta.DisplayName != "" {
				seenExtraDisplays[e.meta.DisplayName] = true
			}
			outEntries = append(outEntries, e)
			res.KeptLegacy = append(res.KeptLegacy, agSyncChange{
				DisplayName: e.meta.DisplayName,
				Model:       ext,
			})
		} else {
			res.Removed = append(res.Removed, agSyncChange{
				DisplayName: e.meta.DisplayName,
				Model:       ext,
			})
		}
	}

	// Build existingNames from kept entries so freed custom names (e.g. gemini-code-complex-work-low) are reused cleanly.
	existingNames := map[string]bool{}
	for _, e := range outEntries {
		existingNames[e.meta.Name] = true
	}

	nextKeyNum := 0
	if isMap {
		for _, e := range outEntries {
			if n, convErr := strconv.Atoi(e.key); convErr == nil && n >= nextKeyNum {
				nextKeyNum = n + 1
			}
		}
	}

	for _, w := range wanted {
		if seenWanted[w.Ext] || existingExts[w.Ext] {
			continue
		}
		customName := w.Name
		for idx := 2; existingNames[customName]; idx++ {
			customName = fmt.Sprintf("%s-%d", w.Name, idx)
		}
		existingNames[customName] = true
		newRaw, perr := patchRawEntry(templateRaw, customName, w.Ext, w.DisplayName, w.Description, true)
		if perr != nil {
			return agSyncResult{}, http.StatusInternalServerError, perr
		}
		newMeta, _ := inspectAGEntry(newRaw)
		entry := agDocEntry{raw: newRaw, meta: newMeta}
		if isMap {
			entry.key = strconv.Itoa(nextKeyNum)
			nextKeyNum++
		}
		outEntries = append(outEntries, entry)
		res.Added = append(res.Added, agSyncChange{
			DisplayName: w.DisplayName,
			Model:       w.Ext,
		})
	}

	afterNonGmini, err := nonGminiFingerprint(outEntries, port)
	if err != nil {
		return agSyncResult{}, http.StatusInternalServerError, err
	}
	if len(beforeNonGmini) != len(afterNonGmini) {
		return agSyncResult{}, http.StatusInternalServerError, errors.New("contrôle d'invariance non-GMINI échoué (nombre modifié)")
	}
	for i := range beforeNonGmini {
		if beforeNonGmini[i] != afterNonGmini[i] {
			return agSyncResult{}, http.StatusInternalServerError, errors.New("contrôle d'invariance non-GMINI échoué (contenu modifié)")
		}
	}

	res.EntriesTotal = len(outEntries)
	changed := len(res.Added)+len(res.Renamed)+len(res.Migrated)+len(res.Removed) > 0
	res.Summary = fmt.Sprintf("%d ajouté(s), %d renommé(s), %d migré(s), %d retiré(s), %d conservé(s) (inchangés: %d, legacy: %d)",
		len(res.Added), len(res.Renamed), len(res.Migrated), len(res.Removed), len(res.Unchanged)+len(res.KeptLegacy), len(res.Unchanged), len(res.KeptLegacy))

	if mode == "apply" && changed {
		var encodedModels json.RawMessage
		if isMap {
			outMap := make(map[string]json.RawMessage, len(outEntries))
			for _, e := range outEntries {
				outMap[e.key] = e.raw
			}
			encodedModels, err = json.Marshal(outMap)
		} else {
			outArr := make([]json.RawMessage, 0, len(outEntries))
			for _, e := range outEntries {
				outArr = append(outArr, e.raw)
			}
			encodedModels, err = json.Marshal(outArr)
		}
		if err != nil {
			return agSyncResult{}, http.StatusInternalServerError, err
		}
		rootMap["models"] = encodedModels
		outDoc, err := json.MarshalIndent(rootMap, "", "  ")
		if err != nil {
			return agSyncResult{}, http.StatusInternalServerError, err
		}
		outDoc = append(outDoc, '\n')

		bakPath, err := backupAntigravityModelsFile(agPath, rawBytes, s.currentNow())
		if err != nil {
			return agSyncResult{}, http.StatusInternalServerError, fmt.Errorf("sauvegarde custom_models.json impossible : %w", err)
		}
		if err := gemini.WriteFileAtomic(agPath, outDoc, 0o600); err != nil {
			return agSyncResult{}, http.StatusInternalServerError, fmt.Errorf("écriture atomique custom_models.json échouée : %w", err)
		}
		res.BackupPath = bakPath
		s.log.Info("antigravity_models_synced",
			"added", len(res.Added), "renamed", len(res.Renamed),
			"migrated", len(res.Migrated), "removed", len(res.Removed),
			"kept_legacy", len(res.KeptLegacy))
	}
	return res, http.StatusOK, nil
}

func (s *Server) handleUIAntigravitySync(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	var req agSyncReq
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		if err != nil {
			sendError(w, http.StatusBadRequest, "requête illisible")
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &req); err != nil {
				sendError(w, http.StatusBadRequest, "JSON invalide")
				return
			}
		}
	}
	res, status, err := s.syncAntigravityModels(req)
	if err != nil {
		code := status
		if code == 0 {
			code = http.StatusBadRequest
		}
		errCode := "sync_failed"
		if err.Error() == "antigravity_running" {
			errCode = "antigravity_running"
		}
		sendJSON(w, code, map[string]any{"ok": false, "error": errCode, "message": err.Error()})
		return
	}
	sendJSON(w, status, res)
}

// ─── Antigravity clone & patch job ───────────────────────────────────────────

type agPatchJob struct {
	State         string   `json:"state"` // idle | running | succeeded | failed
	SourcePath    string   `json:"source_path,omitempty"`
	SourceVersion string   `json:"source_version,omitempty"`
	TargetPath    string   `json:"target_path,omitempty"`
	Step          string   `json:"step,omitempty"`
	Logs          []string `json:"logs,omitempty"`
	Error         string   `json:"error,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	FinishedAt    string   `json:"finished_at,omitempty"`
	ArchivedClone string   `json:"archived_clone,omitempty"`
	BackupPath    string   `json:"backup_path,omitempty"`
	SyncApplied   bool     `json:"sync_applied,omitempty"`
}

type agPatchManager struct {
	mu  sync.Mutex
	job agPatchJob
}

func (s *Server) agPatchStatus() agPatchJob {
	s.agPatch.mu.Lock()
	defer s.agPatch.mu.Unlock()
	cp := s.agPatch.job
	if cp.State == "" {
		cp.State = "idle"
	}
	cp.Logs = append([]string(nil), cp.Logs...)
	return cp
}

func (s *Server) appendPatchLog(step string, lines ...string) {
	s.agPatch.mu.Lock()
	defer s.agPatch.mu.Unlock()
	if step != "" {
		s.agPatch.job.Step = step
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		s.agPatch.job.Logs = append(s.agPatch.job.Logs, line)
		if len(s.agPatch.job.Logs) > 80 {
			s.agPatch.job.Logs = s.agPatch.job.Logs[len(s.agPatch.job.Logs)-80:]
		}
	}
}

func (s *Server) finishPatchJob(state, errMsg string) {
	s.agPatch.mu.Lock()
	defer s.agPatch.mu.Unlock()
	s.agPatch.job.State = state
	s.agPatch.job.Error = errMsg
	s.agPatch.job.FinishedAt = s.currentNow().UTC().Format(time.RFC3339)
}

func (s *Server) handleUIAntigravityPatchStatus(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	sendJSON(w, http.StatusOK, s.agPatchStatus())
}

func (s *Server) handleUIAntigravityPatch(w http.ResponseWriter, r *http.Request) {
	if !s.uiAllowed(w, r, true) {
		return
	}
	s.client.RecordUserActivity()
	var req struct {
		SourcePath      string `json:"source_path"`
		Rebuild         bool   `json:"rebuild"`
		SyncModelsAfter bool   `json:"sync_models_after"`
		SyncWithLevels  *bool  `json:"sync_with_levels"`
		SyncDeduplicate *bool  `json:"sync_deduplicate"`
		SyncKeepLegacy  *bool  `json:"sync_keep_legacy"`
		SyncRename      *bool  `json:"sync_rename"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		sendError(w, http.StatusBadRequest, "invalid request")
		return
	}
	apps := listAntigravity()
	var src *agApp
	for i := range apps {
		if apps[i].Running {
			sendJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "antigravity_running", "name": apps[i].Name})
			return
		}
		if apps[i].Path == req.SourcePath && apps[i].Kind == "official" && apps[i].Version != "" {
			src = &apps[i]
		}
	}
	if src == nil {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown_source", "message": "source_path doit désigner une copie officielle détectée"})
		return
	}
	enablerRepo := enablerRepoFn(s.cfg)
	if enablerRepo == "" {
		sendJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "enabler_repo_not_found", "message": "Dépôt Enabler introuvable (GEMINI_ENABLER_REPO)"})
		return
	}
	nodeBin, npmBin, err := nodeBinFn()
	if err != nil {
		sendJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "node_not_found", "message": err.Error()})
		return
	}
	home, err := userHomeFn()
	if err != nil || home == "" {
		sendError(w, http.StatusInternalServerError, "répertoire utilisateur introuvable")
		return
	}
	targetName := src.TargetCloneName
	if targetName == "" {
		targetName = fmt.Sprintf("Antigravity (add-model %s P1P5a).app", src.Version)
	}
	targetPath := filepath.Join(home, "Applications", targetName)

	s.agPatch.mu.Lock()
	if s.agPatch.job.State == "running" {
		s.agPatch.mu.Unlock()
		sendJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "patch_already_running"})
		return
	}
	s.agPatch.job = agPatchJob{
		State:         "running",
		SourcePath:    src.Path,
		SourceVersion: src.Version,
		TargetPath:    targetPath,
		Step:          "starting",
		Logs:          []string{fmt.Sprintf("Préparation du clone patché v%s → %s", src.Version, targetName)},
		StartedAt:     s.currentNow().UTC().Format(time.RFC3339),
	}
	snap := s.agPatch.job
	s.agPatch.mu.Unlock()

	syncWithLevels := true
	if req.SyncWithLevels != nil {
		syncWithLevels = *req.SyncWithLevels
	}
	syncDedup := true
	if req.SyncDeduplicate != nil {
		syncDedup = *req.SyncDeduplicate
	}
	syncKeepLegacy := true
	if req.SyncKeepLegacy != nil {
		syncKeepLegacy = *req.SyncKeepLegacy
	}
	syncRename := false
	if req.SyncRename != nil {
		syncRename = *req.SyncRename
	}
	syncOpts := agSyncReq{
		Mode:           "apply",
		WithLevels:     &syncWithLevels,
		Deduplicate:    &syncDedup,
		KeepLegacy:     &syncKeepLegacy,
		RenameExisting: &syncRename,
	}
	go s.runAntigravityPatchJob(src.Path, src.Version, targetPath, home, enablerRepo, nodeBin, npmBin, req.Rebuild, req.SyncModelsAfter, syncOpts)
	sendJSON(w, http.StatusAccepted, snap)
}

func runNoShellCmd(ctx context.Context, dir, bin string, extraPathDirs []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	env := os.Environ()
	pathAdd := strings.Join(extraPathDirs, string(os.PathListSeparator))
	updatedPath := false
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + pathAdd + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			updatedPath = true
			break
		}
	}
	if !updatedPath {
		env = append(env, "PATH="+pathAdd)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Server) runAntigravityPatchJob(srcPath, srcVer, targetPath, home, enablerRepo, nodeBin, npmBin string, rebuild, syncAfter bool, syncOpts agSyncReq) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	extraPaths := []string{filepath.Dir(nodeBin), filepath.Dir(npmBin), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}

	// 1. Pre-check compatibility with Enabler deploy.mjs --check
	s.appendPatchLog("check", "Vérification Enabler (deploy.mjs --check)…")
	out, err := runNoShellCmd(ctx, enablerRepo, nodeBin, extraPaths,
		"scripts/deploy.mjs", "--resources", filepath.Join(srcPath, "Contents", "Resources"), "--check")
	if out != "" {
		s.appendPatchLog("check", strings.Split(out, "\n")...)
	}
	if err != nil {
		s.finishPatchJob("failed", fmt.Sprintf("deploy.mjs --check échoué : %v", err))
		return
	}

	// 2. Backup custom_models.json (0600) + archive any existing target clone
	s.appendPatchLog("backup", "Sauvegarde de sécurité…")
	if agPath := s.antigravityModelsFile(); agPath != "" {
		if raw, rerr := os.ReadFile(agPath); rerr == nil {
			if bakPath, berr := backupAntigravityModelsFile(agPath, raw, s.currentNow()); berr == nil {
				s.agPatch.mu.Lock()
				s.agPatch.job.BackupPath = bakPath
				s.agPatch.mu.Unlock()
				s.appendPatchLog("backup", "custom_models.json sauvegardé : "+filepath.Base(bakPath))
			} else {
				s.finishPatchJob("failed", fmt.Sprintf("sauvegarde custom_models.json échouée : %v", berr))
				return
			}
		}
	}
	if _, serr := os.Stat(targetPath); serr == nil {
		archDir := filepath.Join(home, "Applications", ".sauvegardes_antigravity")
		if merr := os.MkdirAll(archDir, 0o755); merr != nil {
			s.finishPatchJob("failed", fmt.Sprintf("création dossier archive impossible : %v", merr))
			return
		}
		ts := s.currentNow().Format("20060102-150405")
		archName := strings.TrimSuffix(filepath.Base(targetPath), ".app") + ".bak." + ts + ".app"
		archPath := filepath.Join(archDir, archName)
		if rerr := os.Rename(targetPath, archPath); rerr != nil {
			s.finishPatchJob("failed", fmt.Sprintf("archivage ancien clone échoué : %v", rerr))
			return
		}
		s.agPatch.mu.Lock()
		s.agPatch.job.ArchivedClone = archPath
		s.agPatch.mu.Unlock()
		s.appendPatchLog("backup", "Ancien clone archivé : "+archName)
	}

	// 3. Build Enabler if requested or dist/modelPatch is missing
	if rebuild || !fileExists(filepath.Join(enablerRepo, "dist", "proxy.js")) {
		s.appendPatchLog("build", "Compilation Enabler (npm run build)…")
		out, err = runNoShellCmd(ctx, enablerRepo, npmBin, extraPaths, "run", "build")
		if out != "" {
			s.appendPatchLog("build", strings.Split(out, "\n")...)
		}
		if err != nil {
			s.finishPatchJob("failed", fmt.Sprintf("npm run build échoué : %v", err))
			return
		}
	}

	// 4. Create patched & ad-hoc re-signed clone
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		s.finishPatchJob("failed", fmt.Sprintf("création dossier cible impossible : %v", err))
		return
	}
	s.appendPatchLog("clone", "Clonage et patch via macos-clone.mjs…")
	out, err = runNoShellCmd(ctx, enablerRepo, nodeBin, extraPaths,
		"scripts/macos-clone.mjs", "--source", srcPath, "--target", targetPath)
	if out != "" {
		s.appendPatchLog("clone", strings.Split(out, "\n")...)
	}
	if err != nil {
		s.finishPatchJob("failed", fmt.Sprintf("macos-clone.mjs échoué : %v", err))
		return
	}

	// 5. Verify codesign + probeApp relay/customModels markers
	s.appendPatchLog("verify", "Vérification signature codesign et marqueurs P1/P5a…")
	if csOut, csErr := codesignVerifyFn(ctx, targetPath); csErr != nil {
		if csOut != "" {
			s.appendPatchLog("verify", csOut)
		}
		s.finishPatchJob("failed", fmt.Sprintf("codesign --verify échoué : %v", csErr))
		return
	}
	agMu.Lock()
	delete(agCache, targetPath)
	agMu.Unlock()
	p := probeApp(targetPath)
	if !p.relay || !p.customModels {
		s.finishPatchJob("failed", fmt.Sprintf("marqueurs absents du clone (custom_models=%v, relay=%v)", p.customModels, p.relay))
		return
	}
	s.appendPatchLog("verify", fmt.Sprintf("Clone validé : v%s · custom_models=%v · relay=%v", p.version, p.customModels, p.relay))

	// 6. Optional custom_models.json sync (with chosen levels/dedup/legacy options)
	if syncAfter && s.antigravityModelsFile() != "" {
		s.appendPatchLog("sync_models", "Synchronisation des modèles vers custom_models.json…")
		syncOpts.Mode = "apply"
		syncRes, _, syncErr := s.syncAntigravityModels(syncOpts)
		if syncErr != nil {
			s.appendPatchLog("sync_models", "Synchronisation ignorée : "+syncErr.Error())
		} else {
			s.agPatch.mu.Lock()
			s.agPatch.job.SyncApplied = true
			s.agPatch.mu.Unlock()
			s.appendPatchLog("sync_models", "Synchronisation : "+syncRes.Summary)
		}
	}

	s.log.Info("antigravity_clone_patched", "source_version", srcVer, "target", filepath.Base(targetPath))
	s.finishPatchJob("succeeded", "")
}
