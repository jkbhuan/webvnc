//go:build windows && amd64

package server

import (
	"fmt"
	"image"
	"strings"

	"webvnc/internal/mfenc"
)

// h264Encoder adapts the Media Foundation encoder to the hub's interface.
//
// The conversion from RGBA to NV12 happens inside mfenc, writing straight into
// the encoder's own buffer, so a frame costs one pass over the pixels that the
// intra-frame path does not pay. That is the price of predicting frames from
// each other, and at 1366x768 it measured at well under a millisecond.
type h264Encoder struct {
	enc *mfenc.Encoder
}

// newH264Encoder builds the inter-frame encoder for a hub configuration.
func newH264Encoder(cfg HubConfig, w, h int) (VideoEncoder, error) {
	enc, err := mfenc.New(mfenc.Config{
		Width:   w,
		Height:  h,
		FPS:     cfg.FPS,
		Bitrate: int(cfg.MaxBitrate),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoHardwareEncoder, err)
	}
	return &h264Encoder{enc: enc}, nil
}

// Encode passes one frame through, asking for a keyframe only on the first.
//
// The first frame of an H.264 stream has to be an IDR whatever anyone asks for,
// so the request is redundant there and only there -- and on every later frame
// it would be worse than redundant. This encoder advertises
// CODECAPI_AVEncForceKeyFrame and answers every attempt to set it with
// E_NOTIMPL, so asking per frame would add a failing call to every tick and
// change nothing. Keyframes come from the GOP, which is set short (one second)
// precisely because that is the only way a client that has just connected gets
// a picture.
func (e *h264Encoder) Encode(img *image.RGBA, _ int) ([]byte, bool, error) {
	au, err := e.enc.Encode(img.Pix, img.Stride, false)
	if err != nil {
		return nil, false, err
	}
	if len(au) == 0 {
		// The encoder is priming. Not an error: there is simply nothing to send
		// this tick.
		return nil, false, nil
	}
	return au, mfenc.IsKeyframe(au), nil
}

func (e *h264Encoder) Codec() VideoCodec { return CodecH264 }
func (e *h264Encoder) Close()            { e.enc.Close() }

// Describe summarises the encoder for the startup log, naming every optional
// setting the encoder refused.
//
// This encoder refuses three that matter and does not say so: it advertises
// CODECAPI_AVEncCommonLowLatency and CODECAPI_AVEncCABACEnable as properties it
// knows, then answers every attempt to set them with E_NOTIMPL, and it accepts
// CODECAPI_AVEncForceKeyFrame at configuration time and refuses it per frame.
// None of that is visible from the outside -- the stream looks fine either way
// -- so a line in the log is the only place it can surface.
func (e *h264Encoder) Describe() string {
	w, h := e.enc.Size()
	desc := fmt.Sprintf("inter-frame H.264 %dx%d", w, h)

	var refused []string
	for _, s := range e.enc.Settings() {
		if s.Err != nil {
			refused = append(refused, s.Name)
		}
	}
	if len(refused) > 0 {
		desc += "; refused: " + strings.Join(refused, ", ")
	}
	return desc
}
