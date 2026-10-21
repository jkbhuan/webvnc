//go:build windows && amd64

package mfenc

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// mftCategoryVideoEncoder is MFT_CATEGORY_VIDEO_ENCODER
// {F79EAC7D-E545-4387-BDEE-D647D7BDE42A}. MFTEnumEx takes it by value, which is
// the whole reason this package is amd64-only: under the amd64 ABI a 16-byte
// struct argument is passed as a pointer to a copy, under 32-bit it is pushed
// word by word.
var mftCategoryVideoEncoder = guid{
	0xF79EAC7D, 0xE545, 0x4387,
	[8]byte{0xBD, 0xEE, 0xD6, 0x47, 0xD7, 0xBD, 0xE4, 0x2A},
}

// mftFriendlyNameAttribute is MFT_FRIENDLY_NAME_Attribute.
var mftFriendlyNameAttribute = guid{
	0x314FFBAE, 0x5B41, 0x4C95,
	[8]byte{0x9C, 0x19, 0x4E, 0x7D, 0x58, 0x6F, 0xAC, 0xE3},
}

// mftRegisterTypeInfo mirrors MFT_REGISTER_TYPE_INFO.
type mftRegisterTypeInfo struct {
	MajorType guid
	Subtype   guid
}

// MFT_ENUM_FLAG_*: every kind of transform, sorted so the preferred ones come
// first.
const mftEnumFlagAllAndSort = 0x3F | 0x40

// Probe exercises the encoder on this machine and writes a report to out.
//
// It drives the same newSession the server will use, so a machine that passes
// the probe runs the production path; the extra work here is inspection --
// which encoder MFTs exist, which media types they offer, and what the
// resulting bitstream actually looks like.
func Probe(out io.Writer, cfg Config, frames int) error {
	cfg = cfg.withDefaults()
	if frames <= 0 {
		frames = 90
	}

	// Same discipline as the encoder: COM is per thread, and the thread must
	// not move under us.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	uninitialize, err := coInitialize()
	if err != nil {
		return err
	}
	defer uninitialize()

	shutdown, err := mfStartup()
	if err != nil {
		return fmt.Errorf("MFStartup: %w", err)
	}
	defer shutdown()
	p("[ok] MFStartup (version 0x%X, no socket)", mfVersion)

	p("")
	p("--- encoder MFTs on this machine ---")
	enumerateEncoders(out)

	p("")
	p("--- what the encoder offers, before any of it is configured ---")
	offerings(out, cfg)

	p("")
	p("--- configuring for %dx%d, %d fps, %d kbps ---",
		cfg.Width, cfg.Height, cfg.FPS, cfg.Bitrate/1000)

	s, err := newSession(cfg)
	if err != nil {
		p("[FAIL] %v", err)
		return err
	}
	p("[ok] configured: input NV12 stride %d (%d bytes), encoder allocates its own output samples: %v",
		s.stride, s.inSize, s.outProvidesSamples)

	reportTypes(out, s.transform)
	s.close()

	err = compareModes(out, cfg, frames)

	// The comparison runs even when the default encoder failed: which encoders
	// a machine has, and how deep each one's pipeline is, is the question that
	// decides whether the default is the right one, and a machine where the
	// default does not work at all is exactly where it matters most.
	tryEncoders(out, cfg, frames)
	return err
}

