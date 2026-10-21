package server

import (
	"errors"
	"fmt"
	"image"
)

// ErrNoHardwareEncoder is returned when the machine cannot run the inter-frame
// encoder, which is not fatal: the intra-frame path needs nothing but the Go
// standard library and always works.
var ErrNoHardwareEncoder = errors.New("no H.264 encoder is available on this machine")

// VideoCodec names what a binary frame carries. It is the first byte of every
// binary message on the video socket.
//
// Every message carries the tag rather than the stream agreeing on a codec once
// at connect time, because the server can change codec mid-stream -- a client
// that turns out not to be able to decode H.264 downgrades the whole stream
// rather than being refused -- and a client that had to remember what it was
// promised would be one reconnection away from decoding nonsense.
type VideoCodec uint8

const (
	// CodecJPEG is one complete baseline JPEG. Every one is independently
	// decodable, which is what makes it the fallback: it needs nothing from the
	// browser beyond createImageBitmap.
	CodecJPEG VideoCodec = 0

	// CodecH264 is one H.264 access unit in Annex B form -- start codes and
	// all, with SPS and PPS repeated in band before every keyframe.
	//
	// In band rather than out of band is a deliberate choice and it is what
	// keeps the client simple: WebCodecs decodes an Annex B stream with no
	// `description` at all, so there is no avcC box to build, no init segment
	// to keep in step with the encoder, and a decoder that has been reset
	// recovers from the next keyframe without anyone renegotiating anything.
	CodecH264 VideoCodec = 1
)

// Frame flags occupy the second byte of every binary message.
const (
	// FlagKey marks a frame a decoder can start from: an IDR, for H.264, and
	// every JPEG. A client that has just connected, or whose decoder has just
	// reported an error, discards input until it sees this flag.
	FlagKey = 1 << 0
)

// videoHeaderLen is the size of the codec-and-flags prefix.
const videoHeaderLen = 2

// frameMessage frames one encoded payload for the wire.
//
// The copy is deliberate. The payload belongs to a buffer the encoder reuses --
// grab returns the same pixels every tick, and the H.264 encoder hands back a
// slice into its own output -- so the message has to be built before the next
// frame overwrites it. Two bytes of header and one copy per frame is a rounding
// error next to the encode itself.
func frameMessage(codec VideoCodec, key bool, payload []byte) []byte {
	msg := make([]byte, videoHeaderLen+len(payload))
	msg[0] = byte(codec)
	if key {
		msg[1] = FlagKey
	}
	copy(msg[videoHeaderLen:], payload)
	return msg
}

// VideoEncoder turns a captured frame into the payload of a binary message.
//
// The two implementations differ in more than the format. JPEG encodes each
// frame alone, so every frame is a keyframe and the encoder has no state to
// lose. H.264 predicts each frame from the ones before it, which is where the
// bandwidth goes and also why it has to be told when a client needs a fresh
// starting point.
type VideoEncoder interface {
	// Encode serialises one frame. keyframe reports whether the payload is one
	// a decoder can start from with no earlier frame.
	//
	// An empty payload with a nil error means the encoder had nothing to emit
	// for this frame, which the caller should pass over rather than send.
	Encode(img *image.RGBA, quality int) (payload []byte, keyframe bool, err error)

	// Codec is the tag this encoder's payloads go on the wire with.
	Codec() VideoCodec

	// Describe is a one-line summary for the startup log, covering the things
	// that cannot be seen from outside: a setting the encoder refused, a
	// pipeline depth nobody asked for. It is worth a log line because every one
	// of those failures is silent.
	Describe() string

	// Close releases the encoder and its thread. It is safe to call twice.
	Close()
}

// ---------------------------------------------------------------------------
// Intra-frame
// ---------------------------------------------------------------------------

// jpegEncoder is the original path, unchanged: one JPEG per frame, quality set
// by the rate controller.
type jpegEncoder struct{}

func (jpegEncoder) Encode(img *image.RGBA, quality int) ([]byte, bool, error) {
	frame, err := encodeJPEG(img, quality)
	if err != nil {
		return nil, false, err
	}
	// Every JPEG stands alone, so every JPEG is a keyframe. Saying so is what
	// lets the client treat the two codecs the same way.
	return frame, true, nil
}

func (jpegEncoder) Codec() VideoCodec { return CodecJPEG }
func (jpegEncoder) Close()            {}

func (jpegEncoder) Describe() string {
	return "intra-frame JPEG, every frame independent"
}

// ---------------------------------------------------------------------------
// Choosing one
// ---------------------------------------------------------------------------

// VideoMode is how the operator asked for the stream to be encoded.
type VideoMode string

const (
	// ModeAuto prefers inter-frame and falls back to intra-frame, at startup if
	// the machine cannot encode H.264 and at runtime if the encoder fails or a
	// client turns out not to be able to decode it.
	ModeAuto VideoMode = "auto"
	// ModeH264 is inter-frame only: a client that cannot decode it is refused
	// rather than served something else.
	ModeH264 VideoMode = "h264"
	// ModeJPEG is intra-frame only, which is what this server did before
	// inter-frame existed.
	ModeJPEG VideoMode = "jpeg"
)

// ParseVideoMode turns the command-line value into a mode.
func ParseVideoMode(s string) (VideoMode, error) {
	switch m := VideoMode(s); m {
	case ModeAuto, ModeH264, ModeJPEG:
		return m, nil
	default:
		return "", fmt.Errorf("unknown video codec %q: want auto, h264 or jpeg", s)
	}
}

// videoBackend reports which encoder the hub will run, and describes it,
// without keeping one.
//
// The capture loop builds the real encoder lazily -- a server nobody is
// watching should not hold an encoder open -- but the operator needs to know
// before the first client connects which of the two paths this machine will
// take, and in the h264-only mode a machine that cannot take it needs to say so
// at startup rather than by failing a frame at a time.
//
// Building one and throwing it away is the only honest way to find out: whether
// the encoder exists cannot be asked without creating it.
func videoBackend(mode VideoMode, cfg HubConfig, w, h int) (VideoCodec, string, error) {
	if mode == ModeJPEG {
		return CodecJPEG, jpegEncoder{}.Describe(), nil
	}

	enc, err := newH264Encoder(cfg, w, h)
	if err != nil {
		if mode == ModeH264 {
			return 0, "", err
		}
		return CodecJPEG, jpegEncoder{}.Describe() + " (H.264 unavailable: " + err.Error() + ")", nil
	}
	defer enc.Close()

	desc := enc.Describe()
	if mode == ModeAuto {
		desc += "; falls back to JPEG if a client cannot decode it"
	}
	return CodecH264, desc, nil
}

// newVideoEncoder builds the encoder for a mode.
//
// It returns the encoder and the codec it produces. ModeAuto asks for the
// inter-frame encoder and settles for the intra-frame one if the machine will
// not provide it, which is the whole difference between the two modes at
// startup: ModeH264 makes that failure the caller's problem.
//
// w and h are the captured frame size, which the H.264 encoder needs up front:
// it is configured for one resolution and cannot be handed another.
func newVideoEncoder(mode VideoMode, cfg HubConfig, w, h int) (VideoEncoder, error) {
	if mode == ModeJPEG {
		return jpegEncoder{}, nil
	}

	enc, err := newH264Encoder(cfg, w, h)
	if err == nil {
		return enc, nil
	}
	if mode == ModeH264 {
		return nil, err
	}
	return jpegEncoder{}, nil
}
