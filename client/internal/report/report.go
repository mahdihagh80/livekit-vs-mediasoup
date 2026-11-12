// Package report reads a finished run's steps.csv and Prometheus and writes
// report.md. It does not start rooms or participants.
package report

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"sfubench/loadgen/internal/rooms"
)

// SLO is the pass/fail line for a step. Fix it BEFORE running. The capacity
// of an SFU is the largest step that passes every check.
type SLO struct {
	VideoDelayP95  float64 // seconds
	AudioDelayP95  float64
	JitterP95      float64 // seconds (video)
	LossRatio      float64
	FreezeRatio    float64 // share of video time frozen
	JoinSuccess    float64 // ratio
	MinVideoRatio  float64 // per requested layer: received kbps/track >= ratio * that layer's bitrate (detects SFU downgrading layers)
	MaxGenCPU      float64 // percent; above this the GENERATOR is the bottleneck -> step INVALID
	MaxGenDrop     float64 // percent of packets received on the generator NIC dropped; above this -> step INVALID
	MaxGenSendLag  float64 // seconds; publisher send lag p99 above this -> step INVALID (the generator is late)
	MaxSFUCoreBusy float64 // percent; informational threshold shown in the table
}

func DefaultSLO() SLO {
	return SLO{
		VideoDelayP95: 0.100, AudioDelayP95: 0.060, JitterP95: 0.030, LossRatio: 0.01, FreezeRatio: 0.01,
		JoinSuccess: 0.995, MinVideoRatio: 0.8, MaxGenCPU: 70, MaxGenDrop: 0.5, MaxGenSendLag: 0.020, MaxSFUCoreBusy: 90,
	}
}

type ReportConfig struct {
	PromURL  string // e.g. http://prometheus:9090
	SFUNode  string // label matcher for the SFU host's node-exporter, e.g. instance="10.0.0.10:9100"
	GenNodes string // label matcher for generator hosts, e.g. instance=~"10.0.0.11:9100|10.0.0.12:9100"
	NetDev   string // regex of the SFU data NIC, e.g. "bond1|ens1f0"
	GenDev   string // regex of the generator data NIC
	SLO      SLO
	Dir      string // results/<run>
	Warmup   time.Duration
	// ExpectedKbps: requested quality ("low"/"medium"/"high") -> video kbps a
	// subscriber of that quality should receive. Only qualities the plan uses.
	ExpectedKbps map[string]float64
}

type stepResult struct {
	step     rooms.Step
	V        map[string]float64
	Pass     bool
	Invalid  bool
	NoData   bool // Prometheus has no loadgen data for the step: can't judge
	Failures []string
}

// target is the step's participant target: steps.csv only knows this
// generator, so with several the summed gauges count.
func (r stepResult) target() int {
	if r.V["gens"] > 1 && r.V["target_all"] > 0 {
		return int(r.V["target_all"])
	}
	return r.step.Target
}

