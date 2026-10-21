package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Timing constants for the WebSocket transport.
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = 25 * time.Second // must be < pongWait
	maxMessageSize = 1 << 20          // 1 MiB: a paste, not a file transfer
	sendQueueLen   = 4                // messages buffered per client before dropping

	// audioSendQueueLen is the audio socket's queue, in packets. It is much
	// deeper than the video queue because the two losses are not comparable: a
	// dropped frame is one stale picture, while a dropped audio packet is an
	// audible gap. At 20 ms per packet this is about 320 ms of slack.
	audioSendQueueLen = 16
)

// audioRetryDelay is how long to wait before trying to open the capture device
// again, so a machine with no output device does not spin.
const audioRetryDelay = 3 * time.Second

// How hard to try to get a clipboard reply into a full send queue. Long enough
// to outlast a burst of frames, short enough that a reply for an action the user
// has already forgotten does not arrive much later.
const (
	clipboardSendTimeout = time.Second
	clipboardSendRetry   = 20 * time.Millisecond
)

// noH264Message is what a client that cannot decode H.264 is told when the
// server has been configured to send nothing else. It reaches the browser as
// the WebSocket close reason, which is the only string that survives a failed
// connection, so it has to explain both what happened and what to do about it.
//
// It has to stay under 123 bytes. That is the ceiling RFC 6455 puts on a close
// reason, and a frame over it is malformed rather than truncated: the browser
// discards the whole frame, so an over-long message arrives as a bare close with
// no reason at all -- the opposite of what this string is for. Chinese is three
// bytes a character here, which is what makes this easy to get wrong.
const noH264Message = "无法解码 H.264：WebCodecs 需要 HTTPS；或服务端用 -codec jpeg 启动"

// wireFormat is the version of the binary frame layout the client has to
// understand to be served. It goes in the connection URL as ?fmt=.
//
// It exists because the frontend is embedded in the binary, so the two halves of
// this program are always built from the same tree -- and the only way to get a
// mismatch is to rebuild one half and not the other, which is easy to do (go
// build, or build.ps1 -SkipFrontend) and produces the worst possible symptom.
// An old client handed a framed message does not fail loudly: it passes the
// whole thing, header included, to createImageBitmap, which rejects it, and the
// rejection is discarded by the same catch that handles a genuinely corrupt
// frame. The result is a black screen, a bandwidth graph that keeps moving, and
// nothing at all in either console.
//
// A version number rather than a share of the code, because the two halves do
// not share a build step: the byte is the contract.
const wireFormat = "1"

// staleClientMessage is what a page built against a different frame layout is
// told. The person reading it is usually not the person who built the binary,
// so it names the one thing that fixes it rather than the version numbers.
//
// Like noH264Message it has to stay under the 123-byte close reason limit.
const staleClientMessage = "页面前端版本过旧：请重新完整构建（不要用 -SkipFrontend）"

// controlWindow is the shortest interval the rate controller will act on.
// Feeding it faster makes it chase noise -- and its own last adjustment, since
// lowering the frame rate lowers the very bitrate being measured.
const controlWindow = time.Second

// HubConfig configures the streaming hub.
type HubConfig struct {
	FPS         int
	Quality     int
	Scale       float64
	MaxClients  int
	StatsPeriod time.Duration

	// MaxBitrate is the target for the server's *total* egress in bits per
	// second. 0 disables rate control. It is a soft ceiling: see rateController.
	MaxBitrate int64
	Priority   Priority

	// MinQuality and MinFPS are the floors the controller may throttle down to.
	MinQuality int
	MinFPS     int

	// StaticSkip skips the encode and the broadcast for a frame identical to the
	// last one sent, which is what makes a still desktop cost no bandwidth.
	StaticSkip bool

	// VideoMode selects how frames are encoded: predicted from each other, or
	// each one alone. See VideoMode for what auto does when the machine or the
	// browser cannot manage the former.
	VideoMode VideoMode

	// AudioEnabled offers the /ws/audio endpoint. When false the route is not
	// served at all and nothing is ever captured.
	AudioEnabled bool

	// ViewOnly is live mode: clients watch and listen, and nothing they send
	// has any effect on this machine. It is a security boundary, not a UI
	// preference -- the check lives on the server (see Client.handle) so a
	// modified or scripted client cannot get around it.
	ViewOnly bool

	// AudioBackend overrides how the audio capturer is created. Tests use it to
	// drive the audio loop against a scripted source; nil means the real WASAPI
	// backend.
	AudioBackend func() (AudioCapturer, error)
}

// Hub streams captured frames to every connected client and applies the input
// events they send back.
type Hub struct {
	cfg      HubConfig
	cap      Capturer
	inj      Injector
	auth     *Authenticator
	log      *slog.Logger
	upgrader websocket.Upgrader

	// closeOnce makes Close idempotent. stop is closed by Close so a capture
	// loop that was started with a context nobody cancels still stops; runDone
	// is closed by Run once it has released the encoder and is no longer
	// touching the capturer's pixels. Close must wait for that before calling
	// cap.Close, because Grab hands out a buffer the capturer owns and
	// cap.Close unmaps it -- freeing it mid-encode is an access violation.
	closeOnce  sync.Once
	stop       chan struct{}
	runStarted atomic.Bool
	runDone    chan struct{}

	mu      sync.RWMutex
	clients map[*Client]struct{}

	// frame is the most recent wire message -- header and all -- served to a
	// client that connects mid-stream. frameKey says whether a decoder can
	// start from it, because for H.264 that is the difference between a picture
	// and a decoder error; frameCodec is what it carries.
	frame      []byte
	frameKey   bool
	frameCodec VideoCodec

	// audio sits alongside clients but is accounted separately: an audio
	// listener is not a viewer, so it does not consume MaxClients.
	audio     map[*audioConn]struct{}
	audioFmt  audioStreamFormat
	audioOn   bool          // a capture is running, so audioFmt is meaningful
	audioWake chan struct{} // nudges RunAudio when the first listener arrives

	// diff, rc and enc belong to the capture loop goroutine alone.
	diff frameDiffer
	rc   *rateController
	enc  VideoEncoder

	// encW and encH are the size enc was built for. The H.264 encoder is
	// configured for one resolution and cannot be handed another, so a change
	// of screen resolution means a new encoder rather than a resize.
	encW, encH int

	// jpegOnly is set when a client reports it cannot decode H.264. Written by
	// the handshake goroutine and read by the capture loop, so it is atomic
	// where the fields above are not.
	//
	// It is sticky: once any client has needed the intra-frame stream, the
	// server stays on it for the rest of the process's life. A viewer leaving
	// is not a reason to make every remaining client tear down its decoder and
	// wait for a keyframe.
	jpegOnly atomic.Bool

	// videoCodec is the codec of the most recent frame, published for the HTTP
	// handlers. It is set at construction from the startup probe and updated by
	// the capture loop if it ever has to change.
	videoCodec atomic.Int64

	// videoDesc describes the encoder for the banner, and videoErr is why there
	// is none when there is none.
	videoDesc string
	videoErr  error

	screenW, screenH atomic.Int64
	framesSent       atomic.Uint64
	fpsTenths        atomic.Int64

	// Published by the capture loop for the HTTP handlers to read.
	effQuality atomic.Int64
	effFPS     atomic.Int64
	bitrateBps atomic.Int64
	idle       atomic.Bool
	limited    atomic.Bool
	atFloors   atomic.Bool

	// Published by the audio loop.
	audioClients atomic.Int64
	audioKbps    atomic.Int64
	audioIdle    atomic.Bool
	// audioNominal is the wire cost of the running audio capture, which the
	// video budget subtracts. Zero whenever nothing is being captured, so a
	// stream nobody is listening to costs the video path nothing.
	audioNominal atomic.Int64
}

