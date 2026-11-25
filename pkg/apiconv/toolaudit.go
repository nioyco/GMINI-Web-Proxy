package apiconv

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ToolTurnSummary summarizes verified tool calls actually present in the
// structured conversation history (assistant tool_calls / tool results).
type ToolTurnSummary struct {
	Counts       map[string]int
	WriteCount   int
	ReadCount    int
	CommandCount int
	WriteFiles   []string // basenames only (never raw content)
}

// ConversationToolFacts captures verified tool executions both across the whole
// conversation and specifically since the latest user turn.
type ConversationToolFacts struct {
	SinceLastUser ToolTurnSummary
	Total         ToolTurnSummary
}

func isWriteToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write_file", "edit_file", "write_to_file", "replace_file_content", "multi_replace_file_content", "create_file", "patch_file":
		return true
	}
	return false
}

func isFullWriteToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "write_file", "write_to_file", "create_file":
		return true
	}
	return false
}

func isReadToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read_file", "view_file", "view_file_outline", "view_code_item", "list_dir", "find_by_name", "grep_search":
		return true
	}
	return false
}

func isCommandToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "run_command", "bash", "shell", "execute_command", "exec_command", "run_shell", "terminal":
		return true
	}
	return false
}

func extractPathBaseFromArgs(argsJSON string) string {
	if strings.TrimSpace(argsJSON) == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(argsJSON), &obj) != nil {
		return ""
	}
	for _, k := range []string{"Path", "path", "TargetFile", "target_file", "file_path", "filepath", "file"} {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return filepath.Base(strings.TrimSpace(v))
		}
	}
	return ""
}

func (s *ToolTurnSummary) add(name, argsJSON string) {
	if name == "" {
		return
	}
	if s.Counts == nil {
		s.Counts = map[string]int{}
	}
	s.Counts[name]++
	if isWriteToolName(name) {
		s.WriteCount++
		if base := extractPathBaseFromArgs(argsJSON); base != "" && base != "." && base != "/" {
			found := false
			for _, existing := range s.WriteFiles {
				if existing == base {
					found = true
					break
				}
			}
			if !found && len(s.WriteFiles) < 8 {
				s.WriteFiles = append(s.WriteFiles, base)
			}
		}
	}
	if isReadToolName(name) {
		s.ReadCount++
	}
	if isCommandToolName(name) {
		s.CommandCount++
	}
}

// TotalCalls returns the total number of verified tool invocations in s.
func (s ToolTurnSummary) TotalCalls() int {
	n := 0
	for _, c := range s.Counts {
		n += c
	}
	return n
}

// FormatSummary returns a compact human-readable French summary of verified tools.
func (s ToolTurnSummary) FormatSummary() string {
	if len(s.Counts) == 0 {
		return "aucun outil exécuté"
	}
	names := make([]string, 0, len(s.Counts))
	for k := range s.Counts {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s ×%d", k, s.Counts[k]))
	}
	out := strings.Join(parts, ", ")
	if len(s.WriteFiles) > 0 {
		out += " (fichiers écrits : " + strings.Join(s.WriteFiles, ", ") + ")"
	}
	return out
}

// ExtractConversationToolFacts inspects structured OpenAI chat messages and
// counts real tool invocations whose results were returned in the history.
func ExtractConversationToolFacts(messages []Message) ConversationToolFacts {
	var facts ConversationToolFacts
	lastUserIdx := -1
	for i, m := range messages {
		if m.Role == "user" {
			lastUserIdx = i
		}
	}

	// Track which tool_call_ids actually have a matching role="tool" result.
	executedIDs := map[string]bool{}
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			executedIDs[m.ToolCallID] = true
		}
	}

	for i, m := range messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		sinceUser := i > lastUserIdx
		for _, tc := range m.ToolCalls {
			// Count calls that have an executed tool result (or no ID tracked).
			if tc.ID != "" && len(executedIDs) > 0 && !executedIDs[tc.ID] {
				continue
			}
			facts.Total.add(tc.Function.Name, tc.Function.Arguments)
			if sinceUser {
				facts.SinceLastUser.add(tc.Function.Name, tc.Function.Arguments)
			}
		}
	}
	return facts
}

// SummarizeToolCalls returns compact "name×N" labels for one turn's tool calls.
func SummarizeToolCalls(calls []ToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	counts := map[string]int{}
	order := make([]string, 0, len(calls))
	for _, tc := range calls {
		n := tc.Function.Name
		if n == "" {
			continue
		}
		if counts[n] == 0 {
			order = append(order, n)
		}
		counts[n]++
	}
	out := make([]string, 0, len(order))
	for _, n := range order {
		if counts[n] > 1 {
			out = append(out, fmt.Sprintf("%s×%d", n, counts[n]))
		} else {
			out = append(out, n)
		}
	}
	return out
}

