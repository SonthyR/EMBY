<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { Activity, Gauge, Plus, Square, Trash2, Wifi } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiField from '@/components/ui/UiField.vue'
import {
  speedtestApi,
  type SpeedtestCustomTarget,
  type SpeedtestCustomTargetInput,
  type SpeedtestDirectTarget,
  type SpeedtestISP,
  type SpeedtestJob,
  type SpeedtestNodeSummary,
  type SpeedtestProvince,
  type SpeedtestTargetResult,
} from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { formatBytes } from '@/utils/format'

const props = withDefaults(
  defineProps<{
    open: boolean
    initialNodeId?: number
    initialTab?: 'node' | 'direct'
  }>(),
  {
    initialNodeId: 0,
    initialTab: 'node',
  },
)

const emit = defineEmits<{
  close: []
  completed: [job: SpeedtestJob]
}>()

const activeTab = ref<'node' | 'direct'>(props.initialTab)
const loadingTargets = ref(false)
const provinces = ref<SpeedtestProvince[]>([])
const isps = ref<SpeedtestISP[]>([])
const nodes = ref<SpeedtestNodeSummary[]>([])
const customTargets = ref<SpeedtestCustomTarget[]>([])

const selectedNodeId = ref<number>(props.initialNodeId)
type NodeSpeedtestMode = 'builtin' | 'apple' | 'custom' | 'all'

const targetMode = ref<'builtin' | 'apple'>('builtin')
const includeCustom = ref(false)
const selectedProvinces = ref<string[]>(['bj', 'sh', 'gd'])
const selectedISPs = ref<string[]>(['ct', 'cu', 'cm'])
const selectedCustomIds = ref<number[]>([])
const catalogCount = ref<number>(0)

const showCustomPanel = ref(false)
const savingCustom = ref(false)
const newCustom = ref<SpeedtestCustomTargetInput>({
  name: '',
  isp_group: 'ct',
  province: '',
  upload_url: '',
  download_url: '',
  method: 'POST',
  enabled: true,
})

const startingMode = ref<NodeSpeedtestMode | null>(null)
const starting = computed(() => startingMode.value !== null)
const canceling = ref(false)
const activeJob = ref<SpeedtestJob | null>(null)
let pollTimer: number | null = null
let pollGen = 0

interface DirectRowState {
  target: SpeedtestDirectTarget
  status: 'idle' | 'running' | 'done' | 'blocked' | 'error'
  mbps: number
  ttfbMs: number
  receivedBytes: number
  note: string
}

const directLoading = ref(false)
const directRunning = ref(false)
const directRows = ref<DirectRowState[]>([])
let directAbort: AbortController | null = null
let directRunId = 0

const selectedNode = computed(() => nodes.value.find(n => n.id === selectedNodeId.value) || null)
const isJobRunning = computed(() => activeJob.value?.status === 'running' || activeJob.value?.status === 'pending')
const jobBusy = computed(() => starting.value || isJobRunning.value)
const canStart = computed(() => !loadingTargets.value && selectedNode.value?.online !== false)
const hasEnabledCustomTargets = computed(() => customTargets.value.some(target => target.enabled))
const primaryMode = computed<NodeSpeedtestMode>(() =>
  targetMode.value === 'builtin' && includeCustom.value && hasEnabledCustomTargets.value ? 'all' : targetMode.value,
)
const progressPct = computed(() => {
  const job = activeJob.value
  if (!job || !job.total_targets) return 0
  return Math.min(100, Math.round((job.completed_targets / job.total_targets) * 100))
})

function stopPolling() {
  pollGen++
  if (pollTimer !== null) {
    window.clearTimeout(pollTimer)
    pollTimer = null
  }
}

function stopDirectTest() {
  directRunId++
  if (directAbort) {
    directAbort.abort('stopped')
    directAbort = null
  }
  directRunning.value = false
}

function needsHTTPPopup(target: SpeedtestDirectTarget): boolean {
  return window.location.protocol === 'https:' && target.scheme === 'http' && !target.is_master
}

async function loadTargets() {
  loadingTargets.value = true
  try {
    const res = await speedtestApi.targets()
    provinces.value = res.provinces || []
    isps.value = res.isps || []
    nodes.value = res.nodes || []
    customTargets.value = res.custom_targets || []
    const enabledCustomIds = new Set(customTargets.value.filter(target => target.enabled).map(target => target.id))
    selectedCustomIds.value = selectedCustomIds.value.filter(id => enabledCustomIds.has(id))
    if (!enabledCustomIds.size) includeCustom.value = false
    catalogCount.value = res.catalog_count || 0
    const validProvCodes = new Set(provinces.value.map(p => p.code))
    if (validProvCodes.size) {
      selectedProvinces.value = selectedProvinces.value.filter(code => validProvCodes.has(code))
    }
    if (!selectedProvinces.value.length && res.default_provinces?.length) {
      selectedProvinces.value = [...res.default_provinces]
    }
    if (!selectedISPs.value.length && res.default_isps?.length) {
      selectedISPs.value = [...res.default_isps]
    }
    if (nodes.value.length && !nodes.value.some(n => n.id === selectedNodeId.value)) {
      selectedNodeId.value = nodes.value[0].id
    }
  } catch (err) {
    toast.error(errorMessage(err, '加载测速配置失败'))
  } finally {
    loadingTargets.value = false
  }
}

