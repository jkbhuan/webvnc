package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// This file is the platform-independent half of audio support: format
// conversion, packetisation and the silence decision. Keeping it free of any
// build tag is deliberate -- it is the only part of the audio path that can be
// exercised without Windows and without a sound card.
//
// The wire format is deliberately uncompressed 16-bit PCM. The project builds
// with CGO_ENABLED=0 and takes no third-party dependency beyond gorilla, and no
// pure-Go Opus encoder exists; sending Opus to a browser would additionally
// require a WebM muxer for MediaSource. The cost is a constant 1536 kbps for
// 48 kHz stereo, which is exactly why the video rate controller reserves room
// for it (see videoBudget).

const (
	// audioPacketMs is the duration carried by one binary message. Fixed-size
	// packets make the wire format, the bandwidth arithmetic and the client's
	// playback scheduling all predictable.
	audioPacketMs = 20

	// audioBitsPerSample is fixed by the wire format. The capture device may
	// use wider samples; they are converted down.
	audioBitsPerSample = 16
)

// silencePeak is the largest sample magnitude still counted as silence. One LSB
// of slack stops dither on a digitally-silent stream from reading as sound.
const silencePeak = 1

// silenceHold is how long the stream must be quiet before sending stops. Short
// enough that an idle desktop costs nothing, long enough not to gap on the
// silence between two tracks.
const silenceHold = 200 * time.Millisecond

// ErrUnsupportedAudio is returned on platforms without an audio backend.
var ErrUnsupportedAudio = errors.New("audio capture is not supported on this platform")

// sampleFormat is how the capture device represents one sample.
type sampleFormat int

const (
	samplesInt16 sampleFormat = iota
	samplesInt32
	samplesFloat32
)

func (s sampleFormat) String() string {
	switch s {
	case samplesInt16:
		return "int16"
	case samplesInt32:
		return "int32"
	case samplesFloat32:
		return "float32"
	}
	return "unknown"
}

// bytes is the width of one sample as the device stores it, which is what turns
// a frame count from the capture API into a buffer length.
func (s sampleFormat) bytes() int {
	switch s {
	case samplesInt16:
		return 2
	case samplesInt32, samplesFloat32:
		return 4
	}
	return 0
}

// deviceFormat describes what the endpoint's mix format says it produces.
//
// A valid-bit count is deliberately not carried: WAVE_FORMAT_EXTENSIBLE stores
// samples MSB-aligned in their container, so shifting the container down to 16
// always keeps the meaningful high bits whatever wValidBitsPerSample says. That
// makes the field unnecessary rather than merely unused.
type deviceFormat struct {
	SampleRate int
	Channels   int
	Samples    sampleFormat
}

// Format tags and sub-format GUIDs that a mix format can carry.
const (
	waveFormatTagPCM        = 0x0001
	waveFormatTagIEEEFloat  = 0x0003
	waveFormatTagExtensible = 0xFFFE

	// waveFormatExtensibleMinSize is the smallest cbSize that leaves room for
	// the extension: 22 bytes of wValidBitsPerSample, dwChannelMask and
	// SubFormat following the 18-byte WAVEFORMATEX. Below it the union fields
	// are not there to read.
	waveFormatExtensibleMinSize = 22
)

// The two SubFormat GUIDs worth distinguishing. The whole 16 bytes are
// compared: matching only Data1 (1 for PCM, 3 for float) is a widely copied
// shortcut that also matches every other GUID sharing those four bytes.
var (
	subFormatPCM = [16]byte{
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
		0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
	}
	subFormatIEEEFloat = [16]byte{
		0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
		0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
	}
)

