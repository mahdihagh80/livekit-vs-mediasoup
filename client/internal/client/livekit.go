package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/livekit/protocol/livekit"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"sfubench/loadgen/internal/media"
	"sfubench/loadgen/internal/netem"
	"sfubench/loadgen/internal/stats"
)

// liveKit uses the official Go SDK (server-sdk-go), the same stack as
// `lk load-test`, with our media and stats code on top.
type liveKit struct {
	spec      Spec
	room      *lksdk.Room
	ctx       context.Context
	cancel    context.CancelFunc
	done      doneOnce
	closing   atomic.Bool
	joinStart time.Time
	ttffOnce  sync.Once

	nack      *nackGenSet
	srtp      srtpSeen
	srtpRoom  atomic.Pointer[lksdk.Room]
	trackOnce sync.Once
}

func newLiveKit(spec Spec) *liveKit {
	ctx, cancel := context.WithCancel(context.Background())
	return &liveKit{spec: spec, ctx: ctx, cancel: cancel, done: newDoneOnce()}
}

func (c *liveKit) Done() <-chan struct{} { return c.done.ch }

func (c *liveKit) firstFrame() {
	c.ttffOnce.Do(func() { stats.M.TTFF.WithLabelValues(profileOf(c.spec)).Observe(time.Since(c.joinStart).Seconds()) })
}

// connectOptions needs c.nack.
func (c *liveKit) connectOptions() ([]lksdk.ConnectOption, error) {
	opts := []lksdk.ConnectOption{lksdk.WithAutoSubscribe(true), lksdk.WithConnectTimeout(20 * time.Second),
		// Ours goes first in the chain, ahead of the SDK's defaults (see NewRTCPInterceptorFactory).
		lksdk.WithInterceptors([]interceptor.Factory{stats.NewRTCPInterceptorFactory()}),
		lksdk.WithIncludeDefaultInterceptors(true),
		lksdk.WithNackGenerator(c.nack)}
	if c.spec.Red {
		opts = append(opts, lksdk.WithRED())
	}
	if sh := c.spec.Shaper; sh != nil {
		// The profile shapes the subscriber PC only; the publisher PC sees outages only.
		sub, err := netem.NewNet(sh)
		if err != nil {
			return nil, err
		}
		pub, err := netem.NewOutageNet(sh)
		if err != nil {
			return nil, err
		}
		opts = append(opts,
			lksdk.WithSettingEngine(func(se *webrtc.SettingEngine, publisher bool) {
				if publisher {
					se.SetNet(pub)
				} else {
					se.SetNet(sub)
				}
			}),
			lksdk.WithSignalNetDial(sh.DialContext))
	}
	return opts, nil
}

// recordSRTP counts the cipher of each connected PeerConnection once and
// reports whether all the ones this participant uses are counted.
func (c *liveKit) recordSRTP() bool {
	r := c.srtpRoom.Load()
	if r == nil {
		return false
	}
	pub, sub := r.PeerConnections()
	pubDone := c.srtp.record("livekit", pub) || !c.spec.Publish
	subDone := c.srtp.record("livekit", sub) || sub == nil
	return pubDone && subDone
}

// sampleSRTP retries recordSRTP until it succeeds or the client stops: the
// publisher PC connects only once a track is published.
func (c *liveKit) sampleSRTP() {
	retryUntil(c.ctx, time.Second, c.recordSRTP)
}

