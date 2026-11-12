package report

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BWE vs link: the SFU's per-viewer send estimate and send rate next to the
// downlink limit the load generator applies to that viewer. Needs the SFU's
// per-peer series (mediasoup bwe_telemetry: mediasoup_peer_*) and the
// loadgen's bench_peer_info / bench_peer_link_bps; skipped when they are
// missing (LiveKit, or telemetry off).

// bweStep is the query_range resolution, the Prometheus scrape interval.
const bweStep = 5 * time.Second

// bweRow is one network profile over the whole run.
type bweRow struct {
	profile string
	peers   int
	// all samples, bps / ratio
	est, estRatio, sentRatio []float64
	layers                   []float64
	bytes                    map[string]float64 // kind -> integrated bps*s over the run
}

func bweQueries(ctx context.Context, prom string, from, to time.Time) []bweRow {
	rows := map[string]*bweRow{}
	row := func(p string) *bweRow {
		if rows[p] == nil {
			rows[p] = &bweRow{profile: p, bytes: map[string]float64{}}
		}
		return rows[p]
	}
	rng := func(expr string) []promSeries {
		s, err := promRange(ctx, prom, expr, from, to, bweStep)
		if err != nil {
			return nil
		}
		return s
	}
	const withProfile = ` * on(peer) group_left(profile) bench_peer_info`
	const perLink = ` / on(peer) group_left(profile) bench_peer_link_bps`

	peers := map[string]map[string]bool{}
	for _, s := range rng(`mediasoup_peer_bwe_available_bps` + withProfile) {
		p := s.metric["profile"]
		r := row(p)
		r.est = append(r.est, s.values...)
		if peers[p] == nil {
			peers[p] = map[string]bool{}
		}
		peers[p][s.metric["peer"]] = true
	}
	if len(rows) == 0 {
		return nil
	}
	for p, m := range peers {
		row(p).peers = len(m)
	}
	for _, s := range rng(`mediasoup_peer_bwe_available_bps` + perLink) {
		r := row(s.metric["profile"])
		r.estRatio = append(r.estRatio, s.values...)
	}
	for _, s := range rng(`rate(mediasoup_peer_sent_bytes_total{kind="all"}[15s]) * 8` + perLink) {
		r := row(s.metric["profile"])
		r.sentRatio = append(r.sentRatio, s.values...)
	}
	for _, s := range rng(`mediasoup_peer_video_layer` + withProfile) {
		r := row(s.metric["profile"])
		r.layers = append(r.layers, s.values...)
	}
	for _, s := range rng(`sum by (profile, kind) (rate(mediasoup_peer_sent_bytes_total[15s])` + withProfile + `)`) {
		r := row(s.metric["profile"])
		for _, v := range s.values {
			r.bytes[s.metric["kind"]] += v * bweStep.Seconds()
		}
	}
	out := make([]bweRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].profile < out[j].profile })
	return out
}

func renderBWE(b *strings.Builder, rows []bweRow) {
	if len(rows) == 0 {
		return
	}
	top := -1.0
	for _, r := range rows {
		for _, l := range r.layers {
			top = math.Max(top, l)
		}
	}
	b.WriteString("\n## BWE vs link (whole run)\n\n" +
		"Per viewer, every 5 s: the SFU's send estimate for that viewer (est), what it sent to the viewer (sent, all bytes incl. RTX, probes and RTCP) " +
		"and the downlink limit the generator applies (limit; for a dip-only profile only during dips). Samples of all the profile's viewers are pooled. " +
		"est÷limit > 1 means the estimate is above what the link can carry; sent÷limit > 1 means the link queue fills and drops. " +
		"RTX and probe share are of all bytes sent. Top layer = time on the highest spatial layer seen in the run")
	if top >= 0 {
		fmt.Fprintf(b, " (layer %.0f)", top)
	}
	b.WriteString(", of the time any video was forwarded.\n\n" +
		"| profile | viewers | est p5 Mbps | est p50 Mbps | est÷limit p5 | est÷limit p50 | est÷limit p95 | est > limit | sent÷limit p50 | sent÷limit p95 | sent > limit | RTX share | probe share | top layer |\n" +
		"|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		share := func(k string) float64 {
			if r.bytes["all"] <= 0 {
				return math.NaN()
			}
			return 100 * r.bytes[k] / r.bytes["all"]
		}
		topShare := math.NaN()
		if top >= 0 {
			n, on := 0, 0
			for _, l := range r.layers {
				if l >= 0 {
					n++
					if l == top {
						on++
					}
				}
			}
			if n > 0 {
				topShare = 100 * float64(on) / float64(n)
			}
		}
		fmt.Fprintf(b, "| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.profile, r.peers,
			fmtNum(quantile(r.est, .05)/1e6), fmtNum(quantile(r.est, .5)/1e6),
			fmtNum(quantile(r.estRatio, .05)), fmtNum(quantile(r.estRatio, .5)), fmtNum(quantile(r.estRatio, .95)), pct(above(r.estRatio, 1)),
			fmtNum(quantile(r.sentRatio, .5)), fmtNum(quantile(r.sentRatio, .95)), pct(above(r.sentRatio, 1)),
			pct(share("rtx")), pct(share("probation")), pct(topShare))
	}
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "–"
	}
	return fmt.Sprintf("%.1f%%", v)
}

// quantile is the q-quantile of xs (linear between closest ranks), NaN if empty.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(pos)
	if lo+1 >= len(s) {
		return s[len(s)-1]
	}
	return s[lo] + (pos-float64(lo))*(s[lo+1]-s[lo])
}

// above is the percentage of xs greater than t, NaN if empty.
func above(xs []float64, t float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	n := 0
	for _, x := range xs {
		if x > t {
			n++
		}
	}
	return 100 * float64(n) / float64(len(xs))
}

type promSeries struct {
	metric map[string]string
	values []float64
}

// promRange runs a range query; NaN/Inf samples are dropped.
func promRange(ctx context.Context, base, expr string, from, to time.Time, step time.Duration) ([]promSeries, error) {
	u := strings.TrimRight(base, "/") + "/api/v1/query_range?" + url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(from.Unix(), 10)},
		"end":   {strconv.FormatInt(to.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	}.Encode()
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
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("%s: %s", expr, out.Error)
	}
	res := make([]promSeries, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		s := promSeries{metric: r.Metric}
		for _, v := range r.Values {
			str, _ := v[1].(string)
			if f, err := strconv.ParseFloat(str, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				s.values = append(s.values, f)
			}
		}
		res = append(res, s)
	}
	return res, nil
}