// NewHub wires up the capture and input backends.
func NewHub(cfg HubConfig, auth *Authenticator, log *slog.Logger) (*Hub, error) {
	cfg = normalizeHubConfig(cfg)

	cap, err := NewCapturer(CaptureConfig{Scale: cfg.Scale})
	if err != nil {
		return nil, err
	}

	h := newHub(cfg, cap, auth, log)

	// A capture backend can work perfectly and still be serving the wrong thing
	// -- an XWayland root window is the whole picture on a Wayland session, and
	// a display without MIT-SHM costs a full screen copy per frame. Neither is
	// visible from the output, so the backend gets a chance to say so once.
	if note, ok := cap.(platformNote); ok {
		for _, n := range note.PlatformNotes() {
			log.Warn(n)
		}
	}

	if h.videoErr != nil {
		// Only -codec h264 reaches here, and only on a machine without an
		// encoder for it: the operator asked for something this machine cannot
		// do, and starting anyway would serve an empty screen.
		h.Close()
		return nil, h.videoErr
	}
	return h, nil
}

func normalizeHubConfig(cfg HubConfig) HubConfig {
	// The zero value has to mean the documented default. Leaving it empty made
	// auto silently behave like h264: videoBackend only special-cases ModeJPEG,
	// and the jpegOnly downgrade is gated on ModeAuto, so a client that cannot
	// decode H.264 was still sent it.
	if cfg.VideoMode == "" {
		cfg.VideoMode = ModeAuto
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 15
	}
	if cfg.Quality <= 0 || cfg.Quality > 100 {
		cfg.Quality = 65
	}
	if cfg.StatsPeriod <= 0 {
		cfg.StatsPeriod = time.Second
	}
	if cfg.MaxBitrate < 0 {
		cfg.MaxBitrate = 0
	}
	if cfg.MinQuality <= 0 {
		cfg.MinQuality = minQualityFloor
	}
	// The floors must not sit above the ceilings, or the controller would be
	// "throttling" by raising the setting.
	cfg.MinQuality = min(cfg.MinQuality, cfg.Quality)
	if cfg.MinFPS <= 0 {
		cfg.MinFPS = minFPSFloor
	}
	cfg.MinFPS = min(cfg.MinFPS, cfg.FPS)
	return cfg
}

// newHub finishes construction around a capture backend the caller supplies.
// Split out so tests can drive the real capture loop and transport against a
// scripted frame source, instead of depending on whatever is on screen.
func newHub(cfg HubConfig, cap Capturer, auth *Authenticator, log *slog.Logger) *Hub {
	h := &Hub{
		cfg:       cfg,
		cap:       cap,
		inj:       NewInjector(),
		auth:      auth,
		log:       log,
		clients:   make(map[*Client]struct{}),
		audio:     make(map[*audioConn]struct{}),
		audioWake: make(chan struct{}, 1),
		stop:      make(chan struct{}),
		runDone:   make(chan struct{}),
		rc:        newRateController(cfg.Priority, cfg.Quality, cfg.MinQuality, cfg.FPS, cfg.MinFPS),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 1 << 20,
			// The password gate is the access control here, not the Origin
			// header, which is absent or spoofable behind reverse proxies.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}

	// Which encoder this machine will run, settled now rather than at the first
	// captured frame, so that -codec h264 on a machine that cannot manage it
	// stops startup with a reason instead of serving a black screen.
	//
	// The size comes from the capturer because the probe needs the dimensions
	// the frames will actually have, and Scale has already been applied to it.
	// If the screen cannot be measured, the probe is skipped: the capture loop
	// settles the same question on its first frame, at the cost of a log line
	// arriving later.
	if cw, ch := cap.Size(); cw > 0 && ch > 0 {
		codec, desc, err := videoBackend(cfg.VideoMode, cfg, cw, ch)
		if err != nil {
			h.log.Error("video encoder unavailable", "mode", string(cfg.VideoMode), "err", err)
			h.videoErr = err
		} else {
			h.videoCodec.Store(int64(codec))
			h.videoDesc = desc
		}
	}

	w, hh := ScreenSize()
	h.screenW.Store(int64(w))
	h.screenH.Store(int64(hh))
	h.publishSetpoint()
	return h
}

// Default floors for the rate controller, used when the flags are left at 0.
const (
	minQualityFloor = 30
	minFPSFloor     = 5
)

// publishSetpoint copies the controller's state into the atomics the HTTP
// handlers read. Only the capture loop mutates the controller.
func (h *Hub) publishSetpoint() {
	h.effQuality.Store(int64(h.rc.quality()))
	h.effFPS.Store(int64(h.rc.fps()))
	h.bitrateBps.Store(int64(h.rc.bitrate()))
	h.limited.Store(h.rc.limited())
	h.atFloors.Store(h.rc.atFloors())
}

// ScreenSize reports the native desktop resolution.
func (h *Hub) ScreenSize() (int, int) {
	return int(h.screenW.Load()), int(h.screenH.Load())
}

// Count returns the number of connected clients.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// FPS reports the measured capture rate.
func (h *Hub) FPS() float64 { return float64(h.fpsTenths.Load()) / 10 }

// FramesSent reports how many distinct frames reached at least one client.
func (h *Hub) FramesSent() uint64 { return h.framesSent.Load() }

// EffectiveFPS reports the frame rate the stream is currently running at. It
// differs from FPS, which is what was actually measured; this is the setpoint,
// which is what the client should expect.
func (h *Hub) EffectiveFPS() int { return int(h.effFPS.Load()) }

// EffectiveQuality reports the JPEG quality the stream is currently encoded at.
func (h *Hub) EffectiveQuality() int { return int(h.effQuality.Load()) }

// Bitrate reports the most recent measured egress, in bits per second.
func (h *Hub) Bitrate() int64 { return h.bitrateBps.Load() }

// Idle reports whether the last capture found the screen unchanged.
func (h *Hub) Idle() bool { return h.idle.Load() }

// Limited reports whether the rate controller has throttled below the configured
// maximum because the stream was over its bitrate target.
func (h *Hub) Limited() bool { return h.limited.Load() }

// AtFloors reports that the controller has throttled as far as it can and the
// stream is still over target, so only -scale can bring the bitrate down.
func (h *Hub) AtFloors() bool { return h.atFloors.Load() }

// frameInterval is the current gap between capture ticks, derived from the
// controller's live frame rate rather than the configured one.
func (h *Hub) frameInterval() time.Duration {
	fps := h.rc.fps()
	if fps < 1 {
		// Guard the division: a zero frame rate would panic.
		fps = 1
	}
	return time.Second / time.Duration(fps)
}

// Close stops the capture loop, drops all clients, and releases the backend.
//
// Waiting for the loop matters as much as stopping it: the frame Grab returns
// aliases the capturer's own pixels, so freeing them while the loop is still
// encoding reads unmapped memory. Run closes runDone only after its deferred
// discardEncoder has run, which is the point at which nothing else touches the
// frame.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		close(h.stop)

		h.mu.Lock()
		for c := range h.clients {
			c.close()
		}
		for a := range h.audio {
			a.close()
		}
		h.mu.Unlock()

		// A hub built for a test may never have had Run started; there is then
		// nothing to wait for. Run's first act is to publish runStarted, so a
		// loop that is already under way is always visible here.
		if h.runStarted.Load() {
			<-h.runDone
		}

		h.cap.Close()
	})
}

