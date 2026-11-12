// Package netem impairs one participant's network inside the load generator,
// without root or tc: it wraps the UDP sockets Pion's ICE agent opens (through
// a transport.Net given to the SettingEngine) and the signalling TCP conn.
//
// Model, per participant (all its sockets share one Shaper: one link clock, one
// outage switch). The profile shapes only the participant's subscribe connection
// (NewNet); its publish connection gets outages only (NewOutageNet), so what a
// publisher sends, and the SFU's feedback on it, stays clean:
//
//   - Downlink (SFU -> participant): random loss, a rate limit with a bounded
//     queue (tail drop), then a one-way delay that varies by ±jitter. The jitter
//     offset is re-drawn every 20 ms and packets never overtake each other, so
//     the receiver sees jitter and queueing, not reordering.
//   - Rate dips: the downlink rate drops to Dip.Rate for Dip.For every Dip.Every.
//   - Burst loss: a time-based Gilbert-Elliott link. Bad periods (every packet
//     lost) last Burst.Length on average, good periods (no loss) are long enough
//     that the link is bad for Burst.Loss of the time. Both are exponential.
//   - Outage (Shaper.Outage): every packet in both directions is dropped, and
//     signalling TCP reads and writes stall until it ends (TCP holds the data).
//   - Uplink: outages only. Delay and loss are applied once, on the downlink.
//   - Publish connection: outages only, in both directions.
package netem

import (
	"context"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
)

// Profile is a network condition. The zero value is a clean network.
type Profile struct {
	Name   string
	Delay  time.Duration // one-way, added on the downlink
	Jitter time.Duration // delay varies within ±Jitter
	Loss   float64       // downlink loss ratio, 0..1
	Kbps   int           // downlink rate limit, 0 = none
	Dip    Dip
	Burst  Burst
}

// Burst is bursty downlink loss (see the package comment).
type Burst struct {
	Loss   float64       // share of time in the bad state, 0..1 (= average loss)
	Length time.Duration // mean bad period
}

// on reports whether the burst model is enabled and sane.
func (b Burst) on() bool { return b.Loss > 0 && b.Loss < 1 && b.Length > 0 }

// meanGood is the mean good period that gives the average loss.
func (b Burst) meanGood() time.Duration {
	return time.Duration(float64(b.Length) * (1 - b.Loss) / b.Loss)
}

// Dip is a periodic drop of the downlink rate.
type Dip struct {
	Kbps  int
	For   time.Duration
	Every time.Duration
}

// Shaped reports whether packets need queueing (delay, loss or rate).
func (p Profile) Shaped() bool {
	return p.Delay > 0 || p.Jitter > 0 || p.Loss > 0 || p.Kbps > 0 || p.Dip.Kbps > 0 || p.Burst.on()
}

const (
	maxQueue     = 300 * time.Millisecond // downlink buffer before tail drop
	jitterPeriod = 20 * time.Millisecond
	chanDepth    = 4096
)

// Shaper is one participant's link.
type Shaper struct {
	p      Profile
	start  time.Time
	phase  time.Duration // first dip starts at start+phase
	outage atomic.Int64  // unix nanos the current outage ends; 0 = none

	mu          sync.Mutex
	rng         *rand.Rand
	linkFree    time.Time // when the rate-limited link finishes the queued bytes
	lastDeliver time.Time
	jitterAt    time.Time
	jitterOff   time.Duration
	geBad       bool      // burst model: in the bad state
	geUntil     time.Time // burst model: when the current state ends
}

// NewShaper starts the link clock now. The first rate dip starts at a random
// time between Every/2 and Every, so dips of different participants don't line up.
func NewShaper(p Profile) *Shaper {
	s := &Shaper{p: p, start: time.Now(), rng: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))}
	if p.Dip.Every > 0 {
		s.phase = p.Dip.Every/2 + time.Duration(rand.Int64N(int64(p.Dip.Every/2)+1))
	}
	return s
}

// Profile returns the condition this Shaper applies.
func (s *Shaper) Profile() Profile {
	if s == nil {
		return Profile{}
	}
	return s.p
}

