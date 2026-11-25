// Package apiconv translates the OpenAI Chat Completions, OpenAI Responses,
// Anthropic Messages, and Google generateContent request/response formats to and
// from the prompt/text shape the gemini client speaks. It owns the request DTOs
// (with their Prompt builders) and the response/SSE-frame builders the HTTP layer
// serializes.
package apiconv

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/nioyco/GMINI-Web-Proxy/pkg/gemini"
)

// toolFenceTags are language tags that signal tool-call intent. A block carrying
// one of these is treated as a tool call whenever its body names a tool, even if
// the JSON omits an "arguments" key. Blocks with any other tag (or none) must also
// carry an "arguments" key to be accepted, which avoids misreading an ordinary
// ```json answer that merely happens to contain a "name" field.
var toolFenceTags = map[string]bool{
	"tool_call":     true,
	"tool_code":     true,
	"tool_use":      true,
	"tool":          true,
	"function":      true,
	"function_call": true,
}

// ─── Incoming request types (OpenAI Chat Completions) ────────────────────────

// ChatRequest is the OpenAI /v1/chat/completions request body.
type ChatRequest struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Tools      []Tool          `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Stream     bool            `json:"stream"`
	// ReasoningEffort (low | medium | high) selects the web thinking level.
	ReasoningEffort string `json:"reasoning_effort"`
}

// Message is a single chat message. Content is polymorphic (string or an array
// of typed parts), so it is captured raw and decoded by contentText.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"` // correlates a tool-result message with its call
	ToolCalls  []ToolCall      `json:"tool_calls"`
}

