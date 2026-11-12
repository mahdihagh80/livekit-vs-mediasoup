package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeMedia is a media service that logs the join / closePeer calls in order.
// closePeer is slow, like a real one closing transports, so a reconnect that
// does not wait for it overlaps it.
type fakeMedia struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeMedia) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	json.NewDecoder(r.Body).Decode(&req)
	switch req.Method {
	case "join":
		f.log("join")
	case "closePeer":
		f.log("closePeer start")
		time.Sleep(200 * time.Millisecond)
		f.log("closePeer end")
	}
	w.Write([]byte(`{"ok":true,"data":{}}`))
}

func (f *fakeMedia) log(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeMedia) waitCalls(t *testing.T, n int) []string {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		f.mu.Lock()
		got := append([]string(nil), f.calls...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
	}
	t.Fatalf("media calls: want %d", n)
	return nil
}

func newTestServer(t *testing.T) (*fakeMedia, string) {
	media := &fakeMedia{}
	ms := httptest.NewServer(media)
	t.Cleanup(ms.Close)
	s := &Server{hub: NewHub(), media: NewMediaClient(ms.URL), consume: make(chan struct{}, 4)}
	ss := httptest.NewServer(http.HandlerFunc(s.handleWS))
	t.Cleanup(ss.Close)
	return media, "ws" + strings.TrimPrefix(ss.URL, "http") + "/ws?roomId=r1&peerId=p1"
}

// joinWS connects and makes the media service place the peer (the "join" RPC).
func joinWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteJSON(clientRequest{ID: 1, Method: "getRouterRtpCapabilities"}); err != nil {
		t.Fatal(err)
	}
	var resp serverResponse
	if err := c.ReadJSON(&resp); err != nil || !resp.OK {
		t.Fatalf("getRouterRtpCapabilities: %+v %v", resp, err)
	}
	return c
}

var wantOrder = []string{"join", "closePeer start", "closePeer end", "join"}

func checkOrder(t *testing.T, got []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("media calls %q, want %q: the new session must join after the old one is closed", got, wantOrder)
	}
}

// A client reconnects with the same peerId right after closing its socket.
func TestReconnectWaitsForClosePeer(t *testing.T) {
	media, url := newTestServer(t)
	c1 := joinWS(t, url)
	c1.Close()
	c2 := joinWS(t, url)
	defer c2.Close()
	checkOrder(t, media.waitCalls(t, 4))
}

// A client reconnects while the server still has its old socket open
// (the client lost the network): the old session is dropped.
func TestReconnectKicksOldSession(t *testing.T) {
	media, url := newTestServer(t)
	c1 := joinWS(t, url)
	defer c1.Close()
	c2 := joinWS(t, url)
	defer c2.Close()
	checkOrder(t, media.waitCalls(t, 4))
	c1.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c1.ReadMessage(); err == nil {
		t.Fatal("old socket still open")
	}
}
