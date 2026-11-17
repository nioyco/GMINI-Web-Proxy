package apiconv

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Tolerant tool-call decoding and tool-name inference.
//
// Gemini web sometimes answers a tool-call fence whose JSON has no "name" (only the
// tool's arguments), or uses another key for the name/arguments. When the fence tag
// itself signals tool intent (tool_call, function_call, ...) the call is still
// recovered: variant keys are accepted and, when the name is missing, the tool is
// inferred from the tools DECLARED in the request. Anything ambiguous is NOT guessed:
// it stays text and is reported (ParseReport.Unparsed). Fences without an intent tag
// keep the strict legacy rule (name + arguments), so ordinary code is never converted.

// DeclaredTool is a tool declared by the client: its name and parameter schema keys.
type DeclaredTool struct {
	Name         string
	Props        map[string]bool   // property names of the parameters schema
	PropTypes    map[string]string // property name -> JSON schema type (when present)
	PropDefaults map[string]any    // property name -> JSON schema default (when present)
	Required     []string
}

// Unparsed describes a tool-intent fence that could not be turned into a call. It
// carries no values, only key names.
type Unparsed struct {
	Tag      string
	Keys     []string // top-level key names present (sorted)
	Declared int      // number of tools declared in the request
	Reason   string
}

// ParseReport summarises what the tolerant parser did besides plain parsing.
type ParseReport struct {
	Unparsed []Unparsed
	Inferred []string // names of tools whose name was inferred from the declared set
	// Repaired counts tool-call blocks whose JSON needed the tolerant reading
	// (invalid escape, raw line break in a string). DroppedAfter is the number of
	// bytes of text removed because they followed a tool call. CodeRefRemoved
	// counts code_reference blocks stripped because a valid tool call followed.
	Repaired       int
	DroppedAfter   int
	CodeRefRemoved int
}

// declaredFromSchema builds a DeclaredTool from a JSON-schema-like value.
func declaredFromSchema(name string, params any) DeclaredTool {
	d := DeclaredTool{Name: name, Props: map[string]bool{}, PropTypes: map[string]string{}, PropDefaults: map[string]any{}}
	if m, ok := params.(map[string]any); ok {
		if props, ok := m["properties"].(map[string]any); ok {
			for k, rawProp := range props {
				d.Props[k] = true
				if pm, ok := rawProp.(map[string]any); ok {
					if typ, ok := pm["type"].(string); ok {
						d.PropTypes[k] = strings.ToLower(strings.TrimSpace(typ))
					}
					if def, hasDef := pm["default"]; hasDef {
						d.PropDefaults[k] = def
					}
				}
			}
		}
		if req, ok := m["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					d.Required = append(d.Required, s)
				}
			}
		}
	}
	return d
}

// commandPropKey returns the command-line property name of a command tool schema, if any.
func commandPropKey(d DeclaredTool) string {
	for _, k := range []string{"CommandLine", "command", "cmd", "script"} {
		if d.Props[k] {
			return k
		}
	}
	for k := range d.Props {
		switch strings.ToLower(k) {
		case "commandline", "command", "cmd":
			return k
		}
	}
	return ""
}

// FindCommandTool returns the declared shell/command execution tool, if any.
// It matches "run_command" (and common command tool names) or any tool whose
// parameter schema declares a command property (CommandLine, command, cmd).
func FindCommandTool(declared []DeclaredTool) (DeclaredTool, bool) {
	for _, d := range declared {
		if d.Name == "run_command" {
			return d, true
		}
	}
	for _, d := range declared {
		switch strings.ToLower(d.Name) {
		case "bash", "shell", "execute_command", "exec_command", "run_shell", "terminal":
			return d, true
		}
		if commandPropKey(d) != "" {
			return d, true
		}
	}
	return DeclaredTool{}, false
}

func declaredFromTools(tools []Tool) []DeclaredTool {
	var out []DeclaredTool
	for _, td := range normalizeTools(tools) {
		if td.Name != "" {
			out = append(out, declaredFromSchema(td.Name, td.Parameters))
		}
	}
	return out
}

// DeclaredTools returns the tools declared by an OpenAI chat request.
func (r *ChatRequest) DeclaredTools() []DeclaredTool { return declaredFromTools(r.Tools) }

// DeclaredTools returns the tools declared by an OpenAI Responses request.
func (r *ResponsesRequest) DeclaredTools() []DeclaredTool { return declaredFromTools(r.Tools) }

// DeclaredTools returns the tools declared by an Anthropic request.
func (r *AnthropicRequest) DeclaredTools() []DeclaredTool {
	return declaredFromTools(anthropicToolsToTools(r.Tools))
}

// DeclaredTools returns the functions declared by a Google generateContent request.
func (r *GenerateContentRequest) DeclaredTools() []DeclaredTool {
	var out []DeclaredTool
	for _, td := range googleToolDefs(r) {
		if td.Name != "" {
			out = append(out, declaredFromSchema(td.Name, td.Parameters))
		}
	}
	return out
}

