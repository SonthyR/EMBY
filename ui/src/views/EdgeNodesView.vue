<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Plus, Search, Terminal, Copy, RefreshCw, Pencil, Trash2, Power, RotateCcw, ChevronDown, ArrowUpCircle } from '@lucide/vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiField from '@/components/ui/UiField.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { edgeApi, type EdgeNode, type EdgeNodeInput, type EdgeNodeStatus } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { GB, copyText, formatBytes, relativeTime } from '@/utils/format'

const nodes = ref<EdgeNode[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const filter = ref<'all' | EdgeNodeStatus>('all')

const formOpen = ref(false)
const editing = ref<EdgeNode | null>(null)
const saving = ref(false)
const showAdvanced = ref(false)

const form = reactive({
  name: '',
  public_port: '19090',
  listen_port: '',
  public_host: '',
  public_ip: '',
  tls_mode: 'auto' as 'auto' | 'external' | 'http',
  enabled: true,
  quota_gb: '',
  traffic_reset_day: 0,
  note: '',
})
const errors = reactive<Record<string, string>>({})

const installOpen = ref(false)
const installNode = ref<EdgeNode | null>(null)
const upgradingId = ref<number | null>(null)

const STATUS_LABEL: Record<EdgeNodeStatus, string> = {
  online: '在线',
  waiting: '待接入',
  offline: '离线',
  quota: '超额',
  disabled: '已停用',
}
const STATUS_TONE: Record<EdgeNodeStatus, string> = {
  online: 'good',
  waiting: 'warn',
  offline: 'bad',
  quota: 'warn',
  disabled: 'muted',
}

function formatBps(bps: number | undefined): string {
  const value = Number(bps) || 0
  if (value <= 0) return '0 Mbps'
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(2)} Gbps`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)} Mbps`
  if (value >= 1_000) return `${(value / 1_000).toFixed(0)} Kbps`
  return `${value} bps`
}

function formatUptime(sec: number | undefined): string {
  const s = Math.max(0, Math.floor(Number(sec) || 0))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  return `${Math.max(1, m)} 分钟`
}

function latencyTone(ms: number): string {
  if (ms < 0) return 'bad'
  if (ms < 150) return 'good'
  if (ms < 400) return 'warn'
  return 'bad'
}

function supportsRemoteUpgrade(version: string | undefined): boolean {
  const raw = (version || '').trim().replace(/^v/i, '')
  const parts = raw.split('.').map(n => Number.parseInt(n, 10))
  if (parts.length < 2 || parts.some(Number.isNaN)) return false
  if (parts[0] > 1) return true
  if (parts[0] < 1) return false
  return parts[1] >= 2
}

function upgradeStatusInfo(node: EdgeNode): { text: string; tone: string } | null {
  const raw = (node.upgrade_status || '').trim()
  if (!raw) {
    if (node.online && node.upgrade_available && !supportsRemoteUpgrade(node.agent_version)) {
      return { text: `当前从机为旧版 ${node.agent_version}，不支持远程自更新；请点击「安装命令」在从机重跑一次升级到 ${node.master_version || '最新版'}。`, tone: 'warn' }
    }
    return null
  }
  if (raw === 'pending' || raw === 'queued') {
    if (!supportsRemoteUpgrade(node.agent_version)) {
      return { text: `旧版从机（${node.agent_version || '未知版本'}）无法接收远程升级指令，请点击「安装命令」在从机重跑一次即可原地升级。`, tone: 'warn' }
    }
    return { text: `已下发升级指令（${node.upgrade_target || node.master_version || '最新版'}），等待从机响应…`, tone: 'warn' }
  }
  if (raw === 'updating') return { text: `从机正在下载并校验 ${node.upgrade_target || node.master_version || '更新'}…`, tone: 'warn' }
  if (raw === 'restarting') return { text: '新版本已就绪，从机正在重启…', tone: 'good' }
  if (raw.startsWith('failed:')) return { text: `升级失败：${raw.slice(7).trim()}`, tone: 'bad' }
  return { text: raw, tone: 'warn' }
}

function isUpgradeBusy(node: EdgeNode): boolean {
  const st = (node.upgrade_status || '').trim()
  return st === 'pending' || st === 'queued' || st === 'updating' || st === 'restarting'
}

