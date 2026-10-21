//go:build windows

package server

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// The flat declaration of WAVEFORMATEXTENSIBLE exists so that these offsets
// hold. The compile-time assertions in audio_windows.go already pin the size;
// this states each field position in a form that names what went wrong.
func TestWaveFormatExtensibleLayout(t *testing.T) {
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"FormatTag", unsafe.Offsetof(waveFormatExtensible{}.FormatTag), 0},
		{"Channels", unsafe.Offsetof(waveFormatExtensible{}.Channels), 2},
		{"SamplesPerSec", unsafe.Offsetof(waveFormatExtensible{}.SamplesPerSec), 4},
		{"AvgBytesPerSec", unsafe.Offsetof(waveFormatExtensible{}.AvgBytesPerSec), 8},
		{"BlockAlign", unsafe.Offsetof(waveFormatExtensible{}.BlockAlign), 12},
		{"BitsPerSample", unsafe.Offsetof(waveFormatExtensible{}.BitsPerSample), 14},
		{"CBSize", unsafe.Offsetof(waveFormatExtensible{}.CBSize), 16},
		{"ValidBits", unsafe.Offsetof(waveFormatExtensible{}.ValidBits), 18},
		{"ChannelMask", unsafe.Offsetof(waveFormatExtensible{}.ChannelMask), 20},
		{"SubFormat", unsafe.Offsetof(waveFormatExtensible{}.SubFormat), 24},
	} {
		if c.got != c.want {
			t.Errorf("%s is at offset %d, want %d", c.name, c.got, c.want)
		}
	}

	// Nesting WAVEFORMATEX instead of declaring flat would round the header
	// from 18 to 20 bytes and push everything after it four bytes along, which
	// is a silent misread rather than a crash.
	if got := unsafe.Sizeof(waveFormatExtensible{}); got != waveFormatExtensibleSize {
		t.Errorf("WAVEFORMATEXTENSIBLE is %d bytes, want %d", got, waveFormatExtensibleSize)
	}
	if got := unsafe.Offsetof(waveFormatExtensible{}.ValidBits); got != waveFormatExSize {
		t.Errorf("the extension starts at %d, so WAVEFORMATEX spans %d bytes, want %d",
			got, got, waveFormatExSize)
	}
}

// A GUID must be exactly 16 bytes with the three leading fields little-endian
// and Data4 in order. Getting this wrong is what makes CoCreateInstance return
// REGDB_E_CLASSNOTREG, so the byte image is checked rather than assumed.
func TestGUIDLayout(t *testing.T) {
	if got := unsafe.Sizeof(guid{}); got != 16 {
		t.Fatalf("guid is %d bytes, want 16", got)
	}

	// CLSID_MMDeviceEnumerator {BCDE0395-E52F-467C-8E3D-C4579291692E}, written
	// out the way it must appear in memory.
	want := [16]byte{
		0x95, 0x03, 0xDE, 0xBC, // Data1, little-endian
		0x2F, 0xE5, // Data2, little-endian
		0x7C, 0x46, // Data3, little-endian
		0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E, // Data4, as written
	}
	got := *(*[16]byte)(unsafe.Pointer(&clsidMMDeviceEnumerator))
	if got != want {
		t.Errorf("CLSID_MMDeviceEnumerator is %x, want %x", got, want)
	}
}

