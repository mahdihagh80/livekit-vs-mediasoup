package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/webrtc/v4"
	pionmedia "github.com/pion/webrtc/v4/pkg/media"

	"sfubench/loadgen/internal/netem"
	"sfubench/loadgen/internal/stats"
)

// mediasoupClient is a minimal mediasoup-client written in Go on Pion: two
// PeerConnections (send / recv) driven by SDP we build from the transport
// parameters, plus the JSON-over-WebSocket protocol of ../../mediasoup/signaling.
type mediasoupClient struct {
	spec   Spec
	ctx    context.Context
	cancel context.CancelFunc
	done   doneOnce
	sig    *msSignal

	routerCaps msRtpCapabilities

	recvT  msTransport
	recvPC *webrtc.PeerConnection
	nack   *nackGen // the recv PC's NACK generator
	sendPC *webrtc.PeerConnection
	srtp   srtpSeen

	consumers    []msNewConsumer // every consumer ever negotiated (m-sections are never removed)
	recvVersion  int
	recvConnOnce sync.Once
	probator     bool

	tracks          sync.Map // consumer id -> *webrtc.TrackRemote
	closedConsumers sync.Map // consumer id -> true

	closing   atomic.Bool
	joinStart time.Time
	ttffOnce  sync.Once
}

func newMediasoup(spec Spec) *mediasoupClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &mediasoupClient{spec: spec, ctx: ctx, cancel: cancel, done: newDoneOnce()}
}

func (c *mediasoupClient) Done() <-chan struct{} { return c.done.ch }

func (c *mediasoupClient) fail() {
	if !c.closing.Load() {
		c.done.close()
	}
}

// newAPI builds a Pion API whose payload types match the router's, so the same
// numbers are used on both directions and no PT remapping is needed. gen is
// the NACK generator of the one PeerConnection made from it; recv says whether
// that is the recv PeerConnection (the only one the network profile shapes).
func (c *mediasoupClient) newAPI(gen *nackGen, recv bool) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	videoFB := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}, {Type: "goog-remb"}, {Type: webrtc.TypeRTCPFBTransportCC}}
	var videoPTs []int
	for _, rc := range c.routerCaps.Codecs {
		switch strings.ToLower(rc.MimeType) {
		case "audio/opus":
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
					RTCPFeedback: []webrtc.RTCPFeedback{{Type: webrtc.TypeRTCPFBTransportCC}}},
				PayloadType: webrtc.PayloadType(rc.PreferredPayloadType),
			}, webrtc.RTPCodecTypeAudio); err != nil {
				return nil, err
			}
		case "video/vp8", "video/vp9":
			videoPTs = append(videoPTs, rc.PreferredPayloadType)
			vc := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: videoFB}
			if strings.EqualFold(rc.MimeType, "video/vp9") {
				vc.MimeType = webrtc.MimeTypeVP9
				vc.SDPFmtpLine = "profile-id=0"
			}
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: vc,
				PayloadType:        webrtc.PayloadType(rc.PreferredPayloadType),
			}, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
		}
	}
	for _, rc := range c.routerCaps.Codecs {
		if !strings.EqualFold(rc.MimeType, "video/rtx") {
			continue
		}
		apt := fmt.Sprint(rc.Parameters["apt"])
		for _, pt := range videoPTs {
			if apt != fmt.Sprint(pt) {
				continue
			}
			if err := m.RegisterCodec(webrtc.RTPCodecParameters{
				RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000, SDPFmtpLine: fmt.Sprintf("apt=%d", pt)},
				PayloadType:        webrtc.PayloadType(rc.PreferredPayloadType),
			}, webrtc.RTPCodecTypeVideo); err != nil {
				return nil, err
			}
		}
	}
	for _, kind := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo} {
		for _, uri := range []string{uriMid, uriAbsSend, uriTWCC} {
			if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, kind); err != nil {
				return nil, err
			}
		}
	}
	for _, uri := range []string{uriRID, uriRRID} {
		if err := m.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}

	ir := &interceptor.Registry{}
	// First in the chain so it sees the NACK generator's output (see NewRTCPInterceptorFactory).
	ir.Add(stats.NewRTCPInterceptorFactory())
	// Pion's default set (webrtc.RegisterDefaultInterceptors: NACK responder
	// and generator, RTCP SR/RR, simulcast header extensions, stats, TWCC
	// feedback), with our NACK generator in place of Pion's (see nackGen).
	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		return nil, err
	}
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack"}, webrtc.RTPCodecTypeVideo)
	m.RegisterFeedback(webrtc.RTCPFeedback{Type: "nack", Parameter: "pli"}, webrtc.RTPCodecTypeVideo)
	ir.Add(responder)
	ir.Add(nackGenFactory{gen})
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureStatsInterceptor(ir); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCSender(m, ir); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		return nil, err
	}

	se, err := c.settingEngine(recv)
	if err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se)), nil
}

