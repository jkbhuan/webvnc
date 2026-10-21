/**
 * Playback for the remote desktop's audio stream.
 *
 * The server sends uncompressed 16-bit PCM in fixed 20 ms packets down its own
 * WebSocket; this turns them back into sound. Three details make it more than a
 * queue of buffers:
 *
 *  - Packets are scheduled ahead on the AudioContext's own clock rather than
 *    played as they arrive. That clock is the only one that keeps its cadence
 *    while the main thread is busy, and the main thread here spends its time
 *    decoding JPEG frames. Playing each packet the moment it lands would put
 *    every frame decode's worth of stall straight into the sound.
 *
 *  - The server stops sending while the remote desktop is quiet, so playback is
 *    regularly starved and restarted. That is the designed behaviour, not an
 *    error, and the resync in `push` exists for it.
 *
 *  - Sound arrives a little slower than it plays -- about half a percent end to
 *    end, which is enough to erode the whole cushion in twenty seconds of
 *    continuous sound. `drift` measures the rate that is actually arriving and
 *    plays at that rate instead.
 *
 * Autoplay policy is why sound starts off: a browser refuses to run an
 * AudioContext that no user gesture asked for. The mute button is therefore the
 * feature, not a formality -- see `enable`.
 */
import type { AudioFormat } from './ws'

/**
 * How far ahead of the clock playback is scheduled. Long enough to ride out
 * ordinary network jitter and a frame decode, short enough that the delay from
 * the remote speaker to the local one stays unnoticeable.
 */
const CUSHION_S = 0.1

/**
 * Window over which the arriving frame rate is measured, for the clock
 * correction described on `drift` below. Long enough to average out network
 * jitter, short enough that the correction settles quickly.
 */
const RATE_WINDOW_S = 2

/**
 * Ceiling on the rate correction, as a fraction. One percent is a one percent
 * change in pitch, which is under the threshold where it is noticeable in
 * music, and it is several times the mismatch measured end to end -- so a
 * heavily loaded machine stays inside it too.
 */
const DRIFT_LIMIT = 0.01

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, v))
}

export class AudioPlayer {
  private ctx: AudioContext | null = null
  private gain: GainNode | null = null
  private format: AudioFormat | null = null

  /** When the next packet is due, on the context's clock. */
  private nextPlayTime = 0

  /** Frames and wall-clock span of the rate measurement in progress. */
  private arrivedFrames = 0
  private arrivedSince = 0

  /**
   * Playback rate correction, from the measured frame rate rather than from a
   * feedback loop.
   *
   * Audio does not arrive at quite the rate it is played. Measured end to end
   * against a real 44.1 kHz loopback endpoint, with a gap-free source and an
   * otherwise idle machine, the stream arrives at about 99.4% of real time --
   * some 5 ms of audio short every second, which is a small fraction of a
   * packet and so never visible as a missing packet. Playing back at a flat
   * 100% therefore drains the cushion and reaches zero in about twenty seconds
   * of continuous sound. From then on every packet is played the instant it
   * lands and there is no jitter margin left to trade for a late one.
   *
   * Measuring the arrival rate and playing at that rate absorbs a mismatch
   * whatever its cause -- and there is more than one, since the capture device,
   * the server's packet pacing and this machine's sound card are all separate
   * clocks. Nothing to tune, and no loop that can oscillate.
   */
  private drift = 1

  /**
   * Sources that have been scheduled but not yet finished.
   *
   * Held only to keep them alive: a scheduled AudioBufferSourceNode with no
   * JavaScript reference may be collected before it ever plays. Entries remove
   * themselves on end, so this never grows past the cushion's worth.
   */
  private live = new Set<AudioBufferSourceNode>()

  /** True while the context exists and audio will be played. */
  get running(): boolean {
    return this.ctx !== null
  }

  /**
   * Opens the audio output. Must be called from a user gesture: that is the only
   * circumstance in which a browser will let an AudioContext run, which is the
   * whole reason audio is opt-in.
   */
  enable(): void {
    if (this.ctx) return

    const ctx = new AudioContext()
    // An explicit gain node, rather than connecting sources straight to the
    // destination, keeps one stable mixing point for every packet and leaves
    // room for a volume control that is not being built yet.
    const gain = ctx.createGain()
    gain.connect(ctx.destination)

    this.ctx = ctx
    this.gain = gain
    // Chrome and Safari hand back a suspended context even when the call comes
    // from a click; resuming is what actually starts it.
    void ctx.resume().catch(() => {})
  }

