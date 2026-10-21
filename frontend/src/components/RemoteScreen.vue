<script setup lang="ts">
/**
 * The remote desktop surface.
 *
 * Layout: a <canvas> paints the incoming frames, and a transparent <input>
 * layered on top owns the keyboard. The input is what makes Chinese (and any
 * other IME) input work: the candidate window belongs to the *client's* input
 * method, composition happens entirely locally, and only the committed string is
 * forwarded -- see `onCompositionEnd`. Physical key events are forwarded
 * separately, except while a composition is in flight.
 *
 * Two decoders share the canvas. Intra-frame JPEG goes through
 * createImageBitmap; inter-frame H.264 goes through WebCodecs and is owned by
 * H264Player. Which one a frame belongs to is on the frame itself rather than
 * decided once per connection, because the server switches codec mid-stream
 * when a client turns out not to be able to decode H.264.
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { AudioPlayer } from '../audio'
import { prefersTouchMode, TouchGestures, type InputMode, type TouchAction } from '../touch'
import { H264Player } from '../video'
import {
  CodecH264,
  CodecJPEG,
  RemoteClient,
  type ClipboardResult,
  type ConnState,
  type StateInfo,
} from '../ws'

const props = defineProps<{
  token: string
}>()

const emit = defineEmits<{
  (e: 'unauthorized'): void
  /** Sound was switched on or off, so the toolbar can label its button. */
  (e: 'audio', on: boolean): void
  /** The touch input mode changed, so the toolbar can show which is in force. */
  (e: 'inputMode', mode: InputMode): void
  /** The soft keyboard was switched on or off, so the toolbar can label its button. */
  (e: 'keyboard', on: boolean): void
  /**
   * The server is in live mode, so the toolbar can hide the controls that would
   * only pretend to work. Reported rather than assumed because it is the
   * server's decision; see `viewOnly` below.
   */
  (e: 'viewOnly', on: boolean): void
}>()

const wrapEl = ref<HTMLDivElement | null>(null)
const canvasEl = ref<HTMLCanvasElement | null>(null)
const inputEl = ref<HTMLInputElement | null>(null)

const connState = ref<ConnState>('idle')
const stateDetail = ref<string>('')

// Live mode, as the server announced it. Everything below treats it purely as a
// UI concern -- hiding controls that cannot work and keeping the soft keyboard
// from opening for a viewer who cannot type. It is NOT the security boundary:
// the server drops every input frame it receives in this mode, so a client that
// ignores this flag (an old cached bundle, or a script driving the socket
// directly) still cannot touch the remote machine. Which is the point of
// putting the real check on the server.
const viewOnly = ref(false)
const retryAttempt = ref(0)
const retryInMs = ref(0)

const remoteW = ref(0)
const remoteH = ref(0)
const latencyMs = ref(0)
const receivedFps = ref(0)
const dropping = ref(false)

// Live stream state reported by the server. Needed because a still screen sends
// no frames at all, so receivedFps on its own would sit frozen at its last value
// and read as "stuck", not "idle".
const hasStats = ref(false)
const streamFps = ref(0)
const streamQuality = ref(0)
const streamKbps = ref(0)
const streamIdle = ref(false)
const streamLimited = ref(false)
const streamVideo = ref('')

// Audio is a second, independent connection, and off until asked for. The
// browser's autoplay policy is what makes that a requirement rather than a
// preference: an AudioContext cannot start without a user gesture.
const audioOn = ref(false)
const audioDetail = ref('')
const audioKbps = ref(0)
const audioIdle = ref(false)
const hasAudioStats = ref(false)

let client: RemoteClient | null = null
let ctx: CanvasRenderingContext2D | null = null
let resizeObserver: ResizeObserver | null = null
let audioClient: RemoteClient | null = null
let player: AudioPlayer | null = null

/** Where the frame is painted inside the canvas, in device pixels. */
let view = { ox: 0, oy: 0, dw: 0, dh: 0 }

// --- frame decoding ------------------------------------------------------
// Decoding is async, so frames that arrive mid-decode are coalesced into a
// single pending slot. A slow client therefore skips frames instead of building
// an ever-growing backlog.
//
// H.264 does not go through that slot. Its frames are cheap to hand to the
// decoder -- the work happens on the decoder's own thread -- and dropping one
// would corrupt every frame that predicts from it, so the two paths are kept
// apart rather than merged into one queue.
let pendingFrame: Blob | null = null
let decoding = false
let fpsWindowStart = 0
let fpsWindowCount = 0

let h264: H264Player | null = null

/** A decoded frame: an ImageBitmap, an <img> where that API is missing, or a VideoFrame. */
type Frame = ImageBitmap | HTMLImageElement | VideoFrame

// The most recent decoded frame, retained so a window resize can repaint
// immediately instead of waiting for the next capture tick. Deliberately not a
// Vue ref: the decoded image is a large opaque object with nothing to track.
let lastBitmap: Frame | null = null

const statusText = computed(() => {
  switch (connState.value) {
    case 'connected':
      return '已连接'
    case 'connecting':
      return '连接中…'
    case 'reconnecting':
      return retryAttempt.value > 0 ? `重新连接中 (第 ${retryAttempt.value} 次)` : '重新连接中…'
    case 'unauthorized':
      return '口令已失效'
    case 'closed':
      return '已断开'
    default:
      return '未连接'
  }
})

const statusClass = computed(() => {
  switch (connState.value) {
    case 'connected':
      return 'ok'
    case 'connecting':
    case 'reconnecting':
      return 'warn'
    default:
      return 'bad'
  }
})

const resolutionText = computed(() =>
  remoteW.value && remoteH.value ? `${remoteW.value} × ${remoteH.value}` : '—',
)

/** Renders a rate in kbps the way a person would say it. */
function formatRate(kbps: number): string {
  return kbps >= 1000 ? `${(kbps / 1000).toFixed(1)} Mbps` : `${kbps} kbps`
}

const bitrateText = computed(() => formatRate(streamKbps.value))

const videoText = computed(() => {
  switch (streamVideo.value) {
    case 'h264':
      return 'H.264 帧间'
    case 'jpeg':
      return 'JPEG 帧内'
    default:
      return ''
  }
})

/**
 * What the rate controller has throttled to.
 *
 * The quality percentage is left out under H.264: that encoder is not driven by
 * the JPEG quality knob, so the number would keep moving while nothing about
 * the stream did. Only the frame rate is a lever there.
 */
const limitedText = computed(() =>
  streamVideo.value === 'h264'
    ? `${streamFps.value}fps`
    : `${streamQuality.value}% / ${streamFps.value}fps`,
)

const audioText = computed(() => {
  if (!audioOn.value) return '无声'
  if (!hasAudioStats.value) return '声音 …'
  // The stream is open but the remote desktop is not making a sound, so the
  // server has stopped sending. Saying that beats showing a rate of zero, which
  // reads as something broken. This mirrors "静止" for the video the server is
  // not sending either.
  if (audioIdle.value) return '声音 待机'
  return `声音 ${formatRate(audioKbps.value)}`
})

// --- lifecycle -----------------------------------------------------------

