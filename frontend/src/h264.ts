/**
 * Just enough H.264 to feed a browser decoder correctly.
 *
 * The stream arrives in Annex B form -- start codes and all, with the parameter
 * sets repeated in band ahead of every keyframe -- which is what makes WebCodecs
 * usable without an out-of-band `description`. Two things still have to be read
 * out of the bytes, and both of them are the kind of detail that works on the
 * machine it was written on and fails on the next one:
 *
 *  - whether an access unit is an IDR, because a decoder that has just been
 *    configured or reset cannot be given anything else;
 *  - the profile and level, because `VideoDecoder.configure` needs a codec
 *    string and the level inside it is chosen by the encoder from the frame
 *    size. Hard-coding one works until the screen resolution changes.
 */

// NAL unit types, from H.264 Table 7-1. Only the two that change what the
// client does are named; everything else -- slices, SEI, the access unit
// delimiter -- is passed over untouched.
const NAL_IDR = 5
const NAL_SPS = 7

/**
 * The decoded form of one access unit.
 *
 * `hasIDR` is the field that matters: a keyframe access unit is the only kind a
 * decoder may be handed after a configure or a reset, and it is also the only
 * kind that carries the parameter sets needed to build a codec string.
 */
export interface AccessUnitInfo {
  hasIDR: boolean
  profile: number
  constraints: number
  level: number
}

/**
 * Splits an Annex B buffer into NAL units, each without its start code.
 *
 * Start codes are three bytes, optionally preceded by one zero byte that is
 * part of the framing rather than of any NAL unit. A scanner that looks for
 * `00 00 01` therefore finds the four-byte form one byte late, and the zero it
 * skips over lands at the end of the *previous* unit. Zero bytes there are
 * discarded, which is also what the standard does: a byte stream may pad
 * between units with trailing zeros, and a unit's own payload never ends in one,
 * because the RBSP trailing bits of the last syntax element are non-zero.
 *
 * Nothing here is copied. Each unit is a view into the buffer it came from.
 */
export function splitNALUs(data: Uint8Array): Uint8Array[] {
  const units: Uint8Array[] = []
  let i = 0
  let start = -1

  while (i + 2 < data.length) {
    if (data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 1) {
      if (start >= 0) units.push(trimTrailingZeros(data, start, i))
      i += 3
      start = i
      continue
    }
    i++
  }
  if (start >= 0 && start < data.length) {
    units.push(trimTrailingZeros(data, start, data.length))
  }

  return units
}

/** A view of data[from:to] with any trailing zero bytes dropped. */
function trimTrailingZeros(data: Uint8Array, from: number, to: number): Uint8Array {
  let end = to
  while (end > from && data[end - 1] === 0) end--
  return data.subarray(from, end)
}

/** The NAL unit type of a unit's payload, or 0 if it is empty. */
export function nalType(unit: Uint8Array): number {
  return unit.length > 0 ? unit[0] & 0x1f : 0
}

/**
 * Reads an access unit for the two things the client needs from it.
 *
 * Returns null when the unit is empty or starts with something that is not a
 * NAL unit, which is not worth raising to the caller: the stream is the server's
 * and the only response available is to keep waiting for a keyframe.
 */
export function inspectAccessUnit(data: Uint8Array): AccessUnitInfo | null {
  if (data.length < 1) return null

  const info: AccessUnitInfo = { hasIDR: false, profile: 0, constraints: 0, level: 0 }

  for (const unit of splitNALUs(data)) {
    const type = nalType(unit)
    if (type === NAL_IDR) {
      info.hasIDR = true
    } else if (type === NAL_SPS && unit.length >= 4 && info.profile === 0) {
      // An SPS payload is: one header byte, then profile_idc,
      // constraint_set flags, and level_idc. The rest describes the picture,
      // and none of it is needed here -- the decoder reads that itself.
      info.profile = unit[1]
      info.constraints = unit[2]
      info.level = unit[3]
    }
  }

  return info
}

/**
 * The RFC 6381 codec string for a profile and level, as `avc1.PPCCLL`.
 *
 * Every field is two hex digits, upper case, and all three are required: a
 * browser given `avc1.42C` cannot tell which level is meant and will refuse.
 */
export function codecString(profile: number, constraints: number, level: number): string {
  const hex = (n: number) => n.toString(16).padStart(2, '0').toUpperCase()
  return `avc1.${hex(profile)}${hex(constraints)}${hex(level)}`
}
