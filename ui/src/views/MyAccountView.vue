<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Copy, LogOut, MonitorSmartphone, Moon, Sun, SunMoon } from '@lucide/vue'
import logo from '@/assets/logo.svg'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { meApi, type MyAccount } from '@/api'
import { logout, session } from '@/stores/session'
import { confirm } from '@/stores/confirm'
import { cycleTheme, themeMode } from '@/stores/theme'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, daysUntil, dateOnly, formatBytes, relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from '@/components/share/shareStatus'

const me = ref<MyAccount | null>(null)
const loadError = ref('')
const pw = reactive({ open: false, current: '', next: '', busy: false, error: '' })

async function load() {
  try {
    me.value = await meApi.get()
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取失败')
  }
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => document.visibilityState === 'visible' && load(), 30000)
})
onBeforeUnmount(() => clearInterval(timer))

const headline = computed(() => {
  const u = me.value
  if (!u) return ''
  if (u.status === 'active') return '账号正常，可以随时观看'
  if (u.status === 'expired') return '账号已过期，请联系分享人续期'
  if (u.status === 'quota') return '本期流量已用完'
  return '账号已被停用'
})
const expiry = computed(() => {
  const u = me.value
  if (!u?.expires_at) return { main: '永久', sub: '' }
  const days = daysUntil(u.expires_at)
  return { main: dateOnly(u.expires_at), sub: days === null ? '' : days < 0 ? '已过期' : days === 0 ? '今天到期' : `还剩 ${days} 天` }
})

async function copy(value: string) {
  if (await copyText(value)) toast.success('已复制')
}
async function kick() {
  if (!(await confirm({ title: '断开所有设备？', message: '所有设备上的 Emby 都会退出登录，需要重新输入账号密码。', confirmText: '断开' }))) return
  try {
    await meApi.kick()
    toast.success('已断开')
    load()
  } catch (error) {
    toast.error(errorMessage(error))
  }
}
async function changePassword() {
  pw.error = ''
  if (!pw.current) return (pw.error = '请输入当前密码')
  if ([...pw.next].length < 4) return (pw.error = '新密码至少 4 个字符')
  pw.busy = true
  try {
    await meApi.password(pw.current, pw.next)
    toast.success('密码已修改，请在 Emby 客户端用新密码重新登录')
    Object.assign(pw, { open: false, current: '', next: '' })
    load()
  } catch (error) {
    pw.error = errorMessage(error)
  } finally {
    pw.busy = false
  }
}
</script>

<template>
  <div class="me">
    <header class="bar">
      <div class="brand"><img :src="logo" alt="" />云渡</div>
      <div class="right">
        <span class="who">{{ session.username }}</span>
        <button type="button" class="icon" aria-label="切换主题" @click="cycleTheme">
          <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
        </button>
        <button type="button" class="icon" aria-label="退出登录" title="退出登录" @click="logout"><LogOut /></button>
      </div>
    </header>

    <main class="content">
      <p v-if="loadError && !me" class="faint">{{ loadError }}</p>
      <template v-if="me">
        <section class="hero">
          <span class="state"><span class="dot" :class="STATUS_TONE[me.status]" />{{ STATUS_LABEL[me.status] }}</span>
          <h1>{{ headline }}</h1>
        </section>

        <section class="usage">
          <div class="label">本期已用</div>
          <div class="big num">{{ formatBytes(me.traffic_used) }}<span class="of">{{ me.traffic_quota ? ` / ${formatBytes(me.traffic_quota)}` : '' }}</span></div>
          <UiProgress v-if="me.traffic_quota" :value="me.traffic_used" :max="me.traffic_quota" />
          <p class="faint">{{ me.traffic_quota ? (me.next_reset_at ? `${me.next_reset_at} 自动清零` : '流量额度不会自动清零') : '流量不限' }}</p>
        </section>

        <dl class="facts">
          <div><dt>到期</dt><dd>{{ expiry.main }}<small v-if="expiry.sub">{{ expiry.sub }}</small></dd></div>
          <div><dt>在线设备</dt><dd class="num">{{ me.session_count }}<small>{{ me.max_devices ? `最多 ${me.max_devices} 台` : '不限台数' }}</small></dd></div>
          <div><dt>速度</dt><dd>{{ me.speed_limit ? `${me.speed_limit} Mbps` : '不限速' }}</dd></div>
        </dl>

        <section class="block">
          <h2>服务器地址</h2>
          <p class="faint hint">在 Emby 客户端里添加下面的地址，用你的账号和密码登录。</p>
          <ul class="servers">
            <li v-for="server in me.servers" :key="server.name">
              <span class="name">{{ server.name }}</span>
              <div v-for="addr in server.addresses" :key="addr" class="addr">
                <span class="mono">{{ addr }}</span>
                <UiButton variant="ghost" size="sm" icon aria-label="复制地址" @click="copy(addr)"><Copy /></UiButton>
              </div>
            </li>
            <li v-if="!me.servers?.length" class="faint">暂无可用服务器</li>
          </ul>
        </section>

        <section class="block">
          <div class="block-head">
            <h2>已登录设备</h2>
            <UiButton v-if="me.sessions?.length" variant="ghost" size="sm" @click="kick">全部断开</UiButton>
          </div>
          <ul class="devices">
            <li v-for="(s, i) in me.sessions || []" :key="i">
              <MonitorSmartphone />
              <div>
                <span>{{ s.device_name || '未知设备' }}</span>
                <small class="faint">{{ [s.client, s.site_name].filter(Boolean).join(' · ') }}</small>
              </div>
              <small class="faint">{{ relativeTime(s.last_seen_at) }}</small>
            </li>
            <li v-if="!me.sessions?.length" class="faint none">没有设备登录</li>
          </ul>
        </section>

        <section class="block">
          <div class="block-head">
            <h2>密码</h2>
            <UiButton v-if="!pw.open" variant="ghost" size="sm" @click="pw.open = true">修改</UiButton>
          </div>
          <form v-if="pw.open" class="pw" @submit.prevent="changePassword">
            <UiField label="当前密码" for="me-cur"><input id="me-cur" v-model="pw.current" class="input" type="password" autocomplete="current-password" /></UiField>
            <UiField label="新密码" for="me-new" help="修改后所有设备需要用新密码重新登录。"><input id="me-new" v-model="pw.next" class="input" type="password" autocomplete="new-password" /></UiField>
            <p v-if="pw.error" class="err">{{ pw.error }}</p>
            <div class="pw-actions">
              <UiButton @click="pw.open = false">取消</UiButton>
              <UiButton variant="primary" type="submit" :loading="pw.busy">保存</UiButton>
            </div>
          </form>
          <p v-else class="faint none">Emby 客户端和这个页面用的是同一个密码。</p>
        </section>
      </template>
    </main>
  </div>
