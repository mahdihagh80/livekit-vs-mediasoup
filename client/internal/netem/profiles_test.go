package netem

// Checks that each network profile of plan 61 (W2) does what it says.
// First on the shaper's own clock (exact and fast: minutes of link time in
// milliseconds), then through a real loopback UDP socket in real time.
// Run with -v to see the measured numbers.

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"sort"
	"strings"
	"testing"
	"time"
)

// Plan 61's impaired profiles as rooms.newRoom builds them (plan loss and
// burst loss are percent there, divided by 100).
var plan61 = map[string]Profile{
	"clean":  {Name: "clean"},
	"mobile": {Name: "mobile", Delay: 60 * time.Millisecond, Jitter: 20 * time.Millisecond, Loss: 0.01},
	"dippy":  {Name: "dippy", Dip: Dip{Kbps: 300, For: 15 * time.Second, Every: time.Minute}},
	"capped": {Name: "capped", Kbps: 1000},
	"burst":  {Name: "burst", Burst: Burst{Loss: 0.05, Length: 50 * time.Millisecond}},
	"far":    {Name: "far", Delay: 150 * time.Millisecond, Jitter: 30 * time.Millisecond},
}

// arrival is one packet reaching the viewer's socket.
type arrival struct {
	at     time.Time
	size   int
	marker bool // last packet of a video frame (what the e2e histogram samples)
	audio  bool
}