// tryEncoders runs the same clip through every NV12 -> H.264 encoder registered
// on the machine and prints one line of numbers for each.
//
// This exists because the choice of encoder is the one decision that cannot be
// made from documentation. All of these produce H.264; what separates them is
// how they are driven (a synchronous MFT answers ProcessOutput as it goes, an
// asynchronous one wants an event loop) and how many frames each holds back
// before emitting the first. The default encoder held twelve, which at thirty
// frames a second is four hundred milliseconds of lag for every viewer, and no
// property it exposes would turn that off.
func tryEncoders(out io.Writer, cfg Config, frames int) {
	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	p("")
	p("--- every encoder on this machine, same configuration ---")

	items, err := enumEncoders()
	if err != nil {
		p("[warn] %v", err)
		return
	}
	defer releaseAll(items)
	if len(items) == 0 {
		p("[warn] none registered")
		return
	}

	// Long enough to get past the priming delay and see a steady rate, short
	// enough that a machine with several encoders finishes in one sitting.
	clip := frames
	if clip > 60 {
		clip = 60
	}
	p("%d frames each, %dx%d, %d kbps", clip, cfg.Width, cfg.Height, cfg.Bitrate/1000)
	p("")

	for i, item := range items {
		name := friendlyName(item)
		p("[%d] %s", i+1, name)

		transform, err := activateObject(item)
		if err != nil {
			p("    [FAIL] %v", err)
			continue
		}

		async, known := transformIsAsync(transform)
		switch {
		case !known:
			// Absent is the normal state for a synchronous MFT -- the
			// attribute only exists to warn callers off an asynchronous one --
			// so this is the expected reading, not a fault.
			p("    async: no (attribute absent, which is what a synchronous one looks like)")
		case async:
			p("    async: yes")
			// An async MFT refuses every media type until it is told the
			// caller is not the Media Foundation pipeline. Without this the
			// measurement below would fail at the first SetOutputType and
			// report the encoder as broken when it is merely locked.
			if err := unlockAsync(transform); err != nil {
				p("    [FAIL] unlock: %v", err)
				release(&transform)
				continue
			}
		default:
			p("    async: no")
		}

		// measureWith takes the transform over in every case -- it closes the
		// session it builds, which releases it -- so there is nothing to do
		// with it afterwards, error or not.
		m, err := measureWith(transform, cfg, clip)
		reportEncoder(p, m, err)
	}
}

// reportEncoder prints one encoder's measurement as a single line, or why there
// is no line to print.
func reportEncoder(p func(string, ...any), m *measurement, err error) {
	if m == nil {
		if err == nil {
			err = fmt.Errorf("no measurement")
		}
		p("    [FAIL] %v", err)
		return
	}

	if m.outputs == 0 {
		p("    [FAIL] produced no output: %v", err)
		return
	}

	// Lag is measured against the frames the encoder actually took. Counting
	// refused frames as held-back ones would report a pipeline depth that is
	// really just the caller outrunning the encoder.
	accepted := m.frames - m.dropped
	lag := accepted - m.outputs
	p("    first %d, out %d/%d, keys %d, key %d B, delta %d B, %.0f kbps, %.2f ms/frame, %s",
		m.firstOut, m.outputs, accepted, m.keyCount, m.avgKey(), m.avgDelta(),
		m.kbps(), m.msPerFrame(), "forced keyframe "+forceGapText(m))
	if m.dropped > 0 {
		p("    refused %d of %d frames without draining (feeds faster than it encodes)",
			m.dropped, m.frames)
	}
	switch {
	case lag > 0:
		p("    holds back %d frames (%.0f ms at %d fps) -- every frame the viewer is behind",
			lag, float64(lag)*1000/float64(m.fps), m.fps)
	case m.dropped > 0:
		p("    output volume is not a latency reading here: frames were refused")
	default:
		p("    holds back nothing: output keeps up with input")
	}
	if err != nil {
		p("    stopped early: %v", err)
	}
	p("    %s", lowLatencyVerdict(m.settings))
}

// reportSettings prints what the encoder did with each optional setting, for the
// one mode being reported.
//
// The point of this table is the difference between a property the encoder
// accepted and one it quietly dropped, which is invisible otherwise and decides
// whether the latency and rate control this pipeline assumes actually hold.
func reportSettings(out io.Writer, settings []CodecSetting, mode string) {
	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	p("")
	p("--- optional settings (%s) ---", mode)
	if len(settings) == 0 {
		p("  (none attempted: the encoder exposes no ICodecAPI)")
		return
	}
	for _, s := range settings {
		mark := "[ok]  "
		if !s.Supported || s.Err != nil {
			mark = "[warn]"
		}
		p("  %s %s", mark, s)
	}
}

