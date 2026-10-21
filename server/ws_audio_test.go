package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// scriptedAudio is a capture device the test drives.
//
// It reproduces the two properties of WASAPI loopback the audio path is built
// around: an idle endpoint produces *nothing* rather than a buffer of zeros,
// and the chunks it does produce do not line up with the 20ms packet size --
// which is the entire reason the repacketizer exists. At 441 frames per call it
// hands back 10ms at a time, close to the ~887 the real device returns.
type scriptedAudio struct {
	format audioStreamFormat
	frames int
	period time.Duration

	mu       sync.Mutex
	sounding bool
}

func newScriptedAudio() *scriptedAudio {
	return &scriptedAudio{
		format:   audioStreamFormat{SampleRate: 44100, Channels: 2},
		frames:   441,
		period:   10 * time.Millisecond,
		sounding: true,
	}
}

func (s *scriptedAudio) Format() audioStreamFormat { return s.format }

func (s *scriptedAudio) Read() ([]int16, bool, error) {
	// Pace like the device: a real Read blocks for roughly the length of the
	// audio it hands back, which is what keeps the loop at real time.
	time.Sleep(s.period)

	s.mu.Lock()
	sounding := s.sounding
	s.mu.Unlock()

	if !sounding {
		return nil, false, nil
	}

	out := make([]int16, s.frames*s.format.Channels)
	for i := range out {
		// Deliberately non-zero: the watchdog counts an all-zero chunk as
		// silence too, so a fake that emitted zeros would exercise the fallback
		// instead of the streaming path.
		out[i] = int16(1000 + i%97)
	}
	return out, true, nil
}

func (s *scriptedAudio) Close() {}

func (s *scriptedAudio) setSounding(v bool) {
	s.mu.Lock()
	s.sounding = v
	s.mu.Unlock()
}

// audioHandshake is the parsed form of the {"type":"audio"} message.
type audioHandshake struct {
	SampleRate int `json:"sampleRate"`
	Channels   int `json:"channels"`
	Bits       int `json:"bits"`
	PacketMs   int `json:"packetMs"`
}

// audioTally records what actually arrived on an audio socket. Like the video
// test it reads from its own goroutine and never uses a read deadline, which
// would poison the connection in gorilla/websocket.
type audioTally struct {
	mu        sync.Mutex
	handshake *audioHandshake
	packets   int
	bytes     int
	oddSizes  int // binary messages that were not exactly one packet long
	sawIdle   bool
	sawSound  bool
}

func (a *audioTally) addPacket(n, want int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.packets++
	a.bytes += n
	if want > 0 && n != want {
		a.oddSizes++
	}
}

func (a *audioTally) addControl(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &head) != nil {
		return
	}

	switch head.Type {
	case "audio":
		var hs audioHandshake
		if json.Unmarshal(data, &hs) != nil {
			return
		}
		a.mu.Lock()
		a.handshake = &hs
		a.mu.Unlock()
	case "audioStats":
		var st struct {
			Idle bool `json:"idle"`
		}
		if json.Unmarshal(data, &st) != nil {
			return
		}
		a.mu.Lock()
		if st.Idle {
			a.sawIdle = true
		} else {
			a.sawSound = true
		}
		a.mu.Unlock()
	}
}

func (a *audioTally) snapshot() (hs *audioHandshake, packets, bytes, odd int, sawIdle, sawSound bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.handshake, a.packets, a.bytes, a.oddSizes, a.sawIdle, a.sawSound
}

