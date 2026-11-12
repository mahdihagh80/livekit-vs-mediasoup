package client

import (
	"github.com/pion/rtcp"

	"sfubench/loadgen/internal/stats"
)

// handleRTCP counts RTCP packets arriving at a publisher and turns PLI/FIR into
// a keyframe request. NACKs are answered by Pion's NACK responder interceptor.
func handleRTCP(p rtcp.Packet, kf *keyframeRequest) {
	switch p.(type) {
	case *rtcp.PictureLossIndication:
		stats.M.PubRTCP.WithLabelValues("pli").Inc()
		if kf != nil {
			kf.request()
		}
	case *rtcp.FullIntraRequest:
		stats.M.PubRTCP.WithLabelValues("fir").Inc()
		if kf != nil {
			kf.request()
		}
	case *rtcp.TransportLayerNack:
		stats.M.PubRTCP.WithLabelValues("nack").Inc()
	case *rtcp.ReceiverEstimatedMaximumBitrate:
		stats.M.PubRTCP.WithLabelValues("remb").Inc()
	case *rtcp.TransportLayerCC:
		stats.M.PubRTCP.WithLabelValues("twcc").Inc()
	case *rtcp.ReceiverReport:
		stats.M.PubRTCP.WithLabelValues("rr").Inc()
	default:
		stats.M.PubRTCP.WithLabelValues("other").Inc()
	}
}
