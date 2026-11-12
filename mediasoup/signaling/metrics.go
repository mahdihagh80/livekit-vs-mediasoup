package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var buckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

var (
	mWSConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mssig_ws_connections", Help: "Open WebSocket connections"})
	mRooms = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mssig_rooms", Help: "Rooms with at least one peer"})
	mRequestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "mssig_request_duration_seconds", Help: "Client request latency (incl. media RPC)", Buckets: buckets},
		[]string{"method", "ok"})
	mMediaRPCSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "mssig_media_rpc_duration_seconds", Help: "Signaling -> media service RPC latency", Buckets: buckets},
		[]string{"method", "ok"})
	mConsumes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mssig_server_consumes_total", Help: "Server-driven consume calls"}, []string{"ok"})
	mDroppedMessages = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mssig_dropped_messages_total", Help: "Messages dropped because a client's send queue was full"})
)
