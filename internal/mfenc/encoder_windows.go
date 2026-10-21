//go:build windows && amd64

package mfenc

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Config describes the stream an Encoder produces. Width and Height must both
// be even: NV12 addresses chroma in 2x2 blocks, so an odd dimension has no
// representation in the only format the encoder accepts.
type Config struct {
	Width   int
	Height  int
	FPS     int
	Bitrate int // target bit rate in bits per second
	// KeyframeEvery is the GOP length in seconds. A keyframe costs several
	// times a predicted frame, but it is what a client that has just connected
	// -- or just lost its decoder -- needs before it can show anything.
	KeyframeEvery int
	// Profile selects the H.264 profile; 0 takes the default, which is the
	// baseline profile because it is the one every decoder understands.
	Profile int
	// RateControl selects how the encoder spends its bits; 0 takes this
	// package's default.
	RateControl RateControl
}

// RateControl is one of the modes CODECAPI_AVEncCommonRateControlMode accepts,
// plus a zero value meaning "whatever this package thinks is best".
//
// It is a distinct type rather than the codec API's own numbering because the
// two disagree: CBR is 0 there, which would make an unset field mean the
// opposite of unset.
type RateControl int32

const (
	RateControlDefault RateControl = iota
	// RateControlCBR aims at a constant bit rate. Encoders generally implement
	// it as a ceiling rather than by padding, so it costs little on a still
	// screen, but its rate controller looks ahead and that look-ahead is
	// latency.
	RateControlCBR
	// RateControlVBR targets an average and lets individual frames vary.
	RateControlVBR
	// RateControlQuality aims at a quality level and lets the bit rate fall
	// where it may. The closest fit for screen content: a still screen costs
	// almost nothing and a busy one is not throttled into smearing.
	RateControlQuality
	// RateControlLowDelayVBR is VBR tuned for interactive latency.
	RateControlLowDelayVBR
)

func (r RateControl) codecAPIValue() (uint32, bool) {
	switch r {
	case RateControlCBR:
		return avEncRateControlCBR, true
	case RateControlVBR:
		return avEncRateControlVBR, true
	case RateControlQuality:
		return avEncRateControlQuality, true
	case RateControlLowDelayVBR:
		return avEncRateControlLowDelayVBR, true
	}
	return 0, false
}

func (r RateControl) String() string {
	switch r {
	case RateControlDefault:
		return "default"
	case RateControlCBR:
		return "CBR"
	case RateControlVBR:
		return "VBR"
	case RateControlQuality:
		return "quality"
	case RateControlLowDelayVBR:
		return "low-delay VBR"
	}
	return fmt.Sprintf("RateControl(%d)", int32(r))
}

// CodecSetting records how the encoder answered one optional setting.
//
// These are reported rather than checked because none of them is a requirement:
// an encoder that does not implement a property still encodes. But "applied"
// and "silently ignored" look identical from the outside, and the difference
// decides whether a latency or rate-control expectation holds -- which is worth
// a line of output on a machine nobody can debug directly.
// The two points a codec property can be applied at. Which one a property
// took is the whole point of applying them twice.
const (
	stageBeforeTypes = "before types"
	stageAfterTypes  = "after types"
)

type CodecSetting struct {
	Name string
	// Stage says when the setting was applied, for the properties that are
	// applied more than once. Empty means it was applied once, after the media
	// types were negotiated.
	Stage string
	// Supported is what ICodecAPI::IsSupported said, which is the encoder
	// declaring it knows the property at all.
	Supported bool
	// Err is what SetValue said. Non-nil means the value did not take.
	Err error
}

func (s CodecSetting) String() string {
	what := s.Name
	if s.Stage != "" {
		what = s.Stage + " " + what
	}
	// Both are shown when both are known. "The encoder does not know this
	// property" and "the encoder knows it and refused this value" are different
	// problems, and reporting only one of them hides the other.
	switch {
	case !s.Supported && s.Err != nil:
		return fmt.Sprintf("%s: not supported (%v)", what, s.Err)
	case !s.Supported:
		return what + ": not supported"
	case s.Err != nil:
		return fmt.Sprintf("%s: %v", what, s.Err)
	default:
		return what + ": ok"
	}
}

// maxAccessUnitsPerFrame bounds the per-frame drain. One frame produces one
// access unit in every case this encoder is asked for; the allowance is for a
// future configuration with B-frames, where a single input can release several,
// and for the slight overshoot a stream change can cause.
const maxAccessUnitsPerFrame = 64

func (c Config) withDefaults() Config {
	if c.FPS <= 0 {
		c.FPS = 15
	}
	if c.Bitrate <= 0 {
		c.Bitrate = 4_000_000
	}
	if c.KeyframeEvery <= 0 {
		// One second, not the four a video file would use.
		//
		// This encoder will not force a keyframe on request -- the property is
		// advertised and every attempt to set it answers E_NOTIMPL -- so the
		// GOP is the only thing that decides when a viewer that has just
		// connected gets a picture. At four seconds it sits looking at nothing
		// for four seconds. One second costs a keyframe every fifteen frames,
		// which the measurements put at around 88 kbps in total: not free, and
		// far cheaper than being unusable for the first four seconds.
		c.KeyframeEvery = 1
	}
	if c.Profile == 0 {
		c.Profile = profileBaseline
	}
	return c
}

