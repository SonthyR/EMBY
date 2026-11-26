<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Activity, Plus, Search, Terminal, Copy, RefreshCw, Pencil, Trash2, Power, RotateCcw, ChevronDown, ArrowUpCircle, GripVertical, ShieldAlert } from '@lucide/vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SpeedtestDrawer from '@/components/edge/SpeedtestDrawer.vue'
import IspProbeDrawer from '@/components/edge/IspProbeDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiField from '@/components/ui/UiField.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { edgeApi, edgeAutoHttpsApi, masterNodeApi, type EdgeAutoHTTPSStatus, type EdgeNode, type EdgeNodeInput, type EdgeNodeStatus, type MasterNodeInput } from '@/api'
import { usePointerSort } from '@/composables/usePointerSort'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { GB, copyText, formatBytes, relativeTime } from '@/utils/format'

const masterNode = ref<EdgeNode | null>(null)
const edgeAutoHttps = ref<EdgeAutoHTTPSStatus | null>(null)
const nodes = ref<EdgeNode[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const filter = ref<'all' | EdgeNodeStatus>('all')

const formOpen = ref(false)
const editing = ref<EdgeNode | null>(null)
const saving = ref(false)
const showAdvanced = ref(false)

const masterFormOpen = ref(false)
const savingMaster = ref(false)
const probingMaster = ref(false)
const masterForm = reactive({
  name: '',
  quota_gb: '',
  traffic_reset_day: 0,
  max_balance_streams: '2',
  note: '',
})
const masterErrors = reactive<Record<string, string>>({})

const form = reactive({
  name: '',
  public_port: '19090',
  listen_port: '',
  public_host: '',
  public_ip: '',
  tls_mode: 'auto' as 'auto' | 'external' | 'http',
  auto_https: false,
  enabled: true,
  auto_circuit_breaker: true,
  probe_mode: 'tcp' as 'tcp' | 'icmp' | 'off',
  quota_gb: '',
  traffic_reset_day: 0,
  max_balance_streams: '2',
  note: '',
})
const errors = reactive<Record<string, string>>({})

const installOpen = ref(false)
const installNode = ref<EdgeNode | null>(null)
const upgradingId = ref<number | null>(null)
const upgradingAll = ref(false)
const DEFAULT_UNINSTALL_COMMAND = 'curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/uninstall-agent.sh | sh'
const uninstallCommand = computed(() => installNode.value?.uninstall_command || DEFAULT_UNINSTALL_COMMAND)

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
  if (node.is_master) return null
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

function isNodeUpgradeable(node: EdgeNode): boolean {
  return !node.is_master && !!(node.online && node.upgrade_available && supportsRemoteUpgrade(node.agent_version) && !isUpgradeBusy(node))
}

const upgradeableNodes = computed(() => nodes.value.filter(isNodeUpgradeable))

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

async function upgradeAllNodes() {
  const targets = [...upgradeableNodes.value]
  if (!targets.length || upgradingAll.value) return
  const targetVersion = targets[0]?.master_version || '最新版本'
  const previewNames = targets
    .slice(0, 5)
    .map(n => n.name)
    .join('、')
  const suffix = targets.length > 5 ? ` 等 ${targets.length} 台从机` : ''
  if (
    !(await confirm({
      title: `一键升级 ${targets.length} 台从机到 ${targetVersion}？`,
      message: `将向 ${previewNames}${suffix} 同时下发远程升级指令，升级并重启期间正在播放的视频会短暂中断。`,
      confirmText: `升级全部（${targets.length} 台）`,
    }))
  )
    return
  upgradingAll.value = true
  try {
    const results = await Promise.allSettled(targets.map(n => edgeApi.upgrade(n.id, n.master_version || '')))
    const failedNames: string[] = []
    let succeeded = 0
    results.forEach((res, idx) => {
      if (res.status === 'fulfilled') succeeded++
      else failedNames.push(targets[idx].name)
    })
    if (failedNames.length === 0) {
      toast.success(`已向 ${succeeded} 台从机下发升级指令`)
    } else if (succeeded > 0) {
      toast.error(`${succeeded} 台已下发升级，${failedNames.length} 台失败（${failedNames.join('、')}）`)
    } else {
      toast.error(`全部 ${failedNames.length} 台下发升级指令失败`)
    }
    await load(true)
  } finally {
    upgradingAll.value = false
  }
}

const savingOrder = ref(false)
const pointerSort = usePointerSort()

async function load(silent = false) {
  if (silent && (pointerSort.isDragging.value || savingOrder.value)) return
  if (!silent) loading.value = true
  try {
    const masterPromise = masterNodeApi?.get ? masterNodeApi.get().catch(() => null) : Promise.resolve(null)
    const autoHttpsPromise = edgeAutoHttpsApi?.get ? edgeAutoHttpsApi.get().catch(() => null) : Promise.resolve(null)
    const [edgeList, master, autoCfg] = await Promise.all([edgeApi.list(), masterPromise, autoHttpsPromise])
    nodes.value = edgeList
    masterNode.value = master
    edgeAutoHttps.value = autoCfg
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取节点列表失败')
  } finally {
    loading.value = false
  }
}

const autoHttpsReady = computed(() => !!(edgeAutoHttps.value?.enabled && edgeAutoHttps.value?.base_domain && edgeAutoHttps.value?.token_configured))

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => document.visibilityState === 'visible' && !formOpen.value && !masterFormOpen.value && load(true), 8000)
})
onBeforeUnmount(() => clearInterval(timer))

const allNodes = computed<EdgeNode[]>(() => (masterNode.value ? [masterNode.value, ...nodes.value] : [...nodes.value]))

const counts = computed(() => {
  const result: Record<string, number> = { all: allNodes.value.length, online: 0, waiting: 0, offline: 0, quota: 0, disabled: 0 }
  for (const node of allNodes.value) {
    result[node.status] = (result[node.status] || 0) + 1
  }
  return result
})

