// Signaling server for the mediasoup side of the benchmark.
//
// Load generator <-> this server: WebSocket, JSON {id,method,data} requests,
// {response,id,ok,data|error} responses, {notification,method,data} pushes.
// This server <-> media service: HTTP POST /rpc (media.go).
//
// Unlike the lab, subscription is SERVER-DRIVEN, the same as LiveKit's
// auto-subscribe: when a peer joins it is subscribed to every existing
// producer, and when a producer appears every joined peer is subscribed. The
// client only receives "newConsumer", wires the receiver, and calls
// "resumeConsumer". This keeps both SFUs on the same message count per join.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// All settings come from signaling.yaml. The only flag is its path.
var configPath = flag.String("config", defaultConfigPath(), "path to signaling.yaml")

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true }, // benchmark only
}

type Server struct {
	hub     *Hub
	media   *MediaClient
	consume chan struct{} // semaphore
}

type clientRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Data   json.RawMessage `json:"data"`
}

type serverResponse struct {
	Response bool   `json:"response"`
	ID       int    `json:"id"`
	OK       bool   `json:"ok"`
	Data     any    `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("roomId")
	peerID := r.URL.Query().Get("peerId")
	if roomID == "" || peerID == "" {
		http.Error(w, "roomId and peerId are required", http.StatusBadRequest)
		return
	}
	// role places the peer under the webinar scheduler (publishers share a worker).
	role := r.URL.Query().Get("role")
	if role != "publisher" {
		role = "subscriber"
	}
	peer := &Peer{ID: peerID, Role: role, send: make(chan []byte, 256), done: make(chan struct{}), gone: make(chan struct{})}
	// The same peerId again is a reconnect, often before or just as the old
	// socket closes. Like LiveKit with a duplicate identity, the old session is
	// dropped, and this one waits until the media service has removed the old
	// peer: the media service keys peers by id, so the old closePeer would
	// otherwise remove the new peer (or the new join reuse the old one).
	deadline := time.After(15 * time.Second) // > the closePeer timeout below
	for {
		old := s.hub.AddPeer(roomID, peer)
		if old == nil {
			break
		}
		old.kick()
		select {
		case <-old.gone:
		case <-deadline:
			http.Error(w, "peerId already in room", http.StatusConflict)
			return
		case <-r.Context().Done():
			return
		}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.hub.RemovePeer(peer)
		close(peer.gone)
		return
	}
	room := peer.room
	room.mu.Lock()
	peer.conn = conn
	kicked := peer.kicked // a newer reconnect came while upgrading
	room.mu.Unlock()
	if kicked {
		conn.Close()
	}
	mWSConnections.Inc()

	go s.writePump(peer)
	s.readPump(peer)

	// Keep the id taken until the media service has dropped the peer (above).
	room.mu.Lock()
	peer.joined = false // no new subscriptions to it, nobody consumes its producers
	peer.producers = nil
	room.mu.Unlock()
	close(peer.done)
	mWSConnections.Dec()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.media.Call(ctx, "closePeer", roomID, peerID, nil); err != nil {
		log.Printf("[%s] closePeer %s: %v", roomID, peerID, err)
	}
	s.hub.RemovePeer(peer)
	close(peer.gone)
}

func (s *Server) writePump(p *Peer) {
	ping := time.NewTicker(20 * time.Second)
	defer func() { ping.Stop(); p.conn.Close() }()
	for {
		select {
		case <-p.done:
			return
		case msg := <-p.send:
			p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			p.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (s *Server) readPump(p *Peer) {
	p.conn.SetReadLimit(1 << 20)
	p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	p.conn.SetPongHandler(func(string) error {
		p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		_, raw, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var req clientRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			continue
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		data, err := s.handleRequest(ctx, p, req)
		cancel()
		ok := "true"
		resp := serverResponse{Response: true, ID: req.ID, OK: err == nil, Data: data}
		if err != nil {
			ok = "false"
			resp.Error = err.Error()
			log.Printf("[%s] %s %s failed: %v", p.room.ID, p.ID, req.Method, err)
		}
		mRequestSeconds.WithLabelValues(req.Method, ok).Observe(time.Since(start).Seconds())
		msg, _ := json.Marshal(resp)
		p.enqueue(msg)
	}
}

func (s *Server) handleRequest(ctx context.Context, p *Peer, req clientRequest) (any, error) {
	room := p.room
	call := func(method string, data any) (json.RawMessage, error) {
		return s.media.Call(ctx, method, room.ID, p.ID, data)
	}

	switch req.Method {
	case "getRouterRtpCapabilities":
		return call("join", map[string]string{"role": p.Role}) // also places the peer on a worker

	case "createWebRtcTransport":
		var d struct {
			Direction string `json:"direction"`
		}
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return nil, err
		}
		res, err := call("createWebRtcTransport", req.Data)
		if err != nil {
			return nil, err
		}
		if d.Direction == "recv" {
			var out struct {
				ID string `json:"id"`
			}
			json.Unmarshal(res, &out)
			room.mu.Lock()
			p.recvTransportID = out.ID
			room.mu.Unlock()
		}
		return res, nil

	case "connectWebRtcTransport", "resumeConsumer":
		return call(req.Method, req.Data)

	case "join":
		var d struct {
			RtpCapabilities       json.RawMessage `json:"rtpCapabilities"`
			PreferredSpatialLayer *int            `json:"preferredSpatialLayer"`
		}
		if err := json.Unmarshal(req.Data, &d); err != nil {
			return nil, err
		}
		room.mu.Lock()
		if p.joined {
			room.mu.Unlock()
			return nil, errors.New("already joined")
		}
		if p.recvTransportID == "" {
			room.mu.Unlock()
			return nil, errors.New("create the recv transport before join")
		}
		p.joined = true
		p.rtpCapabilities = d.RtpCapabilities
		p.preferredLayer = d.PreferredSpatialLayer
		// Snapshot under the lock: together with the same snapshot in "produce"
		// this guarantees every (subscriber, producer) pair is consumed exactly once.
		var existing []string
		for _, other := range room.peers {
			if other != p {
				existing = append(existing, other.producers...)
			}
		}
		room.mu.Unlock()
		if len(existing) > 0 {
			s.subscribeMany(p, existing)
		}
		return map[string]any{"existingProducers": len(existing)}, nil

	case "produce":
		res, err := call("produce", req.Data)
		if err != nil {
			return nil, err
		}
		var out struct {
			ID string `json:"id"`
		}
		json.Unmarshal(res, &out)
		room.mu.Lock()
		p.producers = append(p.producers, out.ID)
		var subs []*Peer
		for _, other := range room.peers {
			if other != p && other.joined {
				subs = append(subs, other)
			}
		}
		room.mu.Unlock()
		for _, sub := range subs {
			s.subscribe(sub, out.ID)
		}
		return res, nil
	}
	return nil, errors.New("unknown method " + req.Method)
}

// subscribe creates a (paused) consumer for sub and pushes "newConsumer".
// Runs asynchronously with bounded concurrency so a webinar publisher's
// produce() doesn't block on thousands of consume RPCs.
func (s *Server) subscribe(sub *Peer, producerID string) {
	go func() {
		s.consume <- struct{}{}
		defer func() { <-s.consume }()
		select {
		case <-sub.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req := consumeRequest(sub)
		req["producerId"] = producerID
		res, err := s.media.Call(ctx, "consume", sub.room.ID, sub.ID, req)
		if err != nil {
			mConsumes.WithLabelValues("false").Inc()
			log.Printf("[%s] consume %s for %s: %v", sub.room.ID, producerID, sub.ID, err)
			return
		}
		mConsumes.WithLabelValues("true").Inc()
		sub.Notify("newConsumer", res)
	}()
}

// subscribeMany is subscribe for every producer a joining peer needs, in one
// "consumeMany" RPC instead of one "consume" RPC per producer.
func (s *Server) subscribeMany(sub *Peer, producerIDs []string) {
	go func() {
		s.consume <- struct{}{}
		defer func() { <-s.consume }()
		select {
		case <-sub.done:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req := consumeRequest(sub)
		req["producerIds"] = producerIDs
		res, err := s.media.Call(ctx, "consumeMany", sub.room.ID, sub.ID, req)
		if err != nil {
			mConsumes.WithLabelValues("false").Add(float64(len(producerIDs)))
			log.Printf("[%s] consumeMany (%d producers) for %s: %v", sub.room.ID, len(producerIDs), sub.ID, err)
			return
		}
		var items []struct {
			ProducerID string          `json:"producerId"`
			OK         bool            `json:"ok"`
			Data       json.RawMessage `json:"data"`
			Error      string          `json:"error"`
		}
		if err := json.Unmarshal(res, &items); err != nil {
			mConsumes.WithLabelValues("false").Add(float64(len(producerIDs)))
			log.Printf("[%s] consumeMany for %s: bad response: %v", sub.room.ID, sub.ID, err)
			return
		}
		for _, it := range items {
			if !it.OK {
				mConsumes.WithLabelValues("false").Inc()
				log.Printf("[%s] consume %s for %s: %s", sub.room.ID, it.ProducerID, sub.ID, it.Error)
				continue
			}
			mConsumes.WithLabelValues("true").Inc()
			sub.Notify("newConsumer", it.Data)
		}
	}()
}

// consumeRequest is the per-subscriber part of a consume / consumeMany request.
func consumeRequest(sub *Peer) map[string]any {
	room := sub.room
	room.mu.Lock()
	caps, transportID, layer := sub.rtpCapabilities, sub.recvTransportID, sub.preferredLayer
	room.mu.Unlock()
	req := map[string]any{"transportId": transportID, "rtpCapabilities": caps}
	if layer != nil {
		req["preferredSpatialLayer"] = *layer
	}
	return req
}

func (s *Server) handleMediaEvent(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var ev struct {
		RoomID string          `json:"roomId"`
		PeerID string          `json:"peerId"`
		Method string          `json:"method"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p := s.hub.FindPeer(ev.RoomID, ev.PeerID); p != nil {
		p.Notify(ev.Method, ev.Data)
	}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("config %s", *configPath)
	s := &Server{hub: NewHub(), media: NewMediaClient(cfg.MediaURL), consume: make(chan struct{}, cfg.ConsumeConcurrency)}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		log.Printf("metrics on http://%s/metrics", cfg.MetricsListen)
		log.Fatal(http.ListenAndServe(cfg.MetricsListen, mux))
	}()

	// Internal listener (loopback): events from the media service + health probe.
	// Separate from the client listener so `listen` can be bound to one interface
	// (e.g. the internal VLAN) without breaking media -> signaling events.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /internal/events", s.handleMediaEvent)
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		log.Printf("internal events on http://%s/internal/events", cfg.EventsListen)
		log.Fatal(http.ListenAndServe(cfg.EventsListen, mux))
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	log.Printf("signaling on ws://%s/ws (media service %s)", cfg.Listen, cfg.MediaURL)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
