<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import TlsSettings from '@/components/settings/TlsSettings.vue'
import TelegramSettings from '@/components/settings/TelegramSettings.vue'
import { api, sitesApi } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'

type Settings = Record<string, any>
const SECTIONS = [
  { key: 'general', label: '流量与时间' },
  { key: 'logs', label: '请求日志' },
  { key: 'backup', label: '备份与恢复' },
  { key: 'tls', label: '面板地址与证书' },
  { key: 'telegram', label: 'Telegram 机器人' },
]
const section = ref('general')
const settings = ref<Settings | null>(null)
const original = ref('')
const saving = ref(false)
const version = ref('')

const TIMEZONES = ['Asia/Shanghai', 'Asia/Hong_Kong', 'Asia/Taipei', 'Asia/Tokyo', 'Asia/Singapore', 'Europe/London', 'Europe/Berlin', 'America/New_York', 'America/Los_Angeles', 'UTC']
const LOG_TYPES = [
  ['log_write_playback', '播放请求'],
  ['log_write_video', '视频流'],
  ['log_write_api', 'API'],
  ['log_write_auth', '登录'],
  ['log_write_subtitle', '字幕'],
  ['log_write_metadata', '媒体信息'],
  ['log_write_image', '图片'],
  ['log_write_asset', '静态资源'],
  ['log_write_websocket', 'WebSocket'],
]
const LOG_FIELDS = [
  ['log_write_client_ip', '客户端 IP'],
  ['log_write_ua', '客户端 UA'],
  ['log_write_upstream_ua', '回源 UA'],
  ['log_write_backend_address', '回源节点'],
  ['log_write_colo', 'CDN 节点'],
]

onMounted(async () => {
  try {
    const [s, caps] = await Promise.all([api.get<Settings>('/api/system-settings'), sitesApi.capabilities().catch(() => ({}) as any)])
    settings.value = s
    original.value = JSON.stringify(s)
    version.value = caps.app_version || ''
  } catch (error) {
    toast.error(errorMessage(error, '读取设置失败'))
  }
})
const dirty = computed(() => !!settings.value && JSON.stringify(settings.value) !== original.value)

async function save() {
  if (!settings.value) return
  saving.value = true
  try {
    const saved = await api.post<Settings>('/api/system-settings', settings.value)
    settings.value = saved
    original.value = JSON.stringify(saved)
    toast.success('设置已保存')
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}
function reset() {
  settings.value = JSON.parse(original.value)
}

// backup
const backup = ref({ password: '', confirm: '', includeTLS: false, busy: false })
async function exportBackup() {
  const b = backup.value
  const size = new TextEncoder().encode(b.password).length
  if (size < 6 || size > 128) return toast.error('备份密码需要 6–128 个字节')
  if (b.password !== b.confirm) return toast.error('两次输入的备份密码不一致')
  b.busy = true
  try {
    const response = await fetch('/api/backup/export', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: b.password, include_tls: b.includeTLS }) })
    if (!response.ok) {
      let message = '创建备份失败'
      try {
        message = (await response.json()).error || message
      } catch {
        /* keep */
      }
      throw new Error(message)
    }
    const disposition = response.headers.get('Content-Disposition') || ''
    const name = disposition.match(/filename="?([^";]+)"?/)?.[1] || `yundu-backup-${new Date().toISOString().slice(0, 10)}.zip`
    const link = document.createElement('a')
    link.href = URL.createObjectURL(await response.blob())
    link.download = name
    link.click()
    setTimeout(() => URL.revokeObjectURL(link.href), 1000)
    Object.assign(b, { password: '', confirm: '' })
    toast.success('备份已下载')
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    b.busy = false
  }
}
const restore = ref({ file: null as File | null, password: '', busy: false })
async function doRestore() {
  const r = restore.value
  if (!r.file) return toast.error('请选择备份文件')
  if (!r.password) return toast.error('请输入备份密码')
  if (!(await confirm({ title: '用备份覆盖当前数据？', message: '当前所有站点、账号和设置都会被备份里的内容替换，服务会自动重启，之后需要用备份里的管理员账号重新登录。', confirmText: '恢复', danger: true }))) return
  r.busy = true
  try {
    const body = new FormData()
    body.append('backup', r.file)
    body.append('password', r.password)
    body.append('confirm', '恢复')
    const response = await fetch('/api/backup/restore', { method: 'POST', credentials: 'same-origin', body })
    const data = await response.json().catch(() => ({}))
    if (!response.ok) throw new Error(data.error || '恢复失败')
    toast.success(data.message || '恢复成功，服务正在重启')
    setTimeout(() => location.reload(), 4000)
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    r.busy = false
  }
}
</script>

