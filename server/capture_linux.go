//go:build linux

// Screen capture on X11, through MIT-SHM where the server offers it and a plain
// GetImage where it does not.
//
// The shape of this file follows capture_windows.go closely, because the problem
// is the same one: get a frame into a reusable RGBA image with as few copies as
// the platform allows. What differs is where the copies are. GDI hands back a
// DIB section whose pixels the Go heap can alias, so Windows pays one swizzle.
// X11 has no shared-pixel mode for the drawing client, so the frame always
// arrives as BGRX and always costs a swizzle -- but MIT-SHM removes the socket
// transfer, which at 1920x1080 is the 8 MB that would otherwise cross a unix
// socket on every frame.

package server

import (
	"fmt"
	"image"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
)

// System V IPC constants for the shared segment a MIT-SHM frame lands in. Go's
// syscall package has wrappers for sockets and files but not for these, so they
// are called through raw Syscall the way the Win32 entry points are.
const (
	ipcPrivate = 0
	ipcCreat   = 0o1000 // IPC_CREAT
	ipcRmid    = 0      // IPC_RMID
)

// shmFailureLimit is how many consecutive frames the shared-memory path may fail
// before it is given up on.
//
// One failure is often transient -- a BadMatch when the resolution changed
// between the geometry query and the request -- and the next rebuild clears it.
// A segment that has gone bad for good would otherwise cost a failed round trip
// *and* a full socket copy of the screen on every frame from then on, which is
// worse than either path alone.
const shmFailureLimit = 3

// x11Capturer captures the root window with MIT-SHM, or with GetImage when the
// shared-memory extension is missing or has stopped working.
type x11Capturer struct {
	mu    sync.Mutex
	scale float64

	// d is this capturer's own connection, separate from the one behind
	// ScreenSize and the one behind the injector. Each holds its own reply
	// stream and must not be shared, and a dedicated one keeps a slow frame from
	// queueing in front of a mouse move.
	d *x11Display

	screenW, screenH int
	outW, outH       int

	// shmOK records that the extension was negotiated at startup. It is not a
	// per-frame flag: whether the extension exists cannot change while the
	// connection lives, so it is asked once, in NewCapturer.
	shmOK bool
	// shmReason is why it was not, for the startup log.
	shmReason string

	// shmSeg, shmid and shmAddr describe the shared segment for the current
	// resolution. shmAddr is zero when there is none, which is what fetch tests:
	// the three travel together and are released together.
	shmSeg  shm.Seg
	shmid   uint32
	shmAddr uintptr
	shmSize uint32

	shmFailures int

	// img is the RGBA frame handed to the encoder. It is reused every tick, as
	// the Capturer contract requires.
	img *image.RGBA

	// wayland records that a Wayland compositor is behind the X server, which is
	// what XWayland is. It changes what the capture means and the operator is
	// told once, at startup.
	wayland bool
}

// NewCapturer creates the X11 screen capture backend.
//
// Scale is clamped to (0, 1]; pass 1.0 for native resolution.
func NewCapturer(cfg CaptureConfig) (Capturer, error) {
	scale := cfg.Scale
	if scale <= 0 {
		scale = 1
	}
	if scale > 1 {
		scale = 1
	}

	d, err := openX11()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedCapture, err)
	}

	c := &x11Capturer{
		scale: scale,
		d:     d,
		// XWayland sets both DISPLAY and WAYLAND_DISPLAY; a session that is
		// genuinely X11 sets only the former. So this is exactly the "there is a
		// compositor behind the X server" test, and not a guess about the
		// distribution's defaults.
		wayland: os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland",
	}

	// Negotiating the extension has to happen before any shm request, and only
	// once per connection, so it cannot live in rebuild: rebuild runs again on
	// every resolution change, and a second shm.Init on the same connection is
	// an error. A failure here is not fatal -- it means GetImage, which works.
	if err := c.initSHM(); err != nil {
		c.shmOK = false
		c.shmReason = err.Error()
	} else {
		c.shmOK = true
	}

	if err := c.rebuild(); err != nil {
		d.Close()
		return nil, err
	}
	return c, nil
}