// compareModes runs the same clip through each rate control mode, so the choice
// can be made on measurements rather than on the names.
func compareModes(out io.Writer, cfg Config, frames int) error {
	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	modes := []RateControl{
		RateControlCBR,
		RateControlVBR,
		RateControlQuality,
		RateControlLowDelayVBR,
	}

	want := cfg.RateControl
	if want == RateControlDefault {
		want = defaultRateControl
	}

	p("")
	p("--- rate control modes, %d frames each ---", frames)
	p("%-14s %5s %5s %5s %9s %9s %8s %8s %5s %-24s %s",
		"mode", "first", "out", "keys", "key B", "delta B", "kbps", "ms/frame",
		"held", "forced keyframe", "low latency")

	results := make([]*measurement, len(modes))
	reported := -1

	for i, mode := range modes {
		c := cfg
		c.RateControl = mode

		m, err := runMode(c, frames)
		if m == nil {
			p("%-14s [FAIL] %v", mode, err)
			continue
		}
		results[i] = m

		if m.firstOut < 0 {
			p("%-14s [FAIL] no output: %v", mode, err)
			continue
		}
		if mode == want {
			reported = i
		}

		// held is frames the encoder took and has not answered for. It is the
		// number this table exists to show: it is how far behind a viewer runs,
		// and unlike the frame sizes it does not improve by waiting.
		held := m.frames - m.dropped - m.outputs
		p("%-14s %5d %5d %5d %9d %9d %8.0f %8.2f %5d %-24s %s",
			mode, m.firstOut, m.outputs, m.keyCount, m.avgKey(), m.avgDelta(),
			m.kbps(), m.msPerFrame(), held, forceGapText(m), lowLatencyVerdict(m.settings))
		if m.dropped > 0 {
			p("               %d frames refused without draining", m.dropped)
		}
		if m.err != nil {
			p("               stopped early: %v", m.err)
		}
	}

	// A one-second GOP, to confirm the GOP setting is honoured at all. It
	// matters because it is the fallback when a keyframe cannot be forced: a
	// client that joins then waits at most one GOP for a picture, where the
	// four-second default leaves it staring at nothing for four seconds.
	gop := cfg
	gop.KeyframeEvery = 1
	if m, err := runMode(gop, frames); m != nil && m.firstOut >= 0 {
		p("")
		p("--- one-second GOP (GOPSize %d) ---", cfg.FPS)
		p("keyframes           %v", m.keyIndices)
		p("forced keyframe lag %s", forceGapText(m))
		if err != nil {
			p("stopped early:      %v", err)
		}
	}

	// The trace is only worth printing once, and the run worth printing is the one
	// whose mode the configuration actually asked for -- that is what the server
	// will be running. If that run failed, the first one that produced anything
	// is a better answer than nothing.
	if reported < 0 {
		for i, m := range results {
			if m != nil && m.firstOut >= 0 {
				reported = i
				break
			}
		}
	}
	if reported < 0 {
		return fmt.Errorf("no rate control mode produced any output")
	}
	report(out, results[reported])
	return nil
}

// runMode calls measure with a panic barrier.
//
// The whole point of this probe is to get information out of a machine nobody
// can attach a debugger to. A panic in one mode would take the other three down
// with it and leave nothing to read, so a failure becomes an error like any
// other and the remaining modes still run.
func runMode(cfg Config, frames int) (m *measurement, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return measure(cfg, frames)
}

// nearMultiple reports whether n is a multiple of period, allowing one frame of
// slack: an encoder that counts its GOP slightly differently from the way its
// configuration reads would otherwise have every keyframe look forced.
func nearMultiple(n, period int) bool {
	r := n % period
	return r <= 1 || r >= period-1
}

// forceGapText is how long a forced keyframe took to appear, or that it never
// did. A keyframe that cannot be forced is the difference between a client
// seeing a picture the moment it connects and waiting for the GOP to come round.
func forceGapText(m *measurement) string {
	switch {
	case m.forceGap < 0:
		return "no IDR"
	case m.forceGapNatural:
		return fmt.Sprintf("%d frames (GOP would explain it)", m.forceGap)
	default:
		return fmt.Sprintf("%d frames", m.forceGap)
	}
}

// lowLatencyVerdict summarises whether the encoder kept the latency setting,
// which is the one that decides how far behind the picture runs.
func lowLatencyVerdict(settings []CodecSetting) string {
	const prefix = "MF_LOW_LATENCY ("
	var asked, kept []string
	for _, s := range settings {
		if !strings.HasPrefix(s.Name, prefix) {
			continue
		}
		where := strings.TrimSuffix(strings.TrimPrefix(s.Name, prefix), ")")
		asked = append(asked, where)
		if s.Supported && s.Err == nil {
			kept = append(kept, where)
		}
	}
	switch {
	case len(asked) == 0:
		return "not asked"
	case len(kept) == len(asked):
		return "kept (" + strings.Join(kept, ",") + ")"
	case len(kept) == 0:
		return "dropped (" + strings.Join(asked, ",") + ")"
	default:
		return "partial: kept " + strings.Join(kept, ",") + " of " + strings.Join(asked, ",")
	}
}

