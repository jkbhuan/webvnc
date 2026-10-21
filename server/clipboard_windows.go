//go:build windows

package server

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Clipboard formats, allocation flags and limits.
const (
	// cfUnicodeText is the only format this program moves. Anything else on the
	// clipboard (a bitmap, a file list) is reported as "no text" rather than
	// converted -- see the package docs on what is deliberately out of scope.
	cfUnicodeText = 13

	// gmemMoveable is required for any handle handed to SetClipboardData: the
	// clipboard takes ownership of the block and may move it afterwards.
	gmemMoveable = 0x0002

	// maxClipboardUnits bounds the UTF-16 block we are willing to copy, in
	// 16-bit units. It is deliberately looser than MaxClipboardBytes: ASCII
	// costs one unit per byte while Chinese costs one unit per three, so a text
	// that passes the UTF-8 check can be up to this long. Refusing anything
	// above it therefore never refuses a paste that would have been allowed --
	// it only bounds the allocation for a block some other program made huge.
	maxClipboardUnits = MaxClipboardBytes
)

// How long to keep trying to open the clipboard.
//
// Only one process can hold the clipboard open at a time, and the window is
// short -- another app is usually mid-copy. Failing immediately would make the
// feature look broken at random; a couple of hundred milliseconds of retrying
// covers essentially every real contention.
const (
	clipboardOpenAttempts = 10
	clipboardOpenBackoff  = 20 * time.Millisecond
)

// ErrClipboardBusy means another process held the clipboard open for longer
// than we were willing to wait.
var ErrClipboardBusy = errors.New("剪贴板被其他程序占用")

// winErr renders the reason a Win32 call gave for failing.
//
// A LazyProc.Call always returns a non-nil error, but one that failed without
// setting a last error reports Errno(0) -- whose message is the actively
// misleading "The operation completed successfully." This text ends up in the
// browser's status line, where that sentence would send the reader looking in
// entirely the wrong place, so the numeric code is shown instead.
func winErr(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	if errno == 0 {
		return "系统未报告错误码"
	}
	return fmt.Sprintf("%v（错误码 %d）", errno, int(errno))
}

// clipboardMu serialises every open/close bracket in this process.
//
// The clipboard is one global exclusive lock, and it is not reentrant: a second
// thread fails to open it while the first still holds it, even inside this same
// process. The browser opens several connections at once, so two clients can
// press Ctrl+C together. The retry loop below would still get the loser through
// eventually, but making the wait explicit turns that from "fail after 200ms"
// into "succeed a few milliseconds later".
var clipboardMu sync.Mutex

// openClipboard opens the clipboard, retrying while another process holds it.
//
// The returned cleanup must be called exactly once on every path once this
// succeeds, including error paths: leaving the clipboard open locks every other
// application out of copy and paste until this process exits.
//
// NULL as the owner window is legal and associates the clipboard with the
// calling task. It needs neither a window nor a message loop, which is what
// makes this work from a plain console program.
func openClipboard() (func(), error) {
	clipboardMu.Lock()

	for attempt := 0; attempt < clipboardOpenAttempts; attempt++ {
		ok, _, _ := procOpenClipboard.Call(0)
		if ok != 0 {
			return func() {
				procCloseClipboard.Call()
				clipboardMu.Unlock()
			}, nil
		}
		if attempt < clipboardOpenAttempts-1 {
			time.Sleep(clipboardOpenBackoff)
		}
	}

	clipboardMu.Unlock()
	return nil, ErrClipboardBusy
}

// ReadClipboardText returns the clipboard's text, or an empty string when the
// clipboard holds no text at all.
func ReadClipboardText() (string, error) {
	release, err := openClipboard()
	if err != nil {
		return "", err
	}
	defer release()

	// Ask before fetching: GetClipboardData returns NULL both for "no such
	// format" and for a genuine failure, and those deserve different messages.
	if ok, _, _ := procIsClipboardFormatAvailable.Call(cfUnicodeText); ok == 0 {
		return "", nil
	}

	h, _, err := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", fmt.Errorf("读取剪贴板失败: %s", winErr(err))
	}

	// The handle belongs to the clipboard and must not be freed, and it stays
	// valid only until CloseClipboard -- hence the copy below rather than a
	// slice that outlives the release.
	src, _, err := procGlobalLock.Call(h)
	if src == 0 {
		return "", fmt.Errorf("锁定剪贴板内存失败: %s", winErr(err))
	}
	defer procGlobalUnlock.Call(h)

	size, _, _ := procGlobalSize.Call(h)
	units := int(size / 2)
	if units == 0 {
		return "", nil
	}
	if units > maxClipboardUnits {
		return "", errors.New(errClipboardTooLarge)
	}

	// The block is copied with RtlMoveMemory rather than by reinterpreting the
	// handle as a Go pointer. Two reasons: converting a uintptr back into a
	// pointer is what vet rightly distrusts (see comCall in audio_windows.go),
	// and the clipboard block is not Go memory, so nothing good comes of
	// pretending otherwise. The copy takes a uintptr as a call argument, which
	// is the direction that is safe.
	buf := make([]uint16, units)
	procRtlMoveMemory.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		src,
		uintptr(units*2),
	)

	// The block is the allocation size, not the string length, and is not
	// guaranteed to be NUL-terminated: scan within the allocation instead of
	// trusting either.
	end := 0
	for end < units && buf[end] != 0 {
		end++
	}

	text := string(utf16.Decode(buf[:end]))
	if !clipboardTextFits(text) {
		return "", errors.New(errClipboardTooLarge)
	}
	return text, nil
}

// WriteClipboardText replaces the clipboard's contents with text.
func WriteClipboardText(text string) error {
	if !clipboardTextFits(text) {
		return errors.New(errClipboardTooLarge)
	}

	release, err := openClipboard()
	if err != nil {
		return err
	}
	defer release()

	// EmptyClipboard may only be called by the clipboard owner, so it has to
	// follow OpenClipboard. It also frees whatever the previous contents were.
	if ok, _, err := procEmptyClipboard.Call(); ok == 0 {
		return fmt.Errorf("清空剪贴板失败: %s", winErr(err))
	}

	// The trailing NUL is part of the format, so the block is one unit longer
	// than the string.
	buf := append(utf16.Encode([]rune(text)), 0)
	nbytes := uintptr(len(buf) * 2)

	h, _, err := procGlobalAlloc.Call(gmemMoveable, nbytes)
	if h == 0 {
		return fmt.Errorf("分配剪贴板内存失败: %s", winErr(err))
	}

	dst, _, err := procGlobalLock.Call(h)
	if dst == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("锁定剪贴板内存失败: %s", winErr(err))
	}

	procRtlMoveMemory.Call(dst, uintptr(unsafe.Pointer(&buf[0])), nbytes)
	// buf's last mention is the uintptr above, which the collector does not
	// count as a reference: without this it may be freed during the call.
	runtime.KeepAlive(buf)

	// The block must be unlocked before it is handed over: the clipboard may
	// move it, and a locked block cannot be moved. The return value is
	// deliberately ignored -- GlobalUnlock reports failure when the lock count
	// merely reaches zero, which is the normal outcome here.
	procGlobalUnlock.Call(h)

	if ok, _, err := procSetClipboardData.Call(cfUnicodeText, h); ok == 0 {
		// Handing over failed, so the block is still ours and would leak.
		procGlobalFree.Call(h)
		return fmt.Errorf("写入剪贴板失败: %s", winErr(err))
	}

	// Ownership has moved to the system. Freeing h here would be a double free.
	return nil
}