// SplitPrematureReadWriteBatch defers full-file write_file calls when the model
// emits them in the exact same turn as read_file calls (e.g. writing a review
// report before reading the files requested in the same batch).
// Legitimate edit_file batches are left untouched.
func SplitPrematureReadWriteBatch(calls []ToolCall) (kept []ToolCall, deferred int) {
	hasRead := false
	hasFullWrite := false
	for _, tc := range calls {
		if isReadToolName(tc.Function.Name) {
			hasRead = true
		}
		if isFullWriteToolName(tc.Function.Name) {
			hasFullWrite = true
		}
	}
	if !hasRead || !hasFullWrite {
		return calls, 0
	}
	kept = make([]ToolCall, 0, len(calls))
	for _, tc := range calls {
		if isFullWriteToolName(tc.Function.Name) {
			deferred++
			continue
		}
		kept = append(kept, tc)
	}
	if len(kept) == 0 {
		return calls, 0
	}
	return kept, deferred
}

var (
	earlyFakeWriteOpenerRe = regexp.MustCompile(`(?i)^\s*(voici\s+(?:le|la)\s+(?:fichier|contenu|rapport|version|mise\s+à\s+jour)\b[^\n]{0,220}(?:mis[e]?\s+à\s+jour|corrigé|modifié|complété|contre-revue|/\S+\.\w+)|here\s+is\s+the\s+(?:updated|modified|complete|full)\s+file\b)`)
	fakeWriteClaimRe       = regexp.MustCompile(`(?i)(voici\s+(?:le|la)\s+(?:fichier|rapport)\s+` + "`?" + `/[^\s` + "`" + `]+` + "`?" + `\s+(?:entièrement\s+)?(?:mis[e]?\s+à\s+jour|corrigé|modifié)|j['’]ai\s+(?:bien\s+)?(?:exécuté\s+l['’]écriture\s+sur\s+le\s+disque|écrit\s+(?:le\s+fichier|sur\s+le\s+disque|le\s+rapport)|mis\s+à\s+jour\s+(?:le\s+fichier|sur\s+le\s+disque|` + "`?" + `/[^\s` + "`" + `]+)|modifié\s+le\s+fichier\b|créé\s+le\s+fichier\b|enregistré\s+(?:les\s+modifications|le\s+fichier)\s+sur\s+le\s+disque)|(?:la\s+vue|le\s+composant)\s+\S+[^\n]{0,40}\s+a\s+été\s+(?:créé[e]?|écrit[e]?|modifié[e]?|mis[e]?\s+à\s+jour)|livrable\b[^\n]{0,80}\b(?:est\s+(?:maintenant\s+)?(?:parfaitement\s+)?à\s+jour\s+sur\s+votre\s+machine)|i\s+have\s+(?:updated|written|saved|modified|created)\s+(?:the\s+file|it\s+to\s+disk)|\b(?:le|la)\s+(?:fichier|rapport|livrable|vue|composant)\b[^\n]{0,60}\ba\s+été\s+(?:créé[e]?|écrit[e]?|modifié[e]?|enregistré[e]?)[^\n]{0,40}sur\s+le\s+disque)`)
	fakeCommandClaimRe     = regexp.MustCompile(`(?i)(j['’]ai\s+(?:bien\s+)?(?:exécuté|lancé|relancé|démarré)\s+(?:la\s+suite\s+de\s+vérification|les\s+tests|la\s+suite\s+de\s+tests|le\s+build|la\s+commande|go\s+test)|i\s+(?:have\s+)?(?:ran|executed|launched|started|triggered|kicked\s+off)\s+(?:the\s+(?:verification\s+suite|test\s+suite|tests|build|checks|command)|go\s+test))`)
	hypotheticalClaimRe    = regexp.MustCompile(`(?i)\b(je\s+peux|souhaites-tu|voulez-vous|veux-tu|si\s+tu\s+veux|tu\s+peux|vous\s+pouvez|pourrait|would\s+you\s+like|i\s+can|you\s+can)\b`)
)

func hasDeclaredWriteTool(declared []DeclaredTool) bool {
	for _, d := range declared {
		if isWriteToolName(d.Name) {
			return true
		}
	}
	return false
}

func hasDeclaredCommandTool(declared []DeclaredTool) bool {
	_, ok := FindCommandTool(declared)
	return ok
}

