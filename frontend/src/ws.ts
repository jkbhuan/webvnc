/**
 * WebSocket transport for the remote desktop stream.
 *
 * Responsibilities:
 *  - build the /ws URL from the current page location (http -> ws, https -> wss)
 *  - tell the server what this client can decode, so it encodes accordingly
 *  - auto-reconnect with exponential backoff + jitter
 *  - keep the uplink (JSON input events) and downlink (binary frames) apart,
 *    since a single connection carries both
 */

export type ConnState =
  | 'idle'
  | 'connecting'
  | 'connected'
  | 'reconnecting'
  | 'unauthorized'
  | 'closed'

/**
 * What the first byte of a binary message says the payload is.
 *
 * These mirror the server's VideoCodec. They are repeated rather than shared
 * because the two halves of this program have no build step in common, and a
 * header is a wire format: the tags are part of the protocol, not an internal
 * detail either side is free to renumber.
 */
export const CodecJPEG = 0
export const CodecH264 = 1

/** The flag bit marking a frame a decoder may start from. */
const FlagKey = 1 << 0

/** Codec tag plus flags, at the front of every binary frame. */
const FRAME_HEADER_LEN = 2

/**
 * One binary frame, as it came off the wire.
 *
 * The codec is per message rather than agreed once at connect, because the
 * server can change it mid-stream: a client that cannot decode H.264 makes it
 * fall back to JPEG, and a client that had remembered what it was originally
 * promised would spend the rest of the session decoding noise.
 */
export interface FrameMessage {
  /** One of the Codec constants above. */
  codec: number
  /** A decoder can start here, with no earlier frame. */
  key: boolean
  /** The encoded payload, with the header already stripped. */
  payload: Uint8Array
}

export interface RemoteClientHandlers {
  onState?: (state: ConnState, info: StateInfo) => void
  /** One encoded frame arrived. The handler owns the payload. */
  onFrame?: (frame: FrameMessage) => void
  /**
   * The remote screen resolution, sent on connect and on change.
   *
   * `viewOnly` is live mode, as the server sees it. It rides along on a message
   * that already had to be sent rather than getting one of its own, so a client
   * that predates the field still learns the resolution. Treat it as a hint for
   * the UI only -- the server refuses input regardless, so a client that ignores
   * this flag loses nothing but politeness.
   */
  onScreen?: (width: number, height: number, viewOnly: boolean) => void
  /** Application-level round-trip time, in milliseconds. */
  onLatency?: (ms: number) => void
  /** Live stream settings, sent once per stats period. */
  onStats?: (stats: StreamStats) => void
  /** A packet of raw little-endian interleaved 16-bit PCM. */
  onAudio?: (pcm: ArrayBuffer) => void
  /** The audio format, sent once when the stream opens. */
  onAudioFormat?: (format: AudioFormat) => void
  /** Live audio settings, sent once per stats period while sound is on. */
  onAudioStats?: (stats: AudioStats) => void
  /** The answer to a clipboard_pull or clipboard_push. */
  onClipboard?: (result: ClipboardResult) => void
}

/**
 * What came back from a clipboard request.
 *
 * `ok` false is reserved for a real failure (the remote clipboard was locked,
 * say). An empty `text` with `ok` true is the ordinary "there is no text to
 * copy" case, and the two need different wording.
 */
export interface ClipboardResult {
  op: 'pull' | 'push'
  ok: boolean
  /** The remote clipboard's text. Only ever set for a pull. */
  text: string
  error: string
}

/** What the audio stream carries. Always 16-bit, whatever the device uses. */
export interface AudioFormat {
  sampleRate: number
  channels: number
  /** Always 16; carried so the client can assert it rather than assume. */
  bits: number
  /** Duration of one binary packet, in milliseconds. */
  packetMs: number
}

export interface AudioStats {
  /** Measured server egress for audio, in kbps. */
  kbps: number
  /** The remote desktop is producing no sound, so nothing is being sent. */
  idle: boolean
}

/** Per-connection settings for clients that are not the video stream. */
export interface RemoteClientOptions {
  /** Endpoint path. The video stream is the default. */
  path?: string
  /**
   * Which stream this is, and therefore what a binary message means.
   *
   * Declared rather than inferred from the endpoint, because the audio client
   * is the one that has to be told apart from everything else and inferring it
   * from a path is the kind of coupling that breaks the first time a path moves.
   */
  kind?: 'video' | 'audio'
  /**
   * Overrides whether this client claims it can decode H.264.
   *
   * Left unset it is detected, which is right for a browser whose answer depends
   * only on how the page was served. It is settable so that a client whose
   * decoder has failed can say so on its next connection without the transport
   * having to guess why.
   */
  videoCapable?: boolean
  /**
   * Stop retrying after this many consecutive failures and report 'closed'.
   *
   * Zero retries forever, which is right for the video stream: it is the whole
   * point of the page, so giving up would leave a dead screen. The audio stream
   * is optional and its server side can be switched off entirely, where a
   * refusal is permanent and retrying would be an endless silent loop.
   */
  maxAttempts?: number
}

