package main

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// promGather renders selected metrics from the default registry as short text
// lines (histograms as count/mean/p50/p95 estimated from buckets).
func promGather() (map[string]string, error) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, mf := range mfs {
		var lines []string
		for _, m := range mf.GetMetric() {
			lbl := labels(m)
			switch {
			case m.Counter != nil:
				lines = append(lines, fmt.Sprintf("  %s%s = %.0f", mf.GetName(), lbl, m.Counter.GetValue()))
			case m.Gauge != nil:
				lines = append(lines, fmt.Sprintf("  %s%s = %.0f", mf.GetName(), lbl, m.Gauge.GetValue()))
			case m.Histogram != nil:
				h := m.Histogram
				if h.GetSampleCount() == 0 {
					continue
				}
				lines = append(lines, fmt.Sprintf("  %s%s count=%d mean=%.4f p50≈%.4f p95≈%.4f", mf.GetName(), lbl,
					h.GetSampleCount(), h.GetSampleSum()/float64(h.GetSampleCount()), quantile(h, 0.5), quantile(h, 0.95)))
			}
		}
		sort.Strings(lines)
		out[mf.GetName()] = strings.Join(lines, "\n")
	}
	return out, nil
}

func labels(m *dto.Metric) string {
	if len(m.Label) == 0 {
		return ""
	}
	var p []string
	for _, l := range m.Label {
		p = append(p, l.GetName()+"="+l.GetValue())
	}
	return "{" + strings.Join(p, ",") + "}"
}

func quantile(h *dto.Histogram, q float64) float64 {
	total := float64(h.GetSampleCount())
	rank := q * total
	prevCount, prevBound := 0.0, 0.0
	for _, b := range h.Bucket {
		c := float64(b.GetCumulativeCount())
		if c >= rank {
			if c == prevCount {
				return b.GetUpperBound()
			}
			return prevBound + (b.GetUpperBound()-prevBound)*(rank-prevCount)/(c-prevCount)
		}
		prevCount, prevBound = c, b.GetUpperBound()
	}
	return math.Inf(1)
}
