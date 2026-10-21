package server

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func f32bytes(vals ...float32) []byte {
	b := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

func i16bytes(vals ...int16) []byte {
	b := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func i32bytes(vals ...int32) []byte {
	b := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], uint32(v))
	}
	return b
}

// The float path is the one every real machine takes, and the one where an
// over-range sample must clamp rather than wrap: wrapping turns a loud moment
// into its opposite, which is audible as a click.
func TestPCMConvertFloat32(t *testing.T) {
	c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesFloat32}, 2)

	for _, tc := range []struct {
		name string
		in   float32
		want int16
	}{
		{"silence", 0.0, 0},
		{"positive full scale", 1.0, 32767},
		{"negative full scale", -1.0, -32767},
		{"half scale rounds", 0.5, 16384},
		{"negative half scale rounds", -0.5, -16384},
		{"over range clamps high", 1.4, 32767},
		{"over range clamps low", -1.4, -32768},
		{"far over range still clamps", 100.0, 32767},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := c.convert(f32bytes(tc.in), 1)
			if got[0] != tc.want {
				t.Errorf("convert(%v) = %d, want %d", tc.in, got[0], tc.want)
			}
		})
	}
}

// A NaN cannot be converted to an integer in Go; it must not become noise.
func TestPCMConvertFloat32NaN(t *testing.T) {
	c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesFloat32}, 2)
	got, peak := c.convert(f32bytes(float32(math.NaN())), 1)
	if got[0] != 0 {
		t.Errorf("NaN converted to %d, want 0", got[0])
	}
	if peak != 0 {
		t.Errorf("NaN produced peak %d, want 0", peak)
	}
}

func TestPCMConvertVariants(t *testing.T) {
	t.Run("16-bit passes through", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesInt16}, 2)
		got, peak := c.convert(i16bytes(0, 12345, -12345, -32768), 4)
		want := []int16{0, 12345, -12345, -32768}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
			}
		}
		if peak != 32768 {
			t.Errorf("peak = %d, want 32768", peak)
		}
	})

	t.Run("32-bit takes the high half", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesInt32}, 2)
		// 0x40000000 >> 16 == 0x4000, and -1 >> 16 stays -1.
		got, _ := c.convert(i32bytes(0, 0x40000000, -1, int32(-0x40000000)), 4)
		want := []int16{0, 0x4000, -1, -0x4000}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
			}
		}
	})

	t.Run("mono stays mono", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesInt16}, 2)
		got, _ := c.convert(i16bytes(7, 8), 2)
		if len(got) != 2 {
			t.Fatalf("got %d samples for 2 mono frames, want 2", len(got))
		}
	})

	t.Run("stereo is interleaved", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 2, Samples: samplesInt16}, 2)
		got, _ := c.convert(i16bytes(1, 2, 3, 4), 2)
		want := []int16{1, 2, 3, 4}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
			}
		}
	})

	t.Run("surround keeps the front pair", func(t *testing.T) {
		// Six channels per frame: FL FR FC LFE BL BR.
		in := i16bytes(
			10, 20, 30, 40, 50, 60,
			11, 21, 31, 41, 51, 61,
		)
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 6, Samples: samplesInt16}, 2)
		got, _ := c.convert(in, 2)
		want := []int16{10, 20, 11, 21}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
			}
		}
	})

	t.Run("a silent buffer reports zero peak", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 2, Samples: samplesFloat32}, 2)
		_, peak := c.convert(f32bytes(0, 0, 0, 0), 2)
		if peak != 0 {
			t.Errorf("peak = %d, want 0", peak)
		}
	})

	t.Run("shared scratch survives a larger buffer", func(t *testing.T) {
		c := newPCMConverter(deviceFormat{SampleRate: 48000, Channels: 1, Samples: samplesInt16}, 2)
		c.convert(i16bytes(1, 2, 3), 3)
		got, _ := c.convert(i16bytes(9, 8, 7, 6, 5), 5)
		if len(got) != 5 || got[0] != 9 || got[4] != 5 {
			t.Fatalf("scratch reuse corrupted the buffer: %v", got)
		}
	})
}

