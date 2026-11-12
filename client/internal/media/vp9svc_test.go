package media

import (
	"bytes"
	"testing"

	"github.com/pion/rtp/codecs"
)

func unmarshalVP9(t *testing.T, payload []byte) codecs.VP9Packet {
	t.Helper()
	var p codecs.VP9Packet
	if _, err := p.Unmarshal(payload); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVP9SVCSpatialLayers(t *testing.T) {
	s := NewVP9SVC([]*Layer{
		{Width: 320, Height: 180},
		{Width: 640, Height: 360},
		{Width: 1280, Height: 720},
	})
	s.BeginPicture(0)
	frame := bytes.Repeat([]byte{0xab}, 3000)

	base := s.Packetize(frame, 0, true, 90000)
	if len(base) < 2 {
		t.Fatalf("expected a fragmented keyframe, got %d packets", len(base))
	}
	first := unmarshalVP9(t, base[0].Payload)
	if !first.B || first.E || !first.V || !first.L || first.P || first.SID != 0 || first.D || !first.Z || first.TID != 0 {
		t.Fatalf("base key header: %+v", first)
	}
	if first.NS != 2 || len(first.Width) != 3 || first.Width[2] != 1280 || first.Height[2] != 720 {
		t.Fatalf("scalability structure: NS=%d widths=%v heights=%v", first.NS, first.Width, first.Height)
	}
	// Temporal group of the L3T3 pattern 0,2,1,2.
	if first.NG != 4 || len(first.PGTID) != 4 {
		t.Fatalf("picture group: NG=%d PGTID=%v", first.NG, first.PGTID)
	}
	if first.PGTID[0] != 0 || first.PGTID[1] != 2 || first.PGTID[2] != 1 || first.PGTID[3] != 2 {
		t.Fatalf("picture group TIDs: %v", first.PGTID)
	}
	for g := range first.PGTID {
		if first.PGU[g] != true || len(first.PGPDiff[g]) != 1 {
			t.Fatalf("picture group %d: U=%v pdiff=%v", g, first.PGU[g], first.PGPDiff[g])
		}
	}
	if first.PGPDiff[0][0] != 4 || first.PGPDiff[1][0] != 1 || first.PGPDiff[2][0] != 2 || first.PGPDiff[3][0] != 1 {
		t.Fatalf("picture group P_DIFF: %v", first.PGPDiff)
	}
	if base[0].Marker || !base[len(base)-1].Marker {
		t.Fatal("marker must be set on the last packet of the layer")
	}
	last := unmarshalVP9(t, base[len(base)-1].Payload)
	if !last.E || last.B || last.PictureID != first.PictureID {
		t.Fatalf("last fragment: B=%v E=%v pid=%d want pid=%d", last.B, last.E, last.PictureID, first.PictureID)
	}

	// KEY mode: the upper spatial layers of a key picture are inter-layer
	// predicted (D) but not inter-picture predicted (P), so an SFU can switch
	// a subscriber up on them.
	top := s.Packetize(frame[:20], 2, true, 90000)
	if len(top) != 1 {
		t.Fatalf("short layer: %d packets", len(top))
	}
	high := unmarshalVP9(t, top[0].Payload)
	if high.SID != 2 || high.V || high.P || !high.D || high.PictureID != first.PictureID || !top[0].Marker {
		t.Fatalf("top layer of a key picture: %+v marker=%v", high, top[0].Marker)
	}

	s.BeginPicture(2)
	inter := s.Packetize(frame[:20], 1, false, 93000)
	delta := unmarshalVP9(t, inter[0].Payload)
	if !delta.P || delta.D || delta.V || delta.PictureID == first.PictureID || delta.SID != 1 || delta.TID != 2 || !delta.U {
		t.Fatalf("inter frame: %+v", delta)
	}
}

func TestVP9SVCTemporalLayers(t *testing.T) {
	s := NewVP9SVC([]*Layer{{Width: 320, Height: 180}})
	frame := bytes.Repeat([]byte{0x01}, 100)

	want := []uint8{0, 2, 1, 2, 0, 2, 1, 2}
	var tl0 []uint8
	for i, tid := range want {
		if got := TemporalID(i); got != tid {
			t.Fatalf("TemporalID(%d) = %d, want %d", i, got, tid)
		}
		s.BeginPicture(tid)
		p := unmarshalVP9(t, s.Packetize(frame, 0, i == 0, uint32(i*3000))[0].Payload)
		if p.TID != tid || !p.U {
			t.Fatalf("picture %d: TID=%d U=%v, want TID=%d U=true", i, p.TID, p.U, tid)
		}
		tl0 = append(tl0, p.TL0PICIDX)
	}
	// TL0PICIDX advances on TID 0 pictures only.
	if tl0[0] != tl0[1] || tl0[1] != tl0[2] || tl0[2] != tl0[3] || tl0[4] != tl0[0]+1 || tl0[7] != tl0[4] {
		t.Fatalf("TL0PICIDX sequence: %v", tl0)
	}
}