// columns: key, header, PromQL template. {W} = steady window, {S} = whole step
// window (for join metrics), {SFU}/{GEN}/{DEV}/{GDEV} = selectors, {P} = the
// profile matcher of the viewers the SLO judges (clean network only, see
// sloProfile); per-profile tables replace it with one profile.
// bench_* series are summed over every loadgen Prometheus scrapes, so with
// several generators (loadgen.name_prefix) each one's report covers all of them.
var reportQueries = []struct{ key, header, q string }{
	// max over the window: a generator that finished a few seconds early has reset its target to 0.
	{"gens", "generators", `count(max_over_time(bench_target_participants[{W}]) > 0)`},
	{"target_all", "target (all gens)", `sum(max_over_time(bench_target_participants[{W}]))`},
	{"connected", "connected", `sum(bench_participants{state="connected"})`},
	{"join_ok", "join ok %", `100 * sum(increase(bench_join_duration_seconds_count{result="ok"}[{S}])) / clamp_min(sum(increase(bench_join_duration_seconds_count[{S}])), 1)`},
	{"join_p95", "join p95 s", `histogram_quantile(0.95, sum by (le) (increase(bench_join_duration_seconds_bucket{result="ok"}[{S}])))`},
	{"ttff_p95", "ttff p95 s", `histogram_quantile(0.95, sum by (le) (increase(bench_ttff_seconds_bucket[{S}])))`},
	{"vdelay_p50", "video e2e p50 ms", `1000 * histogram_quantile(0.50, sum by (le) (increase(bench_sub_e2e_delay_seconds_bucket{kind="video",{P}}[{W}])))`},
	{"vdelay_p95", "video e2e p95 ms", `1000 * histogram_quantile(0.95, sum by (le) (increase(bench_sub_e2e_delay_seconds_bucket{kind="video",{P}}[{W}])))`},
	{"vdelay_p99", "video e2e p99 ms", `1000 * histogram_quantile(0.99, sum by (le) (increase(bench_sub_e2e_delay_seconds_bucket{kind="video",{P}}[{W}])))`},
	{"adelay_p95", "audio e2e p95 ms", `1000 * histogram_quantile(0.95, sum by (le) (increase(bench_sub_e2e_delay_seconds_bucket{kind="audio",{P}}[{W}])))`},
	{"jitter_p95", "video jitter p95 ms", `1000 * histogram_quantile(0.95, sum by (le) (increase(bench_sub_jitter_seconds_bucket{kind="video",{P}}[{W}])))`},
	{"loss", "loss %", `100 * sum(increase(bench_sub_lost_packets_total{{P}}[{W}])) / clamp_min(sum(increase(bench_sub_packets_total{{P}}[{W}])) + sum(increase(bench_sub_lost_packets_total{{P}}[{W}])), 1)`},
	{"freeze", "freeze %", `100 * sum(increase(bench_sub_freeze_seconds_total{{P}}[{W}])) / clamp_min(avg_over_time(sum(bench_sub_tracks{kind="video",{P}})[{W}:{RES}]) * {WSEC}, 1)`},
	{"vkbps", "video kbps/track", `sum(rate(bench_sub_bytes_total{kind="video"}[{W}])) * 8 / 1000 / clamp_min(avg_over_time(sum(bench_sub_tracks{kind="video"})[{W}:{RES}]), 1)`},
	{"vk_low", "kbps/track low", `sum(rate(bench_sub_video_bytes_by_quality_total{quality="low",{P}}[{W}])) * 8 / 1000 / (avg_over_time(sum(bench_sub_video_tracks_by_quality{quality="low",{P}})[{W}:{RES}]) > 0)`},
	{"vk_medium", "kbps/track medium", `sum(rate(bench_sub_video_bytes_by_quality_total{quality="medium",{P}}[{W}])) * 8 / 1000 / (avg_over_time(sum(bench_sub_video_tracks_by_quality{quality="medium",{P}})[{W}:{RES}]) > 0)`},
	{"vk_high", "kbps/track high", `sum(rate(bench_sub_video_bytes_by_quality_total{quality="high",{P}}[{W}])) * 8 / 1000 / (avg_over_time(sum(bench_sub_video_tracks_by_quality{quality="high",{P}})[{W}:{RES}]) > 0)`},
	// Per-viewer view: one sample per track per 5 s, so these say how the worst
	// viewers fare, not the average packet.
	{"fps_bad", "viewers fps<25 %", `100 * sum(increase(bench_sub_track_fps_bucket{le=~"25(\\.0)?",{P}}[{W}])) / clamp_min(sum(increase(bench_sub_track_fps_count{{P}}[{W}])), 1)`},
	{"fps_p5", "viewer fps p5", `histogram_quantile(0.05, sum by (le) (increase(bench_sub_track_fps_bucket{{P}}[{W}])))`},
	{"tloss_p95", "viewer loss p95 %", `100 * histogram_quantile(0.95, sum by (le) (increase(bench_sub_track_loss_ratio_bucket{kind="video",{P}}[{W}])))`},
	{"tloss_bad", "viewers loss>2% %", `100 * (1 - sum(increase(bench_sub_track_loss_ratio_bucket{le=~"0\\.02",{P}}[{W}])) / clamp_min(sum(increase(bench_sub_track_loss_ratio_count{{P}}[{W}])), 1))`},
	{"kbr_p5", "viewer kbps/requested p5", `histogram_quantile(0.05, sum by (le) (increase(bench_sub_track_kbps_ratio_bucket{{P}}[{W}])))`},
	{"freeze_p95", "freeze length p95 s", `histogram_quantile(0.95, sum by (le) (increase(bench_sub_freeze_duration_seconds_bucket{{P}}[{W}])))`},
	{"lower_pct", "frames below requested layer %", `100 * sum(increase(bench_sub_layer_frames_total{delivered="lower",{P}}[{W}])) / clamp_min(sum(increase(bench_sub_layer_frames_total{{P}}[{W}])), 1)`},
	{"vtracks", "video tracks fwd", `avg_over_time(sum(bench_sub_tracks{kind="video"})[{W}:{RES}])`},
	{"atracks", "audio tracks fwd", `avg_over_time(sum(bench_sub_tracks{kind="audio"})[{W}:{RES}])`},
	{"disc", "drops", `sum(increase(bench_disconnects_total[{W}]))`},
	{"nack", "NACK/s at pub", `sum(rate(bench_pub_rtcp_total{type="nack"}[{W}]))`},
	{"pli", "PLI/s at pub", `sum(rate(bench_pub_rtcp_total{type=~"pli|fir"}[{W}]))`},
	{"nack_sent", "NACK/s from subs", `sum(rate(bench_sub_rtcp_sent_total{type="nack"}[{W}]))`},
	{"rtx", "RTX pkt/s at subs", `sum(rate(bench_sub_rtx_packets_total[{W}]))`},
	{"rtx_dup", "dup RTX %", `100 * sum(increase(bench_sub_rtx_duplicate_packets_total[{W}])) / clamp_min(sum(increase(bench_sub_rtx_packets_total[{W}])), 1)`},
	{"ooo", "out-of-order %", `100 * sum(increase(bench_sub_out_of_order_packets_total[{W}])) / clamp_min(sum(increase(bench_sub_packets_total[{W}])), 1)`},
	{"rtt_p95", "pub RTT p95 ms", `1000 * histogram_quantile(0.95, sum by (le) (increase(bench_pub_rtt_seconds_bucket[{W}])))`},
	{"sfu_cpu", "SFU CPU %", `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle",{SFU}}[{W}])))`},
	{"sfu_cores", "SFU cores used", `sum(rate(node_cpu_seconds_total{mode!="idle",{SFU}}[{W}]))`},
	{"sfu_softirq", "SFU softirq %", `100 * sum(rate(node_cpu_seconds_total{mode="softirq",{SFU}}[{W}])) / count(node_cpu_seconds_total{mode="idle",{SFU}})`},
	{"sfu_maxcore", "SFU busiest core %", `100 * max(1 - rate(node_cpu_seconds_total{mode="idle",{SFU}}[{W}]))`},
	{"sfu_mem", "SFU mem GiB", `(avg_over_time(node_memory_MemTotal_bytes{{SFU}}[{W}]) - avg_over_time(node_memory_MemAvailable_bytes{{SFU}}[{W}])) / 2^30`},
	{"sfu_rx_mpps", "SFU rx Mpps", `sum(rate(node_network_receive_packets_total{{SFU},device=~"{DEV}"}[{W}])) / 1e6`},
	{"sfu_tx_mpps", "SFU tx Mpps", `sum(rate(node_network_transmit_packets_total{{SFU},device=~"{DEV}"}[{W}])) / 1e6`},
	{"sfu_rx_gbps", "SFU rx Gbps", `sum(rate(node_network_receive_bytes_total{{SFU},device=~"{DEV}"}[{W}])) * 8 / 1e9`},
	{"sfu_tx_gbps", "SFU tx Gbps", `sum(rate(node_network_transmit_bytes_total{{SFU},device=~"{DEV}"}[{W}])) * 8 / 1e9`},
	{"sfu_rx_drop", "SFU rx drop/s", `sum(rate(node_network_receive_drop_total{{SFU},device=~"{DEV}"}[{W}]))`},
	{"gen_cpu", "gen CPU max %", `100 * max(1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle",{GEN}}[{W}])))`},
	// Info only: a bond's rx_dropped also counts frames it discards on purpose
	// (inactive slave, unknown protocol), even on an idle host.
	{"gen_drop", "gen NIC rx drop/s", `sum(rate(node_network_receive_drop_total{{GEN},device=~"{GDEV}"}[{W}]))`},
	{"send_lag_p99", "gen send lag p99 ms", `1000 * histogram_quantile(0.99, sum by (le) (increase(bench_gen_send_lag_seconds_bucket[{W}])))`},
	// The INVALID rule: received packets the generator's kernel dropped before
	// the loadgen read them (softirq backlog full, UDP socket buffer full).
	{"gen_drop_pct", "gen kernel rx drop %", `100 * (sum(increase(node_softnet_dropped_total{{GEN}}[{W}])) + (sum(increase(node_netstat_Udp_RcvbufErrors{{GEN}}[{W}])) or vector(0))) / clamp_min(sum(increase(node_network_receive_packets_total{{GEN},device=~"{GDEV}"}[{W}])), 1)`},
}