async function loadLatestJobForNode(nodeId: number) {
  stopPolling()
  const gen = pollGen
  try {
    const res = await speedtestApi.jobs(nodeId)
    if (gen !== pollGen || !props.open || selectedNodeId.value !== nodeId) return
    const latest = res.jobs?.[0] || null
    activeJob.value = latest
    if (latest && (latest.status === 'running' || latest.status === 'pending')) {
      startPolling(latest.id)
    }
  } catch {
    if (gen === pollGen) {
      activeJob.value = null
    }
  }
}

let directLoadGen = 0

async function loadDirectTargets(expectedRunId?: number) {
  const gen = ++directLoadGen
  directLoading.value = true
  try {
    const res = await speedtestApi.directTargets()
    if (gen !== directLoadGen || (expectedRunId !== undefined && expectedRunId !== directRunId)) return
    directRows.value = (res.targets || []).map(target => {
      if (!target.online) {
        return {
          target,
          status: 'blocked',
          mbps: 0,
          ttfbMs: -1,
          receivedBytes: 0,
          note: target.unavailable_reason || '节点离线或未配置访问地址',
        }
      }
      if (needsHTTPPopup(target)) {
        if (!target.supports_popup || !target.popup_url || !target.report_id) {
          return {
            target,
            status: 'blocked',
            mbps: 0,
            ttfbMs: -1,
            receivedBytes: 0,
            note: target.unavailable_reason || '需将从机升级至最新版，即可免配 HTTPS 直接测速',
          }
        }
        return {
          target,
          status: 'idle',
          mbps: 0,
          ttfbMs: -1,
          receivedBytes: 0,
          note: 'HTTP 从机 · 通过同源小窗直测后安全回传（无需 HTTPS）',
        }
      }
      return {
        target,
        status: 'idle',
        mbps: 0,
        ttfbMs: -1,
        receivedBytes: 0,
        note: '',
      }
    })
  } catch (err) {
    toast.error(errorMessage(err, '加载直测节点失败'))
  } finally {
    directLoading.value = false
  }
}

function startPolling(jobId: string) {
  stopPolling()
  const gen = pollGen
  let consecutiveErrs = 0

  const tick = async () => {
    if (gen !== pollGen || !props.open || activeTab.value !== 'node') return
    try {
      const res = await speedtestApi.job(jobId)
      if (gen !== pollGen || !props.open || activeTab.value !== 'node') return
      consecutiveErrs = 0
      const job = res.job
      activeJob.value = job
      if (job.status !== 'running' && job.status !== 'pending') {
        stopPolling()
        await loadTargets()
        if (props.open) emit('completed', job)
        return
      }
    } catch {
      if (gen !== pollGen || !props.open) return
      consecutiveErrs++
      if (consecutiveErrs >= 3) {
        stopPolling()
        return
      }
    }
    if (gen === pollGen && props.open && activeTab.value === 'node') {
      pollTimer = window.setTimeout(tick, 1000)
    }
  }

  pollTimer = window.setTimeout(tick, 1000)
}

function toggleProvince(code: string) {
  const idx = selectedProvinces.value.indexOf(code)
  if (idx >= 0) {
    if (selectedProvinces.value.length > 1) selectedProvinces.value.splice(idx, 1)
  } else {
    selectedProvinces.value.push(code)
  }
}

function toggleISP(group: string) {
  const idx = selectedISPs.value.indexOf(group)
  if (idx >= 0) {
    if (selectedISPs.value.length > 1) selectedISPs.value.splice(idx, 1)
  } else {
    selectedISPs.value.push(group)
  }
}

function selectPreset() {
  selectedProvinces.value = provinces.value.length ? provinces.value.map(p => p.code) : ['bj', 'sh', 'gd']
  selectedISPs.value = ['ct', 'cu', 'cm']
}

function toggleCustomSelection(targetId: number) {
  const idx = selectedCustomIds.value.indexOf(targetId)
  if (idx >= 0) {
    selectedCustomIds.value.splice(idx, 1)
  } else {
    selectedCustomIds.value.push(targetId)
  }
}

async function addCustomTarget() {
  if (!newCustom.value.name.trim()) {
    toast.error('请填写自定义目标名称')
    return
  }
  if (!newCustom.value.upload_url?.trim() && !newCustom.value.download_url?.trim()) {
    toast.error('回程地址或去程地址至少填写一项')
    return
  }
  savingCustom.value = true
  try {
    await speedtestApi.createCustom({
      ...newCustom.value,
      name: newCustom.value.name.trim(),
      province: newCustom.value.province?.trim() || '',
      upload_url: newCustom.value.upload_url?.trim() || '',
      download_url: newCustom.value.download_url?.trim() || '',
    })
    toast.success('已添加自定义测速目标')
    newCustom.value = {
      name: '',
      isp_group: 'ct',
      province: '',
      upload_url: '',
      download_url: '',
      method: 'POST',
      enabled: true,
    }
    await loadTargets()
  } catch (err) {
    toast.error(errorMessage(err, '创建自定义测速目标失败'))
  } finally {
    savingCustom.value = false
  }
}

