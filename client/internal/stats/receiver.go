package stats

import (
	"math/rand/v2"
	"time"

	"github.com/pion/rtp"
	"github.com/prometheus/client_golang/prometheus"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
)

// Options controls receiver-side sampling (to bound Prometheus overhead at
// tens of thousands of tracks).
type Options struct {
	FreezeThreshold  time.Duration // default 200ms (~ Chrome's max(3*frameDur, frameDur+150ms) at 30fps)
	AudioDelayEveryN int           // observe e2e delay on 1 of N audio packets (default 5 = 10/s)
	VideoDelayEveryN int           // observe e2e delay on 1 of N video frames (default 3 = 10/s)
}

func (o *Options) defaults() {
	if o.FreezeThreshold == 0 {
		o.FreezeThreshold = 200 * time.Millisecond
	}
	if o.AudioDelayEveryN == 0 {
		o.AudioDelayEveryN = 5
	}
	if o.VideoDelayEveryN == 0 {
		o.VideoDelayEveryN = 3
	}
}

// ReadFunc adapts TrackRemote.Read (which returns interceptor.Attributes) to a
// plain func. rtx reports that the packet came from the RTX stream (Pion
// unwraps it into the original sequence number before returning it).
type ReadFunc func(b []byte) (n int, rtx bool, err error)

// TrackInfo describes one received track.
type TrackInfo struct {
	Kind      string // audio | video
	Quality   string // layer the subscriber requested (video)
	Profile   string // network profile of the subscriber ("clean" if empty)
	ClockRate uint32
	Publisher string // identity of the publisher (publisher-reconnect matching)
	Source    string // publisher's track name (cam | screen | mic); empty if the SFU does not say

	Video        *media.Video  // layer set of the room (video; nil = no layer tracking)
	Shaper       *netem.Shaper // subscriber's network (bandwidth dips drive the layer-switch check)
	Mon          *Monitor      // may be nil
	OnFirstFrame func()        // called once for the first complete video frame (may be nil)
}

