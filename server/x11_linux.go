//go:build linux

// This file is the X11 plumbing the Linux capture and input backends share: one
// place that knows how to reach the server, what the root window is, and how to
// ask how big the desktop is.
//
// Everything here goes through github.com/jezek/xgb, a pure-Go X11 client. That
// choice keeps the two constraints the project is built around -- CGO_ENABLED=0
// and a single self-contained binary -- because the library is a wire-protocol
// encoder and nothing more: no C, no cgo, no runtime files. It is the same kind
// of dependency as gorilla/websocket on the other side of the program, and it is
// what makes hand-writing the X11 protocol unnecessary rather than merely
// unpleasant.

package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// xgbLogger sends the XGB package's diagnostics through this program's slog
// logger instead of letting them out of its own stderr pipe.
//
// Left alone, XGB prints with its own "XGB: " prefix and short file names, so
// its lines ignore -log-level and -log-format and land ahead of the startup
// banner as though they came from nowhere. Three connections are opened per
// run, so the commonest of them printed three times.
//
// It is forwarded rather than silenced because most of what XGB logs is real
// protocol damage -- "A write error is unrecoverable", a reply whose sequence id
// does not match its cookie -- and swallowing that would be the same silent
// failure this program spends the rest of its code avoiding. Only the missing
// .Xauthority notice is demoted: Xvfb, containers and root sessions routinely
// have no authority file, the connection proceeds without one, and at the
// default level the line is pure noise.
func init() {
	xgb.Logger = log.New(xgbLogWriter{}, "", 0)
}

type xgbLogWriter struct{}

func (xgbLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")

	// The writer is fixed at init, before main reads -log-level, so the choice
	// of level cannot be made here -- but it can be deferred: slog.Default() is
	// resolved on every write, and by the time a connection is opened main has
	// already replaced the default handler.
	level := slog.LevelWarn
	if strings.HasPrefix(msg, "Could not get authority info") ||
		strings.HasPrefix(msg, "Trying connection without authority info") {
		level = slog.LevelDebug
	}

	slog.Log(context.Background(), level, "xgb: "+msg)
	return len(p), nil
}

// ErrNoX11 is returned when the process cannot reach an X server.
//
// It is distinct from ErrUnsupportedCapture so the operator can tell "this build
// has no backend" from "this build has one and there is nothing for it to talk
// to" -- on a headless machine the second is normal and the fix is to run under
// Xvfb or set DISPLAY, not to recompile.
var ErrNoX11 = errors.New("cannot connect to an X server")

// x11Display is one connection to the X server, with the screen facts that never
// change for the life of the connection.
//
// A connection is emphatically not safe for concurrent use. X11 requests and
// replies share one sequence space, so a reply read by one goroutine can be the
// answer to a request another goroutine sent; xgb documents no locking of its
// own around this. Every round trip below therefore holds mu across the whole
// request/reply pair, which is what makes the type safe to hand to the capture
// loop and the input path at the same time.
type x11Display struct {
	conn  *xgb.Conn
	root  xproto.Window
	depth byte

	// width and height are the value from connection setup. They are a starting
	// point only: geometry() re-asks, because the resolution can change under a
	// running server and the hub polls for exactly that.
	width, height int

	mu sync.Mutex
}

// openX11 connects to the display named by $DISPLAY.
//
// A pure-Wayland session has no X server at all, so this is where that shows up:
// xgb fails to resolve the display and the caller gets ErrNoX11 with the
// underlying reason attached.
func openX11() (*x11Display, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoX11, err)
	}

	screen := xproto.Setup(conn).DefaultScreen(conn)
	return &x11Display{
		conn:   conn,
		root:   screen.Root,
		depth:  screen.RootDepth,
		width:  int(screen.WidthInPixels),
		height: int(screen.HeightInPixels),
	}, nil
}

// geometry asks the server how big the root window is now.
//
// The root window spans every monitor, so a multi-head desktop is served as one
// wide canvas. That differs from the Windows backend, which captures the primary
// monitor alone, and it is the more useful of the two defaults here: X11 has no
// notion of a primary monitor without XRandR, and a single canvas is what the
// browser can present without needing to know about the display layout.
func (d *x11Display) geometry() (int, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	reply, err := xproto.GetGeometry(d.conn, xproto.Drawable(d.root)).Reply()
	if err != nil {
		return 0, 0, fmt.Errorf("GetGeometry on the root window: %w", err)
	}
	return int(reply.Width), int(reply.Height), nil
}

func (d *x11Display) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		d.conn.Close()
		d.conn = nil
	}
}

// ---------------------------------------------------------------------------
// The shared connection behind ScreenSize
// ---------------------------------------------------------------------------

// screenSizeRetry is how long ScreenSize waits before trying to reconnect after
// a failure.
//
// The hub asks for the desktop size once per captured frame, so a machine that
// cannot reach an X server would otherwise attempt a fresh TCP connection every
// tick -- thousands of syscalls and, on a forwarded display, a DNS lookup each.
// One attempt per second is enough to notice a server that comes back.
const screenSizeRetry = time.Second

var (
	screenSizeMu       sync.Mutex
	screenSizeConn     *x11Display
	screenSizeFailedAt time.Time
)

// ScreenSize reports the root window's size, or (0, 0) if it cannot be
// determined.
//
// It holds a connection of its own and keeps it open. That is not just an
// optimisation: the capture loop calls this on every frame to notice a
// resolution change, so anything that opens a connection per call would put a
// handshake in the middle of the frame path. It is also deliberately not the
// capturer's connection -- this one is asked from inside the capture loop, and
// sharing would let a busy input path block the frame it is measuring for.
func ScreenSize() (int, int) {
	screenSizeMu.Lock()
	defer screenSizeMu.Unlock()

	if screenSizeConn == nil {
		if !screenSizeFailedAt.IsZero() && time.Since(screenSizeFailedAt) < screenSizeRetry {
			return 0, 0
		}
		d, err := openX11()
		if err != nil {
			screenSizeFailedAt = time.Now()
			return 0, 0
		}
		screenSizeConn = d
		screenSizeFailedAt = time.Time{}
	}

	w, h, err := screenSizeConn.geometry()
	if err != nil {
		// The common cause is a display that went away -- the X server was
		// restarted, or the session ended. Dropping the connection means the
		// next call retries from scratch instead of failing on a dead socket
		// forever.
		screenSizeConn.Close()
		screenSizeConn = nil
		screenSizeFailedAt = time.Now()
		return 0, 0
	}
	return w, h
}
