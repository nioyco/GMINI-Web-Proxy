package gemini

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func makeBody54Line(t *testing.T, b54 any) string {
	t.Helper()
	inner2 := make([]any, 55)
	inner2[4] = []any{
		[]any{"rc_1", []any{"Hello"}, nil, nil, nil},
	}
	inner2[54] = b54
	innerJSON, err := json.Marshal(inner2)
	if err != nil {
		t.Fatalf("marshal inner2: %v", err)
	}
	outer := []any{[]any{"wrb.fr", nil, string(innerJSON)}}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		t.Fatalf("marshal outer: %v", err)
	}
	return string(outerJSON)
}

func TestK58_ParseBody54Limit(t *testing.T) {
	t.Run("normal_ok", func(t *testing.T) {
		b54 := []any{nil, nil, float64(0), nil, nil, nil, nil, nil, false, false, float64(1436)}
		fr, ok := parseFrame(makeBody54Line(t, b54))
		if !ok {
			t.Fatal("parseFrame failed")
		}
		if fr.limitBanner != "" {
			t.Fatalf("limitBanner = %q, want empty", fr.limitBanner)
		}
	})

	t.Run("early_warning_b54_2_zero_not_paused", func(t *testing.T) {
		b54 := []any{nil, nil, float64(0), nil, []any{
			[]any{float64(3), []any{float64(1795650303), float64(894116000)}, float64(1), nil, []any{float64(1)}},
		}, nil, nil, nil, false, false, float64(1436)}
		fr, ok := parseFrame(makeBody54Line(t, b54))
		if !ok {
			t.Fatal("parseFrame failed")
		}
		if fr.limitBanner != "" {
			t.Fatalf("early warning must not set limitBanner, got %q", fr.limitBanner)
		}
	})

	t.Run("warning_90pct_4_neon_non_blocking", func(t *testing.T) {
		const ts = int64(1795650303)
		b54 := []any{nil, nil, float64(1), nil, []any{
			[]any{float64(3), []any{float64(ts), float64(894116000)}, float64(1), nil, []any{float64(1)}},
			[]any{float64(5), []any{float64(ts), float64(894116000)}, float64(1), nil, []any{float64(1)}},
		}, nil, nil, nil, false, false, float64(1436)}
		fr, ok := parseFrame(makeBody54Line(t, b54))
		if !ok {
			t.Fatal("parseFrame failed")
		}
		if fr.limitBanner != "" {
			t.Fatalf("b54[2]==1 must NOT set limitBanner, got %q", fr.limitBanner)
		}
		wantHHMM := time.Unix(ts, 0).In(time.Local).Format("15:04")
		if !strings.Contains(fr.quotaWarning, "~90 %") || !strings.Contains(fr.quotaWarning, wantHHMM) {
			t.Fatalf("quotaWarning = %q, want ~90 %% and reset time %s", fr.quotaWarning, wantHHMM)
		}
	})

	t.Run("quota_exhausted_flash_lite", func(t *testing.T) {
		const ts = int64(1795596303)
		b54 := []any{
			[]any{float64(8), float64(3), float64(5), float64(1)},
			float64(6),
			float64(3),
			[]any{[]any{float64(8), nil, "Limit resets {REFILL_TIME}", nil, []any{float64(ts), float64(894116000)}}},
			[]any{},
			float64(1),
			[]any{float64(ts), float64(894116000)},
			nil, false, false, float64(1436),
		}
		fr, ok := parseFrame(makeBody54Line(t, b54))
		if !ok {
			t.Fatal("parseFrame failed")
		}
		wantHHMM := time.Unix(ts, 0).In(time.Local).Format("15:04")
		if !strings.Contains(fr.limitBanner, wantHHMM) {
			t.Fatalf("limitBanner = %q, want reset time %s", fr.limitBanner, wantHHMM)
		}
	})
}