// ServeWS upgrades an HTTP request into a remote desktop session.
//
// The session token (from POST /api/login, or the password itself) must be
// present as the `token` query parameter: the WebSocket handshake cannot carry
// custom headers from a browser.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if !h.auth.Validate(token) {
		h.log.Warn("rejected websocket upgrade", "remote", remoteIP(r))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if h.cfg.MaxClients > 0 && h.Count() >= h.cfg.MaxClients {
		h.log.Warn("rejected websocket upgrade: too many clients", "remote", remoteIP(r))
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}

	// What the browser can decode. It has to come from the client: whether
	// WebCodecs exists depends on the page's origin, not on its user agent, so
	// the same browser answers differently over https and over http.
	// Absent means the client predates this question, and the answer for it is
	// the one that always worked.
	canH264 := r.URL.Query().Get("v") == "h264"

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		h.log.Debug("websocket upgrade failed", "err", err)
		return
	}

	// A page whose frontend was not rebuilt alongside this binary reads binary
	// messages the old way, as a bare JPEG, and hands the frame header to
	// createImageBitmap as if it were image data. That fails silently -- see
	// wireFormat -- so it is refused here with the reason instead of being fed
	// a stream it will paint nothing from.
	//
	// Checked before the codec, because a stale client also answers "no" to the
	// H.264 question and its real problem is the one worth naming.
	if fmt := r.URL.Query().Get("fmt"); fmt != wireFormat {
		h.log.Warn("rejected client with a stale frontend",
			"remote", remoteIP(r), "fmt", fmt, "want", wireFormat)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, staleClientMessage),
			time.Now().Add(writeWait))
		_ = conn.Close()
		return
	}

	// A client that cannot decode H.264, against a server told to send it and
	// nothing else. There is no frame this connection could be sent that would
	// help, so it is closed with the reason the browser will show the user.
	//
	// The close has to happen after the upgrade rather than before, because a
	// refused handshake reaches the page as a bare "connection failed" with the
	// explanation stripped off, and that is exactly the message that sends
	// someone hunting for a network fault that is not there.
	if !canH264 && h.cfg.VideoMode == ModeH264 {
		h.log.Warn("rejected client that cannot decode H.264",
			"remote", remoteIP(r), "codec", h.cfg.VideoMode)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, noH264Message),
			time.Now().Add(writeWait))
		_ = conn.Close()
		return
	}

	if !canH264 {
		// Not a refusal: the stream will move to intra-frame at the next
		// capture tick and this client will be served like any other.
		if h.jpegOnly.CompareAndSwap(false, true) {
			h.log.Info("client cannot decode H.264; switching the stream to JPEG",
				"remote", remoteIP(r))
		}
	}

	c := &Client{
		wsConn: newWSConn(conn, remoteIP(r), sendQueueLen),
		hub:    h,
	}
	h.add(c)

	h.log.Info("client connected", "remote", c.remoteIP, "clients", h.Count())

	go c.writePump()
	go c.readPump()
}

func (h *Hub) add(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	last, lastKey := h.frame, h.frameKey
	h.mu.Unlock()

	// Send the resolution first, then the last frame, so the canvas is never
	// blank while waiting for the next capture tick.
	w, hgt := h.ScreenSize()
	c.enqueueText(screenMsg(w, hgt, h.cfg.ViewOnly))

	// Only a frame a decoder can start from is worth replaying. Under JPEG
	// every frame qualifies, so this changes nothing there. Under H.264 a
	// predicted frame is not a small head start but a decode error, and one
	// that arrives before the client has seen a keyframe is worse than an
	// empty screen: it costs a decoder reset at exactly the moment the client
	// is trying to start. The keyframe it is waiting for is at most a second
	// away, which is what the short GOP is for.
	if last != nil && lastKey {
		c.enqueueFrame(last)
	}
}

func (h *Hub) remove(c *Client) {
	h.mu.Lock()
	_, ok := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()

	if ok {
		h.log.Info("client disconnected", "remote", c.remoteIP, "clients", h.Count())
	}
}

// VideoDescription names the encoder this hub will run, for the startup banner.
func (h *Hub) VideoDescription() string { return h.videoDesc }

