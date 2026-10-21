package server

import (
	"strings"
	"testing"
)

func TestClipboardTextFits(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty", "", true},
		{"one byte under", strings.Repeat("a", MaxClipboardBytes-1), true},
		{"exactly at the limit", strings.Repeat("a", MaxClipboardBytes), true},
		{"one byte over", strings.Repeat("a", MaxClipboardBytes+1), false},
		{"far over", strings.Repeat("a", MaxClipboardBytes*2), false},
		// The limit bounds the wire message, so it counts bytes, not runes.
		// A rune-count check would wave this through at three times the size.
		{"multi-byte at the limit", strings.Repeat("中", MaxClipboardBytes/3), true},
		{"multi-byte over the limit", strings.Repeat("中", MaxClipboardBytes/3+1), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clipboardTextFits(tc.text); got != tc.want {
				t.Errorf("clipboardTextFits(len=%d bytes, %d runes) = %v, want %v",
					len(tc.text), len([]rune(tc.text)), got, tc.want)
			}
		})
	}
}
