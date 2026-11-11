<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Plus, Search, Copy } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import ShareUserForm from '@/components/share/ShareUserForm.vue'
import ShareUserDetail from '@/components/share/ShareUserDetail.vue'
import ShareTextDialog from '@/components/share/ShareTextDialog.vue'
import { shareApi, sitesApi, type ShareStatus, type ShareUser, type Site } from '@/api'
import { copyText, daysUntil, dateOnly, formatBytes } from '@/utils/format'
import { errorMessage, toast } from '@/stores/toast'
import { STATUS_LABEL, STATUS_TONE } from '@/components/share/shareStatus'

const route = useRoute()
const router = useRouter()
const users = ref<ShareUser[]>([])
const sites = ref<Site[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const filter = ref<'all' | ShareStatus>('all')

const formOpen = ref(false)
const editing = ref<ShareUser | null>(null)
const selectedId = ref<number | null>(null)
const textOpen = ref(false)
const shareText = ref('')
const justCreated = ref(false)

const selected = computed(() => users.value.find(u => u.id === selectedId.value) || null)

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const [list, siteList] = await Promise.all([shareApi.list(), sitesApi.list()])
    users.value = list
    sites.value = siteList
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取失败')
  } finally {
    loading.value = false
  }
}

function onKey(event: KeyboardEvent) {
  const target = event.target as HTMLElement
  if (event.key.toLowerCase() !== 'n' || event.metaKey || event.ctrlKey || event.altKey) return
  if (/^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName) || document.querySelector('[role="dialog"]')) return
  event.preventDefault()
  openCreate()
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  document.addEventListener('keydown', onKey)
  timer = setInterval(() => document.visibilityState === 'visible' && !formOpen.value && load(true), 15000)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  document.removeEventListener('keydown', onKey)
})
watch(
  () => route.query.new,
  value => {
    if (value) {
      openCreate()
      router.replace({ query: {} })
    }
  },
  { immediate: true },
)

const counts = computed(() => {
  const result: Record<string, number> = { all: users.value.length, active: 0, disabled: 0, expired: 0, quota: 0 }
  for (const user of users.value) result[user.status]++
  return result
})
const online = computed(() => users.value.reduce((sum, u) => sum + (u.session_count || 0), 0))
const traffic = computed(() => users.value.reduce((sum, u) => sum + (u.traffic_used || 0), 0))
const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  return users.value.filter(user => {
    if (filter.value !== 'all' && user.status !== filter.value) return false
    return !q || [user.username, user.note, ...user.sites.map(s => s.site_name || '')].some(v => (v || '').toLowerCase().includes(q))
  })
})

function expiryText(user: ShareUser) {
  if (!user.expires_at) return { text: '永久', tone: '' }
  const days = daysUntil(user.expires_at)
  if (days === null) return { text: dateOnly(user.expires_at), tone: '' }
  if (days < 0) return { text: '已过期', tone: 'bad' }
  if (days === 0) return { text: '今天到期', tone: 'warn' }
  return { text: `${days} 天`, tone: days <= 7 ? 'warn' : '' }
}

function openCreate() {
  editing.value = null
  formOpen.value = true
}
function openEdit(user: ShareUser) {
  editing.value = user
  formOpen.value = true
}

async function onSaved(user: ShareUser, created: boolean) {
  formOpen.value = false
  await load(true)
  if (created) {
    try {
      shareText.value = (await shareApi.text(user.id)).text
      justCreated.value = true
      textOpen.value = true
    } catch {
      toast.success('已创建')
    }
  } else toast.success('已保存')
}
async function copyShare(user: ShareUser) {
  try {
    const { text } = await shareApi.text(user.id)
    if (await copyText(text)) toast.success('分享信息已复制')
    else {
      shareText.value = text
      justCreated.value = false
      textOpen.value = true
    }
  } catch (error) {
    toast.error(errorMessage(error))
  }
}
async function copyPortal(user: ShareUser) {
  const link = user.portal_url || (user.portal_path ? location.origin + user.portal_path : '')
  if (!link) return toast.error('暂无查询链接')
  if (await copyText(link)) toast.success('查询链接已复制')
  else toast.error('复制失败：' + link)
}

const FILTERS: { key: 'all' | ShareStatus; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'active', label: STATUS_LABEL.active },
  { key: 'quota', label: STATUS_LABEL.quota },
  { key: 'expired', label: STATUS_LABEL.expired },
  { key: 'disabled', label: STATUS_LABEL.disabled },
]
</script>