// VideoCodec reports the codec of the most recent frame, or of the encoder the
// startup probe settled on before the first frame exists.
func (h *Hub) VideoCodec() VideoCodec { return VideoCodec(h.videoCodec.Load()) }

// Broadcast delivers one encoded frame to every client, dropping it for any
// client whose queue is full rather than letting one slow consumer stall the
// capture loop for everyone. It returns how many clients actually queued it.
//
// msg is the framed wire message -- codec tag and flags included -- because it
// is stored verbatim for the next client that connects and cannot be rebuilt
// afterwards: the payload it points into belongs to the encoder and is
// overwritten by the next frame.
func (h *Hub) Broadcast(msg []byte, codec VideoCodec, key bool) int {
	h.mu.Lock()
	h.frame = msg
	h.frameKey = key
	h.frameCodec = codec
	queued := 0
	for c := range h.clients {
		if c.enqueueFrame(msg) {
			queued++
		}
	}
	h.mu.Unlock()

	// Only count frames that reached somebody: a frame every client's queue
	// dropped is not traffic, and counting it would inflate the reported rate.
	if queued > 0 {
		h.framesSent.Add(1)
	}
	return queued
}

// broadcastText sends a JSON control message to every client.
func (h *Hub) broadcastText(msg []byte) {
	h.mu.Lock()
	for c := range h.clients {
		c.enqueueText(msg)
	}
	h.mu.Unlock()
}

// Run drives the capture loop until ctx is cancelled or the hub is closed.
func (h *Hub) Run(ctx context.Context) {
	h.runStarted.Store(true)
	defer close(h.runDone)

	// A timer rather than a ticker, because the effective frame rate changes at
	// runtime when the rate controller throttles.
	timer := time.NewTimer(h.frameInterval())
	defer timer.Stop()

	stats := time.NewTicker(h.cfg.StatsPeriod)
	defer stats.Stop()

	// The encoder is this goroutine's, so this is where it is released: the
	// same goroutine that built it, and after the loop has stopped using it.
	// Closing it from Hub.Close instead would have the shutdown path racing the
	// loop for the encoder it is midway through encoding with.
	defer h.discardEncoder()

	var framesSince uint64 // frames broadcast since the last stats tick
	lastStats := time.Now()
	lastW, lastH := h.ScreenSize()

	// The controller keeps its own window so that a short StatsPeriod cannot
	// make it act on noise.
	var ctrlBytes uint64
	ctrlStart := time.Now()
	warnedSaturated := false

	h.log.Info("capture loop started",
		"fps", h.cfg.FPS, "quality", h.cfg.Quality, "scale", h.cfg.Scale,
		"static_skip", h.cfg.StaticSkip, "max_bitrate", h.cfg.MaxBitrate,
		"priority", h.cfg.Priority,
		"resolution", fmt.Sprintf("%dx%d", lastW, lastH))

	for {
		select {
		case <-ctx.Done():
			return

		case <-h.stop:
			return

		case <-timer.C:
			// Reschedule before doing any work. Encoding takes real time, and
			// resetting afterwards would add it to every period -- silently
			// cutting the frame rate well below the configured one.
			timer.Reset(h.frameInterval())

			// Idle when nobody is watching: this is the difference between a
			// background service costing ~0% CPU and one pegging a core.
			if h.Count() == 0 {
				continue
			}

			wire, err := h.captureAndSend()
			if err != nil {
				h.log.Error("capture failed", "err", err)
				continue
			}
			if wire > 0 {
				framesSince++
				ctrlBytes += uint64(wire)
			}

		case <-stats.C:
			now := time.Now()

			elapsed := now.Sub(lastStats).Seconds()
			fps := 0.0
			if elapsed > 0 {
				fps = float64(framesSince) / elapsed
			}
			h.fpsTenths.Store(int64(fps*10 + 0.5))
			framesSince = 0
			lastStats = now

			if w, hgt := ScreenSize(); w != lastW || hgt != lastH {
				lastW, lastH = w, hgt
				h.screenW.Store(int64(w))
				h.screenH.Store(int64(hgt))
				h.log.Info("screen resolution changed", "resolution", fmt.Sprintf("%dx%d", w, hgt))
				h.broadcastText(screenMsg(w, hgt, h.cfg.ViewOnly))
			}

			if ctrlElapsed := now.Sub(ctrlStart).Seconds(); ctrlElapsed >= controlWindow.Seconds() {
				bps := float64(ctrlBytes) * 8 / ctrlElapsed
				// The ceiling covers the server's total egress, so whatever
				// audio is costing comes off the top before video sees it. The
				// nominal rate is used rather than a measurement because PCM is
				// constant-rate: there is nothing to measure, and a target that
				// did not wobble with audio reporting would not make the
				// controller chase it.
				if h.rc.observe(bps, float64(videoBudget(h.cfg.MaxBitrate, h.audioNominal.Load()))) {
					// The setpoint moved, so the frame the clients are holding no
					// longer represents what the server would send now. Without
					// this, a static screen would keep displaying the old quality
					// indefinitely -- precisely while someone is reading it.
					h.diff.Forget()
				}
				if h.rc.atFloors() {
					if !warnedSaturated {
						h.log.Warn("still over the bitrate target at the lowest quality and frame rate: lower -scale or raise -max-bitrate",
							"kbps", int64(bps/1000), "target_kbps", h.cfg.MaxBitrate/1000,
							"audio_kbps", h.audioNominal.Load()/1000)
						warnedSaturated = true
					}
				} else {
					warnedSaturated = false
				}
				h.publishSetpoint()
				ctrlBytes = 0
				ctrlStart = now
			}

			if n := h.Count(); n > 0 {
				h.broadcastText(statsMsg(h.rc.fps(), h.rc.quality(),
					int64(h.rc.bitrate()/1000), h.idle.Load(), h.rc.limited(),
					VideoCodec(h.videoCodec.Load())))
				h.log.Debug("stream stats",
					"fps", fps, "clients", n, "frame_bytes", len(h.frameBytes()))
			}
		}
	}
}

func (h *Hub) frameBytes() []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.frame
}