async function deleteCustomTarget(target: SpeedtestCustomTarget) {
  if (!(await confirm({ title: '删除测速目标', message: `确定删除「${target.name}」吗？`, confirmText: '删除', danger: true }))) {
    return
  }
  try {
    await speedtestApi.removeCustom(target.id)
    selectedCustomIds.value = selectedCustomIds.value.filter(id => id !== target.id)
    toast.success('已删除测速目标')
    await loadTargets()
  } catch (err) {
    toast.error(errorMessage(err, '删除失败'))
  }
}

async function startNodeSpeedtest(mode: NodeSpeedtestMode) {
  if (jobBusy.value || loadingTargets.value) return
  if (selectedNode.value && !selectedNode.value.online) {
    toast.error('所选从机当前离线，无法执行测速')
    return
  }
  if (mode === 'custom' && !hasEnabledCustomTargets.value) {
    toast.error('请先添加并启用至少一个自定义测速目标')
    showCustomPanel.value = true
    return
  }
  startingMode.value = mode
  try {
    const res = await speedtestApi.start({
      node_id: selectedNodeId.value,
      mode,
      provinces: selectedProvinces.value,
      isps: selectedISPs.value,
      custom_target_ids: mode === 'all' || mode === 'custom' ? selectedCustomIds.value : [],
    })
    activeJob.value = res.job
    startPolling(res.job.id)
  } catch (err) {
    toast.error(errorMessage(err, '启动测速失败'))
  } finally {
    startingMode.value = null
  }
}

async function cancelNodeSpeedtest() {
  if (!activeJob.value) return
  canceling.value = true
  try {
    await speedtestApi.cancel(activeJob.value.id)
    stopPolling()
    const res = await speedtestApi.job(activeJob.value.id)
    activeJob.value = res.job
    toast.success('测速任务已取消')
  } catch (err) {
    toast.error(errorMessage(err, '取消失败'))
  } finally {
    canceling.value = false
  }
}

async function refreshDirectTargetRow(nodeId: number) {
  try {
    const res = await speedtestApi.directTargets(nodeId)
    const fresh = (res.targets || []).find(t => t.node_id === nodeId)
    if (!fresh) return
    const existing = directRows.value.find(r => r.target.node_id === nodeId)
    if (existing) {
      existing.target = fresh
    }
  } catch {
    /* non-fatal ticket refresh */
  }
}

async function pollDirectPopupReport(row: DirectRowState, expectedRunId: number): Promise<void> {
  const reportId = row.target.report_id
  if (!row.target.popup_url || !reportId) {
    row.status = 'error'
    row.note = '未签发有效的 HTTP 直测会话'
    return
  }

  row.status = 'running'
  row.mbps = 0
  row.ttfbMs = -1
  row.receivedBytes = 0
  row.note = '已打开同源小窗，正在测量并回传...'

  const deadline = performance.now() + 16000
  try {
    while (performance.now() < deadline) {
      if (expectedRunId !== directRunId || !props.open || activeTab.value !== 'direct') {
        row.status = 'idle'
        row.note = '已手动停止'
        return
      }
      await new Promise(r => window.setTimeout(r, 350))
      if (expectedRunId !== directRunId || !props.open || activeTab.value !== 'direct') {
        row.status = 'idle'
        row.note = '已手动停止'
        return
      }
      try {
        const res = await speedtestApi.directReportPoll(reportId)
        const rep = res.report
        if (rep.ttfb_ms >= 0 && Number.isFinite(rep.ttfb_ms)) {
          row.ttfbMs = rep.ttfb_ms
        }
        if (rep.mbps >= 0 && Number.isFinite(rep.mbps)) {
          row.mbps = rep.mbps
        }
        if (rep.received_bytes >= 0 && Number.isFinite(rep.received_bytes)) {
          row.receivedBytes = rep.received_bytes
        }
        if (rep.status === 'running') {
          row.note = '小窗直测中...'
          continue
        }
        if (rep.status === 'done') {
          if (row.receivedBytes <= 0 || row.ttfbMs <= 0) {
            row.status = 'error'
            row.note = '未收到有效测速字节'
          } else {
            row.status = 'done'
            row.note = `已通过同源小窗拉取 ${formatBytes(row.receivedBytes)}`
          }
          return
        }
        if (rep.status === 'error') {
          row.status = 'error'
          row.note = rep.error || 'HTTP 从机小窗直测失败'
          return
        }
      } catch {
        /* retry until deadline */
      }
    }
    row.status = 'error'
    row.note = '等待小窗回传超时，请确认弹窗未被拦截且本机可直连该从机端口'
  } finally {
    void refreshDirectTargetRow(row.target.node_id)
  }
}

async function runSingleDirectPopup(row: DirectRowState) {
  if (directRunning.value || !row.target.online || !row.target.supports_popup || !row.target.popup_url || !row.target.report_id) {
    return
  }
  window.open(row.target.popup_url, '_blank', `width=460,height=320,noopener`)
  const runId = ++directRunId
  directRunning.value = true
  try {
    await pollDirectPopupReport(row, runId)
  } finally {
    if (runId === directRunId) {
      directRunning.value = false
    }
  }
}