async function upgradeNode(node: EdgeNode) {
  const target = node.master_version || '最新版本'
  if (
    !(await confirm({
      title: `远程升级「${node.name}」到 ${target}？`,
      message: `从机将自动从 GitHub Release 下载 ${target} 并完成 SHA-256 校验后原子替换重启，期间正在播放的视频会短暂中断。`,
      confirmText: '立即升级',
    }))
  )
    return
  upgradingId.value = node.id
  try {
    await edgeApi.upgrade(node.id, node.master_version || '')
    toast.success(`已向「${node.name}」下发升级指令`)
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '下发升级指令失败'))
  } finally {
    upgradingId.value = null
  }
}

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    nodes.value = await edgeApi.list()
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取从机列表失败')
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => document.visibilityState === 'visible' && !formOpen.value && load(true), 8000)
})
onBeforeUnmount(() => clearInterval(timer))

const counts = computed(() => {
  const result: Record<string, number> = { all: nodes.value.length, online: 0, waiting: 0, offline: 0, quota: 0, disabled: 0 }
  for (const node of nodes.value) {
    result[node.status] = (result[node.status] || 0) + 1
  }
  return result
})

const totalStreams = computed(() => nodes.value.reduce((sum, n) => sum + (n.active_streams || 0), 0))
const totalBps = computed(() => nodes.value.reduce((sum, n) => sum + (n.current_bps || 0), 0))

const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  return nodes.value.filter(node => {
    if (filter.value !== 'all' && node.status !== filter.value) return false
    if (!q) return true
    return [node.name, node.public_url, node.public_host, node.public_ip, node.detected_ip, node.note]
      .some(v => (v || '').toLowerCase().includes(q))
  })
})

function clearErrors() {
  for (const key of Object.keys(errors)) delete errors[key]
}

function openCreate() {
  editing.value = null
  clearErrors()
  showAdvanced.value = false
  Object.assign(form, {
    name: '',
    public_port: '19090',
    listen_port: '',
    public_host: '',
    public_ip: '',
    tls_mode: 'auto',
    enabled: true,
    quota_gb: '',
    traffic_reset_day: 0,
    note: '',
  })
  formOpen.value = true
}

function openEdit(node: EdgeNode) {
  editing.value = node
  clearErrors()
  showAdvanced.value = !!(
    node.public_host ||
    node.public_ip ||
    (node.listen_port && node.listen_port !== node.public_port) ||
    node.traffic_quota > 0 ||
    node.note
  )
  Object.assign(form, {
    name: node.name,
    public_port: String(node.public_port || 19090),
    listen_port: node.listen_port && node.listen_port !== node.public_port ? String(node.listen_port) : '',
    public_host: node.public_host || '',
    public_ip: node.public_ip || '',
    tls_mode: (node.tls_mode === 'external' || node.tls_mode === 'http' ? node.tls_mode : 'auto') as 'auto' | 'external' | 'http',
    enabled: node.enabled,
    quota_gb: node.traffic_quota > 0 ? String(Math.round((node.traffic_quota / GB) * 100) / 100) : '',
    traffic_reset_day: node.traffic_reset_day || 0,
    note: node.note || '',
  })
  formOpen.value = true
}

