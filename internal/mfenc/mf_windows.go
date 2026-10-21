//go:build windows && amd64

package mfenc

// Media Foundation plumbing, in the same hand-rolled style as the WASAPI
// capture in server/audio_windows.go: every interface is a bare pointer, every
// call goes through the vtable, and no binding library is involved, because the
// project builds with CGO_ENABLED=0 and depends on nothing but gorilla.
//
// Everything here is amd64-only by design. MFTEnumEx takes a GUID by value, and
// a 16-byte struct is passed by reference under the amd64 ABI but pushed on the
// stack word by word under the 32-bit one; pretending one calling convention is
// the other is exactly the kind of bug that only shows up on someone else's
// machine. The 32-bit build gets a stub that reports the encoder unavailable.

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------------------
// GUIDs
// ---------------------------------------------------------------------------

// guid mirrors the 16-byte in-memory layout of a Windows GUID. The layout rules
// are the same as audio_windows.go's: the three leading fields are little
// endian on Windows and Data4 is stored as written, so none of the constants
// below need byte swapping.
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// mediaTypeGUID builds the GUID of a media type attribute, which always has the
// shape {xxxxxxxx-0000-0010-8000-00AA00389B71} with the attribute's own value
// in the first field.
func mediaTypeGUID(id uint32) guid {
	return guid{id, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
}

var (
	// CLSID_CMSH264EncoderMFT {6CA50344-051A-4DED-9779-A43305165E35} is the
	// H.264 encoder that ships with Windows (mfh264enc.dll). It is registered
	// as a COM object, so it can be created directly rather than through
	// MFTEnumEx -- which is what the probe tries first.
	clsidH264Encoder = guid{
		0x6CA50344, 0x051A, 0x4DED,
		[8]byte{0x97, 0x79, 0xA4, 0x33, 0x05, 0x16, 0x5E, 0x35},
	}
	// IID_IMFTransform {BF94C121-5B05-4E6F-8000-BA598961414D}
	iidIMFTransform = guid{
		0xBF94C121, 0x5B05, 0x4E6F,
		[8]byte{0x80, 0x00, 0xBA, 0x59, 0x89, 0x61, 0x41, 0x4D},
	}
	// IID_ICodecAPI {901DB4C7-31CE-41A2-85DC-8FA0BF41B8DA}
	iidICodecAPI = guid{
		0x901DB4C7, 0x31CE, 0x41A2,
		[8]byte{0x85, 0xDC, 0x8F, 0xA0, 0xBF, 0x41, 0xB8, 0xDA},
	}
	// IID_IMF2DBuffer {E7174C0C-C77A-4E66-9B67-6BE8D0A0F2F9}
	iidIMF2DBuffer = guid{
		0xE7174C0C, 0xC77A, 0x4E66,
		[8]byte{0x9B, 0x67, 0x6B, 0xE8, 0xD0, 0xA0, 0xF2, 0xF9},
	}

	// Major type MFMediaType_Video and the two subtypes this package uses.
	mfMediaTypeVideo  = mediaTypeGUID(0x73646976) // 'vids'
	mfVideoFormatNV12 = mediaTypeGUID(0x3231564E) // 'NV12'
	mfVideoFormatH264 = mediaTypeGUID(0x34363248) // 'H264'

	// Media type attributes.
	mfMTMajorType             = guid{0x48EBA18E, 0xF8C9, 0x4687, [8]byte{0xBF, 0x11, 0x0A, 0x74, 0xC9, 0xF9, 0x6A, 0x8F}}
	mfMTSubtype               = guid{0xF7E34C9A, 0x42E8, 0x4714, [8]byte{0xB7, 0x4B, 0xCB, 0x29, 0xD7, 0x2C, 0x35, 0xE5}}
	mfMTFrameSize             = guid{0x1652C33D, 0xD6B2, 0x4012, [8]byte{0xB8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xA3, 0x7D}}
	mfMTFrameRate             = guid{0xC459A2E8, 0x3D2C, 0x4E44, [8]byte{0xB1, 0x32, 0xFE, 0xE5, 0x15, 0x6C, 0x7B, 0xB0}}
	mfMTPixelAspectRatio      = guid{0xC6376A1E, 0x8D0A, 0x4027, [8]byte{0xBE, 0x45, 0x6D, 0x9A, 0x0A, 0xD3, 0x9B, 0xB6}}
	mfMTInterlaceMode         = guid{0xE2724BB8, 0xE676, 0x4806, [8]byte{0xB4, 0xB2, 0xA8, 0xD6, 0xEF, 0xB4, 0x4C, 0xCD}}
	mfMTDefaultStride         = guid{0x644B4E48, 0x1E02, 0x4516, [8]byte{0xB0, 0xEB, 0xC0, 0x1C, 0xA9, 0xD4, 0x9A, 0xC6}}
	mfMTAvgBitrate            = guid{0x20332624, 0xFB0D, 0x4D9E, [8]byte{0xBD, 0x0D, 0xCB, 0xF6, 0x78, 0x6C, 0x10, 0x2E}}
	mfMTMpeg2Profile          = guid{0xAD76A80B, 0x2D5C, 0x4E0B, [8]byte{0xB3, 0x75, 0x64, 0xE5, 0x20, 0x13, 0x70, 0x36}}
	mfMTMpeg2Level            = guid{0x96F66574, 0x11C5, 0x4015, [8]byte{0x86, 0x66, 0xBF, 0xF5, 0x16, 0x50, 0x17, 0x6E}}
	mfMTFixedSizeSamples      = guid{0xB8EBEFAF, 0xB718, 0x4E04, [8]byte{0xB0, 0xA9, 0x11, 0x67, 0x75, 0xE3, 0x32, 0x1B}}
	mfMTAllSamplesIndependent = guid{0xC9173739, 0x5E56, 0x461C, [8]byte{0xB7, 0x13, 0x46, 0xFB, 0x99, 0x5C, 0xB9, 0x5F}}
	mfMTMinDisplayAperture    = guid{0x0EFE4D4A, 0x3E64, 0x435E, [8]byte{0x91, 0xCC, 0x77, 0x4E, 0x09, 0x6A, 0x0A, 0x1A}}
	mfMTMpegSequenceHeader    = guid{0x1B81BDF0, 0x8FF4, 0x4224, [8]byte{0x9D, 0xCC, 0x95, 0x0F, 0x4D, 0x3F, 0x7C, 0x1F}}
	mfLowLatency              = guid{0x9C27891A, 0xED7A, 0x40E1, [8]byte{0x88, 0xE8, 0xB2, 0x27, 0x27, 0xA0, 0x24, 0xEE}}

	// MF_TRANSFORM_ASYNC is a UINT32 on the transform's own attribute store --
	// not on a media type, which is the trap. Its absence is defined to mean
	// "synchronous", so a wrong GUID here does not fail loudly: it silently
	// reclassifies an asynchronous MFT as a synchronous one, and everything
	// downstream then misreads a refusal as a bug.
	mfTransformAsync = guid{0xF81A699A, 0x649A, 0x497D, [8]byte{0x8C, 0x73, 0x29, 0xF8, 0xFE, 0xD6, 0xAD, 0x7A}}

	// MF_TRANSFORM_ASYNC_UNLOCK is the permission slip an asynchronous MFT
	// insists on before it will accept media types. Media Foundation's own
	// pipeline sets it on the client's behalf, so it is absent from every
	// sample that drives a transform through the pipeline -- and a client that
	// drives one directly, as this package does, has to set it or every
	// SetOutputType comes back MF_E_TRANSFORM_ASYNC_LOCKED.
	mfTransformAsyncUnlock = guid{0xE5666D6B, 0x3422, 0x4EB6, [8]byte{0xA4, 0x21, 0xDA, 0x7D, 0xB1, 0xF8, 0xE2, 0x07}}

	// Codec API properties (ICodecAPI), from strmif.h's codecapi.h.
	codecAPIRateControlMode       = guid{0x1C0608E9, 0x370C, 0x4710, [8]byte{0x8A, 0x58, 0xCB, 0x61, 0x81, 0xC4, 0x24, 0x23}}
	codecAPIMeanBitRate           = guid{0xF7222374, 0x2144, 0x4815, [8]byte{0xB5, 0x50, 0xA3, 0x7F, 0x8E, 0x12, 0xEE, 0x52}}
	codecAPICommonQuality         = guid{0xFCBF57A3, 0x7EA5, 0x4B0C, [8]byte{0x96, 0x44, 0x69, 0xB4, 0x0C, 0x39, 0xC3, 0x91}}
	codecAPILowLatency            = guid{0x9D3ECD55, 0x89E8, 0x490A, [8]byte{0x97, 0x0A, 0x0C, 0x8F, 0x79, 0xD3, 0x6A, 0x6D}}
	codecAPIGOPSize               = guid{0x95F31B26, 0x95A4, 0x41AA, [8]byte{0x93, 0x03, 0x24, 0x6A, 0x7F, 0xC6, 0xEE, 0xF1}}
	codecAPIForceKeyFrame         = guid{0x398C1B98, 0x8353, 0x4283, [8]byte{0x9E, 0xF2, 0x8F, 0x26, 0x5D, 0x26, 0x03, 0x45}}
	codecAPIBPictureCount         = guid{0x8D390AAC, 0xDC5C, 0x4200, [8]byte{0xB5, 0x7F, 0x81, 0x4D, 0x04, 0xBA, 0xBA, 0xB2}}
	codecAPICABACEnable           = guid{0xEE5A4A5B, 0x3DB0, 0x4916, [8]byte{0x8D, 0x3D, 0xB6, 0xBD, 0xF4, 0xB1, 0xFF, 0x5A}}
	codecAPIQualityVsSpeed        = guid{0x98332DF8, 0x03CD, 0x476B, [8]byte{0x89, 0xFA, 0x3F, 0x9E, 0x44, 0x2D, 0xEC, 0x9F}}
	codecAPIVideoEncodeComplexity = guid{0x2C0C8B1B, 0x9C53, 0x4D8B, [8]byte{0xB3, 0x0B, 0x2A, 0x5C, 0x0D, 0x11, 0x9B, 0x0F}}
)

// Keep the attributes the encoder does not set yet, and the alternate
// interfaces it does not query. They are the documentation of what this
// negotiation involves, and the probe prints several of them when it reports
// what a machine's encoder will accept.
var (
	_ = mfMTMinDisplayAperture
	_ = mfMTFixedSizeSamples
	_ = mfMTAllSamplesIndependent
	_ = codecAPIVideoEncodeComplexity
	_ = iidIMF2DBuffer
)

// ---------------------------------------------------------------------------
// Struct layouts
// ---------------------------------------------------------------------------

// mftOutputStreamInfo mirrors MFT_OUTPUT_STREAM_INFO.
type mftOutputStreamInfo struct {
	Flags     uint32
	Size      uint32
	Alignment uint32
}

// mftInputStreamInfo mirrors MFT_INPUT_STREAM_INFO.
type mftInputStreamInfo struct {
	MaxLatency   int64
	Flags        uint32
	Size         uint32
	MaxLookahead uint32
	Alignment    uint32
}

// mftOutputDataBuffer mirrors MFT_OUTPUT_DATA_BUFFER. dwStreamID and dwStatus
// are DWORDs separated by pointers, so each needs explicit padding to keep the
// pointers 8-byte aligned -- the C struct relies on the compiler inserting the
// same bytes.
type mftOutputDataBuffer struct {
	StreamID uint32
	_        uint32
	Sample   unsafe.Pointer
	Status   uint32
	_        uint32
	Events   unsafe.Pointer
}

// ---------------------------------------------------------------------------
// Library bindings
// ---------------------------------------------------------------------------

var (
	mfplat = syscall.NewLazyDLL("mfplat.dll")
	ole32  = syscall.NewLazyDLL("ole32.dll")

	procMFStartup            = mfplat.NewProc("MFStartup")
	procMFShutdown           = mfplat.NewProc("MFShutdown")
	procMFCreateMediaType    = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample       = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFSetAttributeSize   = mfplat.NewProc("MFSetAttributeSize")
	procMFSetAttributeRatio  = mfplat.NewProc("MFSetAttributeRatio")
	procMFTEnumEx            = mfplat.NewProc("MFTEnumEx")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
)

// MF_VERSION: MF_SDK_VERSION << 16 | MF_API_VERSION.
const mfVersion = 0x00020070

// MFSTARTUP_NOSOCKET skips the network stack Media Foundation would otherwise
// bring up; this process only ever encodes into memory.
const mfStartupNoSocket = 0x1

// COINIT_MULTITHREADED, CLSCTX_ALL.
const (
	coinitMultithreaded = 0x0
	clctxAll            = 0x17
)

// ---------------------------------------------------------------------------
// Vtable slots
// ---------------------------------------------------------------------------

// Every interface below derives from IUnknown, whose three methods occupy slots
// 0-2, so each interface's own methods start at 3 in declaration order. These
// numbers are the whole interface contract: a slot that is off by one calls a
// different function with the same arguments, which is why the probe checks
// every call's HRESULT rather than trusting the result.
const (
	vtblQueryInterface = 0
	vtblAddRef         = 1
	vtblRelease        = 2
)

// IMFAttributes. IMFMediaType, IMFSample and IMFActivate all derive from it, so
// their own methods start after CopyAllItems.
const (
	vtblGetUINT32          = 7
	vtblGetUINT64          = 8
	vtblGetGUID            = 10
	vtblGetString          = 12
	vtblGetAllocatedString = 13
	vtblGetBlobSize        = 14
	vtblGetBlob            = 15
	vtblGetAllocatedBlob   = 16
	vtblSetUINT32          = 21
	vtblSetUINT64          = 22
	vtblSetGUID            = 24
	vtblSetString          = 25
	vtblGetCount           = 30

	// IMFActivate, which also derives from IMFAttributes. The numbering is the
	// check that the attribute set above ends where it should: ActivateObject
	// is the first method of its own, so it lands on 33 only if IMFAttributes
	// really does have thirty.
	vtblActivateObject = 33
)

// IMFTransform.
const (
	vtblGetInputStreamInfo     = 6
	vtblGetOutputStreamInfo    = 7
	vtblGetAttributes          = 8
	vtblGetInputAvailableType  = 13
	vtblGetOutputAvailableType = 14
	vtblSetInputType           = 15
	vtblSetOutputType          = 16
	vtblGetInputCurrentType    = 17
	vtblGetOutputCurrentType   = 18
	vtblGetInputStatus         = 19
	vtblProcessMessage         = 23
	vtblProcessInput           = 24
	vtblProcessOutput          = 25

	// IMFSample, which derives from IMFAttributes.
	vtblSampleSetTime          = 36
	vtblSampleSetDuration      = 38
	vtblSampleGetBufferByIndex = 40
	vtblSampleAddBuffer        = 42
	vtblSampleGetTotalLength   = 45

	// IMFMediaBuffer.
	vtblBufferLock             = 3
	vtblBufferUnlock           = 4
	vtblBufferGetCurrentLength = 5
	vtblBufferSetLength        = 6
	vtblBufferGetMaxLength     = 7

	// IMF2DBuffer.
	vtblLock2D   = 3
	vtblUnlock2D = 4

	// ICodecAPI. Its methods are declared in an order that does not match the
	// order they are documented in: IsSupported, IsModifiable,
	// GetParameterRange, GetParameterValues, GetDefaultValue, GetValue,
	// SetValue. SetValue is therefore the seventh method, not the third.
	vtblCodecIsSupported = 3
	vtblCodecGetValue    = 8
	vtblCodecSetValue    = 9
)

// MFVideoInterlace_Progressive. The encoder rejects anything else, and screen
// capture is never interlaced.
const mfVideoInterlaceProgressive = 2

// MFT_OUTPUT_STREAM_* flags.
const (
	mftOutputStreamProvidesSamples   = 0x100
	mftOutputStreamCanProvideSamples = 0x200
)

// MFT_MESSAGE_* commands.
const (
	mftMessageCommandFlush         = 0x00000000
	mftMessageCommandDrain         = 0x00000001
	mftMessageNotifyBeginStreaming = 0x10000000
	mftMessageNotifyStartOfStream  = 0x10000002
	mftMessageNotifyEndOfStream    = 0x10000003
)

// eAVEncCommonRateControlMode values, as CODECAPI_AVEncCommonRateControlMode
// takes them. Note that CBR is zero here, which is why the exported RateControl
// type in encoder_windows.go cannot pass its own constants through unchanged.
const (
	avEncRateControlCBR         = 0
	avEncRateControlVBR         = 1
	avEncRateControlQuality     = 2
	avEncRateControlLowDelayVBR = 3
)

// H.264 profiles, as MF_MT_MPEG2_PROFILE takes them (eAVEncH264VProfile_*).
const (
	profileBaseline = 66
	profileMain     = 77
	profileHigh     = 100
)

// H.264 levels (eAVEncH264VLevel*).
const (
	level3_1 = 31
	level4_0 = 40
	level5_1 = 51
)

// ---------------------------------------------------------------------------
// HRESULT and vtable helpers
// ---------------------------------------------------------------------------

// hresult is a Windows HRESULT.
//
// It is held unsigned. A failing HRESULT is a large 32-bit value, and the sign
// is a convention about its top bit rather than something worth representing:
// an int32 here would mean every error code in this file had to be written as a
// negative decimal, which is unreadable and easy to get wrong by one digit.
type hresult uint32

func (h hresult) failed() bool { return int32(h) < 0 }

func (h hresult) Error() string {
	if name := nameHRESULT(h); name != "" {
		return fmt.Sprintf("%s (0x%08X)", name, uint32(h))
	}
	// The low word is the facility's own code; the rest identifies the facility.
	return fmt.Sprintf("HRESULT 0x%08X", uint32(h))
}

// hr narrows a raw call result to its HRESULT.
//
// The call returns the raw RAX. The x64 ABI does not promise that the upper
// half of a 32-bit return value is zeroed, so it must be truncated before being
// read as signed. Skipping that turns a success code whose high bits are stale
// into a failure, and a failure into whatever those bits happen to spell. (The
// same trap is documented for the WASAPI code in README's implementation
// notes.)
func hr(v uintptr) hresult { return hresult(uint32(v)) }

// known HRESULTs this package acts on. The two MF_E_TRANSFORM_* values drive
// the encode loop, so the probe prints them by name when it meets them: a
// number that turned out to be wrong would show up there as an unnamed code
// rather than as a silently dropped frame.
var (
	errNeedMoreInput = hresult(0xC00D6D72) // MF_E_TRANSFORM_NEED_MORE_INPUT
	errStreamChange  = hresult(0xC00D6D61) // MF_E_TRANSFORM_STREAM_CHANGE
	errTypeNotSet    = hresult(0xC00D6D60) // MF_E_TRANSFORM_TYPE_NOT_SET
	errNotAccepting  = hresult(0xC00D36B5) // MF_E_NOTACCEPTING
	errNoMoreTypes   = hresult(0xC00D36B9) // MF_E_NO_MORE_TYPES
)

// nameHRESULT gives the well-known codes their names, so the probe's output can
// be read without a lookup table.
func nameHRESULT(h hresult) string {
	switch h {
	case errNeedMoreInput:
		return "MF_E_TRANSFORM_NEED_MORE_INPUT"
	case errStreamChange:
		return "MF_E_TRANSFORM_STREAM_CHANGE"
	case errTypeNotSet:
		return "MF_E_TRANSFORM_TYPE_NOT_SET"
	case errNotAccepting:
		return "MF_E_NOTACCEPTING"
	case errNoMoreTypes:
		return "MF_E_NO_MORE_TYPES"
	case 0x80070005:
		return "E_ACCESSDENIED"
	case 0x8007000E:
		return "E_OUTOFMEMORY"
	case 0x8007007E:
		return "ERROR_MOD_NOT_FOUND"
	case 0x80004005:
		return "E_FAIL"
	case 0x80004001:
		return "E_NOTIMPL"
	case 0x80070057:
		return "E_INVALIDARG"
	case 0x800401F0:
		return "CO_E_NOTINITIALIZED"
	case 0x80040154:
		return "REGDB_E_CLASSNOTREG"
	case 0x80010106:
		return "RPC_E_CHANGED_MODE"
	}
	return ""
}

// check turns a failing HRESULT into an error naming the call that produced it.
func check(what string, v uintptr) error {
	if h := hr(v); h.failed() {
		return fmt.Errorf("%s: %w", what, h)
	}
	return nil
}

// comCall invokes vtable slot idx on a COM interface pointer and returns the
// raw HRESULT.
//
// This is the same helper server/audio_windows.go uses, for the same reasons.
// Keep it a direct call to syscall.SyscallN: the guarantee that arguments
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

// queryInterface asks p for the interface identified by iid.
func queryInterface(p unsafe.Pointer, iid *guid) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r := comCall(p, vtblQueryInterface,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if h := hr(r); h.failed() {
		return nil, fmt.Errorf("QueryInterface: %w", h)
	}
	return out, nil
}

// release drops one reference and clears the caller's pointer. Release is the
// same slot on every interface, so one helper covers all of them.
func release(p *unsafe.Pointer) {
	if *p == nil {
		return
	}
	comCall(*p, vtblRelease)
	*p = nil
}

// ---------------------------------------------------------------------------
// Attribute helpers
// ---------------------------------------------------------------------------

func setUINT32(p unsafe.Pointer, key *guid, v uint32) error {
	r := comCall(p, vtblSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v))
	return check("IMFAttributes::SetUINT32", r)
}