// Encoder is a configured H.264 encoder MFT.
//
// All Media Foundation calls happen on one dedicated OS thread that the encoder
// locks for its whole life. COM is initialised per thread, and an MFT is not
// obliged to tolerate being called from a thread that never joined an
// apartment, so the thread is pinned rather than borrowing whichever one the
// capture loop happens to be scheduled on. Callers talk to it through a
// synchronous request channel and never see the thread at all.
type Encoder struct {
	cfg Config

	reqs chan encodeReq
	done chan struct{}

	// settings is written once by run, before it signals readiness, and only
	// read afterwards. The channel handoff is the happens-before edge, so no
	// lock is needed.
	settings []CodecSetting

	closeOnce sync.Once
}

type encodeReq struct {
	rgba          []byte
	rgbaStride    int
	forceKeyframe bool
	reply         chan encodeResp
}

type encodeResp struct {
	annexB []byte
	err    error
}

// New creates and configures the encoder, failing if the machine has no H.264
// encoder MFT or will not accept this resolution.
func New(cfg Config) (*Encoder, error) {
	cfg = cfg.withDefaults()
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("invalid frame size %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width%2 != 0 || cfg.Height%2 != 0 {
		return nil, fmt.Errorf("frame size %dx%d is odd; NV12 needs even dimensions", cfg.Width, cfg.Height)
	}

	e := &Encoder{
		cfg:  cfg,
		reqs: make(chan encodeReq),
		done: make(chan struct{}),
	}

	ready := make(chan error, 1)
	go e.run(ready)

	if err := <-ready; err != nil {
		return nil, err
	}
	return e, nil
}

// Size reports the encoded frame size.
func (e *Encoder) Size() (int, int) { return e.cfg.Width, e.cfg.Height }

// Settings reports how the encoder answered each optional setting it was
// configured with. It is valid once New has returned, and is meant for a
// startup log line: a property the encoder ignores is otherwise invisible until
// it shows up as lag or as a bit rate nobody asked for.
func (e *Encoder) Settings() []CodecSetting { return e.settings }

// Encode converts one RGBA frame and returns the H.264 access unit in Annex B
// form. The result is empty when the encoder is still priming and has nothing
// to emit yet, which is normal for the first frame or two.
//
// forceKeyframe asks for an IDR, which is what a client needs when it has just
// connected or its decoder has been reset.
func (e *Encoder) Encode(rgba []byte, rgbaStride int, forceKeyframe bool) ([]byte, error) {
	reply := make(chan encodeResp, 1)
	select {
	case e.reqs <- encodeReq{rgba: rgba, rgbaStride: rgbaStride, forceKeyframe: forceKeyframe, reply: reply}:
	case <-e.done:
		return nil, fmt.Errorf("encoder is closed")
	}

	select {
	case r := <-reply:
		return r.annexB, r.err
	case <-e.done:
		return nil, fmt.Errorf("encoder is closed")
	}
}

// Close releases the encoder and its thread. It is safe to call twice.
func (e *Encoder) Close() {
	e.closeOnce.Do(func() { close(e.done) })
}

// run owns the encoder MFT for the lifetime of the Encoder, on a locked thread.
func (e *Encoder) run(ready chan<- error) {
	// The thread must not migrate: COM is per thread, and every pointer held
	// below belongs to the apartment this thread joins.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	uninitialize, err := coInitialize()
	if err != nil {
		ready <- err
		return
	}
	defer uninitialize()

	shutdown, err := mfStartup()
	if err != nil {
		ready <- err
		return
	}
	defer shutdown()

	s, err := newSession(e.cfg)
	if err != nil {
		ready <- err
		return
	}
	defer s.close()

	// Published before the ready signal and read after it; the channel carries
	// the write.
	e.settings = s.settings
	ready <- nil

	for {
		select {
		case <-e.done:
			return
		case req := <-e.reqs:
			out, err := s.encode(req.rgba, req.rgbaStride, req.forceKeyframe)
			req.reply <- encodeResp{annexB: out, err: err}
		}
	}
}

// defaultRateControl is what a Config that does not name one gets.
//
// Settled on the probe's numbers rather than on the names. Over the same clip
// and the same target, the four modes came out at:
//
//	CBR              key 6950 B   delta 1609 B   207 kbps
//	VBR              key 6953 B   delta 1596 B   206 kbps
//	quality          key 6947 B   delta 1596 B   206 kbps
//	low-delay VBR    key 4659 B   delta  452 B    65 kbps
//
// Low-delay VBR spends a third of the bits for deltas three and a half times
// smaller. The other three are not choosing to spend those bits on quality --
// the content is a nearly still screen, and a rate control that targets an
// average fills with padding when there is nothing to encode. A remote desktop
// is exactly that content nearly all of the time, so the mode that does not
// pad is the one to run.
//
// "Low delay" in the name is a second reason: it is the mode least inclined to
// hold frames back, and the pipeline depth is what a viewer feels.
const defaultRateControl = RateControlLowDelayVBR