// PlatformNotes reports what the environment is going to do to the picture,
// which is the kind of thing that is invisible until someone complains about it.
func (c *x11Capturer) PlatformNotes() []string {
	var notes []string
	if c.wayland {
		notes = append(notes,
			"检测到 Wayland 会话（XWayland）：画面只包含 XWayland 里的 X11 程序，"+
				"Wayland 原生窗口（GNOME 桌面本身、多数终端与浏览器）不会出现。这是当前实现的已知限制")
	}
	if !c.shmOK {
		notes = append(notes, "MIT-SHM 不可用（"+c.shmReason+"）：抓屏退化为 GetImage，"+
			"每帧都要走一遍 X socket 传整屏像素，高分辨率下帧率和 CPU 占用会明显变差")
	}
	return notes
}

// initSHM negotiates the MIT-SHM extension on this capturer's connection.
func (c *x11Capturer) initSHM() error {
	if err := shm.Init(c.d.conn); err != nil {
		return err
	}
	ver, err := shm.QueryVersion(c.d.conn).Reply()
	if err != nil {
		return err
	}
	if ver.MajorVersion != 1 {
		return fmt.Errorf("MIT-SHM version %d.%d is not the 1.x this backend speaks", ver.MajorVersion, ver.MinorVersion)
	}
	return nil
}

// rebuild (re)allocates the frame resources for the current resolution and
// scale. The caller must not hold c.mu.
func (c *x11Capturer) rebuild() error {
	sw, sh := ScreenSize()
	if sw <= 0 || sh <= 0 {
		// ScreenSize keeps a connection of its own, so its failure says nothing
		// about this one. Ask this capturer's connection before giving up.
		var err error
		sw, sh, err = c.d.geometry()
		if err != nil {
			return fmt.Errorf("cannot determine the root window size: %w", err)
		}
	}
	if sw <= 0 || sh <= 0 {
		return fmt.Errorf("the root window reported an invalid size %dx%d", sw, sh)
	}

	// The root window is almost always 24 bits deep, which X11 transfers as one
	// 32-bit BGRX pixel per position. 16-bit (RGB565) and 8-bit roots survive on
	// old and embedded servers; there is no swizzle for those here, and serving
	// them as though there were would put a corrupted picture in front of the
	// user, so they are refused by name instead.
	if c.d.depth != 24 && c.d.depth != 32 {
		return fmt.Errorf("the X server's root window is %d bits deep; this backend needs 24 or 32", c.d.depth)
	}

	outW := int(float64(sw)*c.scale + 0.5)
	outH := int(float64(sh)*c.scale + 0.5)
	if outW < 1 {
		outW = 1
	}
	if outH < 1 {
		outH = 1
	}

	c.releaseSHM()
	if c.shmOK {
		if err := c.allocSHM(sw, sh); err != nil {
			// The extension is there but the segment is not, which over a
			// forwarded display is the normal outcome: the server's shmget has
			// no meaning on the far side. Fall back for good rather than
			// retrying at every resolution change.
			c.shmOK = false
			c.shmReason = err.Error()
		}
	}

	c.screenW, c.screenH = sw, sh
	c.outW, c.outH = outW, outH
	c.img = image.NewRGBA(image.Rect(0, 0, outW, outH))
	return nil
}

