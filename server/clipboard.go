package server

import "errors"

// ErrUnsupportedClipboard is returned on platforms without a clipboard backend.
var ErrUnsupportedClipboard = errors.New("clipboard access is not supported on this platform")

// MaxClipboardBytes caps what crosses the wire in either direction.
//
// The clipboard is the one input whose size the user does not choose: copying a
// whole document is one keystroke. This is far above any realistic paste and far
// below anything that would stall the video socket the reply travels on.
//
// It is deliberately a quarter of maxMessageSize. That limit is enforced by the
// websocket library *before* this package sees the message, and exceeding it
// closes the connection rather than returning an error -- so an oversized paste
// would not be rejected, it would drop the session. Keeping the cap well under
// it leaves room for the JSON envelope and for escaping.
const MaxClipboardBytes = 256 << 10

// errClipboardTooLarge is what the client shows when text is refused for size.
// Kept here rather than in the Windows file so its wording is testable and the
// two sides cannot drift apart.
const errClipboardTooLarge = "剪贴板内容过大（上限 256 KB）"

// clipboardTextFits reports whether text may be moved between the two machines.
//
// The limit is on bytes, not runes: it exists to bound the JSON message, and a
// string of Chinese costs three bytes per character on the wire, so a rune count
// would bound the wrong thing.
func clipboardTextFits(text string) bool {
	return len(text) <= MaxClipboardBytes
}