onMounted(() => {
  ctx = canvasEl.value?.getContext('2d', { alpha: false }) ?? null

  resizeObserver = new ResizeObserver(() => resizeCanvas())
  if (wrapEl.value) resizeObserver.observe(wrapEl.value)
  resizeCanvas()

  client = new RemoteClient(props.token, {
    onState: (state, info: StateInfo) => {
      connState.value = state
      stateDetail.value = info.detail ?? ''
      retryAttempt.value = info.attempt ?? 0
      retryInMs.value = info.retryInMs ?? 0
      if (state === 'unauthorized') emit('unauthorized')
    },
    onFrame: (frame) => {
      countFrame()
      switch (frame.codec) {
        case CodecH264:
          h264Player().push(frame.payload, frame.key)
          break
        case CodecJPEG:
          // The server has moved to intra-frame, which it does when a client
          // cannot decode H.264. Nothing will come from the decoder again, so
          // it is released here rather than left holding a reference.
          releaseH264()
          pendingFrame = new Blob([frame.payload], { type: 'image/jpeg' })
          void drainFrames()
          break
      }
    },
    onScreen: (w, h, serverViewOnly) => {
      remoteW.value = w
      remoteH.value = h
      // The flag is fixed for the life of the connection -- the server reads it
      // once from its own config -- so there is nothing to re-check when a later
      // `screen` arrives on a resolution change.
      if (serverViewOnly && !viewOnly.value) retractViewOnlyKeyboard()
      viewOnly.value = serverViewOnly
      emit('viewOnly', serverViewOnly)
      // The recogniser needs the desktop size to keep the pointer on it and to
      // know where its middle is.
      gestures.setScreenSize(w, h)
      draw()
    },
    onLatency: (ms) => {
      latencyMs.value = ms
    },
    onStats: (s) => {
      hasStats.value = true
      streamFps.value = s.fps
      streamQuality.value = s.quality
      streamKbps.value = s.kbps
      streamIdle.value = s.idle
      streamLimited.value = s.limited
      streamVideo.value = s.video ?? ''
    },
    onClipboard: (result) => {
      void onClipboard(result)
    },
  })
  client.connect()

  window.addEventListener('keydown', onKeyDown, true)
  window.addEventListener('keyup', onKeyUp, true)
  window.addEventListener('blur', releaseAll)
  document.addEventListener('visibilitychange', onVisibilityChange)
  document.addEventListener('paste', onPaste)

  // The starting mode came from storage or a device guess, neither of which the
  // toolbar can see, so report it now that there is something listening.
  emit('inputMode', inputMode.value)
  emit('keyboard', keyboardOn.value)

  // A device with the keyboard switched on wants the sink focused up front so
  // the first keystroke is not a no-op; one with it switched off wants the page
  // to come up without a soft keyboard -- `focusSink` decides which this is.
  focusSink()
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeyDown, true)
  window.removeEventListener('keyup', onKeyUp, true)
  window.removeEventListener('blur', releaseAll)
  document.removeEventListener('visibilitychange', onVisibilityChange)
  document.removeEventListener('paste', onPaste)
  if (tickTimer !== null) window.clearInterval(tickTimer)
  resizeObserver?.disconnect()
  disableAudio()
  releaseH264()
  client?.close()
  client = null
})

function onVisibilityChange() {
  if (document.hidden) {
    // Timers are throttled in a hidden tab, so a gesture in flight would freeze
    // half-way with its button still down. Let go now instead.
    releaseAll()
    return
  }
  // Browsers throttle timers in hidden tabs, so the backoff timer may not have
  // fired while the user was away. Catch up as soon as the tab returns.
  client?.retryNow()
}

// --- canvas --------------------------------------------------------------

function resizeCanvas() {
  const canvas = canvasEl.value
  const wrap = wrapEl.value
  if (!canvas || !wrap) return

  const dpr = window.devicePixelRatio || 1
  const width = Math.max(1, Math.round(wrap.clientWidth * dpr))
  const height = Math.max(1, Math.round(wrap.clientHeight * dpr))

  if (canvas.width !== width || canvas.height !== height) {
    canvas.width = width
    canvas.height = height
  }
  draw()
  // After draw, not before: the touch scale is derived from where the picture
  // ended up, and only draw() recomputes that.
  updateTouchScale()
}

/** Recomputes the letterboxed destination rect for the current canvas size. */
function layout() {
  const canvas = canvasEl.value
  if (!canvas || !remoteW.value || !remoteH.value) return false

  const scale = Math.min(canvas.width / remoteW.value, canvas.height / remoteH.value)
  const dw = remoteW.value * scale
  const dh = remoteH.value * scale
  view = {
    ox: (canvas.width - dw) / 2,
    oy: (canvas.height - dh) / 2,
    dw,
    dh,
  }
  return true
}

/**
 * Paints the retained frame. Called on resize as well as after each decode, so
 * the picture survives a window resize instead of going blank.
 */
function draw() {
  const canvas = canvasEl.value
  if (!canvas || !ctx) return
  if (lastBitmap) {
    paint(lastBitmap)
    return
  }
  ctx.fillStyle = '#0b0d10'
  ctx.fillRect(0, 0, canvas.width, canvas.height)
}

function paint(frame: Frame) {
  const canvas = canvasEl.value
  if (!canvas || !ctx || !layout()) return

  ctx.fillStyle = '#0b0d10'
  ctx.fillRect(0, 0, canvas.width, canvas.height)
  ctx.imageSmoothingEnabled = true
  ctx.drawImage(frame, view.ox, view.oy, view.dw, view.dh)
}

async function drainFrames() {
  if (decoding) return
  decoding = true

  try {
    while (pendingFrame) {
      const blob = pendingFrame
      pendingFrame = null

      const bitmap = await decodeFrame(blob)
      if (!bitmap) continue
      retain(bitmap)
    }
  } finally {
    decoding = false
  }
}

/**
 * Puts a decoded frame on the canvas and keeps it for repainting.
 *
 * The previous frame is released immediately rather than at the next garbage
 * collection: an ImageBitmap and a VideoFrame both hold GPU memory that the
 * browser has no way to reclaim while a reference is live, and a screen that
 * changes every frame would otherwise accumulate a second of them.
 */
function retain(frame: Frame) {
  const previous = lastBitmap
  lastBitmap = frame
  paint(frame)
  if (previous && 'close' in previous) previous.close()
}

// --- inter-frame ---------------------------------------------------------

/**
 * The H.264 player, created on the first inter-frame message.
 *
 * Lazily, because a server with no encoder or a JPEG-only stream should never
 * see a decoder built: it costs a decoder instance, a thread, and a hardware
 * decode session on a machine that may have a limit on those.
 */
function h264Player(): H264Player {
  if (!h264) {
    h264 = new H264Player(retain, () => {
      // The decoder has failed repeatedly without producing a frame, so the
      // stream is not going to be decoded on this browser. The claim that was
      // made in the connection URL is what the server encodes by, so it can
      // only be withdrawn by connecting again without it -- the server then
      // moves the stream to intra-frame for good.
      console.warn('falling back to intra-frame: reconnecting without H.264 support')
      releaseH264()
      client?.disableH264()
      client?.reconnect()
    })
  }
  return h264
}

function releaseH264() {
  h264?.close()
  h264 = null
}

