//go:build windows

package server

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// WASAPI loopback capture.
//
// This is the only place in the project that talks COM, and it does so with a
// hand-rolled minimum: every interface used here is a bare pointer, and calls
// go through the vtable directly. A COM binding library is not an option --
// the project builds with CGO_ENABLED=0 and depends on nothing but gorilla.
//
// The endpoint is captured in shared mode with AUDCLNT_STREAMFLAGS_LOOPBACK,
// which delivers whatever the selected render endpoint is playing, sampled
// after the mixer. The device's own mix format is converted to the wire format
// by the portable layer in audio.go.

// ---------------------------------------------------------------------------
// GUIDs
// ---------------------------------------------------------------------------

// guid mirrors the 16-byte in-memory layout of a Windows GUID.
//
// It is written as the documented fields rather than as raw bytes so each
// constant below can be checked against the SDK text at a glance. On Windows
// the three leading fields are little-endian and Data4 is stored as given,
// which is exactly this layout -- so no byte swapping appears anywhere, and in
// particular none should ever be added: a swapped GUID is what makes
// CoCreateInstance return REGDB_E_CLASSNOTREG.
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	// CLSID_MMDeviceEnumerator {BCDE0395-E52F-467C-8E3D-C4579291692E}
	clsidMMDeviceEnumerator = guid{
		0xBCDE0395, 0xE52F, 0x467C,
		[8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E},
	}
	// IID_IMMDeviceEnumerator {A95664D2-9614-4F35-A746-DE8DB63617E6}
	iidMMDeviceEnumerator = guid{
		0xA95664D2, 0x9614, 0x4F35,
		[8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6},
	}
	// IID_IAudioClient {1CB9AD4C-DBFA-4C32-B178-C2F568A703B2}
	iidAudioClient = guid{
		0x1CB9AD4C, 0xDBFA, 0x4C32,
		[8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2},
	}
	// IID_IAudioCaptureClient {C8ADBD64-E71E-48A0-A4DE-185C395CD317}
	iidAudioCaptureClient = guid{
		0xC8ADBD64, 0xE71E, 0x48A0,
		[8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17},
	}
)

// The layout the raw pointers above depend on. A GUID is exactly 16 bytes with
// no padding, and every interface pointer is a single pointer to a vtable.
var (
	_ [16 - unsafe.Sizeof(guid{})]byte
	_ [unsafe.Sizeof(guid{}) - 16]byte
	_ [unsafe.Sizeof(unsafe.Pointer(nil)) - unsafe.Sizeof(uintptr(0))]byte
)

// ---------------------------------------------------------------------------
// COM plumbing
// ---------------------------------------------------------------------------