// Outage drops all traffic for d from now (extends a running outage).
func (s *Shaper) Outage(d time.Duration) {
	end := time.Now().Add(d).UnixNano()
	for {
		cur := s.outage.Load()
		if cur >= end || s.outage.CompareAndSwap(cur, end) {
			return
		}
	}
}

// inOutage returns the end of the running outage, if any.
func (s *Shaper) inOutage(now time.Time) (time.Time, bool) {
	end := s.outage.Load()
	if end == 0 || now.UnixNano() >= end {
		return time.Time{}, false
	}
	return time.Unix(0, end), true
}

// DipState reports whether a rate dip is running at now, which dip it is
// (counting from 1, 0 before the first one) and when that dip started.
func (s *Shaper) DipState(now time.Time) (active bool, n int, started time.Time) {
	if s == nil || s.p.Dip.Every <= 0 || s.p.Dip.For <= 0 {
		return false, 0, time.Time{}
	}
	el := now.Sub(s.start) - s.phase
	if el < 0 {
		return false, 0, time.Time{}
	}
	k := int(el / s.p.Dip.Every)
	started = s.start.Add(s.phase + time.Duration(k)*s.p.Dip.Every)
	return now.Sub(started) < s.p.Dip.For, k + 1, started
}

func (s *Shaper) rateAt(now time.Time) int {
	if active, _, _ := s.DipState(now); active {
		return s.p.Dip.Kbps
	}
	return s.p.Kbps
}

// RateKbps is the downlink rate limit at now in kbps, 0 = no limit.
// Outages (netloss events) are not reflected.
func (s *Shaper) RateKbps(now time.Time) int {
	if s == nil {
		return 0
	}
	return s.rateAt(now)
}

// Clock returns when the shaper's clock started and the offset of the first
// dip from it: dip k (from 0) runs [start+phase+k*Every, +For).
func (s *Shaper) Clock() (start time.Time, phase time.Duration) {
	if s == nil {
		return time.Time{}, 0
	}
	return s.start, s.phase
}

// downlink decides the fate of a received packet of n bytes: drop, or deliver at.
func (s *Shaper) downlink(n int, now time.Time) (at time.Time, ok bool) {
	if _, out := s.inOutage(now); out {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.p.Burst.on() && s.burstLost(now) {
		return time.Time{}, false
	}
	if s.p.Loss > 0 && s.rng.Float64() < s.p.Loss {
		return time.Time{}, false
	}
	depart := now
	if kbps := s.rateAt(now); kbps > 0 {
		if s.linkFree.After(depart) {
			depart = s.linkFree
		}
		if depart.Sub(now) > maxQueue {
			return time.Time{}, false // tail drop: the queue is full
		}
		depart = depart.Add(time.Duration(int64(n) * 8 * int64(time.Millisecond) / int64(kbps)))
		s.linkFree = depart
	}
	if s.p.Jitter > 0 && now.Sub(s.jitterAt) >= jitterPeriod {
		s.jitterAt = now
		s.jitterOff = time.Duration(s.rng.Int64N(int64(2*s.p.Jitter)+1)) - s.p.Jitter
	}
	at = depart.Add(s.p.Delay + s.jitterOff)
	if at.Before(s.lastDeliver) {
		at = s.lastDeliver // no reordering
	}
	s.lastDeliver = at
	return at, true
}

// burstLost advances the Gilbert-Elliott state to now and reports whether the
// link is in the bad state. Call with s.mu held. The first state is good (or
// bad with the stationary probability) and starts at the first packet.
func (s *Shaper) burstLost(now time.Time) bool {
	b := s.p.Burst
	good := b.meanGood()
	draw := func(bad bool) time.Duration {
		m := good
		if bad {
			m = b.Length
		}
		return time.Duration(s.rng.ExpFloat64() * float64(m))
	}
	// First packet, or a long gap without packets (the state at now is
	// then independent of the old one): start fresh from the stationary mix.
	if s.geUntil.IsZero() || now.Sub(s.geUntil) > 10*(good+b.Length) {
		s.geBad = s.rng.Float64() < b.Loss
		s.geUntil = now.Add(draw(s.geBad))
	}
	for !now.Before(s.geUntil) {
		s.geBad = !s.geBad
		s.geUntil = s.geUntil.Add(draw(s.geBad))
	}
	return s.geBad
}

// Net is a transport.Net whose UDP sockets go through a Shaper. Give it to a
// webrtc.SettingEngine with SetNet.
type Net struct {
	*stdnet.Net
	sh     *Shaper
	shaped bool // apply the profile; false = outages only
}

var (
	baseOnce sync.Once
	base     *stdnet.Net
	baseErr  error
)

// NewNet wraps the standard network. The interface list is read once per
// process and shared, since thousands of participants would otherwise each
// enumerate the host's interfaces.
// NewNet applies the whole profile: use it for the subscribe connection.
func NewNet(sh *Shaper) (*Net, error) { return newNet(sh, true) }

// NewOutageNet applies outages only: use it for the publish connection.
func NewOutageNet(sh *Shaper) (*Net, error) { return newNet(sh, false) }

func newNet(sh *Shaper, shaped bool) (*Net, error) {
	baseOnce.Do(func() { base, baseErr = stdnet.NewNet() })
	if baseErr != nil {
		return nil, baseErr
	}
	return &Net{Net: base, sh: sh, shaped: shaped}, nil
}

// ListenUDP opens a shaped UDP socket (ICE host candidates use this).
func (n *Net) ListenUDP(network string, laddr *net.UDPAddr) (transport.UDPConn, error) {
	c, err := n.Net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}
	return newUDPConn(c, n.sh, n.shaped), nil
}

