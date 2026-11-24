<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { RefreshCw } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { dashboardApi, type DashboardAnalyticsResponse } from '@/api'
import { errorMessage, toast } from '@/stores/toast'
import { formatBytes } from '@/utils/format'

const data = ref<DashboardAnalyticsResponse | null>(null)
const loading = ref(true)

async function load() {
  loading.value = true
  try {
    data.value = await dashboardApi.analytics()
  } catch (err) {
    toast.error(errorMessage(err, '加载统计看板失败'))
  } finally {
    loading.value = false
  }
}

onMounted(load)

const maxDailyBytes = computed(() => {
  const pts = data.value?.daily_activity || []
  return Math.max(1, ...pts.map(p => p.traffic_bytes || 0))
})

function formatBps(bps: number): string {
  if (!bps || bps <= 0) return '0 bps'
  if (bps >= 1_000_000_000) return `${(bps / 1_000_000_000).toFixed(2)} Gbps`
  if (bps >= 1_000_000) return `${(bps / 1_000_000).toFixed(1)} Mbps`
  if (bps >= 1000) return `${(bps / 1000).toFixed(0)} Kbps`
  return `${bps} bps`
}

function userStatusLabel(status: string): string {
  switch (status) {
    case 'active':
      return '正常'
    case 'disabled':
      return '已停用'
    case 'expired':
      return '已过期'
    case 'quota':
      return '已超额'
    default:
      return status || '—'
  }
}
</script>

<template>
  <div class="analytics">
    <div class="an-bar">
      <p class="faint">
        汇总主机与从机分担流量比例、分享用户消耗排行、站点负载分布与近 14 天观影用量。
      </p>
      <UiButton size="sm" :loading="loading" @click="load"><RefreshCw />刷新统计</UiButton>
    </div>

    <div v-if="loading && !data" class="empty">正在加载统计数据...</div>

    <template v-else-if="data">
      <dl class="kpis">
        <div>
          <dt>累计总流量 ({{ data.billing_mode === 'outbound' ? '单向下行' : '双向计费' }})</dt>
          <dd class="num">{{ formatBytes(data.summary.total_traffic) }}</dd>
        </div>
        <div>
          <dt>本周期流量</dt>
          <dd class="num">{{ formatBytes(data.summary.monthly_traffic) }}</dd>
        </div>
        <div>
          <dt>从机分担比例</dt>
          <dd class="num">
            {{ data.summary.offload_percent.toFixed(1) }}%
            <small>{{ formatBytes(data.summary.edge_traffic) }} · {{ data.summary.active_edge_nodes }}/{{ data.summary.total_edge_nodes }} 在线</small>
          </dd>
        </div>
        <div>
          <dt>实时观影 / 会话</dt>
          <dd class="num">
            {{ data.summary.active_streams }} 流
            <small>/ {{ data.summary.online_sessions }} 设备 · {{ data.summary.share_user_count }} 账号</small>
          </dd>
        </div>
      </dl>

      <div class="an-grid">
        <section class="panel">
          <header class="panel-head">
            <h3>主机 vs 从机卸载分布</h3>
            <span class="faint num">主机 {{ formatBytes(data.summary.master_traffic) }} · 从机 {{ formatBytes(data.summary.edge_traffic) }}</span>
          </header>
          <ul class="bar-list">
            <li v-for="node in data.node_breakdown" :key="node.node_id" class="bar-item">
              <div class="bar-top">
                <span class="bar-title">
                  <strong>{{ node.name }}</strong>
                  <span class="pill" :class="node.online ? 'good' : ''">{{ node.is_master ? '主机' : node.online ? '在线' : '离线' }}</span>
                  <span v-if="node.active_streams" class="faint num">{{ node.active_streams }} 流</span>
                  <span v-if="node.current_bps" class="faint num">{{ formatBps(node.current_bps) }}</span>
                </span>
                <span class="num val">
                  {{ formatBytes(node.traffic_used) }}
                  <small class="faint">({{ node.share_percent.toFixed(1) }}%)</small>
                </span>
              </div>
              <UiProgress :value="node.share_percent" :max="100" />
            </li>
          </ul>
        </section>

        <section class="panel">
          <header class="panel-head">
            <h3>站点流量与请求占比</h3>
            <span class="faint num">总请求 {{ data.summary.total_requests }} 次</span>
          </header>
          <ul v-if="data.site_breakdown.length" class="bar-list">
            <li v-for="site in data.site_breakdown" :key="site.id" class="bar-item">
              <div class="bar-top">
                <span class="bar-title">
                  <span class="dot" :class="site.running ? 'good' : ''" />
                  <strong>{{ site.name }}</strong>
                  <span class="faint num">{{ site.requests }} 请求</span>
                </span>
                <span class="num val">
                  {{ formatBytes(site.traffic_used) }}
                  <small class="faint">({{ site.share_percent.toFixed(1) }}%)</small>
                </span>
              </div>
              <UiProgress :value="site.share_percent" :max="100" />
            </li>
          </ul>
          <p v-else class="faint empty-sm">暂无站点流量数据</p>
        </section>
      </div>

      <div class="an-grid">
        <section class="panel">
          <header class="panel-head">
            <h3>分享账号流量 Top 10</h3>
            <span class="faint">按累计消耗流量排序</span>
          </header>
          <div v-if="data.top_share_users.length" class="table-wrap">
            <table class="an-table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>账号</th>
                  <th>状态</th>
                  <th class="num">播放 / 设备</th>
                  <th class="num">已用流量</th>
                  <th class="num">占比</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(u, idx) in data.top_share_users" :key="u.id">
                  <td class="num faint">{{ idx + 1 }}</td>
                  <td>
                    <strong>{{ u.username }}</strong>
                    <span v-if="u.note" class="faint note-tag"> · {{ u.note }}</span>
                  </td>
                  <td>
                    <span class="pill" :class="u.status === 'active' ? 'good' : 'warn'">{{ userStatusLabel(u.status) }}</span>
                  </td>
                  <td class="num">{{ u.playing_count }} / {{ u.session_count }}</td>
                  <td class="num">
                    {{ formatBytes(u.traffic_used) }}
                    <span v-if="u.traffic_quota > 0" class="faint">/ {{ formatBytes(u.traffic_quota) }}</span>
                  </td>
                  <td class="num">{{ u.share_percent.toFixed(1) }}%</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="faint empty-sm">还没有分享账号数据</p>
        </section>

        <section class="panel">
          <header class="panel-head">
            <h3>近 14 天每日观影流量</h3>
            <span class="faint">按日累计消耗（包含当前未落盘实时增量）</span>
          </header>
          <div class="daily-chart" aria-label="近 14 天每日流量">
            <div v-for="pt in data.daily_activity" :key="pt.date" class="day-col" :title="`${pt.date}: ${formatBytes(pt.traffic_bytes)} · ${pt.requests} 次请求`">
              <div class="day-track">
                <div class="day-fill" :style="{ height: `${pt.traffic_bytes > 0 ? Math.max(4, Math.round((pt.traffic_bytes / maxDailyBytes) * 100)) : 2}%` }" />
              </div>
              <span class="day-lbl num">{{ pt.label }}</span>
            </div>
          </div>
        </section>
      </div>
    </template>
  </div>