var (
	nameKeys = []string{"name", "tool", "tool_name", "function_name", "function"}
	argKeys  = []string{"arguments", "args", "parameters", "input"}
)

// resolvedCall is the outcome of resolveCall.
type resolvedCall struct {
	name     string
	args     string // JSON text
	inferred bool
}

func sortedKeys(obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stringOrName reads a JSON string, or the "name" of a JSON object.
func stringOrName(raw json.RawMessage) (name string, fn map[string]json.RawMessage) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) == nil {
		_ = json.Unmarshal(o["name"], &name)
		return name, o
	}
	return "", nil
}

// argsObject parses an arguments value (object, or a string holding a JSON object).
func argsObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var o map[string]json.RawMessage
	if json.Unmarshal(raw, &o) == nil {
		return o, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && json.Unmarshal([]byte(s), &o) == nil {
		return o, true
	}
	return nil, false
}

// resolveCall interprets one JSON object found in a tool-intent fence. On failure it
// returns a reason (and the key names for reporting).
func resolveCall(obj map[string]json.RawMessage, declared []DeclaredTool) (resolvedCall, string) {
	var name string
	var fnObj map[string]json.RawMessage
	for _, k := range nameKeys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		if n, fo := stringOrName(raw); n != "" {
			name, fnObj = n, fo
			break
		}
	}
	// Arguments: top level first, then inside a {"function": {...}} object.
	var argsRaw json.RawMessage
	for _, src := range []map[string]json.RawMessage{obj, fnObj} {
		for _, k := range argKeys {
			if raw, ok := src[k]; ok && argsRaw == nil {
				argsRaw = raw
			}
		}
	}

	if name != "" {
		args := "{}"
		if argsRaw != nil {
			args = normalizeToolArgs(argsRaw)
		}
		return resolvedCall{name: name, args: args}, ""
	}

	// No name: the arguments are either under an arguments key or the object itself.
	var argObj map[string]json.RawMessage
	var argsText string
	if argsRaw != nil {
		o, ok := argsObject(argsRaw)
		if !ok {
			return resolvedCall{}, "arguments are not a JSON object"
		}
		argObj, argsText = o, normalizeToolArgs(argsRaw)
	} else {
		argObj = obj
		b, _ := json.Marshal(obj)
		argsText = string(b)
	}
	tool, reason := inferTool(sortedKeys(argObj), declared)
	if tool == "" {
		return resolvedCall{}, reason
	}
	return resolvedCall{name: tool, args: argsText, inferred: true}, ""
}

// inferTool picks the declared tool whose schema covers every key present (and whose
// required properties are all present). Several candidates: the tightest fit wins
// (highest keys/properties ratio); a tie is never guessed.
func inferTool(keys []string, declared []DeclaredTool) (string, string) {
	if len(keys) == 0 {
		return "", "no argument keys to infer a tool from"
	}
	if len(declared) == 0 {
		return "", "no declared tools to infer the name from"
	}
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		present[k] = true
	}
	best, bestScore, ties := "", -1.0, 0
	for _, d := range declared {
		ok := true
		for _, k := range keys {
			if !d.Props[k] {
				ok = false
				break
			}
		}
		for _, r := range d.Required {
			if !present[r] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		score := float64(len(keys)) / float64(len(d.Props))
		switch {
		case score > bestScore+1e-9:
			best, bestScore, ties = d.Name, score, 1
		case score > bestScore-1e-9:
			ties++
		}
	}
	switch {
	case best == "":
		return "", "no declared tool matches the keys"
	case ties > 1:
		return "", fmt.Sprintf("ambiguous: %d declared tools fit equally", ties)
	}
	return best, ""
}

// toolIntentCalls parses one JSON value of a tool-intent fence tolerantly.
func toolIntentCalls(raw []byte, tag string, declared []DeclaredTool, rep *ParseReport) []ToolCall {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var arr []json.RawMessage
		if json.Unmarshal([]byte(trimmed), &arr) != nil {
			return nil
		}
		var out []ToolCall
		for _, e := range arr {
			out = append(out, toolIntentCalls(e, tag, declared, rep)...)
		}
		return out
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &obj) != nil {
			return nil
		}
		rc, reason := resolveCall(obj, declared)
		if reason != "" {
			rep.Unparsed = append(rep.Unparsed, Unparsed{Tag: tag, Keys: sortedKeys(obj), Declared: len(declared), Reason: reason})
			return nil
		}
		if rc.inferred {
			rep.Inferred = append(rep.Inferred, rc.name)
		}
		return []ToolCall{{ID: "call_" + hexToken(8), Type: "function",
			Function: ToolCallFunction{Name: rc.name, Arguments: rc.args}}}
	}
	return nil
}