async function decodeFrame(blob: Blob): Promise<Frame | null> {
  if (typeof createImageBitmap === 'function') {
    try {
      return await createImageBitmap(blob)
    } catch {
      return null
    }
  }
  // Fallback for browsers without createImageBitmap: route through <img>.
  return await new Promise<Frame | null>((resolve) => {
    const url = URL.createObjectURL(blob)
    const img = new Image()
    img.onload = () => {
      URL.revokeObjectURL(url)
      resolve(img)
    }
    img.onerror = () => {
      URL.revokeObjectURL(url)
      resolve(null)
    }
    img.src = url
  })
}

function countFrame() {
  const now = performance.now()
  if (now - fpsWindowStart >= 1000) {
    receivedFps.value = Math.round((fpsWindowCount * 1000) / (now - fpsWindowStart))
    fpsWindowStart = now
    fpsWindowCount = 0
  }
  fpsWindowCount++
}

// --- coordinate mapping --------------------------------------------------

/**
 * Maps a pointer event to remote screen pixels.
 *
 * The capture may be scaled (server `-scale`), so the conversion goes through
 * the normalised position inside the painted image rather than its pixel
 * dimensions: a fraction of the frame is the same fraction of the desktop.
 */
function toRemote(clientX: number, clientY: number): { x: number; y: number } | null {
  const canvas = canvasEl.value
  if (!canvas || !view.dw || !view.dh) return null

  const rect = canvas.getBoundingClientRect()
  const dpr = canvas.width / rect.width

  // Pointer position in canvas device pixels.
  const px = (clientX - rect.left) * dpr
  const py = (clientY - rect.top) * dpr

  const fx = (px - view.ox) / view.dw
  const fy = (py - view.oy) / view.dh

  // Outside the painted area (letterbox bars): clamp so a drag that strays into
  // the bar keeps tracking along the edge instead of jumping.
  const cx = Math.min(1, Math.max(0, fx))
  const cy = Math.min(1, Math.max(0, fy))

  return {
    x: Math.round(cx * (remoteW.value - 1)),
    y: Math.round(cy * (remoteH.value - 1)),
  }
}

// --- input mode ----------------------------------------------------------

const INPUT_MODE_KEY = 'webvnc.inputMode'

/**
 * The mode to start in.
 *
 * A device whose primary pointer is coarse (a phone, a tablet) is almost
 * certainly being driven by a finger, and absolute positioning is what a
 * touchscreen user expects, so that is the default there. A remembered choice
 * always wins over the guess.
 */
function loadInputMode(): InputMode {
  try {
    const saved = localStorage.getItem(INPUT_MODE_KEY)
    if (saved === 'mouse' || saved === 'touch') return saved
  } catch {
    // Safari in private mode throws on storage access. Not being able to
    // remember the choice is no reason to refuse to start.
  }
  return prefersTouchMode() ? 'touch' : 'mouse'
}

const inputMode = ref<InputMode>(loadInputMode())

const gestures = new TouchGestures({
  mode: inputMode.value,
  toRemote,
  cssToRemoteX: 1,
  cssToRemoteY: 1,
})

/** Touch/pen pointers currently down, by id. */
const activeTouches = new Set<number>()

/** Physical-mouse buttons the remote thinks are down. */
const heldMouseButtons = new Set<string>()

/**
 * Whether this pointer should be read as a finger.
 *
 * Only a real mouse keeps absolute positioning, in both modes: turning a mouse
 * into a relative device is what a trackpad already is, and a user who plugged
 * one in would just think it had broken.
 */
function isTouchLike(e: PointerEvent): boolean {
  return e.pointerType === 'touch' || e.pointerType === 'pen'
}

/**
 * Recalibrates touch movement to the current picture size.
 *
 * The scale is defined so that dragging a finger across the whole displayed
 * image moves the pointer across the whole remote desktop, which keeps the feel
 * the same regardless of the server's `-scale` or the window size.
 */
function updateTouchScale() {
  const canvas = canvasEl.value
  if (!canvas || !remoteW.value || !remoteH.value) return
  if (!view.dw || !view.dh || !canvas.width || !canvas.height) return

  const rect = canvas.getBoundingClientRect()
  if (!rect.width || !rect.height) return

  // Measured against the painted image rather than the canvas element: the
  // picture is letterboxed, and dragging across the visible image should cross
  // the whole desktop however thick the bars around it happen to be. (view is
  // in device pixels, so its share of the canvas is scaled back to CSS ones.)
  const paintedW = (view.dw / canvas.width) * rect.width
  const paintedH = (view.dh / canvas.height) * rect.height
  if (!paintedW || !paintedH) return

  gestures.setScale(remoteW.value / paintedW, remoteH.value / paintedH)
}

function setInputMode(mode: InputMode) {
  if (mode === inputMode.value) return

  inputMode.value = mode
  applyActions(gestures.setMode(mode))
  activeTouches.clear()
  syncTickTimer()
  dropping.value = false

  try {
    localStorage.setItem(INPUT_MODE_KEY, mode)
  } catch {
    // Storage is a convenience; the choice still holds for this session.
  }

  stateDetail.value =
    mode === 'touch' ? '触屏模式：点哪就是哪' : '鼠标模式：滑动即移动指针'
  emit('inputMode', mode)
}

// --- soft keyboard -------------------------------------------------------

const KEYBOARD_KEY = 'webvnc.keyboard'

/**
 * Whether the soft keyboard starts enabled.
 *
 * A device whose primary pointer is coarse -- a phone, a tablet -- is driven by
 * a finger, and focusing the sink on a tap is what raises the OS keyboard. So
 * there it starts off, and the toolbar button is the only thing that raises it.
 * Anywhere else the old behaviour is kept: the sink holds focus permanently,
 * which is invisible because such a device has no soft keyboard to show.
 *
 * A remembered choice always wins over the device guess, the same as the input
 * mode. Stored as 'on'/'off' rather than a stringified boolean so the parse
 * stays a whitelist.
 */
function loadKeyboard(): boolean {
  try {
    const saved = localStorage.getItem(KEYBOARD_KEY)
    if (saved === 'on') return true
    if (saved === 'off') return false
  } catch {
    // Safari in private mode throws on storage access. Not being able to
    // remember the choice is no reason to refuse to start.
  }
  return !prefersTouchMode()
}

const keyboardOn = ref(loadKeyboard())

/**
 * Switches the soft keyboard on or off.
 *
 * Off is not "disable typing": it means the sink stops holding focus, which is
 * the only thing that raises an OS keyboard. Hardware and Bluetooth keyboards
 * are unaffected -- `isRemoteTarget` accepts any non-form target, so the
 * capture-phase window listeners keep forwarding keys while focus sits on the
 * body. What off does cost is the local IME (composition needs a focused
 * editable) and the mobile keyboard's own "paste" affordance; both come back
 * with the switch.
 *
 * The `inputmode` attribute is written *before* `focus()` rather than left to
 * the reactive binding. A browser reads it at the moment focus lands, and Vue
 * patches attribute bindings on the next tick -- so a reactive-only write would
 * arrive after focus and, on a platform that honours `inputmode="none"`, the
 * keyboard would simply not come up. Writing it here is exactly what makes the
 * attribute safe to keep at all: while off it is `none` (so a focus that
 * arrives from anywhere else -- Tab, bfcache, focus restoration -- still cannot
 * raise a keyboard), and it is already back to `text` at the instant we focus.
 */