// measurement is what one run of the encode loop produced.
type measurement struct {
	rateControl RateControl
	frames      int
	width       int
	height      int
	fps         int
	stride      int

	firstOut   int
	outputs    int
	keyCount   int
	deltaCount int
	keyBytes   int
	deltaBytes int
	totalBytes int

	// dropped is how many frames the encoder refused because it had not been
	// drained. Read alongside outputs: a run where a third of the frames were
	// refused has not measured the encoder's depth, it has measured the
	// caller's pacing, and the two are worth telling apart before either is
	// quoted as a latency.
	dropped int

	// forceGap is how many frames passed between asking for a keyframe
	// mid-stream and the IDR appearing, or -1 if none ever did. It is the
	// pipeline depth: frames the encoder is holding, seen as lag by a viewer.
	//
	// forceGapNatural marks the reading as unattributable: the IDR that
	// answered the request landed where the running GOP would have put one
	// anyway, so it says nothing about whether the request was honoured. An
	// earlier version of this probe counted exactly that as a success and
	// reported a 27-frame "forced keyframe lag" on an encoder that ignores the
	// request entirely -- the natural keyframe of a 30-frame GOP, arriving on
	// schedule. A measurement that can be satisfied by doing nothing is worse
	// than no measurement.
	forceGap        int
	forceGapNatural bool

	encodeTime time.Duration
	keyIndices []int
	settings   []CodecSetting

	// err is set when the run stopped early. Everything measured up to that
	// point is still filled in.
	err error

	// trace is the per-frame line for the frames worth showing, kept rather
	// than written as it goes: the table of modes is printed first, and a
	// running commentary interleaved into it would be unreadable.
	trace []string

	first     []byte
	codecStr  string
	inBandSPS bool
}

func (m *measurement) avgKey() int {
	if m.keyCount == 0 {
		return 0
	}
	return m.keyBytes / m.keyCount
}

func (m *measurement) avgDelta() int {
	if m.deltaCount == 0 {
		return 0
	}
	return m.deltaBytes / m.deltaCount
}

// kbps is the average rate over the clip's wall-clock duration.
func (m *measurement) kbps() float64 {
	if m.frames == 0 {
		return 0
	}
	seconds := float64(m.frames) / float64(m.fps)
	return float64(m.totalBytes) * 8 / 1000 / seconds
}

func (m *measurement) msPerFrame() float64 {
	if m.frames == 0 {
		return 0
	}
	return float64(m.encodeTime.Microseconds()) / 1000 / float64(m.frames)
}

// measure runs a clip through a freshly configured encoder and collects the
// numbers. The per-frame trace covers the first frames, every fifteenth after
// that, and the frame following the forced keyframe.
func measure(cfg Config, frames int) (*measurement, error) {
	return measureWith(nil, cfg, frames)
}