// ConsumeTrack reads RTP until error/EOF, keeping per-track stats and flushing
// them to Prometheus once per second. It blocks; run it in its own goroutine.
func ConsumeTrack(ti TrackInfo, read ReadFunc, opts Options) {
	opts.defaults()
	kind, quality, clockRate := ti.Kind, ti.Quality, ti.ClockRate
	prof := ti.Profile
	if prof == "" {
		prof = "clean"
	}
	isVideo := kind == "video"
	M.SubTracks.WithLabelValues(kind, prof).Inc()
	defer M.SubTracks.WithLabelValues(kind, prof).Dec()
	var cQualityBytes prometheus.Counter
	var lt *layerTracker
	codec := ""
	if isVideo {
		M.SubQualityTracks.WithLabelValues(quality, prof).Inc()
		defer M.SubQualityTracks.WithLabelValues(quality, prof).Dec()
		cQualityBytes = M.SubQualityBytes.WithLabelValues(quality, prof)
		if ti.Video != nil {
			codec = ti.Video.Codec
			lt = newLayerTracker(ti.Video, quality, prof, ti.Shaper)
		}
	}

	cPackets := M.SubPackets.WithLabelValues(kind, prof)
	cBytes := M.SubBytes.WithLabelValues(kind)
	cLost := M.SubLost.WithLabelValues(kind, prof)
	cOOO := M.SubOutOfOrder.WithLabelValues(kind)
	cRTX := M.SubRTX.WithLabelValues(kind)
	hDelay := M.SubDelay.WithLabelValues(kind, prof)
	hJitter := M.SubJitter.WithLabelValues(kind, prof)
	cFrames := M.SubFrames.WithLabelValues(prof)
	cFreezes := M.SubFreezes.WithLabelValues(prof)
	cFreezeSec := M.SubFreezeSeconds.WithLabelValues(prof)
	hFreezeLen := M.FreezeLength.WithLabelValues(prof)
	hTrackLoss := M.TrackLoss.WithLabelValues(kind, prof)
	var hTrackFPS, hKbpsRatio prometheus.Observer
	var wantKbps float64
	if isVideo {
		hTrackFPS = M.TrackFPS.WithLabelValues(prof)
		if ti.Video != nil && quality != "" {
			wantKbps = ti.Video.ExpectedKbps(quality)
			hKbpsRatio = M.TrackKbpsRatio.WithLabelValues(prof, quality)
		}
	}

	mon := ti.Mon
	session := mon.Session()
	var entry *trackEntry
	defer func() {
		if entry != nil {
			entry.ended.Store(true)
		}
	}()

	var (
		buf = make([]byte, 1600)
		pkt rtp.Packet

		started             bool
		baseSeq, maxSeqExt  uint64 // extended sequence numbers
		cycles              uint64
		lastSeq             uint16
		received            uint64
		lostReported        uint64
		pendPackets, pendBy uint64
		pendOOO, pendRTX    uint64
		pendFrames          uint64

		// RFC3550 jitter
		jitter      float64
		lastTransit float64
		haveTransit bool

		lastFrameAt  time.Time
		lastFrameTS  uint32
		frameCounter int
		pktCounter   int
		gotFrame     bool

		lastTalk int64 // unix nanos of the latest talk-start seen

		lastFlush = time.Now()

		// current TrackWindow (bench_sub_track_*)
		winStart            time.Time
		winMaxSeq, winRecv  uint64
		winFrames, winBytes uint64
	)
	// Spread first flush so all tracks don't flush in the same millisecond.
	lastFlush = lastFlush.Add(-time.Duration(rand.IntN(1000)) * time.Millisecond)

	// observeWindow records the finished TrackWindow of this track. It starts
	// at the first packet, so a track that never receives anything adds nothing.
	observeWindow := func(now time.Time) {
		secs := now.Sub(winStart).Seconds()
		if exp := maxSeqExt - winMaxSeq; exp > 0 {
			got := received - winRecv
			loss := 0.0
			if got < exp {
				loss = float64(exp-got) / float64(exp)
			}
			hTrackLoss.Observe(loss)
		}
		if hTrackFPS != nil {
			hTrackFPS.Observe(float64(winFrames) / secs)
		}
		if hKbpsRatio != nil && wantKbps > 0 {
			hKbpsRatio.Observe(float64(winBytes) * 8 / 1000 / secs / wantKbps)
		}
		winStart, winMaxSeq, winRecv, winFrames, winBytes = now, maxSeqExt, received, 0, 0
	}

	flush := func(now time.Time) {
		cPackets.Add(float64(pendPackets))
		cBytes.Add(float64(pendBy))
		if cQualityBytes != nil {
			cQualityBytes.Add(float64(pendBy))
		}
		cOOO.Add(float64(pendOOO))
		cRTX.Add(float64(pendRTX))
		cFrames.Add(float64(pendFrames))
		pendPackets, pendBy, pendOOO, pendRTX, pendFrames = 0, 0, 0, 0, 0
		if started {
			expected := maxSeqExt - baseSeq + 1
			if expected > received {
				lost := expected - received
				if lost > lostReported {
					cLost.Add(float64(lost - lostReported))
					lostReported = lost
				}
			}
		}
		if haveTransit {
			hJitter.Observe(jitter / float64(clockRate))
		}
		lt.flush()
		lastFlush = now
		if started && now.Sub(winStart) >= TrackWindow {
			observeWindow(now)
		}
	}

	for {
		n, rtx, err := read(buf)
		if err != nil { // track ended / PeerConnection closed
			flush(time.Now())
			return
		}
		now := time.Now()
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		pendPackets++
		pendBy += uint64(len(pkt.Payload))
		winBytes += uint64(len(pkt.Payload))
		received++
		if rtx {
			pendRTX++
		}
		if entry == nil {
			entry = mon.track(ti.Publisher, kind, ti.Source, now)
		} else {
			entry.last.Store(now.UnixNano())
		}
		if mon.Pending() {
			mon.media(now, kind, isVideo && media.KeyframeStart(codec, pkt.Payload), session)
		}

		// Extended sequence number (handles wrap). lastSeq is the highest seen.
		seq := pkt.SequenceNumber
		if !started {
			started = true
			baseSeq = uint64(seq)
			maxSeqExt = uint64(seq)
			lastSeq = seq
			// The window counts from the packet before this one, so this
			// packet is expected and received in it.
			winStart, winMaxSeq, winRecv = now, maxSeqExt-1, 0
		} else {
			ext := cycles + uint64(seq)
			switch {
			case seq < lastSeq && lastSeq-seq > 0x8000: // wrapped forward
				cycles += 1 << 16
				ext += 1 << 16
			case seq > lastSeq && seq-lastSeq > 0x8000 && ext >= 1<<16: // late, from before the wrap
				ext -= 1 << 16
			}
			if ext > maxSeqExt {
				maxSeqExt = ext
				lastSeq = seq
			} else if ext < maxSeqExt && !rtx {
				pendOOO++
			}
		}

		// Jitter (RFC3550 6.4.1), in RTP timestamp units. Retransmissions
		// arrive late by design and would inflate it, so they are skipped.
		if !rtx {
			arrival := float64(now.UnixNano()) * float64(clockRate) / 1e9
			transit := arrival - float64(pkt.Timestamp)
			if haveTransit {
				d := transit - lastTransit
				if d < 0 {
					d = -d
				}
				// Ignore huge jumps (simulcast layer switch rewrites timestamps).
				if d < float64(clockRate) {
					jitter += (d - jitter) / 16
				}
			}
			lastTransit, haveTransit = transit, true
		}

		if isVideo {
			lt.packet(now, &pkt)
			// Last packet of a frame. A VP9 SVC picture has one per spatial
			// layer (same timestamp): count the picture once.
			if pkt.Marker && (!gotFrame || pkt.Timestamp != lastFrameTS) {
				lastFrameTS = pkt.Timestamp
				frameCounter++
				pendFrames++
				winFrames++
				if !gotFrame {
					gotFrame = true
					if ti.OnFirstFrame != nil {
						ti.OnFirstFrame()
					}
				} else if gap := now.Sub(lastFrameAt); gap > opts.FreezeThreshold {
					cFreezes.Inc()
					cFreezeSec.Add(gap.Seconds())
					hFreezeLen.Observe(gap.Seconds())
				}
				lastFrameAt = now
				// Retransmissions (e.g. after an outage) carry old send times.
				if frameCounter%opts.VideoDelayEveryN == 0 && !rtx {
					if sent, ok := media.ReadTrailer(pkt.Payload); ok {
						hDelay.Observe(now.Sub(sent).Seconds())
					}
				}
			}
		} else {
			pktCounter++
			sent, ok := media.ReadTrailer(pkt.Payload)
			if ok && pktCounter%opts.AudioDelayEveryN == 0 && !rtx {
				hDelay.Observe(now.Sub(sent).Seconds())
			}
			if ok {
				// First packet of a talk spurt that was sent right after the
				// unmute: how long until this subscriber heard it.
				if ts, has := media.ReadTalkStart(pkt.Payload); has && ts.UnixNano() != lastTalk {
					lastTalk = ts.UnixNano()
					if sent.Sub(ts) < 200*time.Millisecond {
						M.UnmuteHeard.Observe(now.Sub(ts).Seconds())
					}
				}
			}
		}

		if now.Sub(lastFlush) >= time.Second {
			flush(now)
		}
	}
}