func Report(ctx context.Context, rc ReportConfig) (string, error) {
	steps, err := rooms.ReadSteps(filepath.Join(rc.Dir, "steps.csv"))
	if err != nil {
		return "", err
	}
	meta := readMeta(rc.Dir)
	profiles, sloP := sloProfile(meta)
	var results []stepResult
	var profRows []profileRow
	for _, s := range steps {
		from := s.Settled.Add(rc.Warmup)
		if !from.Before(s.End) {
			from = s.Settled
		}
		w := int(s.End.Sub(from).Seconds())
		// +15s: increase() needs a sample from just before the first join of the step.
		sw := int(s.End.Sub(s.Start).Seconds()) + 15
		expand := func(q, prof string) string {
			return strings.NewReplacer(
				"{W}", fmt.Sprintf("%ds", w), "{S}", fmt.Sprintf("%ds", sw), "{WSEC}", strconv.Itoa(w),
				// subquery resolution: 15s for normal holds, finer for short windows (>= 3 points)
				"{RES}", fmt.Sprintf("%ds", max(1, min(15, w/3))),
				"{SFU}", rc.SFUNode, "{GEN}", rc.GenNodes, "{GDEV}", rc.GenDev, "{DEV}", rc.NetDev,
				"{P}", prof,
			).Replace(q)
		}
		r := stepResult{step: s, V: map[string]float64{}}
		for _, q := range reportQueries {
			r.V[q.key] = promValue(ctx, rc.PromURL, expand(q.q, sloP), s.End)
		}
		evaluate(&r, rc.SLO, rc.ExpectedKbps)
		results = append(results, r)
		if len(profiles) > 1 {
			for _, p := range profiles {
				pr := profileRow{step: s.Step, profile: p, V: map[string]float64{}}
				for _, q := range reportQueries {
					if profileCols[q.key] {
						pr.V[q.key] = promValue(ctx, rc.PromURL, expand(q.q, fmt.Sprintf(`profile=%q`, p)), s.End)
					}
				}
				profRows = append(profRows, pr)
			}
		}
	}
	var dist []distRow
	var bwe []bweRow
	var srtp map[string]float64
	if len(steps) > 0 {
		dist = disturbances(ctx, rc.PromURL, steps[0].Start.Add(-15*time.Second), steps[len(steps)-1].End)
		bwe = bweQueries(ctx, rc.PromURL, steps[0].Start, steps[len(steps)-1].End)
		sfu, _ := meta["sfu"].(string)
		srtp = srtpCiphers(ctx, rc.PromURL, sfu, steps[0].Start, steps[len(steps)-1].End)
	}

	md := renderMarkdown(rc, meta, sloP, results, profRows, dist, bwe, srtp)
	if err := os.WriteFile(filepath.Join(rc.Dir, "report.md"), []byte(md), 0o644); err != nil {
		return "", err
	}
	if err := writeReportCSV(filepath.Join(rc.Dir, "report.csv"), results); err != nil {
		return "", err
	}
	return md, nil
}

