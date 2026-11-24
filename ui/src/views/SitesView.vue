<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Plus, Search, Copy, Zap, MoreHorizontal, ArrowUp, ArrowDown, GripVertical } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiMenu from '@/components/ui/UiMenu.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import SiteDetail from '@/components/sites/SiteDetail.vue'
import SiteEditor from '@/components/sites/SiteEditor.vue'
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
const draggingIdx = ref<number | null>(null)
const dragOverIdx = ref<number | null>(null)
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
const route = useRoute()
const router = useRouter()
watch(
  () => route.query.new,
  value => {
    if (!value) return
    router.replace({ query: {} })
    setTimeout(() => edit(), 0)
  },
  { immediate: true },
)

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
const editorOpen = ref(false)
const editingSite = ref<Site | null>(null)
function edit(site?: Site) {
  editingSite.value = site || null
  selectedId.value = null
  editorOpen.value = true
}
function onEditorSaved() {
  editorOpen.value = false
  load(true)
}
function latencyText(site: Site) {
  const l = latency.value[site.id]
  if (!l) return { text: '未测速', tone: 'faint' }
  if (l.busy) return { text: '测速中…', tone: 'faint' }
  if (!l.ok) return { text: '连接失败', tone: 'bad' }
  return { text: `${l.ms} ms`, tone: (l.ms || 0) < 200 ? 'good' : (l.ms || 0) < 800 ? 'warn' : 'bad' }
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
function onSortDragStart(index: number, event: DragEvent) {
  draggingIdx.value = index
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(index))
  }
}
function onSortDragOver(index: number, event: DragEvent) {
  event.preventDefault()
  dragOverIdx.value = index
  if (draggingIdx.value === null || draggingIdx.value === index) return
  const next = [...sortDraft.value]
  const [item] = next.splice(draggingIdx.value, 1)
  next.splice(index, 0, item)
  sortDraft.value = next
  draggingIdx.value = index
}
function onSortDragEnd() {
  draggingIdx.value = null
  dragOverIdx.value = null
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
  { key: 'sort', label: '拖拽调整顺序' },
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

    <dl class="kpi-grid">
      <div class="kpi-card"><dt>站点总数</dt><dd class="num">{{ counts.all }}</dd></div>
      <div class="kpi-card"><dt><span class="dot good" />运行中</dt><dd class="num">{{ counts.running }}</dd></div>
      <div class="kpi-card"><dt>累计流量</dt><dd class="num">{{ formatBytes(totalTraffic) }}</dd></div>
      <div class="kpi-card" :class="{ warn: reminders.length }"><dt>账号即将到期</dt><dd class="num">{{ reminders.length }}</dd></div>
    </dl>

    <button v-if="reminders.length && !sorting" type="button" class="notice" @click="historyOpen = true">
      <span class="dot warn" />
      <span>
        <template v-for="(r, i) in reminders.slice(0, 3)" :key="r.account.id">{{ i ? '，' : '' }}{{ r.site_name }} 的 {{ r.account.account_name || r.account.username }}{{ r.status === 'expired' ? ' 已到期' : ` 剩 ${r.days_left} 天` }}</template>
        <template v-if="reminders.length > 3"> 等 {{ reminders.length }} 个源站账号需要续费</template>
      </span>
    </button>

    <div v-if="sorting" class="sortbar">
      <span class="muted">直接拖拽卡片或点击箭头调整顺序，分享信息和列表都会按这个顺序显示。</span>
      <div class="sort-acts">
        <UiButton size="sm" @click="sorting = false">取消</UiButton>
        <UiButton size="sm" variant="accent" :loading="savingOrder" @click="saveOrder">保存顺序</UiButton>
      </div>
    </div>
    <div v-else class="toolbar-card">
      <nav class="seg" aria-label="按状态筛选">
        <button type="button" :class="{ on: filter === 'all' }" @click="filter = 'all'">全部<span class="n">{{ counts.all }}</span></button>
        <button type="button" :class="{ on: filter === 'running' }" @click="filter = 'running'">运行中<span class="n">{{ counts.running }}</span></button>
        <button type="button" :class="{ on: filter === 'stopped' }" @click="filter = 'stopped'">已停止<span v-if="counts.stopped" class="n">{{ counts.stopped }}</span></button>
      </nav>
      <label class="search"><Search /><input v-model="query" data-page-search type="search" placeholder="搜索站点或地址…" aria-label="搜索站点" /><kbd>/</kbd></label>
    </div>

    <div v-if="loading" class="site-grid">
      <div v-for="n in 4" :key="n" class="site-card skeleton"><span /></div>
    </div>
    <div v-else-if="loadError" class="empty-card"><p>{{ loadError }}</p><UiButton @click="load()">重试</UiButton></div>
    <div v-else-if="!sites.length" class="empty-card">
      <h2>还没有站点</h2>
      <p>添加一个 Emby 源站，云渡会为它生成访问地址；也可以导入之前导出的配置。</p>
      <div class="row-actions">
        <UiButton variant="primary" @click="edit()"><Plus />添加站点</UiButton>
        <UiButton @click="pickImport('json')">导入配置</UiButton>
      </div>
    </div>

    <div v-else-if="sorting" class="sort-list">
      <div
        v-for="(site, index) in visible"
        :key="site.id"
        class="sort-row"
        :class="{ dragging: draggingIdx === index, over: dragOverIdx === index && draggingIdx !== index }"
        draggable="true"
        @dragstart="onSortDragStart(index, $event)"
        @dragover="onSortDragOver(index, $event)"
        @dragend="onSortDragEnd"
      >
        <span class="grip"><GripVertical /></span>
        <span class="rank-badge num">#{{ index + 1 }}</span>
        <div class="who">
          <span class="name">{{ site.name }}</span>
          <span class="sub mono">{{ address(site) || INGRESS_LABEL[normalizedIngressMode(site)] }}</span>
        </div>
        <div class="sort-btns">
          <UiButton variant="ghost" size="sm" icon :disabled="index === 0" aria-label="上移" @click="move(index, -1)"><ArrowUp /></UiButton>
          <UiButton variant="ghost" size="sm" icon :disabled="index === visible.length - 1" aria-label="下移" @click="move(index, 1)"><ArrowDown /></UiButton>
        </div>
      </div>
    </div>

    <div v-else class="site-grid">
      <article
        v-for="site in visible"
        :key="site.id"
        class="site-card"
        :class="{ active: selectedId === site.id, stopped: !site.running }"
        tabindex="0"
        @click="selectedId = site.id"
        @keydown.enter="selectedId = site.id"
      >
        <div class="card-top">
          <div class="who">
            <h3 class="name">{{ site.name }}</h3>
            <span class="sub mono" :title="address(site)">{{ address(site) || INGRESS_LABEL[normalizedIngressMode(site)] }}</span>
          </div>
          <span class="status-pill" :class="site.running ? 'good' : site.enabled ? 'bad' : 'off'">
            <span class="dot" :class="site.running ? 'good' : site.enabled ? 'bad' : ''" />
            {{ site.running ? '运行中' : site.enabled ? '启动失败' : '已停用' }}
          </span>
        </div>

        <div class="card-metrics">
          <div class="metric-box">
            <span class="m-label">已用流量</span>
            <span class="m-val num">
              {{ formatBytes(Number(site.traffic_used) || 0) }}
              <small v-if="site.traffic_quota" class="faint">/ {{ formatBytes(Number(site.traffic_quota)) }}</small>
            </span>
          </div>
          <div class="metric-box">
            <span class="m-label">源站延迟</span>
            <span class="m-val num" :class="latencyText(site).tone">{{ latencyText(site).text }}</span>
          </div>
          <div class="metric-box">
            <span class="m-label">接入模式</span>
            <span class="m-val">{{ INGRESS_LABEL[normalizedIngressMode(site)] }}</span>
          </div>
        </div>

        <UiProgress v-if="site.traffic_quota" :value="Number(site.traffic_used) || 0" :max="Number(site.traffic_quota)" />

        <div class="card-foot">
          <span class="faint hint-text">点击查看详情与源站账号</span>
          <div class="act" @click.stop>
            <UiButton variant="secondary" size="sm" @click="test(site)"><Zap />测速</UiButton>
            <UiButton variant="secondary" size="sm" @click="copyAddress(site)"><Copy />复制</UiButton>
          </div>
        </div>
      </article>
      <p v-if="!visible.length" class="none">没有符合条件的站点</p>
    </div>

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
    <SiteEditor :open="editorOpen" :site="editingSite" :capabilities="capabilities" @close="editorOpen = false" @saved="onEditorSaved" />
    <SiteImportDialog :open="importOpen" :format="importFormat" :data="importData" @close="importOpen = false" @imported="load(true)" />
    <SiteExportDialog :open="exportOpen" @close="exportOpen = false" />
    <ReminderHistoryDialog :open="historyOpen" @close="historyOpen = false" @changed="load(true)" />
  </div>
</template>

<style scoped>
.kpi-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin: 0 0 18px; }
.kpi-card { padding: 16px 20px; border-radius: var(--radius-lg); background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow-card); display: flex; flex-direction: column; gap: 6px; }
.kpi-card.warn dd { color: var(--warn); }
.kpi-card dt { display: inline-flex; align-items: center; gap: 7px; font-size: 12.5px; font-weight: 500; color: var(--text-2); }
.kpi-card dd { margin: 0; font-size: 24px; font-weight: 700; letter-spacing: -0.025em; }

