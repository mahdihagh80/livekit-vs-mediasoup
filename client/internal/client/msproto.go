package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---- mediasoup JSON types (only the fields we use) ------------------------------

type msFeedback struct {
	Type      string `json:"type"`
	Parameter string `json:"parameter,omitempty"`
}

type msCodec struct {
	Kind                 string         `json:"kind,omitempty"`
	MimeType             string         `json:"mimeType"`
	PreferredPayloadType int            `json:"preferredPayloadType,omitempty"`
	PayloadType          int            `json:"payloadType,omitempty"`
	ClockRate            int            `json:"clockRate"`
	Channels             int            `json:"channels,omitempty"`
	Parameters           map[string]any `json:"parameters"`
	RtcpFeedback         []msFeedback   `json:"rtcpFeedback"`
}

type msHeaderExt struct {
	Kind             string `json:"kind,omitempty"`
	URI              string `json:"uri"`
	PreferredID      int    `json:"preferredId,omitempty"`
	PreferredEncrypt bool   `json:"preferredEncrypt,omitempty"`
	Direction        string `json:"direction,omitempty"`
	ID               int    `json:"id,omitempty"`
	Encrypt          bool   `json:"encrypt,omitempty"`
}

type msRtpCapabilities struct {
	Codecs           []msCodec     `json:"codecs"`
	HeaderExtensions []msHeaderExt `json:"headerExtensions"`
}

type msRtx struct {
	SSRC uint32 `json:"ssrc"`
}

type msEncoding struct {
	SSRC            uint32 `json:"ssrc,omitempty"`
	RID             string `json:"rid,omitempty"`
	RTX             *msRtx `json:"rtx,omitempty"`
	MaxBitrate      int    `json:"maxBitrate,omitempty"`
	ScalabilityMode string `json:"scalabilityMode,omitempty"`
}

type msRtcp struct {
	CNAME       string `json:"cname,omitempty"`
	ReducedSize bool   `json:"reducedSize"`
}

type msRtpParameters struct {
	Mid              string        `json:"mid,omitempty"`
	Codecs           []msCodec     `json:"codecs"`
	HeaderExtensions []msHeaderExt `json:"headerExtensions"`
	Encodings        []msEncoding  `json:"encodings"`
	Rtcp             msRtcp        `json:"rtcp"`
}

type msIceCandidate struct {
	Foundation string `json:"foundation"`
	Priority   uint32 `json:"priority"`
	Address    string `json:"address"`
	IP         string `json:"ip"` // older mediasoup
	Protocol   string `json:"protocol"`
	Port       int    `json:"port"`
	Type       string `json:"type"`
	TCPType    string `json:"tcpType,omitempty"`
}

type msFingerprint struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type msDtlsParameters struct {
	Role         string          `json:"role,omitempty"`
	Fingerprints []msFingerprint `json:"fingerprints"`
}

type msTransport struct {
	ID            string `json:"id"`
	IceParameters struct {
		UsernameFragment string `json:"usernameFragment"`
		Password         string `json:"password"`
		IceLite          bool   `json:"iceLite"`
	} `json:"iceParameters"`
	IceCandidates  []msIceCandidate `json:"iceCandidates"`
	DtlsParameters msDtlsParameters `json:"dtlsParameters"`
}

type msNewConsumer struct {
	ID             string          `json:"id"`
	ProducerID     string          `json:"producerId"`
	ProducerPeerID string          `json:"producerPeerId"`
	Kind           string          `json:"kind"`
	RtpParameters  msRtpParameters `json:"rtpParameters"`
}

// ---- WebSocket request/response client (mirrors ../../mediasoup/signaling) --------

type msNotification struct {
	Method string
	Data   json.RawMessage
}

type msSignal struct {
	conn    *websocket.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int
	pending map[int]chan msResponse
	notify  chan msNotification
	closed  chan struct{}
}

type msResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

// dial (may be nil) opens the TCP connection, e.g. through a network shaper.
func dialMsSignal(ctx context.Context, base, room, peer, role string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) (*msSignal, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("roomId", room)
	q.Set("peerId", peer)
	q.Set("role", role)
	u.RawQuery = q.Encode()
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, NetDialContext: dial}
	conn, _, err := d.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	s := &msSignal{conn: conn, pending: map[int]chan msResponse{}, notify: make(chan msNotification, 256), closed: make(chan struct{})}
	go s.readLoop()
	return s, nil
}

func (s *msSignal) readLoop() {
	defer func() {
		close(s.closed)
		s.mu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.mu.Unlock()
	}()
	s.conn.SetPingHandler(func(data string) error {
		s.wmu.Lock()
		defer s.wmu.Unlock()
		return s.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			Response     bool            `json:"response"`
			Notification bool            `json:"notification"`
			ID           int             `json:"id"`
			OK           bool            `json:"ok"`
			Data         json.RawMessage `json:"data"`
			Error        string          `json:"error"`
			Method       string          `json:"method"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if m.Response {
			s.mu.Lock()
			ch := s.pending[m.ID]
			delete(s.pending, m.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- msResponse{OK: m.OK, Data: m.Data, Error: m.Error}
			}
		} else if m.Notification {
			select {
			case s.notify <- msNotification{m.Method, m.Data}:
			case <-time.After(5 * time.Second): // consumer goroutine stuck; drop
			}
		}
	}
}

func (s *msSignal) request(ctx context.Context, method string, data any, out any) error {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan msResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "data": data})
	s.wmu.Lock()
	s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := s.conn.WriteMessage(websocket.TextMessage, msg)
	s.wmu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("signaling closed")
		}
		if !r.OK {
			return fmt.Errorf("%s: %s", method, r.Error)
		}
		if out != nil {
			return json.Unmarshal(r.Data, out)
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (s *msSignal) close() { s.conn.Close() }