func setGUID(p unsafe.Pointer, key, value *guid) error {
	r := comCall(p, vtblSetGUID,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(value)))
	return check("IMFAttributes::SetGUID", r)
}

func getUINT32(p unsafe.Pointer, key *guid) (uint32, error) {
	var out uint32
	r := comCall(p, vtblGetUINT32, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&out)))
	if h := hr(r); h.failed() {
		return 0, h
	}
	return out, nil
}

// getUINT64 reads an attribute whose value is a packed pair of dwords.
func getUINT64(p unsafe.Pointer, key *guid) (uint64, error) {
	var out uint64
	r := comCall(p, vtblGetUINT64, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&out)))
	if h := hr(r); h.failed() {
		return 0, h
	}
	return out, nil
}

// getGUID reads a GUID-valued attribute, such as a media type's subtype.
func getGUID(p unsafe.Pointer, key *guid) (guid, error) {
	var out guid
	r := comCall(p, vtblGetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&out)))
	if h := hr(r); h.failed() {
		return guid{}, h
	}
	return out, nil
}

// getBlob reads a blob-valued attribute into Go memory. The buffer comes back
// allocated with CoTaskMemAlloc, so it has to be handed back rather than
// collected.
func getBlob(p unsafe.Pointer, key *guid) ([]byte, error) {
	var raw unsafe.Pointer
	var n uint32
	r := comCall(p, vtblGetAllocatedBlob,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&raw)), uintptr(unsafe.Pointer(&n)))
	if h := hr(r); h.failed() {
		return nil, h
	}
	if raw == nil || n == 0 {
		return nil, nil
	}
	defer mustCall(procCoTaskMemFree, uintptr(raw))

	out := make([]byte, n)
	copy(out, unsafe.Slice((*byte)(raw), n))
	return out, nil
}