func evaluate(r *stepResult, slo SLO, expected map[string]float64) {
	v := r.V
	fail := func(cond bool, msg string) {
		if cond {
			r.Failures = append(r.Failures, msg)
		}
	}
	gt := func(x, lim float64) bool { return !math.IsNaN(x) && x > lim }
	lt := func(x, lim float64) bool { return !math.IsNaN(x) && x < lim }

	// No load-generator data in Prometheus (wrong URL, scrape down): we can't judge
	// the step. Missing data must never count as "no violation".
	if math.IsNaN(v["connected"]) || math.IsNaN(v["vdelay_p95"]) && math.IsNaN(v["adelay_p95"]) {
		r.NoData = true
	}
	if gt(v["gen_cpu"], slo.MaxGenCPU) || gt(v["gen_drop_pct"], slo.MaxGenDrop) ||
		slo.MaxGenSendLag > 0 && gt(v["send_lag_p99"], slo.MaxGenSendLag*1000) {
		r.Invalid = true
	}
	// Join success from the run's own counts (steps.csv), independent of Prometheus.
	// steps.csv only knows this generator: with several, use the summed gauges.
	connected, target := float64(r.step.Connected), float64(r.step.Target)
	if v["gens"] > 1 && v["target_all"] > 0 {
		connected, target = v["connected"], v["target_all"]
	}
	if target > 0 && connected < slo.JoinSuccess*target {
		r.Failures = append(r.Failures, fmt.Sprintf("connected %.0f/%.0f", connected, target))
	}
	fail(gt(v["vdelay_p95"], slo.VideoDelayP95*1000), "video delay")
	fail(gt(v["adelay_p95"], slo.AudioDelayP95*1000), "audio delay")
	fail(gt(v["jitter_p95"], slo.JitterP95*1000), "jitter")
	fail(gt(v["loss"], slo.LossRatio*100), "loss")
	fail(gt(v["freeze"], slo.FreezeRatio*100), "freezes")
	fail(lt(v["join_ok"], slo.JoinSuccess*100), "join success")
	// Each requested layer must actually arrive: an SFU that quietly forwards a
	// lower layer (to save CPU/bandwidth) fails here.
	for _, q := range []string{"low", "medium", "high"} {
		if exp, ok := expected[q]; ok && v["vtracks"] > 0 {
			fail(lt(v["vk_"+q], slo.MinVideoRatio*exp), "video bitrate "+q)
		}
	}
	fail(gt(v["disc"], 0), "drops")
	r.Pass = len(r.Failures) == 0 && !r.Invalid && !r.NoData
}