// captureAndSend grabs one frame and broadcasts it if the screen changed. A
// still desktop therefore costs one memory comparison per tick and nothing
// else: no encode, no broadcast, no bandwidth.
//
// It returns how many bytes were handed to client sockets, which is the rate
// controller's input. That must be what was delivered rather than what was
// generated, or the controller would throttle to fix congestion the transport
// has already absorbed by dropping frames.
//
// Note what this does *not* see: GDI's BitBlt never composites the mouse
// cursor, so moving the pointer across a still screen leaves the pixels
// identical. That is why cursor motion alone sends nothing -- and why adding
// cursor compositing to the capture would need this comparison revisited.
func (h *Hub) captureAndSend() (wire int, err error) {
	img, err := h.cap.Grab()
	if err != nil {
		return 0, err
	}

	pix, stride := img.Pix, img.Stride
	bounds := img.Bounds()
	w, hgt := bounds.Dx(), bounds.Dy()

	if h.cfg.StaticSkip && !h.diff.Changed(pix, stride, w, hgt) {
		h.idle.Store(true)
		return 0, nil
	}
	h.idle.Store(false)

	enc, err := h.encoderFor(w, hgt)
	if err != nil {
		h.diff.Forget()
		return 0, err
	}

	payload, key, err := enc.Encode(img, h.rc.quality())
	if err != nil {
		// Leave the baseline alone. These pixels still need to go out, so the
		// next tick has to see them as changed; advancing the baseline here
		// would swallow the frame for good.
		h.diff.Forget()

		// An inter-frame encoder that fails cannot be patched up in place, and
		// the failure is not transient: the encoder is the machine's, and if it
		// has given up it will give up again. Dropping to the intra-frame path
		// keeps the screen visible, which is the whole point of having one.
		if enc.Codec() == CodecH264 && h.cfg.VideoMode == ModeAuto {
			h.log.Error("H.264 encoder failed; falling back to JPEG", "err", err)
			h.discardEncoder()
		}
		return 0, err
	}

	if h.cfg.StaticSkip {
		// MarkSent copies, and it must: Grab hands back a buffer it reuses, so
		// the next call overwrites these pixels in place.
		h.diff.MarkSent(pix, stride, w, hgt)
	}

	if len(payload) == 0 {
		// The encoder had nothing to emit for this frame. Not an error, and not
		// something to put on the wire: an empty binary message would reach the
		// client as a zero-length frame and cost a decoder call for nothing.
		return 0, nil
	}

	msg := frameMessage(enc.Codec(), key, payload)
	h.videoCodec.Store(int64(enc.Codec()))
	return len(payload) * h.Broadcast(msg, enc.Codec(), key), nil
}

// encoderFor returns the encoder to encode a frame of this size, building one
// if there is none, if the screen has changed size, or if the stream has to
// move to the intra-frame path.
//
// It runs on the capture loop goroutine, which owns enc and everything that
// goes with it: the diff baseline tracks pixels already sent, and a new encoder
// means every pixel is unsent again.
func (h *Hub) encoderFor(w, hgt int) (VideoEncoder, error) {
	if h.enc != nil && h.encW == w && h.encH == hgt && !h.downgrading() {
		return h.enc, nil
	}

	if h.enc != nil {
		switch {
		case h.downgrading():
			h.log.Info("switching the video stream to intra-frame JPEG")
		default:
			h.log.Info("screen resolution changed; rebuilding the video encoder",
				"from", fmt.Sprintf("%dx%d", h.encW, h.encH),
				"to", fmt.Sprintf("%dx%d", w, hgt))
		}
		h.discardEncoder()
	}

	mode := h.cfg.VideoMode
	if mode == ModeAuto && h.jpegOnly.Load() {
		mode = ModeJPEG
	}

	enc, err := newVideoEncoder(mode, h.cfg, w, hgt)
	if err != nil {
		// newVideoEncoder only fails for ModeH264, which asks for an encoder
		// this machine does not have and refuses to be given another.
		return nil, err
	}

	h.enc, h.encW, h.encH = enc, w, hgt
	h.log.Info("video encoder ready", "encoder", enc.Describe())
	return enc, nil
}

// downgrading reports whether the running encoder is producing a codec some
// client cannot decode.
func (h *Hub) downgrading() bool {
	return h.enc != nil && h.enc.Codec() == CodecH264 &&
		h.cfg.VideoMode == ModeAuto && h.jpegOnly.Load()
}

// discardEncoder drops the running encoder and everything that depended on it.
//
// Clearing the last frame is part of it, not tidiness: a client that connects
// between this call and the next capture tick would otherwise be sent a frame
// from the codec that has just been abandoned.
func (h *Hub) discardEncoder() {
	if h.enc != nil {
		h.enc.Close()
		h.enc = nil
	}
	h.encW, h.encH = 0, 0
	h.diff.Forget()

	h.mu.Lock()
	h.frame = nil
	h.frameKey = false
	h.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Audio
// ---------------------------------------------------------------------------

// AudioClients reports how many audio listeners are attached.
func (h *Hub) AudioClients() int { return int(h.audioClients.Load()) }

// AudioKbps reports the most recent measured audio egress, in kilobits per
// second.
func (h *Hub) AudioKbps() int64 { return h.audioKbps.Load() }

// AudioIdle reports whether the audio stream has gone quiet, either because
// nothing is playing or because nothing is being captured.
func (h *Hub) AudioIdle() bool { return h.audioIdle.Load() }

// ServeAudioWS upgrades an HTTP request into an audio stream.
//
// Audio gets its own socket rather than sharing the video one, for three
// reasons: WebSocket has no prioritisation, so a 200 KB frame ahead of an audio
// packet on a slow link is 200 ms of silence; the two need different drop
// policies; and the video connection's binary messages are all JPEG by
// definition, so multiplexing would mean touching the video path's framing.
//
// The cost is that there is no shared clock, so audio and video are not
// synchronised with each other. Neither is delayed by the other, which is what
// matters for remote assistance.
func (h *Hub) ServeAudioWS(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.AudioEnabled {
		http.NotFound(w, r)
		return
	}

	token := r.URL.Query().Get("token")
	if !h.auth.Validate(token) {
		h.log.Warn("rejected audio websocket upgrade", "remote", remoteIP(r))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Debug("audio websocket upgrade failed", "err", err)
		return
	}

	a := &audioConn{
		wsConn: newWSConn(conn, remoteIP(r), audioSendQueueLen),
		hub:    h,
	}
	h.addAudio(a)

	go a.writePump()
	go a.readPump()
}

func (h *Hub) addAudio(a *audioConn) {
	h.mu.Lock()
	h.audio[a] = struct{}{}
	n := len(h.audio)
	f, active := h.audioFmt, h.audioOn
	h.mu.Unlock()

	h.audioClients.Store(int64(n))
	h.log.Info("audio client connected", "remote", a.remoteIP, "audio_clients", n)

	// If a capture is already running, describe it now. Otherwise the format
	// arrives when the capture opens, which is a moment later -- the listener
	// may well be what starts it.
	if active {
		a.enqueueText(audioMsg(f))
	}

	// Wake the capture loop. The buffer is one deep and the send is
	// non-blocking: a pending wake-up already covers this listener.
	select {
	case h.audioWake <- struct{}{}:
	default:
	}
}

func (h *Hub) removeAudio(a *audioConn) {
	h.mu.Lock()
	_, ok := h.audio[a]
	delete(h.audio, a)
	n := len(h.audio)
	h.mu.Unlock()

	if !ok {
		return
	}
	h.audioClients.Store(int64(n))
	h.log.Info("audio client disconnected", "remote", a.remoteIP, "audio_clients", n)
}

// broadcastAudio delivers one PCM packet to every listener, dropping it for any
// listener whose queue is full. It returns the bytes actually queued, which is
// what the reported audio rate is measured from -- the same "delivered, not
// produced" rule the video path follows.
//
// pkt must not be reused by the caller: it stays reachable from the send queues
// until each listener's writer has drained it, which is why the loop hands over
// a fresh buffer per packet instead of recycling one.
func (h *Hub) broadcastAudio(pkt []byte) int {
	h.mu.Lock()
	queued := 0
	for a := range h.audio {
		if a.enqueueFrame(pkt) {
			queued++
		}
	}
	h.mu.Unlock()
	return len(pkt) * queued
}

// broadcastAudioText sends a JSON control message to every audio listener.
func (h *Hub) broadcastAudioText(msg []byte) {
	h.mu.Lock()
	for a := range h.audio {
		a.enqueueText(msg)
	}
	h.mu.Unlock()
}

func (h *Hub) audioListenerCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.audio)
}

