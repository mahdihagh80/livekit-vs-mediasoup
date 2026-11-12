package media

import "github.com/pion/rtp"

// VP9SVC packetizes the pre-encoded layers of one picture as a VP9 SVC
// stream in L3T3_KEY shape (what typical production WebRTC clients publish): three
// spatial layers in one RTP stream, non-flexible mode, three temporal layers.
//
// The files hold one independently encoded stream per spatial layer, so the
// SVC structure is only in the payload descriptor, exactly what an SFU acts
// on; receivers never decode. Per picture:
//
//   - TID follows the libvpx 3-layer pattern 0, 2, 1, 2 (period 4) and every
//     picture is a switching-up point (U = 1), since no enhancement-layer
//     frame references a frame of its own temporal layer.
//   - TL0PICIDX advances on TID 0 pictures only.
//   - KEY mode: a key picture is intra-coded on SID 0 and inter-layer
//     predicted (D = 1) on the upper spatial layers, with P = 0 on all of
//     them, so an SFU can switch a subscriber up to any spatial layer there
//     (LiveKit's selector wants !P && B on the target SID). Non-key pictures
//     carry P = 1 and D = 0: no inter-layer prediction between key frames.
//   - The scalability structure (first packet of SID 0 of a key picture)
//     lists the three resolutions and the four-picture temporal group.
type VP9SVC struct {
	widths    []uint16
	heights   []uint16
	mtu       int
	pictureID uint16
	tl0       uint8
	tid       uint8
	seq       uint16
}

// temporalPattern is libvpx's 3-layer temporal layering (ts_layer_id=0,2,1,2).
var temporalPattern = [4]uint8{0, 2, 1, 2}

// TemporalLayers is the number of temporal layers of the pattern (T3).
const TemporalLayers = 3

// TemporalID returns the temporal layer of picture i.
func TemporalID(i int) uint8 { return temporalPattern[i%len(temporalPattern)] }

func NewVP9SVC(layers []*Layer) *VP9SVC {
	s := &VP9SVC{mtu: 1200, seq: 1}
	for _, l := range layers {
		s.widths = append(s.widths, uint16(l.Width))
		s.heights = append(s.heights, uint16(l.Height))
	}
	return s
}

// BeginPicture advances the picture id and sets the temporal layer of the
// picture about to be packetized. Call it once per temporal unit, before the
// spatial layers; TL0PICIDX advances on TID 0 pictures only.
func (s *VP9SVC) BeginPicture(tid uint8) {
	s.pictureID = (s.pictureID + 1) & 0x7FFF
	s.tid = tid
	if tid == 0 {
		s.tl0++
	}
}

// TID is the temporal layer of the current picture.
func (s *VP9SVC) TID() uint8 { return s.tid }

func (s *VP9SVC) Packetize(frame []byte, sid int, key bool, timestamp uint32) []*rtp.Packet {
	if len(frame) == 0 || sid < 0 || sid >= len(s.widths) {
		return nil
	}
	var pkts []*rtp.Packet
	off := 0
	first := true
	for off < len(frame) {
		withSS := first && key && sid == 0
		hdrLen := 5
		if withSS {
			hdrLen += 1 + 4*len(s.widths) + 1 + len(temporalPattern)*2
		}
		if hdrLen >= s.mtu {
			return pkts
		}
		frag := len(frame) - off
		if frag > s.mtu-hdrLen {
			frag = s.mtu - hdrLen
		}
		last := off+frag == len(frame)
		payload := make([]byte, hdrLen+frag)
		s.writeDescriptor(payload[:hdrLen], sid, key, first, last, withSS)
		copy(payload[hdrLen:], frame[off:off+frag])
		pkts = append(pkts, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Marker:         last,
				SequenceNumber: s.seq,
				Timestamp:      timestamp,
			},
			Payload: payload,
		})
		s.seq++
		off += frag
		first = false
	}
	return pkts
}

func (s *VP9SVC) writeDescriptor(b []byte, sid int, key, begin, end, withSS bool) {
	var b0 byte = 0x80 | 0x20 | 0x01 // I, L, Z
	if !key {
		b0 |= 0x40 // P: inter-picture predicted
	}
	if begin {
		b0 |= 0x08 // B
	}
	if end {
		b0 |= 0x04 // E
	}
	if withSS {
		b0 |= 0x02 // V
	}
	b[0] = b0
	b[1] = byte(s.pictureID>>8) | 0x80
	b[2] = byte(s.pictureID)
	// T (3 bits), U, S (3 bits), D
	b[3] = s.tid<<5 | 0x10 | byte(sid)<<1
	if key && sid > 0 {
		b[3] |= 0x01 // D: predicted from the lower spatial layer (KEY mode)
	}
	b[4] = s.tl0
	if !withSS {
		return
	}
	i := 5
	b[i] = byte((len(s.widths)-1)<<5) | 0x10 | 0x08 // N_S, Y=1, G=1
	i++
	for k, w := range s.widths {
		h := s.heights[k]
		b[i] = byte(w >> 8)
		i++
		b[i] = byte(w)
		i++
		b[i] = byte(h >> 8)
		i++
		b[i] = byte(h)
		i++
	}
	b[i] = byte(len(temporalPattern)) // N_G
	i++
	// Picture group: T, U, R = 1 and the P_DIFF to the reference picture.
	// 0 references the previous TID 0 (4 back); 2 the picture before it;
	// 1 the TID 0 two back; the last 2 the TID 1 before it.
	pdiff := [4]byte{4, 1, 2, 1}
	for g, t := range temporalPattern {
		b[i] = t<<5 | 0x10 | 0x04 // T, U=1, R=1
		i++
		b[i] = pdiff[g]
		i++
	}
}
