//go:build windows

package server

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"sync"
	"unicode/utf16"
)

// Windows virtual-key codes used by the key map below.
const (
	vkBack     = 0x08
	vkTab      = 0x09
	vkReturn   = 0x0D
	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12
	vkPause    = 0x13
	vkCapital  = 0x14
	vkEscape   = 0x1B
	vkSpace    = 0x20
	vkPrior    = 0x21
	vkNext     = 0x22
	vkEnd      = 0x23
	vkHome     = 0x24
	vkLeft     = 0x25
	vkUp       = 0x26
	vkRight    = 0x27
	vkDown     = 0x28
	vkSnapshot = 0x2C
	vkInsert   = 0x2D
	vkDelete   = 0x2E
	vkLWin     = 0x5B
	vkRWin     = 0x5C
	vkApps     = 0x5D
	vkNum0     = 0x60
	vkMultiply = 0x6A
	vkAdd      = 0x6B
	vkSubtract = 0x6D
	vkDecimal  = 0x6E
	vkDivide   = 0x6F
	vkF1       = 0x70
	vkNumLock  = 0x90
	vkScroll   = 0x91
	vkLShift   = 0xA0
	vkRShift   = 0xA1
	vkLControl = 0xA2
	vkRControl = 0xA3
	vkLMenu    = 0xA4
	vkRMenu    = 0xA5
)

// keyDef describes how a browser KeyboardEvent.code maps onto a Windows
// virtual-key. extended is set for keys that must carry KEYEVENTF_EXTENDEDKEY
// (the navigation cluster, right-hand modifiers, numpad divide/enter).
type keyDef struct {
	vk       uint16
	extended bool
}

// codeToKey maps KeyboardEvent.code -> Windows VK.
//
// Physical `code` is used rather than the layout-dependent `key` because it is
// the same on every client keyboard layout, matching the semantics of injecting
// a key press on the remote machine.
var codeToKey = map[string]keyDef{
	// Letters
	"KeyA": {0x41, false}, "KeyB": {0x42, false}, "KeyC": {0x43, false},
	"KeyD": {0x44, false}, "KeyE": {0x45, false}, "KeyF": {0x46, false},
	"KeyG": {0x47, false}, "KeyH": {0x48, false}, "KeyI": {0x49, false},
	"KeyJ": {0x4A, false}, "KeyK": {0x4B, false}, "KeyL": {0x4C, false},
	"KeyM": {0x4D, false}, "KeyN": {0x4E, false}, "KeyO": {0x4F, false},
	"KeyP": {0x50, false}, "KeyQ": {0x51, false}, "KeyR": {0x52, false},
	"KeyS": {0x53, false}, "KeyT": {0x54, false}, "KeyU": {0x55, false},
	"KeyV": {0x56, false}, "KeyW": {0x57, false}, "KeyX": {0x58, false},
	"KeyY": {0x59, false}, "KeyZ": {0x5A, false},

	// Digits (top row)
	"Digit0": {0x30, false}, "Digit1": {0x31, false}, "Digit2": {0x32, false},
	"Digit3": {0x33, false}, "Digit4": {0x34, false}, "Digit5": {0x35, false},
	"Digit6": {0x36, false}, "Digit7": {0x37, false}, "Digit8": {0x38, false},
	"Digit9": {0x39, false},

	// OEM punctuation
	"Backquote":     {0xC0, false},
	"Minus":         {0xBD, false},
	"Equal":         {0xBB, false},
	"BracketLeft":   {0xDB, false},
	"BracketRight":  {0xDD, false},
	"Backslash":     {0xDC, false},
	"Semicolon":     {0xBA, false},
	"Quote":         {0xDE, false},
	"Comma":         {0xBC, false},
	"Period":        {0xBE, false},
	"Slash":         {0xBF, false},
	"IntlBackslash": {0xE2, false},
	"IntlRo":        {0xC1, false},
	"IntlYen":       {0xDC, false},

	// Whitespace / editing
	"Enter":       {vkReturn, false},
	"NumpadEnter": {vkReturn, true},
	"Tab":         {vkTab, false},
	"Space":       {vkSpace, false},
	"Backspace":   {vkBack, false},
	"Escape":      {vkEscape, false},
	"Delete":      {vkDelete, true},
	"Insert":      {vkInsert, true},

	// Navigation cluster
	"Home":       {vkHome, true},
	"End":        {vkEnd, true},
	"PageUp":     {vkPrior, true},
	"PageDown":   {vkNext, true},
	"ArrowUp":    {vkUp, true},
	"ArrowDown":  {vkDown, true},
	"ArrowLeft":  {vkLeft, true},
	"ArrowRight": {vkRight, true},

	// Modifiers
	"ShiftLeft":    {vkLShift, false},
	"ShiftRight":   {vkRShift, false},
	"ControlLeft":  {vkLControl, false},
	"ControlRight": {vkRControl, true},
	"AltLeft":      {vkLMenu, false},
	"AltRight":     {vkRMenu, true},
	"MetaLeft":     {vkLWin, true},
	"MetaRight":    {vkRWin, true},
	"ContextMenu":  {vkApps, true},
	"CapsLock":     {vkCapital, false},
	"NumLock":      {vkNumLock, true},
	"ScrollLock":   {vkScroll, false},
	"PrintScreen":  {vkSnapshot, true},
	"Pause":        {vkPause, false},

	// Numpad
	"Numpad0": {vkNum0 + 0, false}, "Numpad1": {vkNum0 + 1, false},
	"Numpad2": {vkNum0 + 2, false}, "Numpad3": {vkNum0 + 3, false},
	"Numpad4": {vkNum0 + 4, false}, "Numpad5": {vkNum0 + 5, false},
	"Numpad6": {vkNum0 + 6, false}, "Numpad7": {vkNum0 + 7, false},
	"Numpad8": {vkNum0 + 8, false}, "Numpad9": {vkNum0 + 9, false},
	"NumpadMultiply": {vkMultiply, false},
	"NumpadAdd":      {vkAdd, false},
	"NumpadSubtract": {vkSubtract, false},
	"NumpadDecimal":  {vkDecimal, false},
	"NumpadDivide":   {vkDivide, true},
	"NumpadComma":    {0x6C, false},
	"NumpadEqual":    {0x92, false},
}

