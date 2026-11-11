<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Plus, Search, Copy, Zap, MoreHorizontal, ArrowUp, ArrowDown } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiMenu from '@/components/ui/UiMenu.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SiteDetail from '@/components/sites/SiteDetail.vue'
import SiteImportDialog from '@/components/sites/SiteImportDialog.vue'
import SiteExportDialog from '@/components/sites/SiteExportDialog.vue'
import ReminderHistoryDialog from '@/components/sites/ReminderHistoryDialog.vue'
import { sitesApi, type AccountReminderRow, type IngressCapabilities, type Site } from '@/api'
import { INGRESS_LABEL, diagLatency, normalizedIngressMode, siteAccessAddress } from '@/components/sites/siteLogic'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, formatBytes } from '@/utils/format'

type Latency = { busy?: boolean; ok?: boolean; ms?: number; error?: string }
const sites = ref<Site[]>([])
const capabilities = ref<IngressCapabilities>({})
const reminders = ref<AccountReminderRow[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const filter = ref<'all' | 'running' | 'stopped'>('all')
const latency = ref<Record<number, Latency>>({})
const selectedId = ref<number | null>(null)
const sorting = ref(false)
const sortDraft = ref<Site[]>([])
const savingOrder = ref(false)
const importOpen = ref(false)
const importFormat = ref('json')
const importData = ref('')
const exportOpen = ref(false)
const historyOpen = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const [list, caps] = await Promise.all([sitesApi.list(), sitesApi.capabilities().catch(() => ({}))])
    sites.value = list
    capabilities.value = caps
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取失败')
  } finally {
    loading.value = false
  }
  try {
    const rows = await sitesApi.reminders()
    reminders.value = rows.filter(r => r.status === 'expired' || r.status === 'expiring').sort((a, b) => a.days_left - b.days_left)
  } catch {
    reminders.value = []
  }
}
onMounted(() => load())

const selected = computed(() => sites.value.find(s => s.id === selectedId.value) || null)
const counts = computed(() => ({ all: sites.value.length, running: sites.value.filter(s => s.running).length, stopped: sites.value.filter(s => !s.running).length }))
const totalTraffic = computed(() => sites.value.reduce((n, s) => n + (Number(s.traffic_used) || 0), 0))
const visible = computed(() => {
  if (sorting.value) return sortDraft.value
  const q = query.value.trim().toLowerCase()
  return sites.value.filter(s => {
    if (filter.value === 'running' && !s.running) return false
    if (filter.value === 'stopped' && s.running) return false
    return !q || `${s.name} ${s.target_url} ${s.public_host || ''} ${s.path_prefix || ''}`.toLowerCase().includes(q)
  })
})
const address = (site: Site) => siteAccessAddress(site, capabilities.value)

async function test(site: Site) {
  latency.value[site.id] = { busy: true }
  try {
    latency.value[site.id] = diagLatency(await sitesApi.diag(site.id))
  } catch (error) {
    latency.value[site.id] = { ok: false, error: errorMessage(error, '测速失败') }
  }
}
const testAll = () => Promise.all(sites.value.map(test))
async function toggle(site: Site) {
  try {
    const result = await sitesApi.toggle(site.id)
    toast.success(result.enabled ? '站点已启用' : '站点已停用')
    load(true)
  } catch (error) {
    toast.error(errorMessage(error))
  }
}
async function remove(site: Site) {
  if (!(await confirm({ title: `删除站点 ${site.name}？`, message: '站点配置、保存的源站账号和流量记录会一起删除，分到这个站点的分享账号也会失去访问权限。此操作无法撤销。', confirmText: '删除', danger: true }))) return
  try {
    await sitesApi.remove(site.id)
    selectedId.value = null
    toast.success('站点已删除')
    load(true)
  } catch (error) {
    toast.error(errorMessage(error))
  }
}
async function copyAddress(site: Site) {
  const value = address(site)
  if (!value) return toast.error('这个站点还没有访问地址')
  if (await copyText(value)) toast.success('访问地址已复制')
}
function edit(site?: Site) {
  // The detailed site editor is still served by the previous panel.
  location.href = site ? `/#sites/edit/${site.id}` : '/#sites/new'
}
function latencyText(site: Site) {
  const l = latency.value[site.id]
  if (!l) return { text: '—', tone: 'faint' }
  if (l.busy) return { text: '…', tone: 'faint' }
  if (!l.ok) return { text: '失败', tone: 'bad' }
  return { text: `${l.ms} ms`, tone: (l.ms || 0) < 200 ? '' : (l.ms || 0) < 800 ? 'warn' : 'bad' }
}

