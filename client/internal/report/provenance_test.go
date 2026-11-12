package report

import (
	"strings"
	"testing"
)

func TestNackParity(t *testing.T) {
	for _, c := range []struct {
		name string
		meta map[string]any
		ok   bool
		why  string
	}{
		{"new livekit", map[string]any{"sfu": "livekit", "loadgen": map[string]any{"nack_generator": "sfubench-nackgen/1"}}, true, ""},
		{"old livekit", map[string]any{"sfu": "livekit", "started": "2026-11-05T10:00:00Z"}, false, "LiveKit SDK"},
		{"old mediasoup", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T18:10:59Z"}, false, "Pion stock"},
		{"mediasoup run 1520", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T15:20:30.518893694Z"}, false, "Pion stock"},
		{"old mediasoup, local offset", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T21:40:59+03:30"}, false, "Pion stock"},
		{"mediasoup at cutoff", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T18:11:00Z"}, true, "no provenance recorded"},
		{"mediasoup run 1811", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T18:11:05.348597574Z"}, true, "no provenance recorded"},
		{"mediasoup after cutoff, local offset", map[string]any{"sfu": "mediasoup", "started": "2026-11-04T21:50:00.123+03:30"}, true, "no provenance recorded"},
		{"mediasoup no start", map[string]any{"sfu": "mediasoup"}, false, "Pion stock"},
	} {
		ok, why := nackParity(c.meta)
		if ok != c.ok || !strings.HasPrefix(why, c.why) || (c.why == "" && why != "") {
			t.Errorf("%s: got %v %q, want %v %q", c.name, ok, why, c.ok, c.why)
		}
	}
}

func TestRenderOldAndNewRuns(t *testing.T) {
	old := readMeta("testdata/run-v1")
	if old["run"] != "mediasoup-bwe-old" {
		t.Fatalf("v1 meta: %v", old)
	}
	md := renderMarkdown(ReportConfig{SLO: DefaultSLO()}, old, cleanMatcher, nil, nil, nil, nil, nil)
	for _, want := range []string{"> **Not comparable: Pion stock NACK generator", "- loadgen: not recorded", "- SFU: not recorded",
		"- SRTP: not recorded", "- SFU snapshot problems: not recorded", "check sub loss before discarding"} {
		if !strings.Contains(md, want) {
			t.Errorf("v1 report lacks %q", want)
		}
	}

	nu := readMeta("testdata/run-v2")
	md = renderMarkdown(ReportConfig{SLO: DefaultSLO()}, nu, cleanMatcher, nil, nil, nil, nil, map[string]float64{"SRTP_AEAD_AES_128_GCM": 16})
	for _, want := range []string{"binary 0123456789ab, source fedcba987654", "NACK generator sfubench-nackgen/1",
		"livekit 1.13.11 (docker), image aaaabbbbcccc, config eeeeffff0000, cpuset 14-33", "- SRTP: SRTP_AEAD_AES_128_GCM ×16 PeerConnections",
		"- NACK generator: comparable\n", "- SFU snapshot problems: none"} {
		if !strings.Contains(md, want) {
			t.Errorf("v2 report lacks %q", want)
		}
	}
	for _, bad := range []string{"Not comparable", "WARNING"} {
		if strings.Contains(md, bad) {
			t.Errorf("v2 report has %q", bad)
		}
	}

	md = renderMarkdown(ReportConfig{SLO: DefaultSLO()}, nu, cleanMatcher, nil, nil, nil, nil, map[string]float64{"SRTP_AEAD_AES_128_GCM": 8, "SRTP_AEAD_AES_256_GCM": 8})
	if !strings.Contains(md, "WARNING: SRTP cipher other than SRTP_AEAD_AES_128_GCM: SRTP_AEAD_AES_256_GCM") {
		t.Error("no warning for AES-256")
	}
}