function setKeyboard(on: boolean) {
  // Unreachable from the toolbar, which hides the button in live mode, but this
  // is also the path the saved preference restores through, and switching the
  // keyboard *on* for a viewer would both lie in `stateDetail` and pop a
  // keyboard over the picture.
  if (viewOnly.value && on) return
  if (on === keyboardOn.value) return

  const el = inputEl.value
  if (el) el.setAttribute('inputmode', on ? 'text' : 'none')

  keyboardOn.value = on

  if (on) {
    focusSinkNow()
  } else {
    // `blur` and not the attribute alone: changing `inputmode` on an element
    // that is already focused does not reliably retract the keyboard on either
    // mobile platform. The blur also fires `releaseAllKeys`, which is wanted --
    // a key held as the keyboard goes away can never be paired with its keyup.
    el?.blur()
  }

  try {
    localStorage.setItem(KEYBOARD_KEY, on ? 'on' : 'off')
  } catch {
    // Storage is a convenience; the choice still holds for this session.
  }

  stateDetail.value = on
    ? '键盘已开启：可用本机输入法输入中文'
    : '键盘已关闭：点画面只操作远程，要输入时点工具栏「键盘」'
  emit('keyboard', on)
}

/**
 * Takes back the keyboard that the mount-time focus may have already raised,
 * once the server turns out to be in live mode.
 *
 * The flag rides on the first `screen` message, a round trip after the socket
 * opens, while `onMounted` focuses the sink synchronously -- so on a device
 * whose keyboard defaults on there can be a soft keyboard up before we learn
 * the stream is read-only. The guards elsewhere stop the *next* focus; this
 * deals with the one that already happened.
 *
 * It deliberately does not go through `setKeyboard`, which would write the
 * viewer's stored preference to disk: live mode overriding the UI for this
 * session is not the viewer changing their mind about their own device.
 */
function retractViewOnlyKeyboard() {
  if (!keyboardOn.value) return
  keyboardOn.value = false
  inputEl.value?.setAttribute('inputmode', 'none')
  // Blur rather than just the attribute: changing `inputmode` on an element
  // that is already focused does not reliably retract a raised keyboard, the
  // same reason `setKeyboard` blurs.
  inputEl.value?.blur()
  emit('keyboard', false)
}

// --- mouse ---------------------------------------------------------------

const BUTTONS: Record<number, string> = {
  0: 'left',
  1: 'middle',
  2: 'right',
  3: 'back',
  4: 'forward',
}

/** Latest position during a move burst; flushed once per animation frame. */
let pendingMove: { x: number; y: number } | null = null
let moveScheduled = false

function flushMove() {
  if (!pendingMove) return
  client?.send({ type: 'mousemove', x: pendingMove.x, y: pendingMove.y })
  pendingMove = null
}

/**
 * Queues a move for the next animation frame.
 *
 * Coalescing matters: a 120 Hz mouse would otherwise flood the socket with
 * events the 15 fps capture loop can never show.
 */
function scheduleMove() {
  if (moveScheduled) return
  moveScheduled = true
  requestAnimationFrame(() => {
    moveScheduled = false
    flushMove()
  })
}

/**
 * Sends what the gesture recogniser decided the user meant.
 *
 * A move is flushed before any button event rather than being left to the next
 * animation frame: a tap arrives as a move and a click in the same batch, and
 * coalescing the first would let the click land at the pointer's previous
 * position.
 */
function applyActions(actions: TouchAction[]) {
  for (const a of actions) {
    switch (a.kind) {
      case 'moveTo':
        pendingMove = { x: a.x, y: a.y }
        scheduleMove()
        break
      case 'button':
        flushMove()
        client?.send({ type: a.down ? 'mousedown' : 'mouseup', button: a.button })
        break
      case 'click':
        flushMove()
        client?.send({ type: 'mousedown', button: a.button })
        client?.send({ type: 'mouseup', button: a.button })
        break
      case 'wheel':
        client?.send({ type: 'wheel', deltaX: a.dx, deltaY: a.dy })
        break
      case 'busy':
        dropping.value = a.value
        break
    }
  }
}

/** How often a held finger's timers are checked while a gesture is in flight. */
const TICK_MS = 25

let tickTimer: number | null = null

/**
 * Runs the gesture clock, but only while a finger is actually down.
 *
 * The long press and the drag arming both need a timer, and the recogniser
 * deliberately owns no clock of its own, so something has to drive it. Stopping
 * the timer the moment the last finger lifts keeps an idle page from waking up
 * forty times a second for nothing.
 */
function syncTickTimer() {
  if (activeTouches.size > 0) {
    if (tickTimer === null) {
      tickTimer = window.setInterval(() => {
        applyActions(gestures.tick(performance.now()))
      }, TICK_MS)
    }
    return
  }
  if (tickTimer !== null) {
    window.clearInterval(tickTimer)
    tickTimer = null
  }
}

/**
 * Releases everything the remote still thinks is held.
 *
 * The pointer equivalent of releaseAllKeys, and needed for the same reason: a
 * finger that vanishes mid-drag -- a tab switch, an incoming call, a lost
 * pointer capture -- would otherwise leave the left button down on the remote
 * machine with nothing left to release it.
 */
function releaseAllButtons() {
  for (const button of heldMouseButtons) {
    client?.send({ type: 'mouseup', button })
  }
  heldMouseButtons.clear()

  applyActions(gestures.reset())
  activeTouches.clear()
  syncTickTimer()
  dropping.value = false
}

function onPointerDown(e: PointerEvent) {
  // Live mode: the picture is inert, so this returns before `preventDefault`
  // too -- a viewer keeps whatever browser gestures (scroll, zoom) they would
  // have had over an image. Nothing below can have left state behind to clean
  // up, because the flag is fixed for the life of the connection and was set
  // before the first pointer event could arrive.
  if (viewOnly.value) return

  // Gated: on a phone this is the tap that used to raise the soft keyboard on
  // every touch. A tap while the keyboard is already on deliberately does not
  // dismiss it -- only the toolbar button does that.
  focusSink()

  if (isTouchLike(e)) {
    // Nothing is painted yet, so there is nothing to have touched.
    if (!view.dw || !view.dh) return

    e.preventDefault()
    // Capture so a drag keeps being delivered if the finger strays off the
    // canvas or out of the window.
    canvasEl.value?.setPointerCapture(e.pointerId)
    updateTouchScale()
    activeTouches.add(e.pointerId)
    applyActions(
      gestures.pointerDown(e.pointerId, e.clientX, e.clientY, performance.now()),
    )
    syncTickTimer()
    return
  }

  const p = toRemote(e.clientX, e.clientY)
  if (!p) return

  e.preventDefault()
  canvasEl.value?.setPointerCapture(e.pointerId)
  dropping.value = true

  pendingMove = { x: p.x, y: p.y }
  flushMove()
  const button = BUTTONS[e.button]
  if (button) {
    heldMouseButtons.add(button)
    client?.send({ type: 'mousedown', button })
  }
}

function onPointerMove(e: PointerEvent) {
  if (viewOnly.value) return

  if (isTouchLike(e)) {
    if (!activeTouches.has(e.pointerId)) return
    e.preventDefault()
    applyActions(gestures.pointerMove(e.pointerId, e.clientX, e.clientY))
    return
  }

  const p = toRemote(e.clientX, e.clientY)
  if (!p) return
  pendingMove = p
  scheduleMove()
}