// lockBuffer maps a media buffer's memory and reports its capacity, plus how
// much of that capacity currently holds data.
//
// IMFMediaBuffer::Lock takes three out-parameters -- the pointer, the capacity
// and the current length -- and they are easy to miscount, which is exactly what
// happened the first time this was written: passing NULL for the capacity left
// it reading as zero, and passing NULL for the current length invited the
// encoder to write through a null pointer. They are handled in one place so
// that there is only one set of arguments to get right.
func lockBuffer(buffer unsafe.Pointer) (p unsafe.Pointer, maxLen, curLen uint32, err error) {
	r := comCall(buffer, vtblBufferLock,
		uintptr(unsafe.Pointer(&p)),
		uintptr(unsafe.Pointer(&maxLen)),
		uintptr(unsafe.Pointer(&curLen)))
	if h := hr(r); h.failed() {
		return nil, 0, 0, fmt.Errorf("IMFMediaBuffer::Lock: %w", h)
	}
	return p, maxLen, curLen, nil
}

// mfSetSize packs a width and height into the UINT64 these attributes expect.
// MFSetAttributeSize does the same thing, but doing it here keeps the packing
// visible: the high dword is the width, the low dword the height.
func mfSetSize(p unsafe.Pointer, key *guid, w, h uint32) error {
	r := comCall(p, vtblSetUINT64, uintptr(unsafe.Pointer(key)),
		uintptr(uint64(w)<<32|uint64(h)))
	return check("IMFAttributes::SetUINT64(frame size)", r)
}