// defaultQuality is the quality level asked for in quality rate-control mode,
// as a percentage.
const defaultQuality = 90

// codecProperty is one ICodecAPI property and the value to ask it for.
type codecProperty struct {
	name string
	key  *guid
	val  uint32
	// isBool marks a property whose value is a VARIANT_BOOL rather than a
	// DWORD. The encoder answers the wrong type with E_NOTIMPL, which reads as
	// "not implemented" and is not.
	isBool bool
}

// apply sets the property, in whichever type it takes.
func (c *codecAPI) apply(p codecProperty) error {
	if p.isBool {
		return c.setBOOL(p.key, p.val != 0)
	}
	return c.setUINT32(p.key, p.val)
}

// session holds the transform and the buffers it is fed, so that a frame costs
// no allocation beyond the returned access unit.
type session struct {
	cfg       Config
	transform unsafe.Pointer
	codec     *codecAPI

	// settings records how the encoder answered each optional setting. They are
	// diagnostic: nothing here is required, but a silently ignored property and
	// an applied one look identical from the outside.
	settings []CodecSetting

	// forceKeyframeErr is the first refusal seen from the per-frame keyframe
	// request, which cannot be tried at configuration time and so is not in
	// settings until a frame has actually asked for one.
	forceKeyframeErr error

	stride int
	inSize int

	inSample unsafe.Pointer
	inBuffer unsafe.Pointer
	inPtr    unsafe.Pointer

	outProvidesSamples bool
	outSample          unsafe.Pointer
	outBuffer          unsafe.Pointer

	frameDuration int64 // 100 ns units, the unit Media Foundation works in
	pts           int64

	// dropped counts frames the encoder would not take because it had not been
	// drained. It is a diagnostic: a steady rate of them means the caller is
	// feeding the encoder faster than it encodes, and tells the probe's
	// measurement apart from one where every frame was accepted and the
	// encoder simply took its time about answering.
	dropped int
}

// newSession creates and configures a transform. It is the production path --
// the probe calls it directly rather than through Encoder, so that what the
// probe measures is what the server will run.
func newSession(cfg Config) (*session, error) {
	transform, err := coCreateInstance(&clsidH264Encoder, &iidIMFTransform)
	if err != nil {
		return nil, fmt.Errorf("H.264 encoder MFT unavailable: %w", err)
	}
	return newSessionWith(transform, cfg)
}

// newSessionWith configures an already-created transform.
//
// It takes ownership of transform in every case, including failure: the caller
// hands it over and does not look at it again. Splitting this out of
// newSession is what lets a probe create a transform the ordinary way -- by
// activating one of the encoders MFTEnumEx found -- and then run it through the
// exact configuration path the server uses. Probing a differently-built
// pipeline would answer a question about the probe.
func newSessionWith(transform unsafe.Pointer, cfg Config) (*session, error) {
	s := &session{
		cfg:           cfg,
		transform:     transform,
		stride:        NV12Stride(cfg.Width),
		frameDuration: int64(10_000_000 / cfg.FPS),
	}
	if err := s.configure(); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) configure() error {
	// The codec API settings are applied twice, before and after the media types
	// are negotiated, and both results are recorded.
	//
	// This is not belt and braces. Several of these properties decide the shape
	// of the pipeline -- the latency it runs at, whether B-frames are used, how
	// the GOP is cut -- and those are read while the types are being negotiated.
	// Setting them afterwards is accepted, stored, and ignored, which is
	// indistinguishable from working unless something measures the result. Doing
	// both costs a handful of calls and takes the ordering question off the
	// table; the table says which stage each one actually took.
	if err := s.openCodecAPI(); err != nil {
		s.settings = append(s.settings, CodecSetting{Name: "ICodecAPI", Err: err})
	}
	// MF_LOW_LATENCY has three documented places to live -- the transform's own
	// attribute store, the input type, and the output type -- and the
	// documentation does not say which one an encoder reads. All three are set,
	// for the same reason all three are: guessing right costs one call, and
	// guessing wrong costs a viewer four hundred milliseconds on every frame,
	// which is not a trade worth making to save two.
	s.setTransformLowLatency()

	s.applyCodecSettings(stageBeforeTypes)

	// The output type has to go first, and this is not a free choice: an
	// encoder MFT will not enumerate its input types until an output type is
	// set, and answers GetInputAvailableType with MF_E_TRANSFORM_TYPE_NOT_SET
	// until then. (Set the input type first and the call fails with a code that
	// says nothing about ordering -- which is exactly how the first machine
	// this ran on refused to configure.)
	if err := s.setOutputType(); err != nil {
		return err
	}
	if err := s.setInputType(); err != nil {
		return err
	}
	if err := s.readStreamInfo(); err != nil {
		return err
	}

	s.applyCodecSettings(stageAfterTypes)
	if err := s.prepareInput(); err != nil {
		return err
	}

	// MFTs are allowed to reject input before the stream is started, so this
	// is not optional bookkeeping.
	comCall(s.transform, vtblProcessMessage, mftMessageNotifyBeginStreaming, 0)
	comCall(s.transform, vtblProcessMessage, mftMessageNotifyStartOfStream, 0)
	return nil
}

