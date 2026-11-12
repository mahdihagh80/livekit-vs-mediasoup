// Package media holds the pre-encoded media every simulated publisher sends,
// plus the timestamp trailer used to measure one-way (publisher -> SFU ->
// subscriber) delay.
package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	vp9hdr "github.com/pion/rtp/codecs/vp9"
	"github.com/pion/webrtc/v4/pkg/media/ivfreader"
)

// Layer is one simulcast encoding, loaded fully into memory and shared
// read-only by all publishers.
type Layer struct {
	RID        string // q / h / f (LiveKit convention: low, mid, high)
	Width      int
	Height     int
	BitrateBps int
	Frames     [][]byte
	Keyframes  []int // indexes of keyframes in Frames
	FrameDur   time.Duration
}

// NextKeyframe returns the first keyframe index strictly after i (wrapping).
func (l *Layer) NextKeyframe(i int) int {
	for _, k := range l.Keyframes {
		if k > i {
			return k
		}
	}
	return l.Keyframes[0]
}

// Video is the layer set (1..3), ordered low -> high.
// Codec "vp8" is sent as simulcast. Codec "vp9" is one SVC stream
// (L<n>T3_KEY): the same files, one RTP stream, each layer tagged with its
// SID and every picture with a temporal layer from the 0,2,1,2 pattern.
type Video struct {
	Codec  string
	Layers []*Layer
}

