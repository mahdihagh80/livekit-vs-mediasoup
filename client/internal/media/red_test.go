package media

import (
	"bytes"
	"testing"
	"time"
)

func TestREDEncoder(t *testing.T) {
	e := &REDEncoder{ClockRate: 48000}
	now := time.Unix(1700000000, 123)
	f1 := AppendTrailer(append([]byte{}, OpusFrame...), now)
	f2 := AppendTrailer(append([]byte{}, OpusFrame...), now.Add(20*time.Millisecond))
	f2[5] ^= 0xff

	// First packet: primary only.
	p1 := append([]byte{}, e.Encode(f1, OpusFrameDur)...)
	if len(p1) != 1+len(f1) || p1[0] != REDOpusPT {
		t.Fatalf("first packet: len %d header %#x", len(p1), p1[0])
	}
	prim, n, ok := REDPrimary(p1)
	if !ok || n != 0 || !bytes.Equal(prim, f1) {
		t.Fatalf("first packet primary: ok=%v redundant=%d", ok, n)
	}

	// Second packet: f1 as the redundant block, 960 samples back, then f2.
	p2 := append([]byte{}, e.Encode(f2, OpusFrameDur)...)
	if len(p2) != 5+len(f1)+len(f2) {
		t.Fatalf("second packet: len %d", len(p2))
	}
	if p2[0] != 0x80|REDOpusPT || p2[4] != REDOpusPT {
		t.Fatalf("second packet headers: %#x %#x", p2[0], p2[4])
	}
	offset := int(p2[1])<<6 | int(p2[2])>>2
	length := int(p2[2]&0x03)<<8 | int(p2[3])
	if offset != 960 || length != len(f1) {
		t.Fatalf("redundant header: offset %d length %d", offset, length)
	}
	if !bytes.Equal(p2[5:5+len(f1)], f1) {
		t.Fatal("redundant block is not the previous frame")
	}
	prim, n, ok = REDPrimary(p2)
	if !ok || n != 1 || !bytes.Equal(prim, f2) {
		t.Fatalf("second packet primary: ok=%v redundant=%d", ok, n)
	}
	// The trailer of the primary frame still ends the RED payload.
	if sent, ok := ReadTrailer(p2); !ok || !sent.Equal(now.Add(20*time.Millisecond)) {
		t.Fatalf("trailer through RED: ok=%v sent=%v", ok, sent)
	}
}

func TestREDEncoderSkipsFarOrLongRedundancy(t *testing.T) {
	e := &REDEncoder{ClockRate: 48000}
	e.Encode(OpusFrame, 400*time.Millisecond) // 19200 samples: over the 14-bit offset
	if p := e.Encode(OpusFrame, OpusFrameDur); len(p) != 1+len(OpusFrame) {
		t.Fatalf("far previous frame was sent as redundancy: len %d", len(p))
	}
	e.Encode(make([]byte, 1024), OpusFrameDur) // over the 10-bit length
	if p := e.Encode(OpusFrame, OpusFrameDur); len(p) != 1+len(OpusFrame) {
		t.Fatalf("long previous frame was sent as redundancy: len %d", len(p))
	}
	if _, _, ok := REDPrimary([]byte{0x80 | REDOpusPT, 0, 0}); ok {
		t.Fatal("truncated RED payload accepted")
	}
}