// setInputType declares NV12 at the capture resolution, falling back to an
// NV12 type the encoder offers itself.
//
// This runs after the output type is set, which is what makes the encoder
// willing to talk about inputs at all.
func (s *session) setInputType() error {
	mt, err := createMediaType()
	if err != nil {
		return err
	}
	defer release(&mt)

	if err := fillInputType(mt, s.cfg, s.stride); err != nil {
		return err
	}

	first := check("SetInputType", comCall(s.transform, vtblSetInputType, 0, uintptr(mt), 0))
	if first == nil {
		return nil
	}

	// The encoder may prefer a type it offered, so take one -- but only an NV12
	// one. The colour conversion produces NV12 and nothing else, and accepting
	// a different format here would mean handing over a buffer in a layout the
	// pixels are not in.
	for i := 0; i <= 32; i++ {
		var avail unsafe.Pointer
		r := comCall(s.transform, vtblGetInputAvailableType, 0, uintptr(i),
			uintptr(unsafe.Pointer(&avail)))
		if hr(r).failed() {
			break
		}
		sub, gerr := getGUID(avail, &mfMTSubtype)
		ok := gerr == nil && sub == mfVideoFormatNV12
		if ok {
			// An offered type need not carry a stride, and the stride is the
			// one thing it cannot imply: it describes the caller's buffer.
			ok = setUINT32(avail, &mfMTDefaultStride, uint32(s.stride)) == nil
			// Same reasoning as the constructed type above: the low-latency
			// request belongs on the type actually being used, whichever way
			// that type was arrived at.
			_ = setUINT32(avail, &mfLowLatency, 1)
		}
		if ok {
			ok = !hr(comCall(s.transform, vtblSetInputType, 0, uintptr(avail), 0)).failed()
		}
		release(&avail)
		if ok {
			return nil
		}
	}
	return fmt.Errorf("%w (NV12 %dx%d stride %d was refused, and the encoder offers no NV12 of its own)",
		first, s.cfg.Width, s.cfg.Height, s.stride)
}

// fillInputType applies the frame layout to an input media type. The stride
// carries the one piece of layout a media type cannot otherwise express: how to
// walk a linear buffer, which is what the encoder is handed.
func fillInputType(mt unsafe.Pointer, cfg Config, stride int) error {
	if err := setGUID(mt, &mfMTMajorType, &mfMediaTypeVideo); err != nil {
		return err
	}
	if err := setGUID(mt, &mfMTSubtype, &mfVideoFormatNV12); err != nil {
		return err
	}
	if err := mfSetSize(mt, &mfMTFrameSize, uint32(cfg.Width), uint32(cfg.Height)); err != nil {
		return err
	}
	if err := mfSetRatio(mt, &mfMTFrameRate, uint32(cfg.FPS), 1); err != nil {
		return err
	}
	if err := mfSetRatio(mt, &mfMTPixelAspectRatio, 1, 1); err != nil {
		return err
	}
	if err := setUINT32(mt, &mfMTInterlaceMode, mfVideoInterlaceProgressive); err != nil {
		return err
	}
	if err := setUINT32(mt, &mfMTDefaultStride, uint32(stride)); err != nil {
		return err
	}

	// MF_LOW_LATENCY goes on both media types, not just the output one.
	//
	// The documentation describes it as an attribute of the *stream*, reached
	// either from the transform's attribute store or from a media type, and is
	// not specific about which of the two an encoder reads. The encoder here
	// kept the copy on the output type and still held twelve frames back, so
	// the copy it acts on -- if it acts on either -- is the one on the input.
	// Setting both costs one call and removes the ambiguity; the probe's lag
	// column says whether it was the answer, since the same measurement without
	// it read twelve.
	_ = setUINT32(mt, &mfLowLatency, 1)
	return nil
}

