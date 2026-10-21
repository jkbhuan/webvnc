package server

import (
	"image"
	"log/slog"
	"testing"
	"time"
)

// fakeCapturer hands out a frame the test controls, so the capture loop can be
// exercised end to end without a desktop. The real Capturer interface exists
// precisely so this is possible.
type fakeCapturer struct {
	img *image.RGBA
	err error
}

func (f *fakeCapturer) Grab() (*image.RGBA, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.img, nil
}

func (f *fakeCapturer) Size() (int, int) {
	b := f.img.Bounds()
	return b.Dx(), b.Dy()
}

func (f *fakeCapturer) Close() {}

// newFakeFrame returns a small opaque frame. The alpha byte must be 0xFF: the
// differ compares raw pixels, so an undefined alpha would make a still screen
// look different on every capture.
func newFakeFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 0x20, 0x40, 0x60, 0xFF
	}
	return img
}

// newLoopHub builds just enough Hub for captureAndSend, skipping NewHub so the
// test stays portable to platforms with no capture backend.
func newLoopHub(cap Capturer, staticSkip bool) *Hub {
	return &Hub{
		cfg: HubConfig{
			FPS: 15, Quality: 65, MinQuality: 30, MinFPS: 5, StaticSkip: staticSkip,
		},
		cap:     cap,
		rc:      newRateController(PriorityFPS, 65, 30, 15, 5),
		log:     slog.New(slog.DiscardHandler),
		clients: make(map[*Client]struct{}),
	}
}

// attachClient registers a client whose queue has room, so Broadcast has
// somewhere to deliver and counts the frame as sent.
func attachClient(h *Hub) *Client {
	c := &Client{hub: h, wsConn: &wsConn{send: make(chan outMsg, 4), closed: make(chan struct{})}}
	h.clients[c] = struct{}{}
	return c
}

// The whole point of the feature: a still screen must cost no bandwidth.
func TestCaptureLoopSkipsUnchangedFrames(t *testing.T) {
	fake := &fakeCapturer{img: newFakeFrame()}
	hub := newLoopHub(fake, true)
	attachClient(hub)

	wire, err := hub.captureAndSend()
	if err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if wire <= 0 {
		t.Fatal("first capture sent nothing, want a full frame")
	}
	if hub.Idle() {
		t.Error("Idle() = true after the first frame, want false")
	}

	// Same pixels again: no encode, no broadcast, no bytes.
	wire, err = hub.captureAndSend()
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	if wire != 0 {
		t.Errorf("unchanged frame used %d bytes, want 0", wire)
	}
	if !hub.Idle() {
		t.Error("Idle() = false after an unchanged frame, want true")
	}
	if got := hub.FramesSent(); got != 1 {
		t.Errorf("FramesSent() = %d, want 1 -- an unchanged frame must not be counted", got)
	}

	// One changed pixel must start the stream again.
	fake.img.Pix[3] = 0xFF
	fake.img.Pix[0] = 0x99
	wire, err = hub.captureAndSend()
	if err != nil {
		t.Fatalf("third capture: %v", err)
	}
	if wire <= 0 {
		t.Error("a changed frame sent nothing, want it broadcast")
	}
	if hub.Idle() {
		t.Error("Idle() = true after a changed frame, want false")
	}
}

// With the feature switched off the loop must behave exactly as it did before,
// which is what makes -static-skip=false usable as a debugging baseline.
func TestCaptureLoopWithoutStaticSkipSendsEveryFrame(t *testing.T) {
	fake := &fakeCapturer{img: newFakeFrame()}
	hub := newLoopHub(fake, false)
	attachClient(hub)

	for i := 0; i < 3; i++ {
		wire, err := hub.captureAndSend()
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		if wire <= 0 {
			t.Fatalf("capture %d sent nothing, want every frame sent when static-skip is off", i)
		}
	}
	if got := hub.FramesSent(); got != 3 {
		t.Errorf("FramesSent() = %d, want 3", got)
	}
}

// When the controller moves the setpoint the encoder's output changes even
// though the pixels did not, so the frame has to go out again. Without this a
// still screen would keep showing the old quality while the user reads it.
func TestCaptureLoopResendsAfterTheBaselineIsForgotten(t *testing.T) {
	fake := &fakeCapturer{img: newFakeFrame()}
	hub := newLoopHub(fake, true)
	attachClient(hub)

	if _, err := hub.captureAndSend(); err != nil {
		t.Fatalf("first capture: %v", err)
	}
	if wire, _ := hub.captureAndSend(); wire != 0 {
		t.Fatalf("unchanged frame used %d bytes, want 0", wire)
	}

	hub.diff.Forget() // what Run does when the rate controller adjusts

	wire, err := hub.captureAndSend()
	if err != nil {
		t.Fatalf("capture after Forget: %v", err)
	}
	if wire <= 0 {
		t.Error("identical pixels were not resent after Forget, want a fresh frame")
	}
	if got := hub.FramesSent(); got != 2 {
		t.Errorf("FramesSent() = %d, want 2", got)
	}
}

// The controller's input has to be what reached the sockets. Counting frames
// dropped for a backed-up client would make it throttle to fix congestion the
// transport has already absorbed.
func TestBroadcastCountsOnlyQueuedFrames(t *testing.T) {
	hub := newLoopHub(&fakeCapturer{img: newFakeFrame()}, true)

	// A zero-capacity queue with no reader is always full.
	stuck := &Client{hub: hub, wsConn: &wsConn{send: make(chan outMsg), closed: make(chan struct{})}}
	hub.clients[stuck] = struct{}{}

	if got := hub.Broadcast([]byte("frame"), CodecJPEG, true); got != 0 {
		t.Errorf("Broadcast() = %d with every queue full, want 0", got)
	}
	if got := hub.FramesSent(); got != 0 {
		t.Errorf("FramesSent() = %d, want 0: a frame nobody received is not traffic", got)
	}

	attachClient(hub)
	if got := hub.Broadcast([]byte("frame"), CodecJPEG, true); got != 1 {
		t.Errorf("Broadcast() = %d with one healthy client, want 1", got)
	}
}

// A zero frame rate would panic on the division, so the interval must clamp.
func TestFrameIntervalNeverDividesByZero(t *testing.T) {
	hub := newLoopHub(&fakeCapturer{img: newFakeFrame()}, true)
	hub.rc = newRateController(PriorityFPS, 65, 30, 0, 0)

	if got := hub.frameInterval(); got != time.Second {
		t.Errorf("frameInterval() = %v at zero fps, want %v", got, time.Second)
	}
}
