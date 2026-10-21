package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recordingInjector stands in for the real input backend and remembers what it
// was asked to do.
//
// A counter would be enough to assert "nothing happened", but the calls are kept
// as strings so a failure says *what* leaked through rather than only that the
// total was not zero -- which is the difference between a test that reports a
// bug and one that reports where to look for it.
type recordingInjector struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingInjector) record(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func (r *recordingInjector) MoveTo(x, y int) error {
	r.record(fmt.Sprintf("move(%d,%d)", x, y))
	return nil
}

func (r *recordingInjector) Button(button string, down bool) error {
	r.record(fmt.Sprintf("button(%s,%v)", button, down))
	return nil
}

func (r *recordingInjector) Wheel(deltaX, deltaY int) error {
	r.record(fmt.Sprintf("wheel(%d,%d)", deltaX, deltaY))
	return nil
}

func (r *recordingInjector) Key(symbol uint16, extended, down bool) error {
	r.record(fmt.Sprintf("key(%d,%v,%v)", symbol, extended, down))
	return nil
}

func (r *recordingInjector) Text(s string) error {
	r.record("text(" + s + ")")
	return nil
}

// ScreenSize answers with a plausible desktop so the capture loop has geometry
// to work from; the input tests below do not depend on the exact numbers.
func (r *recordingInjector) ScreenSize() (int, int) { return 1280, 800 }

func (r *recordingInjector) Close() error { return nil }

func (r *recordingInjector) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// countByPrefix tallies the recorded calls that start with prefix, which is how
// the control test below states its expectation without pinning down the exact
// coordinates and key symbols each platform's mapping produces.
func (r *recordingInjector) countByPrefix(prefix string) int {
	n := 0
	for _, call := range r.got() {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// newViewOnlyHarness starts a hub whose injector is the recorder.
//
// It does not use newStreamTestHub because that helper starts the hub's Run
// loop before returning, and the injector has to be swapped while nothing is
// reading it: the capture loop calls h.inj, so assigning after Run began would
// be a data race rather than merely untidy.
func newViewOnlyHarness(t *testing.T, viewOnly bool) (*httptest.Server, *recordingInjector) {
	t.Helper()

	inj := &recordingInjector{}
	hub := newHub(
		normalizeHubConfig(HubConfig{FPS: 5, Quality: 60, MaxClients: 4, ViewOnly: viewOnly}),
		newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler),
	)
	hub.inj = inj
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, inj
}

// controlReader collects the text messages a client receives, so a test can ask
// "did a pong arrive" and "did anything else" without racing the socket.
//
// Unlike the frame-reading helpers elsewhere this keeps text rather than
// binaries: everything asserted here -- the screen announce, the pong, a
// clipboard reply -- is a control message, and the JPEG frames are noise.
func controlReader(conn *websocket.Conn) <-chan map[string]any {
	out := make(chan map[string]any, 64)
	go func() {
		defer close(out)
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				continue
			}
			var msg map[string]any
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			out <- msg
		}
	}()
	return out
}

// awaitType reads until a message of the given type shows up, or the deadline
// passes. Returns nil on timeout rather than failing, so the caller can say
// what it was waiting for.
func awaitType(in <-chan map[string]any, want string, d time.Duration) map[string]any {
	deadline := time.After(d)
	for {
		select {
		case msg, ok := <-in:
			if !ok {
				return nil
			}
			if msg["type"] == want {
				return msg
			}
		case <-deadline:
			return nil
		}
	}
}

// drain reports everything received in the next d, after which the channel must
// be quiet -- the point being to catch a reply the code under test should not
// have sent, which no amount of waiting for a specific type can detect.
func drain(in <-chan map[string]any, d time.Duration) []map[string]any {
	var got []map[string]any
	deadline := time.After(d)
	for {
		select {
		case msg, ok := <-in:
			if !ok {
				return got
			}
			got = append(got, msg)
		case <-deadline:
			return got
		}
	}
}

// inputBurst is one of every client-driven message the protocol has, which is
// what makes the assertions below cover the whole surface rather than the
// handful of types someone remembered to list.
func inputBurst() []map[string]any {
	return []map[string]any{
		{"type": "mousemove", "x": 100, "y": 120},
		{"type": "mousedown", "button": "left"},
		{"type": "mouseup", "button": "left"},
		{"type": "wheel", "deltaY": 120},
		{"type": "keydown", "key": "a", "code": "KeyA"},
		{"type": "keyup", "key": "a", "code": "KeyA"},
		{"type": "text_input", "text": "你好"},
		{"type": "clipboard_push", "text": "hello"},
		{"type": "clipboard_pull"},
		{"type": "ping"},
	}
}

