<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ChevronDown, MonitorSmartphone, X } from '@lucide/vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import AnalyticsTab from '@/components/dashboard/AnalyticsTab.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import { dashboardApi, shareApi, type DashboardInsights, type LiveShareSession, type ShareUser, type TrafficSnapshot, type TrendResponse } from '@/api'
import { errorMessage, toast } from '@/stores/toast'
import { formatBytes, relativeTime } from '@/utils/format'

const router = useRouter()
const snapshot = ref<TrafficSnapshot | null>(null)
const insights = ref<DashboardInsights | null>(null)
const shares = ref<ShareUser[]>([])
const liveSessions = ref<LiveShareSession[]>([])
const kickingSessionId = ref('')
const showAllSessions = ref(false)
const trend = ref<TrendResponse | null>(null)
const range = ref('realtime')
const siteId = ref('all')
const metric = ref<'speed' | 'volume'>('speed')
const dashTab = ref<'overview' | 'analytics'>('overview')

const RANGES = [
  { key: 'realtime', label: '实时' },
  { key: 'hour', label: '1 小时' },
  { key: 'day', label: '24 小时' },
  { key: '7d', label: '7 天' },
  { key: 'month', label: '30 天' },
]

const loadSnapshot = async () => {
  const [snap, live] = await Promise.allSettled([dashboardApi.snapshot(), shareApi.liveSessions()])
  if (snap.status === 'fulfilled') snapshot.value = snap.value
  if (live.status === 'fulfilled') liveSessions.value = live.value
}
async function kickLiveSession(item: LiveShareSession) {
  if (!item.session_id || !item.share_user_id || kickingSessionId.value) return
  kickingSessionId.value = item.session_id
  try {
    await shareApi.kickSession(item.share_user_id, item.session_id)
    toast.success(`已断开 ${item.username} 的「${item.device_name || '该设备'}」`)
    liveSessions.value = liveSessions.value.filter(s => s.session_id !== item.session_id)
  } catch (error) {
    toast.error(errorMessage(error, '断开失败'))
  } finally {
    kickingSessionId.value = ''
  }
}
const loadTrend = async () => {
  try {
    trend.value = await dashboardApi.trends(siteId.value, range.value)
  } catch {
    trend.value = null
  }
}
async function loadSide() {
  const [i, s] = await Promise.allSettled([dashboardApi.insights(), shareApi.list()])
  if (i.status === 'fulfilled') insights.value = i.value
  if (s.status === 'fulfilled') shares.value = s.value
}

let fast: ReturnType<typeof setInterval> | undefined
let slow: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  loadSnapshot()
  loadTrend()
  loadSide()
  fast = setInterval(() => {
    if (document.visibilityState !== 'visible') return
    loadSnapshot()
    if (range.value === 'realtime' || range.value === 'hour') loadTrend()
  }, 5000)
  slow = setInterval(() => {
    if (document.visibilityState !== 'visible') return
    loadSide()
    if (range.value !== 'realtime') loadTrend()
  }, 60000)
})
onBeforeUnmount(() => {
  clearInterval(fast)
  clearInterval(slow)
})
watch([range, siteId], loadTrend)

