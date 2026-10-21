//go:build windows

// Package server implements the screen capture, input injection, WebSocket hub
// and authentication used by the remote desktop service.
//
// The Win32 layer is written with plain syscall bindings (no CGO) so the whole
// program builds with CGO_ENABLED=0 and ships as a single self-contained .exe.
package server

import (
	"encoding/binary"
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetDC                         = user32.NewProc("GetDC")
	procReleaseDC                     = user32.NewProc("ReleaseDC")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSendInput                     = user32.NewProc("SendInput")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procStretchBlt         = gdi32.NewProc("StretchBlt")
	procSetStretchBltMode  = gdi32.NewProc("SetStretchBltMode")
	procSetBrushOrgEx      = gdi32.NewProc("SetBrushOrgEx")
	procGdiFlush           = gdi32.NewProc("GdiFlush")
	procGetDeviceCaps      = gdi32.NewProc("GetDeviceCaps")

	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")

	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procEmptyClipboard             = user32.NewProc("EmptyClipboard")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procSetClipboardData           = user32.NewProc("SetClipboardData")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")

	// RtlMoveMemory is kernel32's memmove, exported under its ntdll name. It is
	// here so clipboard blocks can be copied without ever being turned into Go
	// pointers -- see ReadClipboardText.
	procRtlMoveMemory = kernel32.NewProc("RtlMoveMemory")
)

// utf8CodePage is CP_UTF8.
const utf8CodePage = 65001

// EnableUTF8Console switches the attached console to UTF-8.
//
// Without this, a double-clicked .exe runs under the system's legacy codepage
// (936/GBK on a Simplified Chinese Windows) and every Chinese log line and
// banner entry renders as mojibake. The call is harmless when stdout is a pipe
// or a file, where it simply has no effect.
func EnableUTF8Console() {
	if err := procSetConsoleOutputCP.Find(); err == nil {
		procSetConsoleOutputCP.Call(utf8CodePage)
	}
	if err := procSetConsoleCP.Find(); err == nil {
		procSetConsoleCP.Call(utf8CodePage)
	}
}

// GetSystemMetrics indices.
const (
	smCXScreen = 0
	smCYScreen = 1
)

// GetDeviceCaps index.
const bitsPixel = 12

// BitBlt / StretchBlt raster operations.
const (
	srcCopy      = 0x00CC0020
	captureBlt   = 0x40000000
	halfTone     = 3 // HALFTONE, used with SetStretchBltMode for smooth downscale
	dibRGBColors = 0
)

// DPI awareness contexts (Windows 10 1703+).
const dpiAwarenessPerMonitorAwareV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == -4

var win32Once sync.Once

// EnableDPIAwareness must be called before any window/metrics API is used.
//
// Without it, Windows lies about the screen size on scaled displays: the
// process is given a virtualised, DPI-scaled desktop, so captures come back
// blurry and mouse injection lands on the wrong pixel.
func EnableDPIAwareness() {
	win32Once.Do(func() {
		if err := procSetProcessDpiAwarenessContext.Find(); err == nil {
			r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorAwareV2)
			if r != 0 {
				slog.Debug("dpi awareness: per-monitor v2")
				return
			}
		}
		if err := procSetProcessDPIAware.Find(); err == nil {
			if r, _, _ := procSetProcessDPIAware.Call(); r != 0 {
				slog.Debug("dpi awareness: system aware")
				return
			}
		}
		slog.Warn("could not enable DPI awareness; captures may be scaled")
	})
}

// ScreenSize returns the primary monitor size in physical pixels.
func ScreenSize() (int, int) {
	w, _, _ := procGetSystemMetrics.Call(smCXScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYScreen)
	return int(w), int(h)
}

// ScreenBitsPerPixel reports the desktop's colour depth, which differs between
// machines and is worth having in a capture failure report.
func ScreenBitsPerPixel(hdc uintptr) int {
	bpp, _, _ := procGetDeviceCaps.Call(hdc, bitsPixel)
	return int(bpp)
}

// ---------------------------------------------------------------------------
// Input injection
// ---------------------------------------------------------------------------

// INPUT.dwType values.
const (
	inputMouse    = 0
	inputKeyboard = 1
)

// Mouse event flags.
const (
	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfXDown      = 0x0080
	mouseeventfXUp        = 0x0100
	mouseeventfWheel      = 0x0800
	mouseeventfHWheel     = 0x1000
	mouseeventfAbsolute   = 0x8000
	xbutton1              = 0x0001
	xbutton2              = 0x0002
)

// Keyboard event flags.
const (
	keyeventfExtendedKey = 0x0001
	keyeventfKeyUp       = 0x0002
	keyeventfUnicode     = 0x0004
)

// The INPUT struct is a tagged union whose layout depends on the pointer size.
// Rather than mirror that with build-tagged Go structs, the record is assembled
// into a byte slice at explicit offsets. This keeps one code path that is
// provably correct on both amd64 (40-byte INPUT) and 386 (28-byte INPUT).
var (
	ptrSize   = int(unsafe.Sizeof(uintptr(0)))
	unionOff  = 8 // offset of the union: DWORD type + pointer-size padding
	inputSize = 40
	// Offsets relative to the start of the union.
	mouseDXOff, mouseDataOff, mouseFlagsOff, mouseTimeOff, mouseExtraOff int
	keyVKOff, keyScanOff, keyFlagsOff, keyTimeOff, keyExtraOff           int
)

func init() {
	if ptrSize == 4 {
		unionOff = 4
		inputSize = 28
	}
	// MOUSEINPUT { LONG dx, dy; DWORD mouseData, dwFlags, time; ULONG_PTR dwExtraInfo; }
	mouseDXOff = unionOff
	mouseDataOff = unionOff + 8
	mouseFlagsOff = unionOff + 12
	mouseTimeOff = unionOff + 16
	mouseExtraOff = align(unionOff+20, ptrSize)
	// KEYBDINPUT { WORD wVk, wScan; DWORD dwFlags, time; ULONG_PTR dwExtraInfo; }
	keyVKOff = unionOff
	keyScanOff = unionOff + 2
	keyFlagsOff = unionOff + 4
	keyTimeOff = unionOff + 8
	keyExtraOff = align(unionOff+16, ptrSize)
}

func align(v, a int) int { return (v + a - 1) &^ (a - 1) }

// inputBuf is a reusable INPUT record. SendInput is synchronous for our
// purposes, so a per-goroutine buffer is enough; callers below are all
// serialised by the input mutex.
type inputBuf [40]byte

func (b *inputBuf) bytes() []byte { return b[:inputSize] }

func (b *inputBuf) setType(t uint32) {
	binary.LittleEndian.PutUint32(b.bytes(), t)
	// Zero the rest so no stale field from a previous event leaks through.
	for i := 4; i < inputSize; i++ {
		b[i] = 0
	}
}

func (b *inputBuf) send() error {
	n, _, err := procSendInput.Call(1, uintptr(unsafe.Pointer(b)), uintptr(inputSize))
	if n != 1 {
		return err
	}
	return nil
}