func (c *liveKit) Join(ctx context.Context) error {
	c.joinStart = time.Now()
	cb := lksdk.NewRoomCallback()
	cb.OnDisconnected = func() {
		if !c.closing.Load() {
			c.done.close()
		}
	}
	cb.OnReconnected = func() {
		if c.spec.OnResume != nil && !c.closing.Load() {
			c.spec.OnResume()
		}
	}
	cb.ParticipantCallback.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		kind := track.Kind().String()
		ti := c.spec.trackInfo(kind, track.Codec().ClockRate, rp.Identity(), pub.Name())
		if kind == "video" {
			ti.OnFirstFrame = c.firstFrame
			// Like a client's adaptive stream: ask for the layer that fits the tile.
			// mediasoup gets the same layer as the consumer's preferredLayers.
			if err := pub.SetVideoQuality(c.videoQuality()); err != nil {
				log.Printf("%s: set video quality: %v", c.spec.Identity, err)
			}
		}
		go stats.ConsumeTrack(ti, readRepaired(track, c.nack, stats.M.SubRTXDup.WithLabelValues(profileOf(c.spec))), c.spec.Recv)
		c.trackOnce.Do(func() { go c.recordSRTP() })
	}

	c.nack = &nackGenSet{}
	opts, err := c.connectOptions()
	if err != nil {
		return err
	}

	type res struct {
		room *lksdk.Room
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		room, err := lksdk.ConnectToRoom(c.spec.URL, lksdk.ConnectInfo{
			APIKey:              c.spec.APIKey,
			APISecret:           c.spec.APISecret,
			RoomName:            c.spec.Room,
			ParticipantIdentity: c.spec.Identity,
		}, cb, opts...)
		ch <- res{room, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.room != nil {
				r.room.Disconnect()
			}
		}()
		stats.M.JoinFailures.WithLabelValues("signal").Inc()
		return ctx.Err()
	}
	if r.err != nil {
		stats.M.JoinFailures.WithLabelValues("signal").Inc()
		return fmt.Errorf("connect: %w", r.err)
	}
	c.room = r.room
	c.srtpRoom.Store(r.room)
	defer func() { go c.sampleSRTP() }()

	if !c.spec.Publish {
		return nil
	}
	if c.spec.Audio {
		if err := c.publishAudio(); err != nil {
			stats.M.JoinFailures.WithLabelValues("publish").Inc()
			return err
		}
	}
	if c.spec.Video {
		if err := c.publishVideo("cam", livekit.TrackSource_CAMERA); err != nil {
			stats.M.JoinFailures.WithLabelValues("publish").Inc()
			return err
		}
		if c.spec.ScreenShare {
			if err := c.publishVideo("screen", livekit.TrackSource_SCREEN_SHARE); err != nil {
				stats.M.JoinFailures.WithLabelValues("publish").Inc()
				return err
			}
		}
	}
	return nil
}

// videoQuality is the LiveKit quality of the layer this subscriber wants. The
// publisher announces its layers as qualities[i+3-n] (top layer = HIGH), so
// layer index -> quality uses the same offset.
func (c *liveKit) videoQuality() livekit.VideoQuality {
	qualities := []livekit.VideoQuality{livekit.VideoQuality_LOW, livekit.VideoQuality_MEDIUM, livekit.VideoQuality_HIGH}
	v := c.spec.Media
	return qualities[v.LayerFor(c.spec.Quality)+3-len(v.Layers)]
}

func (c *liveKit) publishAudio() error {
	codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	if c.spec.Red {
		codec.MimeType, codec.SDPFmtpLine = "audio/red", "111/111"
	}
	track, err := lksdk.NewLocalTrack(codec, lksdk.WithRTCPHandler(func(p rtcp.Packet) { handleRTCP(p, nil) }))
	if err != nil {
		return err
	}
	if _, err := c.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "mic", Source: livekit.TrackSource_MICROPHONE, DisableDTX: true,
	}); err != nil {
		return fmt.Errorf("publish audio: %w", err)
	}
	write := func(data []byte, dur time.Duration) error {
		return track.WriteSample(pionmedia.Sample{Data: data, Duration: dur}, nil)
	}
	if c.spec.Red {
		red, plain := &media.REDEncoder{ClockRate: 48000}, write
		write = func(data []byte, dur time.Duration) error { return plain(red.Encode(data, dur), dur) }
	}
	go runAudio(c.ctx, write, c.spec.Talk)
	return nil
}

func (c *liveKit) publishVideo(name string, source livekit.TrackSource) error {
	if c.spec.Media.SVC() {
		return c.publishVP9SVC(name, source)
	}
	qualities := []livekit.VideoQuality{livekit.VideoQuality_LOW, livekit.VideoQuality_MEDIUM, livekit.VideoQuality_HIGH}
	var tracks []*lksdk.LocalTrack
	var kfs []*keyframeRequest
	for i, l := range c.spec.Media.Layers {
		kf := &keyframeRequest{}
		t, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
			lksdk.WithSimulcast(name, &livekit.VideoLayer{
				// fewer than 3 layers: the top layer is always HIGH
				Quality: qualities[i+3-len(c.spec.Media.Layers)], Width: uint32(l.Width), Height: uint32(l.Height), Bitrate: uint32(l.BitrateBps),
			}),
			lksdk.WithRTCPHandler(func(p rtcp.Packet) { handleRTCP(p, kf) }),
		)
		if err != nil {
			return err
		}
		tracks = append(tracks, t)
		kfs = append(kfs, kf)
	}
	top := c.spec.Media.Layers[len(c.spec.Media.Layers)-1]
	if _, err := c.room.LocalParticipant.PublishSimulcastTrack(tracks, &lksdk.TrackPublicationOptions{
		Name: name, Source: source, VideoWidth: top.Width, VideoHeight: top.Height,
	}); err != nil {
		return fmt.Errorf("publish video: %w", err)
	}
	for i, l := range c.spec.Media.Layers {
		t := tracks[i]
		go runVideoLayer(c.ctx, l, i, func(data []byte, dur time.Duration) error {
			return t.WriteSample(pionmedia.Sample{Data: data, Duration: dur}, nil)
		}, kfs[i])
	}
	return nil
}