function onPointerUp(e: PointerEvent) {
  if (viewOnly.value) return

  if (isTouchLike(e)) {
    if (!activeTouches.has(e.pointerId)) return
    e.preventDefault()
    activeTouches.delete(e.pointerId)
    applyActions(
      gestures.pointerUp(e.pointerId, e.clientX, e.clientY, performance.now()),
    )
    syncTickTimer()

    const canvas = canvasEl.value
    if (canvas?.hasPointerCapture(e.pointerId)) {
      canvas.releasePointerCapture(e.pointerId)
    }
    return
  }

  const button = BUTTONS[e.button]
  if (button) {
    e.preventDefault()
    flushMove()
    heldMouseButtons.delete(button)
    client?.send({ type: 'mouseup', button })
  }
  dropping.value = false

  const canvas = canvasEl.value
  if (canvas?.hasPointerCapture(e.pointerId)) {
    canvas.releasePointerCapture(e.pointerId)
  }
}

function onPointerCancel(e: PointerEvent) {
  // Live mode never recorded a press, so there is nothing to release. Checked
  // rather than assumed so the `mouseup` frames below are not sent on the way
  // to being dropped by the server.
  if (viewOnly.value) return

  // The pointer was lost, so what we think is held can no longer be trusted:
  // release all three buttons rather than only the ones we remember pressing.
  for (const button of ['left', 'middle', 'right']) {
    client?.send({ type: 'mouseup', button })
  }
  heldMouseButtons.clear()
  activeTouches.delete(e.pointerId)
  applyActions(gestures.reset())
  syncTickTimer()
  dropping.value = false
}

function onWheel(e: WheelEvent) {
  if (viewOnly.value) return

  e.preventDefault()

  // Normalise deltaMode to pixels; the server converts pixels to Windows wheel
  // notches and carries the remainder for trackpads.
  let { deltaX, deltaY } = e
  if (e.deltaMode === 1) {
    deltaX *= 16
    deltaY *= 16
  } else if (e.deltaMode === 2) {
    const h = wrapEl.value?.clientHeight ?? 800
    deltaX *= h
    deltaY *= h
  }

  client?.send({ type: 'wheel', deltaX, deltaY })
}

function onContextMenu(e: MouseEvent) {
  // Live mode leaves the browser's own menu alone, matching the pointer
  // handlers: nothing here is a remote click then, so there is no reason to
  // hide the menu that belongs to the picture.
  if (viewOnly.value) return
  // The right button is a remote click, not a local menu.
  e.preventDefault()
}

// --- keyboard ------------------------------------------------------------

/** True while the local IME is composing; key events are suppressed then. */
let composing = false

/** Keys currently held down, so they can be released if focus is lost. */
const heldKeys = new Map<string, { key: string; code: string }>()

/** Keys the browser keeps for itself so the user can always escape. */
const LOCAL_KEYS = new Set(['F5', 'F11', 'F12'])

/**
 * True between a Ctrl+C / Ctrl+X press and the release of the letter key.
 *
 * The pull has to wait for the keyup -- see `onKeyUp` -- and keyup cannot be
 * recognised by its own `ctrlKey`, because releasing Ctrl first (which plenty of
 * people do) leaves the letter's keyup reporting no modifier at all.
 */
let copiedChord = false

/** Guards against a second Ctrl+V landing while the clipboard read is in flight. */
let pastePending = false

/** Set once the "cannot write to the local clipboard" reason has been shown. */
let warnedNoClipboardWrite = false

/**
 * Set while a Ctrl+V is being handled through the clipboard API, so a `paste`
 * event that a browser fires anyway cannot deliver the same text twice.
 */
let suppressPasteUntil = 0

/**
 * Focuses the sink without asking whether it should.
 *
 * Only `setKeyboard` uses this, because it has just switched the keyboard on
 * and must focus inside the click's gesture -- deferring to the next tick would
 * step outside the user gesture and the OS would refuse to raise the keyboard.
 * Everywhere else goes through `focusSink`.
 */
function focusSinkNow() {
  inputEl.value?.focus({ preventScroll: true })
}

/**
 * Restores keyboard focus to the sink, but only while the keyboard is enabled.
 *
 * Focus here is a proxy for "raise the soft keyboard", not for "make typing
 * work". Hardware keys are forwarded by the capture-phase window listeners
 * whenever focus is on the body, so an unfocused sink still types; what focus
 * is actually needed for is the local IME and the mobile keyboard's paste
 * affordance. That is why this is a no-op once the user switches the keyboard
 * off, and why the off state is safe.
 *
 * Live mode is a second reason to do nothing, and a stronger one: a viewer
 * clicking the picture must not raise a keyboard they have no use for. Folding
 * it in here covers every caller, including the ones that only exist to keep
 * focus alive during a drag.
 */
function focusSink() {
  if (viewOnly.value) return
  if (!keyboardOn.value) return
  focusSinkNow()
}

function isComposingEvent(e: KeyboardEvent): boolean {
  // 229 is the legacy "IME is processing" code that some browsers still send
  // for keydown while a composition is active.
  return composing || e.isComposing || e.keyCode === 229
}

function onKeyDown(e: KeyboardEvent) {
  // Live mode: no key is forwarded, and none is intercepted either, so the page
  // keeps its ordinary browser shortcuts (reload, find, scroll). `preventDefault`
  // is inside the guard rather than above it for exactly that reason.
  if (viewOnly.value) return

  if (!isRemoteTarget(e.target)) return
  if (isComposingEvent(e)) return

  // Ctrl+V does not forward the keystroke: the local clipboard has to reach the
  // *remote* clipboard first, and `pasteOnRemote` synthesizes the Ctrl+V itself
  // once the text is on its way. The default is suppressed for the same reason
  // -- the browser's own paste would type into the hidden input instead.
  //
  // Without the clipboard API (a plain-http page) nothing here can read the
  // text, so the default is left alone and the `paste` event carries it.
  if (e.ctrlKey && e.code === 'KeyV') {
    if (!e.repeat && canReadClipboard()) {
      e.preventDefault()
      suppressPasteUntil = Date.now() + 500
      void pasteFromClipboard()
    }
    return
  }

  // Ctrl+C / Ctrl+X: the remote does the copying, and the result is collected on
  // keyup. The flag is what makes that work when Ctrl is released first.
  if (e.ctrlKey && (e.code === 'KeyC' || e.code === 'KeyX')) {
    copiedChord = true
  }

  if (!LOCAL_KEYS.has(e.code)) {
    // Stop the local browser from acting on the key (scrolling, shortcuts,
    // quick-find) -- the remote desktop is the focus.
    e.preventDefault()
  }

  if (e.repeat) {
    // Windows generates its own auto-repeat; forwarding the repeats the browser
    // synthesises would double up.
    return
  }

  heldKeys.set(e.code, { key: e.key, code: e.code })
  client?.send({ type: 'keydown', key: e.key, code: e.code })
}

function onKeyUp(e: KeyboardEvent) {
  if (viewOnly.value) return

  if (!isRemoteTarget(e.target)) return
  if (isComposingEvent(e)) return

  if (e.ctrlKey && e.code === 'KeyV') {
    e.preventDefault()
    return
  }

  // The release is the first moment the remote application is guaranteed to have
  // finished copying, which is why the pull happens here and not on the press:
  // SendInput only queues the key, so a pull sent from the keydown handler could
  // arrive before the application has written the new clipboard.
  if (e.code === 'KeyC' || e.code === 'KeyX') {
    if (copiedChord || e.ctrlKey) {
      copiedChord = false
      pullRemoteClipboard()
    }
  }

  if (!LOCAL_KEYS.has(e.code)) e.preventDefault()

  heldKeys.delete(e.code)
  client?.send({ type: 'keyup', key: e.key, code: e.code })
}

