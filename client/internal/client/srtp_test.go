package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pion/srtp/v3"
	"github.com/pion/webrtc/v4"
)

func TestSRTPProfileMatchesLiveKit(t *testing.T) {
	ms := &mediasoupClient{spec: Spec{IncludeLoopback: true}}
	se, err := ms.settingEngine(true)
	if err != nil {
		t.Fatal(err)
	}
	ma, mb := &webrtc.MediaEngine{}, &webrtc.MediaEngine{}
	if err := ma.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	if err := mb.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	a, err := webrtc.NewAPI(webrtc.WithMediaEngine(ma), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := webrtc.NewAPI(webrtc.WithMediaEngine(mb)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	connected := func(pc *webrtc.PeerConnection) <-chan struct{} {
		ch := make(chan struct{})
		var once sync.Once
		pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
			if s == webrtc.PeerConnectionStateConnected {
				once.Do(func() { close(ch) })
			}
		})
		return ch
	}
	ca, cb := connected(a), connected(b)

	// b offers and a answers, like mediasoup and the recv PC.
	if _, err := b.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatal(err)
	}
	offer, err := b.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gb := webrtc.GatheringCompletePromise(b)
	if err := b.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gb
	if err := a.SetRemoteDescription(*b.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := a.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	ga := webrtc.GatheringCompletePromise(a)
	if err := a.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-ga
	if err := b.SetRemoteDescription(*a.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan struct{}{ca, cb} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("PeerConnections did not connect")
		}
	}

	want := srtp.ProtectionProfileAeadAes128Gcm.String()
	for name, pc := range map[string]*webrtc.PeerConnection{"mediasoup-configured": a, "pion-default": b} {
		if got := srtpCipher(pc); got != want {
			t.Errorf("%s PC negotiated %q, want %q", name, got, want)
		}
	}
	if got := SRTPProfiles(); len(got) != 2 || got[0] != want || got[1] != "SRTP_AES128_CM_HMAC_SHA1_80" {
		t.Errorf("SRTPProfiles() = %v", got)
	}
}

func TestRetryUntilStopsOnFirstTrue(t *testing.T) {
	n := 0
	retryUntil(context.Background(), time.Millisecond, func() bool { n++; return n == 3 })
	time.Sleep(10 * time.Millisecond)
	if n != 3 {
		t.Fatalf("f called %d times, want 3", n)
	}
}

func TestRetryUntilHasNoTryLimit(t *testing.T) {
	n := 0
	retryUntil(context.Background(), time.Millisecond, func() bool { n++; return n == 25 })
	if n != 25 {
		t.Fatalf("f called %d times, want 25", n)
	}
}

func TestRetryUntilReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		retryUntil(ctx, time.Millisecond, func() bool { return false })
		close(done)
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retryUntil did not return after cancel")
	}
}
