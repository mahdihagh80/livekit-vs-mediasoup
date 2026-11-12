package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"syscall"
	"time"
)

// MediaClient calls the Node media service: one HTTP POST per mediasoup operation.
type MediaClient struct {
	baseURL string
	http    *http.Client
}

func NewMediaClient(baseURL string) *MediaClient {
	tr := &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 1024, // join storms: avoid reconnecting per RPC
		IdleConnTimeout:     90 * time.Second,
	}
	return &MediaClient{baseURL: baseURL, http: &http.Client{Timeout: 15 * time.Second, Transport: tr}}
}

type rpcRequest struct {
	Method string `json:"method"`
	RoomID string `json:"roomId"`
	PeerID string `json:"peerId"`
	Data   any    `json:"data,omitempty"`
}

type rpcResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
}

func (m *MediaClient) Call(ctx context.Context, method, roomID, peerID string, data any) (json.RawMessage, error) {
	body, err := json.Marshal(rpcRequest{Method: method, RoomID: roomID, PeerID: peerID, Data: data})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := m.post(ctx, body)
	if err != nil && staleConn(err) && ctx.Err() == nil {
		// The reused keep-alive socket was closed by Node before the request
		// reached it: nothing was executed, so one retry on a new socket is safe.
		resp, err = m.post(ctx, body)
	}
	if err != nil {
		mMediaRPCSeconds.WithLabelValues(method, "false").Observe(time.Since(start).Seconds())
		return nil, fmt.Errorf("media service unreachable: %w", err)
	}
	defer resp.Body.Close()
	var out rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("bad media response: %w", err)
	}
	mMediaRPCSeconds.WithLabelValues(method, fmt.Sprint(out.OK)).Observe(time.Since(start).Seconds())
	if !out.OK {
		return nil, errors.New(out.Error)
	}
	return out.Data, nil
}

func (m *MediaClient) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return m.http.Do(req)
}

// staleConn reports a socket closed under us (reset/EOF), not a timeout.
func staleConn(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