// setOutputType asks for an H.264 stream with the requested rate control.
//
// The type is built by hand first, because that is the shortest path and the
// one that carries our settings. An encoder is entitled to refuse a type it did
// not offer, though, so a refusal falls back to asking it what it will accept
// and filling the gaps from our own configuration.
func (s *session) setOutputType() error {
	mt, err := createMediaType()
	if err != nil {
		return err
	}
	defer release(&mt)

	if err := fillOutputType(mt, s.cfg); err != nil {
		return err
	}
	first := check("SetOutputType", comCall(s.transform, vtblSetOutputType, 0, uintptr(mt), 0))
	if first == nil {
		return nil
	}

	for i := 0; i <= 32; i++ {
		var avail unsafe.Pointer
		r := comCall(s.transform, vtblGetOutputAvailableType, 0, uintptr(i),
			uintptr(unsafe.Pointer(&avail)))
		if hr(r).failed() {
			break
		}
		sub, gerr := getGUID(avail, &mfMTSubtype)
		ok := gerr == nil && sub == mfVideoFormatH264
		if ok {
			// An offered type may leave out attributes it has a default for,
			// and may refuse to have some set on it at all. Either way it is
			// worth offering back as it stands, so the result of the fill is
			// deliberately not what decides the attempt.
			_ = fillOutputType(avail, s.cfg)
			ok = !hr(comCall(s.transform, vtblSetOutputType, 0, uintptr(avail), 0)).failed()
		}
		release(&avail)
		if ok {
			return nil
		}
	}
	return fmt.Errorf("%w (and none of the encoder's own output types were usable)", first)
}

// fillOutputType applies the stream settings to a media type.
func fillOutputType(mt unsafe.Pointer, cfg Config) error {
	if err := setGUID(mt, &mfMTMajorType, &mfMediaTypeVideo); err != nil {
		return err
	}
	if err := setGUID(mt, &mfMTSubtype, &mfVideoFormatH264); err != nil {
		return err
	}
	if err := mfSetSize(mt, &mfMTFrameSize, uint32(cfg.Width), uint32(cfg.Height)); err != nil {
		return err
	}
	if err := mfSetRatio(mt, &mfMTFrameRate, uint32(cfg.FPS), 1); err != nil {
		return err
	}
	if err := mfSetRatio(mt, &mfMTPixelAspectRatio, 1, 1); err != nil {
		return err
	}
	if err := setUINT32(mt, &mfMTInterlaceMode, mfVideoInterlaceProgressive); err != nil {
		return err
	}
	if err := setUINT32(mt, &mfMTAvgBitrate, uint32(cfg.Bitrate)); err != nil {
		return err
	}
	if err := setUINT32(mt, &mfMTMpeg2Profile, uint32(cfg.Profile)); err != nil {
		return err
	}

	// MF_LOW_LATENCY asks the encoder not to build up a pipeline of frames it
	// would have to fill before emitting anything. It is a request: an encoder
	// that does not know the attribute may refuse it outright, and the type is
	// still perfectly usable without it, so the result is not checked here.
	// Whether it survived is read back off the negotiated type afterwards.
	_ = setUINT32(mt, &mfLowLatency, 1)
	return nil
}

// applyCodecSettings tunes rate control, latency and the GOP.
//
// Every one of these is a preference rather than a requirement: an encoder that
// does not implement a property still encodes, so a refusal is recorded rather
// than treated as a failure. What each one answered lands in s.settings, because
// a property that was accepted and one that was quietly ignored leave the same
// trace otherwise -- and the difference is what decides whether the latency this
// pipeline is built around actually holds.
func (s *session) applyCodecSettings(stage string) {
	if s.codec == nil {
		return
	}
	codec := s.codec

	mode := s.cfg.RateControl
	if mode == RateControlDefault {
		mode = defaultRateControl
	}

	settings := []codecProperty{
		{name: "mean bit rate", key: &codecAPIMeanBitRate, val: uint32(s.cfg.Bitrate)},
		// Low latency is the whole point here: without it the encoder is free
		// to hold frames back to fill its pipeline, which a viewer sees as lag.
		{name: "low latency", key: &codecAPILowLatency, val: 1, isBool: true},
		// No B-frames. They are reordered, and reordering is latency; for a
		// remote desktop that trade is never worth it.
		{name: "B pictures", key: &codecAPIBPictureCount, val: 0},
		{name: "GOP size", key: &codecAPIGOPSize, val: uint32(s.cfg.FPS * s.cfg.KeyframeEvery)},
		{name: "CABAC", key: &codecAPICABACEnable, val: 1, isBool: true},
		{name: "quality vs speed", key: &codecAPIQualityVsSpeed, val: 50},
	}

	// The mode goes first: several of the settings above only mean anything
	// relative to it.
	if v, ok := mode.codecAPIValue(); ok {
		settings = append([]codecProperty{
			{name: "rate control mode (" + mode.String() + ")", key: &codecAPIRateControlMode, val: v},
		}, settings...)
	} else {
		s.settings = append(s.settings, CodecSetting{Name: "rate control mode"})
	}
	// The quality level is the one knob that only exists in quality mode.
	if mode == RateControlQuality {
		settings = append(settings, codecProperty{name: "quality level", key: &codecAPICommonQuality, val: defaultQuality})
	}

	for _, set := range settings {
		s.settings = append(s.settings, CodecSetting{
			Name:      set.name,
			Stage:     stage,
			Supported: codec.isSupported(set.key),
			Err:       codec.apply(set),
		})
	}

	// The reading of the output type is only meaningful once there is one, and
	// the keyframe property is only reported once, since it is not set here at
	// all: a keyframe is asked for per frame.
	if stage != stageAfterTypes {
		return
	}
	s.settings = append(s.settings, s.lowLatencyAttribute(vtblGetInputCurrentType, "input"))
	s.settings = append(s.settings, s.lowLatencyAttribute(vtblGetOutputCurrentType, "output"))
	s.settings = append(s.settings, CodecSetting{
		Name:      "force key frame",
		Supported: codec.isSupported(&codecAPIForceKeyFrame),
	})
}