// measureWith runs the clip through the supplied transform, or through the
// default encoder when transform is nil.
//
// The transform is configured by exactly the same code the server uses, so a
// measurement taken here describes the encoder this machine would really run,
// not a tidier pipeline invented for the probe.
func measureWith(transform unsafe.Pointer, cfg Config, frames int) (*measurement, error) {
	var (
		s   *session
		err error
	)
	if transform == nil {
		s, err = newSession(cfg)
	} else {
		s, err = newSessionWith(transform, cfg)
	}
	if err != nil {
		return nil, err
	}
	defer s.close()

	m := &measurement{
		rateControl: cfg.RateControl,
		frames:      frames,
		width:       cfg.Width,
		height:      cfg.Height,
		fps:         cfg.FPS,
		stride:      s.stride,
		firstOut:    -1,
		forceGap:    -1,
	}

	rgba := make([]byte, cfg.Width*cfg.Height*4)
	stripe := cfg.Width * 4

	// A keyframe is forced at the halfway point, well after the encoder has
	// finished priming, so that the gap between the request and the IDR measures
	// the pipeline depth rather than the start-up delay.
	//
	// Where the GOP would put a keyframe on its own is worked out from the first
	// one seen rather than from the configured size: the encoder anchors its GOP
	// somewhere of its own choosing -- frame 12 on the machines measured so far,
	// not frame 0 -- and an anchor taken from the configuration would be wrong
	// by exactly that offset.
	forceAt := frames / 2
	awaitForce := false
	gop := cfg.FPS * cfg.KeyframeEvery
	anchor := -1

	for i := 0; i < frames; i++ {
		syntheticFrame(rgba, cfg.Width, cfg.Height, i)

		force := i == 0 || i == forceAt
		if i == forceAt {
			awaitForce = true
		}

		start := time.Now()
		chunk, err := s.encode(rgba, stripe, force)
		m.encodeTime += time.Since(start)
		if err != nil {
			// The frames before the failure are worth keeping: they are what
			// says whether the trouble is the encoder or the configuration, and
			// a run that dies at frame 60 has already answered most of the
			// questions this probe was asked.
			m.err = fmt.Errorf("frame %d: %w", i, err)
			break
		}

		if len(chunk) == 0 {
			m.tracef("  frame %2d: (nothing yet)", i)
			continue
		}
		if m.firstOut < 0 {
			m.firstOut = i
			m.first = chunk
			if cs, err := CodecString(chunk); err == nil {
				m.codecStr = cs
			}
		}

		key := IsKeyframe(chunk)
		m.totalBytes += len(chunk)
		if key {
			m.keyBytes += len(chunk)
			m.keyCount++
			m.keyIndices = append(m.keyIndices, i)
			if anchor < 0 {
				anchor = i
			}
			if awaitForce {
				m.forceGap = i - forceAt
				m.forceGapNatural = gop > 0 && anchor >= 0 && nearMultiple(i-anchor, gop)
				awaitForce = false
			}
		} else {
			m.deltaBytes += len(chunk)
			m.deltaCount++
		}
		if !m.inBandSPS {
			for _, unit := range SplitNALUs(chunk) {
				if NALType(unit) == nalSPS {
					m.inBandSPS = true
					break
				}
			}
		}

		if i < 12 || i%15 == 0 || i == forceAt+1 {
			m.tracef("  frame %2d: %7d bytes  %s", i, len(chunk), DescribeNALUs(chunk))
		}
	}

	m.outputs = m.keyCount + m.deltaCount
	m.dropped = s.dropped
	m.settings = s.settingsReport()

	if m.err != nil {
		return m, m.err
	}
	if m.firstOut < 0 {
		m.err = fmt.Errorf("the encoder produced nothing at all")
		return m, m.err
	}
	return m, nil
}

func (m *measurement) tracef(format string, args ...any) {
	m.trace = append(m.trace, fmt.Sprintf(format, args...))
}

// report prints one run in full, including the parts that decide whether a
// browser can decode it.
func report(out io.Writer, m *measurement) {
	p := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	if len(m.trace) > 0 {
		p("")
		p("--- frame by frame (%s) ---", m.rateControl)
		for _, line := range m.trace {
			p("%s", line)
		}
	}

	reportSettings(out, m.settings, m.rateControl.String())

	p("")
	p("--- report: %s ---", m.rateControl)
	if m.err != nil {
		p("[FAIL]              %v", m.err)
	}
	p("resolution          %dx%d (NV12 stride %d)", m.width, m.height, m.stride)
	p("first output        frame %d", m.firstOut)
	p("frames with output  %d of %d", m.outputs, m.frames-m.dropped)
	if m.dropped > 0 {
		p("frames refused      %d (the encoder had not been drained)", m.dropped)
	}

	// A first output late in the run is only a start-up delay if the frames
	// catch up afterwards; if they never do, the encoder is holding that many
	// frames back and every one of them is latency the viewer sits through.
	//
	// The count is against the frames the encoder accepted, not the frames
	// offered: a frame the encoder refused never entered it and so cannot be
	// one it is holding.
	accepted := m.frames - m.dropped
	if m.outputs > 0 && m.outputs < accepted {
		behind := accepted - m.outputs
		p("input lag           %d frames (%.0f ms at %d fps)",
			behind, float64(behind)*1000/float64(m.fps), m.fps)
	}
	p("keyframes           %v", m.keyIndices)
	p("forced keyframe lag %s", forceGapText(m))
	if m.keyCount > 0 {
		p("key frame size      %d bytes average", m.avgKey())
	}
	if m.deltaCount > 0 {
		p("delta frame size    %d bytes average", m.avgDelta())
	}
	if m.avgKey() > 0 && m.avgDelta() > 0 {
		p("delta/key ratio     1:%.1f", float64(m.avgKey())/float64(m.avgDelta()))
	}
	p("average rate        %.0f kbps over %d frames at %d fps", m.kbps(), m.frames, m.fps)
	if ms := m.msPerFrame(); ms > 0 {
		p("encode cost         %.2f ms per frame (%.0f fps ceiling)", ms, 1000/ms)
	}
	p("")
	p("bitstream form      %s", classifyBitstream(m.first))
	p("SPS and PPS in band %v", m.inBandSPS)
	p("codec string        %s", m.codecStr)

	if sps := firstNAL(m.first, nalSPS); sps != nil {
		p("SPS                 % X", sps)
	}
	if pps := firstNAL(m.first, nalPPS); pps != nil {
		p("PPS                 % X", pps)
	}
	p("")
	p("The last lines are what the browser needs: with Annex B and SPS/PPS in band")
	p("and a matching codec string, WebCodecs decodes this with no out-of-band setup")
	p("at all -- no description, no fMP4, no MSE.")
}

