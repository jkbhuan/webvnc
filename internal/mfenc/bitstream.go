package mfenc

import (
	"fmt"
	"strings"
)

// H.264 NAL unit types (Table 7-1). Only the ones this package looks at.
const (
	nalSlice = 1  // non-IDR coded slice
	nalIDR   = 5  // IDR coded slice: a keyframe's picture data
	nalSEI   = 6  // supplemental information
	nalSPS   = 7  // sequence parameter set
	nalPPS   = 8  // picture parameter set
	nalAUD   = 9  // access unit delimiter
	nalOther = -1 // anything this package does not name
)

// NALType returns the nal_unit_type of a NAL unit from its first byte, or
// nalOther for an empty unit.
func NALType(nal []byte) int {
	if len(nal) == 0 {
		return nalOther
	}
	switch t := int(nal[0] & 0x1f); t {
	case nalSlice, nalIDR, nalSEI, nalSPS, nalPPS, nalAUD:
		return t
	default:
		return nalOther
	}
}

// SplitNALUs walks an Annex B byte stream and returns its NAL units with the
// start codes stripped.
//
// The returned units alias stream; copy them if the buffer is reused. Trailing
// zero bytes are trimmed, since a four-byte start code leaves one behind and
// encoders may also pad with cabac_zero_word.
func SplitNALUs(stream []byte) [][]byte {
	var out [][]byte
	start := -1

	for i := 0; i+2 < len(stream); {
		if stream[i] == 0 && stream[i+1] == 0 && stream[i+2] == 1 {
			if start >= 0 {
				out = append(out, trimTrailingZeros(stream[start:i]))
			}
			start = i + 3
			i += 3
			continue
		}
		i++
	}
	if start >= 0 {
		out = append(out, trimTrailingZeros(stream[start:]))
	}

	// A three-byte start code at the very end yields an empty final unit.
	if n := len(out); n > 0 && len(out[n-1]) == 0 {
		out = out[:n-1]
	}
	return out
}

func trimTrailingZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// CodecString derives the RFC 6381 codec string ("avc1.42E01F") for a stream,
// reading it out of the sequence parameter set.
//
// The browser needs this before it will configure a decoder, and it has to
// match the stream exactly: profile, constraint flags and level are the first
// three bytes after the SPS header, and WebCodecs rejects a config whose string
// disagrees with the bitstream it is fed.
func CodecString(stream []byte) (string, error) {
	for _, nal := range SplitNALUs(stream) {
		if NALType(nal) != nalSPS {
			continue
		}
		if len(nal) < 4 {
			return "", fmt.Errorf("SPS is %d bytes, too short to carry a profile and level", len(nal))
		}
		return fmt.Sprintf("avc1.%02X%02X%02X", nal[1], nal[2], nal[3]), nil
	}
	return "", fmt.Errorf("no sequence parameter set in %d bytes of stream", len(stream))
}

// IsKeyframe reports whether the stream contains an IDR picture.
//
// WebCodecs has to be told when a chunk is a keyframe, and only the encoder
// knows: a stream that starts mid-GOP cannot be decoded until an IDR arrives.
func IsKeyframe(stream []byte) bool {
	for _, nal := range SplitNALUs(stream) {
		if NALType(nal) == nalIDR {
			return true
		}
	}
	return false
}

// DescribeNALUs renders the NAL units of a stream as a short summary, which is
// what makes a probe run readable: the types alone say whether the encoder
// emitted in-band parameter sets, and where the keyframes are.
func DescribeNALUs(stream []byte) string {
	var parts []string
	for _, nal := range SplitNALUs(stream) {
		parts = append(parts, fmt.Sprintf("%s(%d)", nalName(NALType(nal)), len(nal)))
	}
	return strings.Join(parts, " ")
}

func nalName(t int) string {
	switch t {
	case nalSlice:
		return "slice"
	case nalIDR:
		return "IDR"
	case nalSEI:
		return "SEI"
	case nalSPS:
		return "SPS"
	case nalPPS:
		return "PPS"
	case nalAUD:
		return "AUD"
	default:
		return "other"
	}
}