const rate = (bps: number) => formatBytes(bps) + '/s'
function uptime(seconds: number) {
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  return d ? `${d} 天 ${h} 小时` : h ? `${h} 小时 ${m} 分` : `${m} 分钟`
}
const timeFormat = computed(() => {
  const r = range.value
  return (ms: number) => {
    const d = new Date(ms)
    const pad = (n: number) => String(n).padStart(2, '0')
    if (r === '7d' || r === 'month') return `${d.getMonth() + 1}/${d.getDate()}`
    return `${pad(d.getHours())}:${pad(d.getMinutes())}${r === 'realtime' ? ':' + pad(d.getSeconds()) : ''}`
  }
})
const chart = computed(() => {
  const points = trend.value?.points || []
  const speed = metric.value === 'speed'
  return {
    labels: points.map(p => p.timestamp_ms),
    format: speed ? rate : (v: number) => formatBytes(v),
    series: [
      { name: '下行', color: 'var(--text)', values: points.map(p => (speed ? p.download_bps : p.bytes_out) || 0) },
      { name: '上行', color: 'var(--text-3)', values: points.map(p => (speed ? p.upload_bps : p.bytes_in) || 0) },
    ],
  }
})
const current = computed(() => {
  const points = trend.value?.points || []
  const last = points[points.length - 1]
  return { down: last?.download_bps || 0, up: last?.upload_bps || 0 }
})
const online = computed(() => liveSessions.value.length || shares.value.reduce((n, u) => n + (u.session_count || 0), 0))
const playingSessions = computed(() => liveSessions.value.filter(s => s.playing))
const playingCount = computed(() => playingSessions.value.length)
const displayedSessions = computed(() => (showAllSessions.value ? liveSessions.value : playingSessions.value))
const sites = computed(() => [...(snapshot.value?.live_sites || [])].sort((a, b) => (b.monthly_traffic || 0) - (a.monthly_traffic || 0)))
const maxRequests = computed(() => Math.max(1, ...(insights.value?.hourly_requests || [0])))
</script>

