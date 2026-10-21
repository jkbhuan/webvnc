/**
 * Decodes the server's inter-frame stream with WebCodecs.
 *
 * The server sends one H.264 access unit per frame in Annex B form, parameter
 * sets repeated in band ahead of every keyframe. Two consequences shape this
 * class:
 *
 *  - No `description` is ever passed to `configure`. With none supplied, an AVC
 *    bitstream is required to be Annex B, so the in-band SPS and PPS are the
 *    whole of the decoder's initialisation -- no avcC box, no init segment, and
 *    nothing to keep in step with the encoder.
 *  - A decoder may only be handed a key chunk after it is configured or reset,
 *    and the server's keyframe is an IDR; everything before the first IDR is
 *    dropped rather than queued.
 *
 * The codec string is read out of the stream rather than fixed here because the
 * level inside it is the encoder's choice for the frame size. A hard-coded one
 * works until the screen resolution changes and then fails to configure, which
 * is exactly the sort of failure this should not have.
 */

import { codecString, inspectAccessUnit } from './h264'

/**
 * How many decoder errors in a row, with no frame decoded in between, mean the
 * stream is not going to be decoded at all.
 *
 * One error is ordinary: it costs at most a GOP before the next keyframe puts
 * things right, and the server rings a keyframe every second. A stream that
 * fails several times over without producing anything is a decoder that cannot
 * handle this profile, and waiting longer only delays the fallback.
 */
const MAX_FAILURES = 3

export class H264Player {
  private onFrame: (frame: VideoFrame) => void
  private onUnsupported: () => void

  private decoder: VideoDecoder | null = null
  /** The codec string the current decoder was configured with. */
  private codec = ''
  /** Set when the next chunk must be a key chunk. */
  private needsKey = true
  private failures = 0
  private closed = false

  // Timestamps are metadata the decoder passes through, and this player paints
  // every frame the moment it comes out, so nothing here reads them. They still
  // have to be strictly increasing: a decoder is within its rights to reject or
  // reorder on a repeat, and a counter is the cheapest way to guarantee it.
  private timestamp = 0

  constructor(onFrame: (frame: VideoFrame) => void, onUnsupported: () => void) {
    this.onFrame = onFrame
    this.onUnsupported = onUnsupported
  }

  /**
   * Feeds one access unit.
   *
   * `key` is the server's flag, not a guess: it marks the IDR access units the
   * stream can be started from.
   */
  push(payload: Uint8Array, key: boolean): void {
    if (this.closed) return

    if (!key) {
      // A delta frame is meaningless to a decoder with no reference for it, and
      // handing one over anyway is an error rather than a dropped frame.
      if (this.needsKey || !this.decoder) return
      this.decode(payload, 'delta')
      return
    }

    const info = inspectAccessUnit(payload)
    if (!info?.hasIDR) return

    // An SPS is always present ahead of an IDR, so a keyframe that parsed
    // without one is not something to configure a decoder from.
    const codec = info.profile ? codecString(info.profile, info.constraints, info.level) : ''
    if (!this.decoder || this.needsKey || (codec && codec !== this.codec)) {
      if (!this.configure(codec)) return
    }

    this.decode(payload, 'key')
  }

  close(): void {
    this.closed = true
    this.closeDecoder()
  }

  // -------------------------------------------------------------------------

  private configure(codec: string): boolean {
    this.closeDecoder()

    // Constructing a decoder is enough to tell whether this browser has the
    // API. Whether it has *this profile* is only answered by configuring, and
    // whether it has *this stream* only by decoding, which is why the failures
    // that count are counted later rather than here.
    if (typeof VideoDecoder === 'undefined') {
      this.onUnsupported()
      return false
    }

    let decoder: VideoDecoder
    try {
      decoder = new VideoDecoder({
        output: (frame) => this.handleOutput(decoder, frame),
        error: (err) => this.handleError(decoder, err),
      })
      decoder.configure({
        codec,
        // Nothing here needs more than a frame of lookahead, and saying so
        // keeps the decoder from building a reorder buffer it has no use for.
        optimizeForLatency: true,
      })
    } catch (err) {
      console.warn('H.264 decoder rejected the stream', codec, err)
      this.failures += 1
      if (this.failures >= MAX_FAILURES) this.onUnsupported()
      return false
    }

    this.decoder = decoder
    this.codec = codec
    this.needsKey = false
    return true
  }

  private decode(payload: Uint8Array, type: 'key' | 'delta'): void {
    const decoder = this.decoder
    if (!decoder) return

    try {
      decoder.decode(
        new EncodedVideoChunk({
          type,
          timestamp: (this.timestamp += 1000),
          // A view into the socket's buffer, which is dead the moment the next
          // message arrives. EncodedVideoChunk copies the bytes as it is
          // constructed, so nothing is holding on to that buffer afterwards.
          data: payload,
        }),
      )
    } catch (err) {
      // A decode() call throws only for a closed or unconfigured decoder, both
      // of which mean the same thing here: start again from a keyframe.
      console.warn('H.264 decode call failed', err)
      this.closeDecoder()
      this.needsKey = true
    }
  }

  private handleOutput(from: VideoDecoder, frame: VideoFrame): void {
    // Output from a decoder that has already been replaced belongs to a stream
    // nobody is looking at any more.
    if (from !== this.decoder) {
      frame.close()
      return
    }
    this.failures = 0
    this.onFrame(frame)
  }

  private handleError(from: VideoDecoder, err: DOMException): void {
    if (from !== this.decoder) return

    // A decoder that reports an error is closed by the platform, so there is
    // nothing to release here -- only a state to record. Whatever is on screen
    // stays there until the next keyframe, which is a second away.
    console.warn('H.264 decoder error, waiting for the next keyframe', err.message)
    this.decoder = null
    this.needsKey = true
    this.failures += 1
    if (this.failures >= MAX_FAILURES) this.onUnsupported()
  }

  private closeDecoder(): void {
    const decoder = this.decoder
    if (!decoder) return
    this.decoder = null
    try {
      decoder.close()
    } catch {
      // Already closed by an error, which is the ordinary case here.
    }
  }
}