func sendBurst(t *testing.T, conn *websocket.Conn, msgs []map[string]any) {
	t.Helper()
	for _, m := range msgs {
		if err := conn.WriteJSON(m); err != nil {
			t.Fatalf("write %v: %v", m["type"], err)
		}
	}
}

// TestViewOnlyDropsAllInput is the security claim of the whole feature: with
// ViewOnly set, no message a client can send reaches this machine.
func TestViewOnlyDropsAllInput(t *testing.T) {
	srv, inj := newViewOnlyHarness(t, true)

	conn, _, err := websocket.DefaultDialer.Dial(streamURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	in := controlReader(conn)

	screen := awaitType(in, "screen", 2*time.Second)
	if screen == nil {
		t.Fatal("no screen message on connect")
	}
	if screen["viewOnly"] != true {
		t.Errorf("screen.viewOnly = %v, want true -- the client is not told it is read-only",
			screen["viewOnly"])
	}

	sendBurst(t, conn, inputBurst())

	if pong := awaitType(in, "pong", 2*time.Second); pong == nil {
		t.Fatal("ping was swallowed: the liveness probe must stay on the allowlist")
	}

	// Anything arriving after the pong is something the server volunteered. The
	// clipboard replies are the ones that matter: they are not Injector calls,
	// so a guard wrapped around the injector alone would let them through, and
	// a clipboard_pull would hand the viewer this machine's clipboard.
	for _, msg := range drain(in, 300*time.Millisecond) {
		t.Errorf("server sent %v after an input burst in live mode; only pong is expected", msg["type"])
	}

	if calls := inj.got(); len(calls) != 0 {
		t.Errorf("live mode reached the input layer: %v", calls)
	}
}

// TestInputReachesInjectorWithoutViewOnly is the control.
//
// Without it, a bug that made `handle` return early unconditionally -- or a
// mistyped guard that dropped everything -- would leave the test above passing
// in full, and the feature would look verified while the server had silently
// stopped accepting input for everyone.
func TestInputReachesInjectorWithoutViewOnly(t *testing.T) {
	srv, inj := newViewOnlyHarness(t, false)

	conn, _, err := websocket.DefaultDialer.Dial(streamURL(srv), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	in := controlReader(conn)

	screen := awaitType(in, "screen", 2*time.Second)
	if screen == nil {
		t.Fatal("no screen message on connect")
	}
	if screen["viewOnly"] != false {
		t.Errorf("screen.viewOnly = %v, want false", screen["viewOnly"])
	}

	// clipboard_push is deliberately left out here, unlike in the live-mode
	// test: it writes to the real clipboard of whatever machine runs the tests,
	// and proving the write path is not covered is not worth clobbering it.
	burst := inputBurst()[:7] // through text_input
	burst = append(burst, map[string]any{"type": "clipboard_pull"}, map[string]any{"type": "ping"})
	sendBurst(t, conn, burst)

	if pong := awaitType(in, "pong", 2*time.Second); pong == nil {
		t.Fatal("no pong; the server is not processing messages at all")
	}

	// A clipboard_pull reads this machine's clipboard, so a reply of some kind
	// always follows -- `ok` false where there is no clipboard to read, which
	// is the normal case in a headless test run.
	if clip := awaitType(in, "clipboard", 2*time.Second); clip == nil {
		t.Error("clipboard_pull produced no reply; the guard is dropping more than it should")
	}

	// Polled rather than read once: the messages are handled in arrival order,
	// but the assertions are about the whole burst having been applied.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if inj.countByPrefix("key(") == 2 && inj.countByPrefix("text(") == 1 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, want := range []struct {
		prefix string
		n      int
	}{
		{"move(", 1},
		{"button(", 2},
		{"wheel(", 1},
		{"key(", 2},
		{"text(", 1},
	} {
		if got := inj.countByPrefix(want.prefix); got != want.n {
			t.Errorf("%s reached the injector %d times, want %d (all calls: %v)",
				want.prefix, got, want.n, inj.got())
		}
	}
}
