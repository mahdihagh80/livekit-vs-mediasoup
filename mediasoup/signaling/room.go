package main

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

// Peer is one WebSocket client. Fields after the comment are guarded by room.mu.
type Peer struct {
	ID   string
	Role string // "publisher" or "subscriber", from the ws URL
	room *Room
	conn *websocket.Conn
	send chan []byte   // gorilla allows one writer -> writePump
	done chan struct{} // closed when the socket is gone (never close `send`)
	gone chan struct{} // closed when the media service dropped the peer and the id is free

	// guarded by room.mu
	kicked          bool // replaced by a reconnect with the same id
	joined          bool
	rtpCapabilities json.RawMessage
	preferredLayer  *int // spatial layer this peer wants from video producers (nil = highest)
	recvTransportID string
	producers       []string
}

func (p *Peer) enqueue(msg []byte) {
	select {
	case p.send <- msg:
	case <-p.done:
	default:
		mDroppedMessages.Inc()
	}
}

// Notify queues a server->client notification without blocking.
func (p *Peer) Notify(method string, data any) {
	msg, err := json.Marshal(struct {
		Notification bool   `json:"notification"`
		Method       string `json:"method"`
		Data         any    `json:"data"`
	}{true, method, data})
	if err != nil {
		return
	}
	p.enqueue(msg)
}

type Room struct {
	ID    string
	mu    sync.Mutex
	peers map[string]*Peer
}

type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func NewHub() *Hub { return &Hub{rooms: map[string]*Room{}} }

// kick closes the socket of a peer that a reconnect with the same id replaces;
// its readPump then returns and the normal teardown runs.
func (p *Peer) kick() {
	p.room.mu.Lock()
	p.kicked = true
	c := p.conn
	p.room.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// AddPeer adds p to the room, or returns the peer that still holds its id.
func (h *Hub) AddPeer(roomID string, p *Peer) (taken *Peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	if !ok {
		room = &Room{ID: roomID, peers: map[string]*Peer{}}
		h.rooms[roomID] = room
		mRooms.Inc()
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	if old, ok := room.peers[p.ID]; ok {
		return old
	}
	p.room = room
	room.peers[p.ID] = p
	return nil
}

func (h *Hub) RemovePeer(p *Peer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := p.room
	room.mu.Lock()
	delete(room.peers, p.ID)
	empty := len(room.peers) == 0
	room.mu.Unlock()
	if empty {
		delete(h.rooms, room.ID)
		mRooms.Dec()
	}
}

func (h *Hub) FindPeer(roomID, peerID string) *Peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	room, ok := h.rooms[roomID]
	if !ok {
		return nil
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	return room.peers[peerID]
}