async function runDirectTests() {
  if (directRunning.value) {
    stopDirectTest()
    return
  }
  if (!directRows.value.length) {
    await loadDirectTargets()
  }
  const snapshot = [...directRows.value]
  if (!snapshot.length || !props.open || activeTab.value !== 'direct') return

  const eligibleHTTPRows = snapshot.filter(
    r => r.target.online && needsHTTPPopup(r.target) && r.target.supports_popup && r.target.popup_url && r.target.report_id,
  )
  const firstHTTPRow = eligibleHTTPRows[0] || null
  if (firstHTTPRow?.target.popup_url) {
    window.open(firstHTTPRow.target.popup_url, '_blank', 'width=460,height=320,noopener')
  }
  for (const extraRow of eligibleHTTPRows.slice(1)) {
    extraRow.status = 'idle'
    extraRow.note = '请点击右侧「单独直测」开启独立窗口测试（浏览器安全策略每次点击开启一个窗口）'
  }

  const runId = ++directRunId
  const masterCtrl = new AbortController()
  directAbort = masterCtrl
  directRunning.value = true

  try {
    for (const row of snapshot) {
      if (runId !== directRunId || !props.open || activeTab.value !== 'direct') break
      if (row.status === 'blocked') continue

      if (needsHTTPPopup(row.target)) {
        if (firstHTTPRow && row.target.node_id === firstHTTPRow.target.node_id) {
          await pollDirectPopupReport(row, runId)
        }
        continue
      }

      row.status = 'running'
      row.mbps = 0
      row.ttfbMs = -1
      row.receivedBytes = 0
      row.note = '正在连接...'

      const ctrl = new AbortController()
      const onParentAbort = () => ctrl.abort(masterCtrl.signal.reason || 'stopped')
      masterCtrl.signal.addEventListener('abort', onParentAbort, { once: true })
      const maxDurationMs = 5000
      const timeoutId = window.setTimeout(() => ctrl.abort('duration_reached'), maxDurationMs + 2500)

      try {
        const startAt = performance.now()
        const resp = await fetch(row.target.stream_url, {
          method: 'GET',
          cache: 'no-store',
          credentials: row.target.is_master ? 'same-origin' : 'omit',
          signal: ctrl.signal,
        })
        if (!resp.ok) {
          throw new Error(`HTTP ${resp.status}`)
        }
        row.ttfbMs = Math.max(1, Math.round(performance.now() - startAt))
        if (!resp.body) {
          throw new Error('浏览器不支持流式读取')
        }

        const reader = resp.body.getReader()
        const streamStart = performance.now()
        let warmAt = 0
        let warmBytes = 0
        let received = 0
        row.note = '测速中...'

        while (true) {
          if (runId !== directRunId || !props.open || activeTab.value !== 'direct') {
            ctrl.abort('stopped')
            break
          }
          const elapsed = performance.now() - streamStart
          if (elapsed >= maxDurationMs) {
            ctrl.abort('duration_reached')
            break
          }
          const { done, value } = await reader.read()
          if (done) break
          if (value) {
            received += value.byteLength
            row.receivedBytes = received
            const now = performance.now()
            if (warmAt === 0 && now - streamStart >= 700) {
              warmAt = now
              warmBytes = received
            }
            const measureMs = warmAt > 0 && now - warmAt >= 300 ? now - warmAt : Math.max(10, now - streamStart)
            const measureBytes = warmAt > 0 && now - warmAt >= 300 ? received - warmBytes : received
            row.mbps = Math.round(((measureBytes * 8) / (measureMs * 1000)) * 10) / 10
          }
        }
        if (received <= 0 || row.ttfbMs <= 0) {
          throw new Error('未收到测速数据字节')
        }
        const totalElapsed = Math.max(10, performance.now() - (warmAt > 0 && performance.now() - warmAt >= 400 ? warmAt : streamStart))
        const countedBytes = warmAt > 0 && performance.now() - warmAt >= 400 ? received - warmBytes : received
        row.mbps = Math.round(((countedBytes * 8) / (totalElapsed * 1000)) * 10) / 10
        row.status = 'done'
        row.note = `已拉取 ${formatBytes(received)}`
      } catch (err) {
        if (runId !== directRunId || masterCtrl.signal.aborted) {
          row.status = 'idle'
          row.note = '已手动停止'
        } else if (
          row.receivedBytes > 0 &&
          row.ttfbMs > 0 &&
          (ctrl.signal.reason === 'duration_reached' || String(err).includes('AbortError'))
        ) {
          row.status = 'done'
          row.note = `已拉取 ${formatBytes(row.receivedBytes)}`
        } else {
          row.status = 'error'
          row.note = ctrl.signal.reason === 'duration_reached' ? '连接或读取首字节超时' : errorMessage(err, '连接或 CORS 请求失败')
        }
      } finally {
        masterCtrl.signal.removeEventListener('abort', onParentAbort)
        window.clearTimeout(timeoutId)
      }
    }
  } finally {
    if (runId === directRunId) {
      directAbort = null
      directRunning.value = false
    }
  }
}

function speedTone(mbps: number): string {
  if (mbps <= 0) return 'muted'
  if (mbps >= 150) return 'good'
  if (mbps >= 50) return 'warn'
  return 'bad'
}

function retransTone(retrans: number): string {
  if (retrans < 0) return 'muted'
  if (retrans <= 5) return 'good'
  if (retrans <= 50) return 'warn'
  return 'bad'
}

