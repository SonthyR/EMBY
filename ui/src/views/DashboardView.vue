<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Server, HardDriveDownload, Activity, Clock, Users, ScrollText, CalendarClock } from '@lucide/vue'
import AreaChart from '@/components/charts/AreaChart.vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { dashboardApi, shareApi, type DashboardInsights, type ShareUser, type TrafficSnapshot, type TrendResponse } from '@/api'
import { formatBytes } from '@/utils/format'

const snapshot = ref<TrafficSnapshot | null>(null)
const insights = ref<DashboardInsights | null>(null)
const shares = ref<ShareUser[]>([])
const trend = ref<TrendResponse | null>(null)
const range = ref('realtime')
const siteId = ref('all')
const metric = ref<'speed' | 'volume'>('speed')

const RANGES = [
  { key: 'realtime', label: '实时' },
  { key: 'hour', label: '1 小时' },
  { key: 'day', label: '24 小时' },
  { key: '7d', label: '7 天' },
  { key: 'month', label: '30 天' },
]

async function loadSnapshot() {
  try {
    snapshot.value = await dashboardApi.snapshot()
  } catch {
    /* keep last */
  }
}
async function loadTrend() {
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
  slow = setInterval(() => document.visibilityState === 'visible' && (loadSide(), range.value !== 'realtime' && loadTrend()), 60000)
})
onBeforeUnmount(() => {
  clearInterval(fast)
  clearInterval(slow)
})
watch([range, siteId], loadTrend)

function formatRate(bps: number) {
  return formatBytes(bps) + '/s'
}
function formatUptime(seconds: number) {
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
    if (r === '7d' || r === 'month') return `${d.getMonth() + 1}/${d.getDate()}${r === '7d' ? ' ' + pad(d.getHours()) + '时' : ''}`
    return `${pad(d.getHours())}:${pad(d.getMinutes())}${r === 'realtime' ? ':' + pad(d.getSeconds()) : ''}`
  }
})
const chart = computed(() => {
  const points = trend.value?.points || []
  const labels = points.map(p => p.timestamp_ms)
  if (metric.value === 'speed') {
    return {
      labels,
      format: formatRate,
      series: [
        { name: '下行', color: '#6e9bff', values: points.map(p => p.download_bps || 0) },
        { name: '上行', color: '#4fd1a1', values: points.map(p => p.upload_bps || 0) },
      ],
    }
  }
  return {
    labels,
    format: (v: number) => formatBytes(v),
    series: [
      { name: '下行', color: '#6e9bff', values: points.map(p => p.bytes_out || 0) },
      { name: '上行', color: '#4fd1a1', values: points.map(p => p.bytes_in || 0) },
    ],
  }
})
const currentSpeed = computed(() => {
  const points = trend.value?.points || []
  const last = points[points.length - 1]
  return last ? { down: last.download_bps || 0, up: last.upload_bps || 0 } : { down: 0, up: 0 }
})
const shareOnline = computed(() => shares.value.reduce((n, u) => n + (u.session_count || 0), 0))
const sites = computed(() => [...(snapshot.value?.live_sites || [])].sort((a, b) => (b.monthly_traffic || 0) - (a.monthly_traffic || 0)))
const maxRequests = computed(() => Math.max(1, ...(insights.value?.hourly_requests || [0])))
</script>