const totalStreams = computed(() => allNodes.value.reduce((sum, n) => sum + (n.active_streams || 0), 0))
const totalBps = computed(() => allNodes.value.reduce((sum, n) => sum + (n.current_bps || 0), 0))

const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  return allNodes.value.filter(node => {
    if (filter.value !== 'all' && node.status !== filter.value) return false
    if (!q) return true
    return [node.name, node.is_master ? '主机 master 本机运行' : '', node.public_url, node.public_host, node.public_ip, node.detected_ip, node.note]
      .some(v => (v || '').toLowerCase().includes(q))
  })
})

const showAddEdgeHintCard = computed(() => !!masterNode.value && nodes.value.length === 0 && filter.value === 'all' && !query.value.trim())
const canSortEdges = computed(() => filter.value === 'all' && !query.value.trim() && nodes.value.length > 1 && !savingOrder.value)

function startEdgeSort(event: PointerEvent, node: EdgeNode) {
  if (node.is_master || !canSortEdges.value) return
  const itemEl = (event.currentTarget as HTMLElement | null)?.closest<HTMLElement>('[data-edge-sort-id]')
  const containerEl = itemEl?.closest<HTMLElement>('.list')
  if (!itemEl || !containerEl) return

  pointerSort.start<EdgeNode>({
    event,
    itemEl,
    containerEl,
    itemSelector: '[data-edge-sort-id]',
    getKeyFromEl: el => {
      const id = Number(el.dataset.edgeSortId)
      return Number.isNaN(id) ? null : id
    },
    activeKey: node.id,
    scope: 'edge-nodes',
    mode: 'grid',
    getList: () => nodes.value,
    getItemKey: item => item.id,
    applyList: next => {
      nodes.value = next
    },
    onCommit: async (next, previous) => {
      savingOrder.value = true
      try {
        await edgeApi.reorder(next.map(n => n.id))
        toast.success('节点顺序已保存')
      } catch (error) {
        nodes.value = previous
        toast.error(errorMessage(error, '保存节点顺序失败'))
      } finally {
        savingOrder.value = false
      }
    },
  })
}

function clearErrors() {
  for (const key of Object.keys(errors)) delete errors[key]
}

function clearMasterErrors() {
  for (const key of Object.keys(masterErrors)) delete masterErrors[key]
}

function openMasterEdit() {
  const m = masterNode.value
  if (!m) return
  clearMasterErrors()
  Object.assign(masterForm, {
    name: m.name || '主机',
    quota_gb: m.traffic_quota > 0 ? String(Math.round((m.traffic_quota / GB) * 100) / 100) : '',
    traffic_reset_day: m.traffic_reset_day || 0,
    max_balance_streams: String(m.max_balance_streams || 2),
    note: m.note || '',
  })
  masterFormOpen.value = true
}

async function saveMasterForm() {
  clearMasterErrors()
  const name = masterForm.name.trim()
  if (!name) masterErrors.name = '请填写主机名称'

  const rawQuota = String(masterForm.quota_gb).trim()
  let quotaGB = 0
  if (rawQuota !== '') {
    quotaGB = Number(rawQuota)
    if (!Number.isFinite(quotaGB) || quotaGB < 0) {
      masterErrors.quota_gb = '请输入有效的数字'
    }
  }

  const maxBalance = Number.parseInt(String(masterForm.max_balance_streams).trim(), 10)
  if (!Number.isInteger(maxBalance) || maxBalance < 1 || maxBalance > 99) {
    masterErrors.max_balance_streams = '请输入 1–99 的整数'
  }

  if (Object.keys(masterErrors).length) return

  const quotaBytes = quotaGB > 0 ? Math.round(quotaGB * GB) : 0
  const payload: MasterNodeInput = {
    name,
    traffic_quota: quotaBytes,
    traffic_reset_day: quotaBytes > 0 ? Number(masterForm.traffic_reset_day) || 0 : 0,
    max_balance_streams: maxBalance,
    note: masterForm.note.trim(),
  }

  savingMaster.value = true
  try {
    masterNode.value = await masterNodeApi.update(payload)
    masterFormOpen.value = false
    toast.success('主机配置已保存')
  } catch (error) {
    toast.error(errorMessage(error, '保存主机配置失败'))
  } finally {
    savingMaster.value = false
  }
}

async function probeMaster() {
  if (probingMaster.value) return
  probingMaster.value = true
  try {
    masterNode.value = await masterNodeApi.probe()
    toast.success('主机回源延迟已刷新')
  } catch (error) {
    toast.error(errorMessage(error, '刷新延迟失败'))
  } finally {
    probingMaster.value = false
  }
}

async function resetMasterTraffic() {
  const name = masterNode.value?.name || '主机'
  if (!(await confirm({ title: `清零「${name}」流量？`, message: '将重置主机的本期已用流量统计为 0，不影响各站点累计账本。', confirmText: '清零' }))) return
  try {
    masterNode.value = await masterNodeApi.resetTraffic()
    toast.success('主机流量已清零')
  } catch (error) {
    toast.error(errorMessage(error, '清零失败'))
  }
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
    auto_https: autoHttpsReady.value,
    enabled: true,
    auto_circuit_breaker: true,
    probe_mode: 'tcp',
    quota_gb: '',
    traffic_reset_day: 0,
    max_balance_streams: '2',
    note: '',
  })
  formOpen.value = true
}

