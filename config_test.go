package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
	"github.com/n0madic/go-gemini-web2api/pkg/gemini"
)

func TestUnquote(t *testing.T) {
	cases := map[string]string{
		`"hello"`: "hello",
		`'world'`: "world",
		`plain`:   "plain",
		`"mix'`:   `"mix'`,
		`""`:      "",
	}
	for in, want := range cases {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGetEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"true", true}, {"1", true}, {"yes", true}, {"on", true},
		{"false", false}, {"0", false}, {"no", false}, {"off", false},
	}
	for _, tc := range cases {
		t.Setenv("GEMINI_TEST_BOOL", tc.val)
		if got := getEnvBool("GEMINI_TEST_BOOL", !tc.want); got != tc.want {
			t.Errorf("getEnvBool(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
	if got := getEnvBool("GEMINI_TEST_BOOL_MISSING", true); !got {
		t.Errorf("missing key should return default")
	}
}

func TestGetEnvInt(t *testing.T) {
	t.Setenv("GEMINI_TEST_INT", "42")
	if got := getEnvInt("GEMINI_TEST_INT", 7); got != 42 {
		t.Errorf("getEnvInt = %d, want 42", got)
	}
	t.Setenv("GEMINI_TEST_INT", "notanumber")
	if got := getEnvInt("GEMINI_TEST_INT", 7); got != 7 {
		t.Errorf("getEnvInt with bad value = %d, want default 7", got)
	}
}

func TestSplitKeys(t *testing.T) {
	got := splitKeys("a, b ,, c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitKeys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if splitKeys("  ") != nil {
		t.Errorf("blank input should return nil")
	}
}

func TestLoadDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "" +
		"# a comment\n" +
		"\n" +
		"GEMINI_TEST_DOTENV_A=value_a\n" +
		"export GEMINI_TEST_DOTENV_B=\"quoted value\"\n" +
		"GEMINI_TEST_DOTENV_C='single'\n" +
		"GEMINI_TEST_DOTENV_PRESET=fromfile\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// Pre-set one var to verify dotenv does NOT override existing env.
	t.Setenv("GEMINI_TEST_DOTENV_PRESET", "fromenv")

	for _, k := range []string{"GEMINI_TEST_DOTENV_A", "GEMINI_TEST_DOTENV_B", "GEMINI_TEST_DOTENV_C"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	loadDotenv(path)

	checks := map[string]string{
		"GEMINI_TEST_DOTENV_A":      "value_a",
		"GEMINI_TEST_DOTENV_B":      "quoted value",
		"GEMINI_TEST_DOTENV_C":      "single",
		"GEMINI_TEST_DOTENV_PRESET": "fromenv", // not overridden
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestLoadDotenvMissingFile(t *testing.T) {
	// Should not panic or error on a missing file.
	loadDotenv(filepath.Join(t.TempDir(), "does-not-exist.env"))
}

func TestLoadConfigBuildLabelAuto(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "absent.env")
	prev, had := os.LookupEnv("GEMINI_BL")
	t.Cleanup(func() {
		if had {
			os.Setenv("GEMINI_BL", prev)
		} else {
			os.Unsetenv("GEMINI_BL")
		}
	})

	os.Unsetenv("GEMINI_BL")
	if cfg := loadConfig(noEnv); !cfg.Gemini.GeminiBLAuto || cfg.Gemini.GeminiBL != gemini.DefaultBuildLabel {
		t.Errorf("unset: auto=%v bl=%q", cfg.Gemini.GeminiBLAuto, cfg.Gemini.GeminiBL)
	}

	os.Setenv("GEMINI_BL", "auto")
	if cfg := loadConfig(noEnv); !cfg.Gemini.GeminiBLAuto || cfg.Gemini.GeminiBL != gemini.DefaultBuildLabel {
		t.Errorf("auto: auto=%v bl=%q", cfg.Gemini.GeminiBLAuto, cfg.Gemini.GeminiBL)
	}

	os.Setenv("GEMINI_BL", "boq_custom_123")
	if cfg := loadConfig(noEnv); cfg.Gemini.GeminiBLAuto || cfg.Gemini.GeminiBL != "boq_custom_123" {
		t.Errorf("explicit: auto=%v bl=%q", cfg.Gemini.GeminiBLAuto, cfg.Gemini.GeminiBL)
	}
}

func TestLoadConfigUsesConfigPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.env")
	content := "GEMINI_LISTEN=0.0.0.0:9099\nGEMINI_DEFAULT_MODEL=gemini-3.1-pro\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	// Ensure the keys are unset so the file values apply, restoring afterwards.
	for _, k := range []string{"GEMINI_LISTEN", "GEMINI_DEFAULT_MODEL"} {
		prev, had := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, prev)
			} else {
				os.Unsetenv(k)
			}
		})
	}

	cfg := loadConfig(path)
	if cfg.Listen != "0.0.0.0:9099" {
		t.Errorf("Listen = %q, want 0.0.0.0:9099", cfg.Listen)
	}
	if cfg.Gemini.DefaultModel != "gemini-3.1-pro" {
		t.Errorf("DefaultModel = %q, want gemini-3.1-pro", cfg.Gemini.DefaultModel)
	}
}