// mfSetRatio packs a numerator and denominator the same way.
func mfSetRatio(p unsafe.Pointer, key *guid, num, den uint32) error {
	r := comCall(p, vtblSetUINT64, uintptr(unsafe.Pointer(key)),
		uintptr(uint64(num)<<32|uint64(den)))
	return check("IMFAttributes::SetUINT64(ratio)", r)
}

// ---------------------------------------------------------------------------
// Transform introspection
// ---------------------------------------------------------------------------

// transformIsAsync reports whether a transform is an asynchronous MFT.
//
// known is false when the attribute could not be read at all. That is not the
// same as "synchronous", even though the documentation's own rule is that an
// absent attribute means synchronous: a missing attribute and an unreadable one
// produce the same GetUINT32 failure, and only one of them is about the
// encoder. The two are reported separately so that the caller can say which it
// saw rather than quietly guessing.
func transformIsAsync(transform unsafe.Pointer) (async, known bool) {
	attrs, err := getAttributes(transform)
	if err != nil {
		return false, false
	}
	defer release(&attrs)

	v, err := getUINT32(attrs, &mfTransformAsync)
	if err != nil {
		return false, false
	}
	return v != 0, true
}

// unlockAsync tells an asynchronous transform that a client outside the Media
// Foundation pipeline is driving it, which is what makes it willing to accept a
// media type.
func unlockAsync(transform unsafe.Pointer) error {
	attrs, err := getAttributes(transform)
	if err != nil {
		return err
	}
	defer release(&attrs)
	return setUINT32(attrs, &mfTransformAsyncUnlock, 1)
}