/**
 * The server's view of the stream.
 *
 * The client cannot derive any of this from the frames: a still screen sends no
 * frames at all, so a frame-counting display would freeze at its last value and
 * look broken rather than idle.
 */
export interface StreamStats {
  /** The frame rate the server is aiming for, after rate control. */
  fps: number
  /** The JPEG quality the server is encoding at, after rate control. */
  quality: number
  /** Measured server egress, in kbps. */
  kbps: number
  /** The last capture found the screen unchanged. */
  idle: boolean
  /** The rate controller has throttled below the configured maximum. */
  limited: boolean
  /**
   * Which codec the server is encoding with: 'jpeg' or 'h264'.
   *
   * Sent rather than observed because a still screen produces no frames, so a
   * client that only learned the codec from what it received would have nothing
   * to go on exactly when the display matters most. Empty on servers too old to
   * report it, which is also why it is optional.
   */
  video?: string
}

export interface StateInfo {
  /** Human-readable detail, e.g. the retry delay. */
  detail?: string
  /** Consecutive failed attempts, for the "reconnecting (3)" display. */
  attempt?: number
  /** Milliseconds until the next retry, when known. */
  retryInMs?: number
}

const BACKOFF_BASE_MS = 500
const BACKOFF_MAX_MS = 10_000
const PING_INTERVAL_MS = 2_000
const VERIFY_AFTER_ATTEMPTS = 2

/** Exponential backoff with full jitter, plus a floor so we do not hammer. */
export function backoffDelay(attempt: number): number {
  const exponential = Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** Math.max(0, attempt - 1))
  return Math.round(exponential / 2 + Math.random() * (exponential / 2))
}

/**
 * Whether this page can decode H.264 at all.
 *
 * It is a question about the origin, not about the browser: `VideoDecoder` is
 * only exposed in a secure context, so one browser answers yes over https and
 * no over http. That is why the answer has to be sent per connection rather
 * than assumed from the user agent -- and why a server reached over plain http
 * on a LAN will see "no" from clients that are perfectly capable of it.
 */
export function canDecodeH264(): boolean {
  return typeof VideoDecoder !== 'undefined'
}

/**
 * Splits one binary message into its header and payload.
 *
 * Returns null for anything too short to hold a header. That is not worth
 * raising to the caller: a truncated frame cannot be reconstructed from what
 * arrived, and the next one is a few milliseconds behind it.
 */
export function parseFrame(buf: ArrayBuffer): FrameMessage | null {
  if (buf.byteLength < FRAME_HEADER_LEN) return null
  const bytes = new Uint8Array(buf)
  return {
    codec: bytes[0],
    key: (bytes[1] & FlagKey) !== 0,
    // A view, not a copy: the payload is encoded bytes handed straight to a
    // decoder, and copying every frame to trim two bytes off the front would be
    // work done per frame for nothing.
    payload: new Uint8Array(buf, FRAME_HEADER_LEN),
  }
}

export class RemoteClient {
  private ws: WebSocket | null = null
  private handlers: RemoteClientHandlers
  private token: string
  private path: string
  private kind: 'video' | 'audio'
  private h264: boolean
  private maxAttempts: number

  private state: ConnState = 'idle'
  private attempt = 0
  private retryTimer: number | null = null
  private pingTimer: number | null = null
  private lastPingSentAt = 0

  /** Set while we are deliberately closing, to suppress the reconnect loop. */
  private closing = false

  constructor(
    token: string,
    handlers: RemoteClientHandlers = {},
    options: RemoteClientOptions = {},
  ) {
    this.token = token
    this.handlers = handlers
    this.path = options.path ?? '/ws'
    this.kind = options.kind ?? 'video'
    this.h264 = options.videoCapable ?? canDecodeH264()
    this.maxAttempts = options.maxAttempts ?? 0
  }

  get currentState(): ConnState {
    return this.state
  }

  setToken(token: string): void {
    this.token = token
  }

  /**
   * Gives up on H.264 for this client.
   *
   * What a client can decode is settled in the connection URL, so there is no
   * way to withdraw the claim on a connection already open -- the caller has to
   * drop it and connect again, which is what reconnect is for. The server sees
   * the second connection arrive without the claim and moves the stream to
   * intra-frame, for good.
   */
  disableH264(): void {
    this.h264 = false
  }

