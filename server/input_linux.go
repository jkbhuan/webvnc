//go:build linux

// Input injection on X11, through the XTEST extension.
//
// XTEST is the same mechanism xdotool and every other automation tool uses: a
// client asks the server to synthesise an event, and the server delivers it to
// whatever has focus exactly as though a device had produced it. Nothing here
// needs to know which application is on screen, and no window manager
// cooperation is involved.
//
// The interesting part is the indirection through the keyboard *mapping*. XTEST
// injects a keycode -- the physical key number, roughly -- and the server turns
// that into a keysym using the layout loaded on the machine being controlled.
// The browser, meanwhile, reports KeyboardEvent.code, which is also physical.
// So the translation is code -> keysym -> keycode, and the middle step is why
// this file reads the server's mapping at startup and keeps it: which keycode
// produces 'a' depends on the layout, and the layout is the remote machine's
// business, not the client's.

package server

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// X11 keysyms, from keysymdef.h. The Latin-1 range is deliberately written as
// its codepoint: X11 defines a keysym for every character in Latin-1 with the
// same value, which is what makes the ASCII half of the table readable.
const (
	xksSpace      = 0x0020
	xksApostrophe = 0x0027
	xksComma      = 0x002c
	xksMinus      = 0x002d
	xksPeriod     = 0x002e
	xksSlash      = 0x002f
	xks0          = 0x0030
	xksSemicolon  = 0x003b
	xksLess       = 0x003c
	xksEqual      = 0x003d
	xksBracketL   = 0x005b
	xksBackslash  = 0x005c
	xksBracketR   = 0x005d
	xksGrave      = 0x0060
	xksA          = 0x0061
	xksYen        = 0x00a5

	xksBackSpace   = 0xff08
	xksTab         = 0xff09
	xksReturn      = 0xff0d
	xksPause       = 0xff13
	xksScrollLock  = 0xff14
	xksEscape      = 0xff1b
	xksHome        = 0xff50
	xksLeft        = 0xff51
	xksUp          = 0xff52
	xksRight       = 0xff53
	xksDown        = 0xff54
	xksPageUp      = 0xff55
	xksPageDown    = 0xff56
	xksEnd         = 0xff57
	xksPrint       = 0xff61
	xksInsert      = 0xff63
	xksMenu        = 0xff67
	xksNumLock     = 0xff7f
	xksKPEnter     = 0xff8d
	xksKPMultiply  = 0xffaa
	xksKPAdd       = 0xffab
	xksKPSeparator = 0xffac
	xksKPSubtract  = 0xffad
	xksKPDecimal   = 0xffae
	xksKPDivide    = 0xffaf
	xksKP0         = 0xffb0
	xksKPEqual     = 0xffbd
	xksF1          = 0xffbe
	xksShiftL      = 0xffe1
	xksShiftR      = 0xffe2
	xksControlL    = 0xffe3
	xksControlR    = 0xffe4
	xksCapsLock    = 0xffe5
	xksAltL        = 0xffe9
	xksAltR        = 0xffea
	xksSuperL      = 0xffeb
	xksSuperR      = 0xffec
	xksDelete      = 0xffff
)

