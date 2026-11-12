package client

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
)

// mediasoup has no SDP. Like mediasoup-client's RemoteSdp, we build the
// "remote" side of each PeerConnection from the transport's ICE/DTLS
// parameters (send side: an answer to Pion's offer; recv side: an offer
// describing the consumers).

const (
	uriMid     = "urn:ietf:params:rtp-hdrext:sdes:mid"
	uriRID     = "urn:ietf:params:rtp-hdrext:sdes:rtp-stream-id"
	uriRRID    = "urn:ietf:params:rtp-hdrext:sdes:repaired-rtp-stream-id"
	uriAbsSend = "http://www.webrtc.org/experiments/rtp-hdrext/abs-send-time"
	uriTWCC    = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
	sdpFPAlgo  = "sha-256"
)

var supportedExt = map[string]bool{uriMid: true, uriRID: true, uriRRID: true, uriAbsSend: true, uriTWCC: true}

func transportLines(b *strings.Builder, t *msTransport, setup string) {
	fmt.Fprintf(b, "a=ice-ufrag:%s\r\n", t.IceParameters.UsernameFragment)
	fmt.Fprintf(b, "a=ice-pwd:%s\r\n", t.IceParameters.Password)
	for _, c := range t.IceCandidates {
		ip := c.Address
		if ip == "" {
			ip = c.IP
		}
		line := fmt.Sprintf("a=candidate:%s 1 %s %d %s %d typ %s", c.Foundation, c.Protocol, c.Priority, ip, c.Port, c.Type)
		if c.Protocol == "tcp" {
			line += " tcptype passive"
		}
		b.WriteString(line + "\r\n")
	}
	b.WriteString("a=end-of-candidates\r\n")
	fmt.Fprintf(b, "a=setup:%s\r\n", setup)
	b.WriteString("a=rtcp-mux\r\na=rtcp-rsize\r\n")
}

func sessionHeader(b *strings.Builder, t *msTransport, version int, mids []string) error {
	var fp string
	for _, f := range t.DtlsParameters.Fingerprints {
		if f.Algorithm == sdpFPAlgo {
			fp = f.Value
		}
	}
	if fp == "" {
		return fmt.Errorf("transport has no %s fingerprint", sdpFPAlgo)
	}
	b.WriteString("v=0\r\n")
	fmt.Fprintf(b, "o=mediasoup 1000 %d IN IP4 0.0.0.0\r\n", version)
	b.WriteString("s=-\r\nt=0 0\r\na=ice-lite\r\n")
	fmt.Fprintf(b, "a=group:BUNDLE %s\r\n", strings.Join(mids, " "))
	b.WriteString("a=msid-semantic: WMS *\r\n")
	fmt.Fprintf(b, "a=fingerprint:%s %s\r\n", sdpFPAlgo, fp)
	return nil
}

// ---- send side ------------------------------------------------------------------

// offerMedia is what we need from one m-section of Pion's local offer.
type offerMedia struct {
	Kind   string
	Mid    string
	PTs    []string          // payload types to keep (opus / VP8 / rtx)
	Attrs  []string          // rtpmap/fmtp/rtcp-fb lines for those PTs
	Ext    map[string]int    // uri -> id
	RIDs   []string          // simulcast rids, in offer order
	Codecs map[string]string // pt -> "VP8/90000"
	CNAME  string
}

func parseOffer(raw string) ([]offerMedia, error) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal([]byte(raw)); err != nil {
		return nil, err
	}
	var out []offerMedia
	for _, md := range sd.MediaDescriptions {
		om := offerMedia{Kind: md.MediaName.Media, Ext: map[string]int{}, Codecs: map[string]string{}}
		keep := map[string]bool{}
		for _, a := range md.Attributes {
			if a.Key == "rtpmap" {
				pt, codec, _ := strings.Cut(a.Value, " ")
				lc := strings.ToLower(codec)
				if strings.HasPrefix(lc, "opus/") || strings.HasPrefix(lc, "vp8/") || strings.HasPrefix(lc, "vp9/") || strings.HasPrefix(lc, "rtx/") {
					keep[pt] = true
					om.Codecs[pt] = codec
				}
			}
		}
		for _, pt := range md.MediaName.Formats {
			if keep[pt] {
				om.PTs = append(om.PTs, pt)
			}
		}
		for _, a := range md.Attributes {
			switch a.Key {
			case "mid":
				om.Mid = a.Value
			case "rtpmap", "fmtp", "rtcp-fb":
				pt, _, _ := strings.Cut(a.Value, " ")
				if keep[pt] {
					om.Attrs = append(om.Attrs, "a="+a.Key+":"+a.Value)
				}
			case "extmap":
				idDir, uri, _ := strings.Cut(a.Value, " ")
				idStr, _, _ := strings.Cut(idDir, "/")
				id, _ := strconv.Atoi(idStr)
				uri = strings.Fields(uri)[0]
				if supportedExt[uri] {
					om.Ext[uri] = id
				}
			case "rid":
				rid, dir, _ := strings.Cut(a.Value, " ")
				if strings.HasPrefix(dir, "send") {
					om.RIDs = append(om.RIDs, rid)
				}
			case "ssrc":
				if _, rest, ok := strings.Cut(a.Value, " "); ok && strings.HasPrefix(rest, "cname:") {
					om.CNAME = strings.TrimPrefix(rest, "cname:")
				}
			}
		}
		out = append(out, om)
	}
	return out, nil
}