<template>
  <div>
    <PageHeader title="仪表盘" :description="snapshot ? `已运行 ${uptime(snapshot.uptime_seconds)}` : ' '" />

    <nav class="dash-tabs" role="tablist" aria-label="仪表盘视图">
      <button type="button" role="tab" :aria-selected="dashTab === 'overview'" :class="{ on: dashTab === 'overview' }" @click="dashTab = 'overview'">
        实时概览
      </button>
      <button type="button" role="tab" :aria-selected="dashTab === 'analytics'" :class="{ on: dashTab === 'analytics' }" @click="dashTab = 'analytics'">
        统计看板
      </button>
    </nav>

    <template v-if="dashTab === 'overview'">
    <dl class="stats">
      <div><dt>运行中站点</dt><dd class="num">{{ snapshot ? snapshot.running_sites : '—' }}<small>/ {{ snapshot?.total_sites ?? '—' }}</small></dd></div>
      <div><dt>本周期流量</dt><dd class="num">{{ snapshot ? formatBytes(snapshot.monthly_traffic) : '—' }}</dd></div>
      <div><dt>当前下行</dt><dd class="num">{{ rate(current.down) }}</dd></div>
      <div><dt>已登录设备</dt><dd class="num">{{ online }}<small>{{ playingCount ? `台 · ${playingCount} 播放中` : '台' }}</small></dd></div>
    </dl>

    <section class="section">
      <div class="section-head">
        <div class="tabs" role="tablist" aria-label="时间范围">
          <button v-for="r in RANGES" :key="r.key" type="button" role="tab" :aria-selected="range === r.key" :class="{ on: range === r.key }" @click="range = r.key">{{ r.label }}</button>
        </div>
        <div class="controls">
          <button type="button" class="link" :class="{ on: metric === 'speed' }" @click="metric = 'speed'">速度</button>
          <button type="button" class="link" :class="{ on: metric === 'volume' }" @click="metric = 'volume'">流量</button>
          <select v-model="siteId" class="select mini" aria-label="站点">
            <option value="all">全部站点</option>
            <option v-for="s in snapshot?.live_sites || []" :key="s.id" :value="String(s.id)">{{ s.name }}</option>
          </select>
        </div>
      </div>
      <div class="legend">
        <span><i class="sw down" />下行</span>
        <span><i class="sw up" />上行</span>
      </div>
      <AreaChart v-if="trend && trend.points.length" :labels="chart.labels" :series="chart.series" :format="chart.format" :time-format="timeFormat" :height="260" />
      <p v-else class="faint none">暂无数据</p>
    </section>

    <section v-if="liveSessions.length" class="section">
      <div class="live-head">
        <h2>
          正在观影
          <span v-if="playingCount" class="pill good">{{ playingCount }} 台播放中</span>
          <span v-else class="faint sub-count">当前无人播放</span>
        </h2>
        <button type="button" class="live-toggle" :class="{ open: showAllSessions }" @click="showAllSessions = !showAllSessions">
          <span>{{ showAllSessions ? '收起历史设备' : `查看全部 ${liveSessions.length} 台已登录设备` }}</span>
          <ChevronDown />
        </button>
      </div>
      <ul v-if="displayedSessions.length" class="live-list">
        <li v-for="s in displayedSessions" :key="s.session_id">
          <MonitorSmartphone class="dev-ico" />
          <div class="live-main">
            <div class="live-title">
              <strong class="live-user">{{ s.username }}</strong>
              <span class="live-device">{{ s.device_name || '未知设备' }}</span>
              <span v-if="s.draining" class="pill warn">观影后切换</span>
              <span v-else-if="s.playing" class="pill good">播放中{{ s.node_name ? ` · ${s.node_name}` : '' }}</span>
              <span v-else-if="s.node_name" class="pill">{{ s.node_name }}</span>
            </div>
            <div class="live-meta faint" :title="s.client_ip">
              {{ [s.site_name, s.client, s.client_region || s.client_ip].filter(Boolean).join(' · ') }}
            </div>
          </div>
          <span class="faint live-time">{{ relativeTime(s.last_seen_at) }}</span>
          <button
            type="button"
            class="kick-btn"
            :disabled="kickingSessionId === s.session_id"
            title="断开该设备"
            aria-label="断开该设备"
            @click="kickLiveSession(s)"
          >
            <X />
          </button>
        </li>
      </ul>
    </section>

    <div class="cols">
      <section class="section">
        <h2>站点</h2>
        <ul class="list">
          <li v-for="s in sites" :key="s.id" @click="router.push('/sites')">
            <span class="dot" :class="s.running ? 'good' : ''" />
            <span class="name">{{ s.name }}</span>
            <span class="faint num">{{ s.requests }} 次请求</span>
            <span class="num val">{{ formatBytes(s.monthly_traffic) }}</span>
            <UiProgress v-if="s.traffic_quota > 0" class="bar" :value="s.traffic_used" :max="s.traffic_quota" />
          </li>
          <li v-if="!sites.length" class="faint empty">还没有站点</li>
        </ul>
      </section>

      <section class="section">
        <h2>运行状态</h2>
        <dl class="kv">
          <div><dt>请求日志</dt><dd><span class="dot" :class="insights?.log_healthy ? 'good' : 'warn'" />{{ insights ? (insights.log_healthy ? '正常' : '写入异常') : '—' }}</dd></div>
          <div><dt>今日日志</dt><dd class="num">{{ insights?.log_count_today ?? '—' }}</dd></div>
          <div><dt>定时日报</dt><dd>{{ insights ? (insights.schedule_enabled ? insights.schedule_label || '已开启' : '未开启') : '—' }}</dd></div>
        </dl>
        <template v-if="insights?.hourly_requests?.length">
          <div class="bars" aria-label="最近 24 小时请求量">
            <span v-for="(v, i) in insights.hourly_requests" :key="i" :style="{ height: Math.max(3, (v / maxRequests) * 100) + '%' }" :title="`${v} 次`" />
          </div>
          <p class="faint caption">最近 24 小时请求</p>
        </template>
      </section>
    </div>
    </template>

    <AnalyticsTab v-else />
  </div>
</template>

