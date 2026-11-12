package stats

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
)

// NewRTCPInterceptorFactory returns an interceptor that counts the feedback a
// participant sends (NACK/PLI/FIR) and turns the SFU's receiver reports into
// publisher RTT. Both adapters register it, so the numbers come from the same
// code for either SFU.
//
// Register it before the default interceptors: an interceptor writes RTCP
// through the writer it was bound with, so only a writer bound ahead of the
// NACK generator sees the NACKs it sends.
func NewRTCPInterceptorFactory() interceptor.Factory { return rtcpFactory{} }

type rtcpFactory struct{}

func (rtcpFactory) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &rtcpInterceptor{lastReport: map[uint32]uint64{}}, nil
}

type rtcpInterceptor struct {
	interceptor.NoOp

	mu sync.Mutex
	// SSRC -> LSR<<32|DLSR of the last report block observed. Pion hands the
	// same compound packet to every sender it concerns; this drops the copies.
	lastReport map[uint32]uint64
}

func (r *rtcpInterceptor) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, a interceptor.Attributes) (int, error) {
		for _, p := range pkts {
			switch p := p.(type) {
			case *rtcp.TransportLayerNack:
				M.SubRTCPSent.WithLabelValues("nack").Inc()
				n := 0
				for _, pair := range p.Nacks {
					n += len(pair.PacketList())
				}
				M.SubNackedPackets.Add(float64(n))
			case *rtcp.PictureLossIndication:
				M.SubRTCPSent.WithLabelValues("pli").Inc()
			case *rtcp.FullIntraRequest:
				M.SubRTCPSent.WithLabelValues("fir").Inc()
			}
		}
		return w.Write(pkts, a)
	})
}

func (r *rtcpInterceptor) BindRTCPReader(rd interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, a, err := rd.Read(b, a)
		if err != nil {
			return n, a, err
		}
		if a == nil {
			a = make(interceptor.Attributes)
		}
		pkts, perr := a.GetRTCPPackets(b[:n])
		if perr != nil {
			return n, a, nil
		}
		now := time.Now()
		for _, p := range pkts {
			switch p := p.(type) {
			case *rtcp.ReceiverReport:
				r.observeRTT(p.Reports, now)
			case *rtcp.SenderReport:
				r.observeRTT(p.Reports, now)
			}
		}
		return n, a, nil
	})
}

// observeRTT applies RFC 3550 6.4.1: RTT = A - LSR - DLSR, all in 1/65536 s.
// LSR echoes our own SR's NTP time (wall clock), so A comes from the same clock.
func (r *rtcpInterceptor) observeRTT(reports []rtcp.ReceptionReport, now time.Time) {
	if len(reports) == 0 {
		return
	}
	a := compactNTP(now)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rep := range reports {
		if rep.LastSenderReport == 0 {
			continue // no SR received yet
		}
		key := uint64(rep.LastSenderReport)<<32 | uint64(rep.Delay)
		if r.lastReport[rep.SSRC] == key {
			continue
		}
		r.lastReport[rep.SSRC] = key
		rtt := a - rep.LastSenderReport - rep.Delay
		if rtt > 1<<31 { // negative: clock step or a stale report
			continue
		}
		M.PubRTT.Observe(float64(rtt) / 65536)
	}
}

// compactNTP is the middle 32 bits of the 64-bit NTP timestamp of t.
func compactNTP(t time.Time) uint32 {
	secs := uint64(t.Unix()) + 2208988800
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return uint32((secs<<32 | frac) >> 16)
}