func (c *liveKit) publishVP9SVC(name string, source livekit.TrackSource) error {
	kf := &keyframeRequest{}
	track, err := lksdk.NewLocalTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0",
	}, lksdk.WithRTCPHandler(func(p rtcp.Packet) { handleRTCP(p, kf) }))
	if err != nil {
		return err
	}
	// PublishTrack announces one spatial layer. LiveKit then treats VP9 as
	// simulcast and drops packets that carry no dependency descriptor. SVC
	// needs multiple-spatial mode and one VideoLayer per SID.
	if err := publishVP9SVCTrack(c.ctx, c.room.LocalParticipant, track, c.spec.Media.Layers, name, source); err != nil {
		return fmt.Errorf("publish video: %w", err)
	}
	go runSVC(c.ctx, c.spec.Media, func(p *rtp.Packet) error { return track.WriteRTP(p, nil) }, kf)
	return nil
}

func publishVP9SVCTrack(ctx context.Context, p *lksdk.LocalParticipant, track *lksdk.LocalTrack, layers []*media.Layer, name string, source livekit.TrackSource) error {
	pc := p.GetPublisherPeerConnection()
	if pc == nil {
		return fmt.Errorf("no publisher peer connection")
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}
	for _, tr := range pc.GetTransceivers() {
		if tr.Sender() != sender {
			continue
		}
		codecs := append([]webrtc.RTPCodecParameters{}, sender.GetParameters().Codecs...)
		for i, codec := range codecs {
			if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP9) {
				codecs[0], codecs[i] = codecs[i], codecs[0]
				break
			}
		}
		tr.SetCodecPreferences(codecs)
		break
	}

	qualities := []livekit.VideoQuality{livekit.VideoQuality_LOW, livekit.VideoQuality_MEDIUM, livekit.VideoQuality_HIGH}
	videoLayers := make([]*livekit.VideoLayer, len(layers))
	for i, l := range layers {
		videoLayers[i] = &livekit.VideoLayer{
			Quality: qualities[i+len(qualities)-len(layers)],
			Width:   uint32(l.Width),
			Height:  uint32(l.Height),
			Bitrate: uint32(l.BitrateBps),
		}
	}
	top := layers[len(layers)-1]
	req := &livekit.AddTrackRequest{
		Cid:    track.ID(),
		Name:   name,
		Source: source,
		Type:   livekit.TrackType_VIDEO,
		Width:  uint32(top.Width),
		Height: uint32(top.Height),
		Layers: videoLayers,
		SimulcastCodecs: []*livekit.SimulcastCodec{{
			Codec:          webrtc.MimeTypeVP9,
			Cid:            track.ID(),
			Layers:         videoLayers,
			VideoLayerMode: livekit.VideoLayer_MULTIPLE_SPATIAL_LAYERS_PER_STREAM,
		}},
	}

	engine, err := localEngine(p)
	if err != nil {
		return err
	}
	pub, ok := engine.Publisher()
	if !ok {
		return fmt.Errorf("no publisher transport")
	}
	published := make(chan *livekit.TrackPublishedResponse, 1)
	engine.RegisterTrackPublishedListener(track.ID(), published)
	defer engine.UnregisterTrackPublishedListener(track.ID())
	if err := engine.SendAddTrack(req); err != nil {
		return err
	}
	pub.Negotiate()
	select {
	case <-published:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timed out")
	}
}

func localEngine(p *lksdk.LocalParticipant) (*lksdk.RTCEngine, error) {
	v := reflect.ValueOf(p).Elem().FieldByName("engine")
	if !v.IsValid() || v.IsNil() {
		return nil, fmt.Errorf("livekit engine unavailable")
	}
	engine, ok := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*lksdk.RTCEngine)
	if !ok || engine == nil {
		return nil, fmt.Errorf("livekit engine unavailable")
	}
	return engine, nil
}

func (c *liveKit) Close() {
	c.closing.Store(true)
	c.cancel()
	if c.room != nil {
		c.room.Disconnect()
	}
}

var _ = errors.New

func init() {
	// The SDK logs every Pion state change at debug level; keep warnings only.
	protoLogger.InitFromConfig(&protoLogger.Config{Level: "warn"}, "loadgen")
	lksdk.SetLogger(protoLogger.GetLogger())
}