// enumEncoders returns one IMFActivate per registered NV12 -> H.264 encoder,
// with a reference held on each. The caller releases them.
//
// The activations are handed back rather than printed in place because the two
// things that want them want different halves: the listing only needs the name,
// while trying an encoder needs the object itself. Each is created once either
// way, and enumerating twice would risk a machine where the second call returns
// a different set than the first -- which is exactly the kind of thing that
// never happens until it does, on the one machine nobody can reach.
func enumEncoders() ([]unsafe.Pointer, error) {
	input := mftRegisterTypeInfo{MajorType: mfMediaTypeVideo, Subtype: mfVideoFormatNV12}
	output := mftRegisterTypeInfo{MajorType: mfMediaTypeVideo, Subtype: mfVideoFormatH264}

	var list unsafe.Pointer
	var count uint32
	r := mustCall(procMFTEnumEx,
		uintptr(unsafe.Pointer(&mftCategoryVideoEncoder)),
		mftEnumFlagAllAndSort,
		uintptr(unsafe.Pointer(&input)),
		uintptr(unsafe.Pointer(&output)),
		uintptr(unsafe.Pointer(&list)),
		uintptr(unsafe.Pointer(&count)))
	if h := hr(r); h.failed() {
		return nil, fmt.Errorf("MFTEnumEx: %w", h)
	}
	if count == 0 || list == nil {
		return nil, nil
	}

	// The result is a flat array of IMFActivate pointers, each holding a
	// reference, and the array itself is a CoTaskMemAlloc block. Freeing the
	// array does not drop the references its elements hold, so the activations
	// survive being copied out and can be released later.
	items := make([]unsafe.Pointer, count)
	copy(items, unsafe.Slice((*unsafe.Pointer)(list), count))
	mustCall(procCoTaskMemFree, uintptr(list))
	return items, nil
}

// releaseAll drops a slice of interface pointers.
func releaseAll(items []unsafe.Pointer) {
	for i := range items {
		release(&items[i])
	}
}

func enumerateEncoders(out io.Writer) {
	items, err := enumEncoders()
	if err != nil {
		fmt.Fprintf(out, "[warn] %v\n", err)
		return
	}
	defer releaseAll(items)
	if len(items) == 0 {
		fmt.Fprintln(out, "[warn] no NV12 -> H.264 encoder is registered")
		return
	}
	fmt.Fprintf(out, "%d encoder(s):\n", len(items))
	for i, item := range items {
		fmt.Fprintf(out, "  %d. %s\n", i+1, friendlyName(item))
	}
}

// friendlyName reads an IMFActivate's display name, falling back to the
// transform's CLSID when the machine does not supply one.
func friendlyName(activate unsafe.Pointer) string {
	var raw unsafe.Pointer
	var n uint32
	r := comCall(activate, vtblGetAllocatedString,
		uintptr(unsafe.Pointer(&mftFriendlyNameAttribute)),
		uintptr(unsafe.Pointer(&raw)), uintptr(unsafe.Pointer(&n)))
	if h := hr(r); h.failed() || raw == nil {
		return "(unnamed)"
	}
	defer mustCall(procCoTaskMemFree, uintptr(raw))

	// psz is UTF-16 and already NUL-terminated; pcch includes the terminator.
	u16 := unsafe.Slice((*uint16)(raw), n)
	return syscall.UTF16ToString(u16)
}