func renderMarkdown(rc ReportConfig, meta map[string]any, sloP string, rs []stepResult, prof []profileRow, dist []distRow, bwe []bweRow, srtp map[string]float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Report: %v (%v)\n\n", meta["run"], meta["sfu"])
	renderNackBanner(&b, meta)
	if p, ok := meta["plan"].(map[string]any); ok {
		fmt.Fprintf(&b, "Plan **%v**: %v rooms, %v participants/room, %v publisher(s)/room, audio=%v video=%v.\n\n", p["name"], p["rooms"], p["maxParticipants"], p["publishers"], p["audio"], p["video"])
		if sub, ok := p["subscribe"].(map[string]any); ok {
			fmt.Fprintf(&b, "Subscribed layer per participant: high %v%%, medium %v%%, low %v%%.", sub["high"], sub["medium"], sub["low"])
			for _, q := range []string{"low", "medium", "high"} {
				if exp, ok := rc.ExpectedKbps[q]; ok {
					fmt.Fprintf(&b, " Expected %s ≈ %.0f kbps/track.", q, exp)
				}
			}
			b.WriteString("\n\n")
		}
	}
	if meta["sfu"] == "mediasoup" {
		b.WriteString("Note: the mediasoup signaling used here sends no participant joined/left notifications and runs no active-speaker detection; LiveKit does both. mediasoup is doing less control-plane work than LiveKit in this comparison.\n\n")
	}
	fmt.Fprintf(&b, "SLO: video e2e p95 ≤ %.0f ms, audio e2e p95 ≤ %.0f ms, video jitter p95 ≤ %.0f ms, loss ≤ %.2f%%, frozen ≤ %.2f%%, join success ≥ %.1f%%, video ≥ %.0f%% of the requested layer's bitrate, no drops. Steps where a generator exceeded %.0f%% CPU, its kernel dropped more than %.2f%% of received packets (softnet backlog, UDP buffers), or its publishers sent more than %.0f ms late (p99) are INVALID.\n\n",
		rc.SLO.VideoDelayP95*1000, rc.SLO.AudioDelayP95*1000, rc.SLO.JitterP95*1000, rc.SLO.LossRatio*100, rc.SLO.FreezeRatio*100, rc.SLO.JoinSuccess*100, rc.SLO.MinVideoRatio*100, rc.SLO.MaxGenCPU, rc.SLO.MaxGenDrop, rc.SLO.MaxGenSendLag*1000)
	switch {
	case sloP == cleanMatcher && len(prof) > 0:
		b.WriteString("Quality columns and the SLO use the viewers on a clean network only (profile=clean); the impaired profiles are in their own table below.\n\n")
	case sloP != cleanMatcher:
		b.WriteString("Every viewer of this plan has an impaired network profile, so quality columns and the SLO use all of them.\n\n")
	}

	capacity := 0
	for _, r := range rs {
		if r.Pass {
			capacity = r.target()
		} else {
			break
		}
	}
	fmt.Fprintf(&b, "**Capacity at SLO: %d participants** (largest step before the first failing/invalid step).\n\n", capacity)

	b.WriteString("| step | target | verdict |")
	for _, q := range reportQueries {
		b.WriteString(" " + q.header + " |")
	}
	b.WriteString("\n|---|---|---|")
	for range reportQueries {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, r := range rs {
		var parts []string
		if r.NoData {
			parts = append(parts, "NO DATA (check Prometheus / loadgen scrape)")
		}
		if r.Invalid {
			parts = append(parts, "INVALID (generator)")
		}
		if len(r.Failures) > 0 {
			parts = append(parts, "FAIL: "+strings.Join(r.Failures, ", "))
		}
		verdict := "PASS"
		if !r.Pass {
			verdict = strings.Join(parts, "; ")
		}
		fmt.Fprintf(&b, "| %d | %d | %s |", r.step.Step, r.target(), verdict)
		for _, q := range reportQueries {
			fmt.Fprintf(&b, " %s |", fmtNum(r.V[q.key]))
		}
		b.WriteString("\n")
	}
	renderProfiles(&b, prof)
	renderDisturbances(&b, dist)
	renderBWE(&b, bwe)
	renderProvenance(&b, meta, srtp)
	b.WriteString("\nTime windows (UTC): each step's metrics use [settled + warmup, end]; join metrics use [start, end].\n")
	b.WriteString("\"target\" counts every generator when Prometheus scrapes several (\"generators\" column), else this one's share; metrics are summed over every loadgen it scrapes. With several generators, start them together so their windows line up.\n\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "- step %d: start %s, settled %s, end %s\n", r.step.Step, r.step.Start.Format(time.RFC3339), r.step.Settled.Format(time.RFC3339), r.step.End.Format(time.RFC3339))
	}
	return b.String()
}

func fmtNum(v float64) string {
	switch {
	case math.IsNaN(v) || math.IsInf(v, 0):
		return "–"
	case v == math.Trunc(v) && math.Abs(v) < 1e7:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 1:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 3, 64)
	}
}