// allocSHM creates the shared segment for one full-resolution frame and attaches
// both this process and the X server to it.
func (c *x11Capturer) allocSHM(w, h int) error {
	size := uint32(w * h * 4)

	shmid, _, errno := syscall.Syscall(syscall.SYS_SHMGET,
		uintptr(ipcPrivate), uintptr(size), uintptr(ipcCreat|0o600))
	if errno != 0 {
		return fmt.Errorf("shmget: %w", errno)
	}

	addr, _, errno := syscall.Syscall(syscall.SYS_SHMAT, shmid, 0, 0)
	if errno != 0 {
		shmctl(shmid, ipcRmid)
		return fmt.Errorf("shmat: %w", errno)
	}

	seg, err := shm.NewSegId(c.d.conn)
	if err != nil {
		syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0)
		shmctl(shmid, ipcRmid)
		return err
	}
	if err := shm.AttachChecked(c.d.conn, seg, uint32(shmid), false).Check(); err != nil {
		syscall.Syscall(syscall.SYS_SHMDT, addr, 0, 0)
		shmctl(shmid, ipcRmid)
		return err
	}

	c.shmSeg, c.shmid, c.shmAddr, c.shmSize = seg, uint32(shmid), addr, size
	c.shmFailures = 0
	return nil
}

// releaseSHM unmaps this process and marks the segment for destruction. The
// caller must not hold c.mu.
//
// The X server is not told to detach: closing the connection or losing the
// segment id does that, and a Detach request to a server that has already
// forgotten the id would only produce an error nobody can act on.
func (c *x11Capturer) releaseSHM() {
	if c.shmAddr != 0 {
		syscall.Syscall(syscall.SYS_SHMDT, c.shmAddr, 0, 0)
	}
	if c.shmid != 0 {
		shmctl(uintptr(c.shmid), ipcRmid)
	}
	c.shmSeg, c.shmid, c.shmAddr, c.shmSize = 0, 0, 0, 0
	c.shmFailures = 0
}

// shmctl is the System V IPC control call, used here only to mark a segment for
// destruction once the last attached process is gone.
func shmctl(shmid uintptr, cmd uintptr) {
	syscall.Syscall(syscall.SYS_SHMCTL, shmid, cmd, 0)
}

func (c *x11Capturer) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outW, c.outH
}

func (c *x11Capturer) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseSHM()
	c.img = nil
	if c.d != nil {
		c.d.Close()
	}
}

// Grab captures one frame into a reused RGBA image.
func (c *x11Capturer) Grab() (*image.RGBA, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The user may have changed resolution or plugged in a monitor since the
	// last frame.
	if sw, sh := ScreenSize(); sw > 0 && sh > 0 && (sw != c.screenW || sh != c.screenH) {
		if err := c.rebuild(); err != nil {
			return nil, err
		}
	}

	bgra, err := c.fetch(c.screenW, c.screenH)
	if err != nil {
		return nil, err
	}

	if c.outW == c.screenW && c.outH == c.screenH {
		// The copy is not redundant with the swizzle: bgra points into the shm
		// segment or a reply buffer that the next frame overwrites, and the
		// encoder is entitled to hold the image it is handed.
		copy(c.img.Pix, bgra)
		swizzleBGRAtoRGBA(c.img.Pix)
		return c.img, nil
	}

	scaleBGRAtoRGBA(bgra, c.screenW, c.screenH, c.img.Pix, c.outW, c.outH)
	return c.img, nil
}

// fetch reads one full-resolution BGRX frame of the root window.
//
// The returned slice is only valid until the next call: with MIT-SHM it is a
// view of the segment the server just wrote into, and without it is the previous
// reply's buffer.
func (c *x11Capturer) fetch(w, h int) ([]byte, error) {
	if c.shmAddr == 0 {
		return c.fetchPlain(w, h)
	}

	frame, err := c.fetchSHM(w, h)
	if err == nil {
		c.shmFailures = 0
		return frame, nil
	}

	c.shmFailures++
	if c.shmFailures >= shmFailureLimit {
		c.releaseSHM()
		c.shmOK = false
		c.shmReason = err.Error()
	}

	plain, ferr := c.fetchPlain(w, h)
	if ferr != nil {
		return nil, fmt.Errorf("MIT-SHM frame failed (%v) and GetImage failed too (%w)", err, ferr)
	}
	return plain, nil
}

