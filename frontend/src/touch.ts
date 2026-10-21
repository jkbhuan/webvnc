/**
 * Touch gesture recognition for the remote desktop surface.
 *
 * This lives apart from RemoteScreen.vue, and imports nothing, for one reason:
 * the substance of the feature is a state machine over "mode x gesture x how
 * many fingers", which is impossible to reason about while it is tangled up
 * with canvas layout and socket plumbing. Keeping it here also means the
 * timers can be exercised deterministically -- see below.
 *
 * Time is passed in rather than read from a clock. Every timer-driven decision
 * (the long press, arming a drag) is therefore reproducible by calling `tick`
 * with a synthetic timestamp instead of waiting for real milliseconds, which is
 * the only way this logic can be checked without a touchscreen.
 *
 * The machine emits intentions; it never talks to the network. The caller turns
 * each action into a websocket message. Coordinates it emits are already in
 * remote-desktop pixels.
 */

export type InputMode = 'mouse' | 'touch'

export type TouchAction =
  /** Move the remote pointer to an absolute position. */
  | { kind: 'moveTo'; x: number; y: number }
  /** Press or release a mouse button. */
  | { kind: 'button'; button: string; down: boolean }
  /** A complete press-and-release. */
  | { kind: 'click'; button: string }
  /** Scroll, in the browser's wheel convention (positive y scrolls down). */
  | { kind: 'wheel'; dx: number; dy: number }
  /** Whether a button is being held, so the canvas can show a grabbing cursor. */
  | { kind: 'busy'; value: boolean }

/**
 * How long a finger must rest in one place before it means "right click".
 *
 * 500ms is the long-press timeout Android and iOS use for their own
 * press-and-hold affordances, so it matches what the hand already expects.
 */
const LONG_PRESS_MS = 500

/**
 * How long the second press of a tap-then-hold must last to become a drag.
 *
 * Short, because the alternative reading of that same press -- the second half
 * of a double click -- is already decided by the time this expires, and a
 * double click that felt sluggish would be much more annoying than a drag that
 * starts a fraction early. Moving the finger also starts the drag immediately,
 * so this delay is only ever felt by someone holding perfectly still.
 */
const DRAG_ARM_MS = 200

/** How soon after a tap the next press still counts as its continuation. */
const DOUBLE_TAP_MS = 400

/**
 * How far a finger may drift and still count as a tap.
 *
 * A finger is not a mouse: it rolls slightly as it touches down and lifts, so
 * this is loose enough to absorb that. Too loose and a deliberate short drag
 * would register as a click instead.
 */
const MOVE_TOLERANCE_PX = 12

type Phase = 'idle' | 'press' | 'moved' | 'dragArm' | 'dragHold' | 'scroll' | 'done'

export interface TouchGestureOptions {
  mode: InputMode
  /**
   * Converts a client position to remote desktop pixels, or null when the
   * canvas cannot answer yet. RemoteScreen.vue's own `toRemote` is the one
   * that gets passed in, so there is a single coordinate mapping in the app.
   */
  toRemote: (clientX: number, clientY: number) => { x: number; y: number } | null
  /**
   * Remote pixels per CSS pixel. Calibrated so that dragging a finger across
   * the whole displayed picture moves the pointer across the whole remote
   * desktop, which makes the feel independent of the server's `-scale`.
   */
  cssToRemoteX: number
  cssToRemoteY: number
}

export class TouchGestures {
  private mode: InputMode
  private readonly toRemote: TouchGestureOptions['toRemote']
  private cssToRemoteX: number
  private cssToRemoteY: number

  private phase: Phase = 'idle'
  /** The pointer driving the current gesture; others are ignored or scroll. */
  private primaryId: number | null = null
  /** Every pointer currently down, by id. Two or more means scrolling. */
  private readonly pointers = new Map<number, { x: number; y: number }>()
  /** Where the primary pointer last was, in client coordinates. */
  private lastPrimary = { x: 0, y: 0 }
  /** Where it first touched down, which is what a tap's drift is measured from. */
  private origin = { x: 0, y: 0 }
  /** When the current press began, for the long-press and drag-arm timers. */
  private pressStartedAt = 0
  /** When the last tap ended, so the next press can recognise itself as a drag. */
  private lastTapUpAt = 0
  /** Mean position of all fingers, for two-finger scrolling. */
  private lastCentroid: { x: number; y: number } | null = null