function rttTone(rtt: number): string {
  if (rtt < 0) return 'muted'
  if (rtt <= 150) return 'good'
  if (rtt <= 220) return 'warn'
  return 'bad'
}

function formatMbpsCell(row: SpeedtestTargetResult, dir: 'return' | 'outbound'): string {
  if (row.status === 'pending') return '--'
  if (row.status === 'running') return '测速中'
  const val = dir === 'return' ? row.return_mbps : row.outbound_mbps
  return val > 0 ? `${val.toFixed(1)}Mbps` : '--'
}

function handleNodeChange() {
  loadLatestJobForNode(selectedNodeId.value)
}

watch(
  () => props.open,
  async open => {
    if (open) {
      activeTab.value = props.initialTab || 'node'
      selectedNodeId.value = props.initialNodeId ?? 0
      await loadTargets()
      if (activeTab.value === 'node') {
        await loadLatestJobForNode(selectedNodeId.value)
      } else {
        await loadDirectTargets()
      }
    } else {
      stopPolling()
      stopDirectTest()
    }
  },
)

watch(activeTab, tab => {
  if (!props.open) return
  if (tab === 'direct') {
    stopPolling()
    loadDirectTargets()
  } else {
    stopDirectTest()
    loadLatestJobForNode(selectedNodeId.value)
  }
})

onBeforeUnmount(() => {
  stopPolling()
  stopDirectTest()
})
</script>