// openAudio builds the capture backend, honouring a test-supplied one.
func (h *Hub) openAudio() (AudioCapturer, error) {
	if h.cfg.AudioBackend != nil {
		return h.cfg.AudioBackend()
	}
	return NewAudioCapturer()
}

// reportAudioStats folds the bytes sent since the last tick into the published
// rate and pushes a message to the listeners.
func (h *Hub) reportAudioStats(last *time.Time, sent *int64) {
	now := time.Now()
	elapsed := now.Sub(*last).Seconds()

	kbps := int64(0)
	if elapsed > 0 {
		kbps = int64(float64(*sent) * 8 / elapsed / 1000)
	}
	*sent = 0
	*last = now

	h.audioKbps.Store(kbps)
	h.broadcastAudioStats(kbps, h.audioIdle.Load())
}

func (h *Hub) broadcastAudioStats(kbps int64, idle bool) {
	if h.audioListenerCount() > 0 {
		h.broadcastAudioText(audioStatsMsg(kbps, idle))
	}
}

// audioBackoff waits before another attempt at the capture device, keeping the
// stats flowing so a listener still sees the outage reported. It returns false
// when the context ended.
func (h *Hub) audioBackoff(ctx context.Context, stats *time.Ticker, last *time.Time, sent *int64) bool {
	timer := time.NewTimer(audioRetryDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-stats.C:
			h.reportAudioStats(last, sent)
		}
	}
}

// RunAudio captures system audio and streams it to the audio listeners until
// ctx is cancelled. It runs alongside Run and is independent of it.
//
// Nothing is captured while nobody is listening -- the endpoint is not merely
// muted, it is never opened, so audio costs nothing when it is not being heard.
func (h *Hub) RunAudio(ctx context.Context) {
	if !h.cfg.AudioEnabled {
		return
	}

	stats := time.NewTicker(h.cfg.StatsPeriod)
	defer stats.Stop()

	var (
		cap   AudioCapturer
		rep   *repacketizer
		watch *silenceWatchdog
		sent  int64
		last  = time.Now()
	)

	// stop releases the device and publishes the fact that audio is off, which
	// is also what takes the audio cost out of the video budget.
	stop := func() {
		if cap != nil {
			cap.Close()
			cap = nil
		}
		rep, watch = nil, nil
		h.audioNominal.Store(0)
		h.audioIdle.Store(true)

		h.mu.Lock()
		h.audioOn = false
		h.mu.Unlock()
	}
	defer stop()

	for {
		if ctx.Err() != nil {
			return
		}

		if h.audioListenerCount() == 0 {
			stop()
			select {
			case <-ctx.Done():
				return
			case <-h.audioWake:
			case <-stats.C:
				h.reportAudioStats(&last, &sent)
			}
			continue
		}

		if cap == nil {
			c, err := h.openAudio()
			if err != nil {
				h.log.Error("audio capture unavailable", "err", err)
				if !h.audioBackoff(ctx, stats, &last, &sent) {
					return
				}
				continue
			}
			// A fresh capturer means a fresh stream: the repacketizer must not
			// carry a partial packet across the break.
			cap = c
			rep = newRepacketizer(c.Format())
			watch = newSilenceWatchdog(silenceHold, time.Now())
			h.audioNominal.Store(c.Format().nominalBps())
			h.audioIdle.Store(true)

			h.mu.Lock()
			h.audioFmt, h.audioOn = c.Format(), true
			h.mu.Unlock()

			h.log.Info("audio capture running",
				"rate", c.Format().SampleRate, "channels", c.Format().Channels,
				"kbps", c.Format().nominalBps()/1000)

			// Uncompressed audio is a fixed cost that the ceiling has to cover
			// on its own. When it does not, say so once, plainly: the
			// alternative is an operator watching the picture fall apart with
			// no explanation of where the budget went.
			if h.cfg.MaxBitrate > 0 && c.Format().nominalBps() >= h.cfg.MaxBitrate {
				h.log.Warn("audio alone uses the whole bitrate ceiling; video has nothing left",
					"audio_kbps", c.Format().nominalBps()/1000,
					"target_kbps", h.cfg.MaxBitrate/1000)
			}
			h.broadcastAudioText(audioMsg(c.Format()))
		}

		pcm, ok, err := cap.Read()
		if err != nil {
			// A device change is handled inside Read; this is the unrecoverable
			// case, so the device is released and re-opened after a pause.
			h.log.Error("audio capture failed", "err", err)
			stop()
			if !h.audioBackoff(ctx, stats, &last, &sent) {
				return
			}
			continue
		}

		if watch.observe(time.Now(), ok && peakOf(pcm) > silencePeak) {
			// Quiet. Keep reading, so the instant sound returns is noticed, but
			// send nothing -- this is the audio counterpart of a still screen.
			// Dropping the buffered remainder stops the first packet after a
			// resume from carrying a tail of stale audio.
			rep.reset()
			h.audioIdle.Store(true)
		} else {
			h.audioIdle.Store(false)
			for _, pkt := range rep.push(pcm) {
				// A fresh buffer per packet, unlike the video path's frame:
				// this one stays live in the send queues after broadcast
				// returns, so it cannot be recycled here.
				sent += int64(h.broadcastAudio(encodePCM16(pkt, nil)))
			}
		}

		select {
		case <-stats.C:
			h.reportAudioStats(&last, &sent)
		default:
		}
	}
}

