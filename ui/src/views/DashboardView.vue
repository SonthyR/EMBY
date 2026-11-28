<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Activity, ChevronDown, MonitorSmartphone, Radio, Server, X } from '@lucide/vue'
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
function playbackTime(seconds: number): string {
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor(seconds / 60) % 60
  const remainder = seconds % 60
  const pad = (value: number) => String(value).padStart(2, '0')
  return hours ? `${hours}:${pad(minutes)}:${pad(remainder)}` : `${pad(minutes)}:${pad(remainder)}`
}
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
      { name: '下行', color: 'var(--accent)', values: points.map(p => (speed ? p.download_bps : p.bytes_out) || 0) },
      { name: '上行', color: 'var(--good)', values: points.map(p => (speed ? p.upload_bps : p.bytes_in) || 0) },
    ],
  }
})
const current = computed(() => {
  const points = trend.value?.points || []
  const last = points[points.length - 1]
  return { down: last?.download_bps || 0, up: last?.upload_bps || 0 }
})
const online = computed(() => liveSessions.value.length || shares.value.reduce((n, u) => n + (u.session_count || 0), 0))
const watchingSessions = computed(() => liveSessions.value.filter(s => s.playing))
const pausedCount = computed(() => watchingSessions.value.filter(s => s.now_playing_paused).length)
const playingCount = computed(() => watchingSessions.value.length - pausedCount.value)
const displayedSessions = computed(() => (showAllSessions.value ? liveSessions.value : watchingSessions.value))
const sites = computed(() => [...(snapshot.value?.live_sites || [])].sort((a, b) => (b.monthly_traffic || 0) - (a.monthly_traffic || 0)))
const maxRequests = computed(() => Math.max(1, ...(insights.value?.hourly_requests || [0])))
</script>