  /**
   * The remote pointer's position, tracked here because a relative move has
   * nowhere else to be relative *to*. The server is told absolute positions, so
   * nothing else in the system knows or needs to know where the pointer is.
   */
  private cursor = { x: 0, y: 0 }
  private cursorReady = false
  private screenW = 0
  private screenH = 0

  /** True while the remote thinks a button is down, so we can let go of it. */
  private leftHeld = false
  /** Last busy state reported, so the action is only emitted when it changes. */
  private busySent = false

  constructor(opts: TouchGestureOptions) {
    this.mode = opts.mode
    this.toRemote = opts.toRemote
    this.cssToRemoteX = opts.cssToRemoteX
    this.cssToRemoteY = opts.cssToRemoteY
  }

  /** Switches mode, abandoning any gesture in progress. Returns what to send. */
  setMode(mode: InputMode): TouchAction[] {
    const actions = this.reset()
    this.mode = mode
    return actions
  }

  getMode(): InputMode {
    return this.mode
  }

  /**
   * Recalibrates the relative-movement scale, which changes with the window
   * size because the picture is fitted to it.
   */
  setScale(cssToRemoteX: number, cssToRemoteY: number): void {
    this.cssToRemoteX = cssToRemoteX
    this.cssToRemoteY = cssToRemoteY
  }

  /**
   * Sets the remote desktop's size.
   *
   * Only relative mode needs this, to clamp the pointer and to know where the
   * middle is. The pointer starts centred rather than at a corner so the first
   * swipe has somewhere to go in every direction.
   */
  setScreenSize(w: number, h: number): void {
    this.screenW = w
    this.screenH = h
    if (!this.cursorReady && w > 0 && h > 0) {
      this.cursor = { x: Math.floor(w / 2), y: Math.floor(h / 2) }
      this.cursorReady = true
    }
    this.clampCursor()
  }

  /**
   * Releases everything and abandons the gesture.
   *
   * Called when the tab is hidden or the connection drops: a finger that
   * disappears mid-drag would otherwise leave the button held on the remote
   * machine with nothing left to release it.
   */
  reset(): TouchAction[] {
    const actions: TouchAction[] = []
    if (this.leftHeld) {
      actions.push({ kind: 'button', button: 'left', down: false })
      this.leftHeld = false
    }
    this.phase = 'idle'
    this.primaryId = null
    this.pointers.clear()
    this.lastCentroid = null
    this.lastTapUpAt = 0
    return this.finish(actions)
  }

  pointerDown(id: number, x: number, y: number, now: number): TouchAction[] {
    this.pointers.set(id, { x, y })

    // A second finger means scrolling, whatever the first one was doing. The
    // first finger's gesture is dropped rather than paused: resuming it after
    // the scroll would fire a click the user never asked for.
    if (this.pointers.size >= 2) {
      const actions: TouchAction[] = []
      if (this.leftHeld) {
        // Let go first, or the remote stays in drag mode for the rest of the
        // scroll and everything the fingers cross gets dragged along.
        actions.push({ kind: 'button', button: 'left', down: false })
        this.leftHeld = false
      }
      this.phase = 'scroll'
      this.primaryId = null
      this.lastCentroid = this.centroid()
      return this.finish(actions)
    }

    this.primaryId = id
    this.origin = { x, y }
    this.lastPrimary = { x, y }
    this.pressStartedAt = now

    // Pressing again right after a tap continues it, which is how a drag is
    // started. Which of the two it is only becomes clear on release or on
    // movement, and both of those are handled below.
    this.phase = now - this.lastTapUpAt <= DOUBLE_TAP_MS ? 'dragArm' : 'press'
    return this.finish([])
  }