// viewerTraffic is what one plan-61 viewer receives for d: 6 video tracks at
// 30 fps, each frame's packets back to back (as the SFU forwards a frame
// that came in as a burst), and 6 Opus tracks with one packet per 20 ms.
// videoKbps is per track.
func viewerTraffic(t0 time.Time, d time.Duration, videoKbps int, rng *rand.Rand) []arrival {
	var out []arrival
	const frame = time.Second / 30
	for tr := 0; tr < 6; tr++ {
		phase := time.Duration(rng.Int64N(int64(frame)))
		for ft := phase; ft < d; ft += frame {
			bytes := videoKbps * 1000 / 8 / 30
			bytes = int(float64(bytes) * (0.7 + 0.6*rng.Float64())) // frame sizes vary
			for off := 0; bytes > 0; off++ {
				sz := min(bytes, 1100)
				bytes -= sz
				out = append(out, arrival{at: t0.Add(ft + time.Duration(off)*80*time.Microsecond),
					size: sz + 40, marker: bytes == 0})
			}
		}
		aphase := time.Duration(rng.Int64N(int64(20 * time.Millisecond)))
		for at := aphase; at < d; at += 20 * time.Millisecond {
			out = append(out, arrival{at: t0.Add(at), size: 110, audio: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

func sortDur(d []time.Duration) []time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d
}

// cdfAt formats the share of delays <= each Prometheus bucket edge of
// bench_sub_delay_seconds, to compare with the live histogram.
func cdfAt(sorted []time.Duration, edges ...float64) string {
	var b strings.Builder
	for _, e := range edges {
		lim := time.Duration(e * float64(time.Second))
		n := sort.Search(len(sorted), func(i int) bool { return sorted[i] > lim })
		fmt.Fprintf(&b, " le%g=%.3f", e, float64(n)/float64(len(sorted)))
	}
	return b.String()
}

// Delay and jitter: every packet's added delay stays inside Delay ± Jitter,
// packets never reorder, the jitter offset is redrawn no more often than
// every 20 ms and is uniform over ±Jitter.
func TestPlan61DelayJitter(t *testing.T) {
	for _, name := range []string{"mobile", "far"} {
		t.Run(name, func(t *testing.T) {
			p := plan61[name]
			s := NewShaper(p)
			rng := rand.New(rand.NewPCG(1, 2))
			t0 := time.Now()
			arr := viewerTraffic(t0, 10*time.Minute, 730, rng)

			var all, markers, audio []time.Duration
			var offs []float64
			var changes []time.Duration
			var lastAt, lastChange time.Time
			var lost, held int
			for _, a := range arr {
				prevJAt := s.jitterAt
				natural := a.at.Add(p.Delay) // before the jitter offset
				at, ok := s.downlink(a.size, a.at)
				if s.jitterAt != prevJAt {
					offs = append(offs, float64(s.jitterOff)/float64(p.Jitter))
					if !lastChange.IsZero() {
						changes = append(changes, s.jitterAt.Sub(lastChange))
					}
					lastChange = s.jitterAt
				}
				if !ok {
					lost++
					continue
				}
				d := at.Sub(a.at)
				if d < p.Delay-p.Jitter || d > p.Delay+p.Jitter {
					t.Fatalf("delay %s outside %s ± %s", d, p.Delay, p.Jitter)
				}
				if at.Before(lastAt) {
					t.Fatalf("reordered: delivered at %s, previous at %s", at, lastAt)
				}
				if at.After(natural.Add(s.jitterOff)) || at.Before(natural.Add(s.jitterOff)) {
					held++ // held back behind the previous packet (no reordering)
				}
				lastAt = at
				all = append(all, d)
				if a.marker {
					markers = append(markers, d)
				}
				if a.audio {
					audio = append(audio, d)
				}
			}

			// Jitter offset: uniform over [-1, 1] in units of Jitter.
			var sum, sq, lo, hi float64 = 0, 0, 1, -1
			for _, o := range offs {
				sum += o
				sq += o * o
				lo, hi = math.Min(lo, o), math.Max(hi, o)
			}
			mean := sum / float64(len(offs))
			sd := math.Sqrt(sq/float64(len(offs)) - mean*mean)
			sortDur(changes)
			t.Logf("jitter offset: %d draws, mean %+.3f·J, sd %.3f·J (uniform: 0.577), range [%.3f, %.3f]·J; redraw interval min %s p50 %s",
				len(offs), mean, sd, lo, hi, changes[0], quantile(changes, 0.5))
			if math.Abs(mean) > 0.02 || math.Abs(sd-1/math.Sqrt(3)) > 0.01 || lo < -1 || hi > 1 || lo > -0.99 || hi < 0.99 {
				t.Errorf("jitter offset is not uniform over ±J")
			}
			if changes[0] < jitterPeriod {
				t.Errorf("jitter redrawn after %s, want >= %s", changes[0], jitterPeriod)
			}

			lossPct := 100 * float64(lost) / float64(len(arr))
			sortDur(all)
			sortDur(markers)
			sortDur(audio)
			uni := func(q float64) time.Duration { return p.Delay - p.Jitter + time.Duration(q*2*float64(p.Jitter)) }
			t.Logf("%d packets, lost %.3f %%, held back by no-reorder %.1f %%", len(arr), lossPct, 100*float64(held)/float64(len(all)))
			t.Logf("added delay, all:     p5 %s p50 %s p95 %s max %s   (plain uniform: p5 %s p50 %s p95 %s)",
				quantile(all, .05), quantile(all, .5), quantile(all, .95), all[len(all)-1], uni(.05), uni(.5), uni(.95))
			t.Logf("added delay, video frame (marker) packets: p50 %s p95 %s;%s",
				quantile(markers, .5), quantile(markers, .95), cdfAt(markers, .05, .075, .1, .15, .2))
			t.Logf("added delay, audio:   p50 %s p95 %s;%s",
				quantile(audio, .5), quantile(audio, .95), cdfAt(audio, .05, .075, .1, .15, .2))
			if name == "far" && lost != 0 {
				t.Errorf("far has no loss configured but lost %d", lost)
			}
		})
	}
}

// mobile: 1 % uniform random loss, single-packet losses.
func TestPlan61MobileLoss(t *testing.T) {
	s := NewShaper(plan61["mobile"])
	now := time.Now()
	var lost, runs int
	var inRun bool
	const n = 2_000_000
	for range n {
		now = now.Add(time.Millisecond)
		_, ok := s.downlink(1000, now)
		if !ok {
			lost++
			if !inRun {
				runs++
			}
		}
		inRun = !ok
	}
	got := float64(lost) / n
	t.Logf("mobile loss %.3f %% (want 1 %%), %.3f packets per loss run", 100*got, float64(lost)/float64(runs))
	if math.Abs(got-0.01) > 0.0005 {
		t.Errorf("loss %.4f, want 0.01 ± 0.0005", got)
	}
}

// burst: about 5 % loss in runs of about 50 ms whatever the packet pattern,
// and no added delay.
func TestPlan61BurstOnViewerTraffic(t *testing.T) {
	s := NewShaper(plan61["burst"])
	arr := viewerTraffic(time.Now(), 30*time.Minute, 730, rand.New(rand.NewPCG(3, 4)))
	var lost int
	for _, a := range arr {
		at, ok := s.downlink(a.size, a.at)
		if !ok {
			lost++
			continue
		}
		if !at.Equal(a.at) {
			t.Fatalf("burst profile added delay %s", at.Sub(a.at))
		}
	}
	got := float64(lost) / float64(len(arr))
	t.Logf("burst on viewer traffic: %d packets, loss %.2f %% (want 5 %%)", len(arr), 100*got)
	if math.Abs(got-0.05) > 0.006 {
		t.Errorf("loss %.4f, want 0.05 ± 0.006", got)
	}
}

// capped: 1000 kbps of UDP payload, a 300 ms queue, tail drop beyond it.
func TestPlan61Capped(t *testing.T) {
	const pkt = 1200
	serial := time.Duration(pkt * 8 * int64(time.Millisecond) / 1000) // 9.6 ms at 1000 kbps
	constant := func(kbps int, d time.Duration) []arrival {
		gap := time.Duration(int64(pkt*8) * int64(time.Millisecond) / int64(kbps))
		var out []arrival
		t0 := time.Now()
		for at := time.Duration(0); at < d; at += gap {
			out = append(out, arrival{at: t0.Add(at), size: pkt})
		}
		return out
	}
	type result struct {
		lostPct   float64
		kbps      float64 // delivered, by delivery time, after the first 10 s
		qd        []time.Duration
		markers   []time.Duration
		maxQueued time.Duration
	}
	run := func(arr []arrival) result {
		s := NewShaper(plan61["capped"])
		var r result
		var bytes int
		var lost int
		t0 := arr[0].at
		var first, last time.Time
		for _, a := range arr {
			at, ok := s.downlink(a.size, a.at)
			if !ok {
				lost++
				continue
			}
			d := at.Sub(a.at)
			r.qd = append(r.qd, d)
			if a.marker {
				r.markers = append(r.markers, d)
			}
			if at.Sub(t0) >= 10*time.Second {
				if first.IsZero() {
					first = at
				}
				last = at
				bytes += a.size
			}
		}
		r.lostPct = 100 * float64(lost) / float64(len(arr))
		r.kbps = float64(bytes*8) / last.Sub(first).Seconds() / 1000
		sortDur(r.qd)
		sortDur(r.markers)
		r.maxQueued = r.qd[len(r.qd)-1]
		return r
	}

	over := run(constant(2000, time.Minute))
	t.Logf("offered 2000 kbps: delivered %.1f kbps, lost %.1f %%, queue delay p50 %s max %s",
		over.kbps, over.lostPct, quantile(over.qd, .5), over.maxQueued)
	if math.Abs(over.kbps-1000) > 5 {
		t.Errorf("delivered %.1f kbps, want 1000 ± 5", over.kbps)
	}
	if math.Abs(over.lostPct-50) > 1 {
		t.Errorf("lost %.1f %%, want 50 %% ± 1", over.lostPct)
	}
	if over.maxQueued > maxQueue+serial {
		t.Errorf("queue delay %s, want <= %s", over.maxQueued, maxQueue+serial)
	}

	under := run(constant(800, time.Minute))
	t.Logf("offered 800 kbps: lost %.1f %%, delay min %s max %s (one packet's serialization = %s)",
		under.lostPct, under.qd[0], under.maxQueued, serial)
	if under.lostPct != 0 || under.qd[0] != serial || under.maxQueued != serial {
		t.Errorf("under the cap: want no loss and exactly %s delay", serial)
	}

	// What the live capped viewers got from mediasoup: about the low layer
	// on 6 tracks (~150 kbps) plus 6 audio tracks, bursty frames.
	for _, vk := range []int{110, 150} {
		v := run(viewerTraffic(time.Now(), 5*time.Minute, vk, rand.New(rand.NewPCG(5, 6))))
		t.Logf("viewer traffic %d kbps/track video + audio: delivered %.0f kbps, lost %.1f %%, queue delay p50 %s p95 %s max %s; video frame packets:%s",
			vk, v.kbps, v.lostPct, quantile(v.qd, .5), quantile(v.qd, .95), v.maxQueued, cdfAt(v.markers, .1, .2, .3, .5, .75))
		if v.kbps > 1000.5 || v.maxQueued > maxQueue+serial {
			t.Errorf("over the cap: %.1f kbps, max queue %s", v.kbps, v.maxQueued)
		}
	}
}

// dippy: the rate drops to 300 kbps for 15 s every minute, the first dip
// between 30 s and 60 s after the shaper starts; no limit in between.
func TestPlan61Dippy(t *testing.T) {
	p := plan61["dippy"]
	var minPh, maxPh, sumPh time.Duration = time.Hour, 0, 0
	const shapers = 2000
	for range shapers {
		_, ph := NewShaper(p).Clock()
		minPh, maxPh, sumPh = min(minPh, ph), max(maxPh, ph), sumPh+ph
	}
	t.Logf("first dip after: min %s max %s mean %s (want spread over [30s, 60s])", minPh, maxPh, sumPh/shapers)
	if minPh < 30*time.Second || maxPh > time.Minute || minPh > 31*time.Second || maxPh < 59*time.Second {
		t.Errorf("first-dip phase not spread over [30s, 60s]")
	}

	s := NewShaper(p)
	start, phase := s.Clock()
	inDip := func(at time.Time) bool {
		el := at.Sub(start) - phase
		return el >= 0 && el%time.Minute < 15*time.Second
	}
	dips := 0
	for at := start; at.Before(start.Add(10 * time.Minute)); at = at.Add(10 * time.Millisecond) {
		want := 0
		if inDip(at) {
			want = 300
		}
		if got := s.RateKbps(at); got != want {
			t.Fatalf("at %s: rate %d, want %d", at.Sub(start), got, want)
		}
		if a, n, _ := s.DipState(at); a {
			dips = max(dips, n)
		}
	}
	t.Logf("phase %s: %d dips in the first 10 min, RateKbps matches [phase+k·60s, +15s) at 10 ms steps", phase, dips)

	// Throughput: offer 1000 kbps for 5 min.
	const pkt = 1200
	gap := time.Duration(int64(pkt*8) * int64(time.Millisecond) / 1000)
	type win struct{ bytes, sent, lost int }
	var in, out win
	var outDelayMax time.Duration
	var lastAt time.Time
	for at := start; at.Before(start.Add(5 * time.Minute)); at = at.Add(gap) {
		dAt, ok := s.downlink(pkt, at)
		el := at.Sub(start) - phase
		edge := el >= 0 && (el%time.Minute < 500*time.Millisecond || // first 0.5 s of a dip
			(el%time.Minute >= 15*time.Second && el%time.Minute < 15500*time.Millisecond)) // queue drains after it
		if !ok {
			if inDip(at) {
				in.lost++
			} else {
				out.lost++
			}
			continue
		}
		if dAt.Before(lastAt) {
			t.Fatal("reordered")
		}
		lastAt = dAt
		if edge {
			continue
		}
		if inDip(at) {
			in.bytes += pkt
		} else {
			out.bytes += pkt
			outDelayMax = max(outDelayMax, dAt.Sub(at))
		}
	}
	_ = out
	// Delivered rate inside dips: count by delivery over whole dips instead.
	s2 := NewShaper(p)
	start2, phase2 := s2.Clock()
	var dipBytes, dipLost, dipSent int
	var dipSecs float64
	for k := 0; k < 4; k++ {
		ds := start2.Add(phase2 + time.Duration(k)*time.Minute)
		var first, last time.Time
		var b int
		for at := ds.Add(2 * time.Second); at.Before(ds.Add(15 * time.Second)); at = at.Add(gap) {
			dAt, ok := s2.downlink(pkt, at)
			dipSent++
			if !ok {
				dipLost++
				continue
			}
			if first.IsZero() {
				first = dAt
			}
			last = dAt
			b += pkt
		}
		dipBytes += b
		dipSecs += last.Sub(first).Seconds()
	}
	dipKbps := float64(dipBytes*8) / dipSecs / 1000
	t.Logf("offered 1000 kbps: inside dips delivered %.1f kbps, lost %.1f %%; outside dips lost %d, max added delay %s",
		dipKbps, 100*float64(dipLost)/float64(dipSent), out.lost, outDelayMax)
	if math.Abs(dipKbps-300) > 3 {
		t.Errorf("inside dips %.1f kbps, want 300 ± 3", dipKbps)
	}
	if out.lost != 0 || outDelayMax != 0 {
		t.Errorf("outside dips: %d lost, max delay %s, want none", out.lost, outDelayMax)
	}
}

// clean adds nothing: the socket is not even wrapped by the shaping reader.
func TestPlan61CleanIsPassthrough(t *testing.T) {
	p := plan61["clean"]
	if p.Shaped() {
		t.Fatal("clean profile reports shaped")
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if u := newUDPConn(c, NewShaper(p), true); u.shaped || u.ch != nil {
		t.Fatal("clean socket got the shaping reader")
	}
	for name, p := range plan61 {
		if name != "clean" && !p.Shaped() {
			t.Errorf("%s is not shaped", name)
		}
	}
}

// End to end through a real loopback socket, as Pion reads it: real timers,
// the reader goroutine, the 4096-packet channel. All profiles in parallel,
// 12 s each. dippy uses a short clock (dip 3 s every 6 s) to fit.
func TestPlan61Loopback(t *testing.T) {
	if testing.Short() {
		t.Skip("real time")
	}
	const dur = 12 * time.Second
	profiles := []string{"clean", "mobile", "far", "capped", "burst", "dippy"}
	for _, name := range profiles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := plan61[name]
			videoKbps := 300
			if name == "dippy" {
				p.Dip = Dip{Kbps: 300, For: 3 * time.Second, Every: 6 * time.Second}
			}
			if name == "capped" {
				videoKbps = 150 // ~1.14 Mbps offered with audio, like the live low-layer case
			}
			sh := NewShaper(p)
			n, err := NewNet(sh)
			if err != nil {
				t.Fatal(err)
			}
			rx, err := n.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer rx.Close()
			tx, err := net.DialUDP("udp4", nil, rx.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Close()

			t0 := time.Now().Add(50 * time.Millisecond)
			arr := viewerTraffic(t0, dur, videoKbps, rand.New(rand.NewPCG(7, uint64(len(name)))))
			type got struct {
				seq      uint32
				sent, at time.Time
				size     int
				marker   bool
			}
			recv := make(chan []got, 1)
			go func() {
				var out []got
				buf := make([]byte, 1600)
				for {
					_ = rx.SetReadDeadline(time.Now().Add(time.Second))
					k, _, err := rx.ReadFrom(buf)
					if err != nil {
						recv <- out
						return
					}
					now := time.Now()
					out = append(out, got{seq: binary.BigEndian.Uint32(buf), sent: time.Unix(0, int64(binary.BigEndian.Uint64(buf[4:]))),
						at: now, size: k, marker: buf[12] == 1})
				}
			}()
			msg := make([]byte, 1600)
			for i, a := range arr {
				if w := time.Until(a.at); w > 0 {
					time.Sleep(w)
				}
				binary.BigEndian.PutUint32(msg, uint32(i))
				binary.BigEndian.PutUint64(msg[4:], uint64(time.Now().UnixNano()))
				msg[12] = 0
				if a.marker {
					msg[12] = 1
				}
				if _, err := tx.Write(msg[:a.size]); err != nil {
					t.Fatal(err)
				}
			}
			rcv := <-recv

			var ds, mk []time.Duration
			reorders := 0
			for i, g := range rcv {
				if i > 0 && g.seq <= rcv[i-1].seq {
					reorders++
				}
				ds = append(ds, g.at.Sub(g.sent))
				if g.marker {
					mk = append(mk, g.at.Sub(g.sent))
				}
			}
			lossPct := 100 * (1 - float64(len(rcv))/float64(len(arr)))
			sortDur(ds)
			sortDur(mk)
			// Delivered rate over the last 8 s, by receive time.
			var bytes int
			from := t0.Add(dur - 8*time.Second)
			for _, g := range rcv {
				if !g.at.Before(from) && g.at.Before(t0.Add(dur)) {
					bytes += g.size
				}
			}
			kbps := float64(bytes*8) / 8 / 1000
			var offered int
			for _, a := range arr {
				offered += a.size
			}
			t.Logf("%-6s sent %d (%.0f kbps), got %d, loss %.2f %%, reorders %d, delivered %.0f kbps; delay p0 %s p5 %s p50 %s p95 %s max %s",
				name, len(arr), float64(offered*8)/dur.Seconds()/1000, len(rcv), lossPct, reorders, kbps,
				ds[0].Round(10*time.Microsecond), quantile(ds, .05).Round(10*time.Microsecond), quantile(ds, .5).Round(10*time.Microsecond),
				quantile(ds, .95).Round(10*time.Microsecond), ds[len(ds)-1].Round(10*time.Microsecond))
			if reorders != 0 {
				t.Errorf("%d reorders", reorders)
			}
			// ReadFrom hands a packet out up to 1 ms early; timers may fire late.
			lo, hi := p.Delay-p.Jitter-time.Millisecond, p.Delay+p.Jitter+5*time.Millisecond
			switch name {
			case "clean", "burst":
				hi = 2 * time.Millisecond
			case "capped":
				hi = maxQueue + 10*time.Millisecond + 5*time.Millisecond
			case "dippy": // a 1140-byte packet takes 30.4 ms at 300 kbps
				hi = maxQueue + 31*time.Millisecond + 5*time.Millisecond
			}
			if ds[0] < lo || quantile(ds, .999) > hi {
				t.Errorf("delay p0 %s / p99.9 %s outside [%s, %s]", ds[0], quantile(ds, .999), lo, hi)
			}
			switch name {
			case "clean", "far":
				if lossPct != 0 {
					t.Errorf("loss %.2f %%, want 0", lossPct)
				}
			case "mobile":
				if lossPct < 0.4 || lossPct > 1.8 {
					t.Errorf("loss %.2f %%, want ~1 %%", lossPct)
				}
			case "capped":
				if kbps > 1010 || kbps < 950 {
					t.Errorf("delivered %.0f kbps, want ~1000", kbps)
				}
			case "dippy":
				start, phase := sh.Clock()
				var in, out int
				var inS, outS float64
				for _, g := range rcv {
					el := g.sent.Sub(start) - phase
					if el < 0 {
						continue
					}
					switch m := el % p.Dip.Every; {
					case m >= 500*time.Millisecond && m < p.Dip.For:
						in += g.size
					case m >= p.Dip.For+500*time.Millisecond:
						out += g.size
					}
				}
				// Time covered by each class inside the run.
				for at := t0; at.Before(t0.Add(dur)); at = at.Add(time.Millisecond) {
					el := at.Sub(start) - phase
					if el < 0 {
						continue
					}
					switch m := el % p.Dip.Every; {
					case m >= 500*time.Millisecond && m < p.Dip.For:
						inS += 0.001
					case m >= p.Dip.For+500*time.Millisecond:
						outS += 0.001
					}
				}
				inK, outK := float64(in*8)/inS/1000, float64(out*8)/outS/1000
				t.Logf("dippy (short clock, phase %s): in dips %.0f kbps over %.1f s, between dips %.0f kbps over %.1f s",
					phase, inK, inS, outK, outS)
				if inS > 1 && (inK < 270 || inK > 330) {
					t.Errorf("in dips %.0f kbps, want ~300", inK)
				}
			}
			if name == "mobile" || name == "far" {
				t.Logf("%-6s video frame packets:%s", name, cdfAt(mk, .05, .075, .1, .15, .2))
			}
		})
	}
}