async function saveForm() {
  clearErrors()
  const name = form.name.trim()
  if (!name) errors.name = '请填写从机名称'

  const publicPort = Number(String(form.public_port).trim())
  if (!Number.isInteger(publicPort) || publicPort < 1 || publicPort > 65535) {
    errors.public_port = '请输入 1–65535 的整数端口'
  }

  const rawListen = String(form.listen_port).trim()
  let listenPort = 0
  if (rawListen !== '') {
    listenPort = Number(rawListen)
    if (!Number.isInteger(listenPort) || listenPort < 1 || listenPort > 65535) {
      errors.listen_port = '请输入 1–65535 的整数，或留空'
    }
  }

  const rawQuota = String(form.quota_gb).trim()
  let quotaGB = 0
  if (rawQuota !== '') {
    quotaGB = Number(rawQuota)
    if (!Number.isFinite(quotaGB) || quotaGB < 0) {
      errors.quota_gb = '请输入有效的数字'
    }
  }

  if (Object.keys(errors).length) return

  const quotaBytes = quotaGB > 0 ? Math.round(quotaGB * GB) : 0
  const payload: EdgeNodeInput = {
    name,
    public_port: publicPort,
    listen_port: listenPort,
    public_host: form.public_host.trim(),
    public_ip: form.public_ip.trim(),
    tls_mode: form.public_host.trim() ? form.tls_mode : 'http',
    enabled: form.enabled,
    traffic_quota: quotaBytes,
    traffic_reset_day: quotaBytes > 0 ? Number(form.traffic_reset_day) || 0 : 0,
    note: form.note.trim(),
  }

  saving.value = true
  try {
    if (editing.value) {
      await edgeApi.update(editing.value.id, payload)
      formOpen.value = false
      toast.success('从机配置已保存')
      await load(true)
    } else {
      const created = await edgeApi.create(payload)
      formOpen.value = false
      toast.success('从机已创建')
      await load(true)
      installNode.value = created
      installOpen.value = true
    }
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}

async function showInstallCommand(node: EdgeNode) {
  try {
    const fresh = await edgeApi.get(node.id)
    installNode.value = fresh
    installOpen.value = true
  } catch (error) {
    toast.error(errorMessage(error, '获取安装命令失败'))
  }
}

async function copyInstallCommand() {
  const cmd = installNode.value?.install_command || ''
  if (!cmd) return
  if (await copyText(cmd)) toast.success('一键安装命令已复制')
  else toast.error('复制失败，请手动选中复制')
}

async function toggleEnabled(node: EdgeNode) {
  try {
    await edgeApi.update(node.id, {
      name: node.name,
      public_port: node.public_port,
      listen_port: node.listen_port,
      public_host: node.public_host,
      public_ip: node.public_ip,
      tls_mode: node.tls_mode,
      enabled: !node.enabled,
      traffic_quota: node.traffic_quota,
      traffic_reset_day: node.traffic_reset_day,
      note: node.note,
    })
    toast.success(node.enabled ? `已停用「${node.name}」` : `已启用「${node.name}」`)
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '操作失败'))
  }
}

async function resetTraffic(node: EdgeNode) {
  if (!(await confirm({ title: `清零「${node.name}」流量？`, message: '将重置该从机的本期已用流量统计为 0。', confirmText: '清零' }))) return
  try {
    await edgeApi.resetTraffic(node.id)
    toast.success('流量已清零')
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '清零失败'))
  }
}

async function resetToken(node: EdgeNode) {
  if (!(await confirm({
    title: `重置「${node.name}」密钥？`,
    message: '重置后原从机将立即断开认证，需要在从机上重新执行新的一键安装命令。',
    confirmText: '重置密钥',
    danger: true,
  }))) return
  try {
    const updated = await edgeApi.resetToken(node.id)
    toast.success('密钥已重置')
    await load(true)
    installNode.value = updated
    installOpen.value = true
  } catch (error) {
    toast.error(errorMessage(error, '重置失败'))
  }
}

async function removeNode(node: EdgeNode) {
  if (!(await confirm({
    title: `删除从机「${node.name}」？`,
    message: '删除后该节点将从三网线路分流中移除，不会影响主机和其他从机。',
    confirmText: '删除',
    danger: true,
  }))) return
  try {
    await edgeApi.remove(node.id)
    toast.success('已删除从机')
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '删除失败'))
  }
}

const FILTERS: { key: 'all' | EdgeNodeStatus; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'online', label: '在线' },
  { key: 'waiting', label: '待接入' },
  { key: 'offline', label: '离线' },
  { key: 'quota', label: '超额' },
  { key: 'disabled', label: '已停用' },
]
</script>