func buildSendAnswer(t *msTransport, offer []offerMedia, version int) (string, error) {
	var b strings.Builder
	var mids []string
	for _, m := range offer {
		mids = append(mids, m.Mid)
	}
	if err := sessionHeader(&b, t, version, mids); err != nil {
		return "", err
	}
	for _, m := range offer {
		fmt.Fprintf(&b, "m=%s 7 UDP/TLS/RTP/SAVPF %s\r\n", m.Kind, strings.Join(m.PTs, " "))
		b.WriteString("c=IN IP4 127.0.0.1\r\n")
		fmt.Fprintf(&b, "a=mid:%s\r\n", m.Mid)
		b.WriteString("a=recvonly\r\n")
		transportLines(&b, t, "passive") // mediasoup = DTLS server, Pion = client
		for _, a := range m.Attrs {
			b.WriteString(a + "\r\n")
		}
		uris := make([]string, 0, len(m.Ext))
		for u := range m.Ext {
			uris = append(uris, u)
		}
		sort.Strings(uris)
		for _, u := range uris {
			fmt.Fprintf(&b, "a=extmap:%d %s\r\n", m.Ext[u], u)
		}
		if len(m.RIDs) > 0 {
			for _, r := range m.RIDs {
				fmt.Fprintf(&b, "a=rid:%s recv\r\n", r)
			}
			fmt.Fprintf(&b, "a=simulcast:recv %s\r\n", strings.Join(m.RIDs, ";"))
		}
	}
	return b.String(), nil
}

// filterVideo keeps one video codec and the RTX that repairs it.
func filterVideo(m offerMedia, mime string) (offerMedia, bool) {
	var pt string
	for _, p := range m.PTs {
		if strings.HasPrefix(strings.ToLower(m.Codecs[p]), mime+"/") {
			pt = p
			break
		}
	}
	if pt == "" {
		return m, false
	}
	keep := map[string]bool{pt: true}
	for _, p := range m.PTs {
		if !strings.HasPrefix(strings.ToLower(m.Codecs[p]), "rtx/") {
			continue
		}
		if aptOf(m, p) == pt {
			keep[p] = true
		}
	}
	out := m
	out.PTs = nil
	out.Codecs = map[string]string{}
	out.Attrs = nil
	for _, p := range m.PTs {
		if keep[p] {
			out.PTs = append(out.PTs, p)
			out.Codecs[p] = m.Codecs[p]
		}
	}
	for _, a := range m.Attrs {
		rest := a
		if i := strings.Index(rest, ":"); i >= 0 {
			rest = rest[i+1:]
		}
		p, _, _ := strings.Cut(rest, " ")
		if keep[p] {
			out.Attrs = append(out.Attrs, a)
		}
	}
	return out, true
}

func aptOf(m offerMedia, rtxPT string) string {
	prefix := "a=fmtp:" + rtxPT + " "
	for _, a := range m.Attrs {
		if !strings.HasPrefix(a, prefix) {
			continue
		}
		for _, kv := range strings.Split(strings.TrimPrefix(a, prefix), ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if ok && k == "apt" {
				return v
			}
		}
	}
	return ""
}