  /**
   * Drops the connection and connects again immediately.
   *
   * For renegotiating anything that travels in the connection URL rather than
   * on the connection. Retrying through the backoff would be wrong here: the
   * failure is not a network one and the delay would only postpone a connection
   * that is going to behave differently.
   */
  reconnect(): void {
    if (this.closing) return
    this.clearTimers()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close(1000, 'client reconnecting')
      }
    }
    this.attempt = 0
    this.open()
  }

  /** Opens the connection, resetting any backoff from a previous failure. */
  connect(): void {
    this.closing = false
    this.attempt = 0
    this.open()
  }

  /** Closes the connection permanently and cancels any pending retry. */
  close(): void {
    this.closing = true
    this.clearTimers()
    if (this.ws) {
      const ws = this.ws
      this.ws = null
      // Detach handlers first so the teardown does not trigger a reconnect.
      ws.onopen = ws.onclose = ws.onerror = ws.onmessage = null
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close(1000, 'client closed')
      }
    }
    this.setState('closed')
  }

  /** Sends an input event. Silently drops if the socket is not open yet. */
  send(message: Record<string, unknown>): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(message))
    }
  }

  /** Forces an immediate retry, e.g. when the tab becomes visible again. */
  retryNow(): void {
    if (this.closing || this.state === 'connected' || this.state === 'connecting') return
    if (this.retryTimer !== null) {
      this.clearRetryTimer()
      this.attempt = 0
      this.open()
    }
  }

  // -------------------------------------------------------------------------

  private wsURL(): string {
    const scheme = window.location.protocol === 'https:' ? 'wss' : 'ws'
    // fmt is the frame layout this page was built against. The server refuses
    // anything else rather than streaming to it, because a page whose
    // JavaScript predates the framing does not fail loudly -- it paints
    // nothing and says nothing. It is the page's version, not the video's, so
    // it goes on both sockets, though only the video one checks it today.
    let url = `${scheme}://${window.location.host}${this.path}?token=${encodeURIComponent(this.token)}&fmt=1`
    // A claim, not a preference: the server takes it at its word and encodes
    // accordingly. It is only ever sent by a client that would decode what
    // comes back, which is why the detection lives next to the URL rather than
    // in the component -- the two have to agree.
    if (this.kind === 'video' && this.h264) url += '&v=h264'
    return url
  }

  private open(): void {
    if (this.closing) return

    this.setState(this.attempt === 0 ? 'connecting' : 'reconnecting', {
      attempt: this.attempt,
    })

    let ws: WebSocket
    try {
      ws = new WebSocket(this.wsURL())
    } catch (err) {
      this.scheduleReconnect(String(err))
      return
    }

    // Every binary message goes through parseFrame, which reads a header, so
    // both streams arrive as ArrayBuffers. The audio stream is PCM and is
    // handed on whole; the video stream is split into header and payload.
    ws.binaryType = 'arraybuffer'
    this.ws = ws

    ws.onopen = () => {
      if (this.ws !== ws) return
      this.attempt = 0
      this.setState('connected')
      this.startPinging()
    }

    ws.onmessage = (event: MessageEvent) => {
      if (this.ws !== ws) return
      this.handleMessage(event)
    }

    ws.onerror = () => {
      // The browser deliberately withholds handshake details (including the
      // 401) from script, so failures are classified in onclose instead.
    }

    ws.onclose = (event: CloseEvent) => {
      if (this.ws !== ws) return
      this.ws = null
      this.stopPinging()
      if (this.closing) return

      // A normal closure with this code means the server ended it deliberately,
      // and the reason it gives is the whole explanation -- it is the only
      // string that survives a refused connection, so the browser is showing
      // the user whatever the server put here.
      if (event.code === 1000) {
        this.setState('closed', { detail: event.reason || '服务端已关闭连接' })
        return
      }
      this.scheduleReconnect()
    }
  }

  private handleMessage(event: MessageEvent): void {
    if (typeof event.data === 'string') {
      this.handleControl(event.data)
      return
    }
    // Anything that is not an ArrayBuffer cannot be either stream, and guessing
    // at it would decode noise, so it is dropped.
    if (!(event.data instanceof ArrayBuffer)) return

    if (this.kind === 'audio') {
      this.handlers.onAudio?.(event.data)
      return
    }

    const frame = parseFrame(event.data)
    if (frame) this.handlers.onFrame?.(frame)
  }

  private handleControl(text: string): void {
    let msg: {
      type?: string
      width?: number
      height?: number
      viewOnly?: boolean
      t?: number
      fps?: number
      quality?: number
      kbps?: number
      idle?: boolean
      limited?: boolean
      sampleRate?: number
      channels?: number
      bits?: number
      packetMs?: number
      video?: string
      op?: string
      ok?: boolean
      text?: string
      error?: string
    }
    try {
      msg = JSON.parse(text)
    } catch {
      return
    }

    switch (msg.type) {
      case 'screen':
        if (msg.width && msg.height) {
          // Absent means an older server, which cannot be in live mode -- that
          // flag did not exist then -- so false is the correct reading, not just
          // the safe one.
          this.handlers.onScreen?.(msg.width, msg.height, msg.viewOnly === true)
        }
        break
      case 'pong':
        if (typeof msg.t === 'number' && this.lastPingSentAt > 0) {
          this.handlers.onLatency?.(Date.now() - this.lastPingSentAt)
        }
        break
      case 'stats':
        if (
          typeof msg.fps === 'number' &&
          typeof msg.quality === 'number' &&
          typeof msg.kbps === 'number'
        ) {
          this.handlers.onStats?.({
            fps: msg.fps,
            quality: msg.quality,
            kbps: msg.kbps,
            idle: msg.idle === true,
            limited: msg.limited === true,
            video: typeof msg.video === 'string' ? msg.video : '',
          })
        }
        break
      case 'audio':
        // Sent once, before the first packet, and never again: the format only
        // changes if the server restarts, which means a new connection.
        if (
          typeof msg.sampleRate === 'number' &&
          typeof msg.channels === 'number' &&
          typeof msg.bits === 'number' &&
          typeof msg.packetMs === 'number'
        ) {
          this.handlers.onAudioFormat?.({
            sampleRate: msg.sampleRate,
            channels: msg.channels,
            bits: msg.bits,
            packetMs: msg.packetMs,
          })
        }
        break
      case 'audioStats':
        if (typeof msg.kbps === 'number') {
          this.handlers.onAudioStats?.({ kbps: msg.kbps, idle: msg.idle === true })
        }
        break
      case 'clipboard':
        // The answer to a clipboard_pull or a clipboard_push. `ok` is false
        // only for a real failure: an empty string with ok true means the
        // clipboard held no text, which is a normal state and not an error.
        if (msg.op === 'pull' || msg.op === 'push') {
          this.handlers.onClipboard?.({
            op: msg.op,
            ok: msg.ok === true,
            text: typeof msg.text === 'string' ? msg.text : '',
            error: typeof msg.error === 'string' ? msg.error : '',
          })
        }
        break
    }
  }

  private scheduleReconnect(detail?: string): void {
    if (this.closing) return
    this.attempt += 1

    // An optional stream stops rather than looping forever. A server with the
    // feature switched off refuses the endpoint every time, and retrying that
    // silently in the background is indistinguishable from a hang.
    if (this.maxAttempts > 0 && this.attempt >= this.maxAttempts) {
      this.closing = true
      this.clearTimers()
      this.setState('closed', { detail: `连续 ${this.attempt} 次连接失败，已停止重试` })
      return
    }

    const delay = backoffDelay(this.attempt)
    this.setState('reconnecting', {
      attempt: this.attempt,
      retryInMs: delay,
      detail,
    })

    this.clearRetryTimer()
    this.retryTimer = window.setTimeout(() => {
      this.retryTimer = null
      void this.verifyThenRetry()
    }, delay)
  }

  /**
   * A rejected handshake and a dead network look identical to the browser, so
   * after a couple of failures ask the API whether the token is still good.
   * That turns "silently retrying forever with a stale password" into a clear
   * "please log in again".
   */
  private async verifyThenRetry(): Promise<void> {
    if (this.closing) return

    if (this.attempt >= VERIFY_AFTER_ATTEMPTS) {
      try {
        const res = await fetch(`/api/verify?token=${encodeURIComponent(this.token)}`, {
          method: 'GET',
          cache: 'no-store',
        })
        if (res.status === 401) {
          this.closing = true
          this.clearTimers()
          this.setState('unauthorized', { detail: '口令已失效，请重新登录' })
          return
        }
      } catch {
        // Server unreachable: fall through and keep retrying.
      }
    }

    this.open()
  }

  private startPinging(): void {
    this.stopPinging()
    this.pingTimer = window.setInterval(() => {
      this.lastPingSentAt = Date.now()
      this.send({ type: 'ping' })
    }, PING_INTERVAL_MS)
  }

  private stopPinging(): void {
    if (this.pingTimer !== null) {
      window.clearInterval(this.pingTimer)
      this.pingTimer = null
    }
    this.lastPingSentAt = 0
  }

  private clearRetryTimer(): void {
    if (this.retryTimer !== null) {
      window.clearTimeout(this.retryTimer)
      this.retryTimer = null
    }
  }

  private clearTimers(): void {
    this.clearRetryTimer()
    this.stopPinging()
  }

  private setState(state: ConnState, info: StateInfo = {}): void {
    // Always notify, even when the state is unchanged: the retry countdown and
    // attempt number still need to reach the UI.
    this.state = state
    this.handlers.onState?.(state, info)
  }
}