<template>
  <div>
    <PageHeader title="从机" description="添加普通 VPS 或 NAT 小鸡作为轻量媒体中转节点，视频播放自动就近卸载，登录与海报仍由主机接管。">
      <UiButton variant="primary" @click="openCreate"><Plus />添加从机</UiButton>
    </PageHeader>

    <dl class="stats">
      <div><dt>从机</dt><dd class="num">{{ counts.all }}</dd></div>
      <div><dt>在线</dt><dd class="num">{{ counts.online }}</dd></div>
      <div><dt>当前播放流</dt><dd class="num">{{ totalStreams }}</dd></div>
      <div><dt>实时带宽</dt><dd class="num">{{ formatBps(totalBps) }}</dd></div>
    </dl>

    <div class="toolbar">
      <nav class="tabs" aria-label="按状态筛选">
        <button v-for="f in FILTERS" :key="f.key" type="button" :class="{ on: filter === f.key }" @click="filter = f.key">
          {{ f.label }}<span v-if="counts[f.key]" class="n">{{ counts[f.key] }}</span>
        </button>
      </nav>
      <label class="search">
        <Search />
        <input v-model="query" data-page-search type="search" placeholder="搜索" aria-label="搜索从机" />
      </label>
    </div>

    <div v-if="loading" class="list">
      <div v-for="n in 3" :key="n" class="card skeleton"><span /></div>
    </div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!nodes.length" class="empty">
      <h2>还没有从机节点</h2>
      <p>只需填写「名称」和「对外端口」，复制一条命令到任意 Linux VPS 或 NAT 机器即可一键接入，内存仅占约 15–25 MB。</p>
      <UiButton variant="primary" @click="openCreate"><Plus />添加第一个从机</UiButton>
    </div>
    <div v-else-if="!visible.length" class="empty">
      <p>没有匹配的从机节点。</p>
    </div>
    <div v-else class="list">
      <article v-for="node in visible" :key="node.id" class="card" :class="{ muted: !node.enabled }">
        <div class="card-main">
          <div class="title-row">
            <span class="status-pill" :class="STATUS_TONE[node.status]">
              <span class="dot" />{{ STATUS_LABEL[node.status] }}
            </span>
            <h3>{{ node.name }}</h3>
            <span v-if="node.agent_version" class="tag">{{ node.agent_version }}</span>
            <span v-if="node.public_host && node.tls_active" class="tag good">HTTPS 自动证书</span>
            <span v-else-if="node.public_host && node.tls_mode === 'external'" class="tag">外部 HTTPS</span>
            <span v-if="node.listen_port && node.listen_port !== node.public_port" class="tag">
              NAT {{ node.listen_port }} → {{ node.public_port }}
            </span>
          </div>

          <div class="addr-row">
            <span v-if="node.public_url" class="mono">{{ node.public_url }}</span>
            <span v-else class="faint">等待从机首次心跳自动上报公网 IP（端口 {{ node.public_port }}）</span>
            <span v-if="node.last_seen_at" class="faint sep">最近心跳 {{ relativeTime(node.last_seen_at) }}</span>
            <span v-if="node.online && node.memory_bytes" class="faint sep">内存 {{ formatBytes(node.memory_bytes) }}</span>
            <span v-if="node.online && node.uptime_sec" class="faint sep">运行 {{ formatUptime(node.uptime_sec) }}</span>
          </div>

          <div v-if="node.online && node.upstream_latencies?.length" class="latency-row">
            <span class="faint lat-label">回源延迟</span>
            <span v-for="lat in node.upstream_latencies" :key="lat.site_name" class="lat-pill" :class="latencyTone(lat.latency_ms)">
              {{ lat.site_name }} {{ lat.latency_ms >= 0 ? `${lat.latency_ms}ms` : '不通' }}
            </span>
          </div>

          <p v-if="upgradeStatusInfo(node)" class="upgrade-note" :class="upgradeStatusInfo(node)!.tone">
            {{ upgradeStatusInfo(node)!.text }}
          </p>

          <p v-if="node.note" class="note">{{ node.note }}</p>
        </div>

        <div class="card-metrics">
          <div class="metric">
            <span class="label">播放 / 带宽</span>
            <span class="val num">{{ node.active_streams }} 流 · {{ formatBps(node.current_bps) }}</span>
          </div>
          <div class="metric traffic">
            <span class="label">本期流量</span>
            <span class="val num">
              {{ formatBytes(node.traffic_used) }}<span v-if="node.traffic_quota > 0" class="faint"> / {{ formatBytes(node.traffic_quota) }}</span>
            </span>
            <UiProgress v-if="node.traffic_quota > 0" :value="node.traffic_used" :max="node.traffic_quota" />
          </div>
        </div>

        <div class="card-actions">
          <UiButton
            v-if="node.online && node.upgrade_available && supportsRemoteUpgrade(node.agent_version) && !isUpgradeBusy(node)"
            size="sm"
            variant="primary"
            :loading="upgradingId === node.id"
            @click="upgradeNode(node)"
          >
            <ArrowUpCircle />升级 {{ node.master_version }}
          </UiButton>
          <UiButton size="sm" :variant="node.online && node.upgrade_available && supportsRemoteUpgrade(node.agent_version) && !isUpgradeBusy(node) ? 'secondary' : 'primary'" @click="showInstallCommand(node)">
            <Terminal />安装命令
          </UiButton>
          <UiButton size="sm" @click="openEdit(node)"><Pencil />编辑</UiButton>
          <UiButton size="sm" variant="ghost" :title="node.enabled ? '停用节点' : '启用节点'" @click="toggleEnabled(node)">
            <Power />
          </UiButton>
          <UiButton size="sm" variant="ghost" title="清零本期流量" @click="resetTraffic(node)">
            <RotateCcw />
          </UiButton>
          <UiButton size="sm" variant="ghost" title="重置通信密钥" @click="resetToken(node)">
            <RefreshCw />
          </UiButton>
          <UiButton size="sm" variant="ghost" title="删除从机" @click="removeNode(node)">
            <Trash2 />
          </UiButton>
        </div>
      </article>
    </div>

    <UiDrawer
      :open="formOpen"
      :title="editing ? '编辑从机' : '添加从机'"
      :subtitle="editing ? editing.name : '默认仅需名称与对外端口，无需域名也能直接接入'"
      :width="520"
      :dismissible="!saving"
      @close="formOpen = false"
    >
      <form class="form" novalidate @submit.prevent="saveForm">
        <fieldset>
          <legend>基础信息</legend>
          <UiField label="从机名称" for="en-name" :error="errors.name">
            <input id="en-name" v-model="form.name" class="input" maxlength="64" autofocus placeholder="例如：香港 CN2、日本 NAT、新加坡移动" />
          </UiField>
          <UiField
            label="对外端口"
            for="en-port"
            :error="errors.public_port"
            help="普通 VPS 填从机监听端口（默认 19090）；NAT 小鸡填服务商分配的公网映射端口。"
          >
            <input id="en-port" v-model="form.public_port" class="input num" type="number" min="1" max="65535" inputmode="numeric" />
          </UiField>
        </fieldset>

        <fieldset>
          <button type="button" class="adv-toggle" :class="{ open: showAdvanced }" @click="showAdvanced = !showAdvanced">
            <ChevronDown class="chev" />
            <span>更多选项</span>
            <small class="faint">域名 / HTTPS / NAT 内网端口 / 流量配额</small>
          </button>

          <template v-if="showAdvanced">
            <UiField
              label="绑定域名"
              for="en-host"
              help="留空则首次心跳自动上报公网 IP（http://IP:端口）。若填主机泛域名下的子域名（如 hk.example.com），可自动同步泛域名证书实现 HTTPS。"
            >
              <input id="en-host" v-model="form.public_host" class="input mono" autocapitalize="none" spellcheck="false" placeholder="可选，例如 hk.example.com" />
            </UiField>

            <UiField v-if="form.public_host.trim()" label="TLS 模式" for="en-tls">
              <select id="en-tls" v-model="form.tls_mode" class="select">
                <option value="auto">自动同步主机泛域名证书（推荐，支持任意端口 HTTPS）</option>
                <option value="external">外部反代 HTTPS（Nginx / Caddy 等）</option>
                <option value="http">仅 HTTP</option>
              </select>
            </UiField>

            <div class="grid">
              <UiField label="NAT 本机监听端口" for="en-listen" :error="errors.listen_port" help="内外端口不同时填写，留空同对外端口">
                <input id="en-listen" v-model="form.listen_port" class="input num" type="number" min="1" max="65535" inputmode="numeric" placeholder="同对外端口" />
              </UiField>
              <UiField label="公网 IP 覆盖" for="en-ip" help="留空自动识别，出入口 IP 不同时填写">
                <input id="en-ip" v-model="form.public_ip" class="input mono" autocapitalize="none" spellcheck="false" placeholder="自动识别" />
              </UiField>
            </div>

            <div class="grid">
              <UiField label="流量额度" for="en-quota" :error="errors.quota_gb" help="GB，超额自动降级到其他节点">
                <input id="en-quota" v-model="form.quota_gb" class="input num" type="number" min="0" step="1" inputmode="decimal" placeholder="不限" />
              </UiField>
              <UiField label="每月重置" for="en-reset" help="到这天清零已用流量">
                <select id="en-reset" v-model.number="form.traffic_reset_day" class="select">
                  <option :value="0">不重置</option>
                  <option v-for="day in 28" :key="day" :value="day">每月 {{ day }} 日</option>
                </select>
              </UiField>
            </div>

            <UiField label="备注" for="en-note">
              <input id="en-note" v-model="form.note" class="input" maxlength="200" placeholder="可选，例如线路说明、机房位置" />
            </UiField>

            <UiSwitch v-if="editing" v-model="form.enabled" label="启用" description="关闭后该节点不会参与三网分流" />
          </template>
        </fieldset>

        <button type="submit" hidden />
      </form>
      <template #footer>
        <UiButton :disabled="saving" @click="formOpen = false">取消</UiButton>
        <UiButton variant="primary" :loading="saving" @click="saveForm">
          {{ editing ? '保存更改' : '创建并生成安装命令' }}
        </UiButton>
      </template>
    </UiDrawer>

    <UiDialog :open="installOpen" :title="`部署从机 · ${installNode?.name || ''}`" :width="600" @close="installOpen = false">
      <div v-if="installNode" class="install-box">
        <p class="faint">在目标 Linux 服务器（普通 VPS 或 NAT 小鸡均可）以 root 或 sudo 权限执行以下命令：</p>
        <pre class="cmd"><code>{{ installNode.install_command }}</code></pre>
        <ul class="tips">
          <li>从机仅运行轻量 <code>meridian agent</code> 进程（约 15–25 MB 内存，无数据库/无面板），启动后约 5 秒自动上线。</li>
          <li>请确保放行 TCP <strong>{{ installNode.public_port }}</strong> 端口（NAT 机器请确认端口映射指向本机 {{ installNode.listen_port || installNode.public_port }} 端口）。</li>
          <li v-if="installNode.public_host && installNode.tls_mode === 'auto'">
            已启用自动证书同步：请将 <code>{{ installNode.public_host }}</code> 解析到该从机 IP，主机将在心跳中自动下发匹配的泛域名 TLS 证书。
          </li>
          <li v-else-if="!installNode.public_host">
            未填写域名：从机首次心跳会自动上报公网 IP 并生成 <code>http://IP:{{ installNode.public_port }}</code> 接入地址。
          </li>
        </ul>
      </div>
      <template #footer>
        <UiButton @click="installOpen = false">关闭</UiButton>
        <UiButton variant="primary" @click="copyInstallCommand"><Copy />复制安装命令</UiButton>
      </template>
    </UiDialog>
  </div>