func writeReportCSV(path string, rs []stepResult) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	hdr := []string{"step", "target", "pass", "invalid", "failures"}
	for _, q := range reportQueries {
		hdr = append(hdr, q.key)
	}
	w.Write(hdr)
	for _, r := range rs {
		row := []string{strconv.Itoa(r.step.Step), strconv.Itoa(r.target()), strconv.FormatBool(r.Pass), strconv.FormatBool(r.Invalid), strings.Join(r.Failures, ";")}
		for _, q := range reportQueries {
			row = append(row, strconv.FormatFloat(r.V[q.key], 'g', 6, 64))
		}
		w.Write(row)
	}
	w.Flush()
	return w.Error()
}

func promInstant(ctx context.Context, base, expr string, at time.Time) (float64, error) {
	u := strings.TrimRight(base, "/") + "/api/v1/query?" + url.Values{"query": {expr}, "time": {strconv.FormatInt(at.Unix(), 10)}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("%s: %s", expr, out.Error)
	}
	if len(out.Data.Result) == 0 {
		return math.NaN(), nil
	}
	s, _ := out.Data.Result[0].Value[1].(string)
	return strconv.ParseFloat(s, 64)
}

// promValue is promInstant with errors as NaN (shown as "–").
func promValue(ctx context.Context, base, expr string, at time.Time) float64 {
	v, err := promInstant(ctx, base, expr, at)
	if err != nil {
		return math.NaN()
	}
	return v
}

// promVector runs an instant query and returns each series' value by the
// given label.
func promVector(ctx context.Context, base, expr, label string, at time.Time) (map[string]float64, error) {
	u := strings.TrimRight(base, "/") + "/api/v1/query?" + url.Values{"query": {expr}, "time": {strconv.FormatInt(at.Unix(), 10)}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("%s: %s", expr, out.Error)
	}
	m := map[string]float64{}
	for _, r := range out.Data.Result {
		s, _ := r.Value[1].(string)
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			m[r.Metric[label]] = v
		}
	}
	return m, nil
}