/**
 * Releases every key the remote still thinks is held.
 *
 * Switching tabs mid-chord would otherwise leave Ctrl or Shift stuck down on the
 * remote machine with no way to clear it.
 */
function releaseAllKeys() {
  for (const k of heldKeys.values()) {
    client?.send({ type: 'keyup', key: k.key, code: k.code })
  }
  heldKeys.clear()
}

/**
 * The "we have stopped receiving input" response: nothing may stay held.
 *
 * Keys and buttons are released together because they are lost together -- the
 * window blurred, or the tab was hidden -- and a chord half-held on the remote
 * machine is no more recoverable than a button half-pressed.
 */
function releaseAll() {
  releaseAllKeys()
  releaseAllButtons()
}

/**
 * Decides whether a key event belongs to the remote desktop.
 *
 * The hidden input is the normal target, but the remote keyboard should keep
 * working wherever focus happens to be -- the whole page is the remote console,
 * and a stray click on the chrome should not silently mute the keyboard. Real
 * form fields are excluded so the UI stays typeable if any are added later.
 */
function isRemoteTarget(target: EventTarget | null): boolean {
  if (target === inputEl.value) return true

  const el = target as HTMLElement | null
  if (!el || !(el instanceof HTMLElement)) return false
  if (el.isContentEditable) return false

  switch (el.tagName) {
    case 'INPUT':
    case 'TEXTAREA':
    case 'SELECT':
      return false
    default:
      return true
  }
}

// --- IME -----------------------------------------------------------------

function onCompositionStart() {
  composing = true
}

function onCompositionEnd(e: CompositionEvent) {
  composing = false
  // Live mode should not reach here -- the sink is never focused, so no IME can
  // run -- but the check keeps the invariant local to the path that would send
  // the text, rather than resting on a fact established three functions away.
  if (viewOnly.value) return
  const text = e.data
  if (text) {
    client?.send({ type: 'text_input', text })
  }
  // Drop the locally accumulated text: the hidden input is only a vehicle for
  // the IME, never a place where text should live.
  if (inputEl.value) inputEl.value.value = ''
}

/**
 * The `paste` event, kept for the one case the clipboard API cannot serve.
 *
 * On a secure origin Ctrl+V never reaches here: `onKeyDown` reads the clipboard
 * itself and suppresses the default. This is the plain-http fallback, where
 * `navigator.clipboard` does not exist at all but the paste event still carries
 * the text. Listened for on the document, not on the keyboard sink, because a
 * sink that has quietly lost focus swallows the keystroke with no error
 * anywhere -- which is indistinguishable from the feature being broken.
 */
function onPaste(e: ClipboardEvent) {
  // Live mode: a paste is a write to the remote clipboard, which is more than
  // watching. The default is left alone, so the browser pastes normally.
  if (viewOnly.value) return
  if (Date.now() < suppressPasteUntil) return
  // A real form field keeps its own paste; nothing else here does. The target
  // can also be the document itself when no element holds focus, and that is
  // still the remote console -- which is exactly the case this listener exists
  // to catch, so it must not be filtered out along with the form fields.
  if (e.target instanceof HTMLElement && !isRemoteTarget(e.target)) return

  const text = e.clipboardData?.getData('text')
  if (!text) {
    stateDetail.value = '剪贴板里没有文本'
    return
  }
  e.preventDefault()
  pasteOnRemote(text)
}

/**
 * Keeps the hidden input empty.
 *
 * The input is a vehicle for the IME, never a text buffer. Crucially, this must
 * NOT forward its contents: a physical key press produces both a `keydown` (which
 * is forwarded as a key event) and an `input` event, so sending the latter too
 * would type every character twice. Composed text arrives via `compositionend`
 * and pasted text via `paste`, both of which carry the whole string.
 */
function onInput() {
  // Never clear mid-composition: that would rip the text out from under the IME.
  if (composing) return
  const input = inputEl.value
  if (input && input.value) input.value = ''
}

/** The same ceiling the server enforces; checked here so a huge paste is
 *  refused before it trips the socket's inbound message limit. */
const MAX_CLIPBOARD_BYTES = 256 * 1024

/**
 * Ctrl+V, and the toolbar's 「粘贴文本」 button.
 *
 * Both do the same thing, so they cannot disagree about what pasting means.
 * `navigator.clipboard` only exists in a secure context -- https, localhost or
 * 127.0.0.1. Opened at a LAN or IPv6 address over plain http there is no
 * clipboard API at all, and no permission the user can grant brings it back;
 * there the `paste` event is the only route, and it needs the user's own
 * Ctrl+V to fire. So every failure here ends by naming the step that does work
 * rather than leaving a dead end.
 */
async function pasteFromClipboard() {
  if (pastePending) return
  pastePending = true
  try {
    const text = await readLocalClipboard()
    if (text !== null) pasteOnRemote(text)
  } finally {
    pastePending = false
  }
}

/**
 * Whether this page can read the local clipboard at all.
 *
 * A method call rather than a property test because `navigator.clipboard` is
 * only present in a secure context, and the typings do not say so.
 */
function canReadClipboard(): boolean {
  return typeof navigator.clipboard?.readText === 'function'
}

/** Reads the local clipboard, or null when the browser will not hand it over. */
async function readLocalClipboard(): Promise<string | null> {
  if (!canReadClipboard()) {
    // A LAN address is the usual reason, and there the address bar is the whole
    // fix: localhost is a secure context and a LAN address is not. Naming the
    // URL outright beats stating the rule and leaving the user to work it out.
    const port = location.port ? `:${location.port}` : ''
    promptManualPaste(`浏览器不在此地址提供剪贴板接口，改用 https 或 http://localhost${port}`)
    return null
  }
  try {
    return await navigator.clipboard.readText()
  } catch (err) {
    promptManualPaste(clipboardRefusal(err))
    return null
  }
}

/**
 * Delivers text to the remote clipboard and pastes it there.
 *
 * Not by typing the text, which is what this used to do: the text goes into the
 * *remote* clipboard and a real Ctrl+V follows, so the remote application runs
 * its own paste. That is what makes a terminal, a password field, or a "paste as
 * plain text" command behave the way it does locally.
 *
 * The order matters and is free: both messages travel the same socket and the
 * server handles them in arrival order, so the clipboard is written before the
 * key is pressed. The keystroke is synthesized here rather than forwarded from
 * the real one because reading the clipboard is asynchronous -- by the time it
 * resolves the user may already have released V, and forwarding that release
 * would deliver a keyup before its own keydown.
 */
