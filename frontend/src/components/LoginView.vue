<script setup lang="ts">
/**
 * Password gate. Posts the口令 to /api/login and hands the session token back
 * to the app; the token is what the WebSocket upgrade carries.
 */
import { onMounted, ref } from 'vue'

const emit = defineEmits<{
  (e: 'authenticated', token: string, expiresAt: number): void
}>()

const password = ref('')
const busy = ref(false)
const error = ref('')
const inputEl = ref<HTMLInputElement | null>(null)

onMounted(() => inputEl.value?.focus())

async function submit() {
  if (busy.value) return

  error.value = ''
  if (!password.value) {
    error.value = '请输入口令'
    return
  }

  busy.value = true
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: password.value }),
      cache: 'no-store',
    })

    const body = (await res.json().catch(() => ({}))) as {
      token?: string
      expiresAt?: number
      error?: string
    }

    if (!res.ok || !body.token) {
      error.value = body.error || `登录失败 (HTTP ${res.status})`
      password.value = ''
      inputEl.value?.focus()
      return
    }

    emit('authenticated', body.token, body.expiresAt ?? 0)
  } catch (err) {
    error.value = `无法连接服务端: ${err instanceof Error ? err.message : String(err)}`
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login">
    <form class="card" @submit.prevent="submit">
      <h1>webvnc</h1>
      <p class="sub">输入访问口令以连接这台电脑的桌面</p>

      <label class="field">
        <span>访问口令</span>
        <input
          ref="inputEl"
          v-model="password"
          type="password"
          autocomplete="current-password"
          :disabled="busy"
          placeholder="启动日志中显示的口令"
        />
      </label>

      <p v-if="error" class="error">{{ error }}</p>

      <button type="submit" :disabled="busy">
        {{ busy ? '验证中…' : '连接' }}
      </button>

      <p class="hint">
        口令由启动参数 <code>-password</code> 指定；未指定时每次启动随机生成并打印在服务端日志里。
      </p>
    </form>
  </div>
</template>

<style scoped>
.login {
  display: grid;
  place-items: center;
  width: 100%;
  height: 100%;
  padding: 1.5rem;
}

.card {
  display: flex;
  flex-direction: column;
  gap: 0.85rem;
  width: 100%;
  max-width: 340px;
  padding: 1.75rem;
  border-radius: 14px;
  background: #171a21;
  border: 1px solid rgba(255, 255, 255, 0.08);
  box-shadow: 0 18px 48px rgba(0, 0, 0, 0.45);
}

h1 {
  margin: 0;
  font-size: 1.3rem;
  letter-spacing: 0.02em;
}

.sub {
  margin: -0.5rem 0 0.25rem;
  font-size: 0.83rem;
  color: #98a2b3;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  font-size: 0.8rem;
  color: #98a2b3;
}

.field input {
  padding: 0.6rem 0.7rem;
  border-radius: 8px;
  border: 1px solid rgba(255, 255, 255, 0.12);
  background: #0f1218;
  color: #e6e9ef;
  font-size: 0.95rem;
  outline: none;
}

.field input:focus {
  border-color: #4f8cff;
  box-shadow: 0 0 0 3px rgba(79, 140, 255, 0.18);
}

button {
  padding: 0.65rem;
  border: 0;
  border-radius: 8px;
  background: #3f7dfa;
  color: #fff;
  font-size: 0.95rem;
  font-weight: 600;
  cursor: pointer;
}

button:hover:not(:disabled) {
  background: #4f8cff;
}

button:disabled {
  opacity: 0.6;
  cursor: default;
}

.error {
  margin: 0;
  font-size: 0.82rem;
  color: #f97066;
}

.hint {
  margin: 0;
  font-size: 0.72rem;
  line-height: 1.5;
  color: #667085;
}

code {
  padding: 0 3px;
  border-radius: 3px;
  background: rgba(255, 255, 255, 0.07);
  font-size: 0.95em;
}
</style>