func init() {
	// Function keys F1..F24 are contiguous from VK_F1.
	for i := 0; i < 24; i++ {
		codeToKey["F"+strconv.Itoa(i+1)] = keyDef{vkF1 + uint16(i), false}
	}
}

// LookupKey resolves a browser key event to a Windows virtual-key.
// code is KeyboardEvent.code; key is KeyboardEvent.key, used as a fallback.
func LookupKey(code, key string) (vk uint16, extended bool, ok bool) {
	if d, found := codeToKey[code]; found {
		return d.vk, d.extended, true
	}
	// Some clients (and synthetic events) only provide `key`.
	if d, found := codeToKey[key]; found {
		return d.vk, d.extended, true
	}
	if len(key) == 1 {
		switch c := key[0]; {
		case c >= 'a' && c <= 'z':
			return uint16(c - 'a' + 'A'), false, true
		case c >= 'A' && c <= 'Z':
			return uint16(c), false, true
		case c >= '0' && c <= '9':
			return uint16(c), false, true
		}
	}
	return 0, false, false
}

// winInjector injects input through user32!SendInput.
//
// All calls are serialised: SendInput has no per-caller state, but the shared
// scratch INPUT buffer below does.
type winInjector struct {
	mu  sync.Mutex
	buf inputBuf
}

// NewInjector returns the Windows input backend.
func NewInjector() Injector { return &winInjector{} }

func (in *winInjector) Close() error { return nil }

func (in *winInjector) ScreenSize() (int, int) { return ScreenSize() }

// MoveTo moves the cursor to absolute primary-monitor pixel coordinates.
//
// MOUSEEVENTF_ABSOLUTE takes a normalised 0..65535 range covering the primary
// monitor (VIRTUALDESK is deliberately not set, since capture covers only the
// primary monitor).
func (in *winInjector) MoveTo(x, y int) error {
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

	b := &in.buf
	b.setType(inputMouse)
	raw := b.bytes()
	binary.LittleEndian.PutUint32(raw[mouseDXOff:], uint32(int32(x*65535/(sw-1))))
	binary.LittleEndian.PutUint32(raw[mouseDXOff+4:], uint32(int32(y*65535/(sh-1))))
	binary.LittleEndian.PutUint32(raw[mouseFlagsOff:], mouseeventfMove|mouseeventfAbsolute)
	return b.send()
}