<template>
  <div class="page">
    <section class="kpis">
      <div class="kpi">
        <Server />
        <div><span>运行中站点</span><strong>{{ snapshot ? `${snapshot.running_sites} / ${snapshot.total_sites}` : '—' }}</strong></div>
      </div>
      <div class="kpi">
        <HardDriveDownload />
        <div><span>本周期流量</span><strong>{{ snapshot ? formatBytes(snapshot.monthly_traffic) : '—' }}</strong><small v-if="snapshot">累计 {{ formatBytes(snapshot.total_traffic) }}</small></div>
      </div>
      <div class="kpi">
        <Activity />
        <div><span>当前速度</span><strong>↓ {{ formatRate(currentSpeed.down) }}</strong><small>↑ {{ formatRate(currentSpeed.up) }}</small></div>
      </div>
      <div class="kpi">
        <Users />
        <div><span>分享账号在线</span><strong>{{ shareOnline }} 台</strong><small>{{ shares.length }} 个分享账号</small></div>
      </div>
    </section>

    <section class="panel">
      <header class="panel-head">
        <h2>流量趋势</h2>
        <div class="controls">
          <select v-model="siteId" class="select small" aria-label="站点">
            <option value="all">全部站点</option>
            <option v-for="s in snapshot?.live_sites || []" :key="s.id" :value="String(s.id)">{{ s.name }}</option>
          </select>
          <div class="seg">
            <button type="button" :class="{ on: metric === 'speed' }" @click="metric = 'speed'">速度</button>
            <button type="button" :class="{ on: metric === 'volume' }" @click="metric = 'volume'">流量</button>
          </div>
          <div class="seg">
            <button v-for="r in RANGES" :key="r.key" type="button" :class="{ on: range === r.key }" @click="range = r.key">{{ r.label }}</button>
          </div>
        </div>
      </header>
      <AreaChart v-if="trend && trend.points.length" :labels="chart.labels" :series="chart.series" :format="chart.format" :time-format="timeFormat" :height="240" />
      <p v-else class="faint empty">暂无数据</p>
    </section>

    <div class="cols">
      <section class="panel">
        <header class="panel-head"><h2>站点用量</h2></header>
        <ul class="sites">
          <li v-for="s in sites" :key="s.id">
            <div class="row">
              <UiBadge :tone="s.running ? 'good' : 'neutral'" dot>{{ s.running ? '运行' : '停止' }}</UiBadge>
              <strong>{{ s.name }}</strong>
              <span class="faint">{{ s.requests }} 次请求</span>
              <b>{{ formatBytes(s.monthly_traffic) }}</b>
            </div>
            <UiProgress v-if="s.traffic_quota > 0" :value="s.traffic_used" :max="s.traffic_quota" />
          </li>
          <li v-if="!sites.length" class="faint">还没有站点</li>
        </ul>
      </section>

      <section class="panel">
        <header class="panel-head"><h2>运行状态</h2></header>
        <ul class="status">
          <li><Clock /><span>已运行</span><b>{{ snapshot ? formatUptime(snapshot.uptime_seconds) : '—' }}</b></li>
          <li><ScrollText /><span>今日日志</span><b>{{ insights ? insights.log_count_today : '—' }}</b><UiBadge v-if="insights" :tone="insights.log_healthy ? 'good' : 'warn'">{{ insights.log_healthy ? '正常' : '异常' }}</UiBadge></li>
          <li><CalendarClock /><span>定时日报</span><b>{{ insights ? (insights.schedule_enabled ? insights.schedule_label || '已开启' : '未开启') : '—' }}</b></li>
        </ul>
        <div v-if="insights?.hourly_requests?.length" class="bars" aria-label="最近 24 小时请求量">
          <span v-for="(v, i) in insights.hourly_requests" :key="i" :style="{ height: Math.max(4, (v / maxRequests) * 100) + '%' }" :title="`${v} 次`" />
        </div>
        <p v-if="insights?.hourly_requests?.length" class="faint caption">最近 24 小时请求量</p>
      </section>
    </div>
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: 16px; }
.kpis { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.kpi { display: flex; align-items: center; gap: 12px; padding: 16px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); }
.kpi > svg { width: 20px; height: 20px; padding: 9px; box-sizing: content-box; border-radius: 11px; background: var(--accent-soft); color: var(--accent); flex: 0 0 auto; }
.kpi div { display: flex; flex-direction: column; min-width: 0; }
.kpi span { font-size: 12px; color: var(--text-3); }
.kpi strong { font-size: 19px; font-weight: 600; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.kpi small { font-size: 12px; color: var(--text-3); }
.panel { padding: 16px 18px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); min-width: 0; }
.panel-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 12px; }
.panel-head h2 { margin: 0; font-size: 15px; font-weight: 600; }
.controls { display: flex; gap: 8px; flex-wrap: wrap; }
.select.small { min-height: 32px; width: auto; padding-top: 4px; padding-bottom: 4px; font-size: 13px; }
.seg { display: flex; gap: 2px; padding: 3px; border: 1px solid var(--border); border-radius: 9px; background: var(--surface-2); }
.seg button { padding: 4px 10px; border: 0; border-radius: 6px; background: transparent; color: var(--text-2); font-size: 12px; cursor: pointer; white-space: nowrap; }
.seg button.on { background: var(--surface); color: var(--text); box-shadow: 0 1px 3px rgba(0, 0, 0, 0.18); font-weight: 500; }
.empty { padding: 60px 0; text-align: center; }
.cols { display: grid; grid-template-columns: 1.4fr 1fr; gap: 16px; }
.sites { display: flex; flex-direction: column; gap: 10px; margin: 0; padding: 0; list-style: none; }
.sites li { display: flex; flex-direction: column; gap: 6px; }
.sites .row { display: flex; align-items: center; gap: 10px; font-size: 13px; }
.sites strong { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sites .faint { margin-left: auto; font-size: 12px; white-space: nowrap; }
.sites b { font-weight: 600; white-space: nowrap; }
.status { display: flex; flex-direction: column; gap: 10px; margin: 0 0 14px; padding: 0; list-style: none; font-size: 13px; }
.status li { display: flex; align-items: center; gap: 8px; }
.status svg { width: 16px; height: 16px; color: var(--text-3); }
.status span { color: var(--text-2); }
.status b { margin-left: auto; font-weight: 500; }
.bars { display: flex; align-items: flex-end; gap: 3px; height: 64px; }
.bars span { flex: 1; border-radius: 3px 3px 0 0; background: var(--accent); opacity: 0.75; transition: height 400ms var(--ease); }
.caption { margin: 6px 0 0; font-size: 12px; }
@media (max-width: 1100px) { .kpis { grid-template-columns: repeat(2, minmax(0, 1fr)); } .cols { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 520px) { .kpi > svg { display: none; } .kpi { padding: 12px; } .kpi strong { font-size: 16px; } }
</style>
