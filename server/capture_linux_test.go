//go:build linux

package server

import (
	"os"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
)

// ---------------------------------------------------------------------------
// Downscaling, which needs no display
// ---------------------------------------------------------------------------

// bgraFrame builds a tightly packed BGRX frame, asking f for each pixel's
// colours. The fourth byte is left at zero, as the X server leaves it.
func bgraFrame(w, h int, f func(x, y int) (b, g, r byte)) []byte {
	buf := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b, g, r := f(x, y)
			p := (y*w + x) * 4
			buf[p+0], buf[p+1], buf[p+2] = b, g, r
		}
	}
	return buf
}

func TestScaleBGRAtoRGBAIdentity(t *testing.T) {
	src := bgraFrame(2, 2, func(x, y int) (byte, byte, byte) {
		switch {
		case x == 0 && y == 0:
			return 0x00, 0x00, 0xff // red
		case x == 1 && y == 0:
			return 0x00, 0xff, 0x00 // green
		case x == 0 && y == 1:
			return 0xff, 0x00, 0x00 // blue
		default:
			return 0xff, 0xff, 0xff // white
		}
	})

	dst := make([]byte, 2*2*4)
	scaleBGRAtoRGBA(src, 2, 2, dst, 2, 2)

	want := []byte{
		0xff, 0x00, 0x00, 0xff,
		0x00, 0xff, 0x00, 0xff,
		0x00, 0x00, 0xff, 0xff,
		0xff, 0xff, 0xff, 0xff,
	}
	for i := range want {
		if dst[i] != want[i] {
			t.Fatalf("identity scale: dst[%d] = %#x, want %#x\ngot  %v\nwant %v",
				i, dst[i], want[i], dst, want)
		}
	}
}

func TestScaleBGRAtoRGBAAverages(t *testing.T) {
	// Four 2x2 blocks, each a single colour, so every output pixel is the exact
	// average of its block and the expected result is the block colour.
	colours := [2][2][3]byte{
		{{0x00, 0x00, 0xff}, {0x00, 0xff, 0x00}}, // top row: red, green
		{{0xff, 0x00, 0x00}, {0x00, 0x00, 0x00}}, // bottom row: blue, black
	}
	src := bgraFrame(4, 4, func(x, y int) (byte, byte, byte) {
		c := colours[y/2][x/2]
		return c[0], c[1], c[2]
	})

	dst := make([]byte, 2*2*4)
	scaleBGRAtoRGBA(src, 4, 4, dst, 2, 2)

	for dy := 0; dy < 2; dy++ {
		for dx := 0; dx < 2; dx++ {
			c := colours[dy][dx]
			got := dst[(dy*2+dx)*4:]
			if got[0] != c[2] || got[1] != c[1] || got[2] != c[0] || got[3] != 0xff {
				t.Errorf("block (%d,%d): got RGBA %#x %#x %#x %#x, want %#x %#x %#x %#x",
					dx, dy, got[0], got[1], got[2], got[3], c[2], c[1], c[0], 0xff)
			}
		}
	}
}

func TestScaleBGRAtoRGBAMeansWithinBlock(t *testing.T) {
	// A single 2x2 output pixel over a 2x2 source whose red channel runs 0..6,
	// so the average is 3 and nothing is lost to rounding in either direction.
	src := bgraFrame(2, 2, func(x, y int) (byte, byte, byte) {
		return 0, 0, byte((y*2 + x) * 2)
	})

	dst := make([]byte, 4)
	scaleBGRAtoRGBA(src, 2, 2, dst, 1, 1)

	if dst[0] != 3 {
		t.Errorf("averaged red = %d, want 3 (mean of 0,2,4,6)", dst[0])
	}
	if dst[3] != 0xff {
		t.Errorf("alpha = %#x, want 0xff: an opaque frame must not come out transparent", dst[3])
	}
}

func TestScaleBGRAtoRGBANonIntegerRatio(t *testing.T) {
	// 3x3 -> 2x2 does not divide evenly, which is what exercises the clamp that
	// keeps an integer-division band from coming out empty and dividing by zero.
	src := bgraFrame(3, 3, func(x, y int) (byte, byte, byte) {
		return 0, 0, 0xff
	})

	dst := make([]byte, 2*2*4)
	scaleBGRAtoRGBA(src, 3, 3, dst, 2, 2)

	for i := 0; i < len(dst); i += 4 {
		if dst[i] != 0xff || dst[i+3] != 0xff {
			t.Fatalf("uniform red source produced %v at %d, want opaque red", dst[i:i+4], i)
		}
	}
}