// classifyFormat turns the raw fields of a mix format into a deviceFormat.
//
// It is separated from the struct that carries them so the classification --
// the part that silently produces noise when it is wrong -- is testable without
// Windows. subFormat is only looked at for WAVE_FORMAT_EXTENSIBLE, and only
// once cbSize says the extension is actually present.
//
// 24-bit packed PCM is rejected rather than approximated: its samples are three
// bytes wide and not MSB-aligned the way the extensible container is, so
// treating it as 32-bit would shift every sample.
func classifyFormat(formatTag, cbSize uint16, subFormat [16]byte, bitsPerSample, channels, sampleRate int) (deviceFormat, error) {
	dev := deviceFormat{SampleRate: sampleRate, Channels: channels}

	switch formatTag {
	case waveFormatTagExtensible:
		if cbSize < waveFormatExtensibleMinSize {
			return dev, fmt.Errorf("WAVE_FORMAT_EXTENSIBLE with cbSize %d < %d: extension fields absent", cbSize, waveFormatExtensibleMinSize)
		}
		switch {
		case subFormat == subFormatIEEEFloat && bitsPerSample == 32:
			dev.Samples = samplesFloat32
		case subFormat == subFormatPCM:
			s, err := pcmSampleFormat(bitsPerSample)
			if err != nil {
				return dev, err
			}
			dev.Samples = s
		default:
			return dev, fmt.Errorf("unsupported WAVE_FORMAT_EXTENSIBLE sub-format %x at %d bits", subFormat, bitsPerSample)
		}
	case waveFormatTagIEEEFloat:
		if bitsPerSample != 32 {
			return dev, fmt.Errorf("WAVE_FORMAT_IEEE_FLOAT at %d bits, want 32", bitsPerSample)
		}
		dev.Samples = samplesFloat32
	case waveFormatTagPCM:
		s, err := pcmSampleFormat(bitsPerSample)
		if err != nil {
			return dev, err
		}
		dev.Samples = s
	default:
		return dev, fmt.Errorf("unsupported format tag 0x%04x", formatTag)
	}

	if dev.Channels < 1 {
		return dev, fmt.Errorf("format reports %d channels", dev.Channels)
	}
	return dev, nil
}

// pcmSampleFormat maps a container width onto the sample layouts sample() can
// read.
func pcmSampleFormat(bits int) (sampleFormat, error) {
	switch bits {
	case 16:
		return samplesInt16, nil
	case 32:
		// Covers 24-bit-in-32, which the extensible container stores
		// MSB-aligned and which the arithmetic right shift therefore handles.
		return samplesInt32, nil
	}
	return 0, fmt.Errorf("unsupported PCM container of %d bits", bits)
}

// audioStreamFormat is what the client is told and what the wire carries:
// always 16-bit PCM, possibly downmixed from the device's own channel count.
type audioStreamFormat struct {
	SampleRate int
	Channels   int
}

// nominalBps is the wire cost of this format in bits per second. PCM is
// constant-rate, so this is exact rather than an estimate -- which is what lets
// the video rate controller subtract it without any feedback loop.
func (f audioStreamFormat) nominalBps() int64 {
	return int64(f.SampleRate) * int64(f.Channels) * audioBitsPerSample
}

// packetFrames is how many frames make up one packet.
func (f audioStreamFormat) packetFrames() int {
	return f.SampleRate * audioPacketMs / 1000
}

// packetBytes is the size of one binary message on the wire.
func (f audioStreamFormat) packetBytes() int {
	return f.packetFrames() * f.Channels * audioBitsPerSample / 8
}

// AudioCapturer reads the system's audio output.
type AudioCapturer interface {
	// Format is the wire format Read produces.
	Format() audioStreamFormat
	// Read returns the next chunk of interleaved 16-bit PCM in the wire format.
	// It blocks briefly rather than returning instantly, so the caller's loop
	// is paced by the device. ok is false when the device produced nothing,
	// which is how an idle endpoint behaves: WASAPI loopback emits no packets
	// at all while nothing is playing.
	//
	// The returned slice is reused on the next call.
	Read() (pcm []int16, ok bool, err error)
	Close()
}

// pcmConverter converts the device's mix format to interleaved 16-bit PCM.
type pcmConverter struct {
	src     sampleFormat
	srcCh   int
	outCh   int
	scratch []int16
}

// newPCMConverter builds a converter targeting at most two channels, and never
// more than the device has. A mono device stays mono rather than being upmixed:
// that is both cheaper on the wire and what the device actually produced. The
// bound against the source matters for correctness, not just size -- convert
// reads channel ch of a frame directly, so an output count above the source
// count would read the next frame's samples as this one's right channel.
func newPCMConverter(dev deviceFormat, outChannels int) *pcmConverter {
	out := max(1, min(outChannels, dev.Channels, 2))
	return &pcmConverter{
		src:     dev.Samples,
		srcCh:   dev.Channels,
		outCh:   out,
		scratch: make([]int16, 0, 4096),
	}
}

// convert reads frames frames from src and returns interleaved 16-bit PCM along
// with the largest sample magnitude seen, which is what the silence watchdog
// keys on. The returned slice is reused on the next call.
//
// The peak is measured on the *output* samples: it is the converted stream that
// goes on the wire, so it is the converted stream whose silence is being
// detected.
func (c *pcmConverter) convert(src []byte, frames int) ([]int16, int32) {
	need := frames * c.outCh
	if cap(c.scratch) < need {
		c.scratch = make([]int16, need)
	}
	out := c.scratch[:need]

	// Fewer output channels than the device has means keeping the front pair.
	// A proper surround downmix needs the channel mask and per-speaker weights;
	// this is predictable, allocation-free, and loses only surround content on
	// the rare 6/8-channel device.
	var peak int32
	for i := 0; i < frames; i++ {
		base := i * c.srcCh
		for ch := 0; ch < c.outCh; ch++ {
			v := clampToInt16(c.sample(src, base+ch))
			out[i*c.outCh+ch] = v
			peak = max(peak, absInt32(int32(v)))
		}
	}
	return out, peak
}

