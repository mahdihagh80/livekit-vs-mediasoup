// Package stats defines the bench_* Prometheus metrics. They are identical
// for both SFUs, and the comparison is built on them.
package stats

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

// delayBuckets: histogram_quantile interpolates linearly inside a bucket, so
// wide buckets over the emulated delays inflate p95 (x13 cap2: mobile reported
// 94.5 ms for ~81, far 197 for ~180, capped 490 for ~310). Hence 10 ms steps
// over 50-200 ms (mobile 40-80, far 120-180 ms) and extra edges just above the
// 300 ms shaper queue limit (capped, dippy). Every older edge is kept, so CDF
// comparisons at those edges still work across runs.
var delayBuckets = []float64{
	.0005, .001, .002, .003, .005, .0075, .01, .015, .02, .025, .03, .04,
	.05, .06, .07, .075, .08, .09, .1, .11, .12, .13, .14, .15, .16, .17, .18, .19, .2,
	.225, .25, .275, .3, .31, .32, .34, .4, .5, .75, 1, 2, 5,
}

// Metrics is the process-wide Prometheus set. Rooms, participants, and
// publishers all record on this one object.
type Metrics struct {
	Info               *prometheus.GaugeVec
	Target             prometheus.Gauge
	Step               prometheus.Gauge
	Participants       *prometheus.GaugeVec
	JoinSeconds        *prometheus.HistogramVec
	TTFF               *prometheus.HistogramVec
	JoinFailures       *prometheus.CounterVec
	Disconnects        prometheus.Counter
	PubBytes           *prometheus.CounterVec
	PubFrames          *prometheus.CounterVec
	PubRTCP            *prometheus.CounterVec
	PubForcedKeyframes prometheus.Counter
	PubRTT             prometheus.Histogram
	SubTracks          *prometheus.GaugeVec
	SubPackets         *prometheus.CounterVec
	SubBytes           *prometheus.CounterVec
	SubLost            *prometheus.CounterVec
	SubOutOfOrder      *prometheus.CounterVec
	SubRTX             *prometheus.CounterVec
	SubRTXDup          *prometheus.CounterVec
	PCSRTP             *prometheus.CounterVec
	SubRTCPSent        *prometheus.CounterVec
	SubNackedPackets   prometheus.Counter
	SubDelay           *prometheus.HistogramVec
	SubJitter          *prometheus.HistogramVec
	SubFrames          *prometheus.CounterVec
	SubQualityTracks   *prometheus.GaugeVec
	SubQualityBytes    *prometheus.CounterVec
	SubFreezes         *prometheus.CounterVec
	SubFreezeSeconds   *prometheus.CounterVec
	SubLayerFrames     *prometheus.CounterVec
	UnmuteHeard        prometheus.Histogram

	// Per-track distributions: one observation per received track per
	// TrackWindow, so percentiles describe viewers, not packets.
	TrackFPS       *prometheus.HistogramVec
	TrackLoss      *prometheus.HistogramVec
	TrackKbpsRatio *prometheus.HistogramVec
	FreezeLength   *prometheus.HistogramVec
	SendLag        *prometheus.HistogramVec

	// Lifecycle: rejoins, scripted events and how long media took to come back.
	Rejoins         *prometheus.CounterVec
	Events          *prometheus.CounterVec
	Recovery        *prometheus.HistogramVec
	RecoveryMissed  *prometheus.CounterVec
	LayerSwitch     *prometheus.HistogramVec
	LayerSwitchMiss *prometheus.CounterVec
}

// TrackWindow is the window of the per-track distributions (bench_sub_track_*).
const TrackWindow = 5 * time.Second

var recoveryBuckets = []float64{.1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60}

// RejoinReasons label bench_rejoins_total.
var RejoinReasons = []string{"churn", "drop", "mass_rejoin", "publisher_reconnect", "sdk_resume"}

// M is the metrics every part of the process uses.
var M = newMetrics()