var (
	ole32 = syscall.NewLazyDLL("ole32.dll")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

// COINIT_MULTITHREADED. The apartment must be multithreaded: a single-threaded
// apartment needs a message pump, and a goroutine that blocks in a sleep never
// pumps one, so calls would simply hang.
const coinitMultithreaded = 0x0

// CLSCTX_ALL.
const clctxAll = 0x17

// Vtable slots. Every interface here derives from IUnknown, whose three methods
// (QueryInterface, AddRef, Release) occupy slots 0-2, so each interface's own
// methods start at 3 in declaration order.
const (
	vtblQueryInterface = 0
	vtblAddRef         = 1
	vtblRelease        = 2

	// IMMDeviceEnumerator
	vtblGetDefaultAudioEndpoint = 4

	// IMMDevice
	vtblActivate = 3

	// IAudioClient
	vtblInitialize      = 3
	vtblGetMixFormat    = 8
	vtblGetDevicePeriod = 9
	vtblStart           = 10
	vtblStop            = 11
	vtblGetService      = 14

	// IAudioCaptureClient
	vtblGetBuffer         = 3
	vtblReleaseBuffer     = 4
	vtblGetNextPacketSize = 5
)

// EDataFlow / ERole. eConsole is the endpoint the user actually hears.
const (
	eRender  = 0
	eConsole = 0
)

// Stream flags and formats.
const (
	audclntShareModeShared      = 0
	audclntStreamFlagsLoopback  = 0x00020000
	audclntStreamFlagsNoPersist = 0x00080000

	audclntBufferFlagsSilent = 0x00000002
)

// HRESULTs that this code has to tell apart.
const (
	sOK                          = 0x00000000
	sFalse                       = 0x00000001
	audclntSBufferEmpty          = 0x08890001
	audclntEAlreadyInitialized   = 0x88890002
	audclntEDeviceInvalidated    = 0x88890004
	audclntEUnsupportedFormat    = 0x88890008
	audclntEBufferSizeNotAligned = 0x88890019
	audclntEInvalidStreamFlag    = 0x88890021
	rpcEChangedMode              = 0x80010106
	coENotInitialized            = 0x800401F0
)

// comCall invokes vtable slot idx on a COM interface pointer and returns the
// raw HRESULT.
//
// Keep this a direct call to syscall.SyscallN. The guarantee that arguments
// converted to uintptr keep their pointees alive comes from an annotation the
// compiler only honours on a direct call; routing through a func value or an
// interface would let the collector free an argument while the callee still
// holds the pointer.
func comCall(this unsafe.Pointer, idx uintptr, args ...uintptr) uintptr {
	// this points at the object, whose first field is the vtable pointer. The
	// walk is kept in typed pointers rather than round-tripping through
	// uintptr: converting an integer back into a pointer is the thing vet
	// rightly distrusts, and here it would also hide a genuine mistake.
	vtbl := *(**uintptr)(this)
	slot := (*uintptr)(unsafe.Add(unsafe.Pointer(vtbl), idx*unsafe.Sizeof(uintptr(0))))

	r, _, _ := syscall.SyscallN(*slot, append([]uintptr{uintptr(this)}, args...)...)
	return r
}

// comRelease drops one reference and clears the caller's pointer. Release is
// slot 2 on every interface, so one helper covers all of them.
func comRelease(p *unsafe.Pointer) {
	if *p == nil {
		return
	}
	comCall(*p, vtblRelease)
	*p = nil
}

// hrFailed reports whether a raw call result is a failure.
//
// The call returns the raw RAX. The x64 ABI does not promise that the upper
// half of a 32-bit return value is zeroed, so it must be truncated before being
// read as signed. Skipping that turns AUDCLNT_S_BUFFER_EMPTY -- a *success*
// code -- into a huge unsigned number, and a genuine failure into whatever the
// stale upper bits happen to spell.
func hrFailed(r uintptr) bool { return int32(uint32(r)) < 0 }

// hresultText names the codes this file interprets. Anything else is reported
// as hex, which is the form the Windows error lookup expects anyway.
func hresultText(hr uintptr) string {
	switch uint32(hr) {
	case audclntSBufferEmpty:
		return "AUDCLNT_S_BUFFER_EMPTY"
	case audclntEAlreadyInitialized:
		return "AUDCLNT_E_ALREADY_INITIALIZED"
	case audclntEDeviceInvalidated:
		return "AUDCLNT_E_DEVICE_INVALIDATED"
	case audclntEUnsupportedFormat:
		return "AUDCLNT_E_UNSUPPORTED_FORMAT"
	case audclntEBufferSizeNotAligned:
		return "AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED"
	case audclntEInvalidStreamFlag:
		return "AUDCLNT_E_INVALID_STREAM_FLAG"
	case rpcEChangedMode:
		return "RPC_E_CHANGED_MODE"
	case coENotInitialized:
		return "CO_E_NOTINITIALIZED"
	}
	return fmt.Sprintf("HRESULT 0x%08X", uint32(hr))
}

// errDeviceInvalidated marks AUDCLNT_E_DEVICE_INVALIDATED, which is the one
// failure that calls for rebuilding the stream rather than retrying the call.
// It is a wrapped sentinel so callers match it with errors.Is instead of
// looking for a message -- the message is for the operator, not for control
// flow.
var errDeviceInvalidated = errors.New("audio device invalidated")

func hrError(op string, hr uintptr) error {
	msg := fmt.Sprintf("%s: %s", op, hresultText(hr))
	if uint32(hr) == audclntEDeviceInvalidated {
		return fmt.Errorf("%w: %s", errDeviceInvalidated, msg)
	}
	return errors.New(msg)
}

// ---------------------------------------------------------------------------
// Mix format
// ---------------------------------------------------------------------------

// waveFormatExtensible mirrors WAVEFORMATEXTENSIBLE, declared flat.
//
// Flat is not a style choice. WAVEFORMATEX is 18 bytes with 2-byte alignment,
// so nesting it would make Go round it up to 20 and shift dwChannelMask and
// SubFormat four bytes along -- reading the sub-format GUID as garbage and
// silently misclassifying the stream. Declared flat, every field lands on its
// packed offset and the total is exactly 40.
type waveFormatExtensible struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CBSize         uint16

	ValidBits   uint16
	ChannelMask uint32
	SubFormat   [16]byte
}

