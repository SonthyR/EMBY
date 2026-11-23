<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { ArrowUpToLine, Database, Download, GripVertical, RefreshCw, Trash2 } from '@lucide/vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import {
  ip2regionApi,
  ispRoutesApi,
  type IP2RegionLookupResult,
  type IP2RegionStatus,
  type ISPRouteGroup,
  type ISPRouteNode,
} from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { formatBytes } from '@/utils/format'

const groups = ref<ISPRouteGroup[]>([])
const dbStatus = ref<IP2RegionStatus>({
  v4: { version: 'v4', downloaded: false, size_bytes: 0 },
  v6: { version: 'v6', downloaded: false, size_bytes: 0 },
  mirrors: [],
})
const loading = ref(true)
const loadError = ref('')
const savingGroup = ref<string | null>(null)
const updatingDB = ref<'v4' | 'v6' | null>(null)
const deletingDB = ref<'v4' | 'v6' | null>(null)

const testIP = ref('')
const testing = ref(false)
const testResult = ref<IP2RegionLookupResult | null>(null)
const testError = ref('')

// Drag state scoped per ISP group
const dragState = ref<{ group: string; nodeId: number; before: number[] } | null>(null)
let touchDragActive = false

function formatTimestamp(raw?: string): string {
  if (!raw) return ''
  const parsed = new Date(raw)
  if (Number.isNaN(parsed.getTime())) return raw
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())} ${pad(parsed.getHours())}:${pad(parsed.getMinutes())}`
}

async function load(silent = false) {
  if (dragState.value || savingGroup.value) return
  if (!silent) loading.value = true
  try {
    const [routeRes, ipStatus] = await Promise.all([ispRoutesApi.get(), ip2regionApi.status()])
    groups.value = routeRes.groups || []
    dbStatus.value = ipStatus
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '加载线路配置失败')
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => document.visibilityState === 'visible' && load(true), 15000)
})
onBeforeUnmount(() => clearInterval(timer))

function reorderLocal(groupKey: string, sourceId: number, targetId: number) {
  if (sourceId === targetId) return
  const group = groups.value.find(g => g.group === groupKey)
  if (!group) return
  const list = [...group.nodes]
  const from = list.findIndex(n => n.node_id === sourceId)
  const to = list.findIndex(n => n.node_id === targetId)
  if (from < 0 || to < 0) return
  const [moved] = list.splice(from, 1)
  list.splice(to, 0, moved)
  group.nodes = list
}

async function persistGroup(groupKey: string) {
  const group = groups.value.find(g => g.group === groupKey)
  if (!group) return
  savingGroup.value = groupKey
  try {
    const res = await ispRoutesApi.update(
      groupKey,
      group.nodes.map(n => ({ node_id: n.node_id, enabled: n.enabled })),
    )
    groups.value = res.groups || []
    toast.success(`「${group.label}」线路优先级已更新`)
  } catch (error) {
    toast.error(errorMessage(error, '保存线路失败'))
    await load(true)
  } finally {
    savingGroup.value = null
  }
}

function onDragStart(event: DragEvent, groupKey: string, node: ISPRouteNode) {
  const group = groups.value.find(g => g.group === groupKey)
  if (!group || group.nodes.length <= 1 || savingGroup.value) return
  dragState.value = { group: groupKey, nodeId: node.node_id, before: group.nodes.map(n => n.node_id) }
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(node.node_id))
  }
}

function onDragOver(event: DragEvent, groupKey: string, target: ISPRouteNode) {
  if (!dragState.value || dragState.value.group !== groupKey) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  if (dragState.value.nodeId !== target.node_id) {
    reorderLocal(groupKey, dragState.value.nodeId, target.node_id)
  }
}

function onDrop(event: DragEvent) {
  event.preventDefault()
}

async function onDragEnd() {
  if (!dragState.value) return
  const { group: groupKey, before } = dragState.value
  dragState.value = null
  const group = groups.value.find(g => g.group === groupKey)
  if (!group) return
  const after = group.nodes.map(n => n.node_id)
  if (after.length === before.length && after.every((id, i) => id === before[i])) return
  await persistGroup(groupKey)
}

function onTouchStart(event: TouchEvent, groupKey: string, node: ISPRouteNode) {
  const group = groups.value.find(g => g.group === groupKey)
  if (!group || group.nodes.length <= 1 || savingGroup.value || event.touches.length !== 1) return
  touchDragActive = true
  dragState.value = { group: groupKey, nodeId: node.node_id, before: group.nodes.map(n => n.node_id) }
}

function onTouchMove(event: TouchEvent) {
  if (!touchDragActive || !dragState.value || event.touches.length !== 1) return
  event.preventDefault()
  const touch = event.touches[0]
  const el = document.elementFromPoint(touch.clientX, touch.clientY)?.closest<HTMLElement>('[data-route-node]')
  if (!el) return
  const targetGroup = el.dataset.routeGroup
  const targetId = Number(el.dataset.routeNode)
  if (targetGroup === dragState.value.group && !Number.isNaN(targetId) && targetId !== dragState.value.nodeId) {
    reorderLocal(targetGroup, dragState.value.nodeId, targetId)
  }
}

async function onTouchEnd() {
  if (!touchDragActive) return
  touchDragActive = false
  await onDragEnd()
}

async function promoteNode(groupKey: string, node: ISPRouteNode) {
  const group = groups.value.find(g => g.group === groupKey)
  if (!group || savingGroup.value) return
  if (group.nodes[0]?.node_id === node.node_id && node.enabled) return
  node.enabled = true
  group.nodes = [node, ...group.nodes.filter(n => n.node_id !== node.node_id)]
  await persistGroup(groupKey)
}

async function toggleNodeEnabled(groupKey: string, node: ISPRouteNode, value: boolean) {
  node.enabled = value
  await persistGroup(groupKey)
}

function nodeStatusText(node: ISPRouteNode): string {
  if (node.is_master) return '兜底主机'
  switch (node.status) {
    case 'online':
      return '在线'
    case 'waiting':
      return '待接入'
    case 'offline':
      return '离线'
    case 'quota':
      return '超额'
    case 'disabled':
      return '已停用'
    default:
      return node.status || '未知'
  }
}

function nodeStatusClass(node: ISPRouteNode): string {
  if (node.is_master || node.status === 'online') return 'good'
  if (node.status === 'waiting' || node.status === 'quota') return 'warn'
  if (node.status === 'offline') return 'bad'
  return 'muted'
}

function activeNodeLabel(group: ISPRouteGroup): string {
  return group.nodes.find(n => n.active)?.name || '主机'
}

async function runLookup(sample?: string) {
  if (sample) testIP.value = sample
  const ip = testIP.value.trim()
  testError.value = ''
  if (!ip) {
    testError.value = '请输入 IPv4 或 IPv6 地址'
    return
  }
  testing.value = true
  try {
    testResult.value = await ip2regionApi.lookup(ip)
  } catch (error) {
    testResult.value = null
    testError.value = errorMessage(error, '查询失败')
  } finally {
    testing.value = false
  }
}

async function updateIPDB(version: 'v4' | 'v6') {
  updatingDB.value = version
  try {
    const res = await ip2regionApi.update(version)
    dbStatus.value = res.status
    toast.success(`${version === 'v4' ? 'IPv4' : 'IPv6'} 地区库已更新并热切换生效`)
  } catch (error) {
    toast.error(errorMessage(error, '下载地区库失败'))
  } finally {
    updatingDB.value = null
  }
}

async function deleteIPDB(version: 'v4' | 'v6') {
  const label = version === 'v4' ? 'IPv4' : 'IPv6'
  if (!(await confirm({ title: `删除 ${label} 地区库？`, message: `删除后该协议的访客 IP 将归入「其他」线路组，直到重新下载。`, confirmText: '删除', danger: true }))) return
  deletingDB.value = version
  try {
    dbStatus.value = await ip2regionApi.remove(version)
    toast.success(`已删除 ${label} 地区库`)
  } catch (error) {
    toast.error(errorMessage(error, '删除失败'))
  } finally {
    deletingDB.value = null
  }
}
</script>

<template>
  <div class="routes-page">
    <PageHeader title="线路" description="按访问者运营商智能分流媒体播放流：拖动调整每条线路的节点优先级，首位在线节点生效，离线自动降级。">
      <UiButton @click="load()"><RefreshCw />刷新</UiButton>
    </PageHeader>

    <div v-if="loading" class="grid">
      <div v-for="n in 4" :key="n" class="group-card skeleton"><span /></div>
    </div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <template v-else>
      <section class="section">
        <div class="section-head">
          <h2>三网智能分流</h2>
          <p class="faint">控制面（登录、浏览、海报）始终在主机完成；视频播放请求按客户端 IP 运营商自动 307 调度到首位可用节点。</p>
        </div>

        <div class="grid">
          <article v-for="group in groups" :key="group.group" class="group-card">
            <header class="group-head">
              <div class="group-title">
                <h3>{{ group.label }}</h3>
                <span class="active-chip">当前：{{ activeNodeLabel(group) }}</span>
              </div>
              <span class="faint hint">拖动排序</span>
            </header>

            <TransitionGroup name="list" tag="ul" class="node-list">
              <li
                v-for="(node, idx) in group.nodes"
                :key="node.node_id"
                :data-route-group="group.group"
                :data-route-node="node.node_id"
                class="node-row"
                :class="{
                  active: node.active,
                  off: !node.enabled,
                  dragging: dragState?.group === group.group && dragState?.nodeId === node.node_id,
                  movable: group.nodes.length > 1,
                }"
                :draggable="group.nodes.length > 1 && !savingGroup"
                @dragstart="onDragStart($event, group.group, node)"
                @dragover="onDragOver($event, group.group, node)"
                @drop="onDrop"
                @dragend="onDragEnd"
              >
                <span
                  v-if="group.nodes.length > 1"
                  class="grip"
                  title="拖动调整优先级"
                  @touchstart="onTouchStart($event, group.group, node)"
                  @touchmove="onTouchMove"
                  @touchend="onTouchEnd"
                  @touchcancel="onTouchEnd"
                >
                  <GripVertical />
                </span>
                <span class="rank num">{{ idx + 1 }}</span>

                <div class="node-info">
                  <div class="node-name">
                    <span>{{ node.name }}</span>
                    <span v-if="node.active" class="badge-active">当前生效</span>
                    <span class="status-dot" :class="nodeStatusClass(node)">{{ nodeStatusText(node) }}</span>
                  </div>
                  <div v-if="node.public_url" class="node-url mono">{{ node.public_url }}</div>
                </div>

                <div class="node-ctrl">
                  <button
                    v-if="idx > 0 && group.nodes.length > 1"
                    type="button"
                    class="icon-btn"
                    title="设为首选"
                    :disabled="!!savingGroup"
                    @click="promoteNode(group.group, node)"
                  >
                    <ArrowUpToLine />
                  </button>
                  <UiSwitch
                    :model-value="node.enabled"
                    :disabled="!!savingGroup"
                    @update:model-value="toggleNodeEnabled(group.group, node, $event)"
                  />
                </div>
              </li>
            </TransitionGroup>
          </article>
        </div>
      </section>

      <section class="section card-panel">
        <div class="section-head">
          <h2>IP 路由测试</h2>
          <p class="faint">输入任意 IPv4 或 IPv6 地址，验证离线地区库识别结果及最终命中的播放节点。</p>
        </div>

        <form class="test-bar" @submit.prevent="runLookup()">
          <input v-model="testIP" class="input mono test-input" placeholder="输入 IP 地址，例如 202.96.128.86 或 221.179.155.161" />
          <UiButton type="submit" variant="primary" :loading="testing">测试分流</UiButton>
          <div class="samples">
            <button type="button" @click="runLookup('202.96.128.86')">电信示例</button>
            <button type="button" @click="runLookup('221.6.4.66')">联通示例</button>
            <button type="button" @click="runLookup('221.179.155.161')">移动示例</button>
          </div>
        </form>

        <p v-if="testError" class="err">{{ testError }}</p>
        <div v-else-if="testResult" class="test-result">
          <div><span class="k">查询 IP</span><strong class="mono">{{ testResult.ip }}</strong></div>
          <div><span class="k">归属地</span><strong>{{ testResult.region || '未识别（请先下载地区库）' }}</strong></div>
          <div><span class="k">运营商</span><strong>{{ testResult.isp || '—' }}</strong></div>
          <div><span class="k">匹配线路</span><strong>{{ testResult.isp_label }}</strong></div>
          <div>
            <span class="k">命中节点</span>
            <strong class="good">{{ testResult.node_name }}</strong>
            <span v-if="testResult.public_url" class="mono faint"> ({{ testResult.public_url }})</span>
          </div>
        </div>
      </section>

      <section class="section card-panel">
        <div class="db-head">
          <div class="db-title">
            <Database class="db-icon" />
            <div>
              <h2>IP 地区库</h2>
              <p class="faint">离线本地查询，不向第三方发送访客 IP；用于三网智能分流和请求日志的地区/运营商展示。</p>
            </div>
          </div>
        </div>

        <div class="db-rows">
          <div class="db-row">
            <div class="db-meta">
              <div class="db-name">
                <strong>IPv4 地区库</strong>
                <span class="faint size-hint">~11 MB</span>
                <span class="pill" :class="dbStatus.v4.downloaded ? 'good' : 'muted'">
                  {{ dbStatus.v4.downloaded ? '已下载' : '未下载' }}
                </span>
              </div>
              <p class="faint db-sub">
                <template v-if="dbStatus.v4.downloaded">
                  文件大小 {{ formatBytes(dbStatus.v4.size_bytes) }} · 更新于 {{ formatTimestamp(dbStatus.v4.updated_at) }}
                </template>
                <template v-else>
                  未下载 ip2region_v4.xdb，IPv4 访客将默认走「其他」线路组。
                </template>
              </p>
            </div>
            <div class="db-actions">
              <UiButton
                size="sm"
                variant="primary"
                :loading="updatingDB === 'v4'"
                :disabled="updatingDB !== null || deletingDB !== null"
                @click="updateIPDB('v4')"
              >
                <Download />{{ dbStatus.v4.downloaded ? '在线更新' : '下载' }}
              </UiButton>
              <UiButton
                v-if="dbStatus.v4.downloaded"
                size="sm"
                variant="ghost"
                :loading="deletingDB === 'v4'"
                :disabled="updatingDB !== null || deletingDB !== null"
                @click="deleteIPDB('v4')"
              >
                <Trash2 />删除
              </UiButton>
            </div>
          </div>

          <div class="db-row">
            <div class="db-meta">
              <div class="db-name">
                <strong>IPv6 地区库</strong>
                <span class="faint size-hint">~37 MB</span>
                <span class="pill" :class="dbStatus.v6.downloaded ? 'good' : 'muted'">
                  {{ dbStatus.v6.downloaded ? '已下载' : '未下载' }}
                </span>
              </div>
              <p class="faint db-sub">
                <template v-if="dbStatus.v6.downloaded">
                  文件大小 {{ formatBytes(dbStatus.v6.size_bytes) }} · 更新于 {{ formatTimestamp(dbStatus.v6.updated_at) }}
                </template>
                <template v-else>
                  未下载 ip2region_v6.xdb，按需下载（内存仅加载 512 KB 索引，几乎不占内存）。
                </template>
              </p>
            </div>
            <div class="db-actions">
              <UiButton
                size="sm"
                variant="primary"
                :loading="updatingDB === 'v6'"
                :disabled="updatingDB !== null || deletingDB !== null"
                @click="updateIPDB('v6')"
              >
                <Download />{{ dbStatus.v6.downloaded ? '在线更新' : '下载' }}
              </UiButton>
              <UiButton
                v-if="dbStatus.v6.downloaded"
                size="sm"
                variant="ghost"
                :loading="deletingDB === 'v6'"
                :disabled="updatingDB !== null || deletingDB !== null"
                @click="deleteIPDB('v6')"
              >
                <Trash2 />删除
              </UiButton>
            </div>
          </div>
        </div>

        <div class="mirror-note">
          <p>数据来自 <strong>ip2region</strong>（lionsoul2014/ip2region），在线更新依次尝试以下源并自动热切换（无需重启）：</p>
          <ol>
            <li v-for="mirror in dbStatus.mirrors" :key="mirror" class="mono">{{ mirror }}</li>
          </ol>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.routes-page { display: flex; flex-direction: column; gap: 24px; }
.section { display: flex; flex-direction: column; gap: 14px; }
.section-head h2 { margin: 0; font-size: 16px; font-weight: 600; }
.section-head p { margin: 4px 0 0; font-size: 13px; }
.faint { color: var(--text-3); }
.mono { font-family: var(--font-mono); }

.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.group-card { display: flex; flex-direction: column; gap: 12px; padding: 16px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-elev); }
.group-card.skeleton { height: 180px; }
.group-card.skeleton span { display: block; width: 40%; height: 14px; border-radius: 6px; background: var(--surface); }

.group-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.group-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.group-title h3 { margin: 0; font-size: 15px; font-weight: 600; }
.active-chip { display: inline-flex; align-items: center; height: 22px; padding: 0 8px; border-radius: 999px; font-size: 11.5px; background: color-mix(in srgb, var(--good) 12%, transparent); color: var(--good); font-weight: 500; }
.hint { font-size: 12px; }

.node-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.node-row { display: flex; align-items: center; gap: 10px; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), opacity var(--dur) var(--ease); }
.node-row.movable { cursor: grab; }
.node-row.active { border-color: color-mix(in srgb, var(--good) 45%, var(--border)); background: color-mix(in srgb, var(--good) 5%, var(--bg)); }
.node-row.off { opacity: 0.55; }
.node-row.dragging { opacity: 0.4; cursor: grabbing; }

.grip { display: inline-flex; align-items: center; color: var(--text-3); touch-action: none; cursor: grab; }
.grip svg { width: 15px; height: 15px; }
.rank { width: 18px; text-align: center; font-size: 12px; font-weight: 600; color: var(--text-3); }

.node-info { flex: 1; min-width: 0; }
.node-name { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; font-size: 13.5px; font-weight: 500; }
.node-url { margin-top: 2px; font-size: 11.5px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.badge-active { font-size: 11px; padding: 1px 6px; border-radius: 4px; background: color-mix(in srgb, var(--good) 14%, transparent); color: var(--good); font-weight: 500; }
.status-dot { font-size: 11.5px; color: var(--text-3); }
.status-dot.good { color: var(--good); }
.status-dot.warn { color: var(--warn); }
.status-dot.bad { color: var(--bad); }

.node-ctrl { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.icon-btn { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 6px; background: transparent; color: var(--text-3); cursor: pointer; }
.icon-btn:hover { background: var(--surface-hover); color: var(--text); }
.icon-btn svg { width: 14px; height: 14px; }

.card-panel { padding: 18px 20px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-elev); }

.test-bar { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.test-input { flex: 1; min-width: 240px; }

.samples { display: flex; gap: 6px; flex-wrap: wrap; }
.samples button { height: 28px; padding: 0 10px; border: 1px solid var(--border); border-radius: 999px; background: transparent; color: var(--text-2); font-size: 12px; cursor: pointer; }
.samples button:hover { background: var(--surface); color: var(--text); }

.test-result { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; padding: 14px 16px; border-radius: var(--radius-sm); background: var(--surface); font-size: 13px; }
.test-result > div { display: flex; flex-direction: column; gap: 4px; }
.test-result .k { font-size: 11.5px; color: var(--text-3); }
.test-result .good { color: var(--good); }
.err { margin: 0; font-size: 13px; color: var(--bad); }

.db-title { display: flex; align-items: flex-start; gap: 10px; }
.db-icon { width: 18px; height: 18px; margin-top: 2px; color: var(--text-2); flex-shrink: 0; }
.db-title h2 { margin: 0; font-size: 16px; font-weight: 600; }
.db-title p { margin: 4px 0 0; font-size: 13px; }

.db-rows { display: flex; flex-direction: column; gap: 10px; }
.db-row { display: flex; align-items: center; justify-content: space-between; gap: 14px; padding: 14px 16px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg); flex-wrap: wrap; }
.db-meta { flex: 1; min-width: 240px; }
.db-name { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 14px; }
.size-hint { font-size: 12px; }
.pill { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: 999px; font-size: 11.5px; font-weight: 500; }
.pill.good { background: color-mix(in srgb, var(--good) 12%, transparent); color: var(--good); }
.pill.muted { background: var(--surface); color: var(--text-3); }
.db-sub { margin: 4px 0 0; font-size: 12.5px; }
.db-actions { display: flex; align-items: center; gap: 8px; }

.mirror-note { padding: 12px 14px; border-radius: var(--radius-sm); background: var(--surface); font-size: 12px; color: var(--text-2); line-height: 1.6; }
.mirror-note p { margin: 0 0 6px; }
.mirror-note ol { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 2px; color: var(--text-3); word-break: break-all; }

.empty { padding: 48px 24px; text-align: center; border: 1px dashed var(--border); border-radius: var(--radius); color: var(--text-2); }

.list-move { transition: transform 180ms var(--ease); }

@media (max-width: 960px) {
  .grid { grid-template-columns: 1fr; }
}
</style>