<template>
  <UiDrawer
    :open="open"
    title="三网单线程测速中心"
    subtitle="仅管理员可见 · 真实模拟单连接流媒体吞吐与内核 TCP 重传"
    :width="760"
    :dismissible="!starting"
    @close="emit('close')"
  >
    <div class="st-wrap">
      <nav class="st-tabs" role="tablist">
        <button type="button" role="tab" :aria-selected="activeTab === 'node'" :class="{ on: activeTab === 'node' }" @click="activeTab = 'node'">
          <Activity class="ico" />节点三网测速
        </button>
        <button type="button" role="tab" :aria-selected="activeTab === 'direct'" :class="{ on: activeTab === 'direct' }" @click="activeTab = 'direct'">
          <Wifi class="ico" />本机直测（当前浏览器）
        </button>
      </nav>

      <template v-if="activeTab === 'node'">
        <div class="controls-box">
          <div class="row-grid">
            <UiField label="测试节点" for="st-node">
              <select id="st-node" v-model.number="selectedNodeId" class="select" :disabled="jobBusy || loadingTargets" @change="handleNodeChange">
                <option v-for="n in nodes" :key="n.id" :value="n.id">
                  {{ n.name }}{{ n.is_master ? ' (Master)' : n.online ? ' · 在线' : ' · 离线' }}
                </option>
              </select>
            </UiField>

            <fieldset class="target-field" :disabled="jobBusy || loadingTargets">
              <legend>测哪里</legend>
              <div class="target-options">
                <label>
                  <input v-model="targetMode" type="radio" name="st-target" value="builtin" />
                  <span>国内三网</span>
                </label>
                <label>
                  <input v-model="targetMode" type="radio" name="st-target" value="apple" />
                  <span>Apple CDN</span>
                </label>
              </div>
            </fieldset>
          </div>

          <div v-if="targetMode === 'builtin'" class="picker">
            <div class="picker-head">
              <span class="label">地区与运营商</span>
              <div class="presets">
                <button type="button" class="link-btn" :disabled="jobBusy" @click="selectPreset()">北上广全选（{{ catalogCount || 9 }} 项）</button>
              </div>
            </div>
            <div class="chip-group">
              <button
                v-for="p in provinces"
                :key="p.code"
                type="button"
                class="chip"
                :class="{ on: selectedProvinces.includes(p.code) }"
                :disabled="jobBusy"
                @click="toggleProvince(p.code)"
              >
                {{ p.label }}
              </button>
            </div>
            <div class="chip-group isps">
              <button
                v-for="isp in isps"
                :key="isp.group"
                type="button"
                class="chip isp"
                :class="{ on: selectedISPs.includes(isp.group) }"
                :disabled="jobBusy"
                @click="toggleISP(isp.group)"
              >
                {{ isp.label }}
              </button>
            </div>
            <label v-if="hasEnabledCustomTargets" class="include-custom">
              <input v-model="includeCustom" type="checkbox" :disabled="jobBusy" />
              同时测自定义目标
            </label>
          </div>

          <div class="custom-bar">
            <button
              type="button"
              class="link-btn"
              :aria-expanded="showCustomPanel"
              :aria-controls="showCustomPanel ? 'st-custom-panel' : undefined"
              @click="showCustomPanel = !showCustomPanel"
            >
              自定义测速<span v-if="customTargets.length">（{{ customTargets.length }}）</span>
            </button>
            <div class="action-right">
              <UiButton
                v-if="isJobRunning"
                variant="danger"
                size="sm"
                :loading="canceling"
                @click="cancelNodeSpeedtest"
              >
                <Square />取消测速
              </UiButton>
              <UiButton
                v-else
                variant="primary"
                size="sm"
                :loading="starting && startingMode !== 'custom'"
                :disabled="!canStart || jobBusy"
                @click="startNodeSpeedtest(primaryMode)"
              >
                <Gauge />{{ selectedNode && !selectedNode.online ? '节点离线' : '开始测速' }}
              </UiButton>
            </div>
          </div>

          <div v-if="showCustomPanel" id="st-custom-panel" class="custom-panel">
            <div v-if="hasEnabledCustomTargets" class="custom-test-actions">
              <span class="faint">未勾选时测全部已启用目标</span>
              <UiButton
                size="sm"
                :loading="startingMode === 'custom'"
                :disabled="!canStart || jobBusy"
                @click="startNodeSpeedtest('custom')"
              >
                仅测自定义目标
              </UiButton>
            </div>
            <ul v-if="customTargets.length" class="custom-list">
              <li v-for="ct in customTargets" :key="ct.id">
                <label class="ct-check">
                  <input
                    type="checkbox"
                    :checked="selectedCustomIds.includes(ct.id)"
                    :disabled="jobBusy || !ct.enabled"
                    @change="toggleCustomSelection(ct.id)"
                  />
                  <span class="ct-name">{{ ct.name }}</span>
                  <span class="tag">{{ ct.isp_label }}</span>
                  <span v-if="ct.province" class="faint">{{ ct.province }}</span>
                </label>
                <span class="faint mono ct-urls" :title="ct.upload_url || ct.download_url">{{ ct.upload_url || ct.download_url }}</span>
                <button type="button" class="del-btn" title="删除" @click="deleteCustomTarget(ct)"><Trash2 /></button>
              </li>
            </ul>
            <div class="custom-form">
              <div class="cf-row">
                <UiField label="目标名称" for="ct-name">
                  <input id="ct-name" v-model="newCustom.name" class="input" placeholder="例如：深圳电信 LookingGlass" />
                </UiField>
                <UiField label="运营商归属" for="ct-isp">
                  <select id="ct-isp" v-model="newCustom.isp_group" class="select">
                    <option value="ct">电信</option>
                    <option value="cu">联通</option>
                    <option value="cm">移动</option>
                    <option value="other">其他</option>
                  </select>
                </UiField>
                <UiField label="地区标签" for="ct-prov">
                  <input id="ct-prov" v-model="newCustom.province" class="input" placeholder="可选，如：深圳" />
                </UiField>
              </div>
              <div class="cf-row">
                <UiField label="回程上传测速地址 (POST/PUT)" for="ct-up" help="从节点向该地址上传，测算回程速率与内核重传">
                  <input id="ct-up" v-model="newCustom.upload_url" class="input mono" placeholder="https://example.com/upload" />
                </UiField>
                <UiField label="去程下载测速地址 (GET)" for="ct-dl" help="节点从该地址单线程下载">
                  <input id="ct-dl" v-model="newCustom.download_url" class="input mono" placeholder="https://example.com/100mb.bin" />
                </UiField>
              </div>
              <div class="cf-actions">
                <select v-model="newCustom.method" class="select method-sel" aria-label="上传方法">
                  <option value="POST">POST</option>
                  <option value="PUT">PUT</option>
                </select>
                <UiButton size="sm" :loading="savingCustom" @click="addCustomTarget"><Plus />添加目标</UiButton>
              </div>
            </div>
          </div>
        </div>

        <div v-if="activeJob" class="job-box">
          <div class="job-meta">
            <span>
              <strong>{{ activeJob.node_name }}</strong>
              <span class="pill" :class="activeJob.status === 'done' ? 'good' : activeJob.status === 'failed' || activeJob.status === 'canceled' ? 'bad' : 'warn'">
                {{ activeJob.status === 'running' ? '测速进行中' : activeJob.status === 'done' ? '已完成' : activeJob.status === 'canceled' ? '已取消' : activeJob.status === 'failed' ? '失败' : '等待从机认领' }}
              </span>
            </span>
            <span class="faint num">{{ activeJob.completed_targets }} / {{ activeJob.total_targets }} ({{ progressPct }}%)</span>
          </div>
          <div class="prog-track">
            <div
              class="prog-fill"
              :class="{ bad: activeJob.status === 'failed' || activeJob.status === 'canceled' }"
              :style="{ width: `${progressPct}%` }"
            />
          </div>

          <div class="table-wrap">
            <table class="st-table">
              <thead>
                <tr>
                  <th>地区</th>
                  <th class="num">回程重传</th>
                  <th class="num">回程速度</th>
                  <th class="num">去程速度</th>
                  <th class="num">回程延迟</th>
                  <th class="num">去程延迟</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="row in activeJob.results" :key="row.target_key" :class="{ running: row.status === 'running' }">
                  <td class="region-cell" :title="row.resolved_ip ? `目标IP: ${row.resolved_ip}` : ''">
                    <div class="region-main">
                      <span>{{ row.label }}</span>
                      <span v-if="row.resolved_node && row.resolved_node !== row.label" class="node-tag">{{ row.resolved_node }}</span>
                    </div>
                    <div v-if="row.error" class="err-hint" :title="row.error">{{ row.error }}</div>
                  </td>
                  <td class="num" :class="row.status === 'done' ? retransTone(row.return_retrans) : 'muted'">
                    {{ row.status === 'done' && row.return_retrans >= 0 ? row.return_retrans : '--' }}
                  </td>
                  <td class="num" :class="row.status === 'done' ? speedTone(row.return_mbps) : 'muted'">
                    {{ formatMbpsCell(row, 'return') }}
                  </td>
                  <td class="num" :class="row.status === 'done' ? speedTone(row.outbound_mbps) : 'muted'">
                    {{ formatMbpsCell(row, 'outbound') }}
                  </td>
                  <td class="num" :class="row.status === 'done' ? rttTone(row.return_rtt_ms) : 'muted'">
                    {{ row.status === 'done' && row.return_rtt_ms >= 0 ? `${row.return_rtt_ms}ms` : '--' }}
                  </td>
                  <td class="num" :class="row.status === 'done' ? rttTone(row.outbound_rtt_ms) : 'muted'">
                    {{ row.status === 'done' && row.outbound_rtt_ms >= 0 ? `${row.outbound_rtt_ms}ms` : '--' }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
        <div v-else class="empty-hint">
          尚未对该节点运行三网测速，选择地区与运营商后点击「开始单线程测速」。
        </div>
      </template>

      <template v-else>
        <div class="direct-head">
          <p class="faint">
            从当前浏览器直接向主机和各在线从机发起单线程内存流测速（单节点约 5 秒），零 Emby 上游负载、不扣配额；HTTP 从机将通过同源小窗直接测试，无需额外配置 HTTPS。
          </p>
          <UiButton :variant="directRunning ? 'danger' : 'primary'" size="sm" :loading="directLoading" :disabled="directLoading && !directRunning" @click="runDirectTests">
            <Gauge />{{ directRunning ? '停止本机直测' : '开始本机直测' }}
          </UiButton>
        </div>

        <div class="table-wrap">
          <table class="st-table direct-table">
            <thead>
              <tr>
                <th>节点</th>
                <th>状态</th>
                <th class="num">首字节 TTFB</th>
                <th class="num">单线程下行</th>
                <th />
              </tr>
            </thead>
            <tbody>
              <tr v-for="row in directRows" :key="row.target.node_id">
                <td>
                  <strong>{{ row.target.name }}</strong>
                  <div v-if="row.target.public_url" class="faint mono sub-url">{{ row.target.public_url }}</div>
                </td>
                <td>
                  <span class="pill" :class="row.status === 'done' ? 'good' : row.status === 'running' ? 'warn' : row.status === 'error' || row.status === 'blocked' ? 'bad' : ''">
                    {{ row.status === 'running' ? '测速中' : row.status === 'done' ? '完成' : row.status === 'blocked' ? '不可测' : row.status === 'error' ? '失败' : '待测' }}
                  </span>
                  <div v-if="row.note" class="faint sub-note">{{ row.note }}</div>
                </td>
                <td class="num" :class="row.ttfbMs >= 0 ? rttTone(row.ttfbMs) : 'muted'">
                  {{ row.ttfbMs >= 0 ? `${row.ttfbMs}ms` : '--' }}
                </td>
                <td class="num" :class="row.mbps > 0 ? speedTone(row.mbps) : 'muted'">
                  {{ row.mbps > 0 ? `${row.mbps.toFixed(1)}Mbps` : '--' }}
                </td>
                <td class="num">
                  <UiButton
                    v-if="row.target.online && needsHTTPPopup(row.target) && row.target.supports_popup && row.target.popup_url && row.target.report_id"
                    size="sm"
                    variant="ghost"
                    :disabled="directRunning"
                    @click="runSingleDirectPopup(row)"
                  >
                    单独直测
                  </UiButton>
                </td>
              </tr>
              <tr v-if="!directRows.length && !directLoading">
                <td colspan="5" class="faint center">暂无可直测节点</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>
  </UiDrawer>
</template>

<style scoped>
.st-wrap { display: flex; flex-direction: column; gap: 16px; }
.st-tabs { display: flex; gap: 20px; border-bottom: 1px solid var(--border); }
.st-tabs button { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 38px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; cursor: pointer; }
.st-tabs button.on { color: var(--text); font-weight: 500; }
.st-tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.st-tabs .ico { width: 15px; height: 15px; }

.controls-box { display: flex; flex-direction: column; gap: 12px; padding: 14px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg); }
.row-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.target-field { min-width: 0; margin: 0; padding: 0; border: 0; }
.target-field legend { margin-bottom: 6px; padding: 0; color: var(--text-2); font-size: 13px; font-weight: 500; }
.target-options { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 2px; padding: 2px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--surface); }
.target-options label { position: relative; min-width: 0; cursor: pointer; }
.target-options input { position: absolute; width: 1px; height: 1px; opacity: 0; }
.target-options span { display: grid; place-items: center; min-height: 29px; padding: 3px 6px; border-radius: var(--radius-xs); color: var(--text-2); font-size: 13px; text-align: center; }
.target-options input:checked + span { background: var(--bg-elev); color: var(--text); font-weight: 600; box-shadow: 0 0 0 1px var(--border); }
.target-options input:focus-visible + span { outline: 2px solid var(--text-2); outline-offset: -2px; }
.target-field:disabled .target-options { opacity: 0.6; }
.target-field:disabled .target-options label { cursor: default; }
.picker { display: flex; flex-direction: column; gap: 8px; }
.picker-head { display: flex; align-items: center; justify-content: space-between; font-size: 12.5px; }
.picker-head .label { color: var(--text-2); font-weight: 500; }
.presets { display: flex; gap: 12px; }
.link-btn { border: 0; padding: 0; background: transparent; color: var(--text-2); font-size: 12.5px; cursor: pointer; text-decoration: underline; text-underline-offset: 3px; }
.link-btn:hover:not(:disabled) { color: var(--text); }
.link-btn:disabled { opacity: 0.45; cursor: default; }