func (c *mediasoupClient) settingEngine(recv bool) (webrtc.SettingEngine, error) {
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(0) // ice.MulticastDNSModeDisabled
	se.SetIncludeLoopbackCandidate(c.spec.IncludeLoopback)
	// On the recv PC we answer mediasoup's offer (a=setup:actpass). Be the DTLS
	// client, as mediasoup-client is, and tell mediasoup role "client".
	if err := se.SetAnsweringDTLSRole(webrtc.DTLSRoleClient); err != nil {
		return se, err
	}
	se.SetSRTPProtectionProfiles(srtpProfiles...)
	if len(c.spec.LocalIPs) > 0 {
		allowed := map[string]bool{}
		for _, ip := range c.spec.LocalIPs {
			allowed[ip] = true
		}
		se.SetIPFilter(func(ip net.IP) bool { return allowed[ip.String()] })
	}
	if c.spec.Shaper != nil {
		// The profile shapes the recv transport only; the send transport sees outages only.
		newNet := netem.NewOutageNet
		if recv {
			newNet = netem.NewNet
		}
		n, err := newNet(c.spec.Shaper)
		if err != nil {
			return se, err
		}
		se.SetNet(n)
	}
	return se, nil
}

// deviceCaps is what we tell the router we can receive.
func (c *mediasoupClient) deviceCaps() msRtpCapabilities {
	var caps msRtpCapabilities
	for _, rc := range c.routerCaps.Codecs {
		switch strings.ToLower(rc.MimeType) {
		case "audio/opus", "video/vp8", "video/vp9", "video/rtx":
			caps.Codecs = append(caps.Codecs, rc)
		}
	}
	for _, he := range c.routerCaps.HeaderExtensions {
		switch he.URI {
		case uriMid, uriAbsSend, uriTWCC:
			caps.HeaderExtensions = append(caps.HeaderExtensions, he)
		}
	}
	return caps
}

func (c *mediasoupClient) Join(ctx context.Context) error {
	c.joinStart = time.Now()
	stage := "signal"
	err := c.join(ctx, &stage)
	if err != nil {
		stats.M.JoinFailures.WithLabelValues(stage).Inc()
		c.Close()
	}
	return err
}

func (c *mediasoupClient) join(ctx context.Context, stage *string) error {
	role := "subscriber"
	if c.spec.Publish {
		role = "publisher"
	}
	var dial func(ctx context.Context, network, addr string) (net.Conn, error)
	if c.spec.Shaper != nil {
		dial = c.spec.Shaper.DialContext
	}
	sig, err := dialMsSignal(ctx, c.spec.URL, c.spec.Room, c.spec.Identity, role, dial)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.sig = sig
	go func() {
		<-sig.closed
		c.fail()
	}()

	var caps struct {
		RtpCapabilities msRtpCapabilities `json:"rtpCapabilities"`
	}
	if err := sig.request(ctx, "getRouterRtpCapabilities", nil, &caps); err != nil {
		return err
	}
	c.routerCaps = caps.RtpCapabilities

	// Recv transport first (the server needs it before "join" to push consumers).
	if err := sig.request(ctx, "createWebRtcTransport", map[string]string{"direction": "recv"}, &c.recvT); err != nil {
		return err
	}
	c.nack = newNackGen(msProbatorSSRC) // the probator is padding and never resent
	api, err := c.newAPI(c.nack, true)
	if err != nil {
		return err
	}
	if c.recvPC, err = api.NewPeerConnection(webrtc.Configuration{}); err != nil {
		return err
	}
	c.recvPC.OnTrack(c.onTrack)
	recvPC := c.recvPC
	recvPC.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		debugf("%s recv pc state %s", c.spec.Identity, s)
		switch s {
		case webrtc.PeerConnectionStateConnected:
			go retryUntil(c.ctx, time.Second, func() bool { return c.srtp.record("mediasoup", recvPC) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			c.fail()
		}
	})
	go c.notificationLoop()

	// preferredSpatialLayer: the layer this subscriber wants (consumer
	// preferredLayers), the same layer the LiveKit adapter asks for.
	join := map[string]any{"rtpCapabilities": c.deviceCaps()}
	if c.spec.Media != nil && len(c.spec.Media.Layers) > 0 {
		join["preferredSpatialLayer"] = c.spec.Media.LayerFor(c.spec.Quality)
	}
	if err := sig.request(ctx, "join", join, nil); err != nil {
		return err
	}

	if !c.spec.Publish {
		return nil
	}
	*stage = "publish"
	return c.publish(ctx)
}

