<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { api } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const currentOrigin = window.location.origin
const loaded = ref(false)
const tokenConfigured = ref(false)
const secretStable = ref(true)
const status = ref({ state: 'disabled', message: '' })
const busy = ref('')
const errors = reactive<Record<string, string>>({})
const form = reactive({ token: '', botEnabled: false, adminIDs: '', publicURL: '', reportEnabled: false, chatID: '', frequency: 'daily', weekday: 1, time: '20:00' })

async function load() {
  try {
    const s = await api.get<any>('/api/telegram-report')
    const bot = s.bot || {}
    tokenConfigured.value = !!s.bot_token_configured
    secretStable.value = s.secret_stable !== false
    Object.assign(form, {
      token: '',
      botEnabled: !!bot.enabled,
      adminIDs: (bot.admin_ids || []).join(', '),
      publicURL: bot.public_url || location.origin,
      reportEnabled: !!s.enabled,
      chatID: s.chat_id || '',
      frequency: s.frequency || 'daily',
      weekday: Number.isInteger(s.weekday) ? s.weekday : 1,
      time: s.schedule_time || '20:00',
    })
    status.value = { state: bot.status || 'disabled', message: bot.enabled ? bot.status_message || '' : '' }
    loaded.value = true
  } catch (error) {
    toast.error(errorMessage(error, '读取 Telegram 设置失败'))
  }
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(async () => {
    try {
      const s = await api.get<any>('/api/telegram-report')
      status.value = { state: s.bot?.status || 'disabled', message: s.bot?.enabled ? s.bot?.status_message || '' : '' }
    } catch {
      /* ignore */
    }
  }, 10000)
})
onBeforeUnmount(() => clearInterval(timer))

const STATUS: Record<string, [string, string]> = { online: ['已连接', 'good'], error: ['连接异常', 'bad'], connecting: ['连接中', 'warn'], disabled: ['未启用', ''] }

function parseIDs(raw: string): number[] | null {
  const values = raw.trim().split(/[\s,，]+/).filter(Boolean)
  if (values.length > 10 || values.some(v => !/^[1-9]\d*$/.test(v) || !Number.isSafeInteger(Number(v)))) return null
  return [...new Set(values.map(Number))]
}

async function submit(test: boolean) {
  for (const key of Object.keys(errors)) delete errors[key]
  const token = form.token.trim()
  const chat = form.chatID.trim()
  if ((test || form.reportEnabled || form.botEnabled) && !token && !tokenConfigured.value) errors.token = '请先填写 Bot Token'
  if ((test || form.reportEnabled) && !chat) errors.chat = '请填写接收日报的 Chat ID'
  const ids = parseIDs(form.adminIDs)
  const url = form.publicURL.trim().replace(/\/+$/, '')
  if (!test) {
    if (ids === null || (form.botEnabled && !ids.length)) errors.ids = '填写数字用户 ID，最多 10 个'
    if (url || form.botEnabled) {
      let ok = false
      try {
        const u = new URL(url)
        ok = ['http:', 'https:'].includes(u.protocol) && !!u.hostname && !u.username && !u.search && !u.hash && u.pathname === '/'
      } catch {
        ok = false
      }
      if (!ok) errors.url = '填写面板的 HTTP/HTTPS 外部地址，不带路径'
    }
  }
  if (Object.keys(errors).length) return
  const payload: Record<string, unknown> = test
    ? { bot_token: token, chat_id: chat, action: 'test' }
    : { bot_token: token, chat_id: chat, enabled: form.reportEnabled, frequency: form.frequency, weekday: Number(form.weekday), schedule_time: form.time, bot: { enabled: form.botEnabled, admin_ids: ids || [], public_url: url } }
  busy.value = test ? 'test' : 'save'
  try {
    await api.post('/api/telegram-report', payload)
    toast.success(test ? '日报已发送' : 'Telegram 设置已保存')
    if (!test) load()
  } catch (error) {
    toast.error(errorMessage(error, test ? '发送失败' : '保存失败'))
  } finally {
    busy.value = ''
  }
}
const WEEKDAYS = [[1, '星期一'], [2, '星期二'], [3, '星期三'], [4, '星期四'], [5, '星期五'], [6, '星期六'], [0, '星期日']]
</script>

