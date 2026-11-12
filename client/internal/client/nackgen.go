package client

import (
	"encoding/binary"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/prometheus/client_golang/prometheus"

	"sfubench/loadgen/internal/stats"
)

// nackGen is Pion's NACK generator (github.com/pion/interceptor pkg/nack,
// MIT, The Pion community) with two changes, used by both clients:
//
//   - A repaired packet counts as received. Pion hands RTX packets to the
//     application through the RTX stream and never tells the primary
//     stream's receive log, so the stock generator NACKs a repaired packet
//     every 100 ms until it leaves the 512-packet window (~3 s). mediasoup
//     resends it each time and the RTX bytes end up far above the loss rate.
//     readRepaired calls received for every repaired packet the track reads.
//   - A packet is NACKed at most nackMaxPerPacket times, like libwebrtc.
//
// One instance per PeerConnection. The mediasoup client builds one API per PC
// (nackGenFactory); the LiveKit SDK shares one registry, so it gets a
// nackGenSet.
type nackGen struct {
	interceptor.NoOp

	mu     sync.Mutex
	logs   map[uint32]*recvLog          // primary SSRC -> what arrived
	counts map[uint32]map[uint16]uint16 // primary SSRC -> seq -> NACKs sent
	skip   map[uint32]bool

	bound     bool
	close     chan struct{}
	closeOnce sync.Once
	onClose   func()
	wg        sync.WaitGroup
}

// NackGeneratorID names this generator in run.json. Bump it when its
// behaviour changes.
const NackGeneratorID = "sfubench-nackgen/1"

const (
	nackInterval     = 100 * time.Millisecond
	nackMaxPerPacket = 10
	nackLogSize      = 512 // packets; a power of two
	msProbatorSSRC   = 1234
)

// newNackGen never logs or NACKs the skip SSRCs.
func newNackGen(skip ...uint32) *nackGen {
	g := &nackGen{
		logs:   map[uint32]*recvLog{},
		counts: map[uint32]map[uint16]uint16{},
		skip:   map[uint32]bool{},
		close:  make(chan struct{}),
	}
	for _, s := range skip {
		g.skip[s] = true
	}
	return g
}

// nackGenFactory hands Pion the one generator of its PeerConnection.
type nackGenFactory struct{ g *nackGen }

func (f nackGenFactory) NewInterceptor(string) (interceptor.Interceptor, error) { return f.g, nil }

// nackGenSet is an interceptor.Factory that makes one nackGen per
// PeerConnection and routes repaired packets to the generator that logs
// their SSRC.
type nackGenSet struct {
	mu   sync.Mutex
	gens []*nackGen
}

func (s *nackGenSet) NewInterceptor(string) (interceptor.Interceptor, error) {
	g := newNackGen()
	g.onClose = func() { s.remove(g) }
	s.mu.Lock()
	s.gens = append(s.gens, g)
	s.mu.Unlock()
	return g, nil
}

func (s *nackGenSet) remove(g *nackGen) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gens = slices.DeleteFunc(s.gens, func(x *nackGen) bool { return x == g })
}

// received is nackGen.received on the newest generator that logs ssrc; false
// if none does.
func (s *nackGenSet) received(ssrc uint32, seq uint16) bool {
	s.mu.Lock()
	gens := slices.Clone(s.gens)
	s.mu.Unlock()
	for i := len(gens) - 1; i >= 0; i-- {
		if l := gens[i].log(ssrc); l != nil {
			return l.add(seq)
		}
	}
	return false
}

// BindRTCPWriter starts the NACK loop (once per PeerConnection).
func (g *nackGen) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.bound || g.closed() {
		return w
	}
	g.bound = true
	g.wg.Add(1)
	go g.loop(w)
	return w
}

// BindRemoteStream logs the sequence numbers of a stream that negotiated NACK.
// RTX streams have no RTCP feedback and are skipped, like in Pion; their
// packets reach the log through received.
func (g *nackGen) BindRemoteStream(info *interceptor.StreamInfo, r interceptor.RTPReader) interceptor.RTPReader {
	if !supportsNack(info) || g.skip[info.SSRC] {
		return r
	}
	l := newRecvLog(nackLogSize)
	g.mu.Lock()
	g.logs[info.SSRC] = l
	g.mu.Unlock()
	return interceptor.RTPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := r.Read(b, a)
		if err != nil {
			return 0, nil, err
		}
		if attr == nil {
			attr = interceptor.Attributes{}
		}
		h, err := attr.GetRTPHeader(b[:n])
		if err != nil {
			return 0, nil, err
		}
		l.add(h.SequenceNumber)
		return n, attr, nil
	})
}

func (g *nackGen) UnbindRemoteStream(info *interceptor.StreamInfo) {
	g.mu.Lock()
	delete(g.logs, info.SSRC)
	delete(g.counts, info.SSRC)
	g.mu.Unlock()
}

func (g *nackGen) Close() error {
	g.closeOnce.Do(func() {
		close(g.close)
		if g.onClose != nil {
			g.onClose()
		}
	})
	g.wg.Wait()
	return nil
}

func (g *nackGen) closed() bool {
	select {
	case <-g.close:
		return true
	default:
		return false
	}
}

// received marks a repaired packet (primary SSRC, original sequence number)
// and reports whether it had arrived already.
func (g *nackGen) received(ssrc uint32, seq uint16) bool {
	l := g.log(ssrc)
	return l != nil && l.add(seq)
}

func (g *nackGen) log(ssrc uint32) *recvLog {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.logs[ssrc]
}