func (c *mediasoupClient) publish(ctx context.Context) error {
	var st msTransport
	if err := c.sig.request(ctx, "createWebRtcTransport", map[string]string{"direction": "send"}, &st); err != nil {
		return err
	}
	api, err := c.newAPI(newNackGen(msProbatorSSRC), false)
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	c.sendPC = pc
	connected := make(chan struct{})
	var connOnce sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connected) })
			go retryUntil(c.ctx, time.Second, func() bool { return c.srtp.record("mediasoup", pc) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			c.fail()
		}
	})

	streamID := c.spec.Identity
	type pending struct {
		kind   string
		sender *webrtc.RTPSender
		start  func()
	}
	var sends []pending

	if c.spec.Audio {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", streamID)
		if err != nil {
			return err
		}
		tr, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
		if err != nil {
			return err
		}
		sender := tr.Sender()
		go readSenderRTCP(c.ctx, sender, "", nil)
		sends = append(sends, pending{"audio", sender, func() {
			go runAudio(c.ctx, func(data []byte, dur time.Duration) error {
				return track.WriteSample(pionmedia.Sample{Data: data, Duration: dur})
			}, c.spec.Talk)
		}})
	}
	addVideo := func(name string) error {
		if c.spec.Media.SVC() {
			track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP9, ClockRate: 90000, SDPFmtpLine: "profile-id=0",
			}, name, streamID)
			if err != nil {
				return err
			}
			tr, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
			if err != nil {
				return err
			}
			sender := tr.Sender()
			sends = append(sends, pending{"video", sender, func() {
				kf := &keyframeRequest{}
				go readSenderRTCP(c.ctx, sender, "", kf)
				go runSVC(c.ctx, c.spec.Media, track.WriteRTP, kf)
			}})
		} else {
			var tracks []*webrtc.TrackLocalStaticSample
			for _, l := range c.spec.Media.Layers {
				t, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, name, streamID, webrtc.WithRTPStreamID(l.RID))
				if err != nil {
					return err
				}
				tracks = append(tracks, t)
			}
			tr, err := pc.AddTransceiverFromTrack(tracks[0], webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly})
			if err != nil {
				return err
			}
			sender := tr.Sender()
			for _, t := range tracks[1:] {
				if err := sender.AddEncoding(t); err != nil {
					return err
				}
			}
			layers := c.spec.Media.Layers
			sends = append(sends, pending{"video", sender, func() {
				for i, l := range layers {
					kf := &keyframeRequest{}
					go readSenderRTCP(c.ctx, sender, l.RID, kf)
					t := tracks[i]
					go runVideoLayer(c.ctx, l, i, func(data []byte, dur time.Duration) error {
						return t.WriteSample(pionmedia.Sample{Data: data, Duration: dur})
					}, kf)
				}
			}})
		}
		return nil
	}
	if c.spec.Video {
		if err := addVideo("cam"); err != nil {
			return err
		}
		if c.spec.ScreenShare {
			if err := addVideo("screen"); err != nil {
				return err
			}
		}
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return ctx.Err()
	}
	local := pc.LocalDescription().SDP
	fp, err := localFingerprint(local)
	if err != nil {
		return err
	}
	if err := c.sig.request(ctx, "connectWebRtcTransport", map[string]any{
		"transportId":    st.ID,
		"dtlsParameters": msDtlsParameters{Role: "client", Fingerprints: []msFingerprint{{Algorithm: "sha-256", Value: fp}}},
	}, nil); err != nil {
		return err
	}
	media, err := parseOffer(local)
	if err != nil {
		return err
	}
	answer, err := buildSendAnswer(&st, media, 1)
	if err != nil {
		return err
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return fmt.Errorf("set send answer: %w", err)
	}

	for i, s := range sends {
		m := media[i]
		mime := ""
		if s.kind == "video" {
			mime = "vp8"
			if c.spec.Media.SVC() {
				mime = "vp9"
			}
			filtered, ok := filterVideo(m, mime)
			if !ok {
				return fmt.Errorf("send offer has no %s codec", mime)
			}
			m = filtered
		}
		var encs []msEncoding
		params := s.sender.GetParameters()
		for j, e := range params.Encodings {
			enc := msEncoding{SSRC: uint32(e.SSRC), RID: e.RID}
			if e.RTX.SSRC != 0 {
				enc.RTX = &msRtx{SSRC: uint32(e.RTX.SSRC)}
			}
			if s.kind == "video" && c.spec.Media.SVC() {
				enc.RID = ""
				enc.MaxBitrate = c.spec.Media.TotalBitrate()
				enc.ScalabilityMode = c.spec.Media.ScalabilityMode()
			} else if s.kind == "video" {
				enc.MaxBitrate = c.spec.Media.Layers[j].BitrateBps
				if enc.RID == "" {
					enc.RID = c.spec.Media.Layers[j].RID
				}
			}
			encs = append(encs, enc)
		}
		cname := m.CNAME
		if cname == "" {
			cname = c.spec.Identity
		}
		rp := producerParams(m, encs, cname)
		if err := c.sig.request(ctx, "produce", map[string]any{"transportId": st.ID, "kind": s.kind, "rtpParameters": rp}, nil); err != nil {
			return err
		}
	}

	select {
	case <-connected:
	case <-ctx.Done():
		return fmt.Errorf("send transport: %w", ctx.Err())
	}
	for _, s := range sends {
		s.start()
	}
	return nil
}