// LayerSpec describes one simulcast layer (bench.yaml loadgen.media.layers).
type LayerSpec struct {
	RID    string `yaml:"rid"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
	Kbps   int    `yaml:"kbps"`
}

// LoadVideo reads <dir>/<codec>-<height>.ivf for each layer (see scripts/make-media.sh).
// codec is "vp8" (default) or "vp9".
func LoadVideo(dir, codec string, specs []LayerSpec) (*Video, error) {
	if codec == "" {
		codec = "vp8"
	}
	if codec != "vp8" && codec != "vp9" {
		return nil, fmt.Errorf("loadgen.media.codec: want vp8 or vp9, got %q", codec)
	}
	if len(specs) < 1 || len(specs) > 3 {
		return nil, fmt.Errorf("loadgen.media.layers: need 1..3 layers, got %d", len(specs))
	}
	v := &Video{Codec: codec}
	for _, s := range specs {
		file := fmt.Sprintf("%s-%d.ivf", codec, s.Height)
		l, err := loadIVF(filepath.Join(dir, file), codec)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (run client/scripts/make-media.sh)", file, err)
		}
		l.RID, l.Width, l.Height, l.BitrateBps = s.RID, s.Width, s.Height, s.Kbps*1000
		if codec == "vp9" && len(v.Layers) > 0 && len(l.Frames) != len(v.Layers[0].Frames) {
			return nil, fmt.Errorf("%s: vp9 layers must have the same frame count", file)
		}
		v.Layers = append(v.Layers, l)
	}
	return v, nil
}

// SVC is true when the video is published as one VP9 spatial stream.
func (v *Video) SVC() bool { return v != nil && v.Codec == "vp9" }

// ScalabilityMode is the encoding mode of an SVC publish: L<n>T3_KEY, the
// shape of typical production WebRTC clients (see VP9SVC).
func (v *Video) ScalabilityMode() string {
	return fmt.Sprintf("L%dT%d_KEY", len(v.Layers), TemporalLayers)
}

// TotalBitrate is the sum of the layer bitrates, which is what one SVC stream carries.
func (v *Video) TotalBitrate() int {
	n := 0
	for _, l := range v.Layers {
		n += l.BitrateBps
	}
	return n
}

// AllKey reports whether every layer has a keyframe at index i.
func (v *Video) AllKey(i int) bool {
	for _, l := range v.Layers {
		if !keyAt(l, i) {
			return false
		}
	}
	return true
}

// NextKeyframe is the next index where every layer is a keyframe (wrapping).
func (v *Video) NextKeyframe(i int) int {
	base := v.Layers[0]
	for _, k := range base.Keyframes {
		if k > i && v.AllKey(k) {
			return k
		}
	}
	if v.AllKey(base.Keyframes[0]) {
		return base.Keyframes[0]
	}
	return base.NextKeyframe(i)
}

func keyAt(l *Layer, i int) bool {
	for _, k := range l.Keyframes {
		if k == i {
			return true
		}
	}
	return false
}

func loadIVF(path, codec string) (*Layer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, hdr, err := ivfreader.NewWith(f)
	if err != nil {
		return nil, err
	}
	l := &Layer{FrameDur: time.Second * time.Duration(hdr.TimebaseNumerator) / time.Duration(hdr.TimebaseDenominator)}
	if l.FrameDur <= 0 || l.FrameDur > time.Second {
		l.FrameDur = time.Second / 30
	}
	for {
		frame, _, err := r.ParseNextFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if isKeyframe(frame, codec) {
			l.Keyframes = append(l.Keyframes, len(l.Frames))
		}
		l.Frames = append(l.Frames, frame)
	}
	if len(l.Frames) == 0 || len(l.Keyframes) == 0 || l.Keyframes[0] != 0 {
		return nil, fmt.Errorf("%s: no frames or first frame is not a keyframe", path)
	}
	return l, nil
}

func isKeyframe(frame []byte, codec string) bool {
	if len(frame) == 0 {
		return false
	}
	if codec == "vp9" {
		var h vp9hdr.Header
		if err := h.Unmarshal(frame); err != nil {
			return false
		}
		return !h.ShowExistingFrame && !h.NonKeyFrame
	}
	// VP8 payload header: bit 0 of byte 0 is the inverse keyframe flag.
	return frame[0]&0x01 == 0
}

// ---- timestamp trailer -------------------------------------------------------
//
// The last 12 bytes of every video frame and every audio packet are
// "BNC" + tag + uint64 unix-nanoseconds at send time. SFUs forward the encoded
// frame untouched (they only rewrite the VP8/VP9 payload descriptor at the
// front), so the receiver can read the trailer and compute one-way delay. This
// needs chrony on all hosts. A decoder ignores the extra bytes at the end of the
// last partition, so a browser probe still shows the video.
//
// The tag says what was sent: 'H' for audio (and old senders), '0'+i for video
// layer i, so a subscriber can tell which simulcast/SVC layer it is getting.

const TrailerLen = 12

// TagAudio is the trailer tag of audio packets.
const TagAudio byte = 'H'

// LayerTag is the trailer tag of video layer i.
func LayerTag(i int) byte { return '0' + byte(i) }

// TagLayer returns the video layer of a tag, or -1.
func TagLayer(tag byte) int {
	if tag >= '0' && tag <= '9' {
		return int(tag - '0')
	}
	return -1
}

// AppendTrailer appends an audio-tagged trailer to buf and returns it.
func AppendTrailer(buf []byte, now time.Time) []byte {
	return AppendTrailerTag(buf, now, TagAudio)
}

// AppendTrailerTag appends the trailer with the given tag to buf (reusing its capacity).
func AppendTrailerTag(buf []byte, now time.Time, tag byte) []byte {
	buf = append(buf, 'B', 'N', 'C', tag)
	return binary.BigEndian.AppendUint64(buf, uint64(now.UnixNano()))
}

// ReadTrailer returns the send time if payload ends with a trailer.
func ReadTrailer(payload []byte) (time.Time, bool) {
	t, _, ok := ReadTrailerTag(payload)
	return t, ok
}

// ReadTrailerTag returns the send time and tag if payload ends with a trailer.
func ReadTrailerTag(payload []byte) (time.Time, byte, bool) {
	n := len(payload)
	if n < TrailerLen {
		return time.Time{}, 0, false
	}
	t := payload[n-TrailerLen:]
	if t[0] != 'B' || t[1] != 'N' || t[2] != 'C' {
		return time.Time{}, 0, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(t[4:]))), t[3], true
}

// ---- talk start ---------------------------------------------------------------
//
// An unmuted speaker puts "BNTS" + uint64 unix-nanoseconds of the moment it
// unmuted just before the trailer of every audio packet. The first packet a
// subscriber gets with a new value tells how long after the unmute it heard
// the speaker (the SFU may have paused forwarding while the track was silent).

const TalkStartLen = 12

// AppendTalkStart appends the talk-start block to buf and returns it.
func AppendTalkStart(buf []byte, at time.Time) []byte {
	buf = append(buf, 'B', 'N', 'T', 'S')
	return binary.BigEndian.AppendUint64(buf, uint64(at.UnixNano()))
}

// ReadTalkStart returns the unmute time if payload carries a talk-start block
// right before its trailer.
func ReadTalkStart(payload []byte) (time.Time, bool) {
	n := len(payload) - TrailerLen
	if n < TalkStartLen {
		return time.Time{}, false
	}
	t := payload[n-TalkStartLen : n]
	if t[0] != 'B' || t[1] != 'N' || t[2] != 'T' || t[3] != 'S' {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(t[4:]))), true
}

// OpusDTX is the body of a muted (DTX) Opus packet: a TOC byte only.
var OpusDTX = []byte{0xF8}

// ---- received keyframes ---------------------------------------------------------

// KeyframeStart reports whether an RTP payload is the first packet of a
// keyframe (VP8) or of a non-inter-predicted picture (VP9).
func KeyframeStart(codec string, payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	b := payload[0]
	if codec == "vp9" {
		// I P L F B E V Z: P = inter-picture predicted, B = start of frame.
		return b&0x40 == 0 && b&0x08 != 0
	}
	// VP8: X R N S R PID. Start of partition 0, then skip the extensions.
	if b&0x10 == 0 || b&0x07 != 0 {
		return false
	}
	i := 1
	if b&0x80 != 0 {
		if len(payload) <= i {
			return false
		}
		x := payload[i]
		i++
		if x&0x80 != 0 { // I: picture id
			if len(payload) <= i {
				return false
			}
			if payload[i]&0x80 != 0 {
				i += 2
			} else {
				i++
			}
		}
		if x&0x40 != 0 { // L: TL0PICIDX
			i++
		}
		if x&0x30 != 0 { // T or K
			i++
		}
	}
	return len(payload) > i && payload[i]&0x01 == 0
}

// OpusFrame is a synthetic 20 ms Opus packet body (about 32 kbps before the
// trailer). Receivers never decode it. The TOC byte says CELT fullband 20 ms,
// mono, 1 frame, so it parses as Opus; a browser probe would play noise.
var OpusFrame = func() []byte {
	b := make([]byte, 68)
	b[0] = 0xF8
	for i := 1; i < len(b); i++ {
		b[i] = byte(i*37 + 11)
	}
	return b
}()

const OpusFrameDur = 20 * time.Millisecond

// LayerFor maps a subscribed quality ("low" / "medium" / "high") to a layer
// index: high = top layer, medium = the one below it, low = layer 0. With
// fewer than 3 layers, medium and low share layers.
func (v *Video) LayerFor(quality string) int {
	n := len(v.Layers)
	switch quality {
	case "high":
		return n - 1
	case "medium":
		return max(n-2, 0)
	default:
		return 0
	}
}

// ExpectedKbps is the video bitrate a subscriber of the given quality should
// receive: that layer's bitrate for simulcast, the sum of the layers up to it
// for SVC (a spatial SVC subscriber also gets every lower layer).
func (v *Video) ExpectedKbps(quality string) float64 {
	return v.LayerKbps(v.LayerFor(quality))
}

// LayerKbps is what a subscriber gets when it receives layer idx: that layer's
// bitrate for simulcast, the sum up to it for SVC.
func (v *Video) LayerKbps(idx int) float64 {
	if !v.SVC() {
		return float64(v.Layers[idx].BitrateBps) / 1000
	}
	sum := 0
	for _, l := range v.Layers[:idx+1] {
		sum += l.BitrateBps
	}
	return float64(sum) / 1000
}