func (g *nackGen) loop(w interceptor.RTCPWriter) {
	defer g.wg.Done()
	sender := rand.Uint32()
	t := time.NewTicker(nackInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			for _, p := range g.nacks(sender) {
				if _, err := w.Write([]rtcp.Packet{p}, interceptor.Attributes{}); err != nil {
					debugf("nack write: %v", err)
				}
			}
		case <-g.close:
			return
		}
	}
}

// nacks is one tick: a NACK per stream for its missing packets that have not
// been NACKed nackMaxPerPacket times yet.
func (g *nackGen) nacks(sender uint32) []rtcp.Packet {
	var out []rtcp.Packet
	buf := make([]uint16, nackLogSize)
	g.mu.Lock()
	defer g.mu.Unlock()
	for ssrc, l := range g.logs {
		missing := l.missing(buf)
		if len(missing) == 0 {
			delete(g.counts, ssrc)
			continue
		}
		prev := g.counts[ssrc]
		next := make(map[uint16]uint16, len(missing)) // drops seqs that arrived or left the window
		send := missing[:0]
		for _, seq := range missing {
			c := prev[seq]
			if c < nackMaxPerPacket {
				send = append(send, seq)
				c++
			}
			next[seq] = c
		}
		g.counts[ssrc] = next
		if len(send) == 0 {
			continue
		}
		out = append(out, &rtcp.TransportLayerNack{
			SenderSSRC: sender,
			MediaSSRC:  ssrc,
			Nacks:      rtcp.NackPairsFromSequenceNumbers(send),
		})
	}
	return out
}

func supportsNack(info *interceptor.StreamInfo) bool {
	for _, fb := range info.RTCPFeedback {
		if fb.Type == "nack" && fb.Parameter == "" {
			return true
		}
	}
	return false
}

// repairLog is where repaired packets are marked: a nackGen or a nackGenSet.
type repairLog interface {
	received(ssrc uint32, seq uint16) bool
}

// readRepaired is readTrack that also tells the NACK generator about every
// repaired packet and counts the ones that came more than once in dup. Pion
// returns them already unwrapped: primary SSRC, original sequence number,
// AttributeRtxSsrc set.
func readRepaired(t *webrtc.TrackRemote, g repairLog, dup prometheus.Counter) stats.ReadFunc {
	ssrc := uint32(t.SSRC())
	return func(b []byte) (int, bool, error) {
		n, a, err := t.Read(b)
		rtx := a.Get(webrtc.AttributeRtxSsrc) != nil
		if markRepaired(g, ssrc, b, n, rtx, err) {
			dup.Inc()
		}
		return n, rtx, err
	}
}

// markRepaired marks the packet b[:n] in g if it is a repair and reports
// whether it had arrived already.
func markRepaired(g repairLog, ssrc uint32, b []byte, n int, rtx bool, err error) (dup bool) {
	return rtx && err == nil && n >= 4 && g.received(ssrc, binary.BigEndian.Uint16(b[2:4]))
}

// recvLog is Pion's nack receiveLog: a bitmap of the last size sequence
// numbers, end the highest seen, lastConsecutive the last one with no gap
// before it.
type recvLog struct {
	mu              sync.Mutex
	packets         []uint64
	size            uint16
	end             uint16
	started         bool
	lastConsecutive uint16
}

const seqHalf = 1 << 15

func newRecvLog(size uint16) *recvLog {
	return &recvLog{packets: make([]uint64, size/64), size: size}
}

// add logs seq and reports whether it was logged already. A packet older
// than the window is ignored (Pion would set the bit of a newer one).
func (s *recvLog) add(seq uint16) (dup bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		s.set(seq)
		s.end, s.lastConsecutive, s.started = seq, seq, true
		return false
	}
	diff := seq - s.end
	switch {
	case diff == 0:
		return true
	case diff >= seqHalf: // older than end
		if s.end-seq >= s.size {
			return false
		}
		if s.get(seq) {
			return true
		}
		if s.lastConsecutive+1 == seq { // fills the first gap
			s.lastConsecutive = seq
			s.fixLastConsecutive()
		}
	default: // newer than end
		for i := s.end + 1; i != seq; i++ {
			s.del(i) // may still hold a packet from size ago
		}
		s.end = seq
		if s.lastConsecutive+1 == seq {
			s.lastConsecutive = seq
		} else if seq-s.lastConsecutive > s.size {
			s.lastConsecutive = seq - s.size
			s.fixLastConsecutive()
		}
	}
	s.set(seq)
	return false
}

// missing returns the gaps between lastConsecutive and end, in buf.
func (s *recvLog) missing(buf []uint16) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end-s.lastConsecutive >= seqHalf {
		return nil
	}
	c := 0
	for i := s.lastConsecutive + 1; i != s.end+1; i++ {
		if !s.get(i) {
			buf[c] = i
			c++
		}
	}
	return buf[:c]
}

func (s *recvLog) set(seq uint16) { p := seq % s.size; s.packets[p/64] |= 1 << (p % 64) }
func (s *recvLog) del(seq uint16) { p := seq % s.size; s.packets[p/64] &^= 1 << (p % 64) }
func (s *recvLog) get(seq uint16) bool {
	p := seq % s.size
	return s.packets[p/64]&(1<<(p%64)) != 0
}

func (s *recvLog) fixLastConsecutive() {
	i := s.lastConsecutive + 1
	for ; i != s.end+1 && s.get(i); i++ {
	}
	s.lastConsecutive = i - 1
}
