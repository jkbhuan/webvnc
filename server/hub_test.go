//go:build windows

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

var procGetCursorPos = user32.NewProc("GetCursorPos")

type point struct{ X, Y int32 }

func cursorPos(t *testing.T) point {
	t.Helper()
	var p point
	r, _, err := procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	if r == 0 {
		t.Fatalf("GetCursorPos failed: %v", err)
	}
	return p
}

func newTestHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	EnableDPIAwareness()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	hub, err := NewHub(HubConfig{FPS: 10, Quality: 60, Scale: 1, StatsPeriod: time.Second, VideoMode: ModeJPEG},
		NewAuthenticator(AuthConfig{Password: "test-secret"}), logger)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return hub, srv
}

// TestScreenCapture checks that the GDI path produces a decodable JPEG whose
// dimensions match the desktop.
//
// The colour channel order is asserted indirectly: a BGRA/RGBA mix-up leaves the
// alpha byte zero, and decoding an image with a fully transparent alpha channel
// yields a uniformly black frame.
func TestScreenCapture(t *testing.T) {
	hub, srv := newTestHub(t)

	frame := dialAndReadFrame(t, srv, "test-secret")
	if len(frame) < 4 || frame[0] != 0xFF || frame[1] != 0xD8 {
		t.Fatalf("payload is not a JPEG (first bytes: %x)", frame[:min(8, len(frame))])
	}

	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("jpeg.Decode: %v", err)
	}

	wantW, wantH := hub.ScreenSize()
	if b := img.Bounds(); b.Dx() != wantW || b.Dy() != wantH {
		t.Errorf("frame is %dx%d, desktop is %dx%d", b.Dx(), b.Dy(), wantW, wantH)
	}

	if !hasVariation(img) {
		t.Error("frame is a single flat colour; the capture is probably blank")
	}

	// Leave the frame behind for manual inspection when run with -v.
	path := filepath.Join(t.TempDir(), "frame.jpg")
	if err := os.WriteFile(path, frame, 0o600); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	t.Logf("captured %dx%d, %d bytes -> %s", img.Bounds().Dx(), img.Bounds().Dy(), len(frame), path)
}

// hasVariation reports whether the image contains more than one distinct colour
// in a coarse sample.
func hasVariation(img image.Image) bool {
	b := img.Bounds()
	step := max(1, b.Dx()/64)
	first := img.At(b.Min.X, b.Min.Y)
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			if img.At(x, y) != first {
				return true
			}
		}
	}
	return false
}

// TestRejectsBadToken checks that the password gate runs before the upgrade.
func TestRejectsBadToken(t *testing.T) {
	_, srv := newTestHub(t)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=wrong"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		conn.Close()
		t.Fatal("expected the handshake to be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp)
	}
}

// TestKeyboardAndTextInput drives the real injection path: it types into the
// focused window and checks the remote cursor moves for a mouse event.
func TestKeyboardAndTextInput(t *testing.T) {
	hub, srv := newTestHub(t)
	conn := dialWS(t, srv, "test-secret")
	defer conn.Close()

	// Mouse move: compare the OS cursor position before and after. This is the
	// end-to-end assertion that SendInput reached the real desktop.
	//
	// This runs against a live workstation, so the physical mouse competes with
	// the injected movement. Injecting and reading back-to-back keeps that race
	// window tiny, and a few retries absorb any interference; the assertion is
	// therefore "the cursor ended up near the target", not "exactly on it".
	before := cursorPos(t)
	targetW, targetH := hub.ScreenSize()
	t.Cleanup(func() { _ = hub.inj.MoveTo(int(before.X), int(before.Y)) })

	wantX, wantY := targetW/4, targetH/4
	if wantX == int(before.X) && wantY == int(before.Y) {
		wantX += 40
	}

	// SendInput queues the movement; GetCursorPos only reflects it once the
	// input thread has processed it. Poll for the result, re-injecting on each
	// pass so a nudge from the physical mouse does not end the attempt.
	const tolerance = 24
	var got point
	landed := false
	deadline := time.Now().Add(3 * time.Second)

	for !landed && time.Now().Before(deadline) {
		send(t, conn, map[string]any{"type": "mousemove", "x": wantX, "y": wantY})
		time.Sleep(60 * time.Millisecond)

		got = cursorPos(t)
		landed = abs(int(got.X)-wantX) <= tolerance && abs(int(got.Y)-wantY) <= tolerance
	}
	if !landed {
		t.Errorf("cursor is at %d,%d after repeatedly moving it to %d,%d "+
			"(is the physical mouse being moved?)", got.X, got.Y, wantX, wantY)
	}

	// Keys and text must not error. They are injected into whatever window has
	// focus, so their effect is not asserted here -- only that the pipeline
	// accepts them.
	send(t, conn, map[string]any{"type": "keydown", "key": "Shift", "code": "ShiftLeft"})
	send(t, conn, map[string]any{"type": "keyup", "key": "Shift", "code": "ShiftLeft"})
	send(t, conn, map[string]any{"type": "text_input", "text": "你好 webvnc"})
	send(t, conn, map[string]any{"type": "wheel", "deltaY": -240})
	send(t, conn, map[string]any{"type": "mousedown", "button": "left"})
	send(t, conn, map[string]any{"type": "mouseup", "button": "left"})
	time.Sleep(150 * time.Millisecond)

	// The connection must still be healthy after all of that.
	if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
		t.Fatalf("ping after input: %v", err)
	}
}

