package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/n0madic/go-gemini-web2api/pkg/apiconv"
)

// K17 S2: Explicit rule against built-in code execution and code_reference blocks
// appears in both K8 tool rules and B3 reminder only when a command tool is declared,
// and is controlled by GEMINI_NO_INTERNAL_CODE_EXEC_RULE (default on).
func TestK17_S2_NoInternalCodeExecRule(t *testing.T) {
	defer apiconv.SetNoInternalCodeExecRule(true)

	// 1. Declared run_command -> rule in K8 and B3.
	reqRunCmd := apiconv.ChatRequest{
		Messages: []apiconv.Message{
			{Role: "user", Content: json.RawMessage(`"ctx\n<USER_REQUEST>\nlist files\n</USER_REQUEST>"`)},
		},
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "run_command", Parameters: json.RawMessage(`{"type":"object","properties":{"CommandLine":{"type":"string"}},"required":["CommandLine"]}`)}},
		},
	}
	p, _, _ := reqRunCmd.Prompt()
	if !strings.Contains(p, `Never use built-in code execution or write a code_reference block; to run any command, call the provided "run_command" tool directly.`) {
		t.Fatalf("missing K8 code_reference rule for run_command: %s", p)
	}
	if !strings.Contains(p, `NEVER use built-in code execution or write a code_reference block; call "run_command" directly.`) {
		t.Fatalf("missing B3 reminder code_reference rule for run_command: %s", p)
	}

	// 2. Equivalent command tool detected by schema -> rule uses its name.
	reqEquiv := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}} ,
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "exec_shell", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
		},
	}
	pEquiv, _, _ := reqEquiv.Prompt()
	if !strings.Contains(pEquiv, `call the provided "exec_shell" tool directly`) {
		t.Fatalf("missing K8 rule for schema-detected command tool: %s", pEquiv)
	}
	if !strings.Contains(pEquiv, `call "exec_shell" directly`) {
		t.Fatalf("missing B3 rule for schema-detected command tool: %s", pEquiv)
	}

	// 3. No command tool declared -> rule absent.
	reqNoCmd := apiconv.ChatRequest{
		Messages: []apiconv.Message{{Role: "user", Content: json.RawMessage(`"test"`)}} ,
		Tools: []apiconv.Tool{
			{Type: "function", Function: &apiconv.ToolFunction{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"Path":{"type":"string"}}}`)}},
		},
	}
	pNoCmd, _, _ := reqNoCmd.Prompt()
	if strings.Contains(pNoCmd, "code_reference") {
		t.Fatalf("unexpected code_reference rule without command tool: %s", pNoCmd)
	}

	// 4. Toggle off -> rule absent even with run_command.
	apiconv.SetNoInternalCodeExecRule(false)
	pOff, _, _ := reqRunCmd.Prompt()
	if strings.Contains(pOff, "code_reference") {
		t.Fatalf("unexpected code_reference rule when disabled: %s", pOff)
	}

	// 5. Env var GEMINI_NO_INTERNAL_CODE_EXEC_RULE=false loads false; default true.
	if cfg := loadConfig(""); !cfg.NoInternalCodeExecRule {
		t.Fatal("default NoInternalCodeExecRule must be true")
	}
	t.Setenv("GEMINI_NO_INTERNAL_CODE_EXEC_RULE", "false")
	if cfg := loadConfig(""); cfg.NoInternalCodeExecRule {
		t.Fatal("GEMINI_NO_INTERNAL_CODE_EXEC_RULE=false must disable rule")
	}
}