func newMetrics() *Metrics {
	m := &Metrics{
		Info: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bench_info", Help: "1 while a run is active; labels identify it"},
			[]string{"sfu", "scenario", "run"}),
		Target: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "bench_target_participants", Help: "Participants the current step should have connected"}),
		Step: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "bench_step", Help: "Current ramp step number (0 = idle)"}),
		Participants: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bench_participants", Help: "Participants by state"}, []string{"state"}),

		JoinSeconds: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_join_duration_seconds", Help: "Join start -> connected (signaling + ICE + DTLS + publish)",
			Buckets: []float64{.05, .1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10, 20}}, []string{"result"}),
		TTFF: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_ttff_seconds", Help: "Join start -> first complete video frame received",
			Buckets: []float64{.1, .2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10, 20}}, []string{"profile"}),
		JoinFailures: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_join_failures_total", Help: "Failed joins by stage"}, []string{"stage"}),
		Disconnects: promauto.NewCounter(prometheus.CounterOpts{
			Name: "bench_disconnects_total", Help: "Connected participants that dropped unexpectedly"}),

		PubBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_pub_bytes_total", Help: "Payload bytes handed to the publisher stack"}, []string{"kind"}),
		PubFrames: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_pub_frames_total", Help: "Frames/packets written by publishers"}, []string{"kind"}),
		PubRTCP: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_pub_rtcp_total", Help: "RTCP received by publishers, by type"}, []string{"type"}),
		PubForcedKeyframes: promauto.NewCounter(prometheus.CounterOpts{
			Name: "bench_pub_forced_keyframes_total", Help: "Keyframes sent in response to PLI/FIR"}),
		PubRTT: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "bench_pub_rtt_seconds", Help: "Publisher<->SFU RTT from the SFU's receiver reports (LSR/DLSR)",
			Buckets: []float64{.001, .002, .005, .01, .015, .02, .03, .05, .075, .1, .15, .2, .3, .5, 1, 2}}),

		SubTracks: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bench_sub_tracks", Help: "Subscribed tracks currently receiving"}, []string{"kind", "profile"}),
		SubPackets: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_packets_total", Help: "RTP packets received by subscribers"}, []string{"kind", "profile"}),
		SubBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_bytes_total", Help: "RTP payload bytes received by subscribers"}, []string{"kind"}),
		SubLost: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_lost_packets_total", Help: "Sequence-number gaps not filled (RFC3550 cumulative lost)"}, []string{"kind", "profile"}),
		SubOutOfOrder: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_out_of_order_packets_total", Help: "Non-RTX packets that arrived after a higher sequence number"}, []string{"kind"}),
		SubRTX: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_rtx_packets_total", Help: "Retransmissions received on the RTX stream (included in bench_sub_packets_total)"}, []string{"kind"}),
		SubRTXDup: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_rtx_duplicate_packets_total", Help: "RTX packets for a sequence number already received: resent more often than needed"}, []string{"profile"}),
		PCSRTP: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_pc_srtp_total", Help: "PeerConnections by negotiated SRTP protection profile, counted once each after connecting"}, []string{"sfu", "cipher"}),
		SubRTCPSent: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_rtcp_sent_total", Help: "Feedback RTCP sent by subscribers, by type"}, []string{"type"}),
		SubNackedPackets: promauto.NewCounter(prometheus.CounterOpts{
			Name: "bench_sub_nack_requested_packets_total", Help: "Sequence numbers requested in sent NACKs"}),
		SubDelay: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_e2e_delay_seconds", Help: "One-way publisher->subscriber delay from the payload trailer (sampled)",
			Buckets: delayBuckets}, []string{"kind", "profile"}),
		SubJitter: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_jitter_seconds", Help: "RFC3550 interarrival jitter, one observation per track per second",
			Buckets: []float64{.0005, .001, .002, .005, .01, .015, .02, .03, .05, .075, .1, .2, .5}}, []string{"kind", "profile"}),
		SubQualityTracks: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "bench_sub_video_tracks_by_quality", Help: "Subscribed video tracks currently receiving, by requested layer"}, []string{"quality", "profile"}),
		SubQualityBytes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_video_bytes_by_quality_total", Help: "Video RTP payload bytes received, by requested layer"}, []string{"quality", "profile"}),
		SubFrames: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_frames_total", Help: "Complete video frames received (marker bit)"}, []string{"profile"}),
		SubFreezes: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_freezes_total", Help: "Video freezes: inter-frame gap > freeze threshold"}, []string{"profile"}),
		SubFreezeSeconds: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_freeze_seconds_total", Help: "Total time spent frozen (sum of gaps above threshold)"}, []string{"profile"}),
		SubLayerFrames: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_sub_layer_frames_total", Help: "Video frames received, by delivered layer vs the requested one"},
			[]string{"profile", "delivered"}),
		UnmuteHeard: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "bench_unmute_heard_seconds", Help: "Speaker unmute -> first audio packet of that talk spurt received",
			Buckets: delayBuckets}),

		TrackFPS: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_track_fps", Help: "Video frames/s per subscribed track, one observation per track per 5 s",
			Buckets: []float64{0, 5, 10, 15, 20, 25, 28, 29.5, 31}}, []string{"profile"}),
		TrackLoss: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_track_loss_ratio", Help: "Lost/expected packets per subscribed track, one observation per track per 5 s",
			Buckets: []float64{0, .001, .005, .01, .02, .05, .1, .2, .5, 1}}, []string{"kind", "profile"}),
		TrackKbpsRatio: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_track_kbps_ratio", Help: "Received video kbps / requested layer kbps per track, one observation per track per 5 s",
			Buckets: []float64{.1, .25, .5, .65, .8, .9, 1, 1.1, 1.5, 2}}, []string{"profile", "quality"}),
		FreezeLength: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_sub_freeze_duration_seconds", Help: "Length of each video freeze (inter-frame gap above the threshold)",
			Buckets: []float64{.2, .3, .5, .75, 1, 1.5, 2, 3, 5, 10, 30}}, []string{"profile"}),
		SendLag: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_gen_send_lag_seconds", Help: "Publisher frame tick -> write, sampled. High = the generator is late, results not trustworthy",
			Buckets: []float64{.0001, .0005, .001, .002, .005, .01, .02, .05, .1, .2, .5}}, []string{"kind"}),

		Rejoins: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_rejoins_total", Help: "Participants that left and joined again, by reason"}, []string{"reason"}),
		Events: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_events_total", Help: "Scripted plan events fired on this generator"}, []string{"type"}),
		Recovery: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_recovery_seconds", Help: "Disturbance -> media flowing again (video: first keyframe; audio: first packet)",
			Buckets: recoveryBuckets}, []string{"event"}),
		RecoveryMissed: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_recovery_missed_total", Help: "Disturbances with no media within 60 s"}, []string{"event"}),
		LayerSwitch: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name: "bench_layer_switch_seconds", Help: "Bandwidth dip start -> lower layer delivered (down), dip end -> the layer from before the dip back (up)",
			Buckets: recoveryBuckets}, []string{"direction"}),
		LayerSwitchMiss: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "bench_layer_switch_missed_total", Help: "Dips the SFU did not adapt to (down) or did not recover from within 60 s (up)"},
			[]string{"direction"}),
	}

	// Create every labelled series at 0 on startup. Prometheus' increase() cannot
	// see the first increment of a series that appears mid-window, so without this
	// the first joins/NACKs of a run would be missing from the report.
	for _, r := range []string{"ok", "fail"} {
		m.JoinSeconds.WithLabelValues(r)
	}
	for _, s := range []string{"signal", "publish", "consume"} {
		m.JoinFailures.WithLabelValues(s)
	}
	for _, st := range []string{"joining", "connected", "reconnecting", "failed", "disconnected"} {
		m.Participants.WithLabelValues(st)
	}
	for _, k := range []string{"audio", "video"} {
		m.PubBytes.WithLabelValues(k)
		m.PubFrames.WithLabelValues(k)
		m.SubBytes.WithLabelValues(k)
		m.SubOutOfOrder.WithLabelValues(k)
		m.SubRTX.WithLabelValues(k)
		m.SendLag.WithLabelValues(k)
	}
	m.InitProfiles([]string{"clean"})
	for _, r := range RejoinReasons {
		m.Rejoins.WithLabelValues(r)
	}
	for _, d := range []string{"down", "up"} {
		m.LayerSwitch.WithLabelValues(d)
		m.LayerSwitchMiss.WithLabelValues(d)
	}
	for _, t := range []string{"pli", "fir", "nack", "remb", "twcc", "rr", "other"} {
		m.PubRTCP.WithLabelValues(t)
	}
	for _, t := range []string{"nack", "pli", "fir"} {
		m.SubRTCPSent.WithLabelValues(t)
	}
	return m
}

