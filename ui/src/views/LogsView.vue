<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Search, MoreHorizontal, Copy } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiMenu from '@/components/ui/UiMenu.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import { logsApi, type RequestLog } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText } from '@/utils/format'

const CATEGORIES: Record<string, string> = {
  playback: '播放',
  playback_sync: '播放进度',
  stream: '视频流',
  manifest: '播放列表',
  segment: '分片',
  image: '图片',
  metadata: '媒体信息',
  subtitle: '字幕',
  asset: '静态资源',
  websocket: 'WebSocket',
  api: 'API',
  auth: '登录',
  video: '视频',
}
const RANGES = [
  { key: '1h', label: '1 小时', ms: 3600e3 },
  { key: '24h', label: '24 小时', ms: 86400e3 },
  { key: '7d', label: '7 天', ms: 7 * 86400e3 },
  { key: '30d', label: '30 天', ms: 30 * 86400e3 },
]

const logs = ref<RequestLog[]>([])
const dropped = ref(0)
const loading = ref(true)
const query = ref('')
const category = ref('')
const status = ref('')
const range = ref('24h')
const live = ref(true)
const selected = ref<RequestLog | null>(null)

async function load() {
  const span = RANGES.find(r => r.key === range.value)?.ms || 86400e3
  try {
    const result = await logsApi.list({ from_ms: Date.now() - span, to_ms: Date.now() + 60e3, category: category.value, status: status.value, q: query.value.trim(), limit: 300 })
    logs.value = result.logs || []
    dropped.value = result.dropped_logs || 0
  } catch (error) {
    toast.error(errorMessage(error, '读取日志失败'))
  } finally {
    loading.value = false
  }
}
let debounce: ReturnType<typeof setTimeout> | undefined
watch([category, status, range], load)
watch(query, () => {
  clearTimeout(debounce)
  debounce = setTimeout(load, 300)
})
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => live.value && document.visibilityState === 'visible' && !selected.value && load(), 8000)
})
onBeforeUnmount(() => clearInterval(timer))

const pad = (n: number) => String(n).padStart(2, '0')
function time(ms: number) {
  const d = new Date(ms)
  const today = new Date()
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  return d.toDateString() === today.toDateString() ? hm : `${d.getMonth() + 1}/${d.getDate()} ${hm}`
}
const tone = (code: number) => (code >= 500 || code === 499 ? 'bad' : code >= 400 ? 'warn' : code >= 300 ? 'faint' : '')
const summary = computed(() => `${logs.value.length >= 300 ? '最近 300' : logs.value.length} 条`)

async function onMenu(key: string) {
  if (key === 'clear') {
    if (!(await confirm({ title: '清空所有请求日志？', message: '已记录的日志会被永久删除。', confirmText: '清空', danger: true }))) return
    try {
      await logsApi.clear()
      toast.success('日志已清空')
      load()
    } catch (error) {
      toast.error(errorMessage(error))
    }
  } else if (key === 'cache') {
    if (!(await confirm({ title: '清空静态资源缓存？', message: '缓存的海报和静态文件会被删除，之后会重新从源站拉取。', confirmText: '清空' }))) return
    try {
      await logsApi.clearAssetCache()
      toast.success('缓存已清空')
    } catch (error) {
      toast.error(errorMessage(error))
    }
  }
}
async function copy(value: string) {
  if (await copyText(value)) toast.success('已复制')
}
</script>

