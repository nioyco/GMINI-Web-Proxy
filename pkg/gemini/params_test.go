package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// The constants table must reproduce exactly the payload the proxy always sent.
func TestBodyConstantsGolden(t *testing.T) {
	c, _ := New(Config{GeminiBL: "bl"}, nil)
	body, uuid, err := c.buildBody(GenParams{Prompt: "PROMPT"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := url.ParseQuery(body)
	var outer []any
	if err := json.Unmarshal([]byte(vals.Get("f.req")), &outer); err != nil {
		t.Fatal(err)
	}
	var inner []any
	if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
		t.Fatal(err)
	}
	want := map[int]any{
		1: []any{"en"}, 2: []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}, 6: []any{1.0}, 7: 1.0, 10: 1.0, 11: 0.0,
		17: []any{[]any{0.0}}, 18: 0.0, 27: 1.0, 30: []any{4.0}, 41: []any{1.0}, 53: 0.0, 61: []any{}, 68: 2.0,
	}
	if len(inner) != 69 {
		t.Fatalf("inner len = %d", len(inner))
	}
	for i, v := range inner {
		switch i {
		case 0:
			if v.([]any)[0] != "PROMPT" {
				t.Errorf("inner[0] = %v", v)
			}
		case 59:
			if v != uuid {
				t.Errorf("inner[59] = %v want %v", v, uuid)
			}
		default:
			w, ok := want[i]
			if !ok {
				w = nil
			}
			if !reflect.DeepEqual(v, w) {
				t.Errorf("inner[%d] = %#v, want %#v", i, v, w)
			}
		}
	}
}

func TestUpstreamParamsStaticAndLogLine(t *testing.T) {
	var logs bytes.Buffer
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1, LogParams: true, Cookie: "SAPISID=s; __Secure-1PSIDTS=ts"}, slog.New(slog.NewTextHandler(&logs, nil)))
	m := &AvailableModel{Name: "gemini-detailed-research", ModelID: "abcdef0123456789", Capacity: 2, CapacityField: 12}
	c.SetModels([]*AvailableModel{m})
	up := c.UpstreamParams()
	if up.Inner17 != "[[0]]" || up.BodyConstants["68"] != "2" || up.BodyConstants["61"] != "[]" || len(up.HeaderNames) != 4 || len(up.Models) != 1 || up.Models[0].ModelID != "abcdef0123456789" {
		t.Errorf("params = %+v", up)
	}
	var headerSeen []string
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-goog-ext-") {
				headerSeen = append(headerSeen, strings.ToLower(k))
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wrbBody("ok"))), Request: r}, nil
	}))
	if _, err := c.Generate(context.Background(), GenParams{Prompt: "SECRET_PROMPT", Model: m}); err != nil {
		t.Fatal(err)
	}
	l := logs.String()
	for _, want := range []string{"msg=upstream_params", "inner_17=[[0]]", "model_id=abcdef0123456789", "capacity=2", "capacity_field=12", "x-goog-ext-525001261-jspb"} {
		if !strings.Contains(l, want) {
			t.Errorf("log missing %q:\n%s", want, l)
		}
	}
	if strings.Contains(l, "SECRET_PROMPT") || strings.Contains(l, "ts") && strings.Contains(l, "__Secure") {
		t.Errorf("log leaks:\n%s", l)
	}
	// the documented header names are exactly what is really sent
	got := map[string]bool{}
	for _, h := range headerSeen {
		got[h] = true
	}
	for _, h := range up.HeaderNames {
		if !got[h] {
			t.Errorf("documented header %s not sent (sent: %v)", h, headerSeen)
		}
	}
	if len(got) != len(up.HeaderNames) {
		t.Errorf("sent headers %v differ from documented %v", headerSeen, up.HeaderNames)
	}
}

func TestUpstreamParamsNotLoggedByDefault(t *testing.T) {
	var logs bytes.Buffer
	c, _ := New(Config{GeminiBL: "bl", RetryAttempts: 1}, slog.New(slog.NewTextHandler(&logs, nil)))
	c.SetTransport(rtFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wrbBody("ok"))), Request: r}, nil
	}))
	c.Generate(context.Background(), GenParams{Prompt: "p"})
	if strings.Contains(logs.String(), "upstream_params") {
		t.Errorf("logged by default:\n%s", logs.String())
	}
}

// TestBodyTemporaryChatFlag proves R&D-14 R2 (C5): inner[45] == 1 when
// TemporaryChat is enabled, and untouched (nil) by default.
func TestBodyTemporaryChatFlag(t *testing.T) {
	decodeInner := func(temporary bool) ([]any, *Client) {
		c, _ := New(Config{GeminiBL: "bl", TemporaryChat: temporary}, nil)
		body, _, err := c.buildBody(GenParams{Prompt: "PROMPT"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		vals, _ := url.ParseQuery(body)
		var outer []any
		if err := json.Unmarshal([]byte(vals.Get("f.req")), &outer); err != nil {
			t.Fatal(err)
		}
		var inner []any
		if err := json.Unmarshal([]byte(outer[1].(string)), &inner); err != nil {
			t.Fatal(err)
		}
		return inner, c
	}

	offInner, offClient := decodeInner(false)
	if offInner[45] != nil {
		t.Errorf("default TemporaryChat=false: inner[45] = %#v, want nil", offInner[45])
	}
	if offClient.UpstreamParams().TemporaryChat {
		t.Error("default UpstreamParams.TemporaryChat must be false")
	}

	onInner, onClient := decodeInner(true)
	if onInner[45] != float64(1) {
		t.Errorf("TemporaryChat=true: inner[45] = %#v, want 1", onInner[45])
	}
	up := onClient.UpstreamParams()
	if !up.TemporaryChat || up.DynamicSlots["45"] == "" {
		t.Errorf("UpstreamParams when TemporaryChat=true = %+v", up)
	}

	// Per-request GenParams.TemporaryChat override sets inner[45] = 1 even when client setting is false.
	perReqBody, _, err := offClient.buildBody(GenParams{Prompt: "PROMPT", TemporaryChat: true, Mode: 1, Level: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := url.ParseQuery(perReqBody)
	var outer []any
	if err := json.Unmarshal([]byte(vals.Get("f.req")), &outer); err != nil {
		t.Fatal(err)
	}
	var perInner []any
	if err := json.Unmarshal([]byte(outer[1].(string)), &perInner); err != nil {
		t.Fatal(err)
	}
	if perInner[45] != float64(1) || perInner[79] != float64(1) || perInner[80] != float64(1) {
		t.Errorf("per-request override: inner[45]=%v inner[79]=%v inner[80]=%v, want 1/1/1", perInner[45], perInner[79], perInner[80])
	}
}
