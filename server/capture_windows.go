//go:build windows

package server

import (
	"fmt"
	"image"
	"sync"
	"unsafe"
)

// bitmapInfoHeader mirrors the Win32 BITMAPINFOHEADER.
type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// gdiCapturer captures the primary monitor with GDI.
//
// The desktop DC is opened per frame (holding one open indefinitely upsets some
// fullscreen applications), while the memory DC and its DIB section are pooled
// and only rebuilt when the display resolution or scale changes.
type gdiCapturer struct {
	mu    sync.Mutex
	scale float64

	screenW, screenH int
	outW, outH       int

	hdcMem uintptr
	hbm    uintptr
	hbmOld uintptr

	// dib aliases the DIB section's pixels. That memory belongs to GDI, not to
	// the Go heap: release() unmaps it, so nothing outside this type may keep
	// the slice -- or an image aliasing it -- past the next Grab.
	dib []byte
	img *image.RGBA
}

// NewCapturer creates the Windows screen capture backend.
//
// Scale is clamped to (0, 1]; pass 1.0 for native resolution.
func NewCapturer(cfg CaptureConfig) (Capturer, error) {
	EnableDPIAwareness()

	scale := cfg.Scale
	if scale <= 0 {
		scale = 1
	}
	if scale > 1 {
		scale = 1
	}

	c := &gdiCapturer{scale: scale}
	if err := c.rebuild(); err != nil {
		return nil, err
	}
	return c, nil
}

// rebuild (re)allocates the GDI resources for the current resolution and scale.
// The caller must not hold c.mu.
func (c *gdiCapturer) rebuild() error {
	sw, sh := ScreenSize()
	if sw <= 0 || sh <= 0 {
		return fmt.Errorf("GetSystemMetrics reported an invalid screen size %dx%d", sw, sh)
	}

	outW := int(float64(sw)*c.scale + 0.5)
	outH := int(float64(sh)*c.scale + 0.5)
	if outW < 1 {
		outW = 1
	}
	if outH < 1 {
		outH = 1
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return fmt.Errorf("GetDC(NULL) failed")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return fmt.Errorf("CreateCompatibleDC failed")
	}

	// A DIB *section*, not a CreateCompatibleBitmap DDB: GDI draws straight
	// into the mapped pixels, so a frame costs one BitBlt and no read-back.
	//
	// The read-back this replaces was BitBlt -> GetDIBits, and it failed on
	// some machines. GetDIBits requires that the bitmap *not* be selected into
	// a device context, which a pooled memory DC cannot honour without
	// select/deselect around every call; most drivers tolerate the violation
	// but some return zero (ERROR_INVALID_PARAMETER), which is why only a
	// subset of machines ever saw "GetDIBits failed". A section has no such
	// rule to break, and skips a full copy of every frame as well.
	bmi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(outW),
		Height:      -int32(outH), // negative height = top-down, matching image.RGBA
		Planes:      1,
		BitCount:    32,
		Compression: 0, // BI_RGB
		SizeImage:   uint32(outW * outH * 4),
	}
	var bits unsafe.Pointer
	hbm, _, err := procCreateDIBSection.Call(hdcScreen,
		uintptr(unsafe.Pointer(&bmi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		procDeleteDC.Call(hdcMem)
		return fmt.Errorf("CreateDIBSection failed at %dx%d on a %d bpp screen: %s",
			outW, outH, ScreenBitsPerPixel(hdcScreen), winErr(err))
	}

	hbmOld, _, _ := procSelectObject.Call(hdcMem, hbm)

	// HALFTONE gives a much better downscale than the default COLORONCOLOR.
	procSetStretchBltMode.Call(hdcMem, halfTone)
	procSetBrushOrgEx.Call(hdcMem, 0, 0, 0)

	c.release() // free any previous resources
	c.screenW, c.screenH = sw, sh
	c.outW, c.outH = outW, outH
	c.hdcMem, c.hbm, c.hbmOld = hdcMem, hbm, hbmOld

	// 32 bpp rows are DWORD-aligned already, so the stride is the width.
	c.dib = unsafe.Slice((*byte)(bits), outW*outH*4)
	c.img = &image.RGBA{Pix: c.dib, Stride: outW * 4, Rect: image.Rect(0, 0, outW, outH)}
	return nil
}

// release frees the pooled GDI objects. The caller must not hold c.mu.
func (c *gdiCapturer) release() {
	if c.hdcMem != 0 {
		if c.hbmOld != 0 {
			procSelectObject.Call(c.hdcMem, c.hbmOld)
		}
		if c.hbm != 0 {
			// Deleting the bitmap also unmaps the pixels dib aliases.
			procDeleteObject.Call(c.hbm)
		}
		procDeleteDC.Call(c.hdcMem)
	}
	c.hdcMem, c.hbm, c.hbmOld = 0, 0, 0
	c.dib, c.img = nil, nil
}

func (c *gdiCapturer) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outW, c.outH
}

func (c *gdiCapturer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.release()
}

// Grab captures one frame into a reused RGBA image.
func (c *gdiCapturer) Grab() (*image.RGBA, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The user may have changed resolution, docked a display, or toggled DPI
	// scaling since the last frame.
	if sw, sh := ScreenSize(); sw != c.screenW || sh != c.screenH {
		if err := c.rebuild(); err != nil {
			return nil, err
		}
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, fmt.Errorf("GetDC(NULL) failed")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	// CAPTUREBLT also picks up layered windows (tooltips, some overlays).
	rop := uintptr(srcCopy | captureBlt)

	var ok uintptr
	var err error
	if c.outW == c.screenW && c.outH == c.screenH {
		ok, _, err = procBitBlt.Call(
			c.hdcMem, 0, 0, uintptr(c.outW), uintptr(c.outH),
			hdcScreen, 0, 0, rop)
	} else {
		ok, _, err = procStretchBlt.Call(
			c.hdcMem, 0, 0, uintptr(c.outW), uintptr(c.outH),
			hdcScreen, 0, 0, uintptr(c.screenW), uintptr(c.screenH), rop)
	}
	if ok == 0 {
		return nil, fmt.Errorf("BitBlt/StretchBlt failed for %dx%d -> %dx%d on a %d bpp screen: %s",
			c.screenW, c.screenH, c.outW, c.outH, ScreenBitsPerPixel(hdcScreen), winErr(err))
	}

	// GDI batches drawing calls, and a DIB section's pixels are only guaranteed
	// to hold the drawing once the batch is flushed. Without this, a frame can
	// be read back half-drawn.
	procGdiFlush.Call()

	swizzleBGRAtoRGBA(c.dib)
	return c.img, nil
}

// swizzleBGRAtoRGBA lives in capture.go: both backends need it.