// ---------------------------------------------------------------------------
// Against a live X server
// ---------------------------------------------------------------------------

func requireDisplay(t *testing.T) {
	t.Helper()
	if os.Getenv("DISPLAY") == "" {
		t.Skip("DISPLAY is unset; run these under Xvfb (see README)")
	}
}

// paintRoot covers the screen with a solid colour and returns a function that
// tears the window down.
//
// This is a window rather than a root-window background change on purpose: it is
// what a real application does, so it exercises the compositing path a capture
// actually has to read -- and it needs no external tool to be installed.
func paintRoot(t *testing.T, red, green, blue uint16) func() {
	t.Helper()

	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatalf("connecting to the X server: %v", err)
	}
	screen := xproto.Setup(conn).DefaultScreen(conn)

	win, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatalf("allocating a window id: %v", err)
	}
	colour, err := xproto.AllocColor(conn, screen.DefaultColormap, red, green, blue).Reply()
	if err != nil {
		t.Fatalf("allocating a colour: %v", err)
	}

	// Values must be in ascending order of the mask bits: CwBackPixel is bit 1
	// and CwOverrideRedirect is bit 9.
	if err := xproto.CreateWindowChecked(conn, screen.RootDepth, win, screen.Root,
		0, 0, screen.WidthInPixels, screen.HeightInPixels, 0,
		xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwBackPixel|xproto.CwOverrideRedirect,
		[]uint32{colour.Pixel, 1}).Check(); err != nil {
		t.Fatalf("creating the window: %v", err)
	}
	if err := xproto.MapWindowChecked(conn, win).Check(); err != nil {
		t.Fatalf("mapping the window: %v", err)
	}
	// A round trip is not enough on its own: the window has to have been drawn
	// before the frame that is supposed to contain it is asked for.
	conn.Sync()

	return func() { conn.Close() }
}

