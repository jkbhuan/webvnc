//go:build !windows || !amd64

package mfenc

import "errors"

// ErrUnsupported is what every entry point of this package returns on a build
// that cannot reach Media Foundation.
//
// The encoder is a Windows component reached through amd64 calling convention
// assumptions (see mf_windows.go), so the 32-bit Windows build is excluded
// alongside the other platforms. The server treats this as "use the JPEG path",
// which is why it is a plain error rather than a build failure.
var ErrUnsupported = errors.New("H.264 encoding is only available on 64-bit Windows")

// Config mirrors the Windows build's stream description. It exists here so that
// callers compile unchanged on every platform.
type Config struct {
	Width         int
	Height        int
	FPS           int
	Bitrate       int // target bit rate in bits per second
	KeyframeEvery int // GOP length in seconds
	Profile       int // 0 selects the default profile
}

// Encoder is the unusable stand-in for the Windows encoder.
type Encoder struct{}

// New always fails on this platform.
func New(Config) (*Encoder, error) { return nil, ErrUnsupported }

// Size always reports nothing.
func (e *Encoder) Size() (int, int) { return 0, 0 }

// Encode always fails on this platform.
func (e *Encoder) Encode([]byte, int, bool) ([]byte, error) { return nil, ErrUnsupported }

// Close does nothing, there being nothing to release.
func (e *Encoder) Close() {}