const (
	waveFormatExSize         = 18
	waveFormatExtensibleSize = 40
	subFormatOffset          = 24
)

// These are the assertions that catch the padding trap described above. A size
// or offset that drifts makes one of these array lengths negative, which is a
// compile error rather than a stream of noise.
var (
	_ [waveFormatExtensibleSize - unsafe.Sizeof(waveFormatExtensible{})]byte
	_ [unsafe.Sizeof(waveFormatExtensible{}) - waveFormatExtensibleSize]byte
	_ [subFormatOffset - unsafe.Offsetof(waveFormatExtensible{}.SubFormat)]byte
	_ [unsafe.Offsetof(waveFormatExtensible{}.SubFormat) - subFormatOffset]byte
)

// parseMixFormat reads a WAVEFORMATEX* -- possibly actually a
// WAVEFORMATEXTENSIBLE* -- into a deviceFormat.
//
// The header is read first and the extension only afterwards, and only when
// cbSize says it is present: the pointer may well address a bare 18-byte
// WAVEFORMATEX, and copying a fixed 40 bytes from it would read past the end.
func parseMixFormat(p unsafe.Pointer) (deviceFormat, error) {
	head := unsafe.Slice((*byte)(p), waveFormatExSize)
	tag := binary.LittleEndian.Uint16(head[0:])
	cbSize := binary.LittleEndian.Uint16(head[16:])

	n := waveFormatExSize
	if tag == waveFormatTagExtensible && cbSize >= waveFormatExtensibleMinSize {
		n = waveFormatExtensibleSize
	}

	// Only the bytes that are really there are copied; the rest of f stays
	// zero, so a short header can never contribute torn extension fields.
	var f waveFormatExtensible
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&f)), waveFormatExtensibleSize), unsafe.Slice((*byte)(p), n))

	return classifyFormat(f.FormatTag, f.CBSize, f.SubFormat,
		int(f.BitsPerSample), int(f.Channels), int(f.SamplesPerSec))
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

// Polling cadence. The device period is what actually sets the pace; these only
// bound it, in case a driver reports something absurd.
const (
	minPoll        = time.Millisecond
	maxPoll        = 10 * time.Millisecond
	defaultPoll    = 5 * time.Millisecond
	initialBackoff = 100 * time.Millisecond
	maxBackoff     = 2 * time.Second
)

