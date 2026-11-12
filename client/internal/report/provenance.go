package report

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// nackFixCutoff is when the mediasoup client got the RTX-aware NACK generator
//. In UTC: the generators' clock, which names the runs
// (mediasoup-bwe-20261104-1811 started 18:11:05Z).
const nackFixCutoff = "2026-11-04T18:11:00Z"

const wantCipher = "SRTP_AEAD_AES_128_GCM"

// nackParity says whether the run's NACK generator matches the one both
// clients use now. why explains a false, or notes a weak true.
func nackParity(meta map[string]any) (ok bool, why string) {
	if lg, _ := meta["loadgen"].(map[string]any); lg["nack_generator"] != nil {
		return true, ""
	}
	switch meta["sfu"] {
	case "livekit":
		return false, "LiveKit SDK NACK generator, blind to RTX repairs"
	case "mediasoup":
		cutoff, _ := time.Parse(time.RFC3339, nackFixCutoff)
		s, _ := meta["started"].(string)
		started, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || started.Before(cutoff) {
			return false, "Pion stock NACK generator, re-NACKs repaired packets"
		}
	}
	return true, "no provenance recorded"
}

func renderNackBanner(b *strings.Builder, meta map[string]any) {
	if ok, why := nackParity(meta); !ok {
		fmt.Fprintf(b, "> **Not comparable: %s.** This run predates the shared NACK generator. Affected: NACK/s, RTX pkt/s, dup RTX %%, loss after repair, freeze, and on shaped profiles every BWE-derived column. On a clean profile with ~0 subscriber loss the effect is negligible, so check sub loss before discarding the run.\n\n", why)
	}
}

// srtpCiphers counts the PeerConnections per negotiated SRTP cipher over the
// run, summed over every generator.
func srtpCiphers(ctx context.Context, prom, sfu string, from, to time.Time) map[string]float64 {
	expr := fmt.Sprintf(`sum by (cipher) (max_over_time(bench_pc_srtp_total{sfu=%q}[%ds]))`, sfu, max(1, int(to.Sub(from).Seconds())))
	m, err := promVector(ctx, prom, expr, "cipher", to)
	if err != nil {
		return nil
	}
	return m
}

func renderProvenance(b *strings.Builder, meta map[string]any, srtp map[string]float64) {
	b.WriteString("\n## Run provenance\n\n")
	if lg, ok := meta["loadgen"].(map[string]any); ok {
		fmt.Fprintf(b, "- loadgen: binary %s, source %s, %v, host %v, CPUs %v (GOMAXPROCS %v, NumCPU %v), NACK generator %v\n",
			short(lg["binary_sha256"]), short(lg["source_sha256"]), lg["go_version"], lg["host"], lg["cpus_allowed"], lg["gomaxprocs"], lg["num_cpu"], lg["nack_generator"])
	} else {
		b.WriteString("- loadgen: not recorded\n")
	}
	b.WriteString("- SFU: " + sfuLine(meta) + "\n")
	if len(srtp) == 0 {
		b.WriteString("- SRTP: not recorded\n")
	} else {
		var parts, odd []string
		for _, c := range sortedKeys(keysOf(srtp)) {
			parts = append(parts, fmt.Sprintf("%s ×%.0f", c, srtp[c]))
			if c != wantCipher {
				odd = append(odd, c)
			}
		}
		fmt.Fprintf(b, "- SRTP: %s PeerConnections\n", strings.Join(parts, ", "))
		if len(odd) > 0 {
			fmt.Fprintf(b, "- **WARNING: SRTP cipher other than %s: %s.** Crypto cost differs from the other SFU.\n", wantCipher, strings.Join(odd, ", "))
		}
	}
	switch ok, why := nackParity(meta); {
	case !ok:
		fmt.Fprintf(b, "- NACK generator: **not comparable** (%s)\n", why)
	case why != "":
		fmt.Fprintf(b, "- NACK generator: comparable, %s\n", why)
	default:
		b.WriteString("- NACK generator: comparable\n")
	}
	switch p, ok := meta["sfu_snapshot_problems"].([]any); {
	case !ok:
		b.WriteString("- SFU snapshot problems: not recorded\n")
	case len(p) == 0:
		b.WriteString("- SFU snapshot problems: none\n")
	default:
		fmt.Fprintf(b, "- **SFU snapshot problems: %v**\n", p)
	}
}

func sfuLine(meta map[string]any) string {
	snap, _ := meta["sfu_snapshot"].(map[string]any)
	sfu, _ := meta["sfu"].(string)
	s, _ := snap[sfu].(map[string]any)
	if s == nil {
		return "not recorded"
	}
	cpus := s["cpuset"]
	if cpus == nil {
		cpus = s["affinity"]
	}
	if sfu == "livekit" {
		return fmt.Sprintf("livekit %v (%v), image %s, config %s, cpuset %v", s["version"], s["mode"], short(s["image_id"]), short(s["config_sha256"]), orNone(cpus))
	}
	return fmt.Sprintf("mediasoup %v, image %s, config %s, cpuset %v", s["mediasoup_version"], short(s["image_id"]), short(s["media_config_sha256"]), orNone(cpus))
}

func short(v any) string {
	s, _ := v.(string)
	s = strings.TrimPrefix(s, "sha256:")
	if s == "" {
		return "not recorded"
	}
	return s[:min(12, len(s))]
}

func orNone(v any) any {
	if v == nil || v == "" {
		return "none"
	}
	return v
}

func keysOf(m map[string]float64) map[string]bool {
	out := map[string]bool{}
	for k := range m {
		out[k] = true
	}
	return out
}