function pasteOnRemote(text: string) {
  // Live mode: a paste writes to the remote clipboard and then presses Ctrl+V
  // over there, so it is the one input path that does two things. The check sits
  // here, on the funnel every caller passes through (the Ctrl+V handler, the
  // `paste` fallback and the toolbar button), rather than on each of them.
  if (viewOnly.value) return

  if (!text) {
    // Nothing of ours to put over there. The keystroke was suppressed on the way
    // in, so it still has to be pressed -- Ctrl+V should keep meaning Ctrl+V.
    stateDetail.value = '本机剪贴板没有文本，已按普通 Ctrl+V 转发'
    pressRemotePaste()
    return
  }

  // Counted in bytes because that is what bounds the message, and checked here
  // as well as on the server: exceeding the socket's inbound limit does not
  // return an error, it drops the connection.
  if (new TextEncoder().encode(text).length > MAX_CLIPBOARD_BYTES) {
    stateDetail.value = '剪贴板内容过大（上限 256 KB），未发送'
    return
  }

  client?.send({ type: 'clipboard_push', text })
  pressRemotePaste()
  stateDetail.value = `已粘贴 ${[...text].length} 个字符`
}

/** Presses Ctrl+V on the far side. Ctrl itself is already held over there. */
function pressRemotePaste() {
  client?.send({ type: 'keydown', key: 'v', code: 'KeyV' })
  client?.send({ type: 'keyup', key: 'v', code: 'KeyV' })
}

/**
 * Brings the remote clipboard's text into the local one.
 *
 * Driven by Ctrl+C / Ctrl+X and by the toolbar button. The button is what covers
 * a copy made any other way -- a context menu, or an application that copies on
 * its own -- since there is nothing to intercept in those cases.
 *
 * Where the browser cannot write to the clipboard this says so once per session
 * rather than on every Ctrl+C: Ctrl+C is pressed constantly in a remote session,
 * and a repeated complaint would bury everything else in the status line.
 */
function pullRemoteClipboard() {
  // Live mode: this reads the remote machine's clipboard out to the viewer,
  // which is a way of getting data off that machine and so not something
  // "watching the screen" includes. The server refuses it too.
  if (viewOnly.value) return
  if (!client) return

  if (!navigator.clipboard?.writeText) {
    if (!warnedNoClipboardWrite) {
      warnedNoClipboardWrite = true
      const port = location.port ? `:${location.port}` : ''
      stateDetail.value = `浏览器不在此地址提供剪贴板写入，改用 https 或 http://localhost${port}`
    }
    return
  }
  client.send({ type: 'clipboard_pull' })
}

/** Answers a clipboard_pull or clipboard_push. */
async function onClipboard(result: ClipboardResult) {
  if (result.op === 'push') {
    // The Ctrl+V has already gone out, so a failed push means the remote pasted
    // whatever was in its clipboard before. That is the one outcome the user
    // cannot see for themselves, so it is worth spelling out.
    if (!result.ok) {
      stateDetail.value = `远程剪贴板写入失败：${result.error}（远程可能粘贴了旧内容）`
    }
    return
  }

  if (!result.ok) {
    stateDetail.value = `读取远程剪贴板失败：${result.error}`
    return
  }
  if (!result.text) {
    stateDetail.value = '远程剪贴板里没有文本'
    return
  }

  const count = [...result.text].length
  try {
    await navigator.clipboard.writeText(result.text)
    stateDetail.value = `已复制 ${count} 个字符到本机`
  } catch (err) {
    stateDetail.value = `取到远程 ${count} 个字符，但写入本机剪贴板被拒绝：${clipboardRefusal(err)}`
  }
}

/**
 * Turns a clipboard failure into what the user has to do about it.
 *
 * Chrome reports a refused permission and an unfocused window under the same
 * NotAllowedError, and only the message separates them -- but the two ask
 * different things of the user, so they are worth telling apart.
 */
function clipboardRefusal(err: unknown): string {
  if (!(err instanceof DOMException)) return '读取剪贴板失败'
  if (err.name !== 'NotAllowedError') return '当前浏览器不支持直接读取剪贴板'
  return /focus/i.test(err.message)
    ? '浏览器要求窗口处于焦点状态才能读取剪贴板'
    : '剪贴板权限被拒绝（可在地址栏的权限设置里允许）'
}

/**
 * Points at the one step that always works, which depends on the keyboard.
 *
 * With the keyboard on, focusing the sink and pressing Ctrl+V is that step.
 * With it off there is no Ctrl+V to press on a phone, so the toolbar button --
 * which reads the clipboard through the API instead of the key event -- is the
 * step to name. The keyboard is deliberately not switched on here: a clipboard
 * failure should not raise the very keyboard this feature exists to keep down.
 */
function promptManualPaste(why: string) {
  focusSink()
  stateDetail.value = keyboardOn.value
    ? `${why}，请按 Ctrl+V`
    : `${why}，请点工具栏「粘贴文本」`
}

// --- audio ---------------------------------------------------------------

/**
 * Turns the remote sound on or off.
 *
 * Off is the default and not merely a preference: a browser will not start an
 * AudioContext until a user gesture asks for one, so this click is part of the
 * mechanism. Switching it off also closes the socket, which is what tells the
 * server to stop capturing -- there is no sense paying for capture nobody can
 * hear.
 */
function toggleAudio() {
  if (audioOn.value) {
    disableAudio()
    return
  }

  audioOn.value = true
  audioDetail.value = ''
  hasAudioStats.value = false
  audioKbps.value = 0
  audioIdle.value = false

  player = new AudioPlayer()
  // Opened here, synchronously, because this call stack *is* the gesture.
  // Deferring it to the format handshake would land outside the activation
  // window and leave the context suspended for good.
  player.enable()

  const c = new RemoteClient(
    props.token,
    {
      onState: (state, info) => {
        // A teardown already ran, or a newer socket took over.
        if (audioClient !== c) return
        switch (state) {
          case 'connected':
            audioDetail.value = ''
            break
          case 'reconnecting':
            audioDetail.value = '音频连接中断，重连中'
            break
          case 'closed':
          case 'unauthorized':
            disableAudio(info.detail ?? '音频连接已断开')
            break
        }
      },
      onAudioFormat: (format) => {
        // The wire format is fixed at 16-bit. Anything else would still be read
        // as 16-bit and come out as noise, so refuse it rather than play it.
        if (format.bits !== 16 || format.channels < 1) {
          disableAudio('服务端音频格式不受支持')
          return
        }
        player?.configure(format)
      },
      onAudio: (pcm) => player?.push(pcm),
      onAudioStats: (stats) => {
        hasAudioStats.value = true
        audioKbps.value = stats.kbps
        audioIdle.value = stats.idle
      },
    },
    // A socket of its own, separate from the video's: a 200 kB JPEG takes real
    // time to cross a slow link, and 20 ms of audio queued behind it would be
    // an audible gap. WebSocket has no priority, so the only fix is a second
    // connection.
    { path: '/ws/audio', kind: 'audio', maxAttempts: 4 },
  )
  audioClient = c
  c.connect()
  emit('audio', true)
}

/**
 * Stops playback and drops the connection. `detail` is shown in the HUD and is
 * set only when the stop was not the user's own doing.
 */
function disableAudio(detail = '') {
  // Cleared before closing: close() reports 'closed' synchronously, and the
  // handler above would otherwise re-enter this function.
  const c = audioClient
  audioClient = null
  c?.close()

  player?.disable()
  player = null

  audioOn.value = false
  audioDetail.value = detail
  hasAudioStats.value = false
  audioKbps.value = 0
  audioIdle.value = false
  emit('audio', false)
}

// --- fullscreen ----------------------------------------------------------

function onFullscreenChange() {
  // The canvas box changed size; the ResizeObserver covers it, but the
  // fullscreen transition can finish after the observer fires.
  requestAnimationFrame(resizeCanvas)
  // Gated too: entering or leaving fullscreen is a gesture, and on a phone it
  // must not be the thing that raises the keyboard.
  focusSink()
}