// codeToSym maps KeyboardEvent.code -> X11 keysym, the Linux counterpart of the
// Windows table in input_windows.go.
//
// It covers the same set of keys, with two deliberate differences:
//
//   - Enter and NumpadEnter are distinct here, where Windows folds both onto
//     VK_RETURN. X11 has a separate keysym for the numpad key and applications
//     can and do tell them apart.
//
//   - IntlRo is absent. It is the ABNT2 "/?" key, whose X11 keysym depends on
//     which Brazilian variant the layout is using, so there is no single right
//     answer to give. Reporting it as unmappable is better than injecting
//     whatever key happens to sit at that position.
var codeToSym = map[string]xproto.Keysym{
	// Letters. The unshifted keysym is the lowercase letter; the server applies
	// Shift from whatever is held, exactly as it does for a real keyboard.
	"KeyA": xksA + 0, "KeyB": xksA + 1, "KeyC": xksA + 2,
	"KeyD": xksA + 3, "KeyE": xksA + 4, "KeyF": xksA + 5,
	"KeyG": xksA + 6, "KeyH": xksA + 7, "KeyI": xksA + 8,
	"KeyJ": xksA + 9, "KeyK": xksA + 10, "KeyL": xksA + 11,
	"KeyM": xksA + 12, "KeyN": xksA + 13, "KeyO": xksA + 14,
	"KeyP": xksA + 15, "KeyQ": xksA + 16, "KeyR": xksA + 17,
	"KeyS": xksA + 18, "KeyT": xksA + 19, "KeyU": xksA + 20,
	"KeyV": xksA + 21, "KeyW": xksA + 22, "KeyX": xksA + 23,
	"KeyY": xksA + 24, "KeyZ": xksA + 25,

	// Digits (top row)
	"Digit0": xks0 + 0, "Digit1": xks0 + 1, "Digit2": xks0 + 2,
	"Digit3": xks0 + 3, "Digit4": xks0 + 4, "Digit5": xks0 + 5,
	"Digit6": xks0 + 6, "Digit7": xks0 + 7, "Digit8": xks0 + 8,
	"Digit9": xks0 + 9,

	// OEM punctuation
	"Backquote":     xksGrave,
	"Minus":         xksMinus,
	"Equal":         xksEqual,
	"BracketLeft":   xksBracketL,
	"BracketRight":  xksBracketR,
	"Backslash":     xksBackslash,
	"Semicolon":     xksSemicolon,
	"Quote":         xksApostrophe,
	"Comma":         xksComma,
	"Period":        xksPeriod,
	"Slash":         xksSlash,
	"IntlBackslash": xksLess,
	"IntlYen":       xksYen,

	// Whitespace / editing
	"Enter":       xksReturn,
	"NumpadEnter": xksKPEnter,
	"Tab":         xksTab,
	"Space":       xksSpace,
	"Backspace":   xksBackSpace,
	"Escape":      xksEscape,
	"Delete":      xksDelete,
	"Insert":      xksInsert,

	// Navigation cluster
	"Home":       xksHome,
	"End":        xksEnd,
	"PageUp":     xksPageUp,
	"PageDown":   xksPageDown,
	"ArrowUp":    xksUp,
	"ArrowDown":  xksDown,
	"ArrowLeft":  xksLeft,
	"ArrowRight": xksRight,

	// Modifiers
	"ShiftLeft":    xksShiftL,
	"ShiftRight":   xksShiftR,
	"ControlLeft":  xksControlL,
	"ControlRight": xksControlR,
	"AltLeft":      xksAltL,
	"AltRight":     xksAltR,
	"MetaLeft":     xksSuperL,
	"MetaRight":    xksSuperR,
	"ContextMenu":  xksMenu,
	"CapsLock":     xksCapsLock,
	"NumLock":      xksNumLock,
	"ScrollLock":   xksScrollLock,
	"PrintScreen":  xksPrint,
	"Pause":        xksPause,

	// Numpad
	"Numpad0": xksKP0 + 0, "Numpad1": xksKP0 + 1,
	"Numpad2": xksKP0 + 2, "Numpad3": xksKP0 + 3,
	"Numpad4": xksKP0 + 4, "Numpad5": xksKP0 + 5,
	"Numpad6": xksKP0 + 6, "Numpad7": xksKP0 + 7,
	"Numpad8": xksKP0 + 8, "Numpad9": xksKP0 + 9,
	"NumpadMultiply": xksKPMultiply,
	"NumpadAdd":      xksKPAdd,
	"NumpadSubtract": xksKPSubtract,
	"NumpadDecimal":  xksKPDecimal,
	"NumpadDivide":   xksKPDivide,
	"NumpadComma":    xksKPSeparator,
	"NumpadEqual":    xksKPEqual,
}

func init() {
	// Function keys F1..F24 are contiguous from XK_F1, as they are from VK_F1.
	for i := 0; i < 24; i++ {
		codeToSym["F"+strconv.Itoa(i+1)] = xksF1 + xproto.Keysym(i)
	}
}

// LookupKey resolves a browser key event to an X11 keysym.
// code is KeyboardEvent.code; key is KeyboardEvent.key, used as a fallback.
//
// extended is always false: it exists for Windows' KEYEVENTF_EXTENDEDKEY, which
// distinguishes the navigation cluster from the numpad on a keyboard protocol
// that reports both with the same scan code. X11 keysyms are unambiguous, so
// there is nothing for it to carry.
func LookupKey(code, key string) (uint16, bool, bool) {
	if sym, found := codeToSym[code]; found {
		return uint16(sym), false, true
	}
	// Some clients (and synthetic events) only provide `key`.
	if sym, found := codeToSym[key]; found {
		return uint16(sym), false, true
	}
	// A single character that the layout tables do not name -- most often a
	// letter on a client whose `code` is missing. Latin-1 and ASCII keysyms are
	// the codepoint, so this is a direct answer rather than a guess.
	if len(key) == 1 {
		if c := key[0]; c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return uint16(c), false, true
		}
	}
	return 0, false, false
}