// offerings prints the input and output media types the encoder will name, so
// that a configuration failure can be read against what was actually on offer.
func offerings(out io.Writer, cfg Config) {
	transform, err := coCreateInstance(&clsidH264Encoder, &iidIMFTransform)
	if err != nil {
		fmt.Fprintf(out, "[FAIL] CoCreateInstance(CLSID_CMSH264EncoderMFT): %v\n", err)
		return
	}
	defer release(&transform)
	fmt.Fprintln(out, "[ok] CoCreateInstance(CLSID_CMSH264EncoderMFT)")

	blame := func(what string, h hresult) {
		if h == errNoMoreTypes {
			fmt.Fprintf(out, "    (no more types %s)\n", what)
		} else {
			fmt.Fprintf(out, "    (%v %s)\n", h, what)
		}
	}

	fmt.Fprintln(out, "  output types:")
	var offered unsafe.Pointer
	for i := 0; i <= 16; i++ {
		var mt unsafe.Pointer
		r := comCall(transform, vtblGetOutputAvailableType, 0, uintptr(i), uintptr(unsafe.Pointer(&mt)))
		if h := hr(r); h.failed() {
			blame(fmt.Sprintf("after %d", i), h)
			break
		}
		fmt.Fprintf(out, "    %s\n", describeMediaType(mt))
		if offered == nil {
			offered = mt // keep the first one to unlock the input list
			continue
		}
		release(&mt)
	}

	// The input types are only listable once an output type is set. Asking
	// before that is not an error to report but a step to take: the encoder
	// answers MF_E_TRANSFORM_TYPE_NOT_SET, which says nothing about why.
	fmt.Fprintln(out, "  input types:")
	if offered == nil {
		fmt.Fprintln(out, "    (none listable: the encoder offered no output type to unlock them)")
		return
	}
	defer release(&offered)

	// The offered type is bare -- a major type and a subtype and nothing else --
	// and the encoder refuses it as it stands, so it has to be completed first.
	// This is the same filling the real configuration does.
	if err := fillOutputType(offered, cfg); err != nil {
		fmt.Fprintf(out, "    (could not complete the offered type: %v)\n", err)
		return
	}
	if h := hr(comCall(transform, vtblSetOutputType, 0, uintptr(offered), 0)); h.failed() {
		fmt.Fprintf(out, "    (could not set an output type to unlock the list: %v)\n", h)
		return
	}
	for i := 0; i <= 16; i++ {
		var mt unsafe.Pointer
		r := comCall(transform, vtblGetInputAvailableType, 0, uintptr(i), uintptr(unsafe.Pointer(&mt)))
		if h := hr(r); h.failed() {
			blame(fmt.Sprintf("after %d", i), h)
			break
		}
		fmt.Fprintf(out, "    %s\n", describeMediaType(mt))
		release(&mt)
	}
}

// reportTypes prints the types the encoder actually settled on, which are the
// ones that matter: an encoder may accept a type and then quietly narrow it.
func reportTypes(out io.Writer, transform unsafe.Pointer) {
	var in unsafe.Pointer
	if h := hr(comCall(transform, vtblGetInputCurrentType, 0, uintptr(unsafe.Pointer(&in)))); h.failed() {
		fmt.Fprintf(out, "[warn] GetInputCurrentType: %v\n", h)
	} else {
		fmt.Fprintf(out, "negotiated input:  %s\n", describeMediaType(in))
		release(&in)
	}

	var mt unsafe.Pointer
	r := comCall(transform, vtblGetOutputCurrentType, 0, uintptr(unsafe.Pointer(&mt)))
	if h := hr(r); h.failed() {
		fmt.Fprintf(out, "[warn] GetOutputCurrentType: %v\n", h)
		return
	}
	defer release(&mt)
	fmt.Fprintf(out, "negotiated output: %s\n", describeMediaType(mt))

	if v, err := getUINT32(mt, &mfMTAvgBitrate); err == nil {
		fmt.Fprintf(out, "  MF_MT_AVG_BITRATE = %d bps\n", v)
	}
	if v, err := getUINT32(mt, &mfMTMpeg2Profile); err == nil {
		fmt.Fprintf(out, "  MF_MT_MPEG2_PROFILE = %d\n", v)
	}
	if v, err := getUINT32(mt, &mfMTMpeg2Level); err == nil {
		fmt.Fprintf(out, "  MF_MT_MPEG2_LEVEL = %d\n", v)
	}
	// The sequence header is the out-of-band SPS/PPS. If it is present the
	// stream also carries them in band (the probe checks that separately), and
	// having both is what a WebCodecs description would need.
	if blob, err := getBlob(mt, &mfMTMpegSequenceHeader); err == nil {
		fmt.Fprintf(out, "  MF_MT_MPEG_SEQUENCE_HEADER = %d bytes: % X\n", len(blob), blob)
	}
}