<template>
  <div v-if="loaded">
    <section class="group">
      <div class="head"><h2>机器人</h2><span class="state"><span class="dot" :class="STATUS[status.state]?.[1]" />{{ STATUS[status.state]?.[0] || status.state }}</span></div>
      <p v-if="!secretStable" class="warn p">请先配置固定的 JWT_SECRET，才能安全保存 Bot Token。</p>
      <UiField label="Bot Token" for="tg-token" :error="errors.token" :help="tokenConfigured ? 'Token 已加密保存，留空保留原来的' : '从 @BotFather 获取'">
        <input id="tg-token" v-model="form.token" class="input mono" type="password" autocomplete="new-password" :placeholder="tokenConfigured ? '已保存' : ''" />
      </UiField>
      <p v-if="status.message" class="faint p">{{ status.message }}</p>
    </section>
    <section class="group">
      <h2>管理员操作</h2>
      <UiSwitch v-model="form.botEnabled" label="通过 Telegram 管理" description="私聊机器人发送 /start，可以查看日报、增删改站点、管理源站账号。" />
      <UiField label="管理员 Telegram ID" for="tg-ids" :error="errors.ids" help="数字用户 ID，多个用逗号分隔；只有这些人能私聊操作。">
        <input id="tg-ids" v-model="form.adminIDs" class="input mono" placeholder="123456789" />
      </UiField>
      <UiField label="面板外部地址" for="tg-url" :error="errors.url" help="机器人用它生成站点地址，要包含实际端口。">
        <div class="with-btn">
          <input id="tg-url" v-model="form.publicURL" class="input mono" placeholder="https://panel.example.com" />
          <UiButton @click="form.publicURL = currentOrigin">用当前地址</UiButton>
        </div>
      </UiField>
    </section>
    <section class="group">
      <h2>定时日报</h2>
      <UiSwitch v-model="form.reportEnabled" label="发送日报" description="定时把流量、站点状态和到期提醒发到下面的聊天。" />
      <UiField label="接收 Chat ID" for="tg-chat" :error="errors.chat" help="个人、群组或频道，只用来接收日报，不授予管理权限。">
        <input id="tg-chat" v-model="form.chatID" class="input mono" />
      </UiField>
      <div class="grid">
        <UiField label="频率" for="tg-freq">
          <select id="tg-freq" v-model="form.frequency" class="select"><option value="daily">每天</option><option value="weekly">每周</option></select>
        </UiField>
        <UiField v-if="form.frequency === 'weekly'" label="星期" for="tg-wd">
          <select id="tg-wd" v-model.number="form.weekday" class="select"><option v-for="[v, l] in WEEKDAYS" :key="v" :value="v">{{ l }}</option></select>
        </UiField>
        <UiField label="时间" for="tg-time" help="按设置里的时区"><input id="tg-time" v-model="form.time" class="input" type="time" /></UiField>
      </div>
    </section>
    <div class="actions">
      <UiButton :loading="busy === 'test'" :disabled="!!busy" @click="submit(true)">立即发送一次</UiButton>
      <UiButton variant="primary" :loading="busy === 'save'" :disabled="!!busy" @click="submit(false)">保存</UiButton>
    </div>
  </div>
  <p v-else class="faint">正在读取…</p>
</template>

<style scoped>
.group { display: flex; flex-direction: column; gap: 18px; padding-bottom: 32px; margin-bottom: 32px; border-bottom: 1px solid var(--border); }
.head { display: flex; align-items: center; justify-content: space-between; }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.state { display: inline-flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); }
.p { margin: -8px 0 0; font-size: 13px; }
.warn { color: var(--warn); }
.grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 14px; }
.with-btn { display: flex; gap: 6px; }
.with-btn .input { flex: 1; }
.actions { display: flex; justify-content: flex-end; gap: 8px; }
@media (max-width: 640px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
