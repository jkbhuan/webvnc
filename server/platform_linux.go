//go:build linux

// This file holds the parts of the Linux port that have no X11 backend yet, and
// the two no-ops that exist only because Windows needs them.
//
// What is here is deliberately small. Screen capture and input injection do have
// real backends (capture_linux.go, input_linux.go, x11_linux.go); audio and the
// clipboard do not, and report that the way every other unsupported platform
// does, with the same sentinel errors. The rest of the server already treats a
// failure from either as a per-feature refusal rather than a fatal one: audio is
// opened when a client asks for it and the clipboard is read and written per
// message, so both degrade to "this feature is off" and nothing else changes.

package server

// EnableDPIAwareness is a no-op on Linux.
//
// Windows needs it because a non-DPI-aware process gets a virtualised screen
// size and captures a blurry upscale of the real framebuffer. X11 delivered the
// real resolution to every client from the start, and Wayland has no equivalent
// switch, so there is nothing to opt into.
func EnableDPIAwareness() {}

// EnableUTF8Console is a no-op on Linux.
//
// Windows starts its console on the system codepage, so the Chinese log lines
// come out as mojibake until the process asks for UTF-8. A Linux terminal is
// UTF-8 unless the user has deliberately made it otherwise, and the program has
// no business overriding that.
func EnableUTF8Console() {}

// NewAudioCapturer always fails on Linux: WASAPI loopback is the only backend
// this project has, and the Linux equivalent is not written yet.
//
// The wire format being uncompressed PCM means the missing half is capture, not
// encoding -- what a Linux backend has to produce is a 20 ms packet of 16-bit
// samples, which audio.go already builds from any of the three sample formats a
// device might use.
func NewAudioCapturer() (AudioCapturer, error) { return nil, ErrUnsupportedAudio }

// AudioSupported reports that this platform has no audio backend, so the banner
// and the /ws/audio route can say so before anything tries to open a device.
//
// It exists because "start the server, notice the audio line says 开启, click the
// button, get nothing" is the kind of claim a person cannot check from outside:
// the failure is several seconds and four reconnect attempts away from the
// statement that caused it.
func AudioSupported() bool { return false }

// ReadClipboardText always fails on Linux.
//
// X11 has no clipboard *storage*: a selection lives in the process that owns it
// and is transferred on request, so reading it is a conversation with another
// program rather than a call. That is a different shape from the Windows
// implementation and has not been written.
func ReadClipboardText() (string, error) { return "", ErrUnsupportedClipboard }

// WriteClipboardText always fails on Linux, for the same reason as the read.
func WriteClipboardText(string) error { return ErrUnsupportedClipboard }