// IsEarlyFakeWriteOpener reports whether the start of a streamed reply looks
// like the model dumping an "updated file" straight into chat instead of
// calling write_file/edit_file, when no write tool has run since the last user turn.
func IsEarlyFakeWriteOpener(text string, declared []DeclaredTool, sinceLastUser ToolTurnSummary) bool {
	if len(declared) == 0 || sinceLastUser.WriteCount > 0 || !hasDeclaredWriteTool(declared) {
		return false
	}
	if strings.Contains(text, "```tool_call") || strings.Contains(text, "```function_call") {
		return false
	}
	head := text
	if len(head) > 500 {
		head = head[:500]
	}
	return earlyFakeWriteOpenerRe.MatchString(head)
}

// codeSpanRanges returns the [start,end) byte ranges of the fenced code blocks
// (```...```) and inline backtick spans (`...`) of s, delimiters included. An
// unterminated fence or span extends to the end of s.
func codeSpanRanges(s string) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "```") {
			end := strings.Index(s[i+3:], "```")
			if end < 0 {
				out = append(out, [2]int{i, len(s)})
				break
			}
			stop := i + 3 + end + 3
			out = append(out, [2]int{i, stop})
			i = stop
			continue
		}
		if s[i] == '`' {
			end := strings.IndexByte(s[i+1:], '`')
			if end < 0 {
				out = append(out, [2]int{i, len(s)})
				break
			}
			stop := i + 1 + end + 1
			out = append(out, [2]int{i, stop})
			i = stop
			continue
		}
		i++
	}
	return out
}

// stripCodeSpans removes fenced code blocks (```...```) and inline backtick
// spans (`...`) from s, delimiters included, so that a reply merely quoting
// code is not mistaken for an action claim. An unterminated fence or span drops
// the rest of the string.
func stripCodeSpans(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prev := 0
	for _, r := range codeSpanRanges(s) {
		b.WriteString(s[prev:r[0]])
		prev = r[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

// matchOutsideCode reports whether the match at loc starts or ends outside every
// code span of spans. A claim whose whole match sits inside a fence or an inline
// span is only quoted code, not a claim; a claim that merely mentions a path in
// backticks ("Voici le fichier `/x/y.md` mis à jour") stays a real claim.
func matchOutsideCode(loc [2]int, spans [][2]int) bool {
	for _, r := range spans {
		if loc[0] >= r[0] && loc[1] <= r[1] {
			return false
		}
	}
	return true
}

// findClaimOutsideCode returns the first match of re in t that is not entirely
// inside a code span.
func findClaimOutsideCode(re *regexp.Regexp, t string, spans [][2]int) []int {
	for _, loc := range re.FindAllStringIndex(t, -1) {
		if matchOutsideCode([2]int{loc[0], loc[1]}, spans) {
			return loc
		}
	}
	return nil
}

// DetectUnverifiedActionClaim checks a 0-tool-call reply against the verified
// tool executions since the last user turn, flagging explicit claims of disk
// writes or command runs that never happened on the wire. A claim whose whole
// match lies inside a fenced code block or an inline backtick span is only
// quoted code, so it is ignored.
func DetectUnverifiedActionClaim(text string, declared []DeclaredTool, sinceLastUser ToolTurnSummary) (kind string, suspect bool) {
	t := strings.TrimSpace(text)
	if t == "" || len(declared) == 0 {
		return "", false
	}
	if strings.Contains(t, "```tool_call") || strings.Contains(t, "```function_call") {
		return "", false
	}
	spans := codeSpanRanges(t)
	if sinceLastUser.WriteCount == 0 && hasDeclaredWriteTool(declared) {
		if loc := findClaimOutsideCode(fakeWriteClaimRe, t, spans); loc != nil {
			// Check the sentence around the match isn't conditional/future.
			start := loc[0] - 40
			if start < 0 {
				start = 0
			}
			end := loc[1] + 40
			if end > len(t) {
				end = len(t)
			}
			if !hypotheticalClaimRe.MatchString(t[start:end]) {
				return "write_without_tool", true
			}
		}
	}
	if sinceLastUser.CommandCount == 0 && hasDeclaredCommandTool(declared) {
		if loc := findClaimOutsideCode(fakeCommandClaimRe, t, spans); loc != nil {
			start := loc[0] - 40
			if start < 0 {
				start = 0
			}
			end := loc[1] + 40
			if end > len(t) {
				end = len(t)
			}
			if !hypotheticalClaimRe.MatchString(t[start:end]) {
				return "command_without_tool", true
			}
		}
	}
	return "", false
}