// Tool is an OpenAI tool definition. It accepts both the wrapped
// ({"type":"function","function":{...}}) and flat ({"name":...}) shapes.
type Tool struct {
	Type     string        `json:"type"`
	Function *ToolFunction `json:"function"`
	// Flat fallback fields.
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolFunction is the inner function spec of a wrapped tool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is an assistant tool invocation, used both inbound and outbound.
// Index is set only on streaming chat-completion deltas, where the OpenAI schema
// requires each tool_calls entry to carry its position so clients can assemble
// calls across chunks; it is omitted everywhere else.
type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the tool name and JSON-encoded arguments string.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Prompt builds the prompt, image attachments, and tool-active flag for an OpenAI
// chat request.
func (r *ChatRequest) Prompt() (prompt string, images []gemini.InputImage, toolsActive bool) {
	tc := parseOpenAIToolChoice(r.ToolChoice)
	toolsActive = len(r.Tools) > 0 && tc.mode != "none"
	prompt = messagesToPrompt(r.Messages, r.Tools, tc)
	images = imagesFromOpenAIMessages(r.Messages)
	return prompt, images, toolsActive
}

// contentText flattens a message's polymorphic content into a plain string.
func (m Message) contentText() string {
	return rawContentText(m.Content)
}

// rawContentText decodes a content value that may be a string or an array of
// {type, text} parts (text / input_text).
func rawContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		typ, _ := p["type"].(string)
		if typ == "text" || typ == "input_text" {
			if t, ok := p["text"].(string); ok {
				if sb.Len() > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// ─── Prompt construction ─────────────────────────────────────────────────────

// toolDef is the normalized tool shape embedded into the prompt.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// normalizeTools converts the inbound tools into a uniform list.
func normalizeTools(tools []Tool) []toolDef {
	out := make([]toolDef, 0, len(tools))
	for _, t := range tools {
		name, desc := t.Name, t.Description
		var params json.RawMessage = t.Parameters
		if t.Function != nil {
			if t.Function.Name != "" {
				name = t.Function.Name
			}
			if t.Function.Description != "" {
				desc = t.Function.Description
			}
			if len(t.Function.Parameters) > 0 {
				params = t.Function.Parameters
			}
		}
		var p any
		if len(params) > 0 {
			_ = json.Unmarshal(params, &p)
		}
		if p == nil {
			p = map[string]any{}
		}
		out = append(out, toolDef{Name: name, Description: desc, Parameters: p})
	}
	return out
}

// toolChoiceMode is a provider-agnostic representation of how/whether the model
// may call tools. mode is one of "", "auto", "none", "required", "function".
type toolChoiceMode struct {
	mode string
	name string // tool name, for mode == "function"
}

// parseOpenAIToolChoice maps the OpenAI tool_choice field onto toolChoiceMode.
func parseOpenAIToolChoice(raw json.RawMessage) toolChoiceMode {
	if len(raw) == 0 {
		return toolChoiceMode{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "none", "required", "auto":
			return toolChoiceMode{mode: s}
		}
		return toolChoiceMode{}
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return toolChoiceMode{mode: "function", name: obj.Function.Name}
	}
	return toolChoiceMode{}
}

// parseAnthropicToolChoice maps the Anthropic tool_choice object onto toolChoiceMode.
func parseAnthropicToolChoice(raw json.RawMessage) toolChoiceMode {
	if len(raw) == 0 {
		return toolChoiceMode{}
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return toolChoiceMode{}
	}
	switch obj.Type {
	case "any":
		return toolChoiceMode{mode: "required"}
	case "tool":
		if obj.Name != "" {
			return toolChoiceMode{mode: "function", name: obj.Name}
		}
	case "none":
		return toolChoiceMode{mode: "none"}
	}
	return toolChoiceMode{}
}

// toolChoiceInstruction renders a constraint appended to the tool-use prompt.
func toolChoiceInstruction(tc toolChoiceMode) string {
	switch tc.mode {
	case "none":
		return "\n\nIMPORTANT: Do NOT call any tools. Respond with text only."
	case "required":
		return "\n\nIMPORTANT: You MUST call at least one tool. Do not respond with text only."
	case "function":
		return fmt.Sprintf("\n\nIMPORTANT: You MUST call the tool %q. Do not call other tools.", tc.name)
	}
	return ""
}

// toolPromptRules switches the extra rules (toolNameRule, noEchoRules) in the tool
// instructions of every route. Default on; GEMINI_TOOL_PROMPT_NOECHO_RULES=0 turns
// it off, which restores the original tool prompt byte for byte (A/B testing).
var toolPromptRules atomic.Bool

func init() { toolPromptRules.Store(true) }

// SetToolPromptRules enables or disables the extra tool-prompt rules.
func SetToolPromptRules(on bool) { toolPromptRules.Store(on) }

// ToolPromptRules reports whether the extra tool-prompt rules are enabled.
func ToolPromptRules() bool { return toolPromptRules.Load() }

// toolPromptReminder switches the short end-of-prompt reminder (B3, default on).
var toolPromptReminder atomic.Bool

func init() { toolPromptReminder.Store(true) }

// SetToolPromptReminder enables or disables the short end-of-prompt reminder (B3).
func SetToolPromptReminder(on bool) { toolPromptReminder.Store(on) }

// ToolPromptReminder reports whether the short end-of-prompt reminder is active.
func ToolPromptReminder() bool { return toolPromptReminder.Load() }

// noInternalCodeExecRule controls the explicit rule against built-in code execution
// and code_reference blocks when a command tool is declared (S2, default on).
var noInternalCodeExecRule atomic.Bool

func init() { noInternalCodeExecRule.Store(true) }

// SetNoInternalCodeExecRule enables or disables the anti-code_reference rule.
func SetNoInternalCodeExecRule(on bool) { noInternalCodeExecRule.Store(on) }

// NoInternalCodeExecRule reports whether the anti-code_reference rule is active.
func NoInternalCodeExecRule() bool { return noInternalCodeExecRule.Load() }

const shortToolPromptReminder = "Reminder: use ONLY the provided tools above.\n" +
	"Local tools are active via proxy: access local files/paths ONLY via ```tool_call``` (or `function_call` in Google format).\n" +
	"There is NO tool call limit or quota per turn: call the declared tools directly whenever needed.\n" +
	"NEVER trigger Gemini internal artifacts, widgets, web search, or any built-in assistant feature, internal tool, or external connector, and never prefix tool names with any namespace.\n" +
	"Reply directly in plain text when no tool is needed."

// insertToolPromptReminder places the short B3 reminder at the very end of the
// prompt when assistant/tool history follows the last user message (T2+), or
// just before <USER_REQUEST> (or the last user part) on the initial turn (T1).
func insertToolPromptReminder(parts []string, lastUserIdx int, cmdToolName string, googleFormat bool) []string {
	if !toolPromptReminder.Load() || !toolPromptRules.Load() {
		return parts
	}
	reminder := shortToolPromptReminder
	if cmdToolName != "" && noInternalCodeExecRule.Load() {
		fence, argsKey := "tool_call", "arguments"
		if googleFormat {
			fence, argsKey = "function_call", "args"
		}
		reminder += "\nNEVER use built-in code execution or write a code_reference block; call \"" + cmdToolName + "\" directly.\n" +
			"Do NOT output ```bash?code_reference or any shell markdown code block. To run ANY command, you MUST output:\n" +
			"```" + fence + "\n{\"name\": \"" + cmdToolName + "\", \"" + argsKey + "\": {...}}\n```"
	}
	if lastUserIdx < 0 || lastUserIdx >= len(parts) {
		return append(parts, reminder)
	}
	// On T2+ turns, [Assistant]: / [Tool result for ...]: parts follow the
	// initial user message (or in Google format the last user part itself is a
	// [Tool result for ...] block, or a trailing user wakeup like
	// <SYSTEM_MESSAGE>...</SYSTEM_MESSAGE> follows prior turns without
	// <USER_REQUEST>). Place the reminder at the very end so it stays in the
	// model's recency window.
	hasPriorHistory := false
	for _, part := range parts {
		if strings.HasPrefix(part, "[Assistant]:") ||
			strings.HasPrefix(part, "[Tool result for ") ||
			strings.Contains(part, "\n[Tool result for ") {
			hasPriorHistory = true
			break
		}
	}
	if lastUserIdx < len(parts)-1 ||
		strings.Contains(parts[lastUserIdx], "[Tool result for ") ||
		(hasPriorHistory && !strings.Contains(parts[lastUserIdx], "<USER_REQUEST>")) {
		return append(parts, reminder)
	}
	if pos := strings.LastIndex(parts[lastUserIdx], "<USER_REQUEST>"); pos >= 0 {
		u := parts[lastUserIdx]
		parts[lastUserIdx] = u[:pos] + reminder + "\n" + u[pos:]
		return parts
	}
	out := make([]string, 0, len(parts)+1)
	out = append(out, parts[:lastUserIdx]...)
	out = append(out, reminder)
	out = append(out, parts[lastUserIdx:]...)
	return out
}

// toolSpecificRules formats environment instructions for specific active tools (K8, K17 S2).
func toolSpecificRules(hasWriteFile, hasSearchWeb bool, cmdToolName string) string {
	var extra []string
	if hasWriteFile {
		extra = append(extra, "- The word \"artifact\" in instructions refers to a local file to write with the provided \"write_file\" tool, never Gemini's built-in artifact or skills features.")
	}
	if hasSearchWeb {
		extra = append(extra, "- Web searches must be performed only using the provided \"search_web\" tool, never built-in web search.")
	}
	if cmdToolName != "" && noInternalCodeExecRule.Load() {
		extra = append(extra, "- Never use built-in code execution or write a code_reference block; to run any command, call the provided \""+cmdToolName+"\" tool directly.")
	}
	if len(extra) == 0 {
		return ""
	}
	return "\n" + strings.Join(extra, "\n") + "\n"
}

func commandToolNameFromDefs(defs []toolDef) string {
	declared := make([]DeclaredTool, 0, len(defs))
	for _, d := range defs {
		if d.Name != "" {
			declared = append(declared, declaredFromSchema(d.Name, d.Parameters))
		}
	}
	if ct, ok := FindCommandTool(declared); ok {
		return ct.Name
	}
	return ""
}

func commandToolNameFromGoogleDefs(defs []googleToolDef) string {
	declared := make([]DeclaredTool, 0, len(defs))
	for _, d := range defs {
		if d.Name != "" {
			declared = append(declared, declaredFromSchema(d.Name, d.Parameters))
		}
	}
	if ct, ok := FindCommandTool(declared); ok {
		return ct.Name
	}
	return ""
}

// openAIToolRules is the text between the "output ONLY the tool_call block(s)"
// sentence and "Available tools:" (one newline when the rules are off).
func openAIToolRules(defs []toolDef) string {
	if !ToolPromptRules() {
		return "\n"
	}
	hasWriteFile, hasSearchWeb := false, false
	for _, d := range defs {
		if d.Name == "write_file" {
			hasWriteFile = true
		}
		if d.Name == "search_web" {
			hasSearchWeb = true
		}
	}
	return toolNameRule + "\n" + noEchoRules + toolSpecificRules(hasWriteFile, hasSearchWeb, commandToolNameFromDefs(defs)) + "\n\n"
}

// googleToolRules is the same for the Google function_call prompt.
func googleToolRules(toolDefs []googleToolDef) string {
	if !ToolPromptRules() {
		return "\n"
	}
	hasWriteFile, hasSearchWeb := false, false
	for _, d := range toolDefs {
		if d.Name == "write_file" {
			hasWriteFile = true
		}
		if d.Name == "search_web" {
			hasSearchWeb = true
		}
	}
	return "- " + toolNameRule + "\n" + noEchoRules + toolSpecificRules(hasWriteFile, hasSearchWeb, commandToolNameFromGoogleDefs(toolDefs)) + "\n\n"
}

// toolNameRule makes the "name" field explicit: the model once answered with the
// arguments object alone.
const toolNameRule = "The \"name\" field is REQUIRED in every tool call and must be exactly one of the available tool names; " +
	"never output an arguments object without \"name\"."

// noEchoRules forbids the model from imitating the labels the proxy uses to
// flatten the conversation history (they are input markers, not output format).
const noEchoRules = "Rules about the conversation history:\n" +
	"- Lines starting with [System instruction]:, [User]:, [Assistant]: or [Tool result for ...]: are input markers added by the system. NEVER write them in your answer.\n" +
	"- NEVER repeat or replay earlier tool results or earlier assistant messages, and NEVER write, invent or simulate a tool result: tool results are supplied only by the system after your tool call is executed.\n" +
	"- Answer only the latest request, using earlier results as context.\n" +
	"Rules about the environment:\n" +
	"- Files, folders and projects named by the user are on the user's local machine: reach them ONLY with the tools above.\n" +
	"- NEVER invoke, call, or delegate to any built-in assistant feature, web lookup, internal tool, widget, or external connector — not even on the first turn — and never prefix tool names with any namespace: use ONLY the declared local tools above.\n" +
	"- The tools above are not built-in model features: invoke them by outputting the tool call block in your reply. The user's system executes it and returns the result. There is NO tool call limit or quota per turn or per cycle. NEVER reply that tools are not declared, unavailable, or at a tool/action limit or quota, NEVER say you lack local file/command access, and NEVER simulate or invent a report without calling the tools first: invoke the tool instead.\n" +
	"- Answer in plain text or with tool_call blocks only.\n" +
	"- When several tool calls are independent (for example reading several files), output them together in the same answer, one tool_call block each: every answer is costly."

// messagesToPrompt converts OpenAI messages (plus optional tools and tool_choice)
// into a single prompt string with role markers.
func messagesToPrompt(messages []Message, tools []Tool, tc toolChoiceMode) string {
	var parts []string

	if len(tools) > 0 && tc.mode != "none" {
		defs := normalizeTools(tools)
		if len(defs) > 0 {
			defsJSON := marshalToolDefs(defs)
			parts = append(parts, "# Tool Use\n\n"+
				"You can call the following tools to help accomplish tasks. "+
				"These tools connect to the user's local environment and will execute when called.\n\n"+
				"Call format (use this exact format):\n"+
				"```tool_call\n{\"name\": \"func_name\", \"arguments\": {...}}\n```\n"+
				"When calling tools, output ONLY the tool_call block(s), nothing else.\n"+
				openAIToolRules(defs)+
				"Available tools:\n"+string(defsJSON)+
				toolChoiceInstruction(tc))
		}
	}

	ids := toolNameIndex{}
	lastUserIdx := -1
	for _, msg := range messages {
		// Contents supplied by the client cannot forge a history marker (T-04).
		content := neutralizeMarkers(msg.contentText())
		switch msg.Role {
		case "system":
			parts = append(parts, "[System instruction]: "+content)
		case "assistant":
			ids.addCalls(msg.ToolCalls)
			if len(msg.ToolCalls) > 0 {
				var tcStrs []string
				for _, tc := range msg.ToolCalls {
					args := tc.Function.Arguments
					if args == "" {
						args = "{}"
					}
					tcStrs = append(tcStrs, "```tool_call\n{\"name\": "+jsonQuote(tc.Function.Name)+", \"arguments\": "+args+"}\n```")
				}
				parts = append(parts, "[Assistant]: "+content+"\n"+strings.Join(tcStrs, "\n"))
			} else {
				if len(tools) > 0 && tc.mode != "none" {
					content = SanitizeAssistantHistory(content)
				}
				parts = append(parts, "[Assistant]: "+content)
			}
		case "tool":
			// OpenAI tool-result messages carry tool_call_id (and sometimes name):
			// label the result with the function name, found through the id in the
			// earlier assistant tool_calls; fall back to the id itself (T-03).
			parts = append(parts, "[Tool result for "+safeLabel(ids.toolResultLabel(msg))+"]: "+content)
		default:
			if content != "" {
				lastUserIdx = len(parts)
				parts = append(parts, content)
			}
		}
	}

	if len(tools) > 0 && tc.mode != "none" {
		parts = insertToolPromptReminder(parts, lastUserIdx, commandToolNameFromDefs(normalizeTools(tools)), false)
	}

	return joinNonEmpty(parts, "\n\n")
}

// ParseToolCalls extracts tool calls from the model output, returning the cleaned
// text (tool-call blocks removed) and the parsed calls. It scans fenced blocks and
// accepts those whose body is JSON naming a tool; code blocks that are not tool
// calls are preserved in the cleaned text. If no fenced tool call is found and the
// whole reply is a bare JSON tool call (no fence at all), that is parsed too.
func ParseToolCalls(text string) (string, []ToolCall) {
	clean, calls, _ := ParseToolCallsWith(text, nil)
	return clean, calls
}

// ParseToolCallsWith is ParseToolCalls with the tools declared by the request: a
// tool-intent fence (tool_call, function_call, ...) whose JSON lacks a "name" is
// recovered by inferring the tool from the declared schemas (see toolinfer.go), and
// variant key names are accepted. The report lists tool-intent fences that could
// not be converted (they stay in the text) and the names that were inferred.
func ParseToolCallsWith(text string, declared []DeclaredTool) (string, []ToolCall, ParseReport) {
	var calls []ToolCall
	var rep ParseReport
	var clean strings.Builder
	last := 0
	names := declaredNameSet(declared)

	fences := findFences(text)
	decoded := make([][]ToolCall, len(fences))
	lastValidIdx := -1
	for idx, fb := range fences {
		tag := strings.ToLower(fb.tag)
		body := strings.TrimSpace(text[fb.bodyStart:fb.bodyEnd])
		var blockCalls []ToolCall
		inferredBefore := len(rep.Inferred)
		if toolFenceTags[tag] {
			blockCalls = decodeIntentBlock(body, tag, declared, &rep)
		} else {
			blockCalls = decodeToolCalls(body, false)
		}
		if len(blockCalls) == 0 {
			continue // not a tool call: leave the block where it is
		}
		// A request that declares tools only accepts those names; anything else
		// (an example block, an invented tool) stays text and is reported.
		if bad := rejectUndeclared(blockCalls, names); len(bad) > 0 {
			noteUndeclared(&rep, tag, len(declared), bad)
			rep.Inferred = rep.Inferred[:inferredBefore]
			continue
		}
		decoded[idx] = blockCalls
		lastValidIdx = idx
	}

	for idx, fb := range fences {
		blockCalls := decoded[idx]
		isCodeRef := len(blockCalls) == 0 && IsCodeReferenceTag(fb.tag) && idx < lastValidIdx
		if len(blockCalls) == 0 && !isCodeRef {
			continue
		}
		// Text between/after tool calls is dropped (see dropAfterToolCall): only what
		// precedes the first call is kept. Code-reference blocks followed by a valid
		// tool call are removed along with the tool calls.
		if len(calls) == 0 || !DropTextAfterToolCall() {
			clean.WriteString(text[last:fb.start])
		} else {
			rep.DroppedAfter += len(strings.TrimSpace(text[last:fb.start]))
		}
		last = fb.end
		if isCodeRef {
			rep.CodeRefRemoved++
		} else {
			calls = append(calls, blockCalls...)
		}
	}
	if len(calls) == 0 || !DropTextAfterToolCall() {
		clean.WriteString(text[last:])
	} else {
		rep.DroppedAfter += len(strings.TrimSpace(text[last:]))
	}
	result := strings.TrimSpace(clean.String())

	if len(calls) == 0 {
		// Bare-JSON fallback: the entire reply is a tool call with no code fence.
		bare := toolCallsFromJSON([]byte(result), false)
		if len(bare) == 0 && result != "" && (result[0] == '{' || result[0] == '[') {
			if fixed, ins := repairJSONStrings(result); len(ins) > 0 {
				if bare = toolCallsFromJSON([]byte(fixed), false); len(bare) > 0 {
					rep.Repaired++
				}
			}
		}
		if len(bare) > 0 {
			if bad := rejectUndeclared(bare, names); len(bad) > 0 {
				noteUndeclared(&rep, "bare_json", len(declared), bad)
				return result, calls, rep
			}
			return "", bare, rep
		}
	}
	return result, calls, rep
}

// RecoverUnconvertedToolCalls is the safety-net second pass: if the text left
// after ParseToolCallsWith still contains any ```tool_call block with valid JSON
// naming one of the declared tools, it extracts those calls, removes their blocks
// from text, and returns the count of recovered blocks.
func RecoverUnconvertedToolCalls(text string, declared []DeclaredTool, hadCalls bool) (string, []ToolCall, int) {
	if text == "" || len(declared) == 0 {
		return text, nil, 0
	}
	low := strings.ToLower(text)
	const prefix = "```tool_call"
	if !strings.Contains(low, prefix) {
		return text, nil, 0
	}
	names := declaredNameSet(declared)

	type span struct{ start, end int }
	var spans []span
	var recovered []ToolCall

	pos := 0
	for pos < len(text) {
		rel := strings.Index(low[pos:], prefix)
		if rel < 0 {
			break
		}
		start := pos + rel
		afterTag := start + len(prefix)
		if afterTag < len(text) {
			c := text[afterTag]
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
				pos = afterTag
				continue
			}
		}
		jStart := afterTag
		for jStart < len(text) && (text[jStart] == ' ' || text[jStart] == '\t' || text[jStart] == '\r' || text[jStart] == '\n') {
			jStart++
		}
		if jStart < len(text) && text[jStart] != '{' && text[jStart] != '[' {
			if nl := strings.IndexByte(text[afterTag:], '\n'); nl >= 0 {
				jStart = afterTag + nl + 1
				for jStart < len(text) && isJSONSpace(text[jStart]) {
					jStart++
				}
			}
		}
		if jStart >= len(text) || (text[jStart] != '{' && text[jStart] != '[') {
			pos = afterTag
			continue
		}
		dec := json.NewDecoder(strings.NewReader(text[jStart:]))
		var blockCalls []ToolCall
		jEnd := jStart
		for {
			q := int(dec.InputOffset())
			for q < len(text)-jStart && isJSONSpace(text[jStart+q]) {
				q++
			}
			if q >= len(text)-jStart || (text[jStart+q] != '{' && text[jStart+q] != '[') {
				break
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				break
			}
			var dummyRep ParseReport
			calls := toolIntentCalls(raw, "tool_call", declared, &dummyRep)
			if len(calls) == 0 || len(dummyRep.Inferred) > 0 || len(rejectUndeclared(calls, names)) > 0 {
				blockCalls = nil
				break
			}
			blockCalls = append(blockCalls, calls...)
			jEnd = jStart + int(dec.InputOffset())
		}
		if len(blockCalls) == 0 {
			pos = afterTag
			continue
		}
		end := jEnd
		afterJSON := jEnd
		for afterJSON < len(text) && isJSONSpace(text[afterJSON]) {
			afterJSON++
		}
		if strings.HasPrefix(text[afterJSON:], "```") {
			end = afterJSON + 3
			for end < len(text) && text[end] == '`' {
				end++
			}
		}
		spans = append(spans, span{start: start, end: end})
		recovered = append(recovered, blockCalls...)
		pos = end
	}
	if len(spans) == 0 {
		return text, nil, 0
	}
	var sb strings.Builder
	last := 0
	for i, sp := range spans {
		if (i == 0 && !hadCalls) || !DropTextAfterToolCall() {
			sb.WriteString(text[last:sp.start])
		}
		last = sp.end
	}
	if !DropTextAfterToolCall() {
		sb.WriteString(text[last:])
	}
	return strings.TrimSpace(sb.String()), recovered, len(spans)
}

// decodeToolCalls reads one or more JSON values from a fenced block body and turns
// each that names a tool into a ToolCall. tagIntent reports whether the fence tag
// already signalled tool-call intent (see toolFenceTags).
func decodeToolCalls(body string, tagIntent bool) []ToolCall {
	vals, _ := jsonValues(body)
	var out []ToolCall
	for _, raw := range vals {
		out = append(out, toolCallsFromJSON(raw, tagIntent)...)
	}
	return out
}

// toolCallsFromJSON parses a single JSON value (object or array of objects) into
// tool calls. An object qualifies when it has a non-empty string "name" and either
// the fence signalled tool intent (tagIntent) or the object carries an "arguments"
// key. The whole value must be valid JSON, so trailing prose is rejected.
func toolCallsFromJSON(raw []byte, tagIntent bool) []ToolCall {
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
			out = append(out, toolCallsFromJSON(e, tagIntent)...)
		}
		return out
	case '{':
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &obj) != nil {
			return nil
		}
		var name string
		if json.Unmarshal(obj["name"], &name) != nil || name == "" {
			return nil
		}
		argsRaw, hasArgs := obj["arguments"]
		if !tagIntent && !hasArgs {
			return nil
		}
		args := "{}"
		if hasArgs {
			args = normalizeToolArgs(argsRaw)
		}
		return []ToolCall{{
			ID:       "call_" + hexToken(8),
			Type:     "function",
			Function: ToolCallFunction{Name: name, Arguments: args},
		}}
	default:
		return nil
	}
}