// syntheticFrame draws a frame that behaves like a desktop: mostly flat, with a
// moving element and a bar that changes on a different cadence, so that both
// predicted frames and periodic refreshes are exercised.
func syntheticFrame(dst []byte, w, h, i int) {
	for y := 0; y < h; y++ {
		row := y * w * 4
		base := byte(y * 64 / h)
		for x := 0; x < w; x++ {
			o := row + x*4
			dst[o] = base + byte(x*64/w)
			dst[o+1] = base
			dst[o+2] = 40
			dst[o+3] = 0xFF
		}
	}
	bx := (i * 8) % max(w-160, 1)
	by := (i * 3) % max(h-160, 1)
	fillRect(dst, w, h, bx, by, 150, 120, 240, 240, 240)
	fillRect(dst, w, h, 24, 24+(i*2)%64, 420, 26, 16, 16, 16)
}

func fillRect(dst []byte, w, h, x0, y0, rw, rh int, r, g, b byte) {
	for y := y0; y < y0+rh && y < h; y++ {
		if y < 0 {
			continue
		}
		row := y * w * 4
		for x := x0; x < x0+rw && x < w; x++ {
			if x < 0 {
				continue
			}
			o := row + x*4
			dst[o], dst[o+1], dst[o+2], dst[o+3] = r, g, b, 0xFF
		}
	}
}

// classifyBitstream tells Annex B (start codes) from AVCC (length prefixes).
// The distinction decides whether a browser needs an out-of-band description.
func classifyBitstream(b []byte) string {
	if len(b) >= 4 {
		switch {
		case b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1:
			return "Annex B (4-byte start codes)"
		case b[0] == 0 && b[1] == 0 && b[2] == 1:
			return "Annex B (3-byte start codes)"
		}
		n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
		if int(n) == len(b)-4 {
			return "AVCC (length-prefixed NAL units)"
		}
		return fmt.Sprintf("unrecognised: starts % X", b[:min(8, len(b))])
	}
	return "too short to tell"
}

// firstNAL returns the first unit of a given type, for printing.
func firstNAL(stream []byte, typ int) []byte {
	for _, unit := range SplitNALUs(stream) {
		if NALType(unit) == typ {
			return unit
		}
	}
	return nil
}

// describeMediaType renders a media type as its subtype plus the attributes
// that matter for this pipeline, so a mismatch is readable at a glance.
func describeMediaType(mt unsafe.Pointer) string {
	sub, err := getGUID(mt, &mfMTSubtype)
	if err != nil {
		return "(no subtype)"
	}

	desc := describeGUID(sub)
	switch sub {
	case mfVideoFormatNV12:
		desc += " NV12"
	case mfVideoFormatH264:
		desc += " H.264"
	}

	if size, err := getUINT64(mt, &mfMTFrameSize); err == nil {
		desc += fmt.Sprintf(" %dx%d", uint32(size>>32), uint32(size))
	}
	if rate, err := getUINT64(mt, &mfMTFrameRate); err == nil {
		desc += fmt.Sprintf(" @%d/%d fps", uint32(rate>>32), uint32(rate))
	}
	if stride, err := getUINT32(mt, &mfMTDefaultStride); err == nil {
		desc += fmt.Sprintf(" stride=%d", stride)
	}
	if profile, err := getUINT32(mt, &mfMTMpeg2Profile); err == nil {
		desc += fmt.Sprintf(" profile=%d", profile)
	}
	return desc
}

func describeGUID(g guid) string {
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		g.Data1, g.Data2, g.Data3,
		g.Data4[0], g.Data4[1], g.Data4[2], g.Data4[3],
		g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}