  /**
   * Movement alone needs no clock: a finger that moves has already said what it
   * means, and a finger that holds still is precisely what `tick` is for.
   */
  pointerMove(id: number, x: number, y: number): TouchAction[] {
    if (!this.pointers.has(id)) return []
    this.pointers.set(id, { x, y })

    if (this.phase === 'scroll') return this.finish(this.scrollActions())
    if (id !== this.primaryId) return []

    const actions: TouchAction[] = []
    const drifted =
      Math.hypot(x - this.origin.x, y - this.origin.y) > MOVE_TOLERANCE_PX

    if (this.mode === 'mouse') {
      // Trackpad behaviour: the pointer follows the finger's movement, so this
      // runs for every phase including a plain press -- a small drift while
      // tapping is a real move, not noise to be discarded.
      actions.push(...this.relativeMove(x, y))
    }

    switch (this.phase) {
      case 'press':
        if (drifted) {
          // Too far to be a tap, and moving means it is not a long press
          // either. In touch mode nothing is sent: a lone drag over a
          // touchscreen has no meaning once two fingers own scrolling.
          this.phase = 'moved'
        }
        break

      case 'dragArm':
        // Starting to move is the gesture's real intent, and waiting out the
        // arm delay first would make every drag feel like it began late.
        if (drifted) {
          this.phase = 'dragHold'
          this.leftHeld = true
          // The drag must begin under the finger. The tap that preceded it
          // left the pointer wherever it was clicked, so without this the first
          // movement would yank the pointer across from there to here.
          if (this.mode === 'touch') actions.push(...this.absoluteMove(x, y))
          actions.push({ kind: 'button', button: 'left', down: true })
        }
        break

      case 'dragHold':
        // Touch mode follows the finger exactly here: a drag is precisely the
        // case where "where you touch is where it goes" is what you want.
        if (this.mode === 'touch') actions.push(...this.absoluteMove(x, y))
        break
    }

    this.lastPrimary = { x, y }
    return this.finish(actions)
  }

  pointerUp(id: number, x: number, y: number, now: number): TouchAction[] {
    if (!this.pointers.has(id)) return []
    this.pointers.delete(id)

    if (this.phase === 'scroll') {
      // Lifting one finger ends the scroll rather than handing the remaining
      // one a single-finger gesture it never began.
      if (this.pointers.size < 2) {
        this.phase = 'idle'
        this.primaryId = null
        this.lastCentroid = null
      }
      return this.finish([])
    }

    if (id !== this.primaryId) return this.finish([])

    const actions: TouchAction[] = []
    switch (this.phase) {
      case 'press':
        // A tap. In touch mode it also has to move the pointer there first,
        // since the click lands wherever the pointer is.
        if (this.mode === 'touch') actions.push(...this.absoluteMove(x, y))
        actions.push({ kind: 'click', button: 'left' })
        this.lastTapUpAt = now
        break

      case 'dragArm':
        // Released before it had time to become a drag, so it was the second
        // half of a double click after all.
        actions.push({ kind: 'click', button: 'left' })
        this.lastTapUpAt = now
        break

      case 'dragHold':
        actions.push({ kind: 'button', button: 'left', down: false })
        this.leftHeld = false
        // Deliberately not refreshing lastTapUpAt: chaining a third press into
        // another drag would be surprising right after finishing one.
        this.lastTapUpAt = 0
        break

      // 'moved' and 'done' already had their say; a tap was cancelled or a
      // right click was already delivered.
    }

    this.phase = 'idle'
    this.primaryId = null
    return this.finish(actions)
  }

  /**
   * Fires whatever timer has come due. The caller drives this while a gesture
   * is in progress; nothing here reads a clock of its own.
   */
  tick(now: number): TouchAction[] {
    const actions: TouchAction[] = []

    if (this.phase === 'press' && now - this.pressStartedAt >= LONG_PRESS_MS) {
      if (this.mode === 'touch') {
        actions.push(...this.absoluteMove(this.lastPrimary.x, this.lastPrimary.y))
      }
      actions.push({ kind: 'click', button: 'right' })
      // Marked done so lifting the finger afterwards does not also send a left
      // click -- one gesture, one event.
      this.phase = 'done'
      this.lastTapUpAt = 0
    } else if (this.phase === 'dragArm' && now - this.pressStartedAt >= DRAG_ARM_MS) {
      this.phase = 'dragHold'
      this.leftHeld = true
      actions.push({ kind: 'button', button: 'left', down: true })
    }

    return this.finish(actions)
  }

