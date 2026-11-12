package client

import (
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// feed binds a NACK-capable stream and passes the given sequence numbers
// through the generator's reader.
func feed(t *testing.T, g *nackGen, ssrc uint32, fb []interceptor.RTCPFeedback, seqs ...uint16) {
	t.Helper()
	var next []byte
	r := g.BindRemoteStream(&interceptor.StreamInfo{SSRC: ssrc, RTCPFeedback: fb},
		interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
			return copy(b, next), a, nil
		}))
	buf := make([]byte, 1500)
	for _, s := range seqs {
		var err error
		if next, err = (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: s, SSRC: ssrc}}).Marshal(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.Read(buf, nil); err != nil {
			t.Fatal(err)
		}
	}
}

var nackFB = []interceptor.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}}

// nacked is the sequence numbers NACKed for ssrc in one tick.
func nacked(g *nackGen, ssrc uint32) []uint16 {
	var out []uint16
	for _, p := range g.nacks(1) {
		n := p.(*rtcp.TransportLayerNack)
		if n.MediaSSRC != ssrc {
			continue
		}
		for _, pair := range n.Nacks {
			out = append(out, pair.PacketList()...)
		}
	}
	return out
}

func TestNackGenRepairedNotRenacked(t *testing.T) {
	g := newNackGen()
	feed(t, g, 1000, nackFB, 1, 2, 3, 5, 6, 8, 9)
	if got := nacked(g, 1000); !slices.Equal(got, []uint16{4, 7}) {
		t.Fatalf("first tick: %v, want [4 7]", got)
	}
	if g.received(1000, 4) { // RTX repaired 4
		t.Fatal("4 reported as a duplicate")
	}
	if !g.received(1000, 4) {
		t.Fatal("second RTX for 4 not reported as a duplicate")
	}
	if !g.received(1000, 3) {
		t.Fatal("RTX for a packet that arrived not reported as a duplicate")
	}
	if got := nacked(g, 1000); !slices.Equal(got, []uint16{7}) {
		t.Fatalf("after repair: %v, want [7]", got)
	}
	g.received(1000, 7)
	var end uint16 = 9
	if g.received(1000, end-nackLogSize) {
		t.Fatal("packet older than the window reported as a duplicate")
	}
	if got := nacked(g, 1000); len(got) != 0 {
		t.Fatalf("all repaired: %v, want none", got)
	}
}

func TestNackGenMaxPerPacket(t *testing.T) {
	g := newNackGen()
	feed(t, g, 1000, nackFB, 10, 12)
	n := 0
	for range 3 * nackMaxPerPacket {
		if slices.Contains(nacked(g, 1000), 11) {
			n++
		}
	}
	if n != nackMaxPerPacket {
		t.Fatalf("11 NACKed %d times, want %d", n, nackMaxPerPacket)
	}
	// a new loss still gets its own NACKs
	g.received(1000, 14) // arrives out of order: 13 is missing too
	if got := nacked(g, 1000); !slices.Equal(got, []uint16{13}) {
		t.Fatalf("new gap: %v, want [13]", got)
	}
}

func TestNackGenSkips(t *testing.T) {
	g := newNackGen(msProbatorSSRC)
	feed(t, g, 2000, nil, 1, 3)              // RTX stream: no RTCP feedback
	feed(t, g, msProbatorSSRC, nackFB, 1, 3) // mediasoup padding
	if p := g.nacks(1); len(p) != 0 {
		t.Fatalf("NACKs for skipped streams: %v", p)
	}
	g.received(3000, 1) // unknown SSRC: ignored
}

func TestNackGenSetSkipsNothing(t *testing.T) {
	g := setGen(t, &nackGenSet{})
	feed(t, g, msProbatorSSRC, nackFB, 1, 3)
	if got := nacked(g, msProbatorSSRC); !slices.Equal(got, []uint16{2}) {
		t.Fatalf("SSRC %d on a LiveKit generator: %v, want [2]", msProbatorSSRC, got)
	}
}

func TestRecvLogWrap(t *testing.T) {
	l := newRecvLog(nackLogSize)
	for _, s := range []uint16{65533, 65534, 0, 1} {
		l.add(s)
	}
	if got := l.missing(make([]uint16, nackLogSize)); !slices.Equal(got, []uint16{65535}) {
		t.Fatalf("missing across wrap: %v, want [65535]", got)
	}
	l.add(65535)
	if got := l.missing(make([]uint16, nackLogSize)); len(got) != 0 {
		t.Fatalf("after fill: %v", got)
	}
	// a jump past the window gives up on what fell out of it
	l.add(1 + 2*nackLogSize)
	if got := l.missing(make([]uint16, nackLogSize)); len(got) != nackLogSize-1 {
		t.Fatalf("after jump: %d missing, want %d", len(got), nackLogSize-1)
	}
}