</template>

<style scoped>
.stats { display: flex; gap: 48px; margin: 0 0 32px; }
.stats div { display: flex; flex-direction: column; gap: 2px; }
.stats dt { font-size: 12.5px; color: var(--text-3); }
.stats dd { margin: 0; font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }

.toolbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); margin-bottom: 16px; }
.tabs { display: flex; gap: 22px; overflow-x: auto; scrollbar-width: none; }
.tabs::-webkit-scrollbar { display: none; }
.tabs button { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; white-space: nowrap; cursor: pointer; transition: color var(--dur) var(--ease); }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.n { font-size: 11.5px; color: var(--text-3); }

.search { display: flex; align-items: center; gap: 8px; width: 220px; height: 30px; padding: 0 8px; border-radius: var(--radius-sm); color: var(--text-3); transition: background var(--dur) var(--ease); }
.search:focus-within { background: var(--surface-hover); }
.search svg { width: 14px; height: 14px; flex: 0 0 auto; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13.5px; color: var(--text); }
.search input::placeholder { color: var(--text-3); }

.list { display: flex; flex-direction: column; gap: 10px; }
.card { display: grid; grid-template-columns: minmax(0, 1.5fr) minmax(200px, 1fr) auto; align-items: center; gap: 16px; padding: 16px 18px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-elev); transition: border-color var(--dur) var(--ease); }
.card:hover { border-color: var(--border-strong); }
.card.muted { opacity: 0.65; }
.card.skeleton { height: 84px; }
.card.skeleton span { display: block; width: 45%; height: 14px; border-radius: 6px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }

.title-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.title-row h3 { margin: 0; font-size: 15px; font-weight: 600; }
.status-pill { display: inline-flex; align-items: center; gap: 5px; height: 22px; padding: 0 8px; border-radius: 999px; font-size: 11.5px; font-weight: 500; background: var(--surface); color: var(--text-2); }
.status-pill .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status-pill.good { color: var(--good); background: color-mix(in srgb, var(--good) 12%, transparent); }
.status-pill.warn { color: var(--warn); background: color-mix(in srgb, var(--warn) 12%, transparent); }
.status-pill.bad { color: var(--bad); background: color-mix(in srgb, var(--bad) 12%, transparent); }
.status-pill.muted { color: var(--text-3); }

.tag { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: 5px; font-size: 11px; background: var(--surface); color: var(--text-2); font-family: var(--mono); }
.tag.good { color: var(--good); background: color-mix(in srgb, var(--good) 10%, transparent); font-family: inherit; }

.addr-row { display: flex; align-items: center; gap: 10px; margin-top: 6px; font-size: 12.5px; flex-wrap: wrap; }
.mono { font-family: var(--mono); color: var(--text-2); word-break: break-all; }
.faint { color: var(--text-3); }
.sep::before { content: '·'; margin-right: 10px; }
.latency-row { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-top: 7px; font-size: 11.5px; }
.lat-label { margin-right: 2px; }
.lat-pill { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: 5px; font-family: var(--mono); font-size: 11px; background: var(--surface); color: var(--text-2); }
.lat-pill.good { color: var(--good); background: color-mix(in srgb, var(--good) 10%, transparent); }
.lat-pill.warn { color: var(--warn); background: color-mix(in srgb, var(--warn) 12%, transparent); }
.lat-pill.bad { color: var(--bad); background: color-mix(in srgb, var(--bad) 12%, transparent); }
.upgrade-note { margin: 7px 0 0; font-size: 12px; }
.upgrade-note.warn { color: var(--warn); }
.upgrade-note.good { color: var(--good); }
.upgrade-note.bad { color: var(--bad); }
.note { margin: 6px 0 0; font-size: 12.5px; color: var(--text-3); }