// openCodecAPI queries the transform for ICodecAPI once and keeps it. Every
// setting is optional, so a transform that does not expose the interface is a
// note in the settings list rather than a configuration failure.
func (s *session) openCodecAPI() error {
	codec, err := newCodecAPI(s.transform)
	if err != nil {
		return err
	}
	s.codec = codec
	return nil
}

// settingsReport is the settings as they stand now, including what has been
// learned about the per-frame properties since configuration.
func (s *session) settingsReport() []CodecSetting {
	out := append([]CodecSetting(nil), s.settings...)
	if s.forceKeyframeErr != nil {
		// Supported is set so that the error is what gets printed: the point of
		// this row is the HRESULT, not the fact that the request failed.
		out = append(out, CodecSetting{
			Name:      "force key frame (at encode time)",
			Supported: true,
			Err:       s.forceKeyframeErr,
		})
	}
	return out
}

// lowLatencyAttribute reads MF_LOW_LATENCY back off the output type the encoder
// settled on.
//
// The attribute is a request, not a requirement, so the set succeeding says
// nothing about whether the encoder kept it. Reading it back is the only answer
// that means anything, and it is worth having: an encoder that ignores this is
// one that will buffer frames, and buffered frames are lag.
// setTransformLowLatency asks for low latency on the transform's own attribute
// store, which is the one of the three possible locations that is not a media
// type; the other two are set where the types are built.
//
// The failure is recorded rather than returned: a transform whose store cannot
// be reached is still configurable through its media types, and refusing to
// start over a setting that is only a request would be the wrong trade.
func (s *session) setTransformLowLatency() {
	attrs, err := getAttributes(s.transform)
	if err != nil {
		s.settings = append(s.settings, CodecSetting{Name: "MF_LOW_LATENCY (transform)", Err: err})
		return
	}
	defer release(&attrs)

	setting := CodecSetting{Name: "MF_LOW_LATENCY (transform)"}
	if err := setUINT32(attrs, &mfLowLatency, 1); err != nil {
		setting.Err = err
	} else if v, err := getUINT32(attrs, &mfLowLatency); err != nil {
		setting.Err = fmt.Errorf("the transform did not keep it")
	} else {
		setting.Supported = true
		if v == 0 {
			setting.Err = fmt.Errorf("the transform set it back to 0")
		}
	}
	s.settings = append(s.settings, setting)
}

// lowLatencyAttribute reads MF_LOW_LATENCY back off the negotiated type for one
// stream. slot is the GetInputCurrentType or GetOutputCurrentType vtable slot;
// what names the stream in the resulting setting.
//
// It is read back rather than assumed because the setting is a request, and an
// encoder that does not want it is free to drop it -- silently, since setting
// an attribute on a media type always succeeds whatever the value.
func (s *session) lowLatencyAttribute(slot uintptr, what string) CodecSetting {
	setting := CodecSetting{Name: "MF_LOW_LATENCY (" + what + " type)"}

	var mt unsafe.Pointer
	if h := hr(comCall(s.transform, slot, 0, uintptr(unsafe.Pointer(&mt)))); h.failed() {
		setting.Err = h
		return setting
	}
	defer release(&mt)

	v, err := getUINT32(mt, &mfLowLatency)
	if err != nil {
		// An attribute that is not there reads as an error, which here is the
		// same news as a zero: the encoder did not keep it.
		setting.Err = fmt.Errorf("the encoder did not keep it")
		return setting
	}
	setting.Supported = true
	if v == 0 {
		setting.Err = fmt.Errorf("the encoder set it back to 0")
	}
	return setting
}

// prepareInput allocates the single input buffer every frame is converted into,
// and locks it for the life of the session: the encoder reads it on each
// ProcessInput, so it must stay mapped.
func (s *session) prepareInput() error {
	s.inSize = NV12Size(s.stride, s.cfg.Height)

	var err error
	if s.inSample, s.inBuffer, err = createSampleWithBuffer(s.inSize); err != nil {
		return err
	}

	p, maxLen, _, err := lockBuffer(s.inBuffer)
	if err != nil {
		return err
	}
	if p == nil || int(maxLen) < s.inSize {
		return fmt.Errorf("input buffer is %d bytes, need %d", maxLen, s.inSize)
	}
	s.inPtr = p

	// The length is set once: the buffer is filled to the same size every
	// frame, and ProcessInput reads exactly that much.
	return check("IMFMediaBuffer::SetCurrentLength",
		comCall(s.inBuffer, vtblBufferSetLength, uintptr(s.inSize)))
}

