package stats

import (
	"time"

	"github.com/pion/rtp"
	"github.com/prometheus/client_golang/prometheus"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
)

// layerTracker reads the layer tag of every received video frame (see
// media.LayerTag) and checks how the SFU reacts to the subscriber's bandwidth
// dips: how long until it forwards a lower layer after a dip starts (down),
// and until the layer it forwarded when the dip started is back after the dip
// ends (up). That layer, not the requested one, because on a capped link (or
// one shared by several video tracks) the requested layer may never fit.
//
// A frame is one picture (RTP timestamp): a VP9 SVC picture carries one
// layer tag per spatial layer and counts as its highest.
type layerTracker struct {
	v   *media.Video
	req int
	sh  *netem.Shaper
	dip netem.Dip

	curTS    uint32
	haveTS   bool
	curLayer int       // highest layer tag seen for curTS, -1 = none
	curAt    time.Time // latest packet of curTS
	firstAt  time.Time

	pendReq, pendLow, pendHigh uint64
	cReq, cLow, cHigh          prometheus.Counter

	seenDip     int
	inDip       bool
	dipStart    time.Time
	before      int // layer when the dip started (at most req): the up switch waits for it
	waitingDown bool
	downFrom    int
	waitingUp   bool
	upFrom      time.Time
}

func newLayerTracker(v *media.Video, quality, profile string, sh *netem.Shaper) *layerTracker {
	lt := &layerTracker{
		v: v, req: v.LayerFor(quality), sh: sh, curLayer: -1,
		cReq:  M.SubLayerFrames.WithLabelValues(profile, "requested"),
		cLow:  M.SubLayerFrames.WithLabelValues(profile, "lower"),
		cHigh: M.SubLayerFrames.WithLabelValues(profile, "higher"),
	}
	if sh != nil {
		lt.dip = sh.Profile().Dip
	}
	return lt
}

func (lt *layerTracker) packet(now time.Time, pkt *rtp.Packet) {
	if lt == nil {
		return
	}
	if !lt.haveTS || pkt.Timestamp != lt.curTS {
		// A late packet of an earlier picture (within 1 s at 90 kHz; a bigger
		// jump back is a new stream).
		if d := int32(pkt.Timestamp - lt.curTS); lt.haveTS && d < 0 && d > -90000 {
			return
		}
		// The next picture began: the previous one is complete.
		if lt.curLayer >= 0 {
			lt.frame(lt.curAt, lt.curLayer)
		}
		lt.curTS, lt.curLayer, lt.haveTS = pkt.Timestamp, -1, true
	}
	lt.curAt = now
	if _, tag, ok := media.ReadTrailerTag(pkt.Payload); ok {
		if l := media.TagLayer(tag); l > lt.curLayer {
			lt.curLayer = l
		}
	}
}

func (lt *layerTracker) frame(now time.Time, l int) {
	switch {
	case l == lt.req:
		lt.pendReq++
	case l < lt.req:
		lt.pendLow++
	default:
		lt.pendHigh++
	}
	if lt.firstAt.IsZero() {
		lt.firstAt = now
	}
	if lt.dip.Every <= 0 {
		return
	}
	active, n, started := lt.sh.DipState(now)
	if lt.inDip && (!active || n != lt.seenDip) { // the dip we were in is over
		lt.inDip = false
		if lt.waitingDown {
			M.LayerSwitchMiss.WithLabelValues("down").Inc()
			lt.waitingDown = false
		}
		if l < lt.before {
			lt.waitingUp, lt.upFrom = true, lt.dipStart.Add(lt.dip.For)
		}
	}
	if active && n != lt.seenDip { // a new dip
		lt.seenDip = n
		if lt.waitingUp {
			M.LayerSwitchMiss.WithLabelValues("up").Inc()
			lt.waitingUp = false
		}
		if started.After(lt.firstAt) { // skip a dip that began before this track
			// The SFU has not reacted yet: l is still the layer from before the dip.
			lt.inDip, lt.dipStart, lt.before = true, started, min(l, lt.req)
			if l > 0 && lt.v.LayerKbps(l) > float64(lt.dip.Kbps) {
				lt.waitingDown, lt.downFrom = true, l
			}
		}
	}
	if lt.waitingDown && l < lt.downFrom {
		M.LayerSwitch.WithLabelValues("down").Observe(now.Sub(lt.dipStart).Seconds())
		lt.waitingDown = false
	}
	if lt.waitingUp {
		if l >= lt.before {
			M.LayerSwitch.WithLabelValues("up").Observe(now.Sub(lt.upFrom).Seconds())
			lt.waitingUp = false
		} else if now.Sub(lt.upFrom) > RecoveryTimeout {
			M.LayerSwitchMiss.WithLabelValues("up").Inc()
			lt.waitingUp = false
		}
	}
}

func (lt *layerTracker) flush() {
	if lt == nil {
		return
	}
	lt.cReq.Add(float64(lt.pendReq))
	lt.cLow.Add(float64(lt.pendLow))
	lt.cHigh.Add(float64(lt.pendHigh))
	lt.pendReq, lt.pendLow, lt.pendHigh = 0, 0, 0
}