// X11 mouse button numbers, which are not the DOM's.
//
// 1/2/3 are left/middle/right; 8 and 9 are the two side buttons on an ordinary
// five-button mouse. 4 and 5 are wheel-up and wheel-down and 6 and 7 are
// wheel-left and wheel-right, all of which are *buttons* in X11 rather than a
// separate scroll axis -- a notch is a press and release of one of them.
const (
	xbuttonLeft       = 1
	xbuttonMiddle     = 2
	xbuttonRight      = 3
	xbuttonWheelUp    = 4
	xbuttonWheelDown  = 5
	xbuttonWheelLeft  = 6
	xbuttonWheelRight = 7
	xbuttonBack       = 8
	xbuttonForward    = 9
)

// x11Injector injects input through the XTEST extension.
type x11Injector struct {
	mu sync.Mutex
	d  *x11Display

	// keysymsPerKeycode is the width of a row of the server's keyboard mapping,
	// kept because ChangeKeyboardMapping has to be told it on every call.
	keysymsPerKeycode byte

	// keycodes maps a keysym the current layout produces to a keycode that
	// produces it. Built once, at startup.
	keycodes map[xproto.Keysym]xproto.Keycode

	// scratch is a keycode the layout does not use, and scratchSym is the symbol
	// currently mapped onto it. Any character the layout cannot type -- which is
	// every CJK character, and every character an IME produces -- is injected by
	// pointing this keycode at it for the duration. See pressScratch.
	scratch    xproto.Keycode
	scratchSym xproto.Keysym
}

// NewInjector returns the X11 input backend, or a refusing one if there is no X
// server or no XTEST extension.
//
// It deliberately does not return an error. The hub calls this unconditionally
// while building itself, and a machine with no display still has a picture worth
// serving in the cases where capture worked -- and, more to the point, a failure
// here would take down a server whose only broken part is the keyboard.
func NewInjector() Injector {
	d, err := openX11()
	if err != nil {
		return unsupportedInjector{}
	}
	in, err := newX11Injector(d)
	if err != nil {
		d.Close()
		return unsupportedInjector{}
	}
	return in
}

func newX11Injector(d *x11Display) (*x11Injector, error) {
	if err := xtest.Init(d.conn); err != nil {
		return nil, fmt.Errorf("XTEST extension: %w", err)
	}
	// XTEST 2.2 is where the request set this file uses was fixed. Asking is
	// also the cheapest way to be sure the extension is actually answering.
	if _, err := xtest.GetVersion(d.conn, 2, 2).Reply(); err != nil {
		return nil, fmt.Errorf("XTEST GetVersion: %w", err)
	}

	in := &x11Injector{d: d}
	if err := in.loadKeymap(); err != nil {
		return nil, err
	}
	return in, nil
}

// loadKeymap reads the server's keyboard mapping and inverts it.
//
// The mapping is a table of keysym columns, four per keycode on a normal
// layout: unshifted, shifted, then two more that belong to alternate groups. Only
// the first two are considered, because those are the ones a key press with no
// modifier can reach -- which is what XTEST produces, since it synthesises a
// keycode and lets the server apply whatever modifiers happen to be held.
func (in *x11Injector) loadKeymap() error {
	setup := xproto.Setup(in.d.conn)
	first := setup.MinKeycode
	count := byte(setup.MaxKeycode - setup.MinKeycode + 1)

	reply, err := xproto.GetKeyboardMapping(in.d.conn, first, count).Reply()
	if err != nil {
		return fmt.Errorf("GetKeyboardMapping: %w", err)
	}
	per := int(reply.KeysymsPerKeycode)
	if per <= 0 {
		return fmt.Errorf("the X server reported a keyboard mapping %d columns wide", per)
	}
	in.keysymsPerKeycode = reply.KeysymsPerKeycode

	bySym := make(map[xproto.Keysym]xproto.Keycode, len(reply.Keysyms))
	for i, sym := range reply.Keysyms {
		if sym == 0 { // NoSymbol
			continue
		}
		if i%per > 1 {
			continue
		}
		code := first + xproto.Keycode(i/per)
		// First writer wins, and the table runs low keycode to high, so the
		// symbol ends up on the lowest keycode that produces it. Two keys can
		// carry the same symbol (the numpad on a layout without NumLock
		// semantics, the two Shift keys) and picking the lower one is at least
		// deterministic.
		if _, seen := bySym[sym]; !seen {
			bySym[sym] = code
		}
	}
	in.keycodes = bySym

	// The scratch keycode is one the layout leaves entirely empty. Searching
	// downwards from the top finds the spare slots, and skipping used rows
	// matters: remapping a keycode a user's keyboard actually has would make
	// that key type the wrong character until it is put back.
	for code := int(setup.MaxKeycode); code >= int(setup.MinKeycode); code-- {
		base := (code - int(first)) * per
		used := false
		for c := 0; c < per; c++ {
			if reply.Keysyms[base+c] != 0 {
				used = true
				break
			}
		}
		if !used {
			in.scratch = xproto.Keycode(code)
			break
		}
	}
	return nil
}

