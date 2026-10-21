// Package mfenc encodes BGRA desktop frames as H.264 with the H.264 encoder
// that ships inside Windows itself.
//
// The encoder is a Media Foundation transform, reached through the same
// hand-rolled vtable calls the WASAPI capture uses, so the project keeps
// building with CGO_ENABLED=0 and shipping as one file. The price is that
// everything here is Win32-shaped and cannot be exercised off Windows; the
// colour conversion and bitstream helpers in this file are the exception, which
// is why they live apart from the MF machinery.
package mfenc

// The H.264 encoder MFT takes NV12 and nothing else, so every frame is
// converted on the way in. NV12 is a full-resolution luma plane followed by one
// interleaved chroma plane at half resolution in both directions.
//
// Colour is converted as BT.709 limited range (the "video range" convention,
// luma 16-235), which is what a browser assumes for high definition H.264. The
// coefficients below are the usual fixed-point form: each is the exact BT.709
// coefficient scaled by 219 (luma) or 224 (chroma) and then by 256, so the
// shift by 8 lands back on the 8-bit result. With 8-bit inputs the outputs are
// provably inside 16..235 and 16..240, so nothing here needs clamping.
const (
	yR = 46  // 219 * 0.2126
	yG = 157 // 219 * 0.7152
	yB = 16  // 219 * 0.0722

	cbR = -26 // 224 * -0.1146 / 1.8556 * ...
	cbG = -86
	cbB = 112

	crR = 112
	crG = -102
	crB = -10
)

// NV12Stride returns the row pitch used for both NV12 planes.
//
// The pitch is padded to a multiple of 32 because Media Foundation is much
// happier with an aligned stride, and a misaligned one costs more in the
// encoder's internal copy than the padding costs in memory.
func NV12Stride(width int) int {
	return (width + 31) &^ 31
}

// NV12Size returns the size in bytes of an NV12 image with the given stride.
func NV12Size(stride, height int) int {
	if stride <= 0 || height <= 0 {
		return 0
	}
	// height is rounded up to an even number: the chroma plane addresses pairs
	// of rows, so an odd height still costs a whole chroma row.
	return stride * ((height + 1) &^ 1) * 3 / 2
}

// RGBAtoNV12 converts a tightly packed RGBA image into NV12.
//
// srcStride is in bytes and may exceed width*4, which is what the capturer
// hands back. dst must be at least NV12Size(dstStride, height) bytes.
//
// Odd dimensions are allowed. The 2x2 chroma block that runs off the right or
// bottom edge reuses the last row or column, which is the same truncation the
// chroma plane itself performs.
func RGBAtoNV12(dst []byte, dstStride int, src []byte, srcStride, width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	if dstStride < width || srcStride < width*4 {
		return
	}
	if len(dst) < NV12Size(dstStride, height) || len(src) < srcStride*(height-1)+width*4 {
		return
	}

	uvOrigin := ((height + 1) &^ 1) * dstStride

	for y := 0; y < height; y++ {
		row := y * srcStride
		dstRow := y * dstStride

		for x := 0; x < width; x++ {
			i := row + x*4
			r, g, b := int(src[i]), int(src[i+1]), int(src[i+2])
			dst[dstRow+x] = byte((yR*r+yG*g+yB*b+128)>>8) + 16
		}

		// Chroma addresses pairs of rows and pairs of columns, so it is
		// computed once per 2x2 block rather than once per luma sample.
		if y%2 != 0 {
			continue
		}
		below := row
		if y+1 < height {
			below = row + srcStride
		}
		uvRow := uvOrigin + (y/2)*dstStride
		for x := 0; x < width; x += 2 {
			i := row + x*4
			k := below + x*4
			j, l := i, k
			if x+1 < width {
				j, l = i+4, k+4
			}
			// Average the four luma-adjacent pixels before converting, and
			// fold the average into the shift: summing first means one shift
			// instead of three divisions.
			r := int(src[i]) + int(src[j]) + int(src[k]) + int(src[l])
			g := int(src[i+1]) + int(src[j+1]) + int(src[k+1]) + int(src[l+1])
			b := int(src[i+2]) + int(src[j+2]) + int(src[k+2]) + int(src[l+2])

			dst[uvRow+x] = byte(((cbR*r + cbG*g + cbB*b + 512) >> 10) + 128)
			dst[uvRow+x+1] = byte(((crR*r + crG*g + crB*b + 512) >> 10) + 128)
		}
	}
}