// readStreamInfo records what the encoder wants for output buffers, including
// whether it allocates them itself.
func (s *session) readStreamInfo() error {
	var info mftOutputStreamInfo
	r := comCall(s.transform, vtblGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info)))
	if err := check("GetOutputStreamInfo", r); err != nil {
		return err
	}
	s.outProvidesSamples = info.Flags&mftOutputStreamProvidesSamples != 0
	if s.outProvidesSamples {
		return nil
	}

	// The size the encoder reports is its own worst case, which is exactly what
	// a reusable buffer needs. It is also allowed to report nothing, in which
	// case the raw image size is a generous stand-in: an access unit larger
	// than the pixels it describes would not be compression at all.
	size := int(info.Size)
	if size <= 0 {
		size = NV12Size(s.stride, s.cfg.Height)
	}
	var err error
	s.outSample, s.outBuffer, err = createSampleWithBuffer(size)
	if err != nil {
		return err
	}
	return nil
}

// encode converts one frame and returns whatever the encoder emitted.
func (s *session) encode(rgba []byte, rgbaStride int, forceKeyframe bool) ([]byte, error) {
	if rgbaStride <= 0 {
		rgbaStride = s.cfg.Width * 4
	}

	// The conversion writes straight into the encoder's own buffer, so a frame
	// costs one pass over the pixels and no intermediate copy. The slice is
	// over memory Media Foundation owns, not the Go heap; it holds no
	// pointers, so there is nothing in it for the collector to trace.
	dst := unsafe.Slice((*byte)(s.inPtr), s.inSize)
	RGBAtoNV12(dst, s.stride, rgba, rgbaStride, s.cfg.Width, s.cfg.Height)

	if forceKeyframe && s.codec != nil {
		// Kept rather than discarded: a keyframe request the encoder refuses is
		// why a client that just connected sits waiting for the GOP to come
		// round, and that is worth being able to say out loud.
		if err := s.codec.setBOOL(&codecAPIForceKeyFrame, true); err != nil && s.forceKeyframeErr == nil {
			s.forceKeyframeErr = err
		}
	}

	if err := check("IMFSample::SetSampleTime",
		comCall(s.inSample, vtblSampleSetTime, uintptr(s.pts))); err != nil {
		return nil, err
	}
	if err := check("IMFSample::SetSampleDuration",
		comCall(s.inSample, vtblSampleSetDuration, uintptr(s.frameDuration))); err != nil {
		return nil, err
	}
	s.pts += s.frameDuration

	// The call is made once and its result inspected, never retried: a second
	// ProcessInput would hand the encoder the same frame twice.
	h := hr(comCall(s.transform, vtblProcessInput, 0, uintptr(s.inSample), 0))
	switch {
	case h == errNotAccepting:
		// The encoder will not take this frame. Dropping it is the honest
		// response; the next tick brings a fresher frame anyway.
		//
		// Counted, because the count is the difference between two very
		// different stories when the probe reads the result: an encoder that
		// refuses frames is not the same as one that accepts everything and
		// emits late, and from output sizes alone they look identical.
		s.dropped++
		// The drain is still worth doing. An asynchronous encoder announces
		// itself by refusing input while it has output waiting, so returning
		// early here would turn the one thing that identifies it into silent
		// silence: no output, no reason given.
		return s.drain()
	case h.failed():
		return nil, fmt.Errorf("ProcessInput: %w", h)
	}

	return s.drain()
}

// drain pulls every access unit the encoder has ready.
//
// The loop is bounded. A healthy encoder answers MF_E_TRANSFORM_NEED_MORE_INPUT
// as soon as it has nothing left, but one that keeps reporting success with an
// empty sample would otherwise spin here forever, and this runs on the capture
// path: a hang here is a frozen desktop, not a slow frame.
func (s *session) drain() ([]byte, error) {
	var out []byte

	for attempt := 0; attempt < maxAccessUnitsPerFrame; attempt++ {
		sample, err := s.outputSample()
		if err != nil {
			return nil, err
		}

		buf := mftOutputDataBuffer{StreamID: 0, Sample: sample}
		var status uint32
		r := comCall(s.transform, vtblProcessOutput, 0, 1,
			uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&status)))
		h := hr(r)

		switch {
		case h == errNeedMoreInput:
			// The encoder has consumed everything it was given. A sample
			// allocated for the call stays ours and is reused next time.
			s.resetOutput()
			return out, nil

		case h == errStreamChange:
			s.resetOutput()
			if err := s.renegotiate(); err != nil {
				return nil, err
			}
			continue

		case h.failed():
			s.resetOutput()
			return nil, fmt.Errorf("ProcessOutput: %w", h)
		}

		// A sample the encoder made itself belongs to us now, and must be let
		// go once its bytes have been copied out.
		provided := s.outProvidesSamples && buf.Sample != nil
		chunk, err := s.readSample(buf.Sample)
		if provided {
			release(&buf.Sample)
		} else {
			s.resetOutput()
		}
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)

		if buf.Events != nil {
			release(&buf.Events)
		}
	}

	// The bound was reached with the encoder still claiming more output. Take
	// what was produced and let the next frame try again: the caller gets a
	// usable access unit either way, and no frame is lost.
	return out, nil
}

