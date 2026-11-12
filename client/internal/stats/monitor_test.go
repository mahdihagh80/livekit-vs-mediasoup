package stats

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func samples(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	var m dto.Metric
	if err := o.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.Histogram.GetSampleCount()
}

func TestPublisherReconnect(t *testing.T) {
	rec := M.Recovery.WithLabelValues("publisher_reconnect")
	t0 := time.Now()

	// Named tracks: a republish 400 ms after the last packet counts, even
	// though the old track has not ended yet; the other source does not match.
	m := NewMonitor(true)
	n0 := samples(t, rec)
	m.track("alice", "video", "cam", t0)
	m.track("alice", "video", "screen", t0)
	m.track("alice", "video", "cam", t0.Add(400*time.Millisecond))
	if got := samples(t, rec) - n0; got != 1 {
		t.Fatalf("named: %d recoveries, want 1", got)
	}

	// Unnamed (mediasoup): a live track of the same kind is the publisher's
	// other video, an ended one was replaced whatever the gap.
	m = NewMonitor(true)
	n0 = samples(t, rec)
	cam := m.track("bob", "video", "", t0)
	m.track("bob", "video", "", t0.Add(100*time.Millisecond))
	if got := samples(t, rec) - n0; got != 0 {
		t.Fatalf("unnamed live: %d recoveries, want 0", got)
	}
	cam.ended.Store(true)
	m.track("bob", "video", "", t0.Add(300*time.Millisecond))
	if got := samples(t, rec) - n0; got != 1 {
		t.Fatalf("unnamed ended: %d recoveries, want 1", got)
	}

	// A subscriber disturbed itself in the last 60 s does not count it.
	m = NewMonitor(true)
	n0 = samples(t, rec)
	m.track("carol", "audio", "mic", t0)
	m.Disturbed("mass_rejoin", t0, false)
	m.track("carol", "audio", "mic", t0.Add(500*time.Millisecond))
	if got := samples(t, rec) - n0; got != 0 {
		t.Fatalf("disturbed: %d recoveries, want 0", got)
	}
}