// sample reads one sample and normalises it to the int16 range. The result must
// still be clamped: a float sample can sit outside [-1, 1].
func (c *pcmConverter) sample(src []byte, i int) int32 {
	switch c.src {
	case samplesInt16:
		return int32(int16(binary.LittleEndian.Uint16(src[i*2:])))
	case samplesInt32:
		// MSB-aligned storage means the top 16 bits are the signal regardless
		// of the container width, so this is exact for 24-in-32 as well.
		return int32(binary.LittleEndian.Uint32(src[i*4:])) >> 16
	case samplesFloat32:
		f := math.Float32frombits(binary.LittleEndian.Uint32(src[i*4:]))
		if math.IsNaN(float64(f)) {
			// Converting a NaN to an integer is undefined in Go, and a stuck
			// driver should not turn into arbitrary noise.
			return 0
		}
		return int32(math.Round(float64(f) * 32767))
	}
	return 0
}

// clampToInt16 keeps a converted value inside the 16-bit range. Wrapping here
// would turn an over-range sample into a loud click rather than distortion.
func clampToInt16(v int32) int16 {
	if v > math.MaxInt16 {
		return math.MaxInt16
	}
	if v < math.MinInt16 {
		return math.MinInt16
	}
	return int16(v)
}

func absInt32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// peakOf returns the largest sample magnitude in a buffer, which is what the
// silence watchdog treats as sound. It is measured on what was actually read
// rather than per packet, so a read that spans the moment sound returns is not
// missed.
func peakOf(samples []int16) int32 {
	var peak int32
	for _, s := range samples {
		peak = max(peak, absInt32(int32(s)))
	}
	return peak
}

// repacketizer accumulates interleaved samples and emits fixed-size packets, so
// the wire format does not inherit the capture device's variable buffer sizes.
type repacketizer struct {
	perPacket int // samples per packet, across all channels
	buf       []int16
	n         int
}

func newRepacketizer(f audioStreamFormat) *repacketizer {
	per := f.packetFrames() * f.Channels
	return &repacketizer{perPacket: per, buf: make([]int16, per)}
}

// push appends samples and returns every packet that completed. A partial
// remainder is carried over to the next call, so no samples are lost when the
// device hands back a chunk that does not align with the packet size.
func (r *repacketizer) push(samples []int16) [][]int16 {
	var packets [][]int16
	for len(samples) > 0 {
		n := copy(r.buf[r.n:], samples)
		r.n += n
		samples = samples[n:]
		if r.n == r.perPacket {
			pkt := make([]int16, r.perPacket)
			copy(pkt, r.buf)
			packets = append(packets, pkt)
			r.n = 0
		}
	}
	return packets
}

// pending reports how many samples are buffered but not yet emitted.
func (r *repacketizer) pending() int { return r.n }

// reset drops any partial packet. It is used when the device is reinitialised,
// where the buffered remainder belongs to a stream that no longer exists.
func (r *repacketizer) reset() { r.n = 0 }

// silenceWatchdog decides when an idle stream should stop being sent.
//
// It keys on the capture device going quiet rather than on inspecting sample
// values, because WASAPI loopback produces *no packets at all* while the
// endpoint is idle -- that absence is a more reliable signal than any amplitude
// threshold. A whole-packet peak check backs it up, for an application that
// holds a stream open while emitting digital silence.
type silenceWatchdog struct {
	hold      time.Duration
	lastSound time.Time
}

// newSilenceWatchdog starts the clock at now, so a stream that is quiet from
// the outset goes idle after one hold period rather than waiting for a sound
// that may never come.
func newSilenceWatchdog(hold time.Duration, now time.Time) *silenceWatchdog {
	return &silenceWatchdog{hold: hold, lastSound: now}
}

// observe records whether this moment carried sound and reports whether the
// stream should now be considered silent. Sound resumes the stream immediately;
// silence only takes effect once the hold has elapsed.
func (w *silenceWatchdog) observe(now time.Time, sound bool) bool {
	if sound {
		w.lastSound = now
		return false
	}
	return now.Sub(w.lastSound) >= w.hold
}

// encodePCM16 serialises interleaved samples for the wire.
func encodePCM16(samples []int16, dst []byte) []byte {
	need := len(samples) * 2
	if cap(dst) < need {
		dst = make([]byte, need)
	}
	dst = dst[:need]
	for i, s := range samples {
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(s))
	}
	return dst
}