<template>
  <div>
    <PageHeader title="分享账号" description="给朋友发放登录账号，服务器会用你的源站账号代为登录。">
      <UiButton variant="primary" @click="openCreate"><Plus />新建<kbd class="kbd-on-inverse">N</kbd></UiButton>
    </PageHeader>

    <dl class="stats">
      <div><dt>账号</dt><dd class="num">{{ counts.all }}</dd></div>
      <div><dt>正常</dt><dd class="num">{{ counts.active }}</dd></div>
      <div><dt>在线设备</dt><dd class="num">{{ online }}</dd></div>
      <div><dt>本周期流量</dt><dd class="num">{{ formatBytes(traffic) }}</dd></div>
    </dl>

    <div class="toolbar">
      <nav class="tabs" aria-label="按状态筛选">
        <button v-for="f in FILTERS" :key="f.key" type="button" :class="{ on: filter === f.key }" @click="filter = f.key">
          {{ f.label }}<span v-if="counts[f.key]" class="n">{{ counts[f.key] }}</span>
        </button>
      </nav>
      <label class="search">
        <Search />
        <input v-model="query" data-page-search type="search" placeholder="搜索" aria-label="搜索分享账号" />
        <kbd>/</kbd>
      </label>
    </div>

    <div v-if="loading" class="table">
      <div v-for="n in 4" :key="n" class="row skeleton"><span /></div>
    </div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!users.length" class="empty">
      <h2>还没有分享账号</h2>
      <p>新建一个账号发给朋友，他就能在 Emby 客户端里登录你的服务器。</p>
      <UiButton variant="primary" @click="openCreate"><Plus />新建分享账号</UiButton>
    </div>
    <div v-else class="table" role="table" aria-label="分享账号列表">
      <div class="row head" role="row">
        <span role="columnheader">账号</span>
        <span role="columnheader">状态</span>
        <span role="columnheader">流量</span>
        <span role="columnheader">剩余</span>
        <span role="columnheader">设备</span>
        <span role="columnheader" class="sr-only">操作</span>
      </div>
      <TransitionGroup name="list">
        <div
          v-for="user in visible"
          :key="user.id"
          class="row item"
          :class="{ active: selectedId === user.id, dim: user.status !== 'active' }"
          role="row"
          tabindex="0"
          @click="selectedId = user.id"
          @keydown.enter="selectedId = user.id"
        >
          <span class="who" role="cell">
            <span class="name">{{ user.username }}</span>
            <span class="sub">{{ user.note || user.sites.map(s => s.site_name).join('、') }}</span>
          </span>
          <span class="status" role="cell"><span class="dot" :class="STATUS_TONE[user.status]" />{{ STATUS_LABEL[user.status] }}</span>
          <span class="traffic" role="cell">
            <span class="num">{{ formatBytes(user.traffic_used) }}<span class="faint"> / {{ user.traffic_quota ? formatBytes(user.traffic_quota) : '∞' }}</span></span>
            <UiProgress v-if="user.traffic_quota" :value="user.traffic_used" :max="user.traffic_quota" />
          </span>
          <span class="num expiry" role="cell" :class="expiryText(user).tone">{{ expiryText(user).text }}</span>
          <span class="num devices" role="cell">{{ user.session_count }}<span class="faint">{{ user.max_devices ? ` / ${user.max_devices}` : '' }}</span></span>
          <span class="act" role="cell">
            <UiButton variant="ghost" size="sm" icon aria-label="复制分享信息" title="复制分享信息" @click.stop="copyShare(user)"><Copy /></UiButton>
          </span>
          <span class="mobile-meta" aria-hidden="true">
            {{ formatBytes(user.traffic_used) }}{{ user.traffic_quota ? ' / ' + formatBytes(user.traffic_quota) : '' }} · {{ expiryText(user).text }} · {{ user.session_count }} 台在线
          </span>
        </div>
      </TransitionGroup>
      <p v-if="!visible.length" class="none">没有符合条件的账号</p>
    </div>

    <button type="button" class="fab" aria-label="新建分享账号" @click="openCreate"><Plus /></button>

    <ShareUserDetail
      :open="!!selected && !formOpen"
      :user="selected"
      @close="selectedId = null"
      @edit="selected && openEdit(selected)"
      @changed="load(true)"
      @deleted="
        () => {
          selectedId = null
          load(true)
        }
      "
      @copy-share="selected && copyShare(selected)"
      @copy-portal="selected && copyPortal(selected)"
    />
    <ShareUserForm :open="formOpen" :user="editing" :sites="sites" @close="formOpen = false" @saved="onSaved" />
    <ShareTextDialog :open="textOpen" :text="shareText" :created="justCreated" @close="textOpen = false" />
  </div>