// getAttributes returns the transform's attribute store, which is where
// MF_TRANSFORM_ASYNC lives. It is not an attribute on a media type.
func getAttributes(transform unsafe.Pointer) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r := comCall(transform, vtblGetAttributes, uintptr(unsafe.Pointer(&out)))
	if err := check("IMFTransform::GetAttributes", r); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("GetAttributes reported success but returned no store")
	}
	return out, nil
}

// activateObject turns an IMFActivate -- which is how MFTEnumEx hands back an
// encoder it has not created yet -- into the object itself.
//
// The returned pointer is the caller's to release; the activate object keeps no
// claim on it.
func activateObject(activate unsafe.Pointer) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r := comCall(activate, vtblActivateObject,
		uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&out)))
	if err := check("IMFActivate::ActivateObject", r); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("ActivateObject reported success but returned no transform")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Codec API
// ---------------------------------------------------------------------------

// codecAPI is an ICodecAPI used to configure the encoder's rate control, latency
// and keyframe policy. Every setting is optional: an encoder that does not
// support one returns an error, and the caller reports it rather than failing,
// because the defaults are workable.
type codecAPI struct {
	p unsafe.Pointer
}

func newCodecAPI(transform unsafe.Pointer) (*codecAPI, error) {
	p, err := queryInterface(transform, &iidICodecAPI)
	if err != nil {
		return nil, err
	}
	return &codecAPI{p: p}, nil
}