func (in *winInjector) Button(button string, down bool) error {
	var flag uint32
	var xdata uint32

	switch button {
	case "", ButtonLeft:
		flag = mouseeventfLeftDown
		if !down {
			flag = mouseeventfLeftUp
		}
	case ButtonRight:
		flag = mouseeventfRightDown
		if !down {
			flag = mouseeventfRightUp
		}
	case ButtonMiddle:
		flag = mouseeventfMiddleDown
		if !down {
			flag = mouseeventfMiddleUp
		}
	case ButtonBack:
		flag = mouseeventfXDown
		xdata = xbutton1
		if !down {
			flag = mouseeventfXUp
		}
	case ButtonFwd:
		flag = mouseeventfXDown
		xdata = xbutton2
		if !down {
			flag = mouseeventfXUp
		}
	default:
		return fmt.Errorf("unknown mouse button %q", button)
	}

	in.mu.Lock()
	defer in.mu.Unlock()

	b := &in.buf
	b.setType(inputMouse)
	raw := b.bytes()
	binary.LittleEndian.PutUint32(raw[mouseDataOff:], xdata)
	binary.LittleEndian.PutUint32(raw[mouseFlagsOff:], flag)
	return b.send()
}

// Wheel scrolls the remote desktop. Deltas are in Windows wheel units.
func (in *winInjector) Wheel(deltaX, deltaY int) error {
	in.mu.Lock()
	defer in.mu.Unlock()

	b := &in.buf
	raw := b.bytes()

	if deltaY != 0 {
		b.setType(inputMouse)
		binary.LittleEndian.PutUint32(raw[mouseDataOff:], uint32(int32(deltaY)))
		binary.LittleEndian.PutUint32(raw[mouseFlagsOff:], mouseeventfWheel)
		if err := b.send(); err != nil {
			return err
		}
	}
	if deltaX != 0 {
		b.setType(inputMouse)
		binary.LittleEndian.PutUint32(raw[mouseDataOff:], uint32(int32(deltaX)))
		binary.LittleEndian.PutUint32(raw[mouseFlagsOff:], mouseeventfHWheel)
		if err := b.send(); err != nil {
			return err
		}
	}
	return nil
}

func (in *winInjector) Key(vk uint16, extended, down bool) error {
	var flags uint32
	if extended {
		flags |= keyeventfExtendedKey
	}
	if !down {
		flags |= keyeventfKeyUp
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	return in.sendKey(vk, 0, flags)
}

// sendKey fills the scratch INPUT record as a keyboard event. Callers must hold
// in.mu.
func (in *winInjector) sendKey(vk, scan uint16, flags uint32) error {
	b := &in.buf
	b.setType(inputKeyboard)
	raw := b.bytes()
	binary.LittleEndian.PutUint16(raw[keyVKOff:], vk)
	binary.LittleEndian.PutUint16(raw[keyScanOff:], scan)
	binary.LittleEndian.PutUint32(raw[keyFlagsOff:], flags)
	return b.send()
}

// Text types a string into the focused window using KEYEVENTF_UNICODE.
//
// This bypasses the remote keyboard layout and, crucially, the remote IME:
// each UTF-16 code unit is delivered as a literal character. That makes
// browser-side IME composition work -- the candidate window lives in the
// client's own input method and only the committed string crosses the wire.
func (in *winInjector) Text(s string) error {
	if s == "" {
		return nil
	}
	// Surrogate pairs are sent as their individual UTF-16 code units, which is
	// exactly how a real Unicode keyboard reports a non-BMP character.
	units := utf16.Encode([]rune(s))

	in.mu.Lock()
	defer in.mu.Unlock()

	for _, u := range units {
		if err := in.sendKey(0, u, keyeventfUnicode); err != nil {
			return err
		}
		if err := in.sendKey(0, u, keyeventfUnicode|keyeventfKeyUp); err != nil {
			return err
		}
	}
	return nil
}