<style scoped>
.dash-tabs { display: flex; gap: 22px; border-bottom: 1px solid var(--border); margin-bottom: 24px; }
.dash-tabs button { position: relative; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; cursor: pointer; }
.dash-tabs button:hover { color: var(--text-2); }
.dash-tabs button.on { color: var(--text); font-weight: 500; }
.dash-tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.stats { display: flex; gap: 48px; margin: 0 0 40px; flex-wrap: wrap; }
.stats div { display: flex; flex-direction: column; gap: 2px; }
.stats dt { font-size: 12.5px; color: var(--text-3); }
.stats dd { margin: 0; font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
.stats small { margin-left: 4px; font-size: 13px; font-weight: 400; color: var(--text-3); }
.section { margin-bottom: 40px; min-width: 0; }
.section-head { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); margin-bottom: 14px; }
.tabs { display: flex; gap: 22px; overflow-x: auto; scrollbar-width: none; }
.tabs::-webkit-scrollbar { display: none; }
.tabs button { position: relative; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; white-space: nowrap; cursor: pointer; }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.controls { display: flex; align-items: center; gap: 14px; }
.link { padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13px; cursor: pointer; }
.link.on { color: var(--text); }
.select.mini { width: auto; height: 28px; padding-left: 8px; font-size: 13px; border-color: transparent; }
.select.mini:hover { border-color: var(--border-strong); }
.legend { display: flex; gap: 16px; margin-bottom: 4px; font-size: 12px; color: var(--text-3); }
.legend span { display: inline-flex; align-items: center; gap: 6px; }
.sw { width: 10px; height: 2px; border-radius: 1px; }
.sw.down { background: var(--text); }
.sw.up { background: var(--text-3); }
.none { padding: 80px 0; text-align: center; font-size: 13.5px; }
.cols { display: grid; grid-template-columns: 1.5fr 1fr; gap: 56px; }
h2 { margin: 0 0 6px; padding-bottom: 10px; border-bottom: 1px solid var(--border); font-size: 13px; font-weight: 500; color: var(--text-2); display: flex; align-items: center; gap: 8px; }
.live-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding-bottom: 10px; border-bottom: 1px solid var(--border); }
.live-head h2 { margin: 0; padding: 0; border-bottom: 0; }
.live-toggle { display: inline-flex; align-items: center; gap: 4px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 12.5px; cursor: pointer; transition: color var(--dur) var(--ease); }
.live-toggle:hover { color: var(--text); }
.live-toggle svg { width: 14px; height: 14px; transition: transform 180ms var(--ease); }
.live-toggle.open svg { transform: rotate(180deg); }
.sub-count { font-size: 12px; font-weight: 400; }
.live-list { margin: 0; padding: 0; list-style: none; }
.live-list li { display: flex; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.dev-ico { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.live-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.live-title { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.live-user { font-weight: 600; }
.live-device { color: var(--text-2); }
.live-meta { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.live-time { font-size: 12px; white-space: nowrap; }
.pill { display: inline-flex; align-items: center; height: 19px; padding: 0 7px; border-radius: 999px; font-size: 11px; background: var(--surface); color: var(--text-2); }
.pill.good { background: var(--good-soft); color: var(--good); }
.pill.warn { background: var(--warn-soft); color: var(--warn); }
.kick-btn { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; flex: 0 0 26px; }
.kick-btn:hover:not(:disabled) { background: var(--surface-hover); color: var(--bad); }
.kick-btn:disabled { opacity: 0.4; cursor: default; }
.kick-btn svg { width: 14px; height: 14px; }
.list { margin: 0; padding: 0; list-style: none; }
.list li { display: grid; grid-template-columns: auto minmax(0, 1fr) auto auto; align-items: center; gap: 4px 12px; padding: 11px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; cursor: pointer; }
.list li:hover .name { text-decoration: underline; text-underline-offset: 3px; }
.list .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.list .faint { font-size: 12.5px; }
.list .val { font-weight: 500; }
.list .bar { grid-column: 2 / -1; }
.list .empty { display: block; cursor: default; }
.kv { margin: 0; }
.kv div { display: flex; justify-content: space-between; padding: 11px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.kv dt { color: var(--text-3); }
.kv dd { margin: 0; display: inline-flex; align-items: center; gap: 8px; }
.bars { display: flex; align-items: flex-end; gap: 3px; height: 56px; margin-top: 20px; }
.bars span { flex: 1; border-radius: 2px 2px 0 0; background: var(--text-3); opacity: 0.6; transition: height 400ms var(--ease); }
.bars span:hover { opacity: 1; background: var(--text); }
.caption { margin: 8px 0 0; font-size: 12px; }
@media (max-width: 960px) { .cols { grid-template-columns: minmax(0, 1fr); gap: 0; } }
@media (max-width: 720px) {
  .stats { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px 24px; margin-bottom: 28px; }
  .section-head { flex-direction: column; align-items: stretch; gap: 0; }
  .controls { justify-content: flex-end; padding: 6px 0; }
}
</style>
