package stats

import (
	"io"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	dto "github.com/prometheus/client_model/go"
)

func counter(t *testing.T, c interface{ Write(*dto.Metric) error }) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.Counter.GetValue()
}

type fakePkt struct {
	seq uint16
	rtx bool
}

func feed(pkts []fakePkt) ReadFunc {
	i := 0
	return func(b []byte) (int, bool, error) {
		if i == len(pkts) {
			return 0, false, io.EOF
		}
		p := pkts[i]
		i++
		raw, _ := (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: p.seq, Timestamp: uint32(p.seq) * 960}, Payload: []byte{1, 2, 3}}).Marshal()
		return copy(b, raw), p.rtx, nil
	}
}

func TestConsumeTrackOrderingAndRTX(t *testing.T) {
	lost0 := counter(t, M.SubLost.WithLabelValues("audio", "clean"))
	ooo0 := counter(t, M.SubOutOfOrder.WithLabelValues("audio"))
	rtx0 := counter(t, M.SubRTX.WithLabelValues("audio"))

	// Across the 16-bit wrap: 65534 arrives late after the wrap, 0 is lost and
	// then repaired by RTX, 3 is lost for good.
	ConsumeTrack(TrackInfo{Kind: "audio", ClockRate: 48000}, feed([]fakePkt{
		{65533, false}, {65535, false}, {1, false}, {65534, false}, {2, false}, {0, true}, {4, false},
	}), Options{})

	if d := counter(t, M.SubOutOfOrder.WithLabelValues("audio")) - ooo0; d != 1 {
		t.Errorf("out-of-order = %v, want 1", d)
	}
	if d := counter(t, M.SubRTX.WithLabelValues("audio")) - rtx0; d != 1 {
		t.Errorf("rtx = %v, want 1", d)
	}
	if d := counter(t, M.SubLost.WithLabelValues("audio", "clean")) - lost0; d != 1 {
		t.Errorf("lost = %v, want 1 (only seq 3)", d)
	}
}

func TestRTTFromReceiverReport(t *testing.T) {
	r := &rtcpInterceptor{lastReport: map[uint32]uint64{}}
	now := time.Now()
	lsr := compactNTP(now.Add(-150 * time.Millisecond))
	dlsr := uint32(65536 / 10) // SFU held the SR for ~100 ms
	rep := []rtcp.ReceptionReport{{SSRC: 1, LastSenderReport: lsr, Delay: dlsr}}

	var before dto.Metric
	_ = M.PubRTT.Write(&before)
	r.observeRTT(rep, now)
	r.observeRTT(rep, now) // same block delivered to a second sender: ignored
	var after dto.Metric
	_ = M.PubRTT.Write(&after)

	n := after.Histogram.GetSampleCount() - before.Histogram.GetSampleCount()
	got := after.Histogram.GetSampleSum() - before.Histogram.GetSampleSum()
	if n != 1 || got < 0.049 || got > 0.051 {
		t.Fatalf("got %d samples, rtt %.4fs; want 1 sample of 0.050s", n, got)
	}
}