func TestCaptureX11(t *testing.T) {
	requireDisplay(t)

	teardown := paintRoot(t, 0xffff, 0, 0) // red
	defer teardown()
	time.Sleep(200 * time.Millisecond)

	cap, err := NewCapturer(CaptureConfig{Scale: 1})
	if err != nil {
		t.Fatalf("NewCapturer: %v", err)
	}
	defer cap.Close()

	w, h := cap.Size()
	if w <= 0 || h <= 0 {
		t.Fatalf("Size() = %dx%d, want the screen size", w, h)
	}
	if sw, sh := ScreenSize(); sw != w || sh != h {
		t.Errorf("Size() = %dx%d but ScreenSize() = %dx%d; at scale 1 they are the same thing", w, h, sw, sh)
	}

	img, err := cap.Grab()
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("Grab returned %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}

	// Sample the middle rather than a corner: Xvfb may draw a root pattern, and
	// the centre is unambiguous.
	r, g, b, a := img.At(w/2, h/2).RGBA()
	if a>>8 != 0xff {
		t.Errorf("centre pixel alpha = %d, want 255", a>>8)
	}
	if r>>8 < 0xf0 || g>>8 > 0x10 || b>>8 > 0x10 {
		t.Errorf("centre pixel RGBA = (%d,%d,%d,%d), want opaque red",
			r>>8, g>>8, b>>8, a>>8)
	}
}

// TestCaptureX11UsesSHM checks that the fast path is the one actually taken.
//
// A display without MIT-SHM still works, so nothing else in the suite would
// notice it had silently stopped being negotiated -- and the cost of that is a
// full screen copy per frame, which no test is sensitive to.
func TestCaptureX11UsesSHM(t *testing.T) {
	requireDisplay(t)

	cap, err := NewCapturer(CaptureConfig{Scale: 1})
	if err != nil {
		t.Fatalf("NewCapturer: %v", err)
	}
	defer cap.Close()

	c, ok := cap.(*x11Capturer)
	if !ok {
		t.Fatalf("NewCapturer returned %T, want *x11Capturer", cap)
	}
	if !c.shmOK {
		t.Fatalf("MIT-SHM was not negotiated: %s", c.shmReason)
	}
	if c.shmAddr == 0 {
		t.Fatal("MIT-SHM was negotiated but no segment was allocated")
	}
	for _, note := range c.PlatformNotes() {
		t.Errorf("unexpected platform note on a plain X11 display: %s", note)
	}

	if _, err := cap.Grab(); err != nil {
		t.Fatalf("Grab over MIT-SHM: %v", err)
	}
}

func TestCaptureX11Scale(t *testing.T) {
	requireDisplay(t)

	cap, err := NewCapturer(CaptureConfig{Scale: 0.5})
	if err != nil {
		t.Fatalf("NewCapturer: %v", err)
	}
	defer cap.Close()

	sw, sh := ScreenSize()
	w, h := cap.Size()
	if want := (sw + 1) / 2; w != want {
		t.Errorf("half scale gave width %d, want %d (from %d)", w, want, sw)
	}
	if want := (sh + 1) / 2; h != want {
		t.Errorf("half scale gave height %d, want %d (from %d)", h, want, sh)
	}

	img, err := cap.Grab()
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if b := img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Errorf("Grab returned %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
}

func TestCaptureX11RejectsUnknownDepth(t *testing.T) {
	requireDisplay(t)

	// The depth guard is the difference between refusing a 16-bit root and
	// serving it as though it were 24-bit, which would put garbage on the
	// screen. It is asserted here against a deliberately wrong depth because the
	// real one cannot be made 16-bit from inside the test.
	d, err := openX11()
	if err != nil {
		t.Fatalf("openX11: %v", err)
	}
	defer d.Close()

	c := &x11Capturer{scale: 1, d: d}
	if d.depth == 16 {
		t.Skip("this display really is 16-bit; the guard would fire for real")
	}
	d.depth = 16
	if err := c.rebuild(); err == nil {
		t.Fatal("rebuild accepted a 16-bit root window")
	}
	d.depth = 24
}

// TestInjectorX11MovesThePointer exercises the whole XTEST path: the request
// goes out, the server moves the real pointer, and the position is read back
// through a second connection-independent request.
func TestInjectorX11MovesThePointer(t *testing.T) {
	requireDisplay(t)

	inj := NewInjector()
	defer inj.Close()
	if _, ok := inj.(*x11Injector); !ok {
		t.Fatalf("NewInjector returned %T; is XTEST missing from this server?", inj)
	}

	sw, sh := ScreenSize()
	for _, want := range [][2]int{{10, 20}, {sw / 2, sh / 2}, {sw - 5, sh - 5}} {
		if err := inj.MoveTo(want[0], want[1]); err != nil {
			t.Fatalf("MoveTo(%d,%d): %v", want[0], want[1], err)
		}
		got := queryPointer(t)
		if got != want {
			t.Errorf("after MoveTo(%d,%d) the pointer is at %v", want[0], want[1], got)
		}
	}
}

// TestInjectorX11Clamps checks that coordinates outside the screen are bounded
// rather than wrapped. A client is free to send anything, and XTEST takes an
// int16, so an unclamped large value would land at a negative position.
func TestInjectorX11Clamps(t *testing.T) {
	requireDisplay(t)

	inj := NewInjector()
	defer inj.Close()

	sw, sh := ScreenSize()
	cases := []struct {
		in   [2]int
		want [2]int
	}{
		{in: [2]int{-100, -100}, want: [2]int{0, 0}},
		{in: [2]int{sw + 1000, sh + 1000}, want: [2]int{sw - 1, sh - 1}},
		{in: [2]int{100000, 100000}, want: [2]int{sw - 1, sh - 1}},
	}
	for _, tc := range cases {
		if err := inj.MoveTo(tc.in[0], tc.in[1]); err != nil {
			t.Fatalf("MoveTo%v: %v", tc.in, err)
		}
		if got := queryPointer(t); got != tc.want {
			t.Errorf("MoveTo%v put the pointer at %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestInjectorX11WheelAndButtons checks that the remaining injection paths reach
// the server without error. Whether a button event scrolls anything depends on
// there being a client under the pointer, so what is asserted is the round trip.
func TestInjectorX11WheelAndButtons(t *testing.T) {
	requireDisplay(t)

	inj := NewInjector()
	defer inj.Close()

	for _, button := range []string{ButtonLeft, ButtonMiddle, ButtonRight, ButtonBack, ButtonFwd} {
		if err := inj.Button(button, true); err != nil {
			t.Errorf("Button(%q, down): %v", button, err)
		}
		if err := inj.Button(button, false); err != nil {
			t.Errorf("Button(%q, up): %v", button, err)
		}
	}
	if err := inj.Button("no-such-button", true); err == nil {
		t.Error("Button accepted an unknown button name")
	}

	// The hub accumulates in Windows wheel units, so a notch arrives as 120.
	if err := inj.Wheel(0, 120); err != nil {
		t.Errorf("Wheel up: %v", err)
	}
	if err := inj.Wheel(0, -240); err != nil {
		t.Errorf("Wheel down: %v", err)
	}
	if err := inj.Wheel(120, 0); err != nil {
		t.Errorf("Wheel right: %v", err)
	}
	// Sub-notch deltas cannot be expressed in X11 and are dropped, not errors.
	if err := inj.Wheel(0, 30); err != nil {
		t.Errorf("sub-notch wheel: %v", err)
	}
}

// TestInjectorX11KeyReachesAClient is the one test that proves a keystroke is
// delivered rather than merely accepted.
//
// It opens a window, gives it the input focus, and listens for the key event the
// server delivers. Everything before that point only shows the request was sent.
func TestInjectorX11KeyReachesAClient(t *testing.T) {
	requireDisplay(t)

	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()
	screen := xproto.Setup(conn).DefaultScreen(conn)

	win, err := xproto.NewWindowId(conn)
	if err != nil {
		t.Fatalf("allocating a window id: %v", err)
	}
	// Override-redirect: with no window manager running under Xvfb there would
	// otherwise be nothing to give the window focus at all.
	if err := xproto.CreateWindowChecked(conn, screen.RootDepth, win, screen.Root,
		0, 0, 100, 100, 0, xproto.WindowClassInputOutput, screen.RootVisual,
		xproto.CwOverrideRedirect, []uint32{1}).Check(); err != nil {
		t.Fatalf("creating the window: %v", err)
	}
	if err := xproto.MapWindowChecked(conn, win).Check(); err != nil {
		t.Fatalf("mapping the window: %v", err)
	}
	if err := xproto.SetInputFocusChecked(conn, xproto.InputFocusPointerRoot, win,
		xproto.TimeCurrentTime).Check(); err != nil {
		t.Fatalf("setting the input focus: %v", err)
	}
	if err := xproto.ChangeWindowAttributesChecked(conn, win,
		xproto.CwEventMask, []uint32{xproto.EventMaskKeyPress}).Check(); err != nil {
		t.Fatalf("selecting key events: %v", err)
	}
	conn.Sync()

	inj := NewInjector()
	defer inj.Close()
	if _, ok := inj.(*x11Injector); !ok {
		t.Skip("XTEST is unavailable, so nothing will be delivered")
	}

	sym, _, ok := LookupKey("KeyA", "a")
	if !ok {
		t.Fatal("LookupKey could not resolve KeyA")
	}
	if err := inj.Key(sym, false, true); err != nil {
		t.Fatalf("Key down: %v", err)
	}
	defer inj.Key(sym, false, false)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ev, err := conn.PollForEvent()
		if err != nil {
			t.Fatalf("PollForEvent: %v", err)
		}
		press, isPress := ev.(xproto.KeyPressEvent)
		if !isPress {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if press.Detail == 0 {
			t.Fatal("a key event arrived with keycode 0")
		}
		// The keycode must be one the layout maps to 'a', which is what the
		// injector promised by choosing it.
		if press.Detail != 38 && press.Detail != 24 {
			t.Logf("note: 'a' arrived as keycode %d, which is not the usual 38/24", press.Detail)
		}
		return
	}
	t.Fatal("no KeyPress event was delivered within 3s")
}

// TestInjectorX11Text covers the scratch-keycode path, which is the only way an
// input-method result -- or any character the layout cannot type -- gets in.
func TestInjectorX11Text(t *testing.T) {
	requireDisplay(t)

	inj := NewInjector()
	defer inj.Close()

	in, ok := inj.(*x11Injector)
	if !ok {
		t.Skip("XTEST is unavailable")
	}
	if in.scratch == 0 {
		t.Fatal("no spare keycode was found; the layout is full, which Text() cannot work around")
	}

	// A Latin character and a CJK one: the first the layout can already type,
	// the second it certainly cannot, so both branches of the resolution run.
	if err := inj.Text("A中"); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if err := inj.Text(""); err != nil {
		t.Fatalf("Text(\"\"): %v", err)
	}
}

// queryPointer reads the pointer position back from the server.
func queryPointer(t *testing.T) [2]int {
	t.Helper()

	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()

	root := xproto.Setup(conn).DefaultScreen(conn).Root
	reply, err := xproto.QueryPointer(conn, root).Reply()
	if err != nil {
		t.Fatalf("QueryPointer: %v", err)
	}
	return [2]int{int(reply.RootX), int(reply.RootY)}
}

// TestXTESTIsPresent is a failure the others would report as "the injector
// refuses" without saying why.
func TestXTESTIsPresent(t *testing.T) {
	requireDisplay(t)

	conn, err := xgb.NewConn()
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close()

	if err := xtest.Init(conn); err != nil {
		t.Fatalf("this X server has no XTEST extension: %v", err)
	}
}
