package server

import "errors"

// ErrUnsupportedInput is returned on platforms without an input backend.
var ErrUnsupportedInput = errors.New("input injection is not supported on this platform")

// LookupKey is declared per platform, in input_windows.go and input_linux.go,
// because what a key press *is* differs: Windows has a virtual-key space and X11
// has keysyms, and neither can be expressed in the other's terms without a
// translation table that would only ever be wrong. Its contract, which the hub
// depends on, is the same on both:
//
//	LookupKey(code, key string) (symbol uint16, extended bool, ok bool)
//
// code is KeyboardEvent.code and key is KeyboardEvent.key, used as a fallback
// for clients and synthetic events that only provide one of the two. A false ok
// means the key is not one this backend can inject, which the hub drops rather
// than guessing at.
//
// The symbol is opaque to everything except the matching Injector, and so is
// extended -- it is meaningful on Windows (KEYEVENTF_EXTENDEDKEY) and ignored on
// Linux. Both fit in a uint16 because the keysyms this project maps all sit
// below 0x10000, which is where the Unicode keysyms start and the XF86 media
// keys live. Adding one of those means widening this, not truncating it.

// wheelDelta is one Windows wheel notch (WHEEL_DELTA).
const wheelDelta = 120

// Mouse buttons, named as in the DOM MouseEvent.button values.
const (
	ButtonLeft   = "left"
	ButtonMiddle = "middle"
	ButtonRight  = "right"
	ButtonBack   = "back"
	ButtonFwd    = "forward"
)

// unsupportedInjector is the Injector for a machine with no backend, and the
// fallback on one whose backend could not start.
//
// The hub builds an injector unconditionally and only discovers it is useless
// when an event arrives, so every method has to exist and every one has to fail
// the same way. Same reasoning as the stubs in platform_other.go: a server that
// cannot inject input still serves a picture, and refusing to start would take
// that away too.
type unsupportedInjector struct{}

func (unsupportedInjector) MoveTo(int, int) error        { return ErrUnsupportedInput }
func (unsupportedInjector) Button(string, bool) error    { return ErrUnsupportedInput }
func (unsupportedInjector) Wheel(int, int) error         { return ErrUnsupportedInput }
func (unsupportedInjector) Key(uint16, bool, bool) error { return ErrUnsupportedInput }
func (unsupportedInjector) Text(string) error            { return ErrUnsupportedInput }
func (unsupportedInjector) ScreenSize() (int, int)       { return 0, 0 }
func (unsupportedInjector) Close() error                 { return nil }

// Injector applies input events to the local desktop.
type Injector interface {
	// MoveTo moves the cursor to absolute pixel coordinates on the primary
	// monitor.
	MoveTo(x, y int) error
	// Button presses or releases a mouse button.
	Button(button string, down bool) error
	// Wheel scrolls; delta is in Windows wheel units (120 == one notch).
	Wheel(deltaX, deltaY int) error
	// Key presses or releases a key, identified by whatever symbol LookupKey
	// returned for this platform. extended is meaningful only on Windows.
	Key(symbol uint16, extended, down bool) error
	// Text types a string of Unicode characters, bypassing the keyboard layout
	// and any IME on the remote machine. Used for IME-composed text.
	Text(s string) error
	// ScreenSize reports the injectable desktop size.
	ScreenSize() (int, int)
	Close() error
}
