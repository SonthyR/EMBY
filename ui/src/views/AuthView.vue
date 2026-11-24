<script setup lang="ts">
import logo from '@/assets/logo.svg'
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { Eye, EyeOff } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import { ApiError, redeemApi } from '@/api'
import { checkSession, login, session, setup } from '@/stores/session'
import { toast } from '@/stores/toast'

const isSetup = computed(() => session.phase === 'setup')
const registerMode = ref(false)
const form = reactive({ username: '', password: '', confirm: '', token: '', code: '' })
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

function setMode(register: boolean) {
  registerMode.value = register
  error.value = ''
}

async function submit() {
  error.value = ''
  const username = form.username.trim()
  if (registerMode.value && !isSetup.value) {
    const code = form.code.trim()
    if (!code) return (error.value = '请填写开号兑换码')
    if (!username) return (error.value = '请填写用户名')
    if ([...form.password].length < 4 || utf8Length(form.password) > 72) return (error.value = '密码需要 4–72 个字符')
    busy.value = true
    try {
      await redeemApi.register(code, username, form.password)
      toast.success('账号已开通，正在进入自助页')
      await checkSession()
    } catch (err) {
      if (err instanceof ApiError && err.status === 429 && err.retryAfter) startCountdown(err.retryAfter)
      error.value = err instanceof Error ? err.message : '开号失败'
    } finally {
      busy.value = false
    }
    return
  }
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
        <span class="brand-mark"><img :src="logo" alt="" /></span>
        <div>
          <h1>云渡</h1>
          <p>Emby 分享与反代控制中心</p>
        </div>
      </div>

      <div v-if="session.phase === 'checking'" class="state">正在连接服务…</div>
      <div v-else-if="session.phase === 'error'" class="state">
        <p>无法连接到服务，请确认程序正在运行。</p>
        <UiButton variant="primary" @click="checkSession">重试</UiButton>
      </div>

      <form v-else class="form" novalidate @submit.prevent="submit">
        <div v-if="!isSetup" class="seg" role="tablist" aria-label="登录方式">
          <button type="button" role="tab" :aria-selected="!registerMode" :class="{ on: !registerMode }" @click="setMode(false)">账号登录</button>
          <button type="button" role="tab" :aria-selected="registerMode" :class="{ on: registerMode }" @click="setMode(true)">卡密自助开号</button>
        </div>
        <h2 v-else>创建管理员</h2>

        <p v-if="!isSetup && !registerMode" class="lead">管理员与朋友分享账号均可在此登录。</p>
        <p v-else-if="!isSetup && registerMode" class="lead">输入开号兑换码，自定义你的用户名与密码即可立即观看。</p>

        <UiField v-if="!isSetup && registerMode" label="开号兑换码" for="auth-code">
          <input id="auth-code" v-model="form.code" class="input mono" maxlength="64" autocapitalize="characters" spellcheck="false" placeholder="例如 YD-XXXX-XXXX-XXXX" autofocus />
        </UiField>
        <UiField label="用户名" for="auth-user" :help="isSetup ? '1–64 字节，只能有一个管理员。' : registerMode ? 'Emby 客户端和自助页通用，不含空格。' : ''">
          <input id="auth-user" v-model="form.username" class="input" autocomplete="username" autocapitalize="none" spellcheck="false" :autofocus="!registerMode" />
        </UiField>
        <UiField label="密码" for="auth-pass" :help="isSetup ? '至少 6 个字符。' : registerMode ? '至少 4 个字符。' : ''">
          <div class="secret">
            <input
              id="auth-pass"
              v-model="form.password"
              class="input"
              :type="showPassword ? 'text' : 'password'"
              :autocomplete="isSetup || registerMode ? 'new-password' : 'current-password'"
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
        <UiButton type="submit" variant="primary" block :loading="busy" :disabled="retryIn > 0">
          {{ isSetup ? '创建并进入' : registerMode ? '兑换并开通账号' : '立即登录' }}
        </UiButton>
        <p v-if="session.ephemeralSecret" class="note">当前未固定 JWT_SECRET，服务重启后需要重新登录。</p>
      </form>
    </div>
  </div>
</template>

<style scoped>
.auth { display: grid; place-items: center; min-height: 100vh; padding: 28px 16px; background: radial-gradient(620px 360px at 50% 0%, color-mix(in srgb, var(--accent) 11%, transparent), transparent 72%), var(--bg); }
.card { width: min(410px, 100%); padding: 32px 30px; border-radius: 24px; background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow); }
.brand { display: flex; align-items: center; gap: 13px; margin-bottom: 26px; }
.brand-mark { display: grid; place-items: center; width: 42px; height: 42px; border-radius: 13px; background: var(--hero-gradient); box-shadow: 0 6px 18px rgba(249, 87, 37, 0.25); }
.brand-mark img { width: 22px; height: 22px; filter: brightness(0) invert(1); }
.brand h1 { margin: 0; font-size: 20px; font-weight: 700; letter-spacing: -0.02em; }
.brand p { margin: 1px 0 0; color: var(--text-3); font-size: 12.5px; }
.seg { display: grid; grid-template-columns: 1fr 1fr; gap: 3px; padding: 3px; border-radius: 999px; background: var(--surface-2); border: 1px solid var(--border); }
.seg button { height: 33px; border: 0; border-radius: 999px; background: transparent; color: var(--text-2); font-size: 13px; font-weight: 500; cursor: pointer; transition: all var(--dur) var(--ease); }
.seg button.on { background: var(--surface); color: var(--text); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08); }
.form { display: flex; flex-direction: column; gap: 15px; }
.lead { margin: -2px 0 2px; color: var(--text-2); font-size: 13px; }
.form h2 { margin: 0 0 2px; font-size: 22px; font-weight: 700; letter-spacing: -0.025em; }
.secret { position: relative; }
.secret .input { padding-right: 42px; }
.eye { position: absolute; right: 4px; top: 50%; transform: translateY(-50%); display: grid; place-items: center; width: 34px; height: 34px; border: 0; background: transparent; color: var(--text-3); cursor: pointer; }
.eye svg { width: 17px; height: 17px; }
.error { margin: 0; padding: 10px 12px; border-radius: var(--radius-sm); background: var(--bad-soft); color: var(--bad); font-size: 13px; }
.note { margin: 0; color: var(--warn); font-size: 12px; text-align: center; }
.state { display: flex; flex-direction: column; gap: 12px; align-items: flex-start; color: var(--text-2); }
.state p { margin: 0; }
@media (max-width: 480px) { .card { padding: 24px 20px; } }
</style>