func setGen(t *testing.T, s *nackGenSet) *nackGen {
	t.Helper()
	i, err := s.NewInterceptor("")
	if err != nil {
		t.Fatal(err)
	}
	return i.(*nackGen)
}

func TestNackGenSetRepairedNotRenacked(t *testing.T) {
	s := &nackGenSet{}
	g := setGen(t, s)
	feed(t, g, 1000, nackFB, 1, 2, 4)
	if got := nacked(g, 1000); !slices.Equal(got, []uint16{3}) {
		t.Fatalf("first tick: %v, want [3]", got)
	}
	if s.received(1000, 3) {
		t.Fatal("first repair of 3 reported as a duplicate")
	}
	if got := nacked(g, 1000); len(got) != 0 {
		t.Fatalf("after repair: %v, want none", got)
	}
}

func TestNackGenSetMaxPerPacket(t *testing.T) {
	g := setGen(t, &nackGenSet{})
	feed(t, g, 1000, nackFB, 10, 12)
	n := 0
	for range 3 * nackMaxPerPacket {
		if slices.Contains(nacked(g, 1000), 11) {
			n++
		}
	}
	if n != nackMaxPerPacket {
		t.Fatalf("11 NACKed %d times, want %d", n, nackMaxPerPacket)
	}
}

func TestNackGenSetRouting(t *testing.T) {
	s := &nackGenSet{}
	g1, g2 := setGen(t, s), setGen(t, s)
	feed(t, g1, 1000, nackFB, 1, 3)
	feed(t, g2, 2000, nackFB, 1, 3)
	s.received(1000, 2)
	if got := nacked(g1, 1000); len(got) != 0 {
		t.Fatalf("gen1 after repair: %v, want none", got)
	}
	if got := nacked(g2, 2000); !slices.Equal(got, []uint16{2}) {
		t.Fatalf("gen2 touched by gen1's repair: %v, want [2]", got)
	}
	if s.received(3000, 1) {
		t.Fatal("unknown SSRC reported as a duplicate")
	}
}

func TestNackGenSetLifecycle(t *testing.T) {
	s := &nackGenSet{}
	g1 := setGen(t, s)
	feed(t, g1, 1000, nackFB, 1, 3)
	g1.BindRTCPWriter(interceptor.RTCPWriterFunc(func([]rtcp.Packet, interceptor.Attributes) (int, error) { return 0, nil }))
	if err := g1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := g1.Close(); err != nil {
		t.Fatal(err)
	}
	if len(s.gens) != 0 {
		t.Fatalf("%d generators after Close, want 0", len(s.gens))
	}
	s.received(1000, 2)
	if s.received(1000, 2) {
		t.Fatal("closed generator still routed to")
	}

	old, cur := setGen(t, s), setGen(t, s)
	feed(t, old, 5000, nackFB, 1, 3)
	feed(t, cur, 5000, nackFB, 1, 3)
	s.received(5000, 2)
	if got := nacked(cur, 5000); len(got) != 0 {
		t.Fatalf("newer generator after repair: %v, want none", got)
	}
	if got := nacked(old, 5000); !slices.Equal(got, []uint16{2}) {
		t.Fatalf("older generator: %v, want [2]", got)
	}
}

func TestNackGenSetConcurrent(t *testing.T) {
	s := &nackGenSet{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			g := setGen(t, s)
			feed(t, g, uint32(1000+i), nackFB, 1, 3)
			_ = g.Close()
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				s.received(uint32(1000+i), 2)
			}
		}()
	}
	wg.Wait()
	if len(s.gens) != 0 {
		t.Fatalf("%d generators left", len(s.gens))
	}
}

type countLog struct{ calls int }

func (l *countLog) received(uint32, uint16) bool { l.calls++; return false }

func TestMarkRepaired(t *testing.T) {
	pkt, err := (&rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 2, SSRC: 1000}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	g := newNackGen()
	feed(t, g, 1000, nackFB, 1, 3)
	if markRepaired(g, 1000, pkt, len(pkt), true, nil) {
		t.Fatal("first repair reported as a duplicate")
	}
	if !markRepaired(g, 1000, pkt, len(pkt), true, nil) {
		t.Fatal("second repair not reported as a duplicate")
	}

	l := &countLog{}
	markRepaired(l, 1000, pkt, len(pkt), false, nil)
	markRepaired(l, 1000, pkt, len(pkt), true, io.EOF)
	markRepaired(l, 1000, pkt, 3, true, nil)
	if l.calls != 0 {
		t.Fatalf("log touched %d times by non-repairs", l.calls)
	}
}