func (c *codecAPI) close() {
	if c != nil {
		release(&c.p)
	}
}

// isSupported reports whether the encoder implements a property.
func (c *codecAPI) isSupported(key *guid) bool {
	return !hr(comCall(c.p, vtblCodecIsSupported, uintptr(unsafe.Pointer(key)))).failed()
}

// setUINT32 sets a DWORD-valued property, returning an error for a property the
// encoder does not know.
//
// The variant is filled in by hand: a VARIANT is 24 bytes on x64 (an 8-byte
// header, an 8-byte value union, then room for a pointer), and vt=VT_UI4=19
// puts the payload in the first four bytes of the union.
func (c *codecAPI) setUINT32(key *guid, v uint32) error {
	var variant [24]byte
	*(*uint16)(unsafe.Pointer(&variant[0])) = 19 // VT_UI4
	*(*uint32)(unsafe.Pointer(&variant[8])) = v

	r := comCall(c.p, vtblCodecSetValue,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&variant[0])))
	return check("ICodecAPI::SetValue", r)
}

// setBOOL sets a BOOL-valued property.
//
// This is a separate method because the type is not a detail. Several of the
// properties that matter most here -- low latency, CABAC, force-keyframe -- are
// documented as BOOL, and the encoder answers a DWORD-valued variant for one of
// them with E_NOTIMPL, which reads as "not implemented" and means "not in that
// type". Every UINT32 property is accepted and every BOOL property was refused
// until this existed, which is what gave the distinction away.
//
// VT_BOOL is 11, and VARIANT_TRUE is -1 rather than 1: the true value is
// 0xFFFF, and a nonzero DWORD is not the same thing.
func (c *codecAPI) setBOOL(key *guid, v bool) error {
	var variant [24]byte
	*(*uint16)(unsafe.Pointer(&variant[0])) = 11 // VT_BOOL
	if v {
		*(*uint16)(unsafe.Pointer(&variant[8])) = 0xFFFF
	}

	r := comCall(c.p, vtblCodecSetValue,
		uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&variant[0])))
	return check("ICodecAPI::SetValue", r)
}

