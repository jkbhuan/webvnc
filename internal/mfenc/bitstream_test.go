package mfenc

import (
	"strings"
	"testing"
)

// nal builds an Annex B unit with a four-byte start code, so the parser is
// exercised against the longer of the two forms.
func nal(typ byte, body ...byte) []byte {
	out := []byte{0, 0, 0, 1, 0x60 | typ}
	return append(out, body...)
}

// sps is a sequence parameter set claiming profile 0x42 (baseline),
// constraint flags 0xE0 and level 0x1F (3.1).
func sps() []byte { return nal(nalSPS, 0x42, 0xE0, 0x1F, 0x8C, 0x00) }

func pps() []byte { return nal(nalPPS, 0xCE, 0x38, 0x80) }

func idr(payload ...byte) []byte { return nal(nalIDR, payload...) }

func slice(typ byte) []byte { return nal(typ, 0x9A, 0x01) }

func TestSplitNALUs(t *testing.T) {
	stream := append(append(append(sps(), pps()...), idr(1, 2, 3)...), slice(nalSlice)...)

	units := SplitNALUs(stream)
	if len(units) != 4 {
		t.Fatalf("got %d NAL units, want 4: %s", len(units), DescribeNALUs(stream))
	}
	want := []int{nalSPS, nalPPS, nalIDR, nalSlice}
	for i, typ := range want {
		if got := NALType(units[i]); got != typ {
			t.Errorf("unit %d has type %d, want %d", i, got, typ)
		}
	}
	// The parser must strip the start code but keep the header and payload.
	if units[2][0]&0x1f != nalIDR || len(units[2]) != 4 {
		t.Errorf("IDR unit = %v, want header plus 3 payload bytes", units[2])
	}
}

func TestSplitNALUsThreeByteStartCodes(t *testing.T) {
	// A three-byte start code is legal everywhere but the first unit.
	stream := append(sps(), []byte{0, 0, 1, 0x68, 0xAA}...)

	units := SplitNALUs(stream)
	if len(units) != 2 {
		t.Fatalf("got %d NAL units, want 2", len(units))
	}
	if NALType(units[1]) != nalPPS || len(units[1]) != 2 {
		t.Errorf("second unit = %v, want PPS with one payload byte", units[1])
	}
}

func TestSplitNALUsTrimsTrailingZeros(t *testing.T) {
	// Encoders pad with cabac_zero_word; those bytes are not part of the unit.
	units := SplitNALUs(append(sps(), 0, 0, 0, 0, 0, 0))
	if len(units) != 1 {
		t.Fatalf("got %d NAL units, want 1", len(units))
	}
	if last := units[0][len(units[0])-1]; last == 0 {
		t.Errorf("trailing zero survived: %v", units[0])
	}
}

func TestSplitNALUsEmpty(t *testing.T) {
	for _, in := range [][]byte{nil, {}, {0, 0, 0, 1}} {
		if units := SplitNALUs(in); len(units) != 0 {
			t.Errorf("SplitNALUs(%v) = %d units, want 0", in, len(units))
		}
	}
}

func TestCodecString(t *testing.T) {
	stream := append(append(sps(), pps()...), idr(0xFF)...)

	got, err := CodecString(stream)
	if err != nil {
		t.Fatalf("CodecString: %v", err)
	}
	if want := "avc1.42E01F"; got != want {
		t.Errorf("CodecString = %q, want %q", got, want)
	}
}

func TestCodecStringWithoutSPS(t *testing.T) {
	_, err := CodecString(append(pps(), idr(0x11)...))
	if err == nil {
		t.Fatal("CodecString accepted a stream with no SPS")
	}
	if !strings.Contains(err.Error(), "sequence parameter set") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestCodecStringTruncatedSPS(t *testing.T) {
	// A real SPS is far longer than three bytes; a stub that short means the
	// stream is corrupt, and reading a level out of it would be a lie.
	if _, err := CodecString(nal(nalSPS, 0x42)); err == nil {
		t.Fatal("CodecString accepted a two-byte SPS")
	}
}

func TestIsKeyframe(t *testing.T) {
	if !IsKeyframe(append(append(sps(), pps()...), idr(0x88)...)) {
		t.Error("stream containing an IDR reported as not a keyframe")
	}
	if IsKeyframe(append(sps(), slice(nalSlice)...)) {
		t.Error("stream with no IDR reported as a keyframe")
	}
}

func TestDescribeNALUs(t *testing.T) {
	stream := append(append(sps(), pps()...), idr(1, 2, 3)...)
	got := DescribeNALUs(stream)
	for _, want := range []string{"SPS(", "PPS(", "IDR(4)"} {
		if !strings.Contains(got, want) {
			t.Errorf("DescribeNALUs = %q, missing %q", got, want)
		}
	}
}