// newAudioTestHub builds a Hub whose audio loop runs against the injected
// backend, and serves the real /ws/audio endpoint over a real socket.
func newAudioTestHub(t *testing.T, cfg HubConfig, backend func() (AudioCapturer, error)) (*Hub, *httptest.Server) {
	t.Helper()

	cfg.AudioBackend = backend
	hub := newHub(normalizeHubConfig(cfg), newScriptedCapturer(),
		NewAuthenticator(AuthConfig{Password: "test-secret"}),
		slog.New(slog.DiscardHandler))
	t.Cleanup(hub.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)
	go hub.RunAudio(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws/audio", hub.ServeAudioWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return hub, srv
}

func dialAudio(t *testing.T, srv *httptest.Server, token string) (*websocket.Conn, *audioTally) {
	t.Helper()

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/audio?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial audio: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	counts := &audioTally{}
	go func() {
		for {
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch kind {
			case websocket.BinaryMessage:
				counts.addPacket(len(data), 0)
			case websocket.TextMessage:
				counts.addControl(data)
			}
		}
	}()
	return conn, counts
}

// waitUntil polls cond, which is how these tests wait on state owned by another
// goroutine without sprinkling arbitrary sleeps everywhere.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func audioTestConfig() HubConfig {
	return HubConfig{
		FPS: 5, Quality: 60, Scale: 1,
		StatsPeriod:  200 * time.Millisecond,
		AudioEnabled: true,
	}
}