function startSorting() {
  selectedId.value = null
  sortDraft.value = [...sites.value]
  sorting.value = true
}
function move(index: number, delta: number) {
  const next = [...sortDraft.value]
  const [item] = next.splice(index, 1)
  next.splice(index + delta, 0, item)
  sortDraft.value = next
}
async function saveOrder() {
  savingOrder.value = true
  try {
    await sitesApi.reorder(sortDraft.value.map(s => s.id))
    sites.value = [...sortDraft.value]
    sorting.value = false
    toast.success('顺序已保存')
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    savingOrder.value = false
  }
}
function pickImport(format: string) {
  importFormat.value = format
  if (!fileInput.value) return
  fileInput.value.value = ''
  fileInput.value.accept = format === 'csv' ? '.csv,text/csv' : '.json,application/json'
  fileInput.value.click()
}
async function onFile(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  importData.value = await file.text()
  importOpen.value = true
}
function onMenu(key: string) {
  if (key === 'test') testAll()
  else if (key === 'import-json') pickImport('json')
  else if (key === 'import-csv') pickImport('csv')
  else if (key === 'export') exportOpen.value = true
  else if (key === 'sort') startSorting()
  else if (key === 'history') historyOpen.value = true
}
const menuItems = [
  { key: 'test', label: '全部测速' },
  { key: 'sort', label: '调整顺序' },
  { key: 'd1', label: '', divider: true },
  { key: 'import-json', label: '导入 JSON' },
  { key: 'import-csv', label: '导入 CSV' },
  { key: 'export', label: '导出…' },
  { key: 'd2', label: '', divider: true },
  { key: 'history', label: '到期提醒记录' },
]
</script>

