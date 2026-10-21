package server

import "bytes"

// frameDiffer detects a static screen by comparing each captured frame against
// the last one that was actually sent.
//
// Skipping the encode and the broadcast for an unchanged frame is where the
// idle bandwidth saving comes from: a still desktop sends nothing at all,
// instead of re-sending the same JPEG fifteen times a second.
//
// The comparison short-circuits on the first differing byte, so a moving screen
// costs almost nothing. Only a genuinely static frame pays for the full scan.
type frameDiffer struct {
	prev   []byte
	stride int
	w, h   int
}

// Changed reports whether pix differs from the last frame passed to MarkSent.
//
// A different geometry always counts as changed: a resolution or scale change
// has to be resent even in the unlikely case that the pixels compare equal.
func (d *frameDiffer) Changed(pix []byte, stride, w, h int) bool {
	if d.prev == nil || d.stride != stride || d.w != w || d.h != h || len(d.prev) != len(pix) {
		return true
	}
	return !bytes.Equal(d.prev, pix)
}

// Forget drops the remembered frame, so the next Changed reports true.
//
// It must be called whenever the encoder's settings change. The pixels are the
// same, but the JPEG that represents them is not, and without this a static
// screen would keep the old quality on display indefinitely -- exactly while
// someone is trying to read it. It is also the safe response to a failed
// encode: the frame still has to go out.
func (d *frameDiffer) Forget() {
	// Truncate rather than nil out, so the buffer is reused instead of being
	// reallocated on the next MarkSent.
	d.prev = d.prev[:0]
	d.stride, d.w, d.h = 0, 0, 0
}

// MarkSent records pix as the frame the clients have now seen.
//
// It copies, and it must: the capturer hands back a buffer it reuses, so the
// next Grab overwrites pix in place.
func (d *frameDiffer) MarkSent(pix []byte, stride, w, h int) {
	if cap(d.prev) < len(pix) {
		d.prev = make([]byte, len(pix))
	}
	d.prev = d.prev[:len(pix)]
	copy(d.prev, pix)
	d.stride, d.w, d.h = stride, w, h
}