// The contract the browser depends on: the format arrives before the packets,
// and every packet is exactly one 20ms frame of interleaved PCM.
func TestAudioSendsFormatThenFixedSizePackets(t *testing.T) {
	audio := newScriptedAudio()
	_, srv := newAudioTestHub(t, audioTestConfig(), func() (AudioCapturer, error) { return audio, nil })

	_, counts := dialAudio(t, srv, "test-secret")
	want := audio.format.packetBytes()

	// Let the capture open and settle before measuring a clean second.
	waitUntil(t, "the format handshake", func() bool {
		hs, _, _, _, _, _ := counts.snapshot()
		return hs != nil
	})
	time.Sleep(200 * time.Millisecond)

	_, before, beforeBytes, _, _, _ := counts.snapshot()
	time.Sleep(time.Second)
	hs, after, afterBytes, odd, _, _ := counts.snapshot()

	if hs == nil {
		t.Fatal("no audio handshake arrived")
	}
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"sampleRate", hs.SampleRate, 44100},
		{"channels", hs.Channels, 2},
		{"bits", hs.Bits, audioBitsPerSample},
		{"packetMs", hs.PacketMs, audioPacketMs},
	} {
		if tc.got != tc.want {
			t.Errorf("handshake %s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// The size is the load-bearing assertion: the browser splits the buffer by
	// frame, so a packet that is not whole frames shifts every channel after it.
	if odd != 0 {
		t.Errorf("%d of %d packets were not exactly %d bytes", odd, after, want)
	}
	if got := after - before; got < 30 {
		t.Errorf("%d packets in one second, want at least 30 (~50 at real time)", got)
	}

	// The wire rate has to land near what the format predicts, because the
	// video budget reserves the nominal rate rather than measuring it. The band
	// is wide on purpose: this box's timer granularity stretches the fake's
	// 10ms pacing, and it is the packet size above that has to be exact.
	observed := int64(afterBytes-beforeBytes) * 8
	nominal := int64(audio.format.nominalBps())
	if observed < nominal/2 || observed > nominal*3/2 {
		t.Errorf("measured %d bps, want within 50%% of the nominal %d bps", observed, nominal)
	}
	t.Logf("%d packets/s, %d bytes each, %d bps measured against %d bps nominal",
		after-before, want, observed, nominal)
}

// Going quiet must stop the stream without ending it. This is the audio
// counterpart of a still screen, and it is what makes an idle desktop free.
func TestAudioStopsWhileSilentAndResumesAfter(t *testing.T) {
	audio := newScriptedAudio()
	hub, srv := newAudioTestHub(t, audioTestConfig(), func() (AudioCapturer, error) { return audio, nil })

	_, counts := dialAudio(t, srv, "test-secret")

	waitUntil(t, "the stream to start", func() bool {
		_, packets, _, _, _, _ := counts.snapshot()
		return packets >= 20
	})
	_, sounding, _, _, _, _ := counts.snapshot()

	// Quiet. The watchdog waits out silenceHold before it stops, so allow for
	// that plus a stats period and some slack.
	audio.setSounding(false)
	waitUntil(t, "the stream to report idle", func() bool {
		_, _, _, _, sawIdle, _ := counts.snapshot()
		return sawIdle
	})
	time.Sleep(300 * time.Millisecond)

	_, quiet, _, _, sawIdle, _ := counts.snapshot()
	if !sawIdle {
		t.Error("no audioStats reported idle, want the silence announced rather than implied")
	}
	// A packet or two in flight when the audio stopped is fine; a continuing
	// stream is not.
	if got := quiet - sounding; got > 4 {
		t.Errorf("%d packets arrived after the audio went quiet, want at most 4", got)
	}
	if !hub.AudioIdle() {
		t.Error("AudioIdle() = false while the endpoint was quiet")
	}

	// Sound again: the stream has to come back on its own, without a reconnect.
	audio.setSounding(true)
	waitUntil(t, "the stream to resume", func() bool {
		_, packets, _, _, _, _ := counts.snapshot()
		return packets >= quiet+20
	})

	_, resumed, _, _, _, sawSound := counts.snapshot()
	if !sawSound {
		t.Error("no audioStats reported sound again after the silence")
	}
	if hub.AudioIdle() {
		t.Error("AudioIdle() = true after the sound came back")
	}
	t.Logf("%d packets sounding, %d after going quiet, %d after resuming",
		sounding, quiet-sounding, resumed-quiet)
}

// A listener is not a viewer: it is counted on its own, and it is what turns
// the audio cost on in the video budget.
func TestAudioListenersAreCountedAndBudgeted(t *testing.T) {
	audio := newScriptedAudio()
	hub, srv := newAudioTestHub(t, audioTestConfig(), func() (AudioCapturer, error) { return audio, nil })

	if got := hub.AudioClients(); got != 0 {
		t.Fatalf("AudioClients() = %d before anyone listened, want 0", got)
	}
	if got := hub.audioNominal.Load(); got != 0 {
		t.Errorf("audioNominal = %d with no listener, want 0 so the video budget is untouched", got)
	}

	conn, counts := dialAudio(t, srv, "test-secret")
	// The handshake means the device is open and the format is known, which is
	// the point at which the video budget can be charged for it.
	waitUntil(t, "the format handshake", func() bool {
		hs, _, _, _, _, _ := counts.snapshot()
		return hs != nil
	})

	if got := hub.AudioClients(); got != 1 {
		t.Errorf("AudioClients() = %d with one listener, want 1", got)
	}
	if got, want := hub.audioNominal.Load(), audio.format.nominalBps(); got != want {
		t.Errorf("audioNominal = %d, want %d: the video budget must reserve the audio rate", got, want)
	}

	_ = conn.Close()
	waitUntil(t, "the listener to be forgotten", func() bool { return hub.AudioClients() == 0 })
	// The device is released with the last listener, which is what takes the
	// audio cost back out of the video budget.
	waitUntil(t, "the audio cost to be released", func() bool { return hub.audioNominal.Load() == 0 })
}

// With -audio=false the endpoint is simply absent, and nothing is ever opened.
func TestAudioEndpointIsNotFoundWhenDisabled(t *testing.T) {
	created := 0
	cfg := audioTestConfig()
	cfg.AudioEnabled = false

	_, srv := newAudioTestHub(t, cfg, func() (AudioCapturer, error) {
		created++
		return newScriptedAudio(), nil
	})

	resp, err := http.Get(srv.URL + "/ws/audio?token=test-secret")
	if err != nil {
		t.Fatalf("GET /ws/audio: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d with audio disabled, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if created != 0 {
		t.Errorf("the capture backend was opened %d times with audio disabled, want 0", created)
	}
}

// A bad token must be refused before the socket is upgraded, exactly as on the
// video endpoint.
func TestAudioRejectsBadToken(t *testing.T) {
	audio := newScriptedAudio()
	_, srv := newAudioTestHub(t, audioTestConfig(), func() (AudioCapturer, error) { return audio, nil })

	_, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/audio?token=wrong", nil)
	if err == nil {
		t.Fatal("dial with a bad token succeeded, want a refused handshake")
	}
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	}
}