<template>
  <div>
    <PageHeader title="设置" :description="version ? `云渡 ${version}` : ''" />
    <div class="layout">
      <nav class="side">
        <button v-for="s in SECTIONS" :key="s.key" type="button" :class="{ on: section === s.key }" @click="section = s.key">{{ s.label }}</button>
      </nav>

      <div class="body">
        <template v-if="settings && section === 'general'">
          <section class="group">
            <h2>流量统计</h2>
            <UiField label="计费方式" for="st-billing" help="站点和分享账号的流量额度都按这个方式统计。">
              <select id="st-billing" v-model="settings.traffic_billing_mode" class="select">
                <option value="bidirectional">上行 + 下行</option>
                <option value="outbound">只算下行（发给客户端的流量）</option>
              </select>
            </UiField>
            <UiField label="站点流量每月重置日" for="st-reset">
              <select id="st-reset" v-model.number="settings.traffic_reset_day" class="select">
                <option v-for="d in 28" :key="d" :value="d">每月 {{ d }} 日</option>
              </select>
            </UiField>
          </section>
          <section class="group">
            <h2>时间</h2>
            <UiField label="时区" for="st-tz" help="用于流量重置、日志时间和定时通知。">
              <select id="st-tz" v-model="settings.schedule_timezone" class="select">
                <option v-for="tz in TIMEZONES" :key="tz" :value="tz">{{ tz }}</option>
                <option v-if="!TIMEZONES.includes(settings.schedule_timezone)" :value="settings.schedule_timezone">{{ settings.schedule_timezone }}</option>
              </select>
            </UiField>
          </section>
          <section class="group">
            <h2>回源检测</h2>
            <div class="grid">
              <UiField label="测速超时" for="st-probe" help="毫秒"><input id="st-probe" v-model.number="settings.probe_timeout_ms" class="input num" type="number" min="500" step="100" /></UiField>
              <UiField label="Ping 缓存" for="st-ping" help="分钟"><input id="st-ping" v-model.number="settings.ping_cache_minutes" class="input num" type="number" min="0" /></UiField>
            </div>
          </section>
        </template>

        <template v-if="settings && section === 'logs'">
          <section class="group">
            <h2>记录</h2>
            <UiSwitch v-model="settings.log_enabled" label="记录请求日志" description="关闭后不再写入新日志，已有日志保留。" />
            <UiField label="记录范围" for="st-level">
              <select id="st-level" v-model="settings.log_level" class="select" :disabled="!settings.log_enabled">
                <option value="info">所有请求</option>
                <option value="error">只记录出错的请求</option>
              </select>
            </UiField>
            <UiField label="保留天数" for="st-keep" help="超过天数的日志会自动删除。"><input id="st-keep" v-model.number="settings.log_retention_days" class="input num" type="number" min="1" max="3650" /></UiField>
          </section>
          <section class="group">
            <h2>记录哪些请求</h2>
            <div class="checks">
              <label v-for="[key, label] in LOG_TYPES" :key="key"><input v-model="settings[key]" type="checkbox" :disabled="!settings.log_enabled" />{{ label }}</label>
            </div>
          </section>
          <section class="group">
            <h2>记录哪些字段</h2>
            <div class="checks">
              <label v-for="[key, label] in LOG_FIELDS" :key="key"><input v-model="settings[key]" type="checkbox" :disabled="!settings.log_enabled" />{{ label }}</label>
            </div>
          </section>
        </template>

        <template v-if="section === 'backup'">
          <section class="group">
            <h2>下载备份</h2>
            <p class="faint p">备份包含站点、源站账号、分享账号和所有设置，用你设的密码加密。忘记密码就无法恢复。</p>
            <div class="grid">
              <UiField label="备份密码" for="bk-pw"><input id="bk-pw" v-model="backup.password" class="input" type="password" autocomplete="new-password" placeholder="至少 6 个字符" /></UiField>
              <UiField label="确认密码" for="bk-pw2"><input id="bk-pw2" v-model="backup.confirm" class="input" type="password" autocomplete="new-password" /></UiField>
            </div>
            <UiSwitch v-model="backup.includeTLS" label="包含 TLS 证书与面板域名" description="恢复时会覆盖目标服务器的证书配置。" />
            <div class="actions"><UiButton variant="primary" :loading="backup.busy" @click="exportBackup">下载加密备份</UiButton></div>
          </section>
          <section class="group">
            <h2>从备份恢复</h2>
            <p class="faint p">会覆盖当前所有数据并自动重启。建议先下载一份当前数据的备份。</p>
            <UiField label="备份文件" for="rs-file"><input id="rs-file" class="input file" type="file" accept=".zip,application/zip" @change="restore.file = ($event.target as HTMLInputElement).files?.[0] || null" /></UiField>
            <UiField label="备份密码" for="rs-pw"><input id="rs-pw" v-model="restore.password" class="input" type="password" autocomplete="off" /></UiField>
            <div class="actions"><UiButton variant="danger" :loading="restore.busy" @click="doRestore">校验并恢复</UiButton></div>
          </section>
        </template>

        <TlsSettings v-if="section === 'tls'" />
        <TelegramSettings v-if="section === 'telegram'" />

        <p v-if="!settings && (section === 'general' || section === 'logs')" class="faint">正在读取…</p>
      </div>
    </div>

    <Transition name="bar">
      <div v-if="dirty" class="savebar">
        <span>有未保存的更改</span>
        <UiButton size="sm" variant="ghost" @click="reset">放弃</UiButton>
        <UiButton size="sm" variant="primary" :loading="saving" @click="save">保存</UiButton>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 180px minmax(0, 1fr); gap: 48px; }