function openEdit(node: EdgeNode) {
  if (node.is_master) {
    openMasterEdit()
    return
  }
  editing.value = node
  clearErrors()
  showAdvanced.value = !!(
    node.public_host ||
    node.public_ip ||
    (node.listen_port && node.listen_port !== node.public_port) ||
    node.traffic_quota > 0 ||
    (node.max_balance_streams && node.max_balance_streams !== 2) ||
    node.note
  )
  Object.assign(form, {
    name: node.name,
    public_port: String(node.public_port || 19090),
    listen_port: node.listen_port && node.listen_port !== node.public_port ? String(node.listen_port) : '',
    public_host: node.public_host || '',
    public_ip: node.public_ip || '',
    tls_mode: (node.tls_mode === 'external' || node.tls_mode === 'http' ? node.tls_mode : 'auto') as 'auto' | 'external' | 'http',
    auto_https: !!node.auto_https,
    enabled: node.enabled,
    auto_circuit_breaker: node.auto_circuit_breaker !== false,
    probe_mode: (node.probe_mode === 'icmp' || node.probe_mode === 'off' ? node.probe_mode : 'tcp') as 'tcp' | 'icmp' | 'off',
    quota_gb: node.traffic_quota > 0 ? String(Math.round((node.traffic_quota / GB) * 100) / 100) : '',
    traffic_reset_day: node.traffic_reset_day || 0,
    max_balance_streams: String(node.max_balance_streams || 2),
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

  const maxBalance = Number.parseInt(String(form.max_balance_streams).trim(), 10)
  if (!Number.isInteger(maxBalance) || maxBalance < 1 || maxBalance > 99) {
    errors.max_balance_streams = '请输入 1–99 的整数'
  }

  if (Object.keys(errors).length) return

  const quotaBytes = quotaGB > 0 ? Math.round(quotaGB * GB) : 0
  const payload: EdgeNodeInput = {
    name,
    public_port: publicPort,
    listen_port: listenPort,
    public_host: form.public_host.trim(),
    public_ip: form.public_ip.trim(),
    tls_mode: form.auto_https ? 'auto' : form.public_host.trim() ? form.tls_mode : 'http',
    auto_https: form.auto_https,
    enabled: form.enabled,
    auto_circuit_breaker: form.auto_circuit_breaker,
    probe_mode: form.probe_mode,
    traffic_quota: quotaBytes,
    traffic_reset_day: quotaBytes > 0 ? Number(form.traffic_reset_day) || 0 : 0,
    max_balance_streams: maxBalance,
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

async function copyUninstallCommand() {
  const cmd = uninstallCommand.value
  if (!cmd) return
  if (await copyText(cmd)) toast.success('一键卸载命令已复制')
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
      auto_https: !!node.auto_https,
      enabled: !node.enabled,
      auto_circuit_breaker: node.auto_circuit_breaker !== false,
      probe_mode: node.probe_mode || 'tcp',
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
  if (node.is_master) {
    await resetMasterTraffic()
    return
  }
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

const speedtestOpen = ref(false)
const speedtestNodeId = ref(0)
const ispProbeOpen = ref(false)
const probingIspNodeId = ref<number | null>(null)
const resettingCircuitNodeId = ref<number | null>(null)

function openSpeedtest(nodeId: number) {
  speedtestNodeId.value = nodeId
  speedtestOpen.value = true
}

function speedPillTone(mbps: number): string {
  if (mbps >= 150) return 'good'
  if (mbps >= 50) return 'warn'
  return 'bad'
}

function ispHealthTone(h: { tripped: boolean; latency_ms: number; consec_fails: number }): string {
  if (h.tripped) return 'bad'
  if (h.latency_ms <= 0 || h.consec_fails > 0) return 'warn'
  if (h.latency_ms < 180) return 'good'
  return 'warn'
}

function ispHealthText(h: { isp_label: string; tripped: boolean; latency_ms: number; consec_fails: number }): string {
  if (h.tripped) return `${h.isp_label} 已熔断`
  if (h.latency_ms > 0) return `${h.isp_label} ${h.latency_ms}ms`
  return h.consec_fails > 0 ? `${h.isp_label} 不通(${h.consec_fails})` : `${h.isp_label} 探测失败`
}

async function probeNodeISP(node: EdgeNode) {
  if (probingIspNodeId.value !== null) return
  probingIspNodeId.value = node.id
  try {
    await edgeApi.probeISP(node.id)
    toast.success(`已向「${node.name}」下发立即三网复测指令，约 5–10 秒后刷新结果`)
  } catch (error) {
    toast.error(errorMessage(error, '下发复测失败'))
  } finally {
    probingIspNodeId.value = null
  }
}

async function resetNodeCircuit(node: EdgeNode) {
  if (resettingCircuitNodeId.value !== null) return
  resettingCircuitNodeId.value = node.id
  try {
    await edgeApi.resetCircuit(node.id)
    toast.success(`已手动解除「${node.name}」的阻断熔断状态`)
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '解除熔断失败'))
  } finally {
    resettingCircuitNodeId.value = null
  }
}
</script>

<template>
  <div>
    <PageHeader title="节点" description="统一查看主机与各从机节点的在线状态、回源延迟、三网回程测速与实时流量；添加从机后，视频播放自动就近卸载。">
      <UiButton
        v-if="upgradeableNodes.length"
        variant="accent"
        :loading="upgradingAll"
        :disabled="upgradingId !== null"
        @click="upgradeAllNodes"
      >
        <ArrowUpCircle />全部升级（{{ upgradeableNodes.length }}）
      </UiButton>
      <UiButton @click="ispProbeOpen = true"><ShieldAlert />阻断熔断</UiButton>
      <UiButton @click="openSpeedtest(0)"><Activity />测速中心</UiButton>
      <UiButton variant="primary" @click="openCreate"><Plus />添加从机</UiButton>
    </PageHeader>

    <dl class="kpi-grid">
      <div class="kpi-card">
        <dt>节点总数</dt>
        <dd class="num">
          {{ counts.all }}
          <span v-if="masterNode" class="kpi-sub">1 主 · {{ nodes.length }} 从</span>
        </dd>
      </div>
      <div class="kpi-card"><dt><span class="dot good" />在线节点</dt><dd class="num">{{ counts.online }}</dd></div>
      <div class="kpi-card"><dt>当前播放流</dt><dd class="num">{{ totalStreams }}</dd></div>
      <div class="kpi-card"><dt>实时转发带宽</dt><dd class="num">{{ formatBps(totalBps) }}</dd></div>
    </dl>

    <div class="toolbar-card">
      <nav class="seg" aria-label="按状态筛选">
        <button v-for="f in FILTERS" :key="f.key" type="button" :class="{ on: filter === f.key }" @click="filter = f.key">
          {{ f.label }}<span v-if="counts[f.key]" class="n">{{ counts[f.key] }}</span>
        </button>
      </nav>
      <label class="search">
        <Search />
        <input v-model="query" data-page-search type="search" placeholder="搜索节点名称或地址…" aria-label="搜索节点" />
      </label>
    </div>

    <div v-if="loading" class="list">
      <div v-for="n in 3" :key="n" class="card skeleton"><span /></div>
    </div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!allNodes.length" class="empty">
      <h2>还没有节点</h2>
      <p>只需填写「名称」和「对外端口」，复制一条命令到任意 Linux VPS 或 NAT 机器即可一键接入，内存仅占约 15–25 MB。</p>
      <UiButton variant="primary" @click="openCreate"><Plus />添加第一个从机</UiButton>
    </div>
    <div v-else-if="!visible.length" class="empty">
      <p>没有匹配的节点。</p>
    </div>
    <TransitionGroup v-else name="grid-sort" tag="div" class="list">
      <article
        v-for="node in visible"
        :key="node.is_master ? 'master' : node.id"
        :data-edge-sort-id="node.is_master ? undefined : node.id"
        class="card"
        :class="{
          muted: !node.enabled,
          master: node.is_master,
          dragging: !node.is_master && pointerSort.draggingScope.value === 'edge-nodes' && pointerSort.draggingKey.value === node.id,
          'is-sort-placeholder': !node.is_master && pointerSort.draggingScope.value === 'edge-nodes' && pointerSort.draggingKey.value === node.id,
        }"
      >
        <div class="card-main">
          <div class="title-row">
            <button
              v-if="!node.is_master && canSortEdges"
              type="button"
              class="drag-handle"
              title="按住拖动调整顺序"
              aria-label="拖动调整顺序"
              @click.stop
              @pointerdown.stop="startEdgeSort($event, node)"
            >
              <GripVertical />
            </button>
            <span class="status-pill" :class="STATUS_TONE[node.status]">
              <span class="dot" />{{ STATUS_LABEL[node.status] }}
            </span>
            <h3>{{ node.name }}</h3>
            <span v-if="node.is_master" class="tag master-tag">主机</span>
            <span v-if="!node.is_master && node.any_circuit_tripped" class="tag bad">疑似阻断 · 已熔断</span>
            <span v-if="node.agent_version" class="tag">{{ node.agent_version }}</span>
            <span v-if="!node.is_master && node.public_host && node.tls_active" class="tag good">HTTPS 自动证书</span>
            <span v-else-if="!node.is_master && node.public_host && node.tls_mode === 'auto' && node.online" class="tag warn">等待同步证书</span>
            <span v-else-if="!node.is_master && node.public_host && node.tls_mode === 'external'" class="tag">外部 HTTPS</span>
            <span v-if="!node.is_master && node.auto_https && node.cf_dns_status === 'synced'" class="tag good" :title="`Cloudflare DNS 已解析至 ${node.cf_record_ip || node.detected_ip}`">CF DNS</span>
            <span v-else-if="!node.is_master && node.auto_https && node.cf_dns_status === 'pending_ip'" class="tag warn">等待写入 CF DNS</span>
            <span v-if="!node.is_master && node.listen_port && node.listen_port !== node.public_port" class="tag">
              NAT {{ node.listen_port }} → {{ node.public_port }}
            </span>
          </div>

          <div class="addr-row">
            <template v-if="node.is_master">
              <span v-if="node.public_url" class="mono">{{ node.public_url }}</span>
              <span class="faint" :class="{ sep: !!node.public_url }">本机运行</span>
              <span v-if="node.memory_bytes" class="faint sep">内存 {{ formatBytes(node.memory_bytes) }}</span>
              <span v-if="node.uptime_sec" class="faint sep">运行 {{ formatUptime(node.uptime_sec) }}</span>
            </template>
            <template v-else>
              <span v-if="node.public_url" class="mono">{{ node.public_url }}</span>
              <span v-else class="faint">等待从机首次心跳自动上报公网 IP（端口 {{ node.public_port }}）</span>
              <span v-if="node.last_seen_at" class="faint sep">最近心跳 {{ relativeTime(node.last_seen_at) }}</span>
              <span v-if="node.online && node.memory_bytes" class="faint sep">内存 {{ formatBytes(node.memory_bytes) }}</span>
              <span v-if="node.online && node.uptime_sec" class="faint sep">运行 {{ formatUptime(node.uptime_sec) }}</span>
            </template>
          </div>

          <div v-if="node.online && node.upstream_latencies?.length" class="latency-row">
            <span class="faint lat-label">回源延迟</span>
            <span v-for="lat in node.upstream_latencies" :key="lat.site_name" class="lat-pill" :class="latencyTone(lat.latency_ms)">
              {{ lat.site_name }} {{ lat.latency_ms >= 0 ? `${lat.latency_ms}ms` : '不通' }}
            </span>
          </div>

          <div v-if="node.speed_metrics?.length" class="latency-row">
            <span class="faint lat-label">单线程回程</span>
            <span
              v-for="sm in node.speed_metrics"
              :key="sm.isp_group"
              class="lat-pill"
              :class="speedPillTone(sm.avg_return_mbps)"
              :title="`去程 ${sm.avg_outbound_mbps.toFixed(1)}Mbps · 延迟 ${sm.avg_return_rtt_ms >= 0 ? sm.avg_return_rtt_ms + 'ms' : '--'}${sm.total_retrans >= 0 ? ' · 重传 ' + sm.total_retrans : ''}`"
            >
              {{ sm.isp_label }} {{ sm.avg_return_mbps.toFixed(1) }}Mbps
            </span>
          </div>

          <div v-if="node.online && !node.is_master && node.probe_mode === 'off'" class="latency-row">
            <span class="faint lat-label">三网连通</span>
            <span class="lat-pill muted">已关闭主动探测</span>
          </div>

          <div v-else-if="node.online && node.isp_health?.length" class="latency-row">
            <span class="faint lat-label">三网连通{{ !node.is_master && node.probe_mode === 'icmp' ? ' · ICMP' : '' }}{{ !node.is_master && node.auto_circuit_breaker === false ? ' · 仅探测' : '' }}</span>
            <span
              v-for="ih in node.isp_health"
              :key="ih.isp_group"
              class="isp-pill-wrap"
              tabindex="0"
            >
              <span class="lat-pill" :class="ispHealthTone(ih)">
                {{ ispHealthText(ih) }}
              </span>
              <div class="isp-popover" role="tooltip">
                <template v-if="ih.targets?.length">
                  <div v-for="t in ih.targets" :key="t.target" class="isp-pop-row">
                    <span class="isp-pop-city">{{ t.label || t.target }}</span>
                    <span class="isp-pop-ms" :class="t.reachable && t.latency_ms > 0 ? latencyTone(t.latency_ms) : 'bad'">
                      {{ t.reachable && t.latency_ms > 0 ? `${t.latency_ms}ms` : '不通' }}
                    </span>
                  </div>
                </template>
                <div v-else class="isp-pop-row">
                  <span class="isp-pop-city">{{ ih.isp_label }}探活</span>
                  <span class="isp-pop-ms">{{ ih.reachable_count }}/{{ ih.total_count }} 通</span>
                </div>
                <div v-if="ih.tripped" class="isp-pop-foot bad">已自动熔断切流</div>
              </div>
            </span>
          </div>

          <p v-if="!node.is_master && node.auto_https && node.cf_dns_status?.startsWith('error:')" class="upgrade-note bad">
            Cloudflare DNS 同步失败：{{ node.cf_dns_status.slice(6) }}
          </p>

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
          <template v-if="node.is_master">
            <UiButton size="sm" title="主机三网单线程测速" @click="openSpeedtest(0)">
              <Activity />测速
            </UiButton>
            <UiButton size="sm" :loading="probingMaster" title="立即探测主机到各站点的回源延迟" @click="probeMaster">
              <RefreshCw />刷新延迟
            </UiButton>
            <UiButton size="sm" @click="openMasterEdit"><Pencil />编辑</UiButton>
            <UiButton size="sm" variant="ghost" title="清零本期流量" @click="resetMasterTraffic">
              <RotateCcw />
            </UiButton>
          </template>
          <template v-else>
            <UiButton
              v-if="node.online && node.probe_mode !== 'off' && (node.any_circuit_tripped || (node.auto_circuit_breaker === false && node.isp_health?.some(h => h.reachable_count === 0)))"
              size="sm"
              variant="accent"
              :loading="probingIspNodeId === node.id"
              :title="node.probe_mode === 'icmp' ? '立即重新探测北上广三网 ICMP Ping' : '立即重新探测北上广三网 TCP 连通性'"
              @click="probeNodeISP(node)"
            >
              <RefreshCw />立即复测
            </UiButton>
            <UiButton
              v-if="node.any_circuit_tripped"
              size="sm"
              variant="secondary"
              :loading="resettingCircuitNodeId === node.id"
              title="手动解除熔断并重新参与分流"
              @click="resetNodeCircuit(node)"
            >
              解除熔断
            </UiButton>
            <UiButton
              v-if="isNodeUpgradeable(node)"
              size="sm"
              variant="primary"
              :loading="upgradingId === node.id"
              :disabled="upgradingAll"
              @click="upgradeNode(node)"
            >
              <ArrowUpCircle />升级 {{ node.master_version }}
            </UiButton>
            <UiButton size="sm" :disabled="!node.online" :title="node.online ? '三网单线程测速' : '从机离线'" @click="openSpeedtest(node.id)">
              <Activity />测速
            </UiButton>
            <UiButton size="sm" :variant="isNodeUpgradeable(node) ? 'secondary' : 'primary'" @click="showInstallCommand(node)">
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
          </template>
        </div>
      </article>

      <article v-if="showAddEdgeHintCard" key="add-hint" class="card add-hint">
        <h3>添加从机节点</h3>
        <p class="faint">
          当前仅主机在提供服务。添加普通 VPS 或 NAT 小鸡作为轻量中转节点（约 15–25 MB 内存），视频播放可按三网运营商自动就近卸载。
        </p>
        <UiButton variant="primary" @click="openCreate"><Plus />添加第一个从机</UiButton>
      </article>
    </TransitionGroup>

    <UiDrawer
      :open="masterFormOpen"
      title="编辑主机"
      :subtitle="masterNode?.name || '主机'"
      :width="480"
      :dismissible="!savingMaster"
      @close="masterFormOpen = false"
    >
      <form class="form" novalidate @submit.prevent="saveMasterForm">
        <fieldset>
          <legend>基础信息</legend>
          <UiField
            label="节点名称"
            for="mn-name"
            :error="masterErrors.name"
            help="修改后将同步显示在节点、线路、测速、仪表盘与分享账号页面。"
          >
            <input id="mn-name" v-model="masterForm.name" class="input" maxlength="64" autofocus placeholder="例如：美国主机、香港主机" />
          </UiField>
        </fieldset>

        <fieldset>
          <legend>流量统计与告警</legend>
          <div class="grid">
            <UiField
              label="流量额度"
              for="mn-quota"
              :error="masterErrors.quota_gb"
              help="GB，超额触发告警并优先分流至在线从机；面板不受限制。"
            >
              <input id="mn-quota" v-model="masterForm.quota_gb" class="input num" type="number" min="0" step="1" inputmode="decimal" placeholder="不限" />
            </UiField>
            <UiField label="每月重置" for="mn-reset" help="到这天清零主机本期流量">
              <select id="mn-reset" v-model.number="masterForm.traffic_reset_day" class="select">
                <option :value="0">不重置</option>
                <option v-for="day in 28" :key="day" :value="day">每月 {{ day }} 日</option>
              </select>
            </UiField>
          </div>

          <UiField
            label="负载均衡承载流数（路）"
            for="mn-balance-cap"
            :error="masterErrors.max_balance_streams"
            help="线路设为「负载均衡」时，该节点优先承接的观看设备数（1–99，默认 2）；满额后顺位溢出到下一台节点。"
          >
            <input id="mn-balance-cap" v-model="masterForm.max_balance_streams" class="input num" type="number" min="1" max="99" step="1" inputmode="numeric" />
          </UiField>

          <UiField label="备注" for="mn-note">
            <input id="mn-note" v-model="masterForm.note" class="input" maxlength="200" placeholder="可选，例如机房位置、带宽规格" />
          </UiField>
        </fieldset>

        <button type="submit" hidden />
      </form>
      <template #footer>
        <UiButton :disabled="savingMaster" @click="masterFormOpen = false">取消</UiButton>
        <UiButton variant="primary" :loading="savingMaster" @click="saveMasterForm">保存更改</UiButton>
      </template>
    </UiDrawer>

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
          <div class="auto-https-box">
            <UiSwitch
              v-model="form.auto_https"
              :disabled="!autoHttpsReady && !form.auto_https"
              label="自动配置 HTTPS（Cloudflare DNS + 泛域名证书）"
              :description="
                autoHttpsReady
                  ? `上线自动分配 ${edgeAutoHttps?.subdomain_prefix || 'node'}-<ID>.${edgeAutoHttps?.base_domain} 并写入 CF 仅 DNS 解析、自动下发证书（可绕过 NAT 明文 HTTP 阻断）。`
                  : '在「设置 → 从机自动 HTTPS」填一次 Cloudflare API Token 后即可一键开启，完全不影响主机反代。'
              "
            />
            <p v-if="autoHttpsReady && (!edgeAutoHttps?.cert_valid || !edgeAutoHttps?.cert_matches_domain)" class="auto-https-hint warn">
              提示：泛域名证书尚未申请，请先前往 <RouterLink to="/settings?section=edge-https">设置 → 从机自动 HTTPS</RouterLink> 点击「一键申请泛域名证书」。
            </p>
            <p v-else-if="!autoHttpsReady" class="auto-https-hint">
              <RouterLink to="/settings?section=edge-https">前往配置 Cloudflare 从机自动 HTTPS →</RouterLink>
            </p>
          </div>
          <UiField
            label="三网探测方式"
            for="en-probe-mode"
            help="RFC 等屏蔽出站 TCP 的优化线路机建议选 ICMP Ping；不需主动探活的机房可关闭。"
          >
            <select id="en-probe-mode" v-model="form.probe_mode" class="select">
              <option value="tcp">TCP 握手（默认 · 精准检测端口阻断）</option>
              <option value="icmp">ICMP Ping（适合屏蔽出站 TCP 的优化线路机）</option>
              <option value="off">关闭主动探测（仅按在线心跳、配额与时段分流）</option>
            </select>
          </UiField>
          <UiSwitch
            v-if="form.probe_mode !== 'off'"
            v-model="form.auto_circuit_breaker"
            label="自动熔断"
            description="关闭后继续记录三网探测结果，节点仍按在线状态、配额和时段参与分流。"
          />
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
              :help="
                form.auto_https
                  ? `已开启自动 HTTPS：留空自动分配 ${edgeAutoHttps?.subdomain_prefix || 'node'}-<ID>.${edgeAutoHttps?.base_domain}；也可填写自定义一级子域名（如 usnat.${edgeAutoHttps?.base_domain}）。`
                  : '留空则首次心跳自动上报公网 IP（http://IP:端口）。若填主机泛域名下的子域名（如 hk.example.com），可自动同步泛域名证书实现 HTTPS。'
              "
            >
              <input id="en-host" v-model="form.public_host" class="input mono" autocapitalize="none" spellcheck="false" :placeholder="form.auto_https ? `留空自动分配 *.${edgeAutoHttps?.base_domain || 'example.com'}` : '可选，例如 hk.example.com'" />
            </UiField>

            <UiField v-if="!form.auto_https && form.public_host.trim()" label="TLS 模式" for="en-tls">
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

            <UiField
              label="负载均衡承载流数（路）"
              for="en-balance-cap"
              :error="errors.max_balance_streams"
              help="线路设为「负载均衡」时，该从机优先承接的观看设备数（1–99，默认 2）；满额后顺位溢出到下一台节点。"
            >
              <input id="en-balance-cap" v-model="form.max_balance_streams" class="input num" type="number" min="1" max="99" step="1" inputmode="numeric" />
            </UiField>

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

    <UiDialog :open="installOpen" :title="`部署从机 · ${installNode?.name || ''}`" :width="620" @close="installOpen = false">
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

        <div class="uninstall-section">
          <div class="uninstall-head">
            <strong>卸载该从机</strong>
            <span class="faint">如需停用并彻底清理从机服务与配置，在从机服务器执行：</span>
          </div>
          <pre class="cmd cmd-uninstall"><code>{{ uninstallCommand }}</code></pre>
        </div>
      </div>
      <template #footer>
        <UiButton @click="installOpen = false">关闭</UiButton>
        <UiButton variant="secondary" @click="copyUninstallCommand"><Copy />复制卸载命令</UiButton>
        <UiButton variant="primary" @click="copyInstallCommand"><Copy />复制安装命令</UiButton>
      </template>
    </UiDialog>

    <SpeedtestDrawer :open="speedtestOpen" :initial-node-id="speedtestNodeId" @close="speedtestOpen = false" @completed="load()" />
    <IspProbeDrawer :open="ispProbeOpen" @close="ispProbeOpen = false" @saved="load(true)" />
  </div>
</template>

<style scoped>
.kpi-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin: 0 0 18px; }
.kpi-card { padding: 16px 20px; border-radius: var(--radius-lg); background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow-card); display: flex; flex-direction: column; gap: 6px; }
.kpi-card dt { display: inline-flex; align-items: center; gap: 7px; font-size: 12.5px; font-weight: 500; color: var(--text-2); }
.kpi-card dd { margin: 0; display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; font-size: 24px; font-weight: 700; letter-spacing: -0.025em; }
.kpi-sub { font-size: 12px; font-weight: 500; color: var(--text-3); letter-spacing: 0; }
.toolbar-card { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 16px; padding: 10px 14px; border-radius: var(--radius); background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow-card); }
.seg { display: inline-flex; align-items: center; gap: 2px; padding: 3px; border-radius: 999px; background: var(--surface-2); border: 1px solid var(--border); overflow-x: auto; scrollbar-width: none; }
.seg::-webkit-scrollbar { display: none; }
.seg button { display: inline-flex; align-items: center; gap: 6px; height: 30px; padding: 0 13px; border: 0; border-radius: 999px; background: transparent; color: var(--text-2); font-size: 13px; font-weight: 500; white-space: nowrap; cursor: pointer; transition: all var(--dur) var(--ease); }
.seg button:hover { color: var(--text); }
.seg button.on { background: var(--surface); color: var(--text); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08); }
.n { font-size: 11.5px; color: var(--text-3); }
.search { display: flex; align-items: center; gap: 8px; width: 240px; height: 34px; padding: 0 12px; border-radius: 999px; background: var(--surface-2); border: 1px solid var(--border); color: var(--text-3); }
.search:focus-within { background: var(--surface); border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.search svg { width: 14px; height: 14px; flex: 0 0 auto; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13px; color: var(--text); }
.search input::placeholder { color: var(--text-3); }
.list { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.card { display: flex; flex-direction: column; gap: 14px; padding: 20px 22px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow-card); transition: border-color var(--dur) var(--ease); }
.card:hover { border-color: var(--border-strong); }
.card.master { border-color: color-mix(in srgb, var(--accent) 32%, var(--border)); }
.card.master:hover { border-color: color-mix(in srgb, var(--accent) 52%, var(--border)); }
.card.add-hint { justify-content: center; align-items: flex-start; border-style: dashed; background: var(--surface-2); box-shadow: none; }
.card.add-hint h3 { margin: 0; font-size: 15.5px; font-weight: 700; }
.card.add-hint p { margin: 0; font-size: 13px; line-height: 1.6; max-width: 420px; }
.card.muted { opacity: 0.68; }
.card.dragging { opacity: 0.24; border-style: dashed; border-color: var(--accent); background: var(--accent-soft); box-shadow: none; }
.drag-handle {
  display: inline-grid;
  place-items: center;
  width: 24px;
  height: 24px;
  margin-left: -6px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--text-3);
  cursor: grab;
  touch-action: none;
  flex: 0 0 auto;
  transition: color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.drag-handle:hover { color: var(--text); background: var(--surface-2); }
.drag-handle:active { cursor: grabbing; }
.drag-handle svg { width: 15px; height: 15px; }
.grid-sort-move { transition: transform 180ms var(--ease); }
.card.skeleton { height: 180px; }
.card.skeleton span { display: block; width: 45%; height: 14px; border-radius: 6px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }
.title-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.title-row h3 { margin: 0; font-size: 16px; font-weight: 700; letter-spacing: -0.015em; }
.status-pill { display: inline-flex; align-items: center; gap: 6px; height: 23px; padding: 0 9px; border-radius: 999px; font-size: 11.5px; font-weight: 600; background: var(--surface-2); color: var(--text-2); }
.status-pill .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status-pill.good { color: var(--good); background: var(--good-soft); }
.status-pill.warn { color: var(--warn); background: var(--warn-soft); }
.status-pill.bad { color: var(--bad); background: var(--bad-soft); }
.status-pill.muted { color: var(--text-3); }
.tag { display: inline-flex; align-items: center; height: 21px; padding: 0 8px; border-radius: 999px; font-size: 11px; background: var(--surface-2); color: var(--text-2); font-family: var(--mono); }
.tag.good { color: var(--good); background: var(--good-soft); font-family: inherit; font-weight: 600; }
.tag.warn { color: var(--warn); background: var(--warn-soft); font-family: inherit; font-weight: 600; }
.tag.bad { color: var(--bad); background: var(--bad-soft); font-family: inherit; font-weight: 600; }
.tag.master-tag { color: var(--accent); background: var(--accent-soft); font-family: inherit; font-weight: 600; }
.auto-https-box { padding: 12px 14px; border-radius: var(--radius-sm); background: var(--surface-2); border: 1px solid var(--border); display: flex; flex-direction: column; gap: 8px; }
.auto-https-hint { margin: 0; font-size: 12px; color: var(--text-2); line-height: 1.5; }
.auto-https-hint.warn { color: var(--warn); }
.auto-https-hint a { color: var(--accent); text-decoration: none; font-weight: 500; }
.auto-https-hint a:hover { text-decoration: underline; }
.addr-row { display: flex; align-items: center; gap: 8px; margin-top: 8px; font-size: 12px; flex-wrap: wrap; }
.mono { font-family: var(--mono); color: var(--text-2); word-break: break-all; }
.faint { color: var(--text-3); }
.sep::before { content: '·'; margin-right: 8px; }
.latency-row { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-top: 8px; font-size: 11.5px; }
.lat-label { margin-right: 2px; }
.lat-pill { display: inline-flex; align-items: center; height: 22px; padding: 0 8px; border-radius: 999px; font-family: var(--mono); font-size: 11px; font-weight: 500; background: var(--surface-2); color: var(--text-2); }
.lat-pill.good { color: var(--good); background: var(--good-soft); }
.lat-pill.warn { color: var(--warn); background: var(--warn-soft); }
.lat-pill.bad { color: var(--bad); background: var(--bad-soft); }
.lat-pill.muted { color: var(--text-3); background: var(--surface-2); font-family: inherit; }
.isp-pill-wrap { position: relative; display: inline-flex; outline: none; cursor: default; }
.isp-popover {
  position: absolute;
  left: 50%;
  bottom: calc(100% + 7px);
  transform: translateX(-50%) translateY(3px);
  min-width: 118px;
  padding: 7px 10px;
  border-radius: var(--radius-sm);
  background: var(--surface);
  border: 1px solid var(--border-strong);
  box-shadow: 0 8px 22px rgba(15, 23, 42, 0.16);
  display: flex;
  flex-direction: column;
  gap: 4px;
  z-index: 40;
  opacity: 0;
  pointer-events: none;
  transition: opacity 140ms var(--ease), transform 140ms var(--ease);
}
.isp-pill-wrap:hover .isp-popover,
.isp-pill-wrap:focus-within .isp-popover {
  opacity: 1;
  transform: translateX(-50%) translateY(0);
}
.isp-pop-row { display: flex; align-items: center; justify-content: space-between; gap: 14px; font-size: 12px; line-height: 1.35; white-space: nowrap; }
.isp-pop-city { color: var(--text-2); font-weight: 500; }
.isp-pop-ms { font-family: var(--mono); font-weight: 600; color: var(--text); }
.isp-pop-ms.good { color: var(--good); }
.isp-pop-ms.warn { color: var(--warn); }
.isp-pop-ms.bad { color: var(--bad); }
.isp-pop-foot { margin-top: 2px; padding-top: 4px; border-top: 1px solid var(--border); font-size: 11px; font-weight: 600; text-align: center; white-space: nowrap; }
.isp-pop-foot.bad { color: var(--bad); }
.upgrade-note { margin: 8px 0 0; font-size: 12px; }
.upgrade-note.warn { color: var(--warn); }
.upgrade-note.good { color: var(--good); }
.upgrade-note.bad { color: var(--bad); }
.note { margin: 8px 0 0; font-size: 12.5px; color: var(--text-3); }
.card-metrics { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px; }
.metric { display: flex; flex-direction: column; gap: 4px; padding: 10px 12px; border-radius: var(--radius-sm); background: var(--surface-2); }
.metric .label { font-size: 11px; color: var(--text-3); }
.metric .val { font-size: 13px; font-weight: 600; }
.card-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; padding-top: 12px; border-top: 1px solid var(--border); margin-top: auto; }
.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 64px 20px; border-radius: var(--radius-lg); background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow-card); text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 700; }
.empty p { max-width: 420px; margin: 0 0 8px; color: var(--text-2); line-height: 1.6; }
.form { display: flex; flex-direction: column; gap: 4px; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 18px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
fieldset:first-of-type { padding-top: 0; }
fieldset:last-of-type { border-bottom: 0; }
legend { float: left; width: 100%; margin-bottom: 14px; padding: 0; font-size: 12.5px; font-weight: 600; color: var(--text-2); }
legend + * { clear: both; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }
.adv-toggle { display: flex; align-items: center; gap: 8px; width: 100%; padding: 0; border: 0; background: transparent; cursor: pointer; font-size: 13px; font-weight: 600; color: var(--text); text-align: left; }
.adv-toggle small { margin-left: auto; font-size: 12px; font-weight: 400; }
.adv-toggle .chev { width: 15px; height: 15px; color: var(--text-3); transition: transform 180ms var(--ease); flex: 0 0 auto; }
.adv-toggle.open .chev { transform: rotate(180deg); }
.install-box p { margin: 0 0 12px; font-size: 13.5px; }
.cmd { margin: 0 0 14px; padding: 14px; border-radius: var(--radius-sm); background: var(--surface-2); border: 1px solid var(--border); overflow-x: auto; font-family: var(--mono); font-size: 12.5px; line-height: 1.55; white-space: pre-wrap; word-break: break-all; }
.uninstall-section { margin-top: 18px; padding-top: 16px; border-top: 1px dashed var(--border-strong); }
.uninstall-head { display: flex; flex-direction: column; gap: 3px; margin-bottom: 10px; font-size: 12.5px; }
.uninstall-head strong { font-size: 13px; color: var(--text); }
.cmd-uninstall { margin-bottom: 0; }
.tips { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 6px; font-size: 12.5px; color: var(--text-2); line-height: 1.55; }
.tips code { font-family: var(--mono); font-size: 12px; padding: 1px 6px; border-radius: 5px; background: var(--surface-2); }
@media (max-width: 960px) { .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .list { grid-template-columns: 1fr; } }
@media (max-width: 640px) { .search { width: 100%; } .card-metrics { grid-template-columns: 1fr; } }
@media (max-width: 520px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
