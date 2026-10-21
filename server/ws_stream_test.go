package server

import (
	"context"
	"image"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// scriptedCapturer is a frame source the test drives. It reproduces the one
// property of the real GDI capturer that the differ depends on: Grab hands back
// the same buffer every time, with new content written into it in place.
type scriptedCapturer struct {
	mu     sync.Mutex
	img    *image.RGBA
	moving bool
	grabs  int
}

// newTexturedFrame builds a frame whose encoded size really responds to JPEG
// quality. A flat colour -- or a tiny frame, where the headers dominate -- would
// compress to the same size at every quality, and the rate controller would have
// no lever to pull. At 320x240 this costs roughly 5.8 Mbps at quality 90 and
// 1.5 Mbps at quality 30, which brackets the target the test below uses.
func newTexturedFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223 // LCG, so runs are reproducible
			i := img.PixOffset(x, y)
			img.Pix[i] = byte(x*3 + y/3 + int(seed>>28))
			img.Pix[i+1] = byte(y*5 + int(seed>>26))
			img.Pix[i+2] = byte((x ^ y) * 2)
			img.Pix[i+3] = 0xFF
		}
	}
	return img
}

func newScriptedCapturer() *scriptedCapturer {
	return &scriptedCapturer{img: newTexturedFrame(320, 240)}
}

func (s *scriptedCapturer) setMoving(moving bool) {
	s.mu.Lock()
	s.moving = moving
	s.mu.Unlock()
}

func (s *scriptedCapturer) Grab() (*image.RGBA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grabs++
	if s.moving {
		// One byte per grab is enough to count as a changed screen, and cycles
		// slowly enough that no two consecutive frames collide.
		s.img.Pix[0] = byte(s.grabs)
		s.img.Pix[1] = byte(s.grabs >> 8)
	}
	return s.img, nil
}

func (s *scriptedCapturer) Size() (int, int) {
	b := s.img.Bounds()
	return b.Dx(), b.Dy()
}

func (s *scriptedCapturer) Close() {}

// tally counts what actually arrives over the wire, read from its own
// goroutine: a read deadline would poison the connection in gorilla/websocket,
// so the connection is never read with a timeout.
type tally struct {
	mu     sync.Mutex
	frames int
	bytes  int
}

func (t *tally) add(n int) {
	t.mu.Lock()
	t.frames++
	t.bytes += n
	t.mu.Unlock()
}

func (t *tally) snapshot() (frames, bytes int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames, t.bytes
}

func newStreamTestHub(t *testing.T, cap Capturer, cfg HubConfig) (*Hub, *httptest.Server) {
	t.Helper()

	hub := newHub(normalizeHubConfig(cfg), cap,
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
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

func dialStream(t *testing.T, srv *httptest.Server) (*websocket.Conn, *tally) {
	t.Helper()

	url := streamURL(srv)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	counts := &tally{}
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				counts.add(len(data))
			}
		}
	}()
	return conn, counts
}

// The headline requirement: a still screen must stop costing bandwidth, and
// must start again the moment the picture moves.
func TestHubStopsSendingWhenTheScreenIsStill(t *testing.T) {
	cap := newScriptedCapturer()
	hub, srv := newStreamTestHub(t, cap, HubConfig{
		FPS: 30, Quality: 60, Scale: 1, StatsPeriod: 100 * time.Millisecond, StaticSkip: true,
	})

	_, counts := dialStream(t, srv)

	// Phase 1: still. The connect itself produces one frame; after that the
	// capture loop must find nothing to do.
	time.Sleep(1500 * time.Millisecond)
	stillFrames, stillBytes := counts.snapshot()
	if stillFrames != 1 {
		t.Errorf("still screen sent %d frames, want exactly 1", stillFrames)
	}
	if !hub.Idle() {
		t.Error("Idle() = false while the screen is still")
	}

	// Phase 2: moving. Frames must flow again, at close to the configured rate.
	cap.setMoving(true)
	time.Sleep(1200 * time.Millisecond)
	movingFrames, movingBytes := counts.snapshot()
	if got := movingFrames - stillFrames; got < 15 {
		t.Errorf("moving screen sent %d frames in 1.2s, want at least 15 (~30fps)", got)
	}
	if hub.Idle() {
		t.Error("Idle() = true while the screen is moving")
	}

	// Phase 3: still again. This is the case that regresses most easily -- the
	// differ must remember the last frame it sent, not the last one it grabbed.
	cap.setMoving(false)
	time.Sleep(1500 * time.Millisecond)
	finalFrames, finalBytes := counts.snapshot()
	if got := finalFrames - movingFrames; got > 1 {
		t.Errorf("screen went still but %d more frames arrived, want at most 1", got)
	}
	if !hub.Idle() {
		t.Error("Idle() = false after the screen went still again")
	}

	t.Logf("still: %d frames / %d bytes; moving: %d frames / %d bytes; still again: %d frames / %d bytes",
		stillFrames, stillBytes,
		movingFrames-stillFrames, movingBytes-stillBytes,
		finalFrames-movingFrames, finalBytes-movingBytes)
}