func TestClassifyFormat(t *testing.T) {
	const sr, ch = 48000, 2

	for _, tc := range []struct {
		name    string
		tag     uint16
		cbSize  uint16
		sub     [16]byte
		bits    int
		want    sampleFormat
		wantErr bool
	}{
		{name: "bare PCM 16", tag: waveFormatTagPCM, bits: 16, want: samplesInt16},
		{name: "bare PCM 32", tag: waveFormatTagPCM, bits: 32, want: samplesInt32},
		{name: "bare float", tag: waveFormatTagIEEEFloat, bits: 32, want: samplesFloat32},
		{
			name: "extensible float", tag: waveFormatTagExtensible, cbSize: 22,
			sub: subFormatIEEEFloat, bits: 32, want: samplesFloat32,
		},
		{
			name: "extensible PCM 16", tag: waveFormatTagExtensible, cbSize: 22,
			sub: subFormatPCM, bits: 16, want: samplesInt16,
		},
		{
			name: "extensible PCM 32", tag: waveFormatTagExtensible, cbSize: 22,
			sub: subFormatPCM, bits: 32, want: samplesInt32,
		},
		{
			// cbSize is what says the extension is there at all. Reading the
			// union below it is reading past the buffer.
			name: "extensible without the extension", tag: waveFormatTagExtensible, cbSize: 0,
			sub: subFormatIEEEFloat, bits: 32, wantErr: true,
		},
		{
			name: "extensible with a short extension", tag: waveFormatTagExtensible, cbSize: 18,
			sub: subFormatIEEEFloat, bits: 32, wantErr: true,
		},
		{
			// Only Data1 matching must not be enough: this GUID agrees on the
			// first four bytes with the float one and differs in the last.
			name: "near-miss GUID", tag: waveFormatTagExtensible, cbSize: 22,
			sub: [16]byte{
				0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
				0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x72,
			},
			bits: 32, wantErr: true,
		},
		{name: "24-bit packed PCM", tag: waveFormatTagPCM, bits: 24, wantErr: true},
		{name: "float at 16 bits", tag: waveFormatTagIEEEFloat, bits: 16, wantErr: true},
		{name: "ADPCM", tag: 0x0002, bits: 4, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyFormat(tc.tag, tc.cbSize, tc.sub, tc.bits, ch, sr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("classified as %v, want an error", got.Samples)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Samples != tc.want {
				t.Errorf("Samples = %v, want %v", got.Samples, tc.want)
			}
			if got.SampleRate != sr || got.Channels != ch {
				t.Errorf("got %d Hz / %d ch, want %d Hz / %d ch", got.SampleRate, got.Channels, sr, ch)
			}
		})
	}
}

func TestClassifyFormatRejectsNoChannels(t *testing.T) {
	if _, err := classifyFormat(waveFormatTagPCM, 0, [16]byte{}, 16, 0, 48000); err == nil {
		t.Error("a zero-channel format was accepted")
	}
}

// The wire format's size is the whole basis of the bandwidth arithmetic.
func TestAudioStreamFormatSize(t *testing.T) {
	f := audioStreamFormat{SampleRate: 48000, Channels: 2}

	if got := f.packetFrames(); got != 960 {
		t.Errorf("packetFrames = %d, want 960 (20 ms at 48 kHz)", got)
	}
	if got := f.packetBytes(); got != 3840 {
		t.Errorf("packetBytes = %d, want 3840", got)
	}
	if got := f.nominalBps(); got != 1536000 {
		t.Errorf("nominalBps = %d, want 1536000", got)
	}
}

// Device buffers do not align with the packet size, so the split has to carry a
// remainder rather than drop it -- a dropped remainder is an audible gap.
func TestRepacketizer(t *testing.T) {
	f := audioStreamFormat{SampleRate: 48000, Channels: 2}
	r := newRepacketizer(f)

	const per = 1920 // samples per packet
	if r.perPacket != per {
		t.Fatalf("perPacket = %d, want %d", r.perPacket, per)
	}

	// Chunk sizes chosen to be coprime with the packet size.
	sizes := []int{1, 7, 1000, 13, 1921, 959}
	total := 0
	var packets [][]int16
	for _, n := range sizes {
		chunk := make([]int16, n)
		for i := range chunk {
			chunk[i] = int16(total + i)
		}
		total += n
		packets = append(packets, r.push(chunk)...)
	}

	emitted := 0
	for _, p := range packets {
		if len(p) != per {
			t.Fatalf("packet of %d samples, want %d", len(p), per)
		}
		// Order must be preserved across the packet boundary.
		for _, s := range p {
			if int(s) != emitted {
				t.Fatalf("sample %d carries %d, want %d", emitted, s, emitted)
			}
			emitted++
		}
	}

	if got := emitted + r.pending(); got != total {
		t.Errorf("fed %d samples, accounted for %d", total, got)
	}
	if want := total / per; len(packets) != want {
		t.Errorf("emitted %d packets, want %d", len(packets), want)
	}
}

