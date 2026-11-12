// Package client implements one simulated participant for each SFU. Both
// adapters send the same bytes (media package) on the same schedule and feed
// received RTP into the same stats code, so the SFU is the only variable.
package client

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
	"sfubench/loadgen/internal/stats"
)

type Spec struct {
	SFU       string // "livekit" | "mediasoup"
	URL       string // livekit: ws(s)://host:7880   mediasoup: ws://host:8080/ws or wss://host/ws
	APIKey    string // livekit only
	APISecret string // livekit only
	Room      string
	Identity  string
	Publish   bool
	Quality   string // video layer to subscribe to: "low" | "medium" | "high"
	Audio     bool
	Video     bool
	Media     *media.Video
	Recv      stats.Options

	ScreenShare bool         // publishers also send a second video track ("screen")
	Red         bool         // livekit only: audio as audio/red (RFC 2198), sent and received
	Talk        *atomic.Bool // audio unmuted; nil = always talking
	Profile     string       // network profile name (metric label)
	Shaper      *netem.Shaper
	Mon         *stats.Monitor
	// OnResume (may be nil) is called when the SDK reconnected by itself after
	// a network problem without a new join (LiveKit resume / ICE restart).
	OnResume func()

	// mediasoup adapter only
	IncludeLoopback bool     // allow 127.0.0.1 candidates (single-host smoke tests)
	LocalIPs        []string // host ICE candidates to advertise
}

// Participant is one simulated user.
type Participant interface {
	// Join connects and, if Spec.Publish, starts publishing. It returns once the
	// participant is connected (publishers: once media is flowing out).
	// Subscriptions are handled in the background.
	Join(ctx context.Context) error
	// Done is closed if the participant drops without Close being called.
	Done() <-chan struct{}
	Close()
}

func New(spec Spec) Participant {
	switch spec.SFU {
	case "livekit":
		return newLiveKit(spec)
	case "mediasoup":
		return newMediasoup(spec)
	}
	panic("unknown sfu " + spec.SFU)
}

// ---- shared publishing loops ---------------------------------------------------

type writeFunc func(data []byte, dur time.Duration) error

// keyframeRequest is set by the RTCP reader on PLI/FIR and consumed by the layer loop.
type keyframeRequest struct{ flag atomic.Bool }

func (k *keyframeRequest) request() { k.flag.Store(true) }

// runVideoLayer sends one simulcast layer (index layer) at its frame rate until
// ctx is done. On PLI/FIR it jumps to the next keyframe in the file (at most
// every 300 ms), like a real encoder producing a keyframe on demand.
func runVideoLayer(ctx context.Context, l *media.Layer, layer int, write writeFunc, kf *keyframeRequest) {
	// Random phase so thousands of publishers don't send in lockstep.
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(l.FrameDur)))):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(l.FrameDur)
	defer t.Stop()
	bytesC := stats.M.PubBytes.WithLabelValues("video")
	framesC := stats.M.PubFrames.WithLabelValues("video")
	lagH := stats.M.SendLag.WithLabelValues("video")
	buf := make([]byte, 0, 64*1024)
	idx := 0
	var lastForced time.Time
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n%sendLagEvery == 0 {
				lagH.Observe(time.Since(now).Seconds())
			}
			if kf.flag.Load() && now.Sub(lastForced) > 300*time.Millisecond {
				kf.flag.Store(false)
				lastForced = now
				if !isKey(l, idx) {
					idx = l.NextKeyframe(idx)
				}
				stats.M.PubForcedKeyframes.Inc()
			}
			buf = append(buf[:0], l.Frames[idx]...)
			buf = media.AppendTrailerTag(buf, time.Now(), media.LayerTag(layer))
			if err := write(buf, l.FrameDur); err != nil {
				return
			}
			bytesC.Add(float64(len(buf)))
			framesC.Inc()
			idx = (idx + 1) % len(l.Frames)
		}
	}
}