</template>

<style scoped>
.kbd-on-inverse { margin-left: 4px; border-color: color-mix(in srgb, var(--inverse-text) 25%, transparent); color: color-mix(in srgb, var(--inverse-text) 60%, transparent); }
.stats { display: flex; gap: 48px; margin: 0 0 32px; }
.stats div { display: flex; flex-direction: column; gap: 2px; }
.stats dt { font-size: 12.5px; color: var(--text-3); }
.stats dd { margin: 0; font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }

.toolbar { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); }
.tabs { display: flex; gap: 22px; overflow-x: auto; scrollbar-width: none; }
.tabs::-webkit-scrollbar { display: none; }
.tabs button { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; white-space: nowrap; cursor: pointer; transition: color var(--dur) var(--ease); }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.n { font-size: 11.5px; color: var(--text-3); }
.search { display: flex; align-items: center; gap: 8px; width: 220px; height: 30px; padding: 0 8px; border-radius: var(--radius-sm); color: var(--text-3); transition: background var(--dur) var(--ease); }
.search:focus-within { background: var(--surface-hover); }
.search svg { width: 14px; height: 14px; }
.search input { flex: 1; min-width: 0; border: 0; background: transparent; outline: none; font-size: 13.5px; color: var(--text); }
.search input::placeholder { color: var(--text-3); }

.table { position: relative; }
.row { display: grid; grid-template-columns: minmax(0, 2.2fr) 100px minmax(0, 1.6fr) 90px 70px 40px; align-items: center; gap: 16px; padding: 0 12px; }
.row.head { height: 38px; font-size: 12px; color: var(--text-3); }
.row.item { height: 60px; margin: 0 -12px; padding: 0 24px; border-radius: var(--radius); cursor: pointer; outline: none; transition: background var(--dur) var(--ease); }
.row.item + .row.item { box-shadow: inset 0 1px 0 var(--border); }
.row.item:hover, .row.item:focus-visible, .row.item.active { background: var(--surface-hover); box-shadow: none; }
.row.item:hover + .row.item, .row.item.active + .row.item { box-shadow: none; }
.row.dim .who .name { color: var(--text-2); }
.who { display: flex; flex-direction: column; min-width: 0; }
.name { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sub { font-size: 12.5px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.status { display: inline-flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-2); }
.traffic { display: flex; flex-direction: column; gap: 6px; font-size: 13px; }
.expiry, .devices { font-size: 13px; }
.expiry.warn { color: var(--warn); }
.expiry.bad { color: var(--bad); }
.act { opacity: 0; transition: opacity var(--dur) var(--ease); }
.row.item:hover .act, .row.item.active .act, .row.item:focus-within .act { opacity: 1; }
.mobile-meta { display: none; }
.skeleton { height: 60px; }
.skeleton span { grid-column: 1 / -1; height: 14px; width: 40%; border-radius: 4px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }
.none { padding: 32px 0; text-align: center; color: var(--text-3); font-size: 13.5px; }
.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 72px 20px; text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 600; }
.empty p { max-width: 380px; margin: 0 0 8px; color: var(--text-2); }
.fab { display: none; }

@media (max-width: 860px) {
  .stats { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 18px 24px; margin-bottom: 24px; }
  .toolbar { flex-direction: column-reverse; align-items: stretch; gap: 8px; }
  .search { width: 100%; height: 36px; background: var(--surface-hover); }
  .search kbd { display: none; }
  .row.head { display: none; }
  .row.item { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'who status' 'meta meta'; height: auto; gap: 4px 12px; padding: 13px 16px; margin: 0 -16px; border-radius: 0; }
  .who { grid-area: who; }
  .status { grid-area: status; }
  .traffic, .expiry, .devices, .act { display: none; }
  .who .sub { display: none; }
  .mobile-meta { grid-area: meta; display: block; font-size: 12.5px; color: var(--text-3); }
  .fab { position: fixed; right: 18px; bottom: calc(84px + env(safe-area-inset-bottom)); z-index: 25; display: grid; place-items: center; width: 52px; height: 52px; border: 0; border-radius: 50%; background: var(--inverse-bg); color: var(--inverse-text); box-shadow: 0 8px 28px rgba(0, 0, 0, 0.3); cursor: pointer; }
  .fab svg { width: 22px; height: 22px; }
}
</style>