<template>
  <div>
    <PageHeader title="站点" description="你反代的 Emby 源站。朋友通过这里的访问地址连接。">
      <UiMenu :items="menuItems" label="更多操作" @select="onMenu">
        <UiButton icon aria-label="更多操作"><MoreHorizontal /></UiButton>
      </UiMenu>
      <UiButton variant="primary" @click="edit()"><Plus />添加站点</UiButton>
    </PageHeader>

    <dl class="stats">
      <div><dt>站点</dt><dd class="num">{{ counts.all }}</dd></div>
      <div><dt>运行中</dt><dd class="num">{{ counts.running }}</dd></div>
      <div><dt>累计流量</dt><dd class="num">{{ formatBytes(totalTraffic) }}</dd></div>
      <div><dt>账号即将到期</dt><dd class="num" :class="{ warn: reminders.length }">{{ reminders.length }}</dd></div>
    </dl>

    <button v-if="reminders.length && !sorting" type="button" class="notice" @click="historyOpen = true">
      <span class="dot warn" />
      <span>
        <template v-for="(r, i) in reminders.slice(0, 3)" :key="r.account.id">{{ i ? '，' : '' }}{{ r.site_name }} 的 {{ r.account.account_name || r.account.username }}{{ r.status === 'expired' ? ' 已到期' : ` 剩 ${r.days_left} 天` }}</template>
        <template v-if="reminders.length > 3"> 等 {{ reminders.length }} 个源站账号需要续费</template>
      </span>
    </button>

    <div v-if="sorting" class="sortbar">
      <span class="muted">用箭头调整顺序，分享信息和列表都会按这个顺序显示。</span>
      <UiButton size="sm" @click="sorting = false">取消</UiButton>
      <UiButton size="sm" variant="primary" :loading="savingOrder" @click="saveOrder">保存顺序</UiButton>
    </div>
    <div v-else class="toolbar">
      <nav class="tabs">
        <button type="button" :class="{ on: filter === 'all' }" @click="filter = 'all'">全部<span class="n">{{ counts.all }}</span></button>
        <button type="button" :class="{ on: filter === 'running' }" @click="filter = 'running'">运行中<span class="n">{{ counts.running }}</span></button>
        <button type="button" :class="{ on: filter === 'stopped' }" @click="filter = 'stopped'">已停止<span v-if="counts.stopped" class="n">{{ counts.stopped }}</span></button>
      </nav>
      <label class="search"><Search /><input v-model="query" data-page-search type="search" placeholder="搜索" aria-label="搜索站点" /><kbd>/</kbd></label>
    </div>

    <div v-if="loading" class="rows"><div v-for="n in 3" :key="n" class="row skeleton"><span /></div></div>
    <div v-else-if="loadError" class="empty"><p>{{ loadError }}</p><UiButton @click="load()">重试</UiButton></div>
    <div v-else-if="!sites.length" class="empty">
      <h2>还没有站点</h2>
      <p>添加一个 Emby 源站，云渡会为它生成访问地址；也可以导入之前导出的配置。</p>
      <div class="row-actions">
        <UiButton variant="primary" @click="edit()"><Plus />添加站点</UiButton>
        <UiButton @click="pickImport('json')">导入配置</UiButton>
      </div>
    </div>
    <div v-else class="rows">
      <div class="row head">
        <span>站点</span><span>状态</span><span>流量</span><span>延迟</span><span class="sr-only">操作</span>
      </div>
      <TransitionGroup name="list">
        <div
          v-for="(site, index) in visible"
          :key="site.id"
          class="row item"
          :class="{ active: selectedId === site.id }"
          tabindex="0"
          @click="!sorting && (selectedId = site.id)"
          @keydown.enter="!sorting && (selectedId = site.id)"
        >
          <span class="who">
            <span class="name">{{ site.name }}</span>
            <span class="sub mono">{{ address(site) || INGRESS_LABEL[normalizedIngressMode(site)] }}</span>
          </span>
          <span class="status"><span class="dot" :class="site.running ? 'good' : site.enabled ? 'bad' : ''" />{{ site.running ? '运行中' : site.enabled ? '启动失败' : '已停用' }}</span>
          <span class="traffic">
            <span class="num">{{ formatBytes(Number(site.traffic_used) || 0) }}<span v-if="site.traffic_quota" class="faint"> / {{ formatBytes(Number(site.traffic_quota)) }}</span></span>
            <UiProgress v-if="site.traffic_quota" :value="Number(site.traffic_used) || 0" :max="Number(site.traffic_quota)" />
          </span>
          <span class="num lat" :class="latencyText(site).tone">{{ latencyText(site).text }}</span>
          <span class="act">
            <template v-if="sorting">
              <UiButton variant="ghost" size="sm" icon :disabled="index === 0" aria-label="上移" @click.stop="move(index, -1)"><ArrowUp /></UiButton>
              <UiButton variant="ghost" size="sm" icon :disabled="index === visible.length - 1" aria-label="下移" @click.stop="move(index, 1)"><ArrowDown /></UiButton>
            </template>
            <template v-else>
              <UiButton variant="ghost" size="sm" icon aria-label="测速" title="测速" @click.stop="test(site)"><Zap /></UiButton>
              <UiButton variant="ghost" size="sm" icon aria-label="复制访问地址" title="复制访问地址" @click.stop="copyAddress(site)"><Copy /></UiButton>
            </template>
          </span>
          <span class="mobile-meta">{{ formatBytes(Number(site.traffic_used) || 0) }} · {{ INGRESS_LABEL[normalizedIngressMode(site)] }}<template v-if="latency[site.id]?.ok"> · {{ latency[site.id].ms }} ms</template></span>
        </div>
      </TransitionGroup>
      <p v-if="!visible.length" class="none">没有符合条件的站点</p>
    </div>

    <button type="button" class="fab" aria-label="添加站点" @click="edit()"><Plus /></button>
    <input ref="fileInput" type="file" hidden @change="onFile" />

    <SiteDetail
      :open="!!selected"
      :site="selected"
      :address="selected ? address(selected) : ''"
      :latency="selected ? latency[selected.id] : undefined"
      @close="selectedId = null"
      @edit="selected && edit(selected)"
      @toggle="selected && toggle(selected)"
      @remove="selected && remove(selected)"
      @test="selected && test(selected)"
      @copy="selected && copyAddress(selected)"
    />
    <SiteImportDialog :open="importOpen" :format="importFormat" :data="importData" @close="importOpen = false" @imported="load(true)" />
    <SiteExportDialog :open="exportOpen" @close="exportOpen = false" />
    <ReminderHistoryDialog :open="historyOpen" @close="historyOpen = false" @changed="load(true)" />
  </div>
</template>

