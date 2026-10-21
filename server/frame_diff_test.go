package server

import "testing"

func TestFrameDifferDetectsChanges(t *testing.T) {
	const w, h = 4, 3
	const stride = w * 4

	base := make([]byte, stride*h)
	for i := range base {
		base[i] = byte(i)
	}

	var d frameDiffer

	// Nothing to compare against yet, so the first frame must go out.
	if !d.Changed(base, stride, w, h) {
		t.Fatal("first frame: Changed() = false, want true")
	}
	d.MarkSent(base, stride, w, h)

	// The same pixels again: this is the case that saves the bandwidth.
	if d.Changed(base, stride, w, h) {
		t.Error("identical frame: Changed() = true, want false")
	}

	moved := append([]byte(nil), base...)
	moved[17] ^= 0x01
	if !d.Changed(moved, stride, w, h) {
		t.Error("one byte flipped: Changed() = false, want true")
	}
}

// MarkSent must copy, because the capturer reuses one buffer and the next Grab
// overwrites it in place. Storing a reference would make every frame look
// unchanged as soon as the comparison buffer was aliased to the live one.
func TestFrameDifferCopiesWhatItRemembers(t *testing.T) {
	const w, h = 4, 2
	const stride = w * 4

	original := make([]byte, stride*h)
	for i := range original {
		original[i] = 0x11
	}

	var d frameDiffer
	scratch := append([]byte(nil), original...)
	d.MarkSent(scratch, stride, w, h)

	// Simulate the next Grab overwriting the same backing array.
	for i := range scratch {
		scratch[i] = 0xAA
	}

	if d.Changed(original, stride, w, h) {
		t.Error("MarkSent kept a reference to the caller's buffer instead of copying")
	}
}

func TestFrameDifferGeometryChangesAreAlwaysSent(t *testing.T) {
	const w, h = 16, 1
	const stride = w * 4
	pix := make([]byte, stride*h)

	var d frameDiffer
	d.MarkSent(pix, stride, w, h)

	if !d.Changed(pix, stride, 8, 2) {
		t.Error("width/height change: Changed() = false, want true")
	}
	if !d.Changed(pix, 8, w, h) {
		t.Error("stride change: Changed() = false, want true")
	}
	if !d.Changed(make([]byte, 32*4), 32*4, 32, 1) {
		t.Error("buffer length change: Changed() = false, want true")
	}
	if d.Changed(pix, stride, w, h) {
		t.Error("unchanged geometry and pixels: Changed() = true, want false")
	}
}
