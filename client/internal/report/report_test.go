package report

import (
	"math"
	"strings"
	"testing"
)

func TestEvaluatePerLayerBitrate(t *testing.T) {
	base := map[string]float64{"connected": 20, "join_ok": 100, "vdelay_p95": 2, "adelay_p95": 2, "vtracks": 32,
		"vk_low": 155, "vk_medium": 410, "vk_high": 1520}
	expected := map[string]float64{"low": 150, "medium": 400, "high": 1500}

	r := stepResult{V: base}
	evaluate(&r, DefaultSLO(), expected)
	if !r.Pass {
		t.Fatalf("want pass, got %v", r.Failures)
	}

	// SFU forwards medium instead of high to the "high" subscribers.
	v := map[string]float64{}
	for k, x := range base {
		v[k] = x
	}
	v["vk_high"] = 410
	r = stepResult{V: v}
	evaluate(&r, DefaultSLO(), expected)
	if r.Pass || !strings.Contains(strings.Join(r.Failures, ","), "video bitrate high") {
		t.Fatalf("want 'video bitrate high' failure, got pass=%v %v", r.Pass, r.Failures)
	}

	// A quality the plan doesn't use (not in expected) is never checked.
	r = stepResult{V: map[string]float64{"connected": 20, "join_ok": 100, "vdelay_p95": 2, "vtracks": 10, "vk_high": 1500}}
	evaluate(&r, DefaultSLO(), map[string]float64{"high": 1500})
	if !r.Pass {
		t.Fatalf("want pass, got %v", r.Failures)
	}
}

func TestSLOProfile(t *testing.T) {
	plan := func(nets ...map[string]any) map[string]any {
		var l []any
		for _, n := range nets {
			l = append(l, n)
		}
		return map[string]any{"plan": map[string]any{"network": l}}
	}
	if ps, m := sloProfile(map[string]any{}); m != cleanMatcher || len(ps) != 1 {
		t.Fatalf("no network: %v %s", ps, m)
	}
	ps, m := sloProfile(plan(map[string]any{"name": "clean", "percent": 70.0}, map[string]any{"name": "lossy", "percent": 30.0}))
	if m != cleanMatcher || strings.Join(ps, ",") != "clean,lossy" {
		t.Fatalf("clean+lossy: %v %s", ps, m)
	}
	ps, m = sloProfile(plan(map[string]any{"name": "3g", "percent": 100.0}))
	if m == cleanMatcher || strings.Join(ps, ",") != "3g" {
		t.Fatalf("all impaired: %v %s", ps, m)
	}
}

func TestEvaluateSendLagInvalid(t *testing.T) {
	r := stepResult{V: map[string]float64{"connected": 20, "join_ok": 100, "vdelay_p95": 2, "send_lag_p99": 45}}
	evaluate(&r, DefaultSLO(), nil)
	if !r.Invalid || r.Pass {
		t.Fatalf("want INVALID for 45 ms send lag, got invalid=%v pass=%v", r.Invalid, r.Pass)
	}
}

func TestQuantileAbove(t *testing.T) {
	xs := []float64{4, 1, 3, 2, 5}
	if q := quantile(xs, .5); q != 3 {
		t.Fatalf("p50 = %v", q)
	}
	if q := quantile(xs, .95); q < 4.7 || q > 4.9 {
		t.Fatalf("p95 = %v", q)
	}
	if q := quantile(nil, .5); !math.IsNaN(q) {
		t.Fatalf("empty p50 = %v", q)
	}
	if a := above(xs, 3); a != 40 {
		t.Fatalf("above 3 = %v%%", a)
	}
}