<template>
  <div>
    <PageHeader title="仪表盘" :description="snapshot ? `稳定运行 ${uptime(snapshot.uptime_seconds)} · 实时带宽与播放监控` : '实时带宽与播放监控'">
      <nav class="seg" role="tablist" aria-label="仪表盘视图">
        <button type="button" role="tab" :aria-selected="dashTab === 'overview'" :class="{ on: dashTab === 'overview' }" @click="dashTab = 'overview'">
          实时概览
        </button>
        <button type="button" role="tab" :aria-selected="dashTab === 'analytics'" :class="{ on: dashTab === 'analytics' }" @click="dashTab = 'analytics'">
          统计看板
        </button>
      </nav>
    </PageHeader>

    <template v-if="dashTab === 'overview'">
      <dl class="kpi-grid">
        <div class="kpi-card">
          <dt><span class="dot good" />运行中站点</dt>
          <dd class="num">{{ snapshot ? snapshot.running_sites : '—' }}<small>/ {{ snapshot?.total_sites ?? '—' }}</small></dd>
        </div>
        <div class="kpi-card">
          <dt>本周期流量</dt>
          <dd class="num">{{ snapshot ? formatBytes(snapshot.monthly_traffic) : '—' }}</dd>
        </div>
        <div class="kpi-card highlight">
          <dt>当前下行速率</dt>
          <dd class="num">{{ rate(current.down) }}<small>上行 {{ rate(current.up) }}</small></dd>
        </div>
        <div class="kpi-card">
          <dt>已登录设备</dt>
          <dd class="num">{{ online }}<small>{{ watchingSessions.length ? `台 · ${watchingSessions.length} 观影会话` : '台在线' }}</small></dd>
        </div>
      </dl>

      <div class="bento-main">
        <section class="card chart-card">
          <div class="card-head">
            <div class="card-title-group">
              <span class="eyebrow">REALTIME BANDWIDTH</span>
              <div class="live-nums">
                <span class="big-val num">{{ rate(current.down) }}</span>
                <span class="legend">
                  <span><i class="sw down" />下行</span>
                  <span><i class="sw up" />上行 {{ rate(current.up) }}</span>
                </span>
              </div>
            </div>
            <div class="controls">
              <div class="seg compact" role="tablist" aria-label="指标切换">
                <button type="button" :class="{ on: metric === 'speed' }" @click="metric = 'speed'">速度</button>
                <button type="button" :class="{ on: metric === 'volume' }" @click="metric = 'volume'">流量</button>
              </div>
              <select v-model="siteId" class="select mini" aria-label="站点">
                <option value="all">全部站点</option>
                <option v-for="s in snapshot?.live_sites || []" :key="s.id" :value="String(s.id)">{{ s.name }}</option>
              </select>
            </div>
          </div>

          <div class="range-bar" role="tablist" aria-label="时间范围">
            <button
              v-for="r in RANGES"
              :key="r.key"
              type="button"
              role="tab"
              :aria-selected="range === r.key"
              :class="{ on: range === r.key }"
              @click="range = r.key"
            >
              {{ r.label }}
            </button>
          </div>

          <AreaChart v-if="trend && trend.points.length" :labels="chart.labels" :series="chart.series" :format="chart.format" :time-format="timeFormat" :height="250" />
          <p v-else class="faint none">暂无趋势采样数据</p>
        </section>

        <section class="card live-card">
          <div class="live-head">
            <h2>
              <Radio class="h-ico" />
              <span>正在观影</span>
              <span v-if="playingCount" class="pill good">{{ playingCount }} 播放中</span>
              <span v-if="pausedCount" class="pill warn">{{ pausedCount }} 已暂停</span>
            </h2>
            <button v-if="liveSessions.length" type="button" class="live-toggle" :class="{ open: showAllSessions }" @click="showAllSessions = !showAllSessions">
              <span>{{ showAllSessions ? '仅看播放' : `全部 ${liveSessions.length}` }}</span>
              <ChevronDown />
            </button>
          </div>

          <ul v-if="displayedSessions.length" class="live-list">
            <li v-for="s in displayedSessions" :key="s.session_id">
              <span class="dev-badge"><MonitorSmartphone /></span>
              <div class="live-main">
                <div class="live-title">
                  <RouterLink class="live-user" :to="{ path: '/share', query: { user: String(s.share_user_id) } }">
                    {{ s.username }}
                  </RouterLink>
                  <span class="live-device">{{ s.device_name || '未知设备' }}</span>
                </div>
                <div v-if="s.playing && (s.now_playing_title || s.now_playing_position_seconds != null)" class="live-media">
                  <span v-if="s.now_playing_title" class="live-media-title" :title="s.now_playing_title">{{ s.now_playing_title }}</span>
                  <span v-if="s.now_playing_position_seconds != null" class="live-media-time num">
                    {{ playbackTime(s.now_playing_position_seconds) }}{{ s.now_playing_duration_seconds ? ` / ${playbackTime(s.now_playing_duration_seconds)}` : '' }}
                  </span>
                </div>
                <div class="live-tags">
                  <span v-if="s.draining" class="pill warn">观影后切换</span>
                  <span v-if="s.playing && s.now_playing_paused" class="pill warn">已暂停{{ s.node_name ? ` · ${s.node_name}` : '' }}</span>
                  <span v-else-if="s.playing && !s.draining" class="pill good">播放中{{ s.node_name ? ` · ${s.node_name}` : '' }}</span>
                  <span v-else-if="!s.playing && !s.draining && s.node_name" class="pill">{{ s.node_name }}</span>
                  <span class="faint live-meta" :title="s.client_ip">
                    {{ [s.site_name, s.client_region || s.client_ip].filter(Boolean).join(' · ') }}
                  </span>
                </div>
              </div>
              <div class="live-right">
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
              </div>
            </li>
          </ul>
          <div v-else class="live-empty">
            <MonitorSmartphone class="empty-ico" />
            <p>{{ liveSessions.length ? '当前无人播放' : '暂无在线观影设备' }}</p>
            <button v-if="liveSessions.length && !showAllSessions" type="button" class="text-btn" @click="showAllSessions = true">
              查看全部 {{ liveSessions.length }} 台已登录设备
            </button>
          </div>
        </section>
      </div>

      <div class="bento-sub-grid">
        <section class="card">
          <div class="sub-head">
            <h2><Server class="h-ico" />站点实时负载</h2>
            <button type="button" class="text-btn" @click="router.push('/sites')">管理站点 →</button>
          </div>
          <ul class="list">
            <li v-for="s in sites" :key="s.id" @click="router.push('/sites')">
              <span class="dot" :class="s.running ? 'good' : ''" />
              <span class="name">{{ s.name }}</span>
              <span class="faint num">{{ s.requests }} 次请求</span>
              <span class="num val">{{ formatBytes(s.monthly_traffic) }}</span>
              <UiProgress v-if="s.traffic_quota > 0" class="bar" :value="s.traffic_used" :max="s.traffic_quota" />
            </li>
            <li v-if="!sites.length" class="faint empty">还没有配置源站</li>
          </ul>
        </section>

        <section class="card">
          <div class="sub-head">
            <h2><Activity class="h-ico" />系统健康与 24 小时请求</h2>
          </div>
          <dl class="kv">
            <div><dt>请求日志写入</dt><dd><span class="dot" :class="insights?.log_healthy ? 'good' : 'warn'" />{{ insights ? (insights.log_healthy ? '正常' : '写入异常') : '—' }}</dd></div>
            <div><dt>今日累计日志</dt><dd class="num">{{ insights?.log_count_today ?? '—' }}</dd></div>
            <div><dt>定时推送日报</dt><dd>{{ insights ? (insights.schedule_enabled ? insights.schedule_label || '已开启' : '未开启') : '—' }}</dd></div>
          </dl>
          <template v-if="insights?.hourly_requests?.length">
            <div class="bars" aria-label="最近 24 小时请求量">
              <span v-for="(v, i) in insights.hourly_requests" :key="i" :style="{ height: Math.max(6, (v / maxRequests) * 100) + '%' }" :title="`${v} 次请求`" />
            </div>
            <p class="faint caption">最近 24 小时请求分布</p>
          </template>
        </section>
      </div>
    </template>

    <AnalyticsTab v-else />
  </div>