func TestNormalizeListen(t *testing.T) {
	cases := map[string]string{
		"":                "127.0.0.1:8081", // default
		"  ":              "127.0.0.1:8081", // whitespace → default
		"127.0.0.1:8081":  "127.0.0.1:8081", // full host:port
		":8081":           "127.0.0.1:8081", // port-only → loopback
		"8081":            "127.0.0.1:8081", // bare port → loopback
		"0.0.0.0:9000":    "0.0.0.0:9000",   // explicit wildcard
		"192.168.1.5:443": "192.168.1.5:443",
	}
	for in, want := range cases {
		if got := normalizeListen(in); got != want {
			t.Errorf("normalizeListen(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateStartup(t *testing.T) {
	if err := validateStartup(&Config{}); err == nil || !strings.Contains(err.Error(), "GEMINI_ALLOW_NO_AUTH") {
		t.Errorf("empty keys must be refused with a clear message, got %v", err)
	}
	if err := validateStartup(&Config{AllowNoAuth: true}); err != nil {
		t.Errorf("explicit opt-out must start, got %v", err)
	}
	if err := validateStartup(&Config{APIKeys: []string{"k"}}); err != nil {
		t.Errorf("with a key must start, got %v", err)
	}
}

func TestSecurityEnvVarsLoaded(t *testing.T) {
	const none = "/nonexistent/.env"
	t.Setenv("GEMINI_ALLOW_NO_AUTH", "1")
	t.Setenv("GEMINI_CORS_ORIGINS", "https://a.example, https://b.example")
	t.Setenv("GEMINI_ALLOWED_HOSTS", "proxy.lan")
	t.Setenv("GEMINI_LOG_FORMAT", "JSON")
	cfg := loadConfig(none)
	if !cfg.AllowNoAuth {
		t.Errorf("AllowNoAuth not loaded")
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[1] != "https://b.example" {
		t.Errorf("CORSOrigins = %v", cfg.CORSOrigins)
	}
	if len(cfg.AllowedHosts) != 1 || cfg.AllowedHosts[0] != "proxy.lan" {
		t.Errorf("AllowedHosts = %v", cfg.AllowedHosts)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q", cfg.LogFormat)
	}
}

func TestSecurityDefaults(t *testing.T) {
	const none = "/nonexistent/.env"
	for _, k := range []string{"GEMINI_ALLOW_NO_AUTH", "GEMINI_CORS_ORIGINS", "GEMINI_ALLOWED_HOSTS", "GEMINI_LOG_FORMAT", "GEMINI_LISTEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	cfg := loadConfig(none)
	if cfg.AllowNoAuth || len(cfg.CORSOrigins) != 0 || len(cfg.AllowedHosts) != 0 || cfg.LogFormat != "text" ||
		cfg.Listen != "127.0.0.1:8081" || cfg.SSEKeepaliveSec != 15 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestStripHistoryEchoEnvLoaded(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "none.env")
	t.Setenv("GEMINI_STRIP_HISTORY_ECHO", "")
	os.Unsetenv("GEMINI_STRIP_HISTORY_ECHO")
	if cfg := loadConfig(noEnv); cfg.DisableEchoFilter {
		t.Error("default must keep the echo filter active")
	}
	for _, v := range []string{"0", "false", "off", "OFF"} {
		t.Setenv("GEMINI_STRIP_HISTORY_ECHO", v)
		if cfg := loadConfig(noEnv); !cfg.DisableEchoFilter {
			t.Errorf("%q must disable the filter", v)
		}
	}
	t.Setenv("GEMINI_STRIP_HISTORY_ECHO", "1")
	if cfg := loadConfig(noEnv); cfg.DisableEchoFilter {
		t.Error("1 must keep the filter active")
	}
}

func TestToolPromptRulesEnvLoadedAndExposed(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "none.env")
	os.Unsetenv("GEMINI_TOOL_PROMPT_NOECHO_RULES")
	if cfg := loadConfig(noEnv); cfg.DisableToolPromptRules {
		t.Error("default must keep the rules on")
	}
	for _, v := range []string{"0", "false", "off"} {
		t.Setenv("GEMINI_TOOL_PROMPT_NOECHO_RULES", v)
		if cfg := loadConfig(noEnv); !cfg.DisableToolPromptRules {
			t.Errorf("%q must turn the rules off", v)
		}
	}
}

func TestStatusShowsToolPromptRulesState(t *testing.T) {
	defer apiconv.SetToolPromptRules(true)
	for _, off := range []bool{false, true} {
		cfg := testConfig()
		cfg.DisableToolPromptRules = off
		s := newTestServer(t, cfg)
		rr := httptest.NewRecorder()
		s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
		var st struct {
			R *bool `json:"tool_prompt_noecho_rules"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil || st.R == nil || *st.R == off {
			t.Errorf("off=%v: status field = %v (%v)", off, st.R, err)
		}
		if apiconv.ToolPromptRules() == off {
			t.Errorf("off=%v: package switch = %v", off, apiconv.ToolPromptRules())
		}
	}
}

func TestLot1SessionSettings(t *testing.T) {
	for _, k := range []string{"GEMINI_BLOCK_MAX_BACKOFF_MIN", "GEMINI_STATE_MAX_AGE_MIN", "GEMINI_MODELS_REFRESH_HOURS", "GEMINI_STATE_FILE", "GEMINI_COOKIE", "GEMINI_COOKIE_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("GEMINI_COOKIE_FILE", "/d/cookie.txt")
	cfg := loadConfig(filepath.Join(t.TempDir(), "none.env"))
	if cfg.Gemini.BlockMaxBackoff != time.Hour || cfg.Gemini.StateMaxAge != 24*time.Hour || cfg.ModelsRefreshHours != 0 ||
		cfg.CatalogRefreshAfterCookie || cfg.Gemini.StateFile != "/d/cookie.txt.state.json" {
		t.Errorf("defaults: backoff=%v stateAge=%v refresh=%d afterCookie=%v state=%q", cfg.Gemini.BlockMaxBackoff, cfg.Gemini.StateMaxAge, cfg.ModelsRefreshHours, cfg.CatalogRefreshAfterCookie, cfg.Gemini.StateFile)
	}
	t.Setenv("GEMINI_BLOCK_MAX_BACKOFF_MIN", "30")
	t.Setenv("GEMINI_STATE_MAX_AGE_MIN", "0") // invalid -> default
	t.Setenv("GEMINI_MODELS_REFRESH_HOURS", "24")
	t.Setenv("GEMINI_CATALOG_REFRESH_AFTER_COOKIE", "on")
	t.Setenv("GEMINI_STATE_FILE", "off")
	cfg = loadConfig(filepath.Join(t.TempDir(), "none.env"))
	if cfg.Gemini.BlockMaxBackoff != 30*time.Minute || cfg.Gemini.StateMaxAge != 24*time.Hour || cfg.ModelsRefreshHours != 24 || !cfg.CatalogRefreshAfterCookie || cfg.Gemini.StateFile != "" {
		t.Errorf("overrides: backoff=%v stateAge=%v refresh=%d afterCookie=%v state=%q", cfg.Gemini.BlockMaxBackoff, cfg.Gemini.StateMaxAge, cfg.ModelsRefreshHours, cfg.CatalogRefreshAfterCookie, cfg.Gemini.StateFile)
	}
	t.Setenv("GEMINI_IDLE_QUIET_MIN", "")
	os.Unsetenv("GEMINI_IDLE_QUIET_MIN")
	cfg = loadConfig(filepath.Join(t.TempDir(), "none.env"))
	if cfg.IdleQuietMin != 10 || cfg.Gemini.IdleQuiet != 10*time.Minute {
		t.Errorf("default IdleQuiet = (%d, %v), want (10, 10m)", cfg.IdleQuietMin, cfg.Gemini.IdleQuiet)
	}
	t.Setenv("GEMINI_IDLE_QUIET_MIN", "0")
	cfg = loadConfig(filepath.Join(t.TempDir(), "none.env"))
	if cfg.IdleQuietMin != 0 || cfg.Gemini.IdleQuiet >= 0 {
		t.Errorf("zero IdleQuiet = (%d, %v), want (0, <0)", cfg.IdleQuietMin, cfg.Gemini.IdleQuiet)
	}
}

// The history-marker neutralization (AUDIT-13 T-04) is on by default and can be
// turned off with GEMINI_NEUTRALIZE_HISTORY_MARKERS=0.
func TestMarkerNeutralizationSwitch(t *testing.T) {
	t.Setenv("GEMINI_NEUTRALIZE_HISTORY_MARKERS", "")
	os.Unsetenv("GEMINI_NEUTRALIZE_HISTORY_MARKERS")
	if cfg := loadConfig(filepath.Join(t.TempDir(), "none.env")); cfg.DisableMarkerNeutralization {
		t.Fatal("neutralization must be on by default")
	}
	t.Setenv("GEMINI_NEUTRALIZE_HISTORY_MARKERS", "0")
	if cfg := loadConfig(filepath.Join(t.TempDir(), "none.env")); !cfg.DisableMarkerNeutralization {
		t.Fatal("GEMINI_NEUTRALIZE_HISTORY_MARKERS=0 must disable neutralization")
	}
}

func TestRotateSuspendThresholdEnv(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "none.env")
	t.Setenv("GEMINI_COOKIE_ROTATE_SUSPEND_AFTER", "")
	os.Unsetenv("GEMINI_COOKIE_ROTATE_SUSPEND_AFTER")
	if got := loadConfig(noEnv).Gemini.RotateSuspendAfter; got != 3 {
		t.Fatalf("default RotateSuspendAfter = %d, want 3", got)
	}
	t.Setenv("GEMINI_COOKIE_ROTATE_SUSPEND_AFTER", "5")
	if got := loadConfig(noEnv).Gemini.RotateSuspendAfter; got != 5 {
		t.Fatalf("RotateSuspendAfter = %d, want 5", got)
	}
	t.Setenv("GEMINI_COOKIE_ROTATE_SUSPEND_AFTER", "0")
	if got := loadConfig(noEnv).Gemini.RotateSuspendAfter; got != -1 {
		t.Fatalf("RotateSuspendAfter(0) = %d, want -1", got)
	}
}

func TestTemporaryChatEnvAndStatus(t *testing.T) {
	noEnv := filepath.Join(t.TempDir(), "none.env")
	t.Setenv("GEMINI_TEMPORARY_CHAT", "")
	os.Unsetenv("GEMINI_TEMPORARY_CHAT")
	if loadConfig(noEnv).Gemini.TemporaryChat {
		t.Fatal("GEMINI_TEMPORARY_CHAT must default to false")
	}
	t.Setenv("GEMINI_TEMPORARY_CHAT", "1")
	cfg := loadConfig(noEnv)
	if !cfg.Gemini.TemporaryChat {
		t.Fatal("GEMINI_TEMPORARY_CHAT=1 must enable TemporaryChat")
	}
	s := newTestServer(t, cfg)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, newReq(http.MethodGet, "/_status", nil))
	var st struct {
		Temp *bool `json:"temporary_chat"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil || st.Temp == nil || !*st.Temp {
		t.Fatalf("/_status temporary_chat = %v (err=%v)", st.Temp, err)
	}
}
