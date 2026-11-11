<script setup lang="ts">
import logo from '@/assets/logo.svg'
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { Eye, EyeOff } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import { ApiError } from '@/api'
import { checkSession, login, session, setup } from '@/stores/session'

const isSetup = computed(() => session.phase === 'setup')
const form = reactive({ username: '', password: '', confirm: '', token: '' })
const showPassword = ref(false)
const busy = ref(false)
const error = ref('')
const retryIn = ref(0)
let timer: ReturnType<typeof setInterval> | undefined

function utf8Length(value: string) {
  return new TextEncoder().encode(value).length
}

function startCountdown(seconds: number) {
  retryIn.value = seconds
  clearInterval(timer)
  timer = setInterval(() => {
    retryIn.value = Math.max(0, retryIn.value - 1)
    if (retryIn.value === 0) clearInterval(timer)
  }, 1000)
}
onBeforeUnmount(() => clearInterval(timer))

async function submit() {
  error.value = ''
  const username = form.username.trim()
  if (!username) return (error.value = '请填写用户名')
  if (isSetup.value) {
    if (utf8Length(username) > 64) return (error.value = '用户名不能超过 64 字节')
    if ([...form.password].length < 6 || utf8Length(form.password) > 72) return (error.value = '密码至少 6 个字符，且不超过 72 字节')
    if (form.password !== form.confirm) return (error.value = '两次输入的密码不一致')
    if (!form.token.trim()) return (error.value = '请填写初始化令牌')
  } else if (!form.password) {
    return (error.value = '请输入密码')
  }
  busy.value = true
  try {
    if (isSetup.value) await setup(username, form.password, form.token.trim())
    else await login(username, form.password)
  } catch (err) {
    if (err instanceof ApiError && err.status === 429 && err.retryAfter) startCountdown(err.retryAfter)
    error.value = err instanceof Error ? err.message : '登录失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="auth">
    <div class="card">
      <div class="brand">
        <img :src="logo" alt="" />
        <div>
          <h1>云渡</h1>
          <p>Emby 分享与反代面板</p>
        </div>
      </div>

      <div v-if="session.phase === 'checking'" class="state">正在连接服务…</div>
      <div v-else-if="session.phase === 'error'" class="state">
        <p>无法连接到服务，请确认程序正在运行。</p>
        <UiButton variant="primary" @click="checkSession">重试</UiButton>
      </div>

      <form v-else class="form" novalidate @submit.prevent="submit">
        <h2>{{ isSetup ? '创建管理员' : '登录' }}</h2>
        <UiField label="用户名" for="auth-user" :help="isSetup ? '1–64 字节，只能有一个管理员。' : ''">
          <input id="auth-user" v-model="form.username" class="input" autocomplete="username" autocapitalize="none" spellcheck="false" autofocus />
        </UiField>
        <UiField label="密码" for="auth-pass" :help="isSetup ? '至少 6 个字符。' : ''">
          <div class="secret">
            <input
              id="auth-pass"
              v-model="form.password"
              class="input"
              :type="showPassword ? 'text' : 'password'"
              :autocomplete="isSetup ? 'new-password' : 'current-password'"
            />
            <button type="button" class="eye" :aria-label="showPassword ? '隐藏密码' : '显示密码'" @click="showPassword = !showPassword">
              <EyeOff v-if="showPassword" /><Eye v-else />
            </button>
          </div>
        </UiField>
        <template v-if="isSetup">
          <UiField label="确认密码" for="auth-confirm">
            <input id="auth-confirm" v-model="form.confirm" class="input" :type="showPassword ? 'text' : 'password'" autocomplete="new-password" />
          </UiField>
          <UiField label="初始化令牌" for="auth-token" help="安装时输出的 SETUP_TOKEN。">
            <input id="auth-token" v-model="form.token" class="input mono" type="password" autocomplete="off" spellcheck="false" />
          </UiField>
        </template>
        <p v-if="error" class="error" role="alert">{{ error }}<template v-if="retryIn">（{{ retryIn }} 秒后可重试）</template></p>
        <UiButton type="submit" variant="primary" block :loading="busy" :disabled="retryIn > 0">{{ isSetup ? '创建并进入' : '登录' }}</UiButton>
        <p v-if="session.ephemeralSecret" class="note">当前未固定 JWT_SECRET，服务重启后需要重新登录。</p>
      </form>
    </div>
  </div>
</template>

<style scoped>
.auth {
  display: grid;
  place-items: center;
  min-height: 100vh;
  padding: 24px 16px;
  background: var(--bg);
}
.card { width: min(360px, 100%); }
.brand { display: flex; align-items: center; gap: 12px; margin-bottom: 36px; }
.brand img { width: 30px; height: 30px; }
.brand h1 { margin: 0; font-size: 18px; font-weight: 600; letter-spacing: 0.04em; }
.brand p { margin: 0; color: var(--text-3); font-size: 12.5px; }
.form { display: flex; flex-direction: column; gap: 16px; }
.form h2 { margin: 0 0 6px; font-size: 24px; font-weight: 600; letter-spacing: -0.025em; }
.secret { position: relative; }
.secret .input { padding-right: 42px; }
.eye {
  position: absolute;
  right: 4px;
  top: 50%;
  transform: translateY(-50%);
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  border: 0;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
}
.eye svg { width: 18px; height: 18px; }
.error { margin: 0; padding: 8px 12px; border-radius: var(--radius-sm); background: var(--bad-soft); color: var(--bad); font-size: 13px; }
.note { margin: 0; color: var(--warn); font-size: 12px; }
.state { display: flex; flex-direction: column; gap: 12px; align-items: flex-start; color: var(--text-2); }
.state p { margin: 0; }
</style>