// decodeIntentBlock decodes a tool-intent fence body (one or more JSON values).
func decodeIntentBlock(body, tag string, declared []DeclaredTool, rep *ParseReport) []ToolCall {
	vals, repaired := jsonValues(body)
	var out []ToolCall
	for _, raw := range vals {
		out = append(out, toolIntentCalls(raw, tag, declared, rep)...)
	}
	if repaired && len(out) > 0 {
		rep.Repaired++
	}
	if len(vals) == 0 && strings.TrimSpace(body) != "" {
		rep.Unparsed = append(rep.Unparsed, Unparsed{Tag: tag, Declared: len(declared), Reason: "body is not JSON"})
	}
	return out
}

func isSingleExploitableCommand(body string) bool {
	if body == "" || strings.Contains(body, "tool_call") || strings.Contains(body, "function_call") {
		return false
	}
	var lines []string
	for _, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines = append(lines, t)
	}
	if len(lines) == 0 {
		return false
	}
	if len(lines) == 1 {
		return true
	}
	// Multi-line command is treated as a single command only when it is a heredoc,
	// uses trailing backslash continuation, or is a shell compound statement.
	first := lines[0]
	if strings.Contains(first, "<<") {
		return true
	}
	if strings.HasSuffix(first, "\\") {
		return true
	}
	for _, kw := range []string{"if ", "for ", "while ", "until ", "case ", "{", "("} {
		if strings.HasPrefix(first, kw) {
			return true
		}
	}
	return false
}

func defaultArgForSchemaProp(key, typ string) any {
	switch strings.ToLower(key) {
	case "cwd", "workingdirectory", "working_directory", "workdir", "dir":
		return "."
	case "waitmsbeforeasync", "timeout_ms", "timeout":
		if typ == "" || typ == "integer" || typ == "number" {
			return 5000
		}
	case "safetoautorun", "safe_to_auto_run":
		if typ == "" || typ == "boolean" {
			return false
		}
	}
	switch typ {
	case "integer", "number":
		return 0
	case "boolean":
		return false
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	default:
		return ""
	}
}

func isCommandTool(d DeclaredTool) bool {
	if d.Name == "run_command" {
		return true
	}
	switch strings.ToLower(d.Name) {
	case "bash", "shell", "execute_command", "exec_command", "run_shell", "terminal":
		return true
	}
	return commandPropKey(d) != ""
}

func countCommandTools(declared []DeclaredTool) int {
	count := 0
	for _, d := range declared {
		if isCommandTool(d) {
			count++
		}
	}
	return count
}

func isShellCodeRefTag(tag string) bool {
	if !IsCodeReferenceTag(tag) {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(tag))
	if idx := strings.IndexAny(low, "? \t;"); idx >= 0 {
		low = low[:idx]
	}
	switch low {
	case "bash", "sh", "zsh", "shell", "console":
		return true
	}
	return false
}

// BuildCommandToolCallFromCodeRef builds a synthetic JSON tool call when text
// contains a single complete code_reference block with one exploitable command,
// a command tool is declared, and either body[24][0] names that same command tool
// or the block is an explicit shell fence with a unique declared command tool (K17 S3, K18 T1).
func BuildCommandToolCallFromCodeRef(text string, declared []DeclaredTool, body24Tools map[string]bool) (string, string, bool) {
	cmdTool, ok := FindCommandTool(declared)
	if !ok {
		return "", "", false
	}

	fences := findFences(text)
	var codeRefs []fenceBlock
	for _, fb := range fences {
		if IsCodeReferenceTag(fb.tag) {
			codeRefs = append(codeRefs, fb)
		}
	}
	if len(codeRefs) != 1 || !isClosedFence(text, codeRefs[0]) {
		return "", "", false
	}
	if len(body24Tools) > 0 {
		matchedBody24 := body24Tools[cmdTool.Name]
		if !matchedBody24 {
			for k, v := range body24Tools {
				if v && strings.EqualFold(k, cmdTool.Name) {
					matchedBody24 = true
					break
				}
			}
		}
		if !matchedBody24 {
			return "", "", false
		}
	} else if countCommandTools(declared) != 1 || !isShellCodeRefTag(codeRefs[0].tag) {
		return "", "", false
	}
	cmd := strings.TrimSpace(text[codeRefs[0].bodyStart:codeRefs[0].bodyEnd])
	if !isSingleExploitableCommand(cmd) {
		return "", "", false
	}

	cmdKey := commandPropKey(cmdTool)
	if cmdKey == "" {
		if strings.EqualFold(cmdTool.Name, "run_command") {
			cmdKey = "CommandLine"
		} else {
			cmdKey = "command"
		}
	}
	args := map[string]any{cmdKey: cmd}
	for _, reqKey := range cmdTool.Required {
		if reqKey == cmdKey {
			continue
		}
		if def, hasDef := cmdTool.PropDefaults[reqKey]; hasDef {
			args[reqKey] = def
			continue
		}
		args[reqKey] = defaultArgForSchemaProp(reqKey, cmdTool.PropTypes[reqKey])
	}
	payload := map[string]any{
		"name":      cmdTool.Name,
		"arguments": args,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", "", false
	}
	return string(b), cmdTool.Name, true
}