// wasapiCapturer captures the default render endpoint.
//
// One goroutine drives it for its whole life: CoInitializeEx is per-thread and
// a goroutine can migrate between threads, so the constructing goroutine is
// pinned with runtime.LockOSThread and must be the one calling Read and Close.
type wasapiCapturer struct {
	format   audioStreamFormat
	conv     *pcmConverter
	srcBytes int
	poll     time.Duration

	enumerator unsafe.Pointer
	device     unsafe.Pointer
	client     unsafe.Pointer
	capture    unsafe.Pointer

	comInit bool
	started bool
	open    bool
	closed  bool
	backoff time.Duration

	out []int16
}

// AudioSupported reports that this platform has an audio backend.
//
// The predicate is not a restatement of NewAudioCapturer: it answers the
// question before any device is touched, which is what the startup banner and
// the /ws/audio route need. Windows is the only platform that answers true.
func AudioSupported() bool { return true }

// NewAudioCapturer opens the default render endpoint for loopback capture.
//
// It locks the calling goroutine to its OS thread for the lifetime of the
// capturer, so the caller must be a goroutine of its own -- see wasapiCapturer.
func NewAudioCapturer() (AudioCapturer, error) {
	runtime.LockOSThread()

	c := &wasapiCapturer{poll: defaultPoll}

	// S_OK and S_FALSE both mean COM is now initialised on this thread and both
	// require a matching CoUninitialize. Every failure, by contrast, leaves the
	// count untouched -- RPC_E_CHANGED_MODE in particular means someone else
	// owns the apartment, and uninitialising it would tear down their COM.
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if hrFailed(hr) {
		runtime.UnlockOSThread()
		return nil, hrError("CoInitializeEx", hr)
	}
	c.comInit = true

	if err := c.openStream(); err != nil {
		c.shutdown()
		return nil, err
	}
	return c, nil
}

// openStream builds the whole capture chain from scratch.
//
// There is no in-place retry anywhere: a failed Initialize can leave the client
// unusable, and a second Initialize on the same IAudioClient is liable to come
// back AUDCLNT_E_ALREADY_INITIALIZED. Recovery is always release-then-rebuild.
func (c *wasapiCapturer) openStream() error {
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		clctxAll,
		uintptr(unsafe.Pointer(&iidMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&c.enumerator)),
	)
	if hrFailed(hr) {
		return hrError("CoCreateInstance(IMMDeviceEnumerator)", hr)
	}

	// From here on every failure unwinds through closeStream, which releases
	// whatever was acquired in the reverse order.
	hr = comCall(c.enumerator, vtblGetDefaultAudioEndpoint,
		eRender, eConsole, uintptr(unsafe.Pointer(&c.device)))
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IMMDeviceEnumerator::GetDefaultAudioEndpoint", hr)
	}

	// Activate takes four arguments; pActivationParams is the nil one, and
	// dropping it makes the call fail on every interface.
	hr = comCall(c.device, vtblActivate,
		uintptr(unsafe.Pointer(&iidAudioClient)), clctxAll, 0,
		uintptr(unsafe.Pointer(&c.client)))
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IMMDevice::Activate(IAudioClient)", hr)
	}

	var mixFormat unsafe.Pointer
	hr = comCall(c.client, vtblGetMixFormat, uintptr(unsafe.Pointer(&mixFormat)))
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IAudioClient::GetMixFormat", hr)
	}
	// GetMixFormat allocates with CoTaskMemAlloc and hands ownership over. The
	// format is an input to Initialize, so this must not run before that call
	// returns -- hence a defer in this function rather than a free next to the
	// acquisition. IsFormatSupported is deliberately not called: in shared mode
	// it is redundant, and it allocates a second block that would need freeing.
	defer procCoTaskMemFree.Call(uintptr(mixFormat))

	dev, err := parseMixFormat(mixFormat)
	if err != nil {
		c.closeStream()
		return err
	}

	// Shared mode requires both duration and periodicity to be zero. Passing a
	// computed period here is a common mistake and yields AUDCLNT_E_INVALID_SIZE
	// on some drivers; the device period is only used to pace polling.
	hr = comCall(c.client, vtblInitialize,
		audclntShareModeShared,
		audclntStreamFlagsLoopback|audclntStreamFlagsNoPersist,
		0, 0,
		uintptr(mixFormat),
		0)
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IAudioClient::Initialize", hr)
	}

	// GetService is only valid once the client is initialised.
	hr = comCall(c.client, vtblGetService,
		uintptr(unsafe.Pointer(&iidAudioCaptureClient)),
		uintptr(unsafe.Pointer(&c.capture)))
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IAudioClient::GetService(IAudioCaptureClient)", hr)
	}

	c.conv = newPCMConverter(dev, 2)
	c.srcBytes = dev.Samples.bytes()
	c.format = audioStreamFormat{SampleRate: dev.SampleRate, Channels: c.conv.outCh}

	var defPeriod, minPeriod int64
	if hr = comCall(c.client, vtblGetDevicePeriod,
		uintptr(unsafe.Pointer(&defPeriod)),
		uintptr(unsafe.Pointer(&minPeriod))); !hrFailed(hr) && defPeriod > 0 {
		// REFERENCE_TIME is in 100 ns units; poll at half a period.
		c.poll = clampDuration(time.Duration(defPeriod)*50*time.Nanosecond, minPoll, maxPoll)
	}

	hr = comCall(c.client, vtblStart)
	if hrFailed(hr) {
		c.closeStream()
		return hrError("IAudioClient::Start", hr)
	}
	c.started = true
	c.open = true

	slog.Info("audio capture started",
		"device", dev.SampleRate, "deviceChannels", dev.Channels,
		"deviceSample", dev.Samples.String(),
		"wireRate", c.format.SampleRate, "wireChannels", c.format.Channels,
		"kbps", c.format.nominalBps()/1000)
	return nil
}