  /** Closes the output and drops everything queued. */
  disable(): void {
    const ctx = this.ctx
    this.ctx = null
    this.gain = null
    this.format = null
    this.nextPlayTime = 0
    this.arrivedFrames = 0
    this.arrivedSince = 0
    this.drift = 1
    this.live.clear()
    // Closing stops every scheduled source and releases the output device;
    // stopping them individually first would be redundant.
    if (ctx) void ctx.close().catch(() => {})
  }

  /** Applies the server's format handshake. Packets before it are dropped. */
  configure(format: AudioFormat): void {
    this.format = format
    // A new stream has no continuity with the old one, so the first packet must
    // re-establish the cushion rather than inherit a stale schedule.
    this.nextPlayTime = 0
    // A rate measured against the previous device says nothing about this one.
    this.arrivedFrames = 0
    this.arrivedSince = 0
    this.drift = 1
  }

  /**
   * Updates the playback rate from the frames that have arrived so far.
   *
   * A stream that stopped and restarted is not a slow stream, so a window that
   * spans a gap is thrown away rather than measured: the elapsed time includes
   * a stretch that produced nothing, which would read as an absurdly slow
   * sender. The ratio bound does that rejection on its own, and the window then
   * starts over from the first packet after the gap.
   */
  private measureRate(nowMs: number, frames: number, sampleRate: number): void {
    if (this.arrivedSince === 0) {
      this.arrivedSince = nowMs
      return
    }

    this.arrivedFrames += frames
    const elapsed = (nowMs - this.arrivedSince) / 1000
    if (elapsed < RATE_WINDOW_S) return

    const ratio = this.arrivedFrames / elapsed / sampleRate
    // Anything outside this band is a gap or a burst, not a clock.
    if (ratio > 0.9 && ratio < 1.1) {
      this.drift = clamp(ratio, 1 - DRIFT_LIMIT, 1 + DRIFT_LIMIT)
    }
    this.arrivedFrames = 0
    this.arrivedSince = nowMs
  }

  /** Queues one packet of interleaved little-endian 16-bit PCM. */
  push(data: ArrayBuffer): void {
    const ctx = this.ctx
    const gain = this.gain
    const format = this.format
    if (!ctx || !gain || !format) return

    const { channels, sampleRate } = format
    const frames = data.byteLength / (2 * channels)
    // The server only ever packs whole frames, so a remainder means something
    // upstream is wrong. Playing it would shift every later packet's channels.
    if (channels <= 0 || !Number.isInteger(frames) || frames <= 0) return

    let buffer: AudioBuffer
    try {
      buffer = ctx.createBuffer(channels, frames, sampleRate)
    } catch {
      // A sample rate the context will not accept; dropping the packet is a
      // short silence, where throwing here would kill the message handler.
      return
    }

    // The buffer carries the *stream's* rate, which need not match the
    // context's. Web Audio resamples on playback, so a 44.1 kHz capture plays
    // correctly through a 48 kHz output device.
    const view = new DataView(data)
    for (let ch = 0; ch < channels; ch++) {
      const dest = buffer.getChannelData(ch)
      for (let i = 0; i < frames; i++) {
        // Dividing by 32768 rather than 32767 maps -32768 to exactly -1.0;
        // dividing by 32767 would push +32767 past 1.0.
        dest[i] = view.getInt16(i * channels * 2 + ch * 2, true) / 32768
      }
    }

    const now = ctx.currentTime
    if (this.nextPlayTime < now) {
      // Everything scheduled earlier has already played, so the packet is late.
      const behind = now - this.nextPlayTime
      // A short overshoot is ordinary jitter: start now and absorb it, rather
      // than adding a fresh cushion on top of a delay that was already audible.
      //
      // A long one means the stream stopped, which it does whenever the remote
      // desktop goes quiet. There the cushion has to be rebuilt, because the
      // backlog that follows would otherwise all be scheduled in the past --
      // every packet of it would play at once, on top of the others.
      if (behind > CUSHION_S) {
        this.nextPlayTime = now + CUSHION_S
        // The stream stopped and is starting over, so the rate measurement has
        // to start over with it.
        this.arrivedFrames = 0
        this.arrivedSince = 0
      } else {
        this.nextPlayTime = now
      }
    }

    this.measureRate(performance.now(), frames, sampleRate)

    const source = ctx.createBufferSource()
    source.buffer = buffer
    // Playing the buffer at the corrected rate is what keeps each packet butted
    // against the previous one: advancing the schedule alone would leave a
    // fraction of a millisecond of silence between packets, at the same rate
    // the correction is running at.
    source.playbackRate.value = this.drift
    source.connect(gain)
    source.onended = () => this.live.delete(source)
    this.live.add(source)
    source.start(this.nextPlayTime)
    this.nextPlayTime += buffer.duration / this.drift
  }
}
