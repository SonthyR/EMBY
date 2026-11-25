<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Activity, ArrowDownWideNarrow, ArrowUpToLine, Database, Download, GripVertical, RefreshCw, ShieldAlert, ShieldBan, Trash2 } from '@lucide/vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SpeedtestDrawer from '@/components/edge/SpeedtestDrawer.vue'
import IspProbeDrawer from '@/components/edge/IspProbeDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import {
  ip2regionApi,
  ispRoutesApi,
  type IP2RegionLookupResult,
  type IP2RegionStatus,
  type ISPRouteGroup,
  type ISPRouteNode,
  type ISPRouteStrategy,
} from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { formatBytes } from '@/utils/format'
import { usePointerSort } from '@/composables/usePointerSort'

const groups = ref<ISPRouteGroup[]>([])
const blockNonMainland = ref(false)
const savingNonMainland = ref(false)
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

const pointerSort = usePointerSort()

function formatTimestamp(raw?: string): string {
  if (!raw) return ''
  const parsed = new Date(raw)
  if (Number.isNaN(parsed.getTime())) return raw
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${parsed.getFullYear()}-${pad(parsed.getMonth() + 1)}-${pad(parsed.getDate())} ${pad(parsed.getHours())}:${pad(parsed.getMinutes())}`
}

async function load(silent = false) {
  if (pointerSort.isDragging.value || savingGroup.value || savingNonMainland.value) return
  if (!silent) loading.value = true
  try {
    const [routeRes, ipStatus] = await Promise.all([ispRoutesApi.get(), ip2regionApi.status()])
    groups.value = routeRes.groups || []
    blockNonMainland.value = !!routeRes.block_non_mainland
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

function startRouteSort(event: PointerEvent, groupKey: string, node: ISPRouteNode) {
  const targetEl = event.target as HTMLElement | null
  if (targetEl?.closest('button, input, a, [role="switch"]')) return
  const group = groups.value.find(g => g.group === groupKey)
  if (!group || group.nodes.length <= 1 || savingGroup.value) return
  const itemEl = (event.currentTarget as HTMLElement | null)?.closest<HTMLElement>('[data-route-node]')
  const containerEl = itemEl?.closest<HTMLElement>('.node-list')
  if (!itemEl || !containerEl) return

  pointerSort.start<ISPRouteNode>({
    event,
    itemEl,
    containerEl,
    itemSelector: '[data-route-node]',
    getKeyFromEl: el => {
      const id = Number(el.dataset.routeNode)
      return Number.isNaN(id) ? null : id
    },
    activeKey: node.node_id,
    scope: groupKey,
    mode: 'list',
    getList: () => groups.value.find(g => g.group === groupKey)?.nodes || [],
    getItemKey: item => item.node_id,
    applyList: next => {
      const g = groups.value.find(item => item.group === groupKey)
      if (g) g.nodes = next
    },
    onCommit: async () => {
      await persistGroup(groupKey)
    },
  })
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

async function toggleBlockNonMainland(value: boolean) {
  if (value && !dbStatus.value.v4.downloaded) {
    const ok = await confirm({
      title: '尚未下载 IPv4 地区库',
      message: '未下载 IPv4 地区库时，系统无法识别 IPv4 访客是否来自海外，开启后该规则暂不会对 IPv4 生效。建议先在下方下载 IPv4 地区库，仍要现在开启吗？',
      confirmText: '仍要开启',
    })
    if (!ok) return
  }
  savingNonMainland.value = true
  try {
    const res = await ispRoutesApi.setBlockNonMainland(value)
    blockNonMainland.value = !!res.block_non_mainland
    groups.value = res.groups || []
    toast.success(value ? '已开启：海外 / 非中国大陆 IP 将禁止连接 Emby' : '已关闭海外 / 非中国大陆 IP 拦截')
  } catch (error) {
    toast.error(errorMessage(error, '保存配置失败'))
    await load(true)
  } finally {
    savingNonMainland.value = false
  }
}

async function toggleGroupBlock(group: ISPRouteGroup, value: boolean) {
  if (value && group.group === 'other' && !dbStatus.value.v4.downloaded) {
    const ok = await confirm({
      title: '尚未下载 IPv4 地区库，确定禁止「其他」分组？',
      message: '在未下载 IPv4 地区库时，所有 IPv4 访客都会被归入「其他」分组；现在开启将拦截所有 IPv4 Emby 客户端连接（管理面板和内网不受影响）。确认继续吗？',
      confirmText: '确认禁止',
      danger: true,
    })
    if (!ok) return
  }
  savingGroup.value = group.group
  try {
    const res = await ispRoutesApi.setGroupBlock(group.group, value)
    blockNonMainland.value = !!res.block_non_mainland
    groups.value = res.groups || []
    toast.success(value ? `已禁止「${group.label}」分组连接 Emby` : `已恢复「${group.label}」分组连接`)
  } catch (error) {
    toast.error(errorMessage(error, '保存配置失败'))
    await load(true)
  } finally {
    savingGroup.value = null
  }
}

function nodeStatusText(node: ISPRouteNode): string {
  if (node.is_master) return node.status === 'quota' ? '超额' : '主机'
  if (node.circuit_tripped) return '已熔断（已切流）'
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
  if (!node.is_master && node.circuit_tripped) return 'bad'
  if (node.status === 'quota') return 'warn'
  if (node.is_master || node.status === 'online') return 'good'
  if (node.status === 'waiting') return 'warn'
  if (node.status === 'offline') return 'bad'
  return 'muted'
}

function activeNodeLabel(group: ISPRouteGroup): string {
  const activeNodes = group.nodes.filter(n => n.active)
  if (group.strategy === 'balance' && activeNodes.length > 1) {
    return `负载均衡 (${activeNodes.length} 台)`
  }
  return activeNodes[0]?.name || '主机'
}

const speedtestOpen = ref(false)
const ispProbeOpen = ref(false)

const STRATEGIES: { key: ISPRouteStrategy; label: string }[] = [
  { key: 'priority', label: '手动优先级' },
  { key: 'balance', label: '负载均衡' },
]

async function updateNodeBalanceCap(groupKey: string, node: ISPRouteNode, event: Event) {
  const input = event.target as HTMLInputElement
  const raw = Number.parseInt(input.value.trim(), 10)
  if (!Number.isInteger(raw) || raw < 1 || raw > 99) {
    input.value = String(node.max_balance_streams || 2)
    toast.error('负载均衡上限应为 1–99 路')
    return
  }
  if (raw === (node.max_balance_streams || 2)) return
  const group = groups.value.find(g => g.group === groupKey)
  if (!group || savingGroup.value) return
  savingGroup.value = groupKey
  try {
    const res = await ispRoutesApi.update(
      groupKey,
      group.nodes.map(n => ({
        node_id: n.node_id,
        enabled: n.enabled,
        ...(n.node_id === node.node_id ? { max_balance_streams: raw } : {}),
      })),
    )
    groups.value = res.groups || []
    toast.success(`「${node.name}」负载上限已设为 ${raw} 路`)
  } catch (error) {
    input.value = String(node.max_balance_streams || 2)
    toast.error(errorMessage(error, '保存负载上限失败'))
    await load(true)
  } finally {
    savingGroup.value = null
  }
}

function hasSpeedMetrics(group: ISPRouteGroup): boolean {
  return group.nodes.some(n => (n.speed_metric?.avg_return_bps || 0) > 0)
}

async function setGroupStrategy(group: ISPRouteGroup, strategy: ISPRouteStrategy) {
  if (group.strategy === strategy || savingGroup.value) return
  savingGroup.value = group.group
  try {
    const res = await ispRoutesApi.setStrategy(group.group, strategy)
    blockNonMainland.value = !!res.block_non_mainland
    groups.value = res.groups || []
    const lbl = STRATEGIES.find(s => s.key === strategy)?.label || strategy
    toast.success(`「${group.label}」已切换为${lbl}`)
  } catch (error) {
    toast.error(errorMessage(error, '保存调度模式失败'))
  } finally {
    savingGroup.value = null
  }
}

async function sortGroupBySpeed(group: ISPRouteGroup) {
  if (!hasSpeedMetrics(group) || savingGroup.value) return
  const indexed = group.nodes.map((node, index) => ({ node, index }))
  indexed.sort((a, b) => {
    const aBps = a.node.speed_metric?.avg_return_bps || 0
    const bBps = b.node.speed_metric?.avg_return_bps || 0
    if (aBps !== bBps) return bBps - aBps
    return a.index - b.index
  })
  group.nodes = indexed.map(item => item.node)
  await persistGroup(group.group)
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
    <PageHeader title="线路" description="按访问者运营商智能分流媒体播放流：支持手动优先级与按排序阶梯填满的负载均衡。">
      <UiButton @click="ispProbeOpen = true"><ShieldAlert />阻断熔断</UiButton>
      <UiButton @click="speedtestOpen = true"><Activity />单线程测速</UiButton>
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

        <div class="guard-bar" :class="{ active: blockNonMainland }">
          <div class="guard-meta">
            <ShieldBan class="guard-icon" />
            <div>
              <div class="guard-title">
                <strong>禁止海外 / 非中国大陆 IP 连接 Emby</strong>
                <span v-if="blockNonMainland" class="blocked-chip">已开启 · 拦截非大陆</span>
              </div>
              <p class="faint guard-desc">
                拒绝来自海外及港澳台（常见代理/梯子节点）的 Emby 客户端连接（HTTP 403），防止忘关代理消耗双份流量；国内广电、教育网等小宽带仍正常走「其他」组。管理面板网页、朋友自助页、从机心跳及内网 IP 永远放行。
              </p>
            </div>
          </div>
          <UiSwitch
            :model-value="blockNonMainland"
            :disabled="savingNonMainland"
            @update:model-value="toggleBlockNonMainland"
          />
        </div>

        <div class="grid">
          <article v-for="group in groups" :key="group.group" class="group-card" :class="{ blocked: group.block_connect }">
            <header class="group-head">
              <div class="group-title">
                <h3>{{ group.label }}</h3>
                <span v-if="group.block_connect" class="blocked-chip">已禁止连接</span>
                <span v-else class="active-chip">当前：{{ activeNodeLabel(group) }}</span>
              </div>
              <div class="group-controls">
                <span class="faint block-label">禁止连接</span>
                <UiSwitch
                  :model-value="group.block_connect"
                  :disabled="savingGroup === group.group"
                  @update:model-value="toggleGroupBlock(group, $event)"
                />
              </div>
            </header>

            <p v-if="group.block_connect" class="block-banner">
              已拒绝「{{ group.label }}」分组的 Emby 客户端连接（HTTP 403），不会消耗主机或从机流量；面板网页与内网不受影响。
            </p>

            <div class="strategy-bar" :class="{ dimmed: group.block_connect }">
              <div class="seg" role="group" :aria-label="`${group.label}调度模式`">
                <button
                  v-for="s in STRATEGIES"
                  :key="s.key"
                  type="button"
                  :class="{ on: (group.strategy || 'priority') === s.key }"
                  :disabled="savingGroup === group.group"
                  @click="setGroupStrategy(group, s.key)"
                >
                  {{ s.label }}
                </button>
              </div>
              <UiButton
                size="sm"
                variant="ghost"
                :disabled="!hasSpeedMetrics(group) || !!savingGroup"
                :title="hasSpeedMetrics(group) ? '按单线程实测回程速度降序推荐排序' : '请先在右上角对节点运行单线程测速'"
                @click="sortGroupBySpeed(group)"
              >
                <ArrowDownWideNarrow />按速度排序
              </UiButton>
            </div>
            <p v-if="!group.block_connect && group.strategy === 'balance'" class="strategy-hint faint">
              负载均衡：按拖拽顺序依次填满每台播放路数上限（1 台观看设备计 1 路，全站共享单机上限），全满后按排序轮流顺延；主机开启开关即平等参与。
            </p>

            <TransitionGroup name="list" tag="ul" class="node-list" :class="{ dimmed: group.block_connect }">
              <li
                v-for="(node, idx) in group.nodes"
                :key="node.node_id"
                :data-route-group="group.group"
                :data-route-node="node.node_id"
                class="node-row"
                :class="{
                  active: node.active,
                  off: !node.enabled,
                  tripped: !node.is_master && !!node.circuit_tripped,
                  dragging: pointerSort.draggingScope.value === group.group && pointerSort.draggingKey.value === node.node_id,
                  'is-sort-placeholder': pointerSort.draggingScope.value === group.group && pointerSort.draggingKey.value === node.node_id,
                  movable: group.nodes.length > 1 && !savingGroup,
                }"
                @pointerdown="startRouteSort($event, group.group, node)"
              >
                <span
                  v-if="group.nodes.length > 1"
                  class="grip"
                  title="按住拖动调整优先级"
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
                  <div v-if="node.speed_metric && node.speed_metric.avg_return_bps > 0" class="node-speed num">
                    回程 {{ node.speed_metric.avg_return_mbps.toFixed(1) }}Mbps
                    <span v-if="node.speed_metric.avg_outbound_mbps > 0"> · 去程 {{ node.speed_metric.avg_outbound_mbps.toFixed(1) }}Mbps</span>
                    <span v-if="node.speed_metric.avg_return_rtt_ms >= 0"> · {{ node.speed_metric.avg_return_rtt_ms }}ms</span>
                    <span v-if="node.speed_metric.total_retrans >= 0"> · 重传 {{ node.speed_metric.total_retrans }}</span>
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
                  <label
                    v-if="group.strategy === 'balance'"
                    class="cap-pill"
                    title="该节点全局负载均衡流数上限（1–99 路，1 台观看设备计 1 路；修改后所有线路组同步生效）"
                    @pointerdown.stop
                    @click.stop
                  >
                    <span class="cap-cur num">{{ node.active_sessions || 0 }}/</span>
                    <input
                      type="number"
                      min="1"
                      max="99"
                      step="1"
                      inputmode="numeric"
                      class="cap-input num"
                      :value="node.max_balance_streams || 2"
                      :disabled="!!savingGroup"
                      aria-label="负载均衡流数上限"
                      @change="updateNodeBalanceCap(group.group, node, $event)"
                    />
                    <span class="cap-unit">路</span>
                  </label>
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
            <button type="button" @click="runLookup('8.8.8.8')">海外示例</button>
          </div>
        </form>

        <p v-if="testError" class="err">{{ testError }}</p>
        <div v-else-if="testResult" class="test-result">
          <div><span class="k">查询 IP</span><strong class="mono">{{ testResult.ip }}</strong></div>
          <div>
            <span class="k">归属地</span>
            <strong>
              {{ testResult.region || '未识别（请先下载地区库）' }}
              <span v-if="testResult.non_mainland_cn" class="tag-overseas">非中国大陆</span>
            </strong>
          </div>
          <div><span class="k">运营商</span><strong>{{ testResult.isp || '—' }}</strong></div>
          <div><span class="k">匹配线路</span><strong>{{ testResult.isp_label }}</strong></div>
          <div>
            <span class="k">命中节点</span>
            <strong :class="testResult.blocked ? 'bad' : 'good'">
              {{ testResult.blocked ? `${testResult.node_name} (403)` : testResult.node_name }}
            </strong>
            <span v-if="!testResult.blocked && testResult.public_url" class="mono faint"> ({{ testResult.public_url }})</span>
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

    <SpeedtestDrawer :open="speedtestOpen" @close="speedtestOpen = false" @completed="load(true)" />
    <IspProbeDrawer :open="ispProbeOpen" @close="ispProbeOpen = false" @saved="load(true)" />
  </div>
</template>

<style scoped>
.routes-page { display: flex; flex-direction: column; gap: 24px; }
.section { display: flex; flex-direction: column; gap: 14px; }
.section-head h2 { margin: 0; font-size: 16px; font-weight: 600; }
.section-head p { margin: 4px 0 0; font-size: 13px; }
.faint { color: var(--text-3); }
.mono { font-family: var(--font-mono); }

.guard-bar { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding: 18px 20px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow-card); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease); }
.guard-bar.active { border-color: color-mix(in srgb, var(--bad) 42%, var(--border)); background: color-mix(in srgb, var(--bad) 5%, var(--surface)); }
.guard-meta { display: flex; align-items: flex-start; gap: 12px; flex: 1; min-width: 0; }
.guard-icon { width: 18px; height: 18px; margin-top: 2px; color: var(--text-2); flex-shrink: 0; }
.guard-bar.active .guard-icon { color: var(--bad); }
.guard-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 14.5px; }
.guard-desc { margin: 5px 0 0; font-size: 12.5px; line-height: 1.55; }

.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
.group-card { display: flex; flex-direction: column; gap: 14px; padding: 20px 22px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow-card); transition: border-color var(--dur) var(--ease); }
.group-card.blocked { border-color: color-mix(in srgb, var(--bad) 38%, var(--border)); }
.group-card.skeleton { height: 180px; }
.group-card.skeleton span { display: block; width: 40%; height: 14px; border-radius: 6px; background: var(--surface-hover); }

.group-head { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.group-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.group-title h3 { margin: 0; font-size: 16px; font-weight: 700; letter-spacing: -0.015em; }
.group-controls { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.block-label { font-size: 12px; }
.active-chip { display: inline-flex; align-items: center; height: 23px; padding: 0 9px; border-radius: 999px; font-size: 11.5px; background: var(--good-soft); color: var(--good); font-weight: 600; }
.blocked-chip { display: inline-flex; align-items: center; height: 23px; padding: 0 9px; border-radius: 999px; font-size: 11.5px; background: var(--bad-soft); color: var(--bad); font-weight: 600; }
.block-banner { margin: 0; padding: 10px 12px; border-radius: var(--radius-sm); background: var(--bad-soft); color: var(--bad); font-size: 12px; line-height: 1.5; }
.node-list.dimmed { opacity: 0.48; }
.tag-overseas { display: inline-flex; align-items: center; margin-left: 6px; padding: 1px 7px; border-radius: 999px; font-size: 11px; font-weight: 600; background: var(--warn-soft); color: var(--warn); }
.test-result .bad { color: var(--bad); }

.node-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 8px; }
.node-row { display: flex; align-items: center; gap: 10px; padding: 11px 13px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--surface-2); user-select: none; transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), opacity var(--dur) var(--ease); }
.node-row.movable { cursor: grab; touch-action: pan-y; }
.node-row.active { border-color: var(--accent-border); background: linear-gradient(90deg, var(--accent-soft) 0%, var(--surface) 75%); }
.node-row.off { opacity: 0.55; }
.node-row.tripped { border-color: color-mix(in srgb, var(--bad) 42%, var(--border)); background: color-mix(in srgb, var(--bad) 5%, var(--surface-2)); }
.node-row.dragging { opacity: 0.24; border-style: dashed; border-color: var(--accent); background: var(--accent-soft); cursor: grabbing; }

.grip { display: inline-flex; align-items: center; color: var(--text-3); touch-action: none; cursor: grab; }
.grip svg { width: 15px; height: 15px; }
.rank { display: inline-grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--surface); border: 1px solid var(--border); font-size: 11.5px; font-weight: 700; color: var(--text-2); }
.node-row.active .rank { background: var(--accent); border-color: var(--accent); color: #ffffff; }

.node-info { flex: 1; min-width: 0; }
.node-name { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; font-size: 13.5px; font-weight: 600; }
.node-speed { margin-top: 3px; font-size: 11.5px; color: var(--good); font-weight: 600; }
.node-url { margin-top: 2px; font-size: 11.5px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.strategy-bar { display: flex; align-items: center; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
.strategy-bar.dimmed { opacity: 0.48; pointer-events: none; }
.seg { display: inline-flex; padding: 3px; border: 1px solid var(--border); border-radius: 999px; background: var(--surface-2); }
.seg button { height: 26px; padding: 0 11px; border: 0; border-radius: 999px; background: transparent; color: var(--text-2); font-size: 12px; font-weight: 500; cursor: pointer; transition: all var(--dur) var(--ease); }
.seg button.on { background: var(--surface); color: var(--text); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08); }
.strategy-hint { margin: 0; font-size: 11.5px; line-height: 1.5; }

.badge-active { font-size: 10.5px; padding: 1px 7px; border-radius: 999px; background: var(--accent-soft); color: var(--accent); font-weight: 600; }
.status-dot { font-size: 11.5px; color: var(--text-3); }
.status-dot.good { color: var(--good); }
.status-dot.warn { color: var(--warn); }
.status-dot.bad { color: var(--bad); }

.node-ctrl { display: flex; align-items: center; gap: 8px; flex-shrink: 0; }
.cap-pill { display: inline-flex; align-items: center; gap: 2px; height: 26px; padding: 0 7px; border: 1px solid var(--border); border-radius: 999px; background: var(--surface); font-size: 11.5px; color: var(--text-2); cursor: default; }
.cap-pill:focus-within { border-color: var(--accent); box-shadow: 0 0 0 2px var(--accent-soft); }
.cap-cur { color: var(--text-3); font-weight: 600; }
.cap-input { width: 32px; border: 0; background: transparent; color: var(--text); font-size: 12px; font-weight: 700; text-align: center; outline: none; padding: 0; -moz-appearance: textfield; }
.cap-input::-webkit-outer-spin-button,
.cap-input::-webkit-inner-spin-button { -webkit-appearance: none; margin: 0; }
.cap-unit { color: var(--text-3); }
.icon-btn { display: grid; place-items: center; width: 28px; height: 28px; border: 0; border-radius: 50%; background: transparent; color: var(--text-3); cursor: pointer; }
.icon-btn:hover { background: var(--surface); color: var(--accent); }
.icon-btn svg { width: 14px; height: 14px; }

.card-panel { padding: 20px 22px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow-card); }

.test-bar { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.test-input { flex: 1; min-width: 240px; }

.samples { display: flex; gap: 6px; flex-wrap: wrap; }
.samples button { height: 29px; padding: 0 12px; border: 1px solid var(--border); border-radius: 999px; background: var(--surface-2); color: var(--text-2); font-size: 12px; font-weight: 500; cursor: pointer; transition: all var(--dur) var(--ease); }
.samples button:hover { border-color: var(--accent); background: var(--accent-soft); color: var(--accent); }

.test-result { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; padding: 14px 16px; border-radius: var(--radius-sm); background: var(--surface-2); border: 1px solid var(--border); font-size: 13px; }
.test-result > div { display: flex; flex-direction: column; gap: 4px; }
.test-result .k { font-size: 11.5px; color: var(--text-3); }
.test-result .good { color: var(--good); }
.err { margin: 0; font-size: 13px; color: var(--bad); }

.db-title { display: flex; align-items: flex-start; gap: 10px; }
.db-icon { width: 18px; height: 18px; margin-top: 2px; color: var(--accent); flex-shrink: 0; }
.db-title h2 { margin: 0; font-size: 16px; font-weight: 700; }
.db-title p { margin: 4px 0 0; font-size: 13px; }

.db-rows { display: flex; flex-direction: column; gap: 10px; }
.db-row { display: flex; align-items: center; justify-content: space-between; gap: 14px; padding: 14px 16px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--surface-2); flex-wrap: wrap; }
.db-meta { flex: 1; min-width: 240px; }
.db-name { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 14px; }
.size-hint { font-size: 12px; }
.pill { display: inline-flex; align-items: center; height: 21px; padding: 0 8px; border-radius: 999px; font-size: 11.5px; font-weight: 600; }
.pill.good { background: var(--good-soft); color: var(--good); }
.pill.muted { background: var(--surface); color: var(--text-3); }
.db-sub { margin: 4px 0 0; font-size: 12.5px; }
.db-actions { display: flex; align-items: center; gap: 8px; }

.mirror-note { padding: 12px 14px; border-radius: var(--radius-sm); background: var(--surface-2); font-size: 12px; color: var(--text-2); line-height: 1.6; }
.mirror-note p { margin: 0 0 6px; }
.mirror-note ol { margin: 0; padding-left: 18px; display: flex; flex-direction: column; gap: 2px; color: var(--text-3); word-break: break-all; }

.empty { padding: 48px 24px; text-align: center; border: 1px dashed var(--border); border-radius: var(--radius); color: var(--text-2); }

.list-move { transition: transform 180ms var(--ease); }

@media (max-width: 960px) {
  .grid { grid-template-columns: 1fr; }
}
</style>
