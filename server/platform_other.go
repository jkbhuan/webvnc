//go:build !windows && !linux

// This file keeps the module buildable on hosts that have no backend at all --
// macOS, the BSDs -- so the frontend embed and the protocol code can still be
// vetted. Linux has a real backend (see platform_linux.go and its neighbours)
// and is excluded here; Windows is the original one.

package server

// EnableDPIAwareness is a no-op off Windows.
func EnableDPIAwareness() {}

// EnableUTF8Console is a no-op off Windows.
func EnableUTF8Console() {}

// ScreenSize reports no screen off Windows.
func ScreenSize() (int, int) { return 0, 0 }

// NewCapturer always fails off Windows.
func NewCapturer(CaptureConfig) (Capturer, error) { return nil, ErrUnsupportedCapture }

// NewInjector always fails off Windows.
func NewInjector() Injector { return unsupportedInjector{} }

// LookupKey resolves nothing off Windows.
//
// The hub calls this for every key event regardless of platform, so it has to
// exist for the package to build; with no backend behind it there is nothing to
// resolve to, and reporting the key as unmappable is the honest answer. Linux
// has its own in input_linux.go.
func LookupKey(string, string) (uint16, bool, bool) { return 0, false, false }

// NewAudioCapturer always fails off Windows: WASAPI is the only backend.
func NewAudioCapturer() (AudioCapturer, error) { return nil, ErrUnsupportedAudio }

// AudioSupported reports that this platform has no audio backend. See the Linux
// one for why a predicate is needed at all.
func AudioSupported() bool { return false }

// ReadClipboardText always fails off Windows.
func ReadClipboardText() (string, error) { return "", ErrUnsupportedClipboard }

// WriteClipboardText always fails off Windows.
func WriteClipboardText(string) error { return ErrUnsupportedClipboard }
