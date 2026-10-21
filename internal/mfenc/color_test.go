package mfenc

import (
	"math/rand"
	"testing"
)

// rgbaImage builds a tightly packed RGBA image from a per-pixel colour.
func rgbaImage(w, h int, pixel func(x, y int) (byte, byte, byte)) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, b := pixel(x, y)
			i := (y*w + x) * 4
			out[i], out[i+1], out[i+2], out[i+3] = r, g, b, 0xff
		}
	}
	return out
}

const solid = 8

func TestRGBAtoNV12KnownValues(t *testing.T) {
	// BT.709 limited range: black is 16, white 235, neutral chroma 128. The
	// fixed-point form truncates, so white lands one below the ideal 235.
	cases := []struct {
		name    string
		r, g, b byte
		wantY   byte
		wantCb  byte
		wantCr  byte
	}{
		{"black", 0, 0, 0, 16, 128, 128},
		{"white", 255, 255, 255, 234, 128, 128},
		{"grey", 128, 128, 128, 126, 128, 128},
		{"red", 255, 0, 0, 62, 102, 240},
		{"green", 0, 255, 0, 172, 42, 26},
		{"blue", 0, 0, 255, 32, 240, 118},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stride := NV12Stride(solid)
			dst := make([]byte, NV12Size(stride, solid))
			src := rgbaImage(solid, solid, func(int, int) (byte, byte, byte) { return tc.r, tc.g, tc.b })

			RGBAtoNV12(dst, stride, src, solid*4, solid, solid)

			if got := dst[0]; got != tc.wantY {
				t.Errorf("Y = %d, want %d", got, tc.wantY)
			}
			uv := dst[stride*solid:]
			if got := uv[0]; got != tc.wantCb {
				t.Errorf("Cb = %d, want %d", got, tc.wantCb)
			}
			if got := uv[1]; got != tc.wantCr {
				t.Errorf("Cr = %d, want %d", got, tc.wantCr)
			}
		})
	}
}

func TestRGBAtoNV12ChromaAveragesTheBlock(t *testing.T) {
	// A black pixel next to a white one must produce chroma averaged over the
	// four samples of the block, not the value of the top-left one.
	const w, h = 2, 2
	stride := NV12Stride(w)
	dst := make([]byte, NV12Size(stride, h))
	src := rgbaImage(w, h, func(x, y int) (byte, byte, byte) {
		if x == 0 && y == 0 {
			return 255, 255, 255
		}
		return 0, 0, 0
	})

	RGBAtoNV12(dst, stride, src, w*4, w, h)

	// Both pixels are neutral, so the average is neutral too -- and this is
	// exactly what sampling only the top-left pixel would get wrong if the
	// block held saturated colours instead.
	if uv := dst[stride*h:]; uv[0] != 128 || uv[1] != 128 {
		t.Errorf("chroma of a black/white block = (%d,%d), want (128,128)", uv[0], uv[1])
	}
}

func TestRGBAtoNV12StaysInRange(t *testing.T) {
	const w, h = 64, 48
	stride := NV12Stride(w)
	dst := make([]byte, NV12Size(stride, h))
	rng := rand.New(rand.NewSource(1))
	src := make([]byte, w*h*4)
	rng.Read(src)

	RGBAtoNV12(dst, stride, src, w*4, w, h)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if v := dst[y*stride+x]; v < 16 || v > 235 {
				t.Fatalf("luma %d at (%d,%d) is outside the limited range", v, x, y)
			}
		}
	}
	uv := dst[stride*h:]
	for y := 0; y < h/2; y++ {
		for x := 0; x < w; x++ {
			if v := uv[y*stride+x]; v < 16 || v > 240 {
				t.Fatalf("chroma %d at (%d,%d) is outside the limited range", v, x, y)
			}
		}
	}
}