<template>
  <div>
    <PageHeader title="日志" description="经过云渡的每一个请求。点击一行查看详情。">
      <UiMenu :items="[{ key: 'clear', label: '清空日志', danger: true }, { key: 'cache', label: '清空静态资源缓存' }]" label="日志操作" @select="onMenu">
        <UiButton icon aria-label="更多操作"><MoreHorizontal /></UiButton>
      </UiMenu>
    </PageHeader>

    <div class="toolbar">
      <nav class="tabs">
        <button v-for="r in RANGES" :key="r.key" type="button" :class="{ on: range === r.key }" @click="range = r.key">{{ r.label }}</button>
      </nav>
      <div class="filters">
        <select v-model="category" class="select mini" aria-label="资源类别">
          <option value="">全部类别</option>
          <option v-for="(label, key) in CATEGORIES" :key="key" :value="key">{{ label }}</option>
        </select>
        <select v-model="status" class="select mini" aria-label="状态">
          <option value="">全部状态</option>
          <option value="2xx">成功 2xx</option>
          <option value="3xx">跳转 3xx</option>
          <option value="4xx">客户端错误 4xx</option>
          <option value="5xx">服务端错误 5xx</option>
        </select>
        <label class="search"><Search /><input v-model="query" data-page-search type="search" placeholder="搜索路径、IP、UA" aria-label="搜索日志" /></label>
      </div>
    </div>
    <div class="meta">
      <span class="faint">{{ loading ? '正在读取…' : summary }}<template v-if="dropped"> · 有 {{ dropped }} 条因队列满未写入</template></span>
      <UiSwitch v-model="live" label="自动刷新" />
    </div>

    <div class="table" role="table">
      <div class="row head" role="row"><span>时间</span><span>状态</span><span>请求</span><span>站点</span><span>类别</span><span>客户端</span></div>
      <div v-for="log in logs" :key="log.id" class="row item" role="row" tabindex="0" :class="{ active: selected?.id === log.id }" @click="selected = log" @keydown.enter="selected = log">
        <span class="num faint">{{ time(log.recorded_at_ms) }}</span>
        <span class="num code" :class="tone(log.status_code)">{{ log.status_code }}</span>
        <span class="req"><span class="method">{{ log.method }}</span><span class="mono path">{{ log.path }}</span></span>
        <span class="site">{{ log.site_name }}</span>
        <span class="faint">{{ CATEGORIES[log.resource_category] || log.resource_category }}</span>
        <span class="mono client faint">{{ log.client_ip }}</span>
      </div>
      <p v-if="!loading && !logs.length" class="none">这段时间没有符合条件的请求</p>
    </div>

    <UiDrawer :open="!!selected" title="请求详情" :subtitle="selected ? `${selected.method} · ${selected.status_code}` : ''" :width="480" @close="selected = null">
      <dl v-if="selected" class="props">
        <div><dt>时间</dt><dd class="num">{{ new Date(selected.recorded_at_ms).toLocaleString() }}</dd></div>
        <div><dt>路径</dt><dd class="mono">{{ selected.path }} <button type="button" class="copy" aria-label="复制路径" @click="copy(selected.path)"><Copy /></button></dd></div>
        <div><dt>站点</dt><dd>{{ selected.site_name }}</dd></div>
        <div><dt>类别</dt><dd>{{ CATEGORIES[selected.resource_category] || selected.resource_category }}</dd></div>
        <div><dt>状态码</dt><dd class="num" :class="tone(selected.status_code)">{{ selected.status_code }}</dd></div>
        <div><dt>客户端 IP</dt><dd class="mono">{{ selected.client_ip }}<span v-if="selected.client_region" class="faint"> · {{ selected.client_region }}</span></dd></div>
        <div v-if="selected.backend_address"><dt>回源节点</dt><dd class="mono">{{ selected.backend_address }}</dd></div>
        <div v-if="selected.inbound_colo || selected.outbound_colo"><dt>CDN 节点</dt><dd>{{ [selected.inbound_colo, selected.outbound_colo].filter(Boolean).join(' → ') }}</dd></div>
        <div><dt>客户端 UA</dt><dd class="mono small">{{ selected.user_agent || '—' }}</dd></div>
        <div v-if="selected.upstream_user_agent && selected.upstream_user_agent !== selected.user_agent"><dt>回源 UA</dt><dd class="mono small">{{ selected.upstream_user_agent }}</dd></div>
      </dl>
    </UiDrawer>
  </div>
</template>

<style scoped>
.toolbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); flex-wrap: wrap; }
.tabs { display: flex; gap: 22px; }
.tabs button { position: relative; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; cursor: pointer; white-space: nowrap; }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.filters { display: flex; align-items: center; gap: 4px; }
.select.mini { width: auto; height: 30px; padding-left: 8px; font-size: 13px; border-color: transparent; }
.select.mini:hover { border-color: var(--border-strong); }
.search { display: flex; align-items: center; gap: 8px; width: 200px; height: 30px; padding: 0 8px; border-radius: var(--radius-sm); color: var(--text-3); }
.search:focus-within { background: var(--surface-hover); }
.search svg { width: 14px; height: 14px; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13.5px; color: var(--text); }
.meta { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 0; font-size: 12.5px; }
.meta :deep(.label) { font-size: 12.5px; color: var(--text-3); }
.table { font-size: 13px; }
.row { display: grid; grid-template-columns: 110px 52px minmax(0, 1fr) 120px 80px 130px; align-items: center; gap: 14px; padding: 0 12px; }
.row.head { height: 34px; font-size: 12px; color: var(--text-3); border-bottom: 1px solid var(--border); }
.row.item { height: 40px; margin: 0 -12px; padding: 0 24px; border-radius: var(--radius-sm); cursor: pointer; outline: none; }
.row.item:hover, .row.item:focus-visible, .row.item.active { background: var(--surface-hover); }
.code { font-weight: 500; }
.warn { color: var(--warn); }
.bad { color: var(--bad); }
.req { display: flex; gap: 8px; min-width: 0; }
.method { flex: 0 0 auto; color: var(--text-3); font-size: 12px; }
.path, .site, .client { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.none { padding: 48px 0; text-align: center; color: var(--text-3); }
.props { margin: 0; }
.props div { display: flex; gap: 16px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.props dt { flex: 0 0 80px; color: var(--text-3); }
.props dd { flex: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; }
.small { font-size: 12px; }
.copy { display: inline-grid; place-items: center; width: 22px; height: 22px; margin-left: 4px; vertical-align: middle; border: 0; border-radius: 4px; background: transparent; color: var(--text-3); cursor: pointer; }
.copy:hover { background: var(--surface-hover); color: var(--text); }
.copy svg { width: 13px; height: 13px; }
@media (max-width: 860px) {
  .toolbar { flex-direction: column; align-items: stretch; gap: 6px; padding-bottom: 8px; }
  .filters { flex-wrap: wrap; }
  .search { flex: 1 1 100%; width: auto; background: var(--surface-hover); }
  .row.head { display: none; }
  .row.item { grid-template-columns: auto minmax(0, 1fr) auto; grid-template-areas: 'code req time' 'site site site'; height: auto; gap: 2px 10px; padding: 10px 16px; margin: 0 -16px; border-radius: 0; border-bottom: 1px solid var(--border); }
  .row.item > :nth-child(1) { grid-area: time; font-size: 12px; }
  .row.item > :nth-child(2) { grid-area: code; }
  .row.item > :nth-child(3) { grid-area: req; }
  .row.item > :nth-child(4) { grid-area: site; font-size: 12px; color: var(--text-3); }
  .row.item > :nth-child(5), .row.item > :nth-child(6) { display: none; }
}
</style>
