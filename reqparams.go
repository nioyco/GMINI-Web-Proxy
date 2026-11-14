package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Request-parameter diagnostics. They record WHICH parameters a client sends and
// the values of an explicit allow-list of non-sensitive scalars; message, tool and
// image content is never read into the output. Observability only: nothing here
// changes how a request is served.

// usedFields are the top-level request fields the proxy actually consumes, per
// route kind. Any other field present is counted as "ignored".
var usedFields = map[string]map[string]bool{
	"chat":      set("model", "messages", "tools", "tool_choice", "stream", "reasoning_effort"),
	"responses": set("model", "instructions", "input", "tools", "tool_choice", "stream"),
	"anthropic": set("model", "system", "messages", "tools", "tool_choice", "stream"),
	"google":    set("contents", "systemInstruction", "tools", "toolConfig"),
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// scalarAllow lists the scalar field names whose VALUE may be logged.
var scalarAllow = set(
	"model", "stream", "temperature", "top_p", "topP", "top_k", "topK",
	"max_tokens", "max_completion_tokens", "max_output_tokens", "maxOutputTokens",
	"reasoning_effort", "effort", "thinking_budget", "thinkingBudget", "thinkingLevel",
	"includeThoughts", "budget_tokens", "type", "mode", "tool_choice", "n",
	"candidateCount", "seed", "presence_penalty", "frequency_penalty", "parallel_tool_calls",
)

// nestedParents are the objects whose allow-listed scalar children are recorded
// (as "parent.child"); any other nested object is not entered.
var nestedParents = set("generationConfig", "thinking", "reasoning", "thinkingConfig",
	"toolConfig", "functionCallingConfig")

var (
	safeScalarRe = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)
	safeNameRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// requestParams is the content-free description of an incoming request.
type requestParams struct {
	Fields   []string          // top-level field names present
	Scalars  map[string]string // allow-listed scalar values
	Messages int
	Tools    int
	Images   int
	Ignored  []string // present but not used by the proxy
}

// describeRequest builds the description of body for the given route kind.
func describeRequest(kind string, body []byte, images int) requestParams {
	rp := requestParams{Scalars: map[string]string{}, Images: images}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return rp
	}
	used := usedFields[kind]
	for name, raw := range top {
		safe := name
		if !safeNameRe.MatchString(name) {
			safe = "(unsafe-name)"
		}
		rp.Fields = append(rp.Fields, safe)
		if !used[name] {
			rp.Ignored = append(rp.Ignored, safe)
		}
		collectScalars("", name, raw, rp.Scalars, 0)
		switch name {
		case "messages", "contents", "input":
			rp.Messages = arrayLen(raw, 1)
		case "tools":
			rp.Tools = arrayLen(raw, 0)
		}
	}
	sort.Strings(rp.Fields)
	sort.Strings(rp.Ignored)
	return rp
}

// arrayLen returns the length of a JSON array, or def for a non-array value.
func arrayLen(raw json.RawMessage, def int) int {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return len(arr)
	}
	return def
}

// collectScalars records allow-listed scalar values, entering only the
// nestedParents objects (max depth 3).
func collectScalars(prefix, key string, raw json.RawMessage, out map[string]string, depth int) {
	full := key
	if prefix != "" {
		full = prefix + "." + key
	}
	if nestedParents[key] {
		var obj map[string]json.RawMessage
		if depth < 3 && json.Unmarshal(raw, &obj) == nil {
			for k, v := range obj {
				collectScalars(full, k, v, out, depth+1)
			}
		}
		return
	}
	if !scalarAllow[key] || (key == "type" && prefix == "") {
		return
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	switch x := v.(type) {
	case bool:
		out[full] = fmt.Sprint(x)
	case float64:
		out[full] = strings.TrimSpace(string(raw))
	case string:
		if safeScalarRe.MatchString(x) {
			out[full] = x
		}
	case map[string]any:
		if key == "tool_choice" {
			out[full] = "(object)" // never the tool name or arguments
		}
	}
}

// ignoredStats counts the ignored request fields seen since start (bounded).
type ignoredStats struct {
	mu sync.Mutex
	m  map[string]int64
}

const maxIgnoredNames = 64

func (i *ignoredStats) add(names []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.m == nil {
		i.m = map[string]int64{}
	}
	for _, n := range names {
		if _, ok := i.m[n]; !ok && len(i.m) >= maxIgnoredNames {
			n = "(other)"
		}
		i.m[n]++
	}
}

func (i *ignoredStats) snapshot() map[string]int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[string]int64, len(i.m))
	for k, v := range i.m {
		out[k] = v
	}
	return out
}

// noteRequestParams counts ignored fields and, with GEMINI_LOG_REQUESTS=1, logs one
// "request_params" line (no content).
func (s *Server) noteRequestParams(ctx context.Context, kind string, body []byte, images int) {
	rp := describeRequest(kind, body, images)
	s.ignored.add(rp.Ignored)
	if !s.cfg.LogRequests {
		return
	}
	keys := make([]string, 0, len(rp.Scalars))
	for k := range rp.Scalars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + rp.Scalars[k]
	}
	s.rlog(ctx).Info("request_params", "api", kind,
		"fields", strings.Join(rp.Fields, ","), "scalars", strings.Join(pairs, ","),
		"ignored", strings.Join(rp.Ignored, ","),
		"messages", rp.Messages, "tools", rp.Tools, "images", rp.Images)
}