// closeStream tears the capture chain down in the reverse of the order it was
// built, leaving COM itself initialised so the endpoint can be rebuilt without
// a full restart.
func (c *wasapiCapturer) closeStream() {
	if c.started {
		comCall(c.client, vtblStop)
		c.started = false
	}
	comRelease(&c.capture)
	comRelease(&c.client)
	comRelease(&c.device)
	comRelease(&c.enumerator)
	c.open = false
}

// shutdown releases everything, including the COM apartment and the thread
// lock taken by NewAudioCapturer.
func (c *wasapiCapturer) shutdown() {
	c.closeStream()
	if c.comInit {
		procCoUninitialize.Call()
		c.comInit = false
	}
	runtime.UnlockOSThread()
}

// Format implements AudioCapturer.
func (c *wasapiCapturer) Format() audioStreamFormat { return c.format }

// Close implements AudioCapturer. It must run on the goroutine that constructed
// the capturer.
func (c *wasapiCapturer) Close() {
	if c.closed {
		return
	}
	c.closed = true
	c.shutdown()
}

// Read implements AudioCapturer.
//
// A device is never abandoned: if the endpoint goes away -- the default output
// changing, headphones being unplugged -- the stream is rebuilt and the call
// reports no data rather than an error, because the caller's answer is the same
// either way: keep the audio path alive and wait.
func (c *wasapiCapturer) Read() ([]int16, bool, error) {
	if c.closed {
		return nil, false, nil
	}

	if !c.open {
		if err := c.openStream(); err != nil {
			slog.Warn("audio capture unavailable; retrying",
				"err", err, "retryIn", c.nextBackoff())
			time.Sleep(c.backoff)
			return nil, false, nil
		}
		c.backoff = 0
	}

	c.out = c.out[:0]

	// Wait up to one packet's worth for the device to produce something. An
	// idle endpoint produces nothing at all, so this timeout is also what tells
	// the silence watchdog that the system has gone quiet.
	deadline := time.Now().Add(time.Duration(audioPacketMs) * time.Millisecond)
	for len(c.out) == 0 {
		n, err := c.nextPacketSize()
		if err != nil {
			return c.rebuildable(err)
		}
		if n == 0 {
			if !time.Now().Before(deadline) {
				return nil, false, nil
			}
			time.Sleep(c.poll)
			continue
		}
		// Drain whatever else is queued as well, so a period of backlog does
		// not stretch the packet cadence.
		for n > 0 {
			if err := c.drainPacket(int(n)); err != nil {
				return c.rebuildable(err)
			}
			if n, err = c.nextPacketSize(); err != nil {
				return c.rebuildable(err)
			}
		}
		break
	}

	// Getting data means the endpoint is healthy, so a flapping device does not
	// stay backed off after it recovers.
	c.backoff = 0
	return c.out, true, nil
}

