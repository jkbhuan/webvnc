package server

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
)

// ErrUnsupportedCapture is returned on platforms without a capture backend.
var ErrUnsupportedCapture = errors.New("screen capture is not supported on this platform")

// CaptureConfig holds the tunables for the capture loop.
type CaptureConfig struct {
	// Scale resizes the frame before encoding. 1.0 keeps the native
	// resolution; 0.5 halves both dimensions and roughly quarters the JPEG
	// payload. Coordinates stay in native screen space either way.
	//
	// This is the coarse lever on bandwidth: it is the only one that changes
	// how many pixels there are to encode, so it is what to reach for when the
	// rate controller reports it is already at its floors and still over target.
	Scale float64
}

// Capturer grabs the primary monitor into a reusable RGBA image.
type Capturer interface {
	// Grab returns the current frame. The returned image is reused on the next
	// call and must be encoded before calling Grab again.
	Grab() (*image.RGBA, error)
	// Size reports the encoded frame dimensions.
	Size() (int, int)
	Close()
}

// swizzleBGRAtoRGBA converts a 32bpp BGRA buffer to RGBA in place.
//
// Both capture backends hand back their pixels in this order, for the same
// reason: the desktop is 24 bits deep on both, and each platform pads a pixel to
// 32 bits the way its own memory layout happens to fall. On Windows it is
// BI_RGB, which leaves the fourth byte undefined; on X11 it is BGRX, where the
// fourth byte is padding that is not part of the picture. Either way the byte is
// forced opaque -- an alpha of zero would make the JPEG encoder treat every
// frame as fully transparent.
func swizzleBGRAtoRGBA(p []byte) {
	var i int
	for ; i+7 < len(p); i += 8 {
		p[i+0], p[i+2] = p[i+2], p[i+0]
		p[i+3] = 0xff
		p[i+4], p[i+6] = p[i+6], p[i+4]
		p[i+7] = 0xff
	}
	for ; i+3 < len(p); i += 4 {
		p[i+0], p[i+2] = p[i+2], p[i+0]
		p[i+3] = 0xff
	}
}

// platformNote is implemented by a Capturer that has something to say about the
// environment it found.
//
// It exists because the things the Linux backend needs to report -- a Wayland
// session, a display without MIT-SHM -- are silent from the outside: every call
// succeeds, and the picture or the frame rate is still not what the operator
// expects. Same reasoning as VideoEncoder.Describe, and the same remedy: a line
// in the startup log.
type platformNote interface {
	// PlatformNotes returns caveats worth logging, empty when there are none.
	PlatformNotes() []string
}

// encodeJPEG serialises a frame with the configured quality.
func encodeJPEG(img *image.RGBA, quality int) ([]byte, error) {
	var buf bytes.Buffer
	// Pre-size for a typical ~0.25 bytes/pixel JPEG to avoid regrowth.
	buf.Grow(len(img.Pix) / 4)
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
