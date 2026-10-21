<script setup lang="ts">
/**
 * Application shell: decides between the login gate and the remote desktop
 * surface, and renders the toolbar around it.
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'
import LoginView from './components/LoginView.vue'
import RemoteScreen from './components/RemoteScreen.vue'
import type { InputMode } from './touch'

/** Key under which the session token is kept. sessionStorage, not
 *  localStorage: the token should not outlive the browser tab. */
const TOKEN_KEY = 'webvnc.token'

const token = ref('')
const isFullscreen = ref(false)
/** Mirrors the remote screen's audio state, which it reports as it changes. */
const audioOn = ref(false)
/** Mirrors the remote screen's touch mode the same way. */
const inputMode = ref<InputMode>('touch')
/**
 * Mirrors the remote screen's soft-keyboard switch. The real value comes from
 * storage or a device guess, neither of which this component can see, so the
 * remote screen reports it on mount. Only rendered when `hasTouch`, which is
 * also the only case the default differs from `true`.
 */
const keyboardOn = ref(true)
/**
 * Whether the server is in live mode, mirrored from the remote screen, which is
 * told by the server. Only ever a UI hint: hiding these buttons keeps the
 * toolbar from offering things that cannot work, but the server discards any
 * input frame that arrives anyway, so a client that ignores this loses nothing
 * but the polite presentation.
 */
const viewOnly = ref(false)
const screenRef = ref<InstanceType<typeof RemoteScreen> | null>(null)
const notice = ref('')

/**
 * Whether this device has a touchscreen at all.
 *
 * The mode control is hidden without one: it changes nothing for a mouse, and
 * the toolbar already has seven other buttons competing for the space.
 * maxTouchPoints rather than a coarse-pointer query, because a touchscreen
 * laptop reports a fine pointer and still needs the control.
 */
const hasTouch = ref((navigator.maxTouchPoints ?? 0) > 0)

onMounted(() => {
  document.addEventListener('fullscreenchange', onFullscreenChange)

  // `?token=` in the URL is the "quick access" path from the README. Consume it
  // and then strip it from the address bar so it does not linger in history,
  // bookmarks, or anything the user copies.
  const url = new URL(window.location.href)
  const fromURL = url.searchParams.get('token')
  if (fromURL) {
    token.value = fromURL
    url.searchParams.delete('token')
    window.history.replaceState({}, '', url.pathname + url.search + url.hash)
    return
  }

  const stored = sessionStorage.getItem(TOKEN_KEY)
  if (stored) token.value = stored
})

onBeforeUnmount(() => document.removeEventListener('fullscreenchange', onFullscreenChange))

function onAuthenticated(value: string) {
  token.value = value
  sessionStorage.setItem(TOKEN_KEY, value)
}

function onUnauthorized() {
  sessionStorage.removeItem(TOKEN_KEY)
  token.value = ''
  notice.value = '口令已失效，请重新登录。'
  // The remote screen is about to be torn down with it; reset the label now so
  // it cannot come back stale on the next login.
  audioOn.value = false
}

function signOut() {
  sessionStorage.removeItem(TOKEN_KEY)
  token.value = ''
  notice.value = ''
  audioOn.value = false
}

function onFullscreenChange() {
  isFullscreen.value = document.fullscreenElement !== null
}

function toggleFullscreen() {
  screenRef.value?.toggleFullscreen()
}

function pasteFromClipboard() {
  screenRef.value?.pasteFromClipboard()
}

function pullFromRemote() {
  screenRef.value?.pullRemoteClipboard()
}

function toggleAudio() {
  screenRef.value?.toggleAudio()
}

function setInputMode(mode: InputMode) {
  screenRef.value?.setInputMode(mode)
}

function setKeyboard(on: boolean) {
  screenRef.value?.setKeyboard(on)
}
</script>