// fetchSHM asks the server to render the root window straight into the shared
// segment. Nothing crosses the socket but a reply header.
func (c *x11Capturer) fetchSHM(w, h int) ([]byte, error) {
	reply, err := shm.GetImage(c.d.conn,
		xproto.Drawable(c.d.root), 0, 0, uint16(w), uint16(h),
		^uint32(0), xproto.ImageFormatZPixmap, c.shmSeg, 0).Reply()
	if err != nil {
		return nil, err
	}
	if reply.Size != c.shmSize {
		return nil, fmt.Errorf("MIT-SHM wrote %d bytes for a %dx%d frame, expected %d",
			reply.Size, w, h, c.shmSize)
	}
	return c.shmFrame(), nil
}

// shmFrame exposes the mapped segment as a byte slice.
//
// This is the one place in the Linux port that converts an address to a pointer,
// and it is the reason the address is carried as a uintptr everywhere else. The
// conversion is safe for a reason that does not generalise, so it is spelled out
// here rather than assumed: the mapping is System V shared memory, which the Go
// runtime knows nothing about. It is not on the heap, so it is never moved by a
// collection and never freed by one -- the two things that make uintptr
// round-trips unsound. It lives exactly as long as shmid does, and releaseSHM
// unmaps it before that id is reused.
//
// go vet flags the conversion anyway, because the rule it applies is syntactic:
// a uintptr becoming an unsafe.Pointer is reported wherever it appears, and it
// has no way to know this uintptr came from shmat. The warning is expected here.
func (c *x11Capturer) shmFrame() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(c.shmAddr)), int(c.shmSize))
}

// fetchPlain reads the frame over the socket with the core GetImage request.
//
// It is the fallback for displays where MIT-SHM is unavailable -- a forwarded X
// connection, or a server with the extension compiled out. It costs a full copy
// of the screen through the socket, which is why this backend prefers SHM, not a
// reason to refuse to work.
func (c *x11Capturer) fetchPlain(w, h int) ([]byte, error) {
	reply, err := xproto.GetImage(c.d.conn, xproto.ImageFormatZPixmap,
		xproto.Drawable(c.d.root), 0, 0, uint16(w), uint16(h), ^uint32(0)).Reply()
	if err != nil {
		return nil, fmt.Errorf("GetImage on the root window: %w", err)
	}
	want := w * h * 4
	if len(reply.Data) < want {
		return nil, fmt.Errorf("GetImage returned %d bytes for a %dx%d %d-bit frame, expected %d",
			len(reply.Data), w, h, reply.Depth, want)
	}
	return reply.Data, nil
}

// scaleBGRAtoRGBA converts a packed BGRX frame into a smaller packed RGBA one.
//
// The filter is an area average: each output pixel is the mean of the source
// pixels its footprint covers. That is what GDI's HALFTONE mode approximates on
// the Windows side, and it is the right filter for a desktop -- dropping pixels
// instead aliases exactly the high-contrast edges the picture is mostly made of,
// which turns text and window borders into a shimmer that costs more bandwidth
// than the pixels it saved.
//
// src is tightly packed BGRX at sw x sh and dst tightly packed RGBA at dw x dh,
// with dw <= sw and dh <= sh.
func scaleBGRAtoRGBA(src []byte, sw, sh int, dst []byte, dw, dh int) {
	for dy := 0; dy < dh; dy++ {
		// The half-open band of source rows [y0, y1) this output row covers.
		// The clamp matters when dh does not divide sh: the integer division can
		// otherwise hand back an empty band and divide by a count of zero.
		y0 := dy * sh / dh
		y1 := (dy + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		out := dst[dy*dw*4 : (dy+1)*dw*4]

		for dx := 0; dx < dw; dx++ {
			x0 := dx * sw / dw
			x1 := (dx + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var r, g, b, n uint32
			for y := y0; y < y1; y++ {
				row := src[y*sw*4:]
				for x := x0; x < x1; x++ {
					p := row[x*4:]
					b += uint32(p[0])
					g += uint32(p[1])
					r += uint32(p[2])
					n++
				}
			}

			o := dx * 4
			out[o+0] = byte(r / n)
			out[o+1] = byte(g / n)
			out[o+2] = byte(b / n)
			out[o+3] = 0xff
		}
	}
}
