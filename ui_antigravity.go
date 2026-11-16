package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Antigravity copies installed on this Mac, for the management page: which are
// running, their version, and whether they accept the custom models of this
// proxy (the Enabler patch) and relay keepalives. Launching is limited to the
// copies found by the scan; nothing else can be started through the page.

type agApp struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	Version      string `json:"version"`
	CustomModels bool   `json:"custom_models"` // patched copy: shows the proxy's models
	Relay        bool   `json:"relay"`         // relays keepalives: no cut at 300 s
	Running      bool   `json:"running"`
	PID          int    `json:"pid,omitempty"`
}

type agProbe struct {
	mtime               time.Time
	version             string
	customModels, relay bool
}

var (
	agMu    sync.Mutex
	agCache = map[string]agProbe{}
)

func agDirs() []string {
	dirs := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return dirs
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

// runningApps maps an .app path to the pid of its main process.
func runningApps() map[string]int {
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
	running := runningApps()
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
			p := probeApp(path)
			pid, on := running[path]
			apps = append(apps, agApp{Path: path, Name: strings.TrimSuffix(n, ".app"), Version: p.version,
				CustomModels: p.customModels, Relay: p.relay, Running: on, PID: pid})
		}
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Relay != apps[j].Relay {
			return apps[i].Relay
		}
		return apps[i].Version > apps[j].Version
	})
	return apps
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
