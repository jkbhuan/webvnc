package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"webvnc/server"
)

// newTestServer starts the real route wiring against a real capture backend,
// mirroring what run() puts on the wire.
func newTestServer(t *testing.T, password string) (*httptest.Server, *server.Hub) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth := server.NewAuthenticator(server.AuthConfig{Password: password})

	// ModeJPEG rather than the default auto: this is the end-to-end path, and
	// auto would make the bytes on the wire depend on whether the machine
	// running the test happens to have an H.264 encoder. The frame layout is
	// what is under test here; codec selection has its own tests in server/.
	hub, err := server.NewHub(server.HubConfig{
		FPS: 10, Quality: 60, Scale: 1, VideoMode: server.ModeJPEG,
	}, auth, logger)
	if err != nil {
		// No capture backend means no display to capture. On Windows that is a
		// real failure -- a desktop always has one. On Linux it is an ordinary
		// state for a build machine, and every other test in this repo treats
		// it the same way (see requireDisplay in server/capture_linux_test.go):
		// skip with the reason, because a skip is visible where a silent pass
		// would not be, and a hard failure here would only say "no X server"
		// about a suite that is otherwise perfectly able to run.
		if errors.Is(err, server.ErrUnsupportedCapture) {
			t.Skipf("no capture backend: %v (run under Xvfb; see README)", err)
		}
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)

	assetFS, err := frontendFS()
	if err != nil {
		t.Fatalf("frontendFS: %v", err)
	}

	srv := httptest.NewServer(newMux(hub, auth, logger, assetFS))
	t.Cleanup(srv.Close)
	return srv, hub
}

// TestServesEmbeddedFrontend is the check behind acceptance criterion 1: the
// binary must serve the SPA out of its own embedded filesystem, with no dist
// directory next to it on disk.
func TestServesEmbeddedFrontend(t *testing.T) {
	srv, _ := newTestServer(t, "hunter2")

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	html := string(body)

	if !strings.Contains(html, `id="app"`) {
		t.Errorf("index.html does not contain the Vue mount point:\n%s", truncate(html, 400))
	}
	if !strings.Contains(html, "/assets/") {
		t.Errorf("index.html does not reference a built asset bundle:\n%s", truncate(html, 400))
	}

	// The hashed bundle must be reachable and cacheable.
	asset := firstAssetPath(t, html)
	res2, err := http.Get(srv.URL + asset)
	if err != nil {
		t.Fatalf("GET %s: %v", asset, err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", asset, res2.StatusCode)
	}
	if cc := res2.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset Cache-Control = %q, want it to contain \"immutable\"", cc)
	}

	// An unknown path must fall back to the SPA shell, not 404.
	res3, err := http.Get(srv.URL + "/some/client/route")
	if err != nil {
		t.Fatalf("GET /some/client/route: %v", err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != http.StatusOK {
		t.Errorf("SPA fallback = %d, want 200", res3.StatusCode)
	}
	fallback, _ := io.ReadAll(res3.Body)
	if !strings.Contains(string(fallback), `id="app"`) {
		t.Error("SPA fallback did not serve index.html")
	}
}

func TestLoginFlow(t *testing.T) {
	srv, _ := newTestServer(t, "hunter2")

	// Wrong password.
	res := postLogin(t, srv.URL, "wrong")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad password = %d, want 401", res.StatusCode)
	}
	res.Body.Close()

	// Correct password.
	res = postLogin(t, srv.URL, "hunter2")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("good password = %d, want 200", res.StatusCode)
	}
	var body struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	res.Body.Close()

	if len(body.Token) < 32 {
		t.Errorf("token %q looks too short to be a session token", body.Token)
	}
	if body.ExpiresAt <= time.Now().UnixMilli() {
		t.Error("session already expired on issue")
	}

	// The token must satisfy /api/verify and the WebSocket gate.
	verify, err := http.Get(srv.URL + "/api/verify?token=" + body.Token)
	if err != nil {
		t.Fatalf("GET /api/verify: %v", err)
	}
	verify.Body.Close()
	if verify.StatusCode != http.StatusOK {
		t.Errorf("/api/verify with a fresh token = %d, want 200", verify.StatusCode)
	}
}