async function toggleFullscreen() {
  try {
    if (document.fullscreenElement) {
      await document.exitFullscreen()
    } else {
      await wrapEl.value?.requestFullscreen()
    }
  } catch {
    stateDetail.value = '当前浏览器不允许全屏'
  }
}

onMounted(() => document.addEventListener('fullscreenchange', onFullscreenChange))
onBeforeUnmount(() => document.removeEventListener('fullscreenchange', onFullscreenChange))

defineExpose({
  pasteFromClipboard,
  pullRemoteClipboard,
  toggleFullscreen,
  toggleAudio,
  setInputMode,
  setKeyboard,
})
</script>

<template>
  <div ref="wrapEl" class="screen-wrap" @contextmenu="onContextMenu">
    <canvas
      ref="canvasEl"
      class="screen-canvas"
      :class="{ dropping }"
      @pointerdown="onPointerDown"
      @pointermove="onPointerMove"
      @pointerup="onPointerUp"
      @pointercancel="onPointerCancel"
      @wheel="onWheel"
    />

    <!--
      The keyboard/IME sink. It is invisible but must remain focusable and be a
      real editable element, otherwise the browser will not run an IME or fire
      composition/paste events on it.

      `inputmode` is the declarative half of the keyboard switch: `none` while
      the keyboard is off, so that focus arriving from anywhere this component
      does not control -- Tab, bfcache restore, the browser putting focus back
      after a dialog -- still cannot raise an OS keyboard. `setKeyboard` writes
      the same value imperatively first, because focus must happen inside the
      click's gesture and a reactive patch would land a tick too late.
    -->
    <input
      ref="inputEl"
      class="key-sink"
      type="text"
      :inputmode="keyboardOn ? 'text' : 'none'"
      autocomplete="off"
      autocorrect="off"
      autocapitalize="off"
      spellcheck="false"
      aria-label="远程键盘输入"
      @compositionstart="onCompositionStart"
      @compositionend="onCompositionEnd"
      @input="onInput"
      @blur="releaseAllKeys"
    />

    <div v-if="connState === 'reconnecting'" class="overlay">
      <div class="overlay-card">
        <div class="spinner" />
        <p class="overlay-title">{{ statusText }}</p>
        <p class="overlay-sub">
          与远程桌面的连接已断开，正在自动重试{{ retryInMs ? `（${Math.round(retryInMs / 100) / 10}s 后）` : '' }}
        </p>
      </div>
    </div>

    <div v-else-if="connState === 'unauthorized'" class="overlay">
      <div class="overlay-card">
        <p class="overlay-title">口令已失效</p>
        <p class="overlay-sub">请重新登录后再连接。</p>
      </div>
    </div>

    <div class="hud">
      <!--
        First in the line and tinted, because it changes what everything after
        it means: without it a viewer would read "已连接 · 1280x800 · 12 fps"
        as a session they are driving.
      -->
      <span v-if="viewOnly" class="live-badge">直播模式 · 只能观看</span>
      <span class="dot" :class="statusClass" />
      <span>{{ statusText }}</span>
      <span class="sep">·</span>
      <span>{{ resolutionText }}</span>
      <span class="sep">·</span>
      <!-- While the screen is still no frames arrive, so the counted rate would
           sit frozen at its last value. Say "静止" instead of showing a stale
           number that reads as a stall. -->
      <span v-if="streamIdle" class="detail">静止</span>
      <span v-else>{{ receivedFps }} fps</span>
      <template v-if="connState === 'connected'">
        <span class="sep">·</span>
        <span>{{ latencyMs }} ms</span>
        <template v-if="hasStats">
          <span class="sep">·</span>
          <span>{{ bitrateText }}</span>
          <template v-if="videoText">
            <span class="sep">·</span>
            <span>{{ videoText }}</span>
          </template>
          <span v-if="streamLimited" class="detail">已限速 {{ limitedText }}</span>
        </template>
      </template>
      <span class="sep">·</span>
      <!-- Amber whenever there is no sound, matching "静止" above: both mean the
           server is deliberately sending nothing, not that something broke. -->
      <span v-if="audioIdle || !audioOn" class="detail">{{ audioText }}</span>
      <span v-else>{{ audioText }}</span>
      <span v-if="audioDetail" class="detail">{{ audioDetail }}</span>
      <span v-if="stateDetail" class="detail">{{ stateDetail }}</span>
    </div>
  </div>
</template>

<style scoped>
.screen-wrap {
  position: relative;
  width: 100%;
  height: 100%;
  overflow: hidden;
  background: #0b0d10;
}

.screen-canvas {
  display: block;
  width: 100%;
  height: 100%;
  cursor: default;
  touch-action: none;
  outline: none;
  /* Without this, a long press opens iOS's own selection bubble instead of
     being delivered to us as the right click it is meant to be. */
  -webkit-touch-callout: none;
  -webkit-user-select: none;
  user-select: none;
}

.screen-canvas.dropping {
  cursor: grabbing;
}

/* Invisible but real: an IME will not attach to a display:none / 0x0 element,
   and pointer-events:none keeps it from stealing clicks from the canvas. */
.key-sink {
  position: absolute;
  inset: 0;
  width: 100%;
  height: 100%;
  padding: 0;
  border: 0;
  opacity: 0;
  background: transparent;
  color: transparent;
  caret-color: transparent;
  pointer-events: none;
  cursor: default;
}

.overlay {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  background: rgba(8, 10, 13, 0.72);
  backdrop-filter: blur(2px);
  pointer-events: none;
}

.overlay-card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.5rem;
  padding: 1.5rem 2rem;
  border-radius: 12px;
  background: rgba(24, 27, 33, 0.95);
  border: 1px solid rgba(255, 255, 255, 0.08);
  text-align: center;
}

.overlay-title {
  margin: 0;
  font-size: 1rem;
  font-weight: 600;
}

.overlay-sub {
  margin: 0;
  font-size: 0.82rem;
  color: #98a2b3;
}

.spinner {
  width: 22px;
  height: 22px;
  border: 2px solid rgba(255, 255, 255, 0.18);
  border-top-color: #4f8cff;
  border-radius: 50%;
  animation: spin 0.8s linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

.hud {
  position: absolute;
  left: 12px;
  bottom: 12px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 10px;
  border-radius: 999px;
  font-size: 12px;
  font-variant-numeric: tabular-nums;
  color: #d0d5dd;
  background: rgba(15, 18, 23, 0.72);
  border: 1px solid rgba(255, 255, 255, 0.07);
  pointer-events: none;
  user-select: none;
}

.hud .sep {
  color: #4a5261;
}

/* Tinted rather than a plain HUD span, so the mode reads as a standing fact
   about the session and not as one more live measurement scrolling past. */
.live-badge {
  padding: 1px 8px;
  border-radius: 999px;
  font-weight: 600;
  color: #10202f;
  background: #6cc0f5;
}

.hud .detail {
  color: #f0a94a;
}

.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #8892a0;
}

.dot.ok {
  background: #32d583;
  box-shadow: 0 0 6px rgba(50, 213, 131, 0.8);
}

.dot.warn {
  background: #f0a94a;
}

.dot.bad {
  background: #f04438;
}
</style>