// DialContext dials a TCP conn whose reads and writes stall during outages.
// Use it as the signalling WebSocket's NetDialContext.
func (s *Shaper) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if end, out := s.inOutage(time.Now()); out {
		t := time.NewTimer(time.Until(end))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		}
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &tcpConn{Conn: c, sh: s}, nil
}

type packet struct {
	buf  []byte
	n    int
	addr net.Addr
	at   time.Time
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1600); return &b }}

// udpConn shapes reads and drops writes during outages. Without a shaped
// profile it reads straight from the socket and only filters outages.
type udpConn struct {
	transport.UDPConn
	sh     *Shaper
	shaped bool

	ch      chan packet
	done    chan struct{} // closed when the reader goroutine stops
	readErr error         // set before done is closed

	rmu      sync.Mutex // serializes shaped reads
	head     *packet    // popped but not yet due
	deadline atomic.Int64
	dlChange chan struct{}
}

func newUDPConn(c transport.UDPConn, sh *Shaper, shaped bool) *udpConn {
	u := &udpConn{UDPConn: c, sh: sh, shaped: shaped && sh.p.Shaped()}
	if u.shaped {
		u.ch = make(chan packet, chanDepth)
		u.done = make(chan struct{})
		u.dlChange = make(chan struct{}, 1)
		go u.readLoop()
	}
	return u
}

func (u *udpConn) readLoop() {
	defer close(u.done)
	for {
		bp := bufPool.Get().(*[]byte)
		n, addr, err := u.UDPConn.ReadFrom(*bp)
		if err != nil {
			bufPool.Put(bp)
			u.readErr = err
			return
		}
		at, ok := u.sh.downlink(n, time.Now())
		if !ok {
			bufPool.Put(bp)
			continue
		}
		select {
		case u.ch <- packet{buf: *bp, n: n, addr: addr, at: at}:
		default: // reader too slow: drop like a full socket buffer
			bufPool.Put(bp)
		}
	}
}

var errTimeout = &net.OpError{Op: "read", Net: "udp", Err: os.ErrDeadlineExceeded}