<style scoped>
.stats { display: flex; gap: 48px; margin: 0 0 28px; flex-wrap: wrap; }
.stats div { display: flex; flex-direction: column; gap: 2px; }
.stats dt { font-size: 12.5px; color: var(--text-3); }
.stats dd { margin: 0; font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
.stats dd.warn { color: var(--warn); }
.notice { display: flex; align-items: center; gap: 10px; width: 100%; margin: 0 0 24px; padding: 10px 14px; border: 1px solid color-mix(in srgb, var(--warn) 30%, var(--border)); border-radius: var(--radius); background: color-mix(in srgb, var(--warn) 6%, transparent); color: var(--text-2); font-size: 13px; text-align: left; cursor: pointer; }
.toolbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); }
.sortbar { display: flex; align-items: center; gap: 8px; padding: 8px 0; border-bottom: 1px solid var(--border); }
.sortbar .muted { flex: 1; font-size: 13px; }
.tabs { display: flex; gap: 22px; }
.tabs button { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; cursor: pointer; white-space: nowrap; }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.n { font-size: 11.5px; color: var(--text-3); }
.search { display: flex; align-items: center; gap: 8px; width: 220px; height: 30px; padding: 0 8px; border-radius: var(--radius-sm); color: var(--text-3); }
.search:focus-within { background: var(--surface-hover); }
.search svg { width: 14px; height: 14px; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13.5px; color: var(--text); }
.rows { position: relative; }
.row { display: grid; grid-template-columns: minmax(0, 2.4fr) 110px minmax(0, 1.4fr) 80px 72px; align-items: center; gap: 16px; padding: 0 12px; }
.row.head { height: 38px; font-size: 12px; color: var(--text-3); }
.row.item { height: 60px; margin: 0 -12px; padding: 0 24px; border-radius: var(--radius); cursor: pointer; outline: none; transition: background var(--dur) var(--ease); }
.row.item + .row.item { box-shadow: inset 0 1px 0 var(--border); }
.row.item:hover, .row.item:focus-visible, .row.item.active { background: var(--surface-hover); box-shadow: none; }
.row.item:hover + .row.item, .row.item.active + .row.item { box-shadow: none; }
.who { display: flex; flex-direction: column; min-width: 0; }
.name { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sub { font-size: 12px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.status { display: inline-flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-2); }
.traffic { display: flex; flex-direction: column; gap: 6px; font-size: 13px; }
.lat { font-size: 13px; }
.lat.warn { color: var(--warn); }
.lat.bad { color: var(--bad); }
.act { display: flex; justify-content: flex-end; gap: 2px; opacity: 0; transition: opacity var(--dur) var(--ease); }
.row.item:hover .act, .row.item.active .act, .row.item:focus-within .act { opacity: 1; }
.sortbar ~ .rows .act { opacity: 1; }
.mobile-meta { display: none; }
.skeleton { height: 60px; }
.skeleton span { grid-column: 1 / -1; height: 14px; width: 40%; border-radius: 4px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }
.none { padding: 32px 0; text-align: center; color: var(--text-3); font-size: 13.5px; }
.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 72px 20px; text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 600; }
.empty p { max-width: 380px; margin: 0 0 8px; color: var(--text-2); }
.row-actions { display: flex; gap: 8px; }
.fab { display: none; }
@media (max-width: 860px) {
  .stats { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px 24px; }
  .toolbar { flex-direction: column-reverse; align-items: stretch; gap: 8px; }
  .search { width: 100%; height: 36px; background: var(--surface-hover); }
  .search kbd { display: none; }
  .row.head { display: none; }
  .row.item { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'who status' 'meta meta'; height: auto; gap: 4px 12px; padding: 13px 16px; margin: 0 -16px; border-radius: 0; }
  .sortbar ~ .rows .row.item { grid-template-areas: 'who act' 'meta meta'; }
  .who { grid-area: who; }
  .who .sub { display: none; }
  .status { grid-area: status; }
  .traffic, .lat { display: none; }
  .act { display: none; }
  .sortbar ~ .rows .act { display: flex; grid-area: act; }
  .sortbar ~ .rows .status { display: none; }
  .mobile-meta { grid-area: meta; display: block; font-size: 12.5px; color: var(--text-3); }
  .fab { position: fixed; right: 18px; bottom: calc(84px + env(safe-area-inset-bottom)); z-index: 25; display: grid; place-items: center; width: 52px; height: 52px; border: 0; border-radius: 50%; background: var(--inverse-bg); color: var(--inverse-text); box-shadow: 0 8px 28px rgba(0, 0, 0, 0.3); cursor: pointer; }
  .fab svg { width: 22px; height: 22px; }
}
</style>