// readSenderRTCP drains RTCP for one encoding (rid "" = non-simulcast). Pion
// requires this so interceptors (the NACK responder) see the packets.
func readSenderRTCP(ctx context.Context, s *webrtc.RTPSender, rid string, kf *keyframeRequest) {
	for ctx.Err() == nil {
		var err error
		if rid == "" {
			pkts, _, e := s.ReadRTCP()
			err = e
			for _, p := range pkts {
				handleRTCP(p, kf)
			}
		} else {
			pkts, _, e := s.ReadSimulcastRTCP(rid)
			err = e
			for _, p := range pkts {
				handleRTCP(p, kf)
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *mediasoupClient) onTrack(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	kind := t.Kind().String()
	debugf("%s onTrack %s ssrc=%d", c.spec.Identity, kind, t.SSRC())
	if t.SSRC() == msProbatorSSRC { // mediasoup probation (padding) stream: drain, don't count
		buf := make([]byte, 1600)
		for {
			if _, _, err := t.Read(buf); err != nil {
				return
			}
		}
	}
	// msid is "<producer peer> <consumer id>": the stream is the publisher.
	// The consumer does not name the producer's track (cam/screen).
	ti := c.spec.trackInfo(kind, t.Codec().ClockRate, t.StreamID(), "")
	if kind == "video" {
		ti.OnFirstFrame = func() {
			c.ttffOnce.Do(func() { stats.M.TTFF.WithLabelValues(profileOf(c.spec)).Observe(time.Since(c.joinStart).Seconds()) })
		}
	}
	// consumerClosed does not end a Pion track (its m-section stays): end the
	// read with a deadline so the track stops counting as received.
	c.tracks.Store(t.ID(), t)
	if _, closed := c.closedConsumers.Load(t.ID()); closed {
		_ = t.SetReadDeadline(time.Now())
	}
	defer c.tracks.Delete(t.ID())
	stats.ConsumeTrack(ti, readRepaired(t, c.nack, stats.M.SubRTXDup.WithLabelValues(profileOf(c.spec))), c.spec.Recv)
}

// notificationLoop batches newConsumer notifications: one SDP renegotiation for
// everything that arrived together, then resume each consumer.
func (c *mediasoupClient) notificationLoop() {
	for {
		var batch []msNewConsumer
		select {
		case <-c.ctx.Done():
			return
		case <-c.sig.closed:
			return
		case n := <-c.sig.notify:
			batch = c.appendNotification(batch, n)
		}
	drain:
		for {
			select {
			case n := <-c.sig.notify:
				batch = c.appendNotification(batch, n)
			default:
				break drain
			}
		}
		if len(batch) == 0 {
			continue
		}
		if err := c.addConsumers(batch); err != nil {
			if !c.closing.Load() {
				stats.M.JoinFailures.WithLabelValues("consume").Inc()
			}
			c.fail()
			return
		}
	}
}

func (c *mediasoupClient) appendNotification(batch []msNewConsumer, n msNotification) []msNewConsumer {
	if n.Method == "consumerClosed" {
		var cc struct {
			ConsumerID string `json:"consumerId"`
		}
		if json.Unmarshal(n.Data, &cc) == nil && cc.ConsumerID != "" {
			c.closedConsumers.Store(cc.ConsumerID, true)
			if t, ok := c.tracks.Load(cc.ConsumerID); ok {
				_ = t.(*webrtc.TrackRemote).SetReadDeadline(time.Now())
			}
		}
		return batch // m-sections are kept
	}
	if n.Method != "newConsumer" {
		return batch
	}
	var nc msNewConsumer
	if json.Unmarshal(n.Data, &nc) == nil {
		batch = append(batch, nc)
	}
	return batch
}

func (c *mediasoupClient) addConsumers(batch []msNewConsumer) error {
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	c.consumers = append(c.consumers, batch...)
	if !c.probator {
		// Like mediasoup-client: an extra m-section for mediasoup's BWE probation
		// stream (SSRC 1234, PT 127). Without it Pion logs "unhandled RTP ssrc(1234)".
		for _, nc := range batch {
			if nc.Kind == "video" && len(nc.RtpParameters.Codecs) > 0 {
				codec := nc.RtpParameters.Codecs[0]
				codec.PayloadType = 127
				c.consumers = append(c.consumers, msNewConsumer{
					ID: "probator", ProducerPeerID: "probator", Kind: "video",
					RtpParameters: msRtpParameters{
						Mid: "probator", Codecs: []msCodec{codec}, HeaderExtensions: nc.RtpParameters.HeaderExtensions,
						Encodings: []msEncoding{{SSRC: msProbatorSSRC}}, Rtcp: msRtcp{CNAME: "probator", ReducedSize: true},
					},
				})
				c.probator = true
				break
			}
		}
	}
	c.recvVersion++
	offer, err := buildRecvOffer(&c.recvT, c.consumers, c.recvVersion)
	if err != nil {
		return err
	}
	debugf("%s recv offer v%d:\n%s", c.spec.Identity, c.recvVersion, offer)
	if err := c.recvPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return fmt.Errorf("set recv offer: %w", err)
	}
	answer, err := c.recvPC.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := c.recvPC.SetLocalDescription(answer); err != nil {
		return err
	}
	debugf("%s recv answer v%d:\n%s", c.spec.Identity, c.recvVersion, answer.SDP)
	var connErr error
	c.recvConnOnce.Do(func() {
		fp, err := localFingerprint(answer.SDP)
		if err != nil {
			connErr = err
			return
		}
		connErr = c.sig.request(ctx, "connectWebRtcTransport", map[string]any{
			"transportId":    c.recvT.ID,
			"dtlsParameters": msDtlsParameters{Role: "client", Fingerprints: []msFingerprint{{Algorithm: "sha-256", Value: fp}}},
		}, nil)
	})
	if connErr != nil {
		return connErr
	}
	for _, nc := range batch {
		if err := c.sig.request(ctx, "resumeConsumer", map[string]string{"consumerId": nc.ID}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *mediasoupClient) Close() {
	if !c.closing.CompareAndSwap(false, true) {
		return
	}
	c.cancel()
	if c.sig != nil {
		c.sig.close()
	}
	if c.sendPC != nil {
		c.sendPC.Close()
	}
	if c.recvPC != nil {
		c.recvPC.Close()
	}
}

var _ = errors.New

// DebugSDP logs every SDP the mediasoup adapter builds (bench.yaml: loadgen.debug_sdp).
var DebugSDP bool

func debugf(format string, args ...any) {
	if DebugSDP {
		log.Printf(format, args...)
	}
}