func (u *udpConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if !u.shaped {
		for {
			n, addr, err := u.UDPConn.ReadFrom(b)
			if err != nil {
				return n, addr, err
			}
			if _, out := u.sh.inOutage(time.Now()); !out {
				return n, addr, nil
			}
		}
	}
	u.rmu.Lock()
	defer u.rmu.Unlock()
	var dt *time.Timer
	defer func() {
		if dt != nil {
			dt.Stop()
		}
	}()
	for {
		var dlTimer <-chan time.Time
		if dl := u.deadline.Load(); dl != 0 {
			wait := time.Until(time.Unix(0, dl))
			if wait <= 0 {
				return 0, nil, errTimeout
			}
			if dt == nil {
				dt = time.NewTimer(wait)
			} else {
				dt.Reset(wait)
			}
			dlTimer = dt.C
		}
		if u.head == nil {
			select {
			case p := <-u.ch:
				u.head = &p
			case <-u.done:
				// Deliver what is queued before reporting the error.
				select {
				case p := <-u.ch:
					u.head = &p
				default:
					return 0, nil, u.readErr
				}
			case <-dlTimer:
				return 0, nil, errTimeout
			case <-u.dlChange:
				continue
			}
		}
		if wait := time.Until(u.head.at); wait > time.Millisecond {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-dlTimer:
				t.Stop()
				return 0, nil, errTimeout
			case <-u.dlChange:
				t.Stop()
				continue
			}
		}
		p := u.head
		u.head = nil
		n := copy(b, p.buf[:p.n])
		buf := p.buf
		bufPool.Put(&buf)
		return n, p.addr, nil
	}
}

func (u *udpConn) Read(b []byte) (int, error) {
	n, _, err := u.ReadFrom(b)
	return n, err
}

func (u *udpConn) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	n, addr, err := u.ReadFrom(b)
	ua, _ := addr.(*net.UDPAddr)
	return n, ua, err
}

func (u *udpConn) ReadMsgUDP(b, _ []byte) (n, oobn, flags int, addr *net.UDPAddr, err error) {
	n, addr, err = u.ReadFromUDP(b)
	return n, 0, 0, addr, err
}

func (u *udpConn) SetReadDeadline(t time.Time) error {
	if !u.shaped {
		return u.UDPConn.SetReadDeadline(t)
	}
	if t.IsZero() {
		u.deadline.Store(0)
	} else {
		u.deadline.Store(t.UnixNano())
	}
	select {
	case u.dlChange <- struct{}{}:
	default:
	}
	return nil
}

func (u *udpConn) SetDeadline(t time.Time) error {
	if err := u.SetReadDeadline(t); err != nil {
		return err
	}
	return u.UDPConn.SetWriteDeadline(t)
}

func (u *udpConn) dropWrite() bool {
	_, out := u.sh.inOutage(time.Now())
	return out
}

func (u *udpConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if u.dropWrite() {
		return len(b), nil
	}
	return u.UDPConn.WriteTo(b, addr)
}

func (u *udpConn) Write(b []byte) (int, error) {
	if u.dropWrite() {
		return len(b), nil
	}
	return u.UDPConn.Write(b)
}

func (u *udpConn) WriteToUDP(b []byte, addr *net.UDPAddr) (int, error) {
	if u.dropWrite() {
		return len(b), nil
	}
	return u.UDPConn.WriteToUDP(b, addr)
}

func (u *udpConn) WriteMsgUDP(b, oob []byte, addr *net.UDPAddr) (n, oobn int, err error) {
	if u.dropWrite() {
		return len(b), len(oob), nil
	}
	return u.UDPConn.WriteMsgUDP(b, oob, addr)
}

// tcpConn holds signalling data back while an outage runs, as TCP would.
type tcpConn struct {
	net.Conn
	sh *Shaper

	wmu sync.Mutex
	wdl time.Time
}

func (c *tcpConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if end, out := c.sh.inOutage(time.Now()); out {
		time.Sleep(time.Until(end))
	}
	return n, err
}

func (c *tcpConn) Write(b []byte) (int, error) {
	if end, out := c.sh.inOutage(time.Now()); out {
		c.wmu.Lock()
		dl := c.wdl
		c.wmu.Unlock()
		if !dl.IsZero() && dl.Before(end) {
			time.Sleep(time.Until(dl))
			return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		time.Sleep(time.Until(end))
	}
	return c.Conn.Write(b)
}

func (c *tcpConn) SetWriteDeadline(t time.Time) error {
	c.wmu.Lock()
	c.wdl = t
	c.wmu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *tcpConn) SetDeadline(t time.Time) error {
	c.wmu.Lock()
	c.wdl = t
	c.wmu.Unlock()
	return c.Conn.SetDeadline(t)
}
