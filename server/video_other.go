//go:build !windows || !amd64

package server

// newH264Encoder reports that this platform has no inter-frame encoder.
//
// There is no portable fallback to reach for here. Software H.264 would mean
// either cgo or a pure-Go encoder, and the first breaks the single-binary
// static build while the second would not keep up with a desktop at a usable
// frame rate. Neither is worth it for a program whose whole point is to stream
// the screen of the machine it is running on, which is Windows.
func newH264Encoder(HubConfig, int, int) (VideoEncoder, error) {
	return nil, ErrNoHardwareEncoder
}