</template>

<style scoped>
.seg {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 3px;
  border-radius: 999px;
  background: var(--surface-2);
  border: 1px solid var(--border);
}
.seg button {
  height: 30px;
  padding: 0 14px;
  border: 0;
  border-radius: 999px;
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: background var(--dur) var(--ease), color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
}
.seg button:hover { color: var(--text); }
.seg button.on {
  background: var(--surface);
  color: var(--text);
  font-weight: 600;
  box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08);
}
.seg.compact button { height: 26px; padding: 0 11px; font-size: 12px; }

.kpi-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 14px;
  margin: 0 0 16px;
}
.kpi-card {
  padding: 18px 20px;
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  display: flex;
  flex-direction: column;
  gap: 8px;
  min-width: 0;
}
.kpi-card.highlight {
  border-color: var(--accent-border);
  background: linear-gradient(180deg, color-mix(in srgb, var(--accent) 5%, var(--surface)) 0%, var(--surface) 100%);
}
.kpi-card dt {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  font-size: 12.5px;
  font-weight: 500;
  color: var(--text-2);
}
.kpi-card dd {
  margin: 0;
  font-size: 25px;
  font-weight: 700;
  letter-spacing: -0.03em;
  line-height: 1.15;
}
.kpi-card small {
  display: inline-block;
  margin-left: 6px;
  font-size: 12px;
  font-weight: 500;
  color: var(--text-3);
  letter-spacing: 0;
}

.bento-main {
  display: grid;
  grid-template-columns: minmax(0, 1.85fr) minmax(0, 1fr);
  gap: 16px;
  margin-bottom: 16px;
}
.bento-sub-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.35fr) minmax(0, 1fr);
  gap: 16px;
}

.card {
  padding: 20px 22px;
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.card-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}
.card-title-group { display: flex; flex-direction: column; gap: 4px; }
.eyebrow { font-size: 11px; font-weight: 600; letter-spacing: 0.08em; color: var(--text-3); }
.live-nums { display: flex; align-items: baseline; gap: 14px; flex-wrap: wrap; }
.big-val { font-size: 26px; font-weight: 700; letter-spacing: -0.03em; }
.legend { display: inline-flex; gap: 12px; font-size: 12px; color: var(--text-2); }
.legend span { display: inline-flex; align-items: center; gap: 5px; }
.sw { width: 8px; height: 8px; border-radius: 50%; }
.sw.down { background: var(--accent); }
.sw.up { background: var(--good); }