// normalizeToolArgs renders a tool call's arguments as the JSON string the OpenAI
// schema expects. An object/array/scalar is passed through as its JSON text; a
// double-encoded JSON string ("{...}") is unwrapped to its inner JSON; null/empty
// becomes "{}".
func normalizeToolArgs(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return "{}"
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return t
}

// joinNonEmpty joins only the non-empty parts with sep.
func joinNonEmpty(parts []string, sep string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// ─── Responses API (/v1/responses) ───────────────────────────────────────────

// ResponsesRequest is the OpenAI Responses API request body.
type ResponsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        []Tool          `json:"tools"`
	ToolChoice   json.RawMessage `json:"tool_choice"`
	Stream       bool            `json:"stream"`
}

// Prompt builds the prompt, image attachments, and tool-active flag for an OpenAI
// Responses request.
func (r *ResponsesRequest) Prompt() (prompt string, images []gemini.InputImage, toolsActive bool) {
	tc := parseOpenAIToolChoice(r.ToolChoice)
	toolsActive = len(r.Tools) > 0 && tc.mode != "none"
	prompt = messagesToPrompt(responsesToMessages(r), r.Tools, tc)
	images = imagesFromResponsesInput(r)
	return prompt, images, toolsActive
}

// responsesToMessages flattens Responses API input items into chat messages.
func responsesToMessages(req *ResponsesRequest) []Message {
	var messages []Message
	if req.Instructions != "" {
		messages = append(messages, Message{Role: "system", Content: jsonString(req.Instructions)})
	}
	if len(req.Input) == 0 {
		return messages
	}

	// input may be a bare string.
	var asString string
	if err := json.Unmarshal(req.Input, &asString); err == nil {
		messages = append(messages, Message{Role: "user", Content: jsonString(asString)})
		return messages
	}

	// Otherwise it is an array of items (strings or objects).
	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return messages
	}
	for _, raw := range items {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			messages = append(messages, Message{Role: "user", Content: jsonString(s)})
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		messages = append(messages, responsesItemToMessage(item))
	}
	return messages
}

