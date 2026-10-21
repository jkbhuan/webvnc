//go:build windows && amd64

// Command mfprobe reports what the Media Foundation H.264 encoder will do on
// the machine it runs on.
//
// It is a diagnostic, not part of the server: the encoder path depends on
// components that ship with Windows but vary between machines -- which MFT is
// registered, whether it accepts a linear NV12 buffer, whether it emits Annex B
// -- and none of that can be checked from anywhere but the machine itself.
// Running this once on a target machine answers those questions before the
// server is built around the answers.
//
//	go run ./cmd/mfprobe
//	go run ./cmd/mfprobe -w 1920 -h 1080 -fps 30 -bitrate 8000000
package main

import (
	"flag"
	"fmt"
	"os"

	"webvnc/internal/mfenc"
)

func main() {
	w := flag.Int("w", 1366, "frame width")
	h := flag.Int("h", 768, "frame height")
	fps := flag.Int("fps", 15, "frames per second")
	bitrate := flag.Int("bitrate", 4_000_000, "target bit rate in bits per second")
	frames := flag.Int("frames", 90, "how many frames to encode")
	flag.Parse()

	fmt.Println("webVNC encoder probe -- Media Foundation H.264")
	fmt.Println()

	err := mfenc.Probe(os.Stdout, mfenc.Config{
		Width:   *w,
		Height:  *h,
		FPS:     *fps,
		Bitrate: *bitrate,
	}, *frames)

	fmt.Println()
	if err != nil {
		fmt.Fprintf(os.Stderr, "probe failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("probe finished without errors")
}
