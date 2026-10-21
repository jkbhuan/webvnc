package server

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseVideoMode(t *testing.T) {
	ok := []struct {
		in   string
		want VideoMode
	}{
		{"auto", ModeAuto},
		{"h264", ModeH264},
		{"jpeg", ModeJPEG},
	}
	for _, tc := range ok {
		got, err := ParseVideoMode(tc.in)
		if err != nil {
			t.Errorf("ParseVideoMode(%q) = error %v, want %v", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVideoMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Anything else has to be an error rather than a silent default: a typo in
	// a command line should not decide which codec the server uses.
	for _, bad := range []string{"", "H264", "h.264", "jpeg ", "av1", "none"} {
		if _, err := ParseVideoMode(bad); err == nil {
			t.Errorf("ParseVideoMode(%q) = nil error, want an error", bad)
		}
	}
}

// The wire header is the one thing both halves of this program have to agree on
// and neither can check against the other at compile time, so it is pinned here
// and mirrored by parseFrame in the frontend.
func TestFrameMessageLayout(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	key := frameMessage(CodecH264, true, payload)
	if len(key) != videoHeaderLen+len(payload) {
		t.Fatalf("keyframe message is %d bytes, want %d", len(key), videoHeaderLen+len(payload))
	}
	if key[0] != byte(CodecH264) {
		t.Errorf("codec byte = %d, want %d", key[0], byte(CodecH264))
	}
	if key[1]&FlagKey == 0 {
		t.Errorf("keyframe flag not set: flags = %#02x", key[1])
	}
	if !bytes.Equal(key[videoHeaderLen:], payload) {
		t.Errorf("payload = % x, want % x", key[videoHeaderLen:], payload)
	}

	delta := frameMessage(CodecJPEG, false, payload)
	if delta[0] != byte(CodecJPEG) {
		t.Errorf("codec byte = %d, want %d", delta[0], byte(CodecJPEG))
	}
	if delta[1]&FlagKey != 0 {
		t.Errorf("keyframe flag set on a delta frame: flags = %#02x", delta[1])
	}

	// The copy is the point of this function -- the payload comes from a buffer
	// the encoder reuses -- so writing to the source afterwards must not change
	// the message.
	msg := frameMessage(CodecH264, true, payload)
	payload[0] = 0x00
	if msg[videoHeaderLen] != 0xDE {
		t.Error("frameMessage aliases its payload instead of copying it")
	}

	if got := frameMessage(CodecJPEG, true, nil); len(got) != videoHeaderLen {
		t.Errorf("empty payload produced %d bytes, want %d", len(got), videoHeaderLen)
	}
}

// The close reason reaches the browser as the WebSocket close frame's reason,
// which RFC 6455 caps at 123 bytes. A longer one is malformed rather than
// truncated, so the browser discards it and the user is shown nothing at all --
// the exact opposite of what this string is for. It is three bytes per Chinese
// character, which is what makes this easy to overshoot.
//
// Both refusal messages are checked, because both are written by hand and either
// one being long enough to vanish turns a diagnosable failure into silence.
func TestCloseReasonsFit(t *testing.T) {
	for name, msg := range map[string]string{
		"noH264Message":      noH264Message,
		"staleClientMessage": staleClientMessage,
	} {
		if got := len(msg); got > 123 {
			t.Errorf("%s is %d bytes, want at most 123", name, got)
		}
		if strings.TrimSpace(msg) == "" {
			t.Errorf("%s is empty", name)
		}
	}
}

// A page that was not rebuilt alongside the binary is refused rather than
// streamed to. The distinction matters: serving it produces a black screen with
// an empty console in both places, so the tests that pin the refusal are the
// only thing standing between that and someone debugging the codec.
func TestStaleFrontendIsRefused(t *testing.T) {
	hub := newHub(normalizeHubConfig(HubConfig{VideoMode: ModeJPEG}), newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
	t.Cleanup(hub.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=test-secret"

	// No fmt at all is the old page, and a different fmt is a page from some
	// other protocol. Neither is a client this server can talk to.
	for _, query := range []string{"", "&fmt=0", "&fmt=", "&fmt=jpeg"} {
		conn := dial(t, base+query)
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, _, err := conn.ReadMessage()
		closeErr, ok := err.(*websocket.CloseError)
		if !ok {
			t.Errorf("dialling %q gave %v, want a close frame", query, err)
			continue
		}
		// The text, not just the code: a bare close is the failure this whole
		// check exists to replace.
		if closeErr.Code != websocket.CloseNormalClosure || closeErr.Text != staleClientMessage {
			t.Errorf("dialling %q closed with %d %q, want %d %q",
				query, closeErr.Code, closeErr.Text,
				websocket.CloseNormalClosure, staleClientMessage)
		}
	}

	// The control: the current layout still gets served on the same server, so
	// the check above is about the version and not about the connection.
	conn := dial(t, streamURL(srv))
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Errorf("a current client was not served: %v", err)
	}
}

func TestVideoCodecString(t *testing.T) {
	cases := []struct {
		codec VideoCodec
		want  string
	}{
		{CodecJPEG, "jpeg"},
		{CodecH264, "h264"},
		{VideoCodec(7), "codec-7"},
	}
	for _, tc := range cases {
		if got := tc.codec.String(); got != tc.want {
			t.Errorf("VideoCodec(%d).String() = %q, want %q", uint8(tc.codec), got, tc.want)
		}
	}
}

// The intra-frame encoder is the fallback everything else is allowed to assume,
// so its two defining properties are worth stating: it needs no hardware, and
// every frame it produces stands alone.
func TestJPEGEncoderStandsAlone(t *testing.T) {
	var enc jpegEncoder
	if enc.Codec() != CodecJPEG {
		t.Errorf("Codec() = %v, want %v", enc.Codec(), CodecJPEG)
	}

	img := newTexturedFrame(64, 48)
	for _, quality := range []int{10, 90} {
		payload, key, err := enc.Encode(img, quality)
		if err != nil {
			t.Fatalf("Encode(quality %d): %v", quality, err)
		}
		if !key {
			t.Errorf("Encode(quality %d) reported a non-keyframe; every JPEG is one", quality)
		}
		if _, err := jpeg.Decode(bytes.NewReader(payload)); err != nil {
			t.Errorf("Encode(quality %d) produced bytes that are not a JPEG: %v", quality, err)
		}
	}
	// Close is part of the interface and must be safe on an encoder that holds
	// nothing, since the capture loop calls it unconditionally.
	enc.Close()
}

// ModeH264 is allowed to fail and the other two are not. Which of them succeeds
// depends on the machine -- there is no H.264 encoder on a build host -- so the
// assertions are about the shape of the answer rather than the answer.
func TestNewVideoEncoderFallback(t *testing.T) {
	cfg := normalizeHubConfig(HubConfig{})

	enc, err := newVideoEncoder(ModeJPEG, cfg, 320, 240)
	if err != nil {
		t.Fatalf("ModeJPEG: %v", err)
	}
	if enc.Codec() != CodecJPEG {
		t.Errorf("ModeJPEG produced %v, want %v", enc.Codec(), CodecJPEG)
	}
	enc.Close()

	// Auto is the default, so it must never be the reason startup fails.
	enc, err = newVideoEncoder(ModeAuto, cfg, 320, 240)
	if err != nil {
		t.Fatalf("ModeAuto: %v", err)
	}
	switch enc.Codec() {
	case CodecJPEG, CodecH264:
	default:
		t.Errorf("ModeAuto produced %v, want jpeg or h264", enc.Codec())
	}
	enc.Close()

	enc, err = newVideoEncoder(ModeH264, cfg, 320, 240)
	if err != nil {
		if !errors.Is(err, ErrNoHardwareEncoder) {
			t.Errorf("ModeH264 error = %v, want %v", err, ErrNoHardwareEncoder)
		}
		return
	}
	if enc.Codec() != CodecH264 {
		t.Errorf("ModeH264 produced %v, want %v", enc.Codec(), CodecH264)
	}
	enc.Close()
}

func TestVideoBackendDescriptions(t *testing.T) {
	cfg := normalizeHubConfig(HubConfig{})

	codec, desc, err := videoBackend(ModeJPEG, cfg, 320, 240)
	if err != nil {
		t.Fatalf("ModeJPEG: %v", err)
	}
	if codec != CodecJPEG || desc == "" {
		t.Errorf("ModeJPEG: codec %v, desc %q", codec, desc)
	}

	// Auto reports something either way: an operator reading the banner has to
	// be able to tell which path the machine took, and a machine with no
	// encoder has to say so rather than looking like it simply chose JPEG.
	codec, desc, err = videoBackend(ModeAuto, cfg, 320, 240)
	if err != nil {
		t.Fatalf("ModeAuto: %v", err)
	}
	if codec != CodecJPEG && codec != CodecH264 {
		t.Errorf("ModeAuto: codec %v", codec)
	}
	if desc == "" {
		t.Error("ModeAuto: empty description")
	}
}

// ---------------------------------------------------------------------------
// The replay rule, over a real connection
// ---------------------------------------------------------------------------

// A client that has just connected is replayed the last frame so the canvas is
// not blank while it waits for the next capture tick. Under H.264 that is only
// safe for a keyframe: a predicted frame sent to a decoder that has seen no
// keyframe is an error, not a head start.
//
// The hub is built here without starting Run, so the only frames on the wire are
// the ones this test puts there. That is what makes it deterministic, and it is
// also what lets it cover the H.264 half of the rule on a machine with no H.264
// encoder: the bytes are never decoded, only routed.
func TestNewClientIsOnlyReplayedAKeyframe(t *testing.T) {
	hub := newHub(normalizeHubConfig(HubConfig{}), newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
	t.Cleanup(hub.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := streamURL(srv)

	// A predicted frame nobody has a reference for. It must not be replayed.
	delta := frameMessage(CodecH264, false, []byte("delta"))
	hub.Broadcast(delta, CodecH264, false)

	conn := dial(t, url)
	if got := readBinary(t, conn, 300*time.Millisecond); got != nil {
		t.Errorf("a delta frame was replayed to a new client: % x", got)
	}

	// The keyframe that follows it must be.
	key := frameMessage(CodecH264, true, []byte("keyframe"))
	hub.Broadcast(key, CodecH264, true)

	conn2 := dial(t, url)
	if got := readBinary(t, conn2, 2*time.Second); !bytes.Equal(got, key) {
		t.Errorf("replayed % x, want the keyframe % x", got, key)
	}
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// streamURL is the address a current frontend dials: the token, and the frame
// layout it was built against. Every client that expects to be served has to
// say both, so the tests say them the same way the page does.
func streamURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?token=test-secret&fmt=" + wireFormat
}

// readBinary waits up to d for a binary message and returns it, or nil if none
// arrived. Text messages -- the screen size and the stats, which a new client is
// sent first -- are skipped, since they are not what these tests are about.
func readBinary(t *testing.T, conn *websocket.Conn, d time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if err := conn.SetReadDeadline(deadline); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		if kind == websocket.BinaryMessage {
			return data
		}
	}
}

// The intra-frame path has to keep working end to end, header and all: it is
// what every client falls back to, and what a browser without WebCodecs gets.
func TestJPEGFramesArriveFramed(t *testing.T) {
	hub := newHub(normalizeHubConfig(HubConfig{VideoMode: ModeJPEG}), newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
	t.Cleanup(hub.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Run the loop, unlike the test above: this one is about what the capture
	// loop actually puts on the wire.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)

	conn := dial(t, streamURL(srv))

	msg := readBinary(t, conn, 3*time.Second)
	if msg == nil {
		t.Fatal("no frame arrived")
	}
	if len(msg) <= videoHeaderLen {
		t.Fatalf("frame is %d bytes, too short to hold a header and a payload", len(msg))
	}
	if msg[0] != byte(CodecJPEG) {
		t.Errorf("codec byte = %d, want %d", msg[0], byte(CodecJPEG))
	}
	if msg[1]&FlagKey == 0 {
		t.Error("a JPEG arrived without the keyframe flag; every JPEG is a keyframe")
	}
	if _, err := jpeg.Decode(bytes.NewReader(msg[videoHeaderLen:])); err != nil {
		t.Errorf("payload is not a JPEG: %v", err)
	}
}

// The screen size still has to reach the client, since it is sent before any
// frame and the canvas is laid out from it.
func TestNewClientIsToldTheScreenSize(t *testing.T) {
	hub := newHub(normalizeHubConfig(HubConfig{VideoMode: ModeJPEG}), newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
	t.Cleanup(hub.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", hub.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	conn := dial(t, streamURL(srv))
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	kind, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("first message is not text: %v", kind)
	}
	if !bytes.Contains(data, []byte(`"screen"`)) {
		t.Errorf("first message = %s, want a screen message", data)
	}
}