// ---- compare two (or more) runs ------------------------------------------------

// Compare renders the key columns of several report.csv files side by side, by target.
func Compare(dirs []string) (string, error) {
	type run struct {
		name string
		rows map[int]map[string]string
		cap  int
	}
	var runs []run
	targets := map[int]bool{}
	for _, d := range dirs {
		f, err := os.Open(filepath.Join(d, "report.csv"))
		if err != nil {
			return "", err
		}
		recs, err := csv.NewReader(f).ReadAll()
		f.Close()
		if err != nil {
			return "", err
		}
		r := run{name: filepath.Base(d), rows: map[int]map[string]string{}}
		stillPassing := true
		for _, rec := range recs[1:] {
			m := map[string]string{}
			for i, h := range recs[0] {
				m[h] = rec[i]
			}
			t, _ := strconv.Atoi(m["target"])
			r.rows[t] = m
			targets[t] = true
			if stillPassing && m["pass"] == "true" {
				r.cap = t
			} else {
				stillPassing = false
			}
		}
		runs = append(runs, r)
	}
	var ts []int
	for t := range targets {
		ts = append(ts, t)
	}
	sortInts(ts)

	cols := []struct{ key, label string }{
		{"pass", "pass"}, {"sfu_cores", "SFU cores"}, {"sfu_maxcore", "busiest core %"}, {"vdelay_p95", "video p95 ms"},
		{"vdelay_p99", "video p99 ms"}, {"jitter_p95", "jitter p95 ms"}, {"loss", "loss %"}, {"freeze", "freeze %"}, {"vk_low", "kbps low"}, {"vk_medium", "kbps medium"}, {"vk_high", "kbps high"}, {"join_p95", "join p95 s"},
		{"sfu_softirq", "softirq %"}, {"sfu_rx_gbps", "rx Gbps"}, {"sfu_tx_gbps", "tx Gbps"}, {"sfu_rx_mpps", "rx Mpps"}, {"sfu_tx_mpps", "tx Mpps"},
		{"fps_bad", "viewers fps<25 %"}, {"tloss_p95", "viewer loss p95 %"}, {"kbr_p5", "viewer kbps/req p5"}, {"freeze_p95", "freeze len p95 s"},
		{"sfu_rx_drop", "rx drop/s"}, {"nack", "NACK/s"}, {"pli", "PLI/s"},
		{"nack_sent", "NACK/s sent"}, {"rtx", "RTX pkt/s"}, {"ooo", "out-of-order %"}, {"rtt_p95", "pub RTT p95 ms"},
		{"send_lag_p99", "gen send lag p99 ms"},
	}
	var b strings.Builder
	b.WriteString("# Comparison\n\n")
	for _, r := range runs {
		fmt.Fprintf(&b, "- **%s**: capacity at SLO = **%d** participants\n", r.name, r.cap)
	}
	b.WriteString("\n| target | metric |")
	for _, r := range runs {
		b.WriteString(" " + r.name + " |")
	}
	b.WriteString("\n|---|---|")
	for range runs {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, t := range ts {
		for _, c := range cols {
			fmt.Fprintf(&b, "| %d | %s |", t, c.label)
			for _, r := range runs {
				v := "–"
				if row, ok := r.rows[t]; ok {
					if f, err := strconv.ParseFloat(row[c.key], 64); err == nil {
						v = fmtNum(f)
					} else {
						v = row[c.key]
					}
				}
				b.WriteString(" " + v + " |")
			}
			b.WriteString("\n")
		}
	}
	// Efficiency: cores per 1000 forwarded video tracks at the largest common passing step.
	b.WriteString("\nEfficiency (SFU cores per 1000 forwarded video tracks, per step):\n\n| target |")
	for _, r := range runs {
		b.WriteString(" " + r.name + " |")
	}
	b.WriteString("\n|---|")
	for range runs {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, t := range ts {
		fmt.Fprintf(&b, "| %d |", t)
		for _, r := range runs {
			v := "–"
			if row, ok := r.rows[t]; ok {
				cores, _ := strconv.ParseFloat(row["sfu_cores"], 64)
				vt, _ := strconv.ParseFloat(row["vtracks"], 64)
				if vt > 0 {
					v = fmtNum(cores / vt * 1000)
				}
			}
			b.WriteString(" " + v + " |")
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