// responsesItemToMessage converts a single Responses API input object.
func responsesItemToMessage(item map[string]any) Message {
	typ, _ := item["type"].(string)
	role, _ := item["role"].(string)

	switch {
	case typ == "function_call_output":
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		// Responses function_call_output usually omits the function name and only
		// carries call_id: messagesToPrompt resolves the name from the earlier
		// function_call items, falling back to the id.
		output := stringifyContent(item["output"])
		return Message{Role: "tool", Name: name, ToolCallID: callID, Content: jsonString(output)}

	case typ == "function_call":
		// A top-level function_call item is the assistant's earlier call: keep it
		// in the history so a later function_call_output resolves its function name.
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		if args == "" {
			args = "{}"
		}
		callID, _ := item["call_id"].(string)
		return Message{Role: "assistant", Content: jsonString(""), ToolCalls: []ToolCall{{
			ID: callID, Type: "function", Function: ToolCallFunction{Name: name, Arguments: args}}}}

	case role == "assistant":
		textAcc, toolCalls := extractAssistantContent(item["content"])
		return Message{Role: "assistant", Content: jsonString(textAcc), ToolCalls: toolCalls}

	default:
		if role == "" {
			role = "user"
		}
		content := stringifyContent(item["content"])
		return Message{Role: role, Content: jsonString(content)}
	}
}