// TestKeyMap covers the browser -> virtual-key translation table.
func TestKeyMap(t *testing.T) {
	cases := []struct {
		code, key string
		wantVK    uint16
		wantExt   bool
		wantOK    bool
	}{
		{"KeyA", "a", 0x41, false, true},
		{"Digit1", "1", 0x31, false, true},
		{"Enter", "Enter", vkReturn, false, true},
		{"NumpadEnter", "Enter", vkReturn, true, true},
		{"ArrowUp", "ArrowUp", vkUp, true, true},
		{"ControlLeft", "Control", vkLControl, false, true},
		{"ControlRight", "Control", vkRControl, true, true},
		{"F12", "F12", vkF1 + 11, false, true},
		{"Semicolon", ";", 0xBA, false, true},
		{"", "k", 0x4B, false, true}, // fallback to `key`
		{"Unidentified", "Unidentified", 0, false, false},
	}
	for _, tc := range cases {
		vk, ext, ok := LookupKey(tc.code, tc.key)
		if ok != tc.wantOK || vk != tc.wantVK || ext != tc.wantExt {
			t.Errorf("LookupKey(%q,%q) = (%#x, %v, %v), want (%#x, %v, %v)",
				tc.code, tc.key, vk, ext, ok, tc.wantVK, tc.wantExt, tc.wantOK)
		}
	}
}

// TestWheelAccumulation checks that sub-notch trackpad deltas are carried rather
// than dropped.
func TestWheelAccumulation(t *testing.T) {
	c := &Client{}

	// Three 30px deltas total 90px, which is still short of a whole notch.
	for i := 0; i < 3; i++ {
		if err := c.handleWheel(&clientMsg{DeltaY: 30}); err != nil {
			t.Fatalf("handleWheel: %v", err)
		}
	}
	if c.wheelAccY == 0 {
		t.Error("expected the sub-notch remainder to be retained")
	}
}

// wheelRecorder captures the wheel deltas it is handed.
//
// Embedding the interface supplies the six methods this test does not exercise;
// reaching one of them would panic, which is the right outcome for a test with
// no business calling them.
type wheelRecorder struct {
	Injector
	wheelX, wheelY int
}

func (r *wheelRecorder) Wheel(x, y int) error {
	r.wheelX, r.wheelY = x, y
	return nil
}

// TestWheelDirection pins the sign convention.
//
// Nothing else catches a mistake here: the deltas stay perfectly well-formed,
// they just scroll the wrong way, and only a human at the far end notices.
func TestWheelDirection(t *testing.T) {
	inj := &wheelRecorder{}
	c := &Client{hub: &Hub{inj: inj}}

	// Scrolling down, as a browser reports it. Windows needs the opposite sign
	// to scroll down, so this must come out negative.
	if err := c.handleWheel(&clientMsg{DeltaY: wheelPixelsPerNotch}); err != nil {
		t.Fatalf("handleWheel: %v", err)
	}
	if inj.wheelY != -wheelDelta {
		t.Errorf("a browser scroll down produced %d Windows units, want %d",
			inj.wheelY, -wheelDelta)
	}

	// And back the other way.
	if err := c.handleWheel(&clientMsg{DeltaY: -wheelPixelsPerNotch}); err != nil {
		t.Fatalf("handleWheel: %v", err)
	}
	if inj.wheelY != wheelDelta {
		t.Errorf("a browser scroll up produced %d Windows units, want %d",
			inj.wheelY, wheelDelta)
	}

	// The horizontal conventions already agree, so this one must NOT have been
	// flipped along with the vertical axis.
	if err := c.handleWheel(&clientMsg{DeltaX: wheelPixelsPerNotch}); err != nil {
		t.Fatalf("handleWheel: %v", err)
	}
	if inj.wheelX != wheelDelta {
		t.Errorf("a browser scroll right produced %d Windows units, want %d",
			inj.wheelX, wheelDelta)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func dialWS(t *testing.T, srv *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + token + "&fmt=" + wireFormat
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", url, err, status)
	}
	return conn
}

// dialAndReadFrame connects and returns the first binary frame's payload.
//
// The wire message carries videoHeaderLen bytes of codec-and-flags prefix; the
// callers are asserting on the encoded image itself, so the prefix is stripped
// here rather than in each of them.
func dialAndReadFrame(t *testing.T, srv *httptest.Server, token string) []byte {
	t.Helper()
	conn := dialWS(t, srv, token)
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if kind != websocket.BinaryMessage {
			continue // the "screen" control message arrives first
		}
		if len(data) < videoHeaderLen {
			t.Fatalf("frame is %d bytes, too short to hold a header", len(data))
		}
		return data[videoHeaderLen:]
	}
}

func send(t *testing.T, conn *websocket.Conn, msg map[string]any) {
	t.Helper()
	payload, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write %v: %v", msg["type"], err)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