.card-metrics { display: flex; flex-direction: column; gap: 8px; }
.metric { display: flex; flex-direction: column; gap: 2px; }
.metric .label { font-size: 11.5px; color: var(--text-3); }
.metric .val { font-size: 13px; font-weight: 500; }

.card-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; justify-content: flex-end; }

.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 72px 20px; text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 600; }
.empty p { max-width: 400px; margin: 0 0 8px; color: var(--text-2); line-height: 1.6; }

/* Drawer form matches ShareUserForm.vue */
.form { display: flex; flex-direction: column; gap: 4px; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 18px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
fieldset:first-of-type { padding-top: 0; }
fieldset:last-of-type { border-bottom: 0; }
legend { float: left; width: 100%; margin-bottom: 14px; padding: 0; font-size: 12px; font-weight: 500; color: var(--text-3); }
legend + * { clear: both; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }

.adv-toggle { display: flex; align-items: center; gap: 8px; width: 100%; padding: 0; border: 0; background: transparent; cursor: pointer; font-size: 13px; font-weight: 500; color: var(--text); text-align: left; }
.adv-toggle small { margin-left: auto; font-size: 12px; font-weight: 400; }
.adv-toggle .chev { width: 15px; height: 15px; color: var(--text-3); transition: transform 180ms var(--ease); flex: 0 0 auto; }
.adv-toggle.open .chev { transform: rotate(180deg); }

.install-box p { margin: 0 0 12px; font-size: 13.5px; }
.cmd { margin: 0 0 14px; padding: 14px; border-radius: var(--radius-sm); background: var(--surface); border: 1px solid var(--border); overflow-x: auto; font-family: var(--mono); font-size: 12.5px; line-height: 1.55; white-space: pre-wrap; word-break: break-all; }
.tips { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 6px; font-size: 12.5px; color: var(--text-2); line-height: 1.55; }
.tips code { font-family: var(--mono); font-size: 12px; padding: 1px 5px; border-radius: 4px; background: var(--surface); }

@media (max-width: 860px) {
  .stats { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px 24px; margin-bottom: 24px; }
  .toolbar { flex-direction: column-reverse; align-items: stretch; gap: 8px; }
  .search { width: 100%; height: 36px; background: var(--surface-hover); }
  .card { grid-template-columns: 1fr; }
  .card-actions { justify-content: flex-start; }
}
@media (max-width: 520px) {
  .grid { grid-template-columns: minmax(0, 1fr); }
}
</style>