// ---------------------------------------------------------------------------
// Media Foundation lifetime
// ---------------------------------------------------------------------------

// mfStartup brings up Media Foundation once per process and returns a function
// that shuts it down again.
func mfStartup() (func(), error) {
	if err := check("MFStartup", mustCall(procMFStartup, mfVersion, mfStartupNoSocket)); err != nil {
		return func() {}, err
	}
	return func() { procMFShutdown.Call() }, nil
}

func mustCall(p *syscall.LazyProc, args ...uintptr) uintptr {
	r, _, _ := p.Call(args...)
	return r
}

// coInitialize enters a multithreaded apartment on the calling goroutine's
// thread, and returns the matching teardown. COM is initialised per thread, so
// the goroutine that encodes must not migrate: every encoder holds the OS
// thread it was created on.
//
// The three possible outcomes differ in what they owe back. S_OK and S_FALSE
// both leave the thread holding a reference -- S_FALSE means it was already
// initialised and this call is the second -- so both are balanced by
// CoUninitialize. RPC_E_CHANGED_MODE means the thread is already in an
// apartment of another kind, which this call did not create and must not
// release; the encoder is content to use whatever apartment it finds, since the
// transform runs in-process either way.
func coInitialize() (func(), error) {
	r := mustCall(procCoInitializeEx, 0, coinitMultithreaded)
	switch h := hr(r); h {
	case 0, 1: // S_OK, S_FALSE
		return func() { procCoUninitialize.Call() }, nil
	case 0x80010106: // RPC_E_CHANGED_MODE
		return func() {}, nil
	default:
		return nil, fmt.Errorf("CoInitializeEx: %w", h)
	}
}

