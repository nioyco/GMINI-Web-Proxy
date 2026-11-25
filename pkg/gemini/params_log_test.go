package gemini

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// K36: upstream_params and upstream_frame_shape must both carry request_id so
// every upstream line correlates 100% with the proxy request logs.
func TestK36_UpstreamLogsCarryRequestID(t *testing.T) {
	var logs bytes.Buffer
	c, err := New(Config{GeminiBL: "bl", RetryAttempts: 1, LogParams: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m := &AvailableModel{Name: "gemini-everyday-help", ModelID: "56fdd199312815e2", Capacity: 1, CapacityField: 1}
	c.SetModels([]*AvailableModel{m})

	c.logUpstreamParams(GenParams{Prompt: "PROMPT", Model: m}, "reqk36params")
	l := logs.String()
	if !strings.Contains(l, "msg=upstream_params") {
		t.Fatalf("missing upstream_params line:\n%s", l)
	}
	if !strings.Contains(l, "request_id=reqk36params") {
		t.Errorf("upstream_params missing request_id:\n%s", l)
	}
	if !strings.Contains(l, "temporary_chat=") {
		t.Errorf("upstream_params must keep temporary_chat:\n%s", l)
	}
	if strings.Contains(l, "PROMPT") {
		t.Errorf("upstream_params must never log prompt content:\n%s", l)
	}

	logs.Reset()
	shape := newFrameShape()
	shape.add(frame{text: "abc"}, 5*time.Millisecond)
	c.logFrameShape(shape, "reqk36shape", true)
	l = logs.String()
	if !strings.Contains(l, "msg=upstream_frame_shape") {
		t.Fatalf("missing upstream_frame_shape line:\n%s", l)
	}
	if !strings.Contains(l, "request_id=reqk36shape") {
		t.Errorf("upstream_frame_shape missing request_id:\n%s", l)
	}
	if !strings.Contains(l, "temporary_chat=true") {
		t.Errorf("upstream_frame_shape missing temporary_chat=true:\n%s", l)
	}
}

// The request id is read from the per-request metrics attached to the context:
// with no metrics the field is still emitted, just empty.
func TestK36_UpstreamLogsEmptyRequestIDWithoutMetrics(t *testing.T) {
	var logs bytes.Buffer
	c, err := New(Config{GeminiBL: "bl", RetryAttempts: 1, LogParams: true}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.logUpstreamParams(GenParams{Prompt: "PROMPT"}, "")
	if l := logs.String(); !strings.Contains(l, "request_id=") {
		t.Errorf("upstream_params must always emit request_id:\n%s", l)
	}
}