// describeOutput says where the sample for this ProcessOutput was supposed to
// come from, for the error paths. A pointer that turns out not to be a sample
// is only diagnosable next to what it was meant to be.
func (s *session) describeOutput() string {
	switch {
	case s.outProvidesSamples:
		return "a sample the encoder was to allocate"
	case s.outSample == nil:
		return "a sample that had already been released"
	default:
		return "the reusable output sample"
	}
}

// outputSample yields the sample to hand to ProcessOutput: the reusable one
// when the caller must allocate, and nil when the encoder provides its own.
func (s *session) outputSample() (unsafe.Pointer, error) {
	if s.outProvidesSamples {
		return nil, nil
	}
	if s.outSample == nil {
		return nil, fmt.Errorf("no output sample was allocated")
	}
	return s.outSample, nil
}

// resetOutput returns the reusable output sample to an empty state. Its
// allocation is kept: only the length has to go back, or the next
// ProcessOutput would be told there is already a frame's worth of data in it.
func (s *session) resetOutput() {
	if s.outProvidesSamples || s.outBuffer == nil {
		return
	}
	comCall(s.outBuffer, vtblBufferSetLength, 0)
}

// renegotiate handles MF_E_TRANSFORM_STREAM_CHANGE: the encoder has decided its
// output type must change, and it will not accept input again until the new
// type has been accepted.
func (s *session) renegotiate() error {
	for i := 0; ; i++ {
		var mt unsafe.Pointer
		r := comCall(s.transform, vtblGetOutputAvailableType, 0, uintptr(i),
			uintptr(unsafe.Pointer(&mt)))
		if hr(r).failed() {
			return fmt.Errorf("the encoder demands a new output type but offers none: %w", hr(r))
		}
		sub, err := getGUID(mt, &mfMTSubtype)
		done := err == nil && sub == mfVideoFormatH264 &&
			!hr(comCall(s.transform, vtblSetOutputType, 0, uintptr(mt), 0)).failed()
		release(&mt)
		if done {
			break
		}
		if i > 32 {
			return fmt.Errorf("no usable output type among the encoder's offers")
		}
	}

	// A new type can mean a new buffer requirement. The sample and the buffer
	// are released separately because they hold a reference each: AddBuffer
	// took its own when the sample was assembled.
	release(&s.outSample)
	release(&s.outBuffer)
	return s.readStreamInfo()
}

func (s *session) close() {
	if s.transform != nil {
		comCall(s.transform, vtblProcessMessage, mftMessageNotifyEndOfStream, 0)
		comCall(s.transform, vtblProcessMessage, mftMessageCommandFlush, 0)
	}
	if s.inBuffer != nil {
		comCall(s.inBuffer, vtblBufferUnlock)
	}
	s.inPtr = nil
	release(&s.inSample)
	release(&s.inBuffer)
	release(&s.outSample)
	release(&s.outBuffer)
	s.codec.close()
	release(&s.transform)
}

// readSample copies an output sample's bytes out, so that the sample can be
// released or reused immediately instead of being held until the caller is
// finished with the data.
func (s *session) readSample(sample unsafe.Pointer) ([]byte, error) {
	if sample == nil {
		return nil, fmt.Errorf("ProcessOutput reported success on %s without a sample",
			s.describeOutput())
	}

	var buffer unsafe.Pointer
	r := comCall(sample, vtblSampleGetBufferByIndex, 0, uintptr(unsafe.Pointer(&buffer)))
	if err := check("IMFSample::GetBufferByIndex", r); err != nil {
		return nil, err
	}
	if buffer == nil {
		// A success code with a null out-parameter means the pointer handed in
		// was not a sample: the call landed somewhere else entirely and the
		// "success" was whatever happened to be in the return register. Saying
		// so is worth far more than the null dereference that follows, which
		// names neither the pointer nor what it was supposed to be.
		return nil, fmt.Errorf("GetBufferByIndex reported success but returned no buffer: %s was not a sample",
			s.describeOutput())
	}
	defer release(&buffer)

	p, maxLen, length, err := lockBuffer(buffer)
	if err != nil {
		return nil, err
	}
	defer comCall(buffer, vtblBufferUnlock)

	if length == 0 || p == nil {
		return nil, nil
	}
	if length > maxLen {
		return nil, fmt.Errorf("a buffer reports %d bytes in a %d byte allocation", length, maxLen)
	}

	out := make([]byte, length)
	copy(out, unsafe.Slice((*byte)(p), length))
	return out, nil
}