// With -static-skip=false the stream must behave as it did before the feature
// existed, which is what makes the flag useful as a debugging baseline.
func TestHubWithoutStaticSkipKeepsSendingAStillScreen(t *testing.T) {
	cap := newScriptedCapturer()
	_, srv := newStreamTestHub(t, cap, HubConfig{
		FPS: 30, Quality: 60, Scale: 1, StatsPeriod: 100 * time.Millisecond, StaticSkip: false,
	})

	_, counts := dialStream(t, srv)
	time.Sleep(1200 * time.Millisecond)

	frames, _ := counts.snapshot()
	if frames < 15 {
		t.Errorf("static-skip off: %d frames in 1.2s, want at least 15", frames)
	}
}

// The rate controller must bring a moving screen under the configured cap, and
// must do it by sacrificing whichever knob the operator said to sacrifice first.
func TestHubThrottlesToTheBitrateTarget(t *testing.T) {
	const target = 2_500_000 // 2.5 Mbps: between quality 90 (5.8) and 30 (1.5)

	cap := newScriptedCapturer()
	cap.setMoving(true)
	hub, srv := newStreamTestHub(t, cap, HubConfig{
		FPS: 15, Quality: 90, Scale: 1, StatsPeriod: 200 * time.Millisecond,
		StaticSkip: true, MaxBitrate: target, Priority: PriorityFPS,
		MinQuality: 20, MinFPS: 5,
	})

	_, counts := dialStream(t, srv)

	// Let the controller see several one-second windows. Unthrottled, this
	// stream costs between 5 and 6 Mbps, so there is real work to do.
	time.Sleep(6 * time.Second)

	if !hub.Limited() {
		t.Fatalf("Limited() = false after 6s far over a %d bps target (quality=%d fps=%d, %d bps)",
			target, hub.EffectiveQuality(), hub.EffectiveFPS(), hub.Bitrate())
	}

	// fps-first means quality pays. The frame rate must be untouched.
	if got := hub.EffectiveFPS(); got != 15 {
		t.Errorf("fps = %d, want it held at 15: fps-first should sacrifice quality", got)
	}
	// And it must settle somewhere in the middle. Bottomed out at the floor
	// would mean it never found an equilibrium; untouched would mean it never
	// reacted at all.
	if got := hub.EffectiveQuality(); got <= 20 || got >= 90 {
		t.Errorf("quality = %d, want an interior value between the floor 20 and the maximum 90", got)
	}

	// The measurement that matters: the most recent second of real traffic.
	before, beforeBytes := counts.snapshot()
	time.Sleep(time.Second)
	after, afterBytes := counts.snapshot()

	observed := (afterBytes - beforeBytes) * 8
	if observed > target*3/2 {
		t.Errorf("delivered %d bps against a %d bps target (frames %d), want within 50%% of it",
			observed, target, after-before)
	}
	t.Logf("target %d bps, measured %d bps, quality %d, fps %d, frames %d",
		target, observed, hub.EffectiveQuality(), hub.EffectiveFPS(), after-before)
}

// quality-first is the mirror image: the same budget, spent the other way round.
func TestHubQualityPriorityThrottlesByFrameRate(t *testing.T) {
	const target = 2_500_000

	cap := newScriptedCapturer()
	cap.setMoving(true)
	// Quality starts at 60, not 90: with the image pinned at 90 the only way to
	// reach 2.5 Mbps is the frame-rate floor, and a test that always ends at the
	// floor cannot tell convergence apart from bottling out.
	hub, srv := newStreamTestHub(t, cap, HubConfig{
		FPS: 15, Quality: 60, Scale: 1, StatsPeriod: 200 * time.Millisecond,
		StaticSkip: true, MaxBitrate: target, Priority: PriorityQuality,
		MinQuality: 20, MinFPS: 5,
	})

	_, counts := dialStream(t, srv)
	time.Sleep(6 * time.Second)

	if !hub.Limited() {
		t.Fatalf("Limited() = false after 6s over the target (quality=%d fps=%d)",
			hub.EffectiveQuality(), hub.EffectiveFPS())
	}
	// quality-first must hold the image and drop the frame rate instead.
	if got := hub.EffectiveQuality(); got != 60 {
		t.Errorf("quality = %d, want it held at 60: quality-first should sacrifice frame rate", got)
	}
	if got := hub.EffectiveFPS(); got >= 15 || got <= 5 {
		t.Errorf("fps = %d, want an interior value between the floor 5 and the maximum 15", got)
	}

	_, beforeBytes := counts.snapshot()
	time.Sleep(time.Second)
	_, afterBytes := counts.snapshot()

	if observed := (afterBytes - beforeBytes) * 8; observed > target*3/2 {
		t.Errorf("delivered %d bps against a %d bps target, want within 50%% of it", observed, target)
	}
	t.Logf("quality-first: quality %d, fps %d", hub.EffectiveQuality(), hub.EffectiveFPS())
}