// coCreateInstance creates the COM object for a class id.
func coCreateInstance(clsid, iid *guid) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r := mustCall(procCoCreateInstance,
		uintptr(unsafe.Pointer(clsid)), 0, clctxAll,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if h := hr(r); h.failed() {
		return nil, fmt.Errorf("CoCreateInstance: %w", h)
	}
	return out, nil
}

// createMediaType makes an empty IMFMediaType.
func createMediaType() (unsafe.Pointer, error) {
	var out unsafe.Pointer
	r := mustCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&out)))
	if err := check("MFCreateMediaType", r); err != nil {
		return nil, err
	}
	return out, nil
}

// createSampleWithBuffer makes an IMFSample backed by one linear buffer.
func createSampleWithBuffer(size int) (sample, buffer unsafe.Pointer, err error) {
	r := mustCall(procMFCreateMemoryBuffer, uintptr(size), uintptr(unsafe.Pointer(&buffer)))
	if err = check("MFCreateMemoryBuffer", r); err != nil {
		return nil, nil, err
	}

	r = mustCall(procMFCreateSample, uintptr(unsafe.Pointer(&sample)))
	if err = check("MFCreateSample", r); err != nil {
		release(&buffer)
		return nil, nil, err
	}
	if r = comCall(sample, vtblSampleAddBuffer, uintptr(buffer)); hr(r).failed() {
		release(&sample)
		release(&buffer)
		return nil, nil, fmt.Errorf("IMFSample::AddBuffer: %w", hr(r))
	}
	return sample, buffer, nil
}