.controls { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.select.mini { width: auto; min-width: 116px; height: 32px; padding-left: 10px; font-size: 12.5px; border-radius: 999px; }

.range-bar {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin-bottom: 10px;
  overflow-x: auto;
  scrollbar-width: none;
}
.range-bar::-webkit-scrollbar { display: none; }
.range-bar button {
  height: 27px;
  padding: 0 11px;
  border: 1px solid transparent;
  border-radius: 999px;
  background: transparent;
  color: var(--text-3);
  font-size: 12.5px;
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.range-bar button:hover { color: var(--text); background: var(--surface-2); }
.range-bar button.on {
  background: var(--accent-soft);
  border-color: var(--accent-border);
  color: var(--accent);
  font-weight: 600;
}

.none { padding: 84px 0; text-align: center; font-size: 13.5px; margin: 0; }

.live-head,
.sub-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  margin-bottom: 12px;
}
h2 {
  margin: 0;
  font-size: 15px;
  font-weight: 700;
  letter-spacing: -0.015em;
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
.h-ico { width: 16px; height: 16px; color: var(--accent); stroke-width: 2; }
.live-toggle,
.text-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 0;
  border: 0;
  background: transparent;
  color: var(--text-3);
  font-size: 12.5px;
  font-weight: 500;
  cursor: pointer;
  transition: color var(--dur) var(--ease);
}
.live-toggle:hover,
.text-btn:hover { color: var(--accent); }
.live-toggle svg { width: 14px; height: 14px; transition: transform 180ms var(--ease); }
.live-toggle.open svg { transform: rotate(180deg); }

.live-list {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 318px;
  overflow-y: auto;
}
.live-list li {
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  border: 1px solid var(--border);
  font-size: 13px;
}
.dev-badge {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 10px;
  background: var(--surface);
  border: 1px solid var(--border);
  color: var(--text-2);
  flex: 0 0 32px;
}
.dev-badge svg { width: 15px; height: 15px; }
.live-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 3px; }
.live-title { display: flex; align-items: baseline; gap: 6px; min-width: 0; }
.live-user { color: inherit; font-weight: 600; white-space: nowrap; text-decoration: none; }
.live-user:hover, .live-user:focus-visible { text-decoration: underline; }
.live-device { color: var(--text-2); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.live-media { display: flex; align-items: baseline; gap: 8px; min-width: 0; font-size: 13px; }
.live-media-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.live-media-time { flex-shrink: 0; color: var(--text-2); font-size: 12px; white-space: nowrap; }
.live-tags { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.live-meta { font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.live-right { display: flex; align-items: center; gap: 6px; flex: 0 0 auto; }
.live-time { font-size: 11.5px; white-space: nowrap; }

.live-empty {
  flex: 1;
  min-height: 220px;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  color: var(--text-3);
  font-size: 13px;
}
.empty-ico { width: 28px; height: 28px; opacity: 0.45; }
.live-empty p { margin: 0; }

.pill {
  display: inline-flex;
  align-items: center;
  height: 20px;
  padding: 0 8px;
  border-radius: 999px;
  font-size: 11px;
  font-weight: 600;
  background: var(--surface);
  border: 1px solid var(--border);
  color: var(--text-2);
}
.pill.good { background: var(--good-soft); border-color: transparent; color: var(--good); }
.pill.warn { background: var(--warn-soft); border-color: transparent; color: var(--warn); }

.kick-btn {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
}
.kick-btn:hover:not(:disabled) { background: var(--bad-soft); color: var(--bad); }
.kick-btn:disabled { opacity: 0.4; cursor: default; }
.kick-btn svg { width: 14px; height: 14px; }

.list { margin: 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 6px; }
.list li {
  display: grid;
  grid-template-columns: auto minmax(0, 1fr) auto auto;
  align-items: center;
  gap: 6px 12px;
  padding: 11px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  border: 1px solid transparent;
  font-size: 13.5px;
  cursor: pointer;
  transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.list li:hover { border-color: var(--border-strong); background: var(--surface-hover); }
.list .name { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.list .faint { font-size: 12px; }
.list .val { font-weight: 600; }
.list .bar { grid-column: 2 / -1; }
.list .empty { display: block; cursor: default; text-align: center; padding: 28px 12px; background: transparent; }

.kv { margin: 0; display: flex; flex-direction: column; gap: 8px; }
.kv div {
  display: flex;
  justify-content: space-between;
  align-items: center;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: 13px;
}
.kv dt { color: var(--text-2); }
.kv dd { margin: 0; font-weight: 600; display: inline-flex; align-items: center; gap: 8px; }

.bars {
  display: flex;
  align-items: flex-end;
  gap: 4px;
  height: 76px;
  margin-top: 18px;
  padding: 10px 10px 0;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
}
.bars span {
  flex: 1;
  border-radius: 4px 4px 0 0;
  background: color-mix(in srgb, var(--accent) 55%, var(--surface-hover));
  transition: height 400ms var(--ease), background var(--dur) var(--ease);
}
.bars span:hover { background: var(--accent); }
.caption { margin: 8px 0 0; font-size: 12px; }

@media (max-width: 1024px) {
  .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .bento-main,
  .bento-sub-grid { grid-template-columns: minmax(0, 1fr); }
}
@media (max-width: 640px) {
  .kpi-card { padding: 14px 16px; }
  .kpi-card dd { font-size: 21px; }
  .card { padding: 16px; }
}
</style>