.chip-group { display: flex; flex-wrap: wrap; gap: 6px; }
.chip { height: 26px; padding: 0 10px; border: 1px solid var(--border); border-radius: 999px; background: var(--bg-elev); color: var(--text-2); font-size: 12px; cursor: pointer; }
.chip.on { border-color: var(--text); background: var(--surface); color: var(--text); font-weight: 500; }
.chip.isp.on { border-color: var(--good); color: var(--good); background: var(--good-soft); }

.include-custom { display: inline-flex; align-items: center; align-self: flex-start; gap: 6px; color: var(--text-2); font-size: 12.5px; }
.custom-bar { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding-top: 4px; border-top: 1px solid var(--border); }
.action-right { display: flex; align-items: center; gap: 8px; }

.custom-panel { display: flex; flex-direction: column; gap: 12px; padding-top: 10px; border-top: 1px dashed var(--border); }
.custom-test-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px; font-size: 12.5px; }
.custom-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
.custom-list li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: 10px; padding: 8px 10px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg-elev); font-size: 12.5px; }
.ct-check { display: inline-flex; align-items: center; gap: 6px; cursor: pointer; }
.ct-name { font-weight: 500; }
.ct-urls { font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.del-btn { display: grid; place-items: center; width: 24px; height: 24px; border: 0; border-radius: 4px; background: transparent; color: var(--text-3); cursor: pointer; }
.del-btn:hover { color: var(--bad); background: var(--surface-hover); }
.del-btn svg { width: 13px; height: 13px; }

.custom-form { display: flex; flex-direction: column; gap: 10px; }
.cf-row { display: grid; grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); gap: 10px; }
.cf-actions { display: flex; justify-content: flex-end; align-items: center; gap: 8px; }
.method-sel { width: 96px; }