</template>

<style scoped>
.analytics { display: flex; flex-direction: column; gap: 22px; }
.an-bar { display: flex; align-items: center; justify-content: space-between; gap: 14px; flex-wrap: wrap; }
.an-bar p { margin: 0; font-size: 13px; }
.faint { color: var(--text-3); }

.kpis { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 18px; margin: 0; padding: 16px 18px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-elev); }
.kpis div { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.kpis dt { font-size: 12.5px; color: var(--text-3); }
.kpis dd { margin: 0; font-size: 20px; font-weight: 600; letter-spacing: -0.02em; }
.kpis small { display: block; margin-top: 2px; font-size: 12px; font-weight: 400; color: var(--text-3); }

.an-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px; }
.panel { display: flex; flex-direction: column; gap: 14px; padding: 16px 18px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-elev); min-width: 0; }
.panel-head { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; border-bottom: 1px solid var(--border); padding-bottom: 10px; }
.panel-head h3 { margin: 0; font-size: 14.5px; font-weight: 600; }
.panel-head span { font-size: 12px; }

.bar-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 12px; }
.bar-item { display: flex; flex-direction: column; gap: 6px; }
.bar-top { display: flex; align-items: center; justify-content: space-between; gap: 10px; font-size: 13px; }
.bar-title { display: inline-flex; align-items: center; gap: 7px; min-width: 0; flex-wrap: wrap; }
.val { font-weight: 500; white-space: nowrap; }

.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--text-3); }
.dot.good { background: var(--good); }
.pill { display: inline-flex; align-items: center; height: 19px; padding: 0 7px; border-radius: 999px; font-size: 11px; background: var(--surface); color: var(--text-2); }
.pill.good { background: var(--good-soft); color: var(--good); }
.pill.warn { background: var(--warn-soft); color: var(--warn); }

.table-wrap { overflow-x: auto; }
.an-table { width: 100%; border-collapse: collapse; font-size: 12.5px; }
.an-table th, .an-table td { padding: 8px 6px; text-align: left; border-bottom: 1px solid var(--border); white-space: nowrap; }
.an-table th { font-weight: 500; color: var(--text-3); }
.an-table th.num, .an-table td.num { text-align: right; font-variant-numeric: tabular-nums; }
.an-table tr:last-child td { border-bottom: 0; }
.note-tag { font-size: 11.5px; }

.daily-chart { display: grid; grid-template-columns: repeat(14, minmax(0, 1fr)); gap: 6px; align-items: end; height: 180px; padding-top: 12px; }
.day-col { display: flex; flex-direction: column; align-items: center; gap: 6px; height: 100%; }
.day-track { flex: 1; width: 100%; display: flex; align-items: flex-end; }
.day-fill { width: 100%; border-radius: 3px 3px 0 0; background: var(--text); opacity: 0.72; transition: opacity var(--dur) var(--ease), height 300ms var(--ease); }
.day-col:hover .day-fill { opacity: 1; }
.day-lbl { font-size: 10.5px; color: var(--text-3); writing-mode: horizontal-tb; }

.empty, .empty-sm { padding: 32px 12px; text-align: center; font-size: 13px; color: var(--text-3); }

@media (max-width: 960px) {
  .kpis { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .an-grid { grid-template-columns: 1fr; }
}
@media (max-width: 600px) {
  .day-lbl { font-size: 9px; }
}
</style>