// InitProfiles creates the per-profile series at 0 (see newMetrics).
func (m *Metrics) InitProfiles(profiles []string) {
	for _, p := range profiles {
		m.TTFF.WithLabelValues(p)
		m.SubFrames.WithLabelValues(p)
		m.SubFreezes.WithLabelValues(p)
		m.SubFreezeSeconds.WithLabelValues(p)
		m.TrackFPS.WithLabelValues(p)
		m.FreezeLength.WithLabelValues(p)
		for _, q := range []string{"low", "medium", "high"} {
			m.TrackKbpsRatio.WithLabelValues(p, q)
			m.SubQualityTracks.WithLabelValues(q, p)
			m.SubQualityBytes.WithLabelValues(q, p)
		}
		for _, d := range []string{"requested", "lower", "higher"} {
			m.SubLayerFrames.WithLabelValues(p, d)
		}
		for _, k := range []string{"audio", "video"} {
			m.SubTracks.WithLabelValues(k, p)
			m.SubPackets.WithLabelValues(k, p)
			m.SubLost.WithLabelValues(k, p)
			m.SubDelay.WithLabelValues(k, p)
			m.SubJitter.WithLabelValues(k, p)
			m.TrackLoss.WithLabelValues(k, p)
		}
	}
}

// InitEvents creates the event series at 0 for the event labels a plan uses.
func (m *Metrics) InitEvents(types, recoveryEvents []string) {
	for _, t := range types {
		m.Events.WithLabelValues(t)
	}
	for _, e := range recoveryEvents {
		m.Recovery.WithLabelValues(e)
		m.RecoveryMissed.WithLabelValues(e)
	}
}

// Count reads one bench_participants state.
func (m *Metrics) Count(state string) float64 {
	var metric dto.Metric
	if err := m.Participants.WithLabelValues(state).Write(&metric); err != nil || metric.Gauge == nil {
		return 0
	}
	return metric.Gauge.GetValue()
}