  /** Moves the pointer by the finger's movement, in the same direction. */
  private relativeMove(clientX: number, clientY: number): TouchAction[] {
    const dx = (clientX - this.lastPrimary.x) * this.cssToRemoteX
    const dy = (clientY - this.lastPrimary.y) * this.cssToRemoteY
    if (dx === 0 && dy === 0) return []

    this.cursor = { x: this.cursor.x + dx, y: this.cursor.y + dy }
    this.clampCursor()
    return [{ kind: 'moveTo', x: this.cursor.x, y: this.cursor.y }]
  }

  /** Moves the pointer to where the finger is. */
  private absoluteMove(clientX: number, clientY: number): TouchAction[] {
    const p = this.toRemote(clientX, clientY)
    if (!p) return []
    this.cursor = { x: p.x, y: p.y }
    this.cursorReady = true
    return [{ kind: 'moveTo', x: p.x, y: p.y }]
  }

  /**
   * Keeps the tracked pointer on the desktop.
   *
   * Without this the pointer would run off the edge during a relative drag and
   * accumulate a debt of invisible movement that had to be paid back before it
   * appeared to move again.
   */
  private clampCursor(): void {
    if (this.screenW <= 0 || this.screenH <= 0) return
    this.cursor = {
      x: Math.round(Math.min(this.screenW - 1, Math.max(0, this.cursor.x))),
      y: Math.round(Math.min(this.screenH - 1, Math.max(0, this.cursor.y))),
    }
  }

  private centroid(): { x: number; y: number } {
    let sx = 0
    let sy = 0
    for (const p of this.pointers.values()) {
      sx += p.x
      sy += p.y
    }
    const n = this.pointers.size || 1
    return { x: sx / n, y: sy / n }
  }

  /**
   * Turns the fingers' combined movement into a scroll.
   *
   * Negated because the two conventions are stated from opposite ends: dragging
   * two fingers *up* pushes the page down, which the wheel convention calls a
   * positive delta. This matches what the hand expects from scrolling any phone.
   */
  private scrollActions(): TouchAction[] {
    const c = this.centroid()
    const previous = this.lastCentroid
    this.lastCentroid = c
    if (!previous) return []

    const dx = c.x - previous.x
    const dy = c.y - previous.y
    if (dx === 0 && dy === 0) return []
    return [{ kind: 'wheel', dx: -dx, dy: -dy }]
  }

  /** Emits the busy flag only when it changes, so the canvas is not churned. */
  private finish(actions: TouchAction[]): TouchAction[] {
    const busy = this.leftHeld
    if (busy !== this.busySent) {
      this.busySent = busy
      actions.push({ kind: 'busy', value: busy })
    }
    return actions
  }
}

/**
 * Whether this device's primary input is a finger rather than a mouse.
 *
 * Two defaults hang off this: the touch/mouse input mode, and whether the soft
 * keyboard starts switched off.
 *
 * It used to be a bare `(pointer: coarse)`, and that is wrong on iPad. That
 * query describes the *primary* pointing device only, so an iPad with a
 * trackpad attached can report `fine` -- and then the keyboard defaults on,
 * exactly on the device where a tap-raised keyboard is most in the way. (The
 * reports are inconsistent in the other direction too; the W3C notes that
 * `pointer` never changes as devices are attached, which is the underlying
 * reason it cannot answer "can this thing be touched".)
 *
 * The user agent cannot stand in for it either: since iPadOS 13 an iPad
 * requests desktop sites by default and sends a Macintosh user agent,
 * deliberately identical to a real Mac's.
 *
 * What does hold is the touch-point count: a Mac reports 0 (macOS Safari
 * implements no touch events at all), an iPad 5 -- and attaching a trackpad
 * does not change it.
 */
export function prefersTouchMode(): boolean {
  const touchPoints = navigator.maxTouchPoints ?? 0
  if (touchPoints === 0) return false // no touchscreen: a real desktop

  const ua = navigator.userAgent
  // An iPad, in either of its two disguises: the mobile UA says so outright,
  // while the desktop-mode Macintosh UA only makes sense on a device that
  // still reports touch points.
  if (/iPad/.test(ua)) return true
  if (/Macintosh|Mac OS X/.test(ua)) return touchPoints > 1

  // Phones and Android tablets, where the pointer query is reliable. Keeping it
  // here is what leaves a touchscreen laptop with a mouse defaulting to mouse
  // mode (its primary pointer really is fine).
  return window.matchMedia?.('(pointer: coarse)').matches === true
}
