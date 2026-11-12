package stats

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"sfubench/loadgen/internal/netem"
)

// LinkCollector exports, for the people of the current run on this generator:
//
//	bench_peer_info{peer,profile} 1           every person, to give SFU per-peer
//	                                          series a profile (group_left)
//	bench_peer_link_bps{peer,profile}         downlink rate limit right now, read
//	                                          from the shaper at scrape time; only
//	                                          while a limit applies (the dip rate
//	                                          during a dip)
//
// Put next to the SFU's per-peer send estimate (mediasoup_peer_bwe_available_bps{peer}).
type LinkCollector struct {
	info  *prometheus.Desc
	desc  *prometheus.Desc
	mu    sync.Mutex
	links map[string]link
}

type link struct {
	profile string
	sh      *netem.Shaper
}

// Links holds the people of the current run on this generator.
var Links = func() *LinkCollector {
	c := &LinkCollector{
		info: prometheus.NewDesc("bench_peer_info", "One series per person of the run on this generator, with their network profile",
			[]string{"peer", "profile"}, nil),
		desc: prometheus.NewDesc("bench_peer_link_bps",
			"Downlink rate limit of a rate-limited person right now (bps); the dip rate during a dip",
			[]string{"peer", "profile"}, nil),
		links: map[string]link{},
	}
	prometheus.MustRegister(c)
	return c
}()

// Set registers a person and their shaper (nil: unshaped).
func (c *LinkCollector) Set(peer, profile string, sh *netem.Shaper) {
	c.mu.Lock()
	c.links[peer] = link{profile, sh}
	c.mu.Unlock()
}

// Reset forgets every person (end of a run).
func (c *LinkCollector) Reset() {
	c.mu.Lock()
	c.links = map[string]link{}
	c.mu.Unlock()
}

func (c *LinkCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.info
	ch <- c.desc
}

func (c *LinkCollector) Collect(ch chan<- prometheus.Metric) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for peer, l := range c.links {
		ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1, peer, l.profile)
		kbps := l.sh.RateKbps(now) // 0 for nil
		if kbps <= 0 {
			continue // no limit (unshaped, loss/delay only, or a dip-only profile between dips)
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(kbps)*1000, peer, l.profile)
	}
}
