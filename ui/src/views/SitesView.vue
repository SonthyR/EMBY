<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Plus, Search, Zap, Upload, ArrowUpDown, BellRing, Server, Check } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import UiMenu from '@/components/ui/UiMenu.vue'
import SiteCard from '@/components/sites/SiteCard.vue'
import SiteImportDialog from '@/components/sites/SiteImportDialog.vue'
import SiteExportDialog from '@/components/sites/SiteExportDialog.vue'
import ReminderHistoryDialog from '@/components/sites/ReminderHistoryDialog.vue'
import { sitesApi, type AccountReminderRow, type IngressCapabilities, type Site } from '@/api'
import { diagLatency, siteAccessAddress } from '@/components/sites/siteLogic'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText } from '@/utils/format'

const sites = ref<Site[]>([])
const capabilities = ref<IngressCapabilities>({})
const reminders = ref<AccountReminderRow[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const latency = ref<Record<number, { busy?: boolean; ok?: boolean; ms?: number; error?: string }>>({})
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
  loadReminders()
}
async function loadReminders() {
  try {
    const rows = await sitesApi.reminders()
    reminders.value = rows.filter(r => r.status === 'expired' || r.status === 'expiring').sort((a, b) => a.days_left - b.days_left)
  } catch {
    reminders.value = []
  }
}
onMounted(() => load())

const list = computed(() => (sorting.value ? sortDraft.value : sites.value))
const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q || sorting.value) return list.value
  return list.value.filter(s => `${s.name} ${s.target_url} ${s.public_host || ''} ${s.path_prefix || ''}`.toLowerCase().includes(q))
})
const runningCount = computed(() => sites.value.filter(s => s.running).length)

function address(site: Site) {
  return siteAccessAddress(site, capabilities.value)
}

async function test(site: Site) {
  latency.value[site.id] = { busy: true }
  try {
    latency.value[site.id] = diagLatency(await sitesApi.diag(site.id))
  } catch (error) {
    latency.value[site.id] = { ok: false, error: errorMessage(error, '测速失败') }
  }
}
async function testAll() {
  await Promise.all(sites.value.map(test))
}

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
  if (!(await confirm({ title: `删除站点 ${site.name}？`, message: '站点配置、保存的账号、流量记录都会一起删除，分到这个站点的分享账号也会失去访问权限。此操作无法撤销。', confirmText: '删除', danger: true }))) return
  try {
    await sitesApi.remove(site.id)
    toast.success('站点已删除')
    load(true)
  } catch (error) {
    toast.error(errorMessage(error))
  }
}
async function copyAddress(site: Site) {
  const value = address(site)
  if (!value) return toast.error('这个站点还没有可用的访问地址')
  if (await copyText(value)) toast.success('访问地址已复制')
  else toast.error('复制失败：' + value)
}
function edit(site?: Site) {
  // The detailed site editor is still served by the previous panel.
  location.href = site ? `/#sites/edit/${site.id}` : '/#sites/new'
}

function startSorting() {
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
    toast.error(errorMessage(error, '保存顺序失败'))
  } finally {
    savingOrder.value = false
  }
}

function pickImport(format: string) {
  importFormat.value = format
  if (fileInput.value) {
    fileInput.value.value = ''
    fileInput.value.accept = format === 'csv' ? '.csv,text/csv' : '.json,application/json'
    fileInput.value.click()
  }
}
async function onFile(event: Event) {
  const file = (event.target as HTMLInputElement).files?.[0]
  if (!file) return
  try {
    importData.value = await file.text()
    importOpen.value = true
  } catch {
    toast.error('读取文件失败')
  }
}
function onMenu(key: string) {
  if (key === 'import-json') pickImport('json')
  else if (key === 'import-csv') pickImport('csv')
  else if (key === 'export') exportOpen.value = true
  else if (key === 'history') historyOpen.value = true
  else if (key === 'sort') startSorting()
}
const menuItems = [
  { key: 'import-json', label: '导入 JSON 配置' },
  { key: 'import-csv', label: '导入 CSV 表格' },
  { key: 'export', label: '导出站点…' },
  { key: 'd1', label: '', divider: true },
  { key: 'sort', label: '调整站点顺序' },
  { key: 'history', label: '到期提醒发送记录' },
]
</script>