// extractAssistantContent pulls output_text and function_call parts from a
// Responses assistant item's content array.
func extractAssistantContent(content any) (string, []ToolCall) {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s, nil
		}
		return "", nil
	}
	var sb strings.Builder
	var calls []ToolCall
	for i, p := range parts {
		cp, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch cp["type"] {
		case "output_text":
			if t, ok := cp["text"].(string); ok {
				sb.WriteString(t)
			}
		case "function_call":
			name, _ := cp["name"].(string)
			args, _ := cp["arguments"].(string)
			if args == "" {
				args = "{}"
			}
			id, _ := cp["call_id"].(string)
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			calls = append(calls, ToolCall{
				ID:       id,
				Type:     "function",
				Function: ToolCallFunction{Name: name, Arguments: args},
			})
		}
	}
	return sb.String(), calls
}

// stringifyContent renders a content value (string or array of text parts) to text.
func stringifyContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			cp, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := cp["type"].(string); typ == "text" || typ == "input_text" {
				if t, ok := cp["text"].(string); ok {
					if sb.Len() > 0 {
						sb.WriteByte(' ')
					}
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// jsonString wraps a plain string as a json.RawMessage for use as Message.Content.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// marshalToolDefs renders the tool definitions for the prompt. They are resent
// with every request, so they are compact; with the extra rules switched off the
// historical indented form is kept (byte-identical prompt).
func marshalToolDefs(defs any) []byte {
	if !ToolPromptRules() {
		b, _ := json.MarshalIndent(defs, "", "  ")
		return b
	}
	b, _ := json.Marshal(defs)
	return b
}