// runSVC sends every layer of one picture on a single RTP stream. Spatial
// layers share the picture ID and timestamp; a PLI jumps all of them to the
// next aligned keyframe.
func runSVC(ctx context.Context, v *media.Video, write func(*rtp.Packet) error, kf *keyframeRequest) {
	base := v.Layers[0]
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(base.FrameDur)))):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(base.FrameDur)
	defer t.Stop()
	bytesC := stats.M.PubBytes.WithLabelValues("video")
	framesC := stats.M.PubFrames.WithLabelValues("video")
	lagH := stats.M.SendLag.WithLabelValues("video")
	pay := media.NewVP9SVC(v.Layers)
	ts := rand.Uint32()
	step := uint32(base.FrameDur.Seconds() * 90000)
	if step == 0 {
		step = 3000
	}
	idx := 0
	var lastForced time.Time
	buf := make([]byte, 0, 64*1024)
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n%sendLagEvery == 0 {
				lagH.Observe(time.Since(now).Seconds())
			}
			if kf.flag.Load() && now.Sub(lastForced) > 300*time.Millisecond {
				kf.flag.Store(false)
				lastForced = now
				if !v.AllKey(idx) {
					idx = v.NextKeyframe(idx)
				}
				stats.M.PubForcedKeyframes.Inc()
			}
			// L3T3_KEY: temporal layer from the picture index (0,2,1,2); a key
			// picture is always TID 0, as libvpx does.
			tid := media.TemporalID(idx)
			if v.AllKey(idx) {
				tid = 0
			}
			pay.BeginPicture(tid)
			for sid, l := range v.Layers {
				buf = append(buf[:0], l.Frames[idx]...)
				buf = media.AppendTrailerTag(buf, time.Now(), media.LayerTag(sid))
				for _, p := range pay.Packetize(buf, sid, isKey(l, idx), ts) {
					if err := write(p); err != nil {
						return
					}
				}
				bytesC.Add(float64(len(buf)))
				framesC.Inc()
			}
			ts += step
			idx = (idx + 1) % len(base.Frames)
		}
	}
}

func isKey(l *media.Layer, idx int) bool {
	for _, k := range l.Keyframes {
		if k == idx {
			return true
		}
	}
	return false
}

// sendLagEvery: publishers sample bench_gen_send_lag_seconds on 1 of this many ticks.
const sendLagEvery = 10

// mutedEvery: a muted speaker sends one DTX packet this often, like Opus DTX.
const mutedEvery = 400 * time.Millisecond

// runAudio sends a 20 ms Opus packet every 20 ms. With talk set and false the
// speaker is muted: only a tiny DTX packet goes out every 400 ms. While
// unmuted every packet carries the time of the unmute (media.AppendTalkStart).
func runAudio(ctx context.Context, write writeFunc, talk *atomic.Bool) {
	select {
	case <-time.After(time.Duration(rand.Int64N(int64(media.OpusFrameDur)))):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(media.OpusFrameDur)
	defer t.Stop()
	bytesC := stats.M.PubBytes.WithLabelValues("audio")
	framesC := stats.M.PubFrames.WithLabelValues("audio")
	lagH := stats.M.SendLag.WithLabelValues("audio")
	buf := make([]byte, 0, 256)
	var (
		talking   = true
		talkStart time.Time
		skipped   time.Duration // silence since the last packet sent
	)
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n%sendLagEvery == 0 {
				lagH.Observe(time.Since(now).Seconds())
			}
			dur := media.OpusFrameDur + skipped
			if talk != nil {
				on := talk.Load()
				if on && !talking {
					talkStart = now
				}
				talking = on
			}
			switch {
			case talk == nil:
				buf = append(buf[:0], media.OpusFrame...)
			case talking:
				buf = append(buf[:0], media.OpusFrame...)
				buf = media.AppendTalkStart(buf, talkStart)
			case dur < mutedEvery:
				skipped = dur
				continue
			default:
				buf = append(buf[:0], media.OpusDTX...)
			}
			skipped = 0
			buf = media.AppendTrailer(buf, time.Now())
			if err := write(buf, dur); err != nil {
				return
			}
			bytesC.Add(float64(len(buf)))
			framesC.Inc()
		}
	}
}

// trackInfo describes a received track for stats.ConsumeTrack. source is the
// publisher's track name, "" if the SFU does not tell.
func (s *Spec) trackInfo(kind string, clockRate uint32, publisher, source string) stats.TrackInfo {
	ti := stats.TrackInfo{Kind: kind, Quality: s.Quality, Profile: profileOf(*s), ClockRate: clockRate,
		Publisher: publisher, Source: source, Shaper: s.Shaper, Mon: s.Mon}
	if kind == "video" {
		ti.Video = s.Media
	}
	return ti
}

func profileOf(s Spec) string {
	if s.Profile == "" {
		return "clean"
	}
	return s.Profile
}

// readTrack adapts a remote track for stats.ConsumeTrack.
func readTrack(t *webrtc.TrackRemote) stats.ReadFunc {
	return func(b []byte) (int, bool, error) {
		n, a, err := t.Read(b)
		return n, a.Get(webrtc.AttributeRtxSsrc) != nil, err
	}
}

// doneOnce is a Done() channel that is closed at most once.
type doneOnce struct {
	once sync.Once
	ch   chan struct{}
}

func newDoneOnce() doneOnce { return doneOnce{ch: make(chan struct{})} }
func (d *doneOnce) close()  { d.once.Do(func() { close(d.ch) }) }