func TestRGBAtoNV12LeavesPaddingAlone(t *testing.T) {
	// The destination is padded to an aligned stride. Writing into that padding
	// would corrupt the neighbouring plane, so it must stay untouched.
	const w, h = 10, 6
	stride := NV12Stride(w)
	if stride == w {
		t.Fatalf("test needs a padded stride, got %d for width %d", stride, w)
	}
	dst := make([]byte, NV12Size(stride, h))
	for i := range dst {
		dst[i] = 0xAB
	}
	src := rgbaImage(w, h, func(int, int) (byte, byte, byte) { return 0, 0, 0 })

	RGBAtoNV12(dst, stride, src, w*4, w, h)

	for y := 0; y < h; y++ {
		for x := w; x < stride; x++ {
			if dst[y*stride+x] != 0xAB {
				t.Fatalf("luma padding at (%d,%d) was written", x, y)
			}
		}
	}
	for y := 0; y < h/2; y++ {
		for x := w; x < stride; x++ {
			if v := dst[stride*h+y*stride+x]; v != 0xAB {
				t.Fatalf("chroma padding at (%d,%d) was written", x, y)
			}
		}
	}
}

func TestRGBAtoNV12OddDimensions(t *testing.T) {
	// Odd sizes are not encodable, but the conversion must not read past the
	// image while the caller works that out.
	for _, dim := range [][2]int{{1, 1}, {3, 5}, {7, 2}} {
		w, h := dim[0], dim[1]
		stride := NV12Stride(w)
		dst := make([]byte, NV12Size(stride, h))
		src := rgbaImage(w, h, func(int, int) (byte, byte, byte) { return 200, 100, 50 })

		RGBAtoNV12(dst, stride, src, w*4, w, h)
	}
}

func TestRGBAtoNV12RejectsUndersizedBuffers(t *testing.T) {
	const w, h = 16, 16
	stride := NV12Stride(w)
	full := make([]byte, NV12Size(stride, h))
	src := make([]byte, w*h*4)

	cases := []struct {
		name      string
		dst       []byte
		dstStride int
		src       []byte
		srcStride int
	}{
		{"short dst", full[:len(full)-1], stride, src, w * 4},
		{"dst stride below width", full, w - 1, src, w * 4},
		{"short src", full, stride, src[:len(src)-1], w * 4},
		{"src stride below width", full, stride, src, w*4 - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]byte(nil), tc.dst...)
			RGBAtoNV12(tc.dst, tc.dstStride, tc.src, tc.srcStride, w, h)
			for i := range before {
				if tc.dst[i] != before[i] {
					t.Fatalf("wrote to the buffer despite rejecting the call")
				}
			}
		})
	}
}

func TestNV12Size(t *testing.T) {
	// Stride 32, height 8: 32*8 luma + 32*4 chroma.
	if got, want := NV12Size(32, 8), 32*8*3/2; got != want {
		t.Errorf("NV12Size(32, 8) = %d, want %d", got, want)
	}
	// An odd height still needs a whole chroma row.
	if got, want := NV12Size(32, 9), 32*10*3/2; got != want {
		t.Errorf("NV12Size(32, 9) = %d, want %d", got, want)
	}
	if got := NV12Size(0, 8); got != 0 {
		t.Errorf("NV12Size(0, 8) = %d, want 0", got)
	}
}

func TestNV12Stride(t *testing.T) {
	cases := []struct{ width, want int }{
		{1, 32}, {32, 32}, {33, 64}, {1920, 1920}, {1366, 1376}, {3840, 3840},
	}
	for _, tc := range cases {
		if got := NV12Stride(tc.width); got != tc.want {
			t.Errorf("NV12Stride(%d) = %d, want %d", tc.width, got, tc.want)
		}
	}
}

func TestRGBAtoNV12HonoursSourceStride(t *testing.T) {
	// The capturer's rows are exactly width*4, but the encoder is handed
	// whatever the caller has; a wider source row must be skipped over, not
	// read as pixels.
	const w, h = 4, 2
	srcStride := w*4 + 16
	stride := NV12Stride(w)

	tight := make([]byte, NV12Size(stride, h))
	paddedSrc := make([]byte, srcStride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*srcStride + x*4
			paddedSrc[i], paddedSrc[i+1], paddedSrc[i+2], paddedSrc[i+3] = 10, 20, 30, 0xff
		}
	}
	padded := make([]byte, NV12Size(stride, h))
	RGBAtoNV12(padded, stride, paddedSrc, srcStride, w, h)

	// The same pixels with no padding must convert identically.
	tightSrc := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		copy(tightSrc[y*w*4:], paddedSrc[y*srcStride:y*srcStride+w*4])
	}
	RGBAtoNV12(tight, stride, tightSrc, w*4, w, h)

	for i := range tight {
		if tight[i] != padded[i] {
			t.Fatalf("byte %d differs: tight %d, padded %d", i, tight[i], padded[i])
		}
	}
}