func TestRepacketizerExactMultiple(t *testing.T) {
	r := newRepacketizer(audioStreamFormat{SampleRate: 48000, Channels: 1})
	packets := r.push(make([]int16, 960*3))
	if len(packets) != 3 {
		t.Fatalf("got %d packets, want 3", len(packets))
	}
	if r.pending() != 0 {
		t.Errorf("pending = %d after an exact multiple, want 0", r.pending())
	}
}

func TestRepacketizerResetDropsRemainder(t *testing.T) {
	r := newRepacketizer(audioStreamFormat{SampleRate: 48000, Channels: 1})
	r.push(make([]int16, 100))
	if r.pending() != 100 {
		t.Fatalf("pending = %d, want 100", r.pending())
	}
	r.reset()
	if r.pending() != 0 {
		t.Errorf("pending = %d after reset, want 0", r.pending())
	}
}

func TestSilenceWatchdog(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	w := newSilenceWatchdog(200*time.Millisecond, t0)

	// Quiet from the outset: idle only once the hold has elapsed.
	if w.observe(t0.Add(100*time.Millisecond), false) {
		t.Error("went idle before the hold elapsed")
	}
	if !w.observe(t0.Add(200*time.Millisecond), false) {
		t.Error("still sending after the hold elapsed with no data")
	}

	// Sound resumes the stream at once rather than after another hold.
	if w.observe(t0.Add(210*time.Millisecond), true) {
		t.Error("a sound packet did not resume the stream")
	}

	// The clock restarts from the last sound, not from the last packet.
	if w.observe(t0.Add(350*time.Millisecond), false) {
		t.Error("idle too early after resuming; the hold restarted from the wrong instant")
	}
	if !w.observe(t0.Add(410*time.Millisecond), false) {
		t.Error("did not go idle a full hold after the last sound")
	}
}

// A single loud packet must not be treated as continuous sound.
func TestSilenceWatchdogOnePacketIsNotContinuous(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	w := newSilenceWatchdog(200*time.Millisecond, t0)

	w.observe(t0, true)
	if !w.observe(t0.Add(200*time.Millisecond), false) {
		t.Error("a lone packet kept the stream alive past the hold")
	}
}

// Audio counts against the total egress ceiling, so what is left for video is
// the ceiling minus the fixed audio cost.
func TestVideoBudgetSubtractsAudio(t *testing.T) {
	audio := audioStreamFormat{SampleRate: 48000, Channels: 2}.nominalBps()

	for _, tc := range []struct {
		name       string
		maxBitrate int64
		audioBps   int64
		want       int64
	}{
		{"no ceiling stays unlimited", 0, audio, 0},
		{"no audio leaves the ceiling alone", 2_000_000, 0, 2_000_000},
		{"audio comes out of the ceiling", 2_000_000, audio, 464_000},
		{"a generous ceiling barely notices", 20_000_000, audio, 18_464_000},
		{
			// Zero would read as "no cap" in observe, silently switching off
			// the limit that audio alone just exceeded.
			name: "audio alone blows the ceiling", maxBitrate: 1_000_000, audioBps: audio, want: 1,
		},
		{"exactly consumed leaves the floor", 1_536_000, audio, 1},
		{"negative ceiling is unlimited", -1, audio, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := videoBudget(tc.maxBitrate, tc.audioBps); got != tc.want {
				t.Errorf("videoBudget(%d, %d) = %d, want %d", tc.maxBitrate, tc.audioBps, got, tc.want)
			}
		})
	}
}