<template>
  <div class="app">
    <header v-if="token" class="toolbar">
      <span class="brand">webvnc</span>
      <span class="tag">远程桌面</span>

      <div class="spacer" />

      <!--
        Live mode stands where the input controls would be, so the toolbar reads
        as "this is why they are missing" rather than as a toolbar that lost
        half its buttons. A plain span, not a disabled button: there is nothing
        to press, and a disabled control would invite the viewer to look for the
        thing that re-enables it.
      -->
      <span
        v-if="viewOnly"
        class="live-tag"
        title="服务器开启了直播模式，所有客户端只能观看和收听，不能操作远程电脑。这个开关在服务端，客户端无法解除。"
      >
        直播模式 · 只能观看
      </span>

      <!--
        Two states, not an on/off switch. A single button that swaps its label
        (as the audio one does) would leave the user guessing which mode is
        actually in force.
      -->
      <div v-if="hasTouch && !viewOnly" class="modes" role="group" aria-label="触摸操作模式">
        <button
          type="button"
          :class="{ active: inputMode === 'mouse' }"
          title="鼠标模式：手指滑动让指针朝同一方向移动，手指位置不等于指针位置（像触控板）"
          @mousedown.prevent
          @click="setInputMode('mouse')"
        >
          鼠标
        </button>
        <button
          type="button"
          :class="{ active: inputMode === 'touch' }"
          title="触屏模式：手指点在哪里，指针就到哪里（像用手机）；拖动请点一下再按住"
          @mousedown.prevent
          @click="setInputMode('touch')"
        >
          触屏
        </button>
      </div>

      <!--
        The soft keyboard, which on a touch device starts off: focusing the
        sink on a tap is what raises it, so a plain tap on the remote screen
        must not do that. Same visibility rule as the mode pair above -- a
        device without a touchscreen has no soft keyboard to switch.
      -->
      <button
        v-if="hasTouch && !viewOnly"
        type="button"
        :class="{ active: keyboardOn }"
        :aria-pressed="keyboardOn"
        :title="
          keyboardOn
            ? '收起软键盘，之后点画面只操作远程'
            : '弹出软键盘，用本机输入法输入中文'
        "
        @mousedown.prevent
        @click="setKeyboard(!keyboardOn)"
      >
        {{ keyboardOn ? '收起键盘' : '键盘' }}
      </button>

      <!--
        mousedown.prevent keeps the click from moving focus off the keyboard
        sink. Without it, clicking any toolbar button would silently break IME
        input (composition needs a focused editable) until the canvas is
        clicked again.
      -->
      <button
        type="button"
        :title="
          audioOn
            ? '停止接收远程声音'
            : '接收远程声音（浏览器要求先点一下才允许播放）'
        "
        @mousedown.prevent
        @click="toggleAudio"
      >
        {{ audioOn ? '静音' : '开启声音' }}
      </button>
      <!-- Both clipboard controls are hidden in live mode: a push writes to the
           remote clipboard, and a pull takes its contents off the machine,
           which is more than watching. `Ctrl+V`/`Ctrl+C` are guarded in the
           remote screen for the same reason. -->
      <button
        v-if="!viewOnly"
        type="button"
        title="把本机剪贴板文本发送到远程并粘贴（也可直接按 Ctrl+V；非 https/localhost 打开时只能用 Ctrl+V）"
        @mousedown.prevent
        @click="pasteFromClipboard"
      >
        粘贴文本
      </button>
      <button
        v-if="!viewOnly"
        type="button"
        title="把远程剪贴板的文本取到本机（也可直接按 Ctrl+C；远程用右键菜单复制时用这个按钮）"
        @mousedown.prevent
        @click="pullFromRemote"
      >
        获取远程文本
      </button>
      <button
        type="button"
        :title="isFullscreen ? '退出全屏' : '进入全屏 (F11)'"
        @mousedown.prevent
        @click="toggleFullscreen"
      >
        {{ isFullscreen ? '退出全屏' : '全屏' }}
      </button>
      <button type="button" title="清除本机保存的口令" @mousedown.prevent @click="signOut">退出</button>
    </header>

    <main class="stage">
      <p v-if="notice && !token" class="notice">{{ notice }}</p>

      <LoginView v-if="!token" @authenticated="onAuthenticated" />
      <RemoteScreen
        v-else
        ref="screenRef"
        :token="token"
        @unauthorized="onUnauthorized"
        @audio="audioOn = $event"
        @input-mode="inputMode = $event"
        @keyboard="keyboardOn = $event"
        @view-only="viewOnly = $event"
      />
    </main>
  </div>
</template>

<style scoped>
.app {
  display: flex;
  flex-direction: column;
  width: 100%;
  height: 100%;
  min-height: 0;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  flex: 0 0 auto;
  /* Right now there are eight controls here, which cannot fit across a phone.
     Wrapping is the difference between a slightly taller bar and buttons that
     are simply unreachable. */
  flex-wrap: wrap;
  row-gap: 0.35rem;
  padding: 0.45rem 0.75rem;
  background: #14171d;
  border-bottom: 1px solid rgba(255, 255, 255, 0.07);
}

.brand {
  font-weight: 700;
  letter-spacing: 0.02em;
}

.tag {
  font-size: 0.75rem;
  color: #98a2b3;
}

.spacer {
  flex: 1 1 auto;
}

/* Same pill as the HUD badge, so the two read as one fact stated twice: once
   over the picture and once where the controls would have been. */
.live-tag {
  padding: 0.32rem 0.7rem;
  border-radius: 7px;
  font-size: 0.78rem;
  font-weight: 600;
  color: #10202f;
  background: #6cc0f5;
}

.toolbar button {
  padding: 0.32rem 0.7rem;
  border-radius: 7px;
  border: 1px solid rgba(255, 255, 255, 0.1);
  background: #1d222b;
  color: #d7dce5;
  font-size: 0.78rem;
  cursor: pointer;
}

.toolbar button:hover {
  background: #262c37;
  border-color: rgba(255, 255, 255, 0.18);
}

/* Segmented control: the two halves share one outline, so the pair reads as a
   single choice rather than two unrelated buttons. */
.modes {
  display: flex;
  border: 1px solid rgba(255, 255, 255, 0.1);
  border-radius: 7px;
  overflow: hidden;
}

.modes button {
  border: 0;
  border-radius: 0;
}

.modes button + button {
  border-left: 1px solid rgba(255, 255, 255, 0.1);
}

.toolbar button.active {
  background: #2f6feb;
  border-color: #2f6feb;
  color: #fff;
}

.toolbar button.active:hover {
  background: #3b7bf5;
  border-color: #3b7bf5;
}

/* The brand is decoration; the controls are not. Drop it before the buttons
   have to wrap a second time. */
@media (max-width: 560px) {
  .brand,
  .tag {
    display: none;
  }
}

/* A fingertip is far less precise than a cursor, and the existing padding was
   sized for a mouse. This applies to the older buttons too, which need it just
   as much. */
@media (pointer: coarse) {
  .toolbar button {
    min-height: 40px;
    padding: 0.5rem 0.8rem;
  }
}

.stage {
  position: relative;
  flex: 1 1 auto;
  min-height: 0;
}

.notice {
  position: absolute;
  top: 12px;
  left: 50%;
  z-index: 2;
  transform: translateX(-50%);
  margin: 0;
  padding: 0.4rem 0.9rem;
  border-radius: 999px;
  font-size: 0.8rem;
  color: #ffd8a8;
  background: rgba(90, 55, 15, 0.85);
  border: 1px solid rgba(240, 169, 74, 0.4);
}
</style>