// hrFailed must truncate to 32 bits first. The x64 ABI does not promise the
// upper half of a 32-bit return is zeroed, and AUDCLNT_S_BUFFER_EMPTY is a
// *success* code that an idle endpoint returns constantly -- treating it as
// fatal would kill audio the first time nothing was playing.
func TestHRFailed(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  uintptr
		want bool
	}{
		{"S_OK", sOK, false},
		{"S_FALSE", sFalse, false},
		{"AUDCLNT_S_BUFFER_EMPTY", audclntSBufferEmpty, false},
		{
			"a success code with stale bits above bit 31",
			0xFFFFFFFF08890001, false,
		},
		{"AUDCLNT_E_DEVICE_INVALIDATED", audclntEDeviceInvalidated, true},
		{"E_FAIL", 0x80004005, true},
		{"a failure with stale bits above bit 31", 0xDEADBEEF80004005, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := hrFailed(c.raw); got != c.want {
				t.Errorf("hrFailed(0x%X) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// Only a device invalidation is worth rebuilding the stream for, and the caller
// has to be able to tell it apart from every other failure.
func TestDeviceInvalidatedIsMatchable(t *testing.T) {
	if !isDeviceInvalidated(hrError("IAudioClient::Start", audclntEDeviceInvalidated)) {
		t.Error("AUDCLNT_E_DEVICE_INVALIDATED was not recognised")
	}
	for _, hr := range []uintptr{audclntEUnsupportedFormat, 0x80004005, audclntSBufferEmpty} {
		if isDeviceInvalidated(hrError("op", hr)) {
			t.Errorf("0x%08X was mistaken for a device invalidation", uint32(hr))
		}
	}
	if isDeviceInvalidated(nil) {
		t.Error("a nil error was treated as a device invalidation")
	}
}

// buildExtensible lays out a WAVEFORMATEXTENSIBLE the way the mixer would.
func buildExtensible(tag uint16, channels uint16, rate uint32, bits uint16, sub [16]byte) []byte {
	b := make([]byte, waveFormatExtensibleSize)
	binary.LittleEndian.PutUint16(b[0:], tag)
	binary.LittleEndian.PutUint16(b[2:], channels)
	binary.LittleEndian.PutUint32(b[4:], rate)
	binary.LittleEndian.PutUint16(b[14:], bits)
	binary.LittleEndian.PutUint16(b[16:], waveFormatExtensibleMinSize)
	copy(b[subFormatOffset:], sub[:])
	return b
}

func TestParseMixFormatExtensible(t *testing.T) {
	b := buildExtensible(waveFormatTagExtensible, 2, 48000, 32, subFormatIEEEFloat)

	dev, err := parseMixFormat(unsafe.Pointer(&b[0]))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dev.Samples != samplesFloat32 {
		t.Errorf("Samples = %v, want float32", dev.Samples)
	}
	if dev.SampleRate != 48000 || dev.Channels != 2 {
		t.Errorf("got %d Hz / %d ch, want 48000 Hz / 2 ch", dev.SampleRate, dev.Channels)
	}
	if dev.Samples.bytes() != 4 {
		t.Errorf("sample width = %d, want 4", dev.Samples.bytes())
	}
}

// A bare WAVEFORMATEX carries no extension, so parseMixFormat must not treat
// the bytes after cbSize as one.
func TestParseMixFormatShortHeader(t *testing.T) {
	b := make([]byte, waveFormatExSize)
	binary.LittleEndian.PutUint16(b[0:], waveFormatTagPCM)
	binary.LittleEndian.PutUint16(b[2:], 2)
	binary.LittleEndian.PutUint32(b[4:], 44100)
	binary.LittleEndian.PutUint16(b[14:], 16)
	binary.LittleEndian.PutUint16(b[16:], 0) // cbSize: nothing follows

	dev, err := parseMixFormat(unsafe.Pointer(&b[0]))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dev.Samples != samplesInt16 {
		t.Errorf("Samples = %v, want int16", dev.Samples)
	}
	if dev.SampleRate != 44100 {
		t.Errorf("SampleRate = %d, want 44100", dev.SampleRate)
	}
}

// An extensible format that nonetheless claims no extension is malformed and
// must be refused rather than classified from whatever follows it.
func TestParseMixFormatRejectsTruncatedExtension(t *testing.T) {
	b := buildExtensible(waveFormatTagExtensible, 2, 48000, 32, subFormatIEEEFloat)
	binary.LittleEndian.PutUint16(b[16:], 0)

	if _, err := parseMixFormat(unsafe.Pointer(&b[0])); err == nil {
		t.Error("an extensible format with cbSize 0 was accepted")
	}
}