.job-box { display: flex; flex-direction: column; gap: 10px; }
.job-meta { display: flex; align-items: center; justify-content: space-between; font-size: 13px; }
.job-meta > span { display: inline-flex; align-items: center; gap: 8px; }
.prog-track { height: 4px; border-radius: 999px; background: var(--surface); overflow: hidden; }
.prog-fill { height: 100%; background: var(--good); transition: width 240ms var(--ease); }
.prog-fill.bad { background: var(--bad); }

.table-wrap { border: 1px solid var(--border); border-radius: var(--radius); overflow-x: auto; background: var(--bg-elev); }
.st-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.st-table th, .st-table td { padding: 9px 12px; text-align: left; border-bottom: 1px solid var(--border); white-space: nowrap; }
.st-table th { font-size: 12px; font-weight: 500; color: var(--text-3); background: var(--bg); }
.st-table th.num, .st-table td.num { text-align: right; font-variant-numeric: tabular-nums; }
.st-table tr:last-child td { border-bottom: 0; }
.st-table tr.running { background: color-mix(in srgb, var(--warn) 6%, transparent); }
.region-cell { font-weight: 500; min-width: 140px; }
.region-main { display: flex; align-items: center; gap: 6px; flex-wrap: nowrap; }
.node-tag { font-size: 11px; font-weight: 400; padding: 1px 6px; border-radius: 4px; background: var(--surface); color: var(--text-3); }
.err-hint { font-weight: 400; color: var(--bad); font-size: 11.5px; margin-top: 2px; white-space: normal; max-width: 240px; word-break: break-word; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; }

.good { color: var(--good); font-weight: 600; }
.warn { color: var(--warn); font-weight: 500; }
.bad { color: var(--bad); font-weight: 500; }
.muted { color: var(--text-3); }
.faint { color: var(--text-3); }
.mono { font-family: var(--font-mono); }
.tag { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: var(--surface); color: var(--text-2); }
.pill { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: 999px; font-size: 11.5px; background: var(--surface); color: var(--text-2); }
.pill.good { background: var(--good-soft); color: var(--good); }
.pill.warn { background: var(--warn-soft); color: var(--warn); }
.pill.bad { background: color-mix(in srgb, var(--bad) 12%, transparent); color: var(--bad); }

.empty-hint { padding: 36px 16px; text-align: center; font-size: 13px; color: var(--text-3); border: 1px dashed var(--border); border-radius: var(--radius); }
.direct-head { display: flex; align-items: center; justify-content: space-between; gap: 14px; }
.direct-head p { margin: 0; font-size: 12.5px; line-height: 1.5; }
.sub-url { font-size: 11px; margin-top: 2px; }
.sub-note { font-size: 11.5px; margin-top: 3px; white-space: normal; max-width: 280px; }
.center { text-align: center !important; padding: 24px !important; }

@media (max-width: 640px) {
  .row-grid { grid-template-columns: 1fr; }
  .direct-head { flex-direction: column; align-items: flex-start; }
}
</style>