.notice {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  margin: 0 0 18px;
  padding: 12px 16px;
  border: 1px solid color-mix(in srgb, var(--warn) 34%, var(--border));
  border-radius: var(--radius);
  background: var(--warn-soft);
  color: var(--text);
  font-size: 13px;
  text-align: left;
  cursor: pointer;
}

.toolbar-card,
.sortbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  margin-bottom: 16px;
  padding: 10px 14px;
  border-radius: var(--radius);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
}
.sortbar .muted { font-size: 13px; }
.sort-acts { display: flex; gap: 8px; }

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
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 30px;
  padding: 0 13px;
  border: 0;
  border-radius: 999px;
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  white-space: nowrap;
  transition: all var(--dur) var(--ease);
}
.seg button:hover { color: var(--text); }
.seg button.on { background: var(--surface); color: var(--text); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08); }
.n { font-size: 11.5px; color: var(--text-3); }

.search {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 240px;
  height: 34px;
  padding: 0 12px;
  border-radius: 999px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  color: var(--text-3);
  transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.search:focus-within { background: var(--surface); border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.search svg { width: 14px; height: 14px; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13px; color: var(--text); }

.site-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
}
.site-card {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 20px 22px;
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  cursor: pointer;
  outline: none;
  transition: transform var(--dur) var(--ease), border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
}
.site-card:hover,
.site-card:focus-visible,
.site-card.active {
  border-color: var(--accent-border);
  transform: translateY(-2px);
}
.site-card.stopped { opacity: 0.78; }

.card-top { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.who { display: flex; flex-direction: column; gap: 3px; min-width: 0; }
.name { margin: 0; font-size: 16px; font-weight: 700; letter-spacing: -0.015em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sub { font-size: 12px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 24px;
  padding: 0 10px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
  white-space: nowrap;
  flex: 0 0 auto;
  background: var(--surface-2);
  color: var(--text-2);
}
.status-pill.good { background: var(--good-soft); color: var(--good); }
.status-pill.bad { background: var(--bad-soft); color: var(--bad); }

.card-metrics {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 8px;
}
.metric-box {
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}
.m-label { font-size: 11px; color: var(--text-3); }
.m-val { font-size: 13px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.m-val.good { color: var(--good); }
.m-val.warn { color: var(--warn); }
.m-val.bad { color: var(--bad); }
.m-val.faint { color: var(--text-3); font-weight: 500; }

.card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding-top: 10px;
  border-top: 1px solid var(--border);
}
.hint-text { font-size: 12px; }
.act { display: flex; gap: 6px; }

.sort-list { display: flex; flex-direction: column; gap: 10px; }
.sort-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 14px 16px;
  border-radius: var(--radius);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  cursor: grab;
  transition: transform 140ms var(--ease), border-color 140ms var(--ease), opacity 140ms var(--ease);
}
.sort-row:active { cursor: grabbing; }
.sort-row.dragging { opacity: 0.55; border-color: var(--accent); }
.sort-row.over { border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.grip { display: grid; place-items: center; color: var(--text-3); }
.grip svg { width: 16px; height: 16px; }
.rank-badge {
  display: inline-grid;
  place-items: center;
  min-width: 28px;
  height: 24px;
  padding: 0 6px;
  border-radius: 999px;
  background: var(--surface-2);
  font-size: 11.5px;
  font-weight: 700;
  color: var(--text-2);
}
.sort-row .who { flex: 1; }
.sort-btns { display: flex; gap: 4px; }

.skeleton { min-height: 168px; cursor: default; }
.skeleton span { height: 16px; width: 45%; border-radius: 6px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }
.none { grid-column: 1 / -1; padding: 40px 0; text-align: center; color: var(--text-3); font-size: 13.5px; }
.empty-card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 64px 20px;
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  text-align: center;
}
.empty-card h2 { margin: 0; font-size: 17px; font-weight: 700; }
.empty-card p { max-width: 380px; margin: 0 0 8px; color: var(--text-2); }
.row-actions { display: flex; gap: 8px; }

@media (max-width: 960px) {
  .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .site-grid { grid-template-columns: minmax(0, 1fr); }
}
@media (max-width: 640px) {
  .search { width: 100%; }
  .search kbd { display: none; }
  .card-metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .hint-text { display: none; }
}
</style>