.side { display: flex; flex-direction: column; gap: 2px; position: sticky; top: 24px; align-self: start; }
.side button { height: 32px; padding: 0 10px; border: 0; border-radius: var(--radius-sm); background: transparent; color: var(--text-2); font-size: 13.5px; text-align: left; cursor: pointer; }
.side button:hover { background: var(--surface-hover); color: var(--text); }
.side button.on { background: var(--surface-hover); color: var(--text); font-weight: 500; }
.body { max-width: 560px; }
.group { display: flex; flex-direction: column; gap: 18px; padding-bottom: 32px; margin-bottom: 32px; border-bottom: 1px solid var(--border); }
.group:last-child { border-bottom: 0; }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.p { margin: -8px 0 0; font-size: 13.5px; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.checks { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; }
.checks label { display: flex; align-items: center; gap: 8px; font-size: 13.5px; cursor: pointer; }
.actions { display: flex; justify-content: flex-end; }
.input.file { height: auto; padding: 7px 10px; }
.row-link { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 14px 0; border-bottom: 1px solid var(--border); }
.row-link span { display: flex; flex-direction: column; }
.row-link strong { font-weight: 500; }
.row-link small { color: var(--text-3); font-size: 12.5px; }
.row-link svg { width: 15px; height: 15px; color: var(--text-3); }
.row-link:hover strong { text-decoration: underline; text-underline-offset: 3px; }
.savebar { position: fixed; left: 50%; bottom: 24px; z-index: 40; display: flex; align-items: center; gap: 8px; padding: 8px 8px 8px 16px; transform: translateX(-50%); border-radius: 12px; background: var(--bg-elev); box-shadow: var(--shadow); font-size: 13.5px; }
.savebar span { margin-right: 8px; color: var(--text-2); }
.bar-enter-active, .bar-leave-active { transition: all 220ms var(--ease); }
.bar-enter-from, .bar-leave-to { opacity: 0; transform: translate(-50%, 12px); }
@media (max-width: 860px) {
  .layout { grid-template-columns: minmax(0, 1fr); gap: 20px; }
  .side { position: static; flex-direction: row; overflow-x: auto; scrollbar-width: none; border-bottom: 1px solid var(--border); padding-bottom: 8px; }
  .side button { flex: 0 0 auto; }
  .checks { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .grid { grid-template-columns: minmax(0, 1fr); }
  .savebar { bottom: calc(84px + env(safe-area-inset-bottom)); }
}
</style>
