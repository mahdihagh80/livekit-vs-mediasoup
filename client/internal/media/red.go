package media

import "time"

// ---- audio/red (RFC 2198) -----------------------------------------------------
//
// A RED payload carries the previous Opus frame as one redundant block in front
// of the current (primary) frame, like a browser's default "opus/red" with one
// level of redundancy:
//
//	1 PT(7) | timestamp offset(14) | block length(10)   redundant block header
//	0 PT(7)                                             primary block header
//	redundant data, primary data
//
// The primary frame is last, so the timestamp trailer still ends the payload
// and receivers read it without unpacking RED.

// REDOpusPT is the Opus payload type inside RED ("a=fmtp:63 111/111").
const REDOpusPT = 111

const (
	redMaxOffset = 1<<14 - 1 // timestamp offset field
	redMaxLen    = 1<<10 - 1 // block length field
)

// REDEncoder wraps the Opus frames of one track in RED. Not safe for concurrent use.
type REDEncoder struct {
	ClockRate uint32

	prev    []byte        // previous primary frame
	prevDur time.Duration // its duration = RTP timestamp distance to the next frame
	out     []byte
}

// Encode returns the RED payload for frame, which lasts dur. The result is only
// valid until the next call.
func (e *REDEncoder) Encode(frame []byte, dur time.Duration) []byte {
	out := e.out[:0]
	offset := uint64(e.prevDur) * uint64(e.ClockRate) / uint64(time.Second)
	if n := len(e.prev); n > 0 && n <= redMaxLen && offset > 0 && offset <= redMaxOffset {
		out = append(out, 0x80|REDOpusPT, byte(offset>>6), byte(offset<<2)|byte(n>>8), byte(n))
		out = append(out, REDOpusPT)
		out = append(out, e.prev...)
	} else {
		out = append(out, REDOpusPT)
	}
	out = append(out, frame...)
	e.prev = append(e.prev[:0], frame...)
	e.prevDur = dur
	e.out = out
	return out
}

// REDPrimary returns the primary block of a RED payload and the number of
// redundant blocks in front of it.
func REDPrimary(payload []byte) (primary []byte, redundant int, ok bool) {
	skip, i := 0, 0
	for {
		if i >= len(payload) {
			return nil, 0, false
		}
		if payload[i]&0x80 == 0 {
			i++
			break
		}
		if i+4 > len(payload) {
			return nil, 0, false
		}
		skip += int(payload[i+2]&0x03)<<8 | int(payload[i+3])
		redundant++
		i += 4
	}
	if i+skip > len(payload) {
		return nil, 0, false
	}
	return payload[i+skip:], redundant, true
}