// producerParams converts one offer m-section plus the sender's SSRCs into
// mediasoup Producer rtpParameters.
func producerParams(m offerMedia, encodings []msEncoding, cname string) msRtpParameters {
	p := msRtpParameters{Mid: m.Mid, Encodings: encodings, Rtcp: msRtcp{CNAME: cname, ReducedSize: true}}
	fb := map[string][]msFeedback{}
	fmtp := map[string]map[string]any{}
	for _, a := range m.Attrs {
		key, val, _ := strings.Cut(strings.TrimPrefix(a, "a="), ":")
		pt, rest, _ := strings.Cut(val, " ")
		switch key {
		case "rtcp-fb":
			typ, param, _ := strings.Cut(rest, " ")
			fb[pt] = append(fb[pt], msFeedback{Type: typ, Parameter: param})
		case "fmtp":
			params := map[string]any{}
			for _, kv := range strings.Split(rest, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
				if !ok {
					continue
				}
				if n, err := strconv.Atoi(v); err == nil {
					params[k] = n
				} else {
					params[k] = v
				}
			}
			fmtp[pt] = params
		}
	}
	for _, pt := range m.PTs {
		codec := m.Codecs[pt]
		parts := strings.Split(codec, "/")
		c := msCodec{MimeType: m.Kind + "/" + parts[0], ClockRate: atoi(parts[1]), Parameters: fmtp[pt], RtcpFeedback: fb[pt]}
		c.PayloadType = atoi(pt)
		if len(parts) > 2 {
			c.Channels = atoi(parts[2])
		}
		if c.Parameters == nil {
			c.Parameters = map[string]any{}
		}
		if c.RtcpFeedback == nil {
			c.RtcpFeedback = []msFeedback{}
		}
		p.Codecs = append(p.Codecs, c)
	}
	for uri, id := range m.Ext {
		p.HeaderExtensions = append(p.HeaderExtensions, msHeaderExt{URI: uri, ID: id})
	}
	return p
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// ---- recv side ------------------------------------------------------------------

func buildRecvOffer(t *msTransport, consumers []msNewConsumer, version int) (string, error) {
	var b strings.Builder
	var mids []string
	for _, c := range consumers {
		mids = append(mids, c.RtpParameters.Mid)
	}
	if err := sessionHeader(&b, t, version, mids); err != nil {
		return "", err
	}
	for _, c := range consumers {
		rp := c.RtpParameters
		var pts []string
		for _, codec := range rp.Codecs {
			pts = append(pts, strconv.Itoa(codec.PayloadType))
		}
		fmt.Fprintf(&b, "m=%s 7 UDP/TLS/RTP/SAVPF %s\r\n", c.Kind, strings.Join(pts, " "))
		b.WriteString("c=IN IP4 127.0.0.1\r\n")
		fmt.Fprintf(&b, "a=mid:%s\r\n", rp.Mid)
		b.WriteString("a=sendonly\r\n")
		transportLines(&b, t, "actpass")
		for _, codec := range rp.Codecs {
			name := strings.SplitN(codec.MimeType, "/", 2)[1]
			if codec.Channels > 1 {
				fmt.Fprintf(&b, "a=rtpmap:%d %s/%d/%d\r\n", codec.PayloadType, name, codec.ClockRate, codec.Channels)
			} else {
				fmt.Fprintf(&b, "a=rtpmap:%d %s/%d\r\n", codec.PayloadType, name, codec.ClockRate)
			}
			if len(codec.Parameters) > 0 {
				keys := make([]string, 0, len(codec.Parameters))
				for k := range codec.Parameters {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var kv []string
				for _, k := range keys {
					kv = append(kv, fmt.Sprintf("%s=%v", k, codec.Parameters[k]))
				}
				fmt.Fprintf(&b, "a=fmtp:%d %s\r\n", codec.PayloadType, strings.Join(kv, ";"))
			}
			for _, fb := range codec.RtcpFeedback {
				if fb.Parameter != "" {
					fmt.Fprintf(&b, "a=rtcp-fb:%d %s %s\r\n", codec.PayloadType, fb.Type, fb.Parameter)
				} else {
					fmt.Fprintf(&b, "a=rtcp-fb:%d %s\r\n", codec.PayloadType, fb.Type)
				}
			}
		}
		for _, e := range rp.HeaderExtensions {
			fmt.Fprintf(&b, "a=extmap:%d %s\r\n", e.ID, e.URI)
		}
		stream, track := c.ProducerPeerID, c.ID
		fmt.Fprintf(&b, "a=msid:%s %s\r\n", stream, track)
		if len(rp.Encodings) > 0 {
			enc := rp.Encodings[0]
			if enc.RTX != nil {
				fmt.Fprintf(&b, "a=ssrc-group:FID %d %d\r\n", enc.SSRC, enc.RTX.SSRC)
			}
			fmt.Fprintf(&b, "a=ssrc:%d cname:%s\r\n", enc.SSRC, rp.Rtcp.CNAME)
			fmt.Fprintf(&b, "a=ssrc:%d msid:%s %s\r\n", enc.SSRC, stream, track)
			if enc.RTX != nil {
				fmt.Fprintf(&b, "a=ssrc:%d cname:%s\r\n", enc.RTX.SSRC, rp.Rtcp.CNAME)
				fmt.Fprintf(&b, "a=ssrc:%d msid:%s %s\r\n", enc.RTX.SSRC, stream, track)
			}
		}
	}
	return b.String(), nil
}

// localFingerprint extracts the sha-256 fingerprint from Pion's local SDP.
func localFingerprint(raw string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a=fingerprint:sha-256 ") {
			return strings.TrimPrefix(line, "a=fingerprint:sha-256 "), nil
		}
	}
	return "", fmt.Errorf("no sha-256 fingerprint in local SDP")
}