// TestStreamsFramesOverWebSocket covers the core loop: login, upgrade, and a
// decodable JPEG arriving on the binary channel.
func TestStreamsFramesOverWebSocket(t *testing.T) {
	srv, hub := newTestServer(t, "hunter2")

	token := loginToken(t, srv.URL, "hunter2")

	// fmt=1 is the frame layout this binary speaks, and the server closes the
	// connection without it -- see wireFormat in server/ws.go. The literal is
	// the same one frontend/src/ws.ts builds into its URL, for the same reason:
	// the query parameter is the contract between the two halves, and the test
	// acts as a client, so it states the version it is written against.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=" + token + "&fmt=1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	sawScreenMsg := false
	for frames := 0; frames < 2; {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read (after %d frames): %v", frames, err)
		}

		if kind == websocket.TextMessage {
			var msg struct {
				Type          string `json:"type"`
				Width, Height int
			}
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("control message is not JSON: %v", err)
			}
			if msg.Type == "screen" {
				sawScreenMsg = true
				wantW, wantH := hub.ScreenSize()
				if msg.Width != wantW || msg.Height != wantH {
					t.Errorf("screen message says %dx%d, hub says %dx%d",
						msg.Width, msg.Height, wantW, wantH)
				}
			}
			continue
		}

		// Every binary message is framed, and the frame is not the payload: two
		// bytes of header -- the codec tag and the flags -- come first. Reading
		// the payload from offset zero is precisely the mistake wireFormat
		// exists to catch, so the test reads the header the way the frame
		// layout says to. See videoHeaderLen in server/video.go for the layout.
		const headerLen = 2
		if len(data) <= headerLen {
			t.Fatalf("binary frame is %d bytes, too short to hold the %d-byte header and a payload",
				len(data), headerLen)
		}
		if codec := data[0]; codec != byte(server.CodecJPEG) {
			t.Fatalf("codec byte = %d, want %d (JPEG); the hub was built in jpeg mode",
				codec, byte(server.CodecJPEG))
		}
		if data[1]&1 == 0 {
			t.Error("a JPEG arrived without the keyframe flag; every JPEG is a keyframe")
		}
		if payload := data[headerLen:]; len(payload) < 2 || payload[0] != 0xFF || payload[1] != 0xD8 {
			t.Fatalf("payload is not a JPEG: %x", payload[:min(8, len(payload))])
		}
		frames++
	}

	if !sawScreenMsg {
		t.Error("no screen resolution message was sent on connect")
	}

	// The uplink must accept input without dropping the connection.
	if err := conn.WriteJSON(map[string]any{"type": "keydown", "key": "a", "code": "KeyA"}); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "keyup", "key": "a", "code": "KeyA"}); err != nil {
		t.Fatalf("write input: %v", err)
	}
}

func TestWebSocketRequiresValidToken(t *testing.T) {
	srv, _ := newTestServer(t, "hunter2")
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=nope"

	conn, res, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded with a bad token")
	}
	if res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", res)
	}
}

// ---------------------------------------------------------------------------

func postLogin(t *testing.T, base, password string) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"password": password})
	res, err := http.Post(base+"/api/login", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("POST /api/login: %v", err)
	}
	return res
}

func loginToken(t *testing.T, base, password string) string {
	t.Helper()
	res := postLogin(t, base, password)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", res.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return body.Token
}

// firstAssetPath pulls the first /assets/... reference out of index.html.
func firstAssetPath(t *testing.T, html string) string {
	t.Helper()
	idx := strings.Index(html, "/assets/")
	if idx < 0 {
		t.Fatalf("no /assets/ reference in index.html:\n%s", truncate(html, 400))
	}
	rest := html[idx:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatalf("unterminated asset reference in index.html")
	}
	return rest[:end]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
