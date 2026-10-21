//go:build linux

package server

import (
	"strconv"
	"testing"
)

// TestKeyMapLinux mirrors TestKeyMap in hub_test.go, which is Windows-only and
// therefore never runs on the machine this backend ships to.
//
// The shared contract between the two tables is the set of KeyboardEvent.code
// values below, so they are spelled out rather than sampled: a key that resolves
// on Windows and silently stops resolving on Linux is the failure this test
// exists to catch, and it is invisible from either side alone.
func TestKeyMapLinux(t *testing.T) {
	codes := []string{
		"KeyA", "KeyZ", "Digit0", "Digit9",
		"Backquote", "Minus", "Equal", "BracketLeft", "BracketRight",
		"Backslash", "Semicolon", "Quote", "Comma", "Period", "Slash",
		"IntlBackslash", "IntlYen",
		"Enter", "NumpadEnter", "Tab", "Space", "Backspace", "Escape",
		"Delete", "Insert",
		"Home", "End", "PageUp", "PageDown",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"ShiftLeft", "ShiftRight", "ControlLeft", "ControlRight",
		"AltLeft", "AltRight", "MetaLeft", "MetaRight",
		"ContextMenu", "CapsLock", "NumLock", "ScrollLock", "PrintScreen", "Pause",
		"Numpad0", "Numpad9", "NumpadMultiply", "NumpadAdd", "NumpadSubtract",
		"NumpadDecimal", "NumpadDivide", "NumpadComma", "NumpadEqual",
	}
	for i := 1; i <= 24; i++ {
		codes = append(codes, "F"+strconv.Itoa(i))
	}

	for _, code := range codes {
		sym, extended, ok := LookupKey(code, "")
		if !ok {
			t.Errorf("LookupKey(%q) does not resolve on Linux", code)
			continue
		}
		if extended {
			t.Errorf("LookupKey(%q) reported extended; X11 keysyms are unambiguous and extended means nothing here", code)
		}
		if sym == 0 {
			t.Errorf("LookupKey(%q) resolved to keysym 0, which is NoSymbol", code)
		}
	}
}

// TestKeyMapLinuxExact checks the values themselves, not merely that something
// came back. A table that resolves every code to the same wrong keysym would
// pass the coverage test above.
func TestKeyMapLinuxExact(t *testing.T) {
	cases := []struct {
		code, key string
		wantSym   uint16
	}{
		{"KeyA", "a", 0x0061},
		{"KeyZ", "z", 0x007a},
		{"Digit1", "1", 0x0031},
		{"Enter", "Enter", 0xff0d},
		// The one place the two platforms deliberately differ: Windows folds
		// this onto VK_RETURN, X11 keeps the numpad key separate.
		{"NumpadEnter", "Enter", 0xff8d},
		{"ArrowUp", "ArrowUp", 0xff52},
		{"ControlLeft", "Control", 0xffe3},
		{"MetaLeft", "Meta", 0xffeb},
		{"F12", "F12", 0xffbe + 11},
		{"Semicolon", ";", 0x003b},
		{"Space", " ", 0x0020},
		{"", "k", 0x006b}, // fallback to `key`
		{"Unidentified", "Unidentified", 0},
	}
	for _, tc := range cases {
		sym, _, ok := LookupKey(tc.code, tc.key)
		wantOK := tc.wantSym != 0
		if ok != wantOK || sym != tc.wantSym {
			t.Errorf("LookupKey(%q,%q) = (%#x, ok=%v), want (%#x, ok=%v)",
				tc.code, tc.key, sym, ok, tc.wantSym, wantOK)
		}
	}
}

// TestKeyMapLinuxFitsUint16 guards the type the Injector interface is declared
// with.
//
// LookupKey returns a uint16 because the Windows backend needs nothing wider: a
// virtual-key is a byte. X11 keysyms are 32-bit, and everything this table maps
// happens to sit below 0x10000 -- but "happens to" is exactly the kind of thing
// that stops being true when someone adds an XF86 media key or a keysym in the
// Unicode range. Truncating one of those would inject an unrelated key rather
// than fail, so the limit is asserted instead of assumed.
func TestKeyMapLinuxFitsUint16(t *testing.T) {
	for code, sym := range codeToSym {
		if sym > 0xffff {
			t.Errorf("codeToSym[%q] = %#x exceeds the uint16 LookupKey returns", code, sym)
		}
	}
	for i := 0; i < 24; i++ {
		code := "F" + strconv.Itoa(i+1)
		if sym, ok := codeToSym[code]; !ok {
			t.Errorf("codeToSym is missing %q", code)
		} else if sym > 0xffff {
			t.Errorf("codeToSym[%q] = %#x exceeds the uint16 LookupKey returns", code, sym)
		}
	}
}
