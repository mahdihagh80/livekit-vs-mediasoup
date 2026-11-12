package stats

import (
	"testing"
	"time"

	"github.com/pion/rtp"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
)

func threeLayers(codec string) *media.Video {
	return &media.Video{Codec: codec, Layers: []*media.Layer{
		{BitrateBps: 150_000}, {BitrateBps: 400_000}, {BitrateBps: 1_500_000}}}
}

func layerPkt(ts uint32, layer int, at time.Time) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: ts, Marker: true},
		Payload: media.AppendTrailerTag([]byte{0}, at, media.LayerTag(layer))}
}

// A VP9 SVC picture has a Marker on every spatial layer: it is one frame, of
// its highest layer.
func TestLayerFramesPerPicture(t *testing.T) {
	lt := newLayerTracker(threeLayers("vp9"), "high", "test-svc", nil)
	req0, low0 := counter(t, lt.cReq), counter(t, lt.cLow)
	now := time.Now()
	const pictures = 30
	for i := range pictures + 1 { // the extra picture completes the last one
		for sid := range 3 {
			lt.packet(now, layerPkt(uint32(i*3000), sid, now))
		}
		now = now.Add(33 * time.Millisecond)
	}
	lt.flush()
	if req, low := counter(t, lt.cReq)-req0, counter(t, lt.cLow)-low0; req != pictures || low != 0 {
		t.Fatalf("requested %v lower %v, want %d and 0", req, low, pictures)
	}
}

// On a capped link the SFU forwards medium although high is requested: after
// a dip, medium coming back is the up switch (not a miss).
func TestLayerSwitchUpToLayerBeforeDip(t *testing.T) {
	dip := netem.Dip{Kbps: 300, For: 5 * time.Second, Every: 30 * time.Second}
	sh := netem.NewShaper(netem.Profile{Name: "test-capped", Kbps: 1200, Dip: dip})
	t0 := time.Now()
	var dipAt time.Time
	for at := t0; at.Before(t0.Add(time.Minute)); at = at.Add(10 * time.Millisecond) {
		if active, _, started := sh.DipState(at); active {
			dipAt = started
			break
		}
	}
	if dipAt.IsZero() {
		t.Fatal("no dip in the first minute")
	}
	lt := newLayerTracker(threeLayers("vp8"), "high", "test-capped", sh)
	down, up := M.LayerSwitch.WithLabelValues("down"), M.LayerSwitch.WithLabelValues("up")
	down0, up0 := samples(t, down), samples(t, up)
	miss0 := counter(t, M.LayerSwitchMiss.WithLabelValues("up"))

	// Medium until 1 s into the dip, low until 2 s after it, then medium again.
	ts := uint32(0)
	for at := dipAt.Add(-3 * time.Second); at.Before(dipAt.Add(dip.For + 6*time.Second)); at = at.Add(33 * time.Millisecond) {
		l := 1
		if at.After(dipAt.Add(time.Second)) && at.Before(dipAt.Add(dip.For+2*time.Second)) {
			l = 0
		}
		lt.packet(at, layerPkt(ts, l, at))
		ts += 3000
	}
	if n := samples(t, down) - down0; n != 1 {
		t.Errorf("down switches %d, want 1", n)
	}
	if n := samples(t, up) - up0; n != 1 {
		t.Errorf("up switches %d, want 1", n)
	}
	if n := counter(t, M.LayerSwitchMiss.WithLabelValues("up")) - miss0; n != 0 {
		t.Errorf("up misses %v, want 0", n)
	}
}
