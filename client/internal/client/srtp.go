package client

import (
	"context"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/srtp/v3"
	"github.com/pion/webrtc/v4"

	"sfubench/loadgen/internal/stats"
)

// srtpProfiles is the list the vendored LiveKit SDK offers (transport.go), so
// both SFUs negotiate the same cipher.
var srtpProfiles = []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80}

// SRTPProfiles names srtpProfiles the way Pion's TransportStats.SRTPCipher does.
func SRTPProfiles() []string {
	out := make([]string, len(srtpProfiles))
	for i, p := range srtpProfiles {
		out[i] = srtp.ProtectionProfile(p).String()
	}
	return out
}

func srtpCipher(pc *webrtc.PeerConnection) string {
	for _, s := range pc.GetStats() {
		if t, ok := s.(webrtc.TransportStats); ok && t.SRTPCipher != "" {
			return t.SRTPCipher
		}
	}
	return ""
}

// srtpSeen counts each PeerConnection's negotiated SRTP cipher once.
type srtpSeen struct {
	mu   sync.Mutex
	seen map[*webrtc.PeerConnection]bool
}

// record counts pc's cipher if pc is connected and was not counted yet, and
// reports whether pc has been counted.
func (s *srtpSeen) record(sfu string, pc *webrtc.PeerConnection) bool {
	if pc == nil {
		return false
	}
	s.mu.Lock()
	seen := s.seen[pc]
	s.mu.Unlock()
	if seen {
		return true
	}
	if pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return false
	}
	cipher := srtpCipher(pc)
	if cipher == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[pc] {
		return true
	}
	if s.seen == nil {
		s.seen = map[*webrtc.PeerConnection]bool{}
	}
	s.seen[pc] = true
	stats.M.PCSRTP.WithLabelValues(sfu, cipher).Inc()
	return true
}

// retryUntil calls f now and then every interval until f returns true or ctx is done.
// It has no try limit on purpose: a PC that connects late must still be counted.
func retryUntil(ctx context.Context, every time.Duration, f func() bool) {
	if f() {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if f() {
				return
			}
		}
	}
}
