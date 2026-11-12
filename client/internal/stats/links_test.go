package stats

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"sfubench/loadgen/internal/netem"
)

// collect returns the collector's series as peer -> value.
func collect(t *testing.T, c prometheus.Collector) map[string]float64 {
	ch := make(chan prometheus.Metric, 16)
	c.Collect(ch)
	close(ch)
	out := map[string]float64{}
	for m := range ch {
		if m.Desc() != Links.desc {
			continue
		}
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			t.Fatal(err)
		}
		for _, l := range d.GetLabel() {
			if l.GetName() == "peer" {
				out[l.GetValue()] = d.GetGauge().GetValue()
			}
		}
	}
	return out
}

func TestLinks(t *testing.T) {
	defer Links.Reset()
	Links.Set("capped-1", "capped", netem.NewShaper(netem.Profile{Name: "capped", Kbps: 1000}))
	// Dip-only: the first dip starts Every/2..Every after creation, so no limit yet.
	Links.Set("dippy-1", "dippy", netem.NewShaper(netem.Profile{Name: "dippy",
		Dip: netem.Dip{Kbps: 600, For: 20 * time.Second, Every: time.Minute}}))
	Links.Set("lossy-1", "lossy", netem.NewShaper(netem.Profile{Name: "lossy", Loss: 0.03})) // no rate limit
	Links.Set("clean-1", "clean", nil)

	ch := make(chan prometheus.Metric, 16)
	Links.Collect(ch)
	if n := len(ch); n != 5 { // 4 info + 1 link
		t.Fatalf("got %d series, want 5", n)
	}

	got := collect(t, Links)
	if len(got) != 1 || got["capped-1"] != 1e6 {
		t.Fatalf("got %v, want only capped-1 = 1e6 bps", got)
	}
	Links.Reset()
	if got := collect(t, Links); len(got) != 0 {
		t.Fatalf("after Reset: %v", got)
	}
}