func (in *x11Injector) ScreenSize() (int, int) { return ScreenSize() }

func (in *x11Injector) Close() error {
	in.mu.Lock()
	defer in.mu.Unlock()

	if in.d != nil {
		// Put the scratch keycode back the way the layout had it before this
		// process borrowed it, so a server left running does not keep a stray
		// symbol on a key nobody pressed.
		if in.scratch != 0 && in.scratchSym != 0 {
			blank := make([]xproto.Keysym, in.keysymsPerKeycode)
			_ = xproto.ChangeKeyboardMappingChecked(in.d.conn, 1, in.scratch, in.keysymsPerKeycode, blank).Check()
			in.scratchSym = 0
		}
		in.d.Close()
		in.d = nil
	}
	return nil
}

// MoveTo moves the pointer to absolute pixel coordinates on the root window.
func (in *x11Injector) MoveTo(x, y int) error {
	sw, sh := ScreenSize()
	if sw <= 0 || sh <= 0 {
		return fmt.Errorf("unknown screen size")
	}
	if x < 0 {
		x = 0
	} else if x > sw-1 {
		x = sw - 1
	}
	if y < 0 {
		y = 0
	} else if y > sh-1 {
		y = sh - 1
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	// detail 0 asks XTEST for absolute motion, which is what makes this agree
	// with the coordinates the browser sends: those are pixels in the captured
	// frame, not a distance to travel.
	return in.fake(xproto.MotionNotify, 0, x, y)
}

func (in *x11Injector) Button(button string, down bool) error {
	var num byte
	switch button {
	case "", ButtonLeft:
		num = xbuttonLeft
	case ButtonMiddle:
		num = xbuttonMiddle
	case ButtonRight:
		num = xbuttonRight
	case ButtonBack:
		num = xbuttonBack
	case ButtonFwd:
		num = xbuttonForward
	default:
		return fmt.Errorf("unknown mouse button %q", button)
	}

	event := byte(xproto.ButtonRelease)
	if down {
		event = xproto.ButtonPress
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	return in.fake(event, num, 0, 0)
}

// Wheel scrolls the desktop. Deltas arrive in Windows wheel units, where one
// notch is 120, because that is the unit the hub's accumulator works in.
//
// X11 has no scroll axis in its input protocol: a notch is a press and release
// of a numbered button. So the unit conversion is a division, and a delta too
// small to make a whole notch is dropped -- the hub has already accumulated
// sub-notch motion away before calling, so anything left here is a genuine
// fraction of a notch that this backend cannot express.
func (in *x11Injector) Wheel(deltaX, deltaY int) error {
	in.mu.Lock()
	defer in.mu.Unlock()

	// Positive scrolls up (button 4) and right (button 7), matching the Windows
	// convention the hub normalises to.
	axes := []struct {
		delta    int
		up, down byte
	}{
		{deltaY, xbuttonWheelUp, xbuttonWheelDown},
		{deltaX, xbuttonWheelRight, xbuttonWheelLeft},
	}
	for _, axis := range axes {
		clicks := axis.delta / wheelDelta
		if clicks < 0 {
			clicks = -clicks
		}
		if clicks == 0 {
			continue
		}
		button := axis.up
		if axis.delta < 0 {
			button = axis.down
		}
		for i := 0; i < clicks; i++ {
			if err := in.fake(xproto.ButtonPress, button, 0, 0); err != nil {
				return err
			}
			if err := in.fake(xproto.ButtonRelease, button, 0, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// Key presses or releases a key, identified by the keysym LookupKey returned.
func (in *x11Injector) Key(symbol uint16, _ bool, down bool) error {
	sym := xproto.Keysym(symbol)
	if sym == 0 {
		return fmt.Errorf("no keysym for that key")
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	code, err := in.keycodeFor(sym)
	if err != nil {
		return err
	}
	event := byte(xproto.KeyRelease)
	if down {
		event = xproto.KeyPress
	}
	return in.fake(event, byte(code), 0, 0)
}

// Text types a string into the focused window.
//
// Every character goes through the scratch keycode rather than the layout,
// because the layout cannot express what this carries. The hub uses Text for
// text an input method composed, so the content is by definition arbitrary
// Unicode -- Chinese, emoji, anything -- and only a handful of those characters
// have ever been on the remote keyboard's layout.
//
// Nothing here is specific to CJK: an ASCII character would work the same way
// and only costs more than the direct path would have.
func (in *x11Injector) Text(s string) error {
	if s == "" {
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	for _, r := range s {
		// A Unicode keysym is the codepoint with the 0x01000000 bit set; below
		// 0x0100 the keysym *is* the codepoint, which is why Latin-1 needs no
		// translation.
		sym := xproto.Keysym(r)
		if r >= 0x100 {
			sym = xproto.Keysym(r) | 0x01000000
		}

		if err := in.pressScratch(sym); err != nil {
			return err
		}
	}
	return nil
}

// keycodeFor finds a keycode that produces sym right now.
//
// The layout's own table is asked first: whenever the symbol is something the
// remote keyboard can already type, injecting the real key is strictly better
// than borrowing one, because applications watch the physical key and a
// shortcut bound to it keeps working.
func (in *x11Injector) keycodeFor(sym xproto.Keysym) (xproto.Keycode, error) {
	if code, ok := in.keycodes[sym]; ok {
		return code, nil
	}
	if in.scratch == 0 {
		return 0, fmt.Errorf("the keyboard layout has no key for keysym %#x and no spare keycode to synthesise one", sym)
	}
	if err := in.mapScratch(sym); err != nil {
		return 0, err
	}
	return in.scratch, nil
}

// pressScratch types one keysym through the scratch keycode.
func (in *x11Injector) pressScratch(sym xproto.Keysym) error {
	if in.scratch == 0 {
		return fmt.Errorf("the keyboard has no spare keycode to synthesise %#x on", sym)
	}
	if err := in.mapScratch(sym); err != nil {
		return err
	}
	if err := in.fake(xproto.KeyPress, byte(in.scratch), 0, 0); err != nil {
		return err
	}
	return in.fake(xproto.KeyRelease, byte(in.scratch), 0, 0)
}

// mapScratch points the scratch keycode at sym, if it is not already there.
//
// Both the first two columns get the symbol, so the key produces it whether or
// not Shift happens to be held. That is what makes this safe to use while a
// modifier is down -- typing an upper-case letter while the user holds Shift
// would otherwise come out as its unshifted form.
//
// The Check() is not merely error handling: it is the round trip that proves the
// server has applied the new mapping before the key event that depends on it is
// sent. Requests are processed in order, so once this has been answered the
// mapping is in effect. Without it the keystroke would race the remap.
func (in *x11Injector) mapScratch(sym xproto.Keysym) error {
	if in.scratchSym == sym {
		return nil
	}

	syms := make([]xproto.Keysym, in.keysymsPerKeycode)
	syms[0] = sym
	if len(syms) > 1 {
		syms[1] = sym
	}
	if err := xproto.ChangeKeyboardMappingChecked(in.d.conn, 1, in.scratch, in.keysymsPerKeycode, syms).Check(); err != nil {
		return fmt.Errorf("remapping keycode %d to %#x: %w", in.scratch, sym, err)
	}
	in.scratchSym = sym
	return nil
}

// fake issues one XTEST input event. The caller must hold in.mu, which is what
// serialises requests on the connection.
func (in *x11Injector) fake(event, detail byte, x, y int) error {
	if err := xtest.FakeInputChecked(in.d.conn, event, detail, xproto.TimeCurrentTime,
		in.d.root, int16(x), int16(y), 0).Check(); err != nil {
		return fmt.Errorf("XTEST FakeInput: %w", err)
	}
	return nil
}