// audioConn is one audio listener: the shared transport, and a read loop that
// exists only so control frames are handled. Audio travels one way, so nothing
// the browser sends is acted on.
type audioConn struct {
	*wsConn
	hub *Hub
}

func (a *audioConn) readPump() {
	defer func() {
		a.hub.removeAudio(a)
		a.close()
	}()

	a.conn.SetReadLimit(maxMessageSize)
	_ = a.conn.SetReadDeadline(time.Now().Add(pongWait))
	a.conn.SetPongHandler(func(string) error {
		return a.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := a.conn.ReadMessage(); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				a.hub.log.Debug("audio read error", "remote", a.remoteIP, "err", err)
			}
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// outMsg is a queued outbound message, tagged with whether it is a binary JPEG
// frame or a JSON control message.
type outMsg struct {
	data   []byte
	binary bool
}

// clientMsg is the uplink envelope. Every browser event shares one shape so a
// single decoder handles all of them.
type clientMsg struct {
	Type   string  `json:"type"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Button string  `json:"button"`
	DeltaX float64 `json:"deltaX"`
	DeltaY float64 `json:"deltaY"`
	Code   string  `json:"code"`
	Key    string  `json:"key"`
	Text   string  `json:"text"`
}

// wsConn is the transport half of a session: one socket, one send queue, one
// writer. Both the video session and the audio stream embed it.
//
// It is shared rather than duplicated because the single-writer discipline in
// writePump is the most delicate part of this file -- gorilla/websocket permits
// exactly one concurrent writer, and every rule that follows from that (the
// queue is the only path to the socket, drops are silent, the ping keeps its
// own deadline) has to hold identically on both sockets. Two copies would drift.
type wsConn struct {
	conn     *websocket.Conn
	send     chan outMsg
	closed   chan struct{}
	remoteIP string

	closeOnce sync.Once
}

func newWSConn(conn *websocket.Conn, remoteIP string, queueLen int) *wsConn {
	return &wsConn{
		conn:     conn,
		send:     make(chan outMsg, queueLen),
		closed:   make(chan struct{}),
		remoteIP: remoteIP,
	}
}

func (c *wsConn) enqueueFrame(data []byte) bool {
	return c.enqueue(outMsg{data: data, binary: true})
}

func (c *wsConn) enqueueText(data []byte) bool { return c.enqueue(outMsg{data: data}) }

// enqueue queues a message, reporting false if it had to be dropped.
func (c *wsConn) enqueue(m outMsg) bool {
	select {
	case c.send <- m:
		return true
	case <-c.closed:
		return false
	default:
		// The client is not draining fast enough; skipping a frame is far
		// better than stalling the capture loop for everyone.
		return false
	}
}

func (c *wsConn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *wsConn) writePump() {
	ping := time.NewTicker(pingPeriod)
	defer func() {
		ping.Stop()
		c.close()
	}()

	for {
		select {
		case <-c.closed:
			return
		case m := <-c.send:
			if err := c.write(m); err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *wsConn) write(m outMsg) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))

	kind := websocket.TextMessage
	if m.binary {
		kind = websocket.BinaryMessage
	}
	return c.conn.WriteMessage(kind, m.data)
}

// Client is a single browser session: the transport above, plus the video
// session state and the uplink that carries input events.
type Client struct {
	*wsConn
	hub *Hub

	// Wheel accumulation: browsers report scroll in pixels, Windows wants
	// discrete 120-unit notches. Trackpads emit many sub-notch deltas, so the
	// remainder is carried until it adds up to a notch.
	wheelAccX float64
	wheelAccY float64
}

// wheelPixelsPerNotch is the browser pixel delta that maps onto one Windows
// wheel notch. Chrome and Firefox report 100px per notch in deltaMode 0.
const wheelPixelsPerNotch = 100.0

func (c *Client) readPump() {
	defer func() {
		c.hub.remove(c)
		c.close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.hub.log.Debug("read error", "remote", c.remoteIP, "err", err)
			}
			return
		}

		var msg clientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			c.hub.log.Debug("malformed message", "remote", c.remoteIP, "err", err)
			continue
		}
		if err := c.handle(&msg); err != nil {
			c.hub.log.Debug("input rejected", "remote", c.remoteIP, "type", msg.Type, "err", err)
		}
	}
}

func (c *Client) handle(msg *clientMsg) error {
	// Live mode drops everything but the liveness probe, here rather than at
	// each injection call site: this is the single point every client-driven
	// side effect passes through, so a new case added below is covered without
	// having to remember this.
	//
	// An allowlist and not a denylist of input types, deliberately: with a
	// denylist, the next input message someone adds would be permitted by
	// default, and the omission would be silent. This way the default is "no".
	//
	// It also covers the two clipboard messages, which are not Injector calls
	// (clipboard_pull reads this machine's clipboard out to the viewer, which
	// is more than watching the screen).
	if c.hub.cfg.ViewOnly && msg.Type != "ping" {
		return nil
	}

	inj := c.hub.inj

	switch msg.Type {
	case "mousemove":
		return inj.MoveTo(int(msg.X), int(msg.Y))

	case "mousedown":
		return inj.Button(msg.Button, true)

	case "mouseup":
		return inj.Button(msg.Button, false)

	case "wheel":
		return c.handleWheel(msg)

	case "keydown", "keyup":
		vk, extended, ok := LookupKey(msg.Code, msg.Key)
		if !ok {
			// Unmappable keys are dropped rather than guessed at; printable
			// text arrives through text_input instead.
			return nil
		}
		return inj.Key(vk, extended, msg.Type == "keydown")

	case "text_input":
		// IME-composed or pasted text: injected as literal Unicode, bypassing
		// the remote keyboard layout and IME entirely.
		return inj.Text(msg.Text)

	case "clipboard_pull":
		// The browser wants the remote clipboard's text.
		text, err := ReadClipboardText()
		if err != nil {
			c.sendClipboard("pull", false, "", err.Error())
			return nil
		}
		c.sendClipboard("pull", true, text, "")
		return nil

	case "clipboard_push":
		// The browser is about to synthesize Ctrl+V, so this has to land before
		// it does. Both travel the same socket and readPump handles them in
		// order, which is what makes that sequencing free.
		if err := WriteClipboardText(msg.Text); err != nil {
			c.sendClipboard("push", false, "", err.Error())
			return nil
		}
		c.sendClipboard("push", true, "", "")
		return nil

	case "ping":
		// Application-level round-trip probe used by the status bar. The reply
		// goes through the send queue: gorilla/websocket allows only one
		// concurrent writer, and that is the write pump.
		c.enqueueText(pongMsg())
		return nil
	}
	return nil
}

// sendClipboard queues a clipboard reply, retrying while the send queue is full.
//
// The video socket's queue is four deep and enqueue drops silently when it is
// full, which is right for frames -- the next one is milliseconds away -- and
// wrong for this: there is exactly one reply per user action, so a drop is
// indistinguishable from the feature not working. The retry runs on its own
// goroutine because readPump also carries the mouse and keyboard, and stalling
// those for a clipboard reply would be a far worse trade.
func (c *Client) sendClipboard(op string, ok bool, text, errMsg string) {
	payload := clipboardMsg(op, ok, text, errMsg)

	go func() {
		deadline := time.Now().Add(clipboardSendTimeout)
		for {
			if c.enqueueText(payload) {
				return
			}
			if time.Now().After(deadline) {
				c.hub.log.Debug("clipboard reply dropped", "remote", c.remoteIP, "op", op)
				return
			}
			time.Sleep(clipboardSendRetry)
		}
	}()
}

// handleWheel converts a browser scroll into Windows wheel notches.
//
// The vertical conventions point in opposite directions while the horizontal
// ones agree, and that asymmetry is exactly what is easy to get wrong:
//
//   - browser deltaY > 0 scrolls *down* (towards the end of the document)
//   - Windows wheel  > 0 scrolls *up*   (away from the user)
//   - browser deltaX > 0 and Windows HWHEEL > 0 both scroll *right*
//
// So only the vertical sign is flipped. Leaving it alone is what made the wheel
// scroll backwards. The flip happens here rather than in the browser so that the
// wire format stays raw browser pixels, as the protocol section documents.
func (c *Client) handleWheel(msg *clientMsg) error {
	c.wheelAccX += msg.DeltaX
	c.wheelAccY -= msg.DeltaY

	unitsX := takeNotches(&c.wheelAccX)
	unitsY := takeNotches(&c.wheelAccY)
	if unitsX == 0 && unitsY == 0 {
		return nil
	}
	return c.hub.inj.Wheel(unitsX, unitsY)
}

// takeNotches converts accumulated pixel scroll into whole Windows wheel units,
// leaving the sub-notch remainder behind.
func takeNotches(acc *float64) int {
	units := 0
	for *acc >= wheelPixelsPerNotch {
		units += wheelDelta
		*acc -= wheelPixelsPerNotch
	}
	for *acc <= -wheelPixelsPerNotch {
		units -= wheelDelta
		*acc += wheelPixelsPerNotch
	}
	return units
}

// ---------------------------------------------------------------------------
// Control messages
// ---------------------------------------------------------------------------

// screenMsg announces the frame geometry, and whether the server is in live
// mode. The flag is purely additive -- an older client ignores it and simply
// does not adapt its UI, while the server still refuses its input -- so the
// wire format version does not need to change for it.
func screenMsg(w, h int, viewOnly bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":     "screen",
		"width":    w,
		"height":   h,
		"viewOnly": viewOnly,
	})
	return b
}

// statsMsg reports the stream's live state once per stats period. The client
// needs it because it cannot infer any of this from the frames themselves: an
// idle screen sends no frames at all, so a frame-counting display would simply
// freeze at its last value and look broken rather than idle.
func statsMsg(fps, quality int, kbps int64, idle, limited bool, video VideoCodec) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "stats",
		"fps":     fps,
		"quality": quality,
		"kbps":    kbps,
		"idle":    idle,
		"limited": limited,
		// The codec is repeated in every stats message rather than announced
		// once when it changes, because the change can happen while a client is
		// reconnecting and the announcement would be missed. It is a handful of
		// bytes once a second, and it makes the client's view of the stream
		// self-correcting.
		"video": video.String(),
	})
	return b
}

// String names a codec for the log and for the stats message.
func (c VideoCodec) String() string {
	switch c {
	case CodecH264:
		return "h264"
	case CodecJPEG:
		return "jpeg"
	default:
		return fmt.Sprintf("codec-%d", uint8(c))
	}
}

// audioMsg describes the stream that is about to arrive. The packets themselves
// are raw PCM and carry no description of their own, so this has to reach the
// client before the first of them -- both when a listener joins a running
// capture, and when the capture starts because a listener joined.
func audioMsg(f audioStreamFormat) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":       "audio",
		"sampleRate": f.SampleRate,
		"channels":   f.Channels,
		"bits":       audioBitsPerSample,
		"packetMs":   audioPacketMs,
	})
	return b
}

// audioStatsMsg is the audio counterpart of statsMsg, and exists for the same
// reason: an idle audio stream sends nothing at all, so a display driven by the
// packets alone would look broken rather than quiet.
func audioStatsMsg(kbps int64, idle bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "audioStats",
		"kbps": kbps,
		"idle": idle,
	})
	return b
}

func pongMsg() []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "pong",
		"t":    time.Now().UnixMilli(),
	})
	return b
}

// clipboardMsg answers a clipboard_push or clipboard_pull.
//
// op echoes which one, because the client reacts differently: a pull carries
// text to put in the *local* clipboard, a push only reports success. An empty
// text with ok=true means the clipboard held no text -- a normal state, not an
// error, and one the user deserves to be told apart from a failure.
func clipboardMsg(op string, ok bool, text, errMsg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":  "clipboard",
		"op":    op,
		"ok":    ok,
		"text":  text,
		"error": errMsg,
	})
	return b
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