// nextPacketSize returns the frames waiting in the next packet, or zero when
// the endpoint has nothing to give.
func (c *wasapiCapturer) nextPacketSize() (uint32, error) {
	var size uint32
	hr := comCall(c.capture, vtblGetNextPacketSize, uintptr(unsafe.Pointer(&size)))
	if hrFailed(hr) {
		return 0, hrError("IAudioCaptureClient::GetNextPacketSize", hr)
	}
	return size, nil
}

// drainPacket moves one device packet into the output buffer.
func (c *wasapiCapturer) drainPacket(frames int) error {
	var (
		data  unsafe.Pointer
		flags uint32
	)
	hr := comCall(c.capture, vtblGetBuffer,
		uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&frames)),
		uintptr(unsafe.Pointer(&flags)),
		0, 0)
	switch {
	case uint32(hr) == audclntSBufferEmpty:
		// Success code, not a failure: the device emptied between the size
		// query and this call. Nothing to read.
		return nil
	case hrFailed(hr):
		return hrError("IAudioCaptureClient::GetBuffer", hr)
	}
	if frames <= 0 {
		return nil
	}

	// ReleaseBuffer must run however the conversion goes, and it must run with
	// the original frame count.
	defer comCall(c.capture, vtblReleaseBuffer, uintptr(frames))

	if flags&audclntBufferFlagsSilent != 0 {
		// The buffer contents are undefined when the driver flags silence, so
		// they must not be converted. Appending zeros keeps the packet cadence
		// intact and lets the silence watchdog decide when to stop sending.
		for i := 0; i < frames*c.conv.outCh; i++ {
			c.out = append(c.out, 0)
		}
		return nil
	}

	src := unsafe.Slice((*byte)(data), frames*c.conv.srcCh*c.srcBytes)
	chunk, _ := c.conv.convert(src, frames)
	c.out = append(c.out, chunk...)
	return nil
}

// rebuildable handles a failed streaming call. Only a device invalidation is
// worth rebuilding for; anything else is transient and the next call retries.
func (c *wasapiCapturer) rebuildable(err error) ([]int16, bool, error) {
	if isDeviceInvalidated(err) {
		slog.Warn("audio device changed; rebuilding capture", "retryIn", c.nextBackoff())
		c.closeStream()
		time.Sleep(c.backoff)
		return nil, false, nil
	}
	slog.Warn("audio capture read failed", "err", err)
	return nil, false, nil
}

// nextBackoff advances the rebuild delay and returns the new value.
func (c *wasapiCapturer) nextBackoff() time.Duration {
	if c.backoff <= 0 {
		c.backoff = initialBackoff
	} else {
		c.backoff = min(2*c.backoff, maxBackoff)
	}
	return c.backoff
}

// isDeviceInvalidated reports whether err came back as
// AUDCLNT_E_DEVICE_INVALIDATED, which is what changing or unplugging the
// default output device produces. It can surface from any streaming call, not
// just one particular one.
func isDeviceInvalidated(err error) bool { return errors.Is(err, errDeviceInvalidated) }

func clampDuration(v, lo, hi time.Duration) time.Duration {
	return min(max(v, lo), hi)
}