</template>

<style scoped>
.me { min-height: 100vh; }
.bar { position: sticky; top: 0; z-index: 10; display: flex; align-items: center; justify-content: space-between; height: 56px; padding: env(safe-area-inset-top) 20px 0; background: color-mix(in srgb, var(--bg) 88%, transparent); backdrop-filter: blur(14px); border-bottom: 1px solid var(--border); }
.brand { display: flex; align-items: center; gap: 9px; font-weight: 600; letter-spacing: 0.03em; }
.brand img { width: 22px; height: 22px; }
.right { display: flex; align-items: center; gap: 2px; }
.who { margin-right: 8px; font-size: 13px; color: var(--text-2); }
.icon { display: grid; place-items: center; width: 32px; height: 32px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; }
.icon:hover { background: var(--surface-hover); color: var(--text); }
.icon svg { width: 16px; height: 16px; }
.content { max-width: 620px; margin: 0 auto; padding: 48px 20px 80px; }
.hero { margin-bottom: 36px; }
.state { display: inline-flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); }
h1 { margin: 10px 0 0; font-size: 26px; font-weight: 600; letter-spacing: -0.03em; line-height: 1.25; }
.usage { padding-bottom: 28px; border-bottom: 1px solid var(--border); }
.label { font-size: 12.5px; color: var(--text-3); margin-bottom: 4px; }
.big { font-size: 40px; font-weight: 600; letter-spacing: -0.035em; line-height: 1.1; margin-bottom: 14px; }
.of { font-size: 18px; font-weight: 500; color: var(--text-3); letter-spacing: -0.01em; }
.usage p { margin: 10px 0 0; font-size: 13px; }
.facts { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; margin: 0; padding: 24px 0; border-bottom: 1px solid var(--border); }
.facts dt { font-size: 12.5px; color: var(--text-3); }
.facts dd { margin: 4px 0 0; font-size: 16px; font-weight: 500; display: flex; flex-direction: column; }
.facts small { font-size: 12.5px; font-weight: 400; color: var(--text-3); }
.block { padding: 26px 0 0; }
.block-head { display: flex; align-items: center; justify-content: space-between; }
h2 { margin: 0; font-size: 14px; font-weight: 600; }
.hint { margin: 4px 0 12px; font-size: 13px; }
.servers, .devices { margin: 8px 0 0; padding: 0; list-style: none; }
.servers li { padding: 10px 0; border-bottom: 1px solid var(--border); }
.servers li:last-child { border-bottom: 0; }
.servers .name { font-size: 13px; color: var(--text-2); }
.addr { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-top: 2px; }
.addr .mono { overflow-wrap: anywhere; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 14px; }
.devices li:last-child { border-bottom: 0; }
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.devices small { font-size: 12px; }
.none { font-size: 13px; padding: 6px 0; }
.pw { display: flex; flex-direction: column; gap: 14px; margin-top: 14px; }
.pw-actions { display: flex; justify-content: flex-end; gap: 8px; }
.err { margin: 0; color: var(--bad); font-size: 13px; }
@media (max-width: 560px) {
  .content { padding-top: 32px; }
  .facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  h1 { font-size: 22px; }
  .big { font-size: 34px; }
}
</style>
