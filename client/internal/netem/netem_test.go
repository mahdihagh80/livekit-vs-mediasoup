package netem

import (
	"math"
	"net"
	"testing"
	"time"
)

// The burst model loses about Burst.Loss of the packets, in runs that last
// about Burst.Length, and nothing outside them.
func TestBurstLoss(t *testing.T) {
	s := NewShaper(Profile{Burst: Burst{Loss: 0.05, Length: 50 * time.Millisecond}})
	if !s.Profile().Shaped() {
		t.Fatal("burst profile must be shaped")
	}
	const step = time.Millisecond // 1000 packets/s
	now := time.Now()
	var lost, total, runs int
	var inRun bool
	for i := 0; i < 2_000_000; i++ { // ~33 min of link time
		now = now.Add(step)
		_, ok := s.downlink(100, now)
		total++
		if !ok {
			lost++
			if !inRun {
				runs++
			}
		}
		inRun = !ok
	}
	avg := float64(lost) / float64(total)
	meanRun := time.Duration(lost/runs) * step
	t.Logf("loss %.3f %%, %d bursts, mean burst %s", 100*avg, runs, meanRun)
	if math.Abs(avg-0.05) > 0.005 {
		t.Errorf("average loss %.4f, want 0.05 ± 0.005", avg)
	}
	if meanRun < 44*time.Millisecond || meanRun > 56*time.Millisecond {
		t.Errorf("mean burst %s, want 50 ms ± 6 ms", meanRun)
	}
}

// Uniform loss alone gives single-packet losses, not runs.
func TestUniformLossIsNotBursty(t *testing.T) {
	s := NewShaper(Profile{Loss: 0.05})
	now := time.Now()
	var lost, runs int
	var inRun bool
	for i := 0; i < 200_000; i++ {
		now = now.Add(time.Millisecond)
		_, ok := s.downlink(100, now)
		if !ok {
			lost++
			if !inRun {
				runs++
			}
		}
		inRun = !ok
	}
	if r := float64(lost) / float64(runs); r > 1.2 {
		t.Fatalf("uniform loss: %.2f packets per run, want about 1.05", r)
	}
}

// A long gap without packets restarts the chain instead of looping through it.
func TestBurstGap(t *testing.T) {
	s := NewShaper(Profile{Burst: Burst{Loss: 0.5, Length: time.Millisecond}})
	now := time.Now()
	s.downlink(100, now)
	start := time.Now()
	s.downlink(100, now.Add(1000*time.Hour))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("gap took too long")
	}
}

// The profile shapes the subscribe connection (NewNet) only; the publish
// connection (NewOutageNet) passes everything except outages.
func TestOutageNetIsClean(t *testing.T) {
	sh := NewShaper(Profile{Name: "all-lost", Loss: 1})
	recv := func(newNet func(*Shaper) (*Net, error)) int {
		n, err := newNet(sh)
		if err != nil {
			t.Fatal(err)
		}
		c, err := n.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		tx, err := net.DialUDP("udp4", nil, c.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Close()
		for range 20 {
			if _, err := tx.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		got := 0
		buf := make([]byte, 16)
		for {
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, _, err := c.ReadFrom(buf); err != nil {
				return got
			}
			got++
		}
	}
	if got := recv(NewNet); got != 0 {
		t.Errorf("shaped net with 100%% loss delivered %d of 20", got)
	}
	if got := recv(NewOutageNet); got != 20 {
		t.Errorf("outage-only net delivered %d of 20, want all", got)
	}
	sh.Outage(time.Second)
	if got := recv(NewOutageNet); got != 0 {
		t.Errorf("outage-only net delivered %d of 20 during an outage", got)
	}
}
