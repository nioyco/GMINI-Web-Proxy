package gemini

import (
	"encoding/json"
	"sort"
	"strconv"
)

// bodyConstant is one constant slot of the StreamGenerate inner payload.
type bodyConstant struct {
	Index int
	Value any
}

// bodyConstants lists every constant the proxy writes into the inner payload
// (inner[0] = prompt/images and inner[59] = request uuid are dynamic and set by
// buildBody). It is the single source for both the request body and the
// diagnostics exposed by UpstreamParams. inner[17] is the "reasoning level" slot:
// it is always [[0]]; no thinking level is sent.
var bodyConstants = []bodyConstant{
	{1, []any{"en"}},
	{2, []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}},
	{6, []any{1}},
	{7, 1},
	{10, 1},
	{11, 0},
	{17, []any{[]any{0}}},
	{18, 0},
	{27, 1},
	{30, []any{4}},
	{41, []any{1}},
	{53, 0},
	{61, []any{}}, // must serialize as [] not null
	{68, 2},
}

// headerNames are the model-selection headers sent on StreamGenerate (names only:
// their values embed the model id, capacity or the per-request uuid).
var headerNames = []string{
	"x-goog-ext-525001261-jspb", // model selection: [1,null,null,null,"<model id>",null,null,0,[4],null,null,<capacity>]
	"x-goog-ext-73010989-jspb",  // constant [0]
	"x-goog-ext-73010990-jspb",  // constant [0]
	"x-goog-ext-525005358-jspb", // ["<per-request uuid>",1]
}

// UpstreamModelParams describes what is sent to select one model.
type UpstreamModelParams struct {
	Name          string `json:"name"`
	ModelID       string `json:"model_id"`
	Capacity      int    `json:"capacity"`
	CapacityField int    `json:"capacity_field"`
}

// UpstreamParams is the static, secret-free description of the StreamGenerate
// request the proxy builds.
type UpstreamParams struct {
	BodyConstants map[string]string     `json:"body_constants"` // inner[index] → JSON value
	DynamicSlots  map[string]string     `json:"dynamic_slots"`  // inner[index] → what fills it
	Inner17       string                `json:"inner_17"`       // the reasoning-level slot
	HeaderNames   []string              `json:"header_names"`   // x-goog-ext-* names only
	ThinkingLevel string                `json:"thinking_level"` // what the proxy sends for it
	TemporaryChat bool                  `json:"temporary_chat"` // GEMINI_TEMPORARY_CHAT
	Models        []UpstreamModelParams `json:"models"`
}

// UpstreamParams returns the description for the current model registry.
func (c *Client) UpstreamParams() UpstreamParams {
	up := UpstreamParams{
		BodyConstants: map[string]string{},
		DynamicSlots:  map[string]string{"0": "prompt + image refs", "59": "per-request uuid (also in header x-goog-ext-525005358-jspb)", "at": "XSRF token form field (authenticated sessions only)"},
		HeaderNames:   append([]string(nil), headerNames...),
		ThinkingLevel: "not sent (inner[17] is the constant [[0]]; web Bas/Moyen/Élevé level is not controllable)",
		TemporaryChat: c.cfg.TemporaryChat,
	}
	if c.cfg.TemporaryChat {
		up.DynamicSlots["45"] = "1 (GEMINI_TEMPORARY_CHAT)"
	}
	for _, bc := range bodyConstants {
		b, _ := json.Marshal(bc.Value)
		up.BodyConstants[strconv.Itoa(bc.Index)] = string(b)
		if bc.Index == 17 {
			up.Inner17 = string(b)
		}
	}
	c.modelMu.Lock()
	for _, n := range c.modelOrder {
		if m := c.models[n]; m != nil {
			up.Models = append(up.Models, UpstreamModelParams{Name: m.Name, ModelID: m.ModelID, Capacity: m.Capacity, CapacityField: m.CapacityField})
		}
	}
	c.modelMu.Unlock()
	sort.Slice(up.Models, func(i, j int) bool { return up.Models[i].Name < up.Models[j].Name })
	return up
}

// logUpstreamParams emits one "upstream_params" line for a generation call when
// Config.LogParams is set: model id, capacity, inner[17] and the constants sent,
// plus the names (not values) of the selection headers. No prompt content.
func (c *Client) logUpstreamParams(p GenParams) {
	if !c.cfg.LogParams {
		return
	}
	consts := map[string]string{}
	for _, bc := range bodyConstants {
		b, _ := json.Marshal(bc.Value)
		consts[strconv.Itoa(bc.Index)] = string(b)
	}
	attrs := []any{"inner_17", consts["17"], "body_constants", consts, "header_names", headerNames, "images", len(p.Images),
		"thinking_level", p.Level, "mode", p.Mode, "temporary_chat", c.cfg.TemporaryChat}
	if p.Model != nil {
		attrs = append(attrs, "model", p.Model.Name, "model_id", p.Model.ModelID,
			"capacity", p.Model.Capacity, "capacity_field", p.Model.CapacityField,
			"headers_sent", p.Model.ModelID != "")
	}
	c.log.Info("upstream_params", attrs...)
}