<template>
  <div class="page">
    <section class="summary">
      <div><strong>{{ sites.length }}</strong><span>站点</span></div>
      <div><strong class="good">{{ runningCount }}</strong><span>运行中</span></div>
      <div><strong :class="{ warn: reminders.length }">{{ reminders.length }}</strong><span>账号即将/已经到期</span></div>
    </section>

    <section v-if="reminders.length" class="reminders">
      <BellRing />
      <div class="pills">
        <UiBadge v-for="r in reminders.slice(0, 10)" :key="r.account.id" :tone="r.status === 'expired' ? 'bad' : 'warn'">
          {{ r.site_name }} · {{ r.account.account_name || r.account.username }} · {{ r.status === 'expired' ? '已到期' : `剩 ${r.days_left} 天` }}
        </UiBadge>
        <span v-if="reminders.length > 10" class="faint">等 {{ reminders.length }} 个</span>
      </div>
    </section>

    <section class="toolbar">
      <template v-if="sorting">
        <p class="muted sort-tip">用箭头调整顺序，客户端分享信息和列表都按这个顺序显示。</p>
        <UiButton @click="sorting = false">取消</UiButton>
        <UiButton variant="primary" :loading="savingOrder" @click="saveOrder"><Check />保存顺序</UiButton>
      </template>
      <template v-else>
        <div class="search">
          <Search />
          <input v-model="query" class="input" type="search" placeholder="搜索站点名称、地址或路径" aria-label="搜索站点" />
        </div>
        <UiButton :disabled="!sites.length" @click="testAll"><Zap />全部测速</UiButton>
        <UiMenu :items="menuItems" label="更多站点操作" @select="onMenu">
          <UiButton aria-label="更多"><ArrowUpDown />导入 / 导出</UiButton>
        </UiMenu>
        <UiButton variant="primary" class="create" @click="edit()"><Plus />添加站点</UiButton>
      </template>
      <input ref="fileInput" type="file" hidden @change="onFile" />
    </section>

    <div v-if="loading" class="grid"><div v-for="n in 3" :key="n" class="skeleton" /></div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!sites.length" class="empty">
      <Server class="big" />
      <h2>还没有站点</h2>
      <p>添加一个 Emby 源站，云渡会为它生成反代地址；也可以导入之前导出的配置。</p>
      <div class="row">
        <UiButton variant="primary" @click="edit()"><Plus />添加站点</UiButton>
        <UiButton @click="pickImport('json')"><Upload />导入配置</UiButton>
      </div>
    </div>
    <div v-else-if="!visible.length" class="empty small"><p>没有匹配的站点。</p></div>
    <TransitionGroup v-else name="list" tag="div" class="grid">
      <SiteCard
        v-for="(site, index) in visible"
        :key="site.id"
        :site="site"
        :address="address(site)"
        :latency="latency[site.id]"
        :sorting="sorting"
        :first="index === 0"
        :last="index === visible.length - 1"
        @edit="edit(site)"
        @toggle="toggle(site)"
        @remove="remove(site)"
        @test="test(site)"
        @copy="copyAddress(site)"
        @up="move(index, -1)"
        @down="move(index, 1)"
      />
    </TransitionGroup>

    <button v-if="!sorting" type="button" class="fab" aria-label="添加站点" @click="edit()"><Plus /></button>

    <SiteImportDialog :open="importOpen" :format="importFormat" :data="importData" @close="importOpen = false" @imported="load(true)" />
    <SiteExportDialog :open="exportOpen" @close="exportOpen = false" />
    <ReminderHistoryDialog :open="historyOpen" @close="historyOpen = false" @changed="loadReminders" />
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: 16px; }
.summary { display: flex; gap: 28px; padding: 4px 2px; }
.summary div { display: flex; flex-direction: column; }
.summary strong { font-size: 24px; font-weight: 600; }
.summary span { font-size: 12px; color: var(--text-3); }
.good { color: var(--good); }
.warn { color: var(--warn); }
.reminders { display: flex; align-items: flex-start; gap: 10px; padding: 12px 14px; border: 1px solid color-mix(in srgb, var(--warn) 35%, var(--border)); border-radius: var(--radius-lg); background: var(--warn-soft); }
.reminders > svg { width: 18px; height: 18px; color: var(--warn); flex: 0 0 18px; margin-top: 2px; }
.pills { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
.toolbar { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.sort-tip { flex: 1; margin: 0; font-size: 13px; }
.search { position: relative; flex: 1 1 240px; max-width: 380px; }
.search svg { position: absolute; left: 11px; top: 50%; transform: translateY(-50%); width: 16px; height: 16px; color: var(--text-3); }
.search .input { padding-left: 34px; }
.create { margin-left: auto; }
.grid { position: relative; display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 14px; }
.skeleton { height: 250px; border-radius: var(--radius-lg); background: linear-gradient(90deg, var(--surface) 0%, var(--surface-hover) 50%, var(--surface) 100%); background-size: 200% 100%; animation: shimmer 1.2s infinite linear; }
@keyframes shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }
.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 56px 20px; border: 1px dashed var(--border-strong); border-radius: var(--radius-lg); text-align: center; }
.empty.small { padding: 32px 20px; }
.empty .big { width: 40px; height: 40px; color: var(--accent); }
.empty h2 { margin: 6px 0 0; font-size: 18px; }
.empty p { max-width: 440px; margin: 0; color: var(--text-2); line-height: 1.7; }
.row { display: flex; gap: 8px; flex-wrap: wrap; justify-content: center; }
.fab { display: none; }
@media (max-width: 860px) {
  .create { display: none; }
  .search { max-width: none; flex-basis: 100%; }
  .grid { grid-template-columns: minmax(0, 1fr); }
  .fab { position: fixed; right: 18px; bottom: calc(80px + env(safe-area-inset-bottom)); z-index: 25; display: grid; place-items: center; width: 54px; height: 54px; border: 0; border-radius: 18px; background: var(--accent-strong); color: var(--on-accent); box-shadow: 0 8px 24px rgba(40, 80, 200, 0.4); cursor: pointer; }
  .fab svg { width: 24px; height: 24px; }
}
</style>
