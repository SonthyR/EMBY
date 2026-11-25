<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Plus, Search, Copy, Ticket, GripVertical } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import ShareUserForm from '@/components/share/ShareUserForm.vue'
import ShareUserDetail from '@/components/share/ShareUserDetail.vue'
import ShareTextDialog from '@/components/share/ShareTextDialog.vue'
import RedeemCodesPanel from '@/components/share/RedeemCodesPanel.vue'
import { shareApi, sitesApi, type ShareStatus, type ShareUser, type Site } from '@/api'
import { usePointerSort } from '@/composables/usePointerSort'
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
const viewMode = ref<'users' | 'codes'>('users')

const formOpen = ref(false)
const editing = ref<ShareUser | null>(null)
const selectedId = ref<number | null>(null)
const textOpen = ref(false)
const shareText = ref('')
const justCreated = ref(false)

const selected = computed(() => users.value.find(u => u.id === selectedId.value) || null)
const savingOrder = ref(false)
const pointerSort = usePointerSort()
let listLoadGen = 0

async function load(silent = false) {
  if (silent && (pointerSort.isDragging.value || savingOrder.value)) return
  const gen = ++listLoadGen
  if (!silent) loading.value = true
  try {
    const [list, siteList] = await Promise.all([shareApi.list(), sitesApi.list()])
    if (gen !== listLoadGen) return
    users.value = list
    sites.value = siteList
    loadError.value = ''
  } catch (error) {
    if (gen !== listLoadGen) return
    loadError.value = errorMessage(error, '读取失败')
  } finally {
    if (gen === listLoadGen) loading.value = false
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
    return !q || [user.username, user.route_prefix || '', user.note, ...user.sites.map(s => s.site_name || '')].some(v => (v || '').toLowerCase().includes(q))
  })
})
const canSort = computed(() => filter.value === 'all' && !query.value.trim() && users.value.length > 1 && !savingOrder.value)

function openUserDetail(user: ShareUser) {
  if (pointerSort.shouldSuppressClick()) return
  selectedId.value = user.id
}

function startUserSort(event: PointerEvent, user: ShareUser) {
  if (!canSort.value) return
  const itemEl = (event.currentTarget as HTMLElement | null)?.closest<HTMLElement>('[data-share-sort-id]')
  const containerEl = itemEl?.closest<HTMLElement>('.table')
  if (!itemEl || !containerEl) return

  pointerSort.start<ShareUser>({
    event,
    itemEl,
    containerEl,
    itemSelector: '[data-share-sort-id]',
    getKeyFromEl: el => {
      const id = Number(el.dataset.shareSortId)
      return Number.isNaN(id) ? null : id
    },
    activeKey: user.id,
    scope: 'share-users',
    mode: 'list',
    getList: () => users.value,
    getItemKey: item => item.id,
    applyList: next => {
      users.value = next
    },
    onCommit: async (next, previous) => {
      savingOrder.value = true
      try {
        await shareApi.reorder(next.map(u => u.id))
        toast.success('分享账号顺序已保存')
      } catch (error) {
        users.value = previous
        toast.error(errorMessage(error, '保存分享账号顺序失败'))
      } finally {
        savingOrder.value = false
      }
    },
  })
}

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
    <PageHeader title="分享账号" description="给朋友发放登录账号，服务器会用你的源站账号代为登录。点击账号可拖拽调整首选 Emby 服。">
      <div class="head-acts">
        <UiButton :variant="viewMode === 'codes' ? 'primary' : 'secondary'" @click="viewMode = viewMode === 'codes' ? 'users' : 'codes'">
          <Ticket />{{ viewMode === 'codes' ? '返回账号列表' : '兑换码' }}
        </UiButton>
        <UiButton v-if="viewMode === 'users'" variant="primary" @click="openCreate"><Plus />新建账号<kbd class="kbd-on-inverse">N</kbd></UiButton>
      </div>
    </PageHeader>

    <RedeemCodesPanel v-if="viewMode === 'codes'" :sites="sites" />

    <template v-else>
      <dl class="kpi-grid">
        <div class="kpi-card"><dt>分享账号</dt><dd class="num">{{ counts.all }}</dd></div>
        <div class="kpi-card"><dt><span class="dot good" />正常可用</dt><dd class="num">{{ counts.active }}</dd></div>
        <div class="kpi-card"><dt>在线设备</dt><dd class="num">{{ online }}</dd></div>
        <div class="kpi-card"><dt>本周期流量</dt><dd class="num">{{ formatBytes(traffic) }}</dd></div>
      </dl>

      <div class="bento-table-card">
        <div class="toolbar">
          <nav class="seg" aria-label="按状态筛选">
            <button v-for="f in FILTERS" :key="f.key" type="button" :class="{ on: filter === f.key }" @click="filter = f.key">
              {{ f.label }}<span v-if="counts[f.key]" class="n">{{ counts[f.key] }}</span>
            </button>
          </nav>
          <label class="search">
            <Search />
            <input v-model="query" data-page-search type="search" placeholder="搜索账号、备注或源站…" aria-label="搜索分享账号" />
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
            <span role="columnheader">账号与首选 Emby 服</span>
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
              :data-share-sort-id="user.id"
              class="row item"
              :class="{
                active: selectedId === user.id,
                dim: user.status !== 'active',
                dragging: pointerSort.draggingScope.value === 'share-users' && pointerSort.draggingKey.value === user.id,
                'is-sort-placeholder': pointerSort.draggingScope.value === 'share-users' && pointerSort.draggingKey.value === user.id,
              }"
              role="row"
              tabindex="0"
              @click="openUserDetail(user)"
              @keydown.enter="openUserDetail(user)"
            >
              <span class="who-cell" role="cell">
                <button
                  v-if="canSort"
                  type="button"
                  class="drag-handle"
                  title="按住拖动调整顺序"
                  aria-label="拖动调整顺序"
                  @click.stop
                  @pointerdown.stop="startUserSort($event, user)"
                >
                  <GripVertical />
                </button>
                <span class="who">
                  <span class="who-top">
                    <span class="name">{{ user.username }}</span>
                    <span v-if="user.active_site_name || user.sites[0]?.site_name" class="emby-chip">
                      {{ user.active_site_name || user.sites[0]?.site_name }}
                    </span>
                  </span>
                  <span class="sub">{{ [user.route_prefix, user.note || user.sites.map(s => s.site_name).join('、')].filter(Boolean).join(' · ') }}</span>
                </span>
              </span>
              <span class="status" role="cell">
                <span class="status-pill" :class="STATUS_TONE[user.status]">
                  <span class="dot" :class="STATUS_TONE[user.status]" />
                  {{ STATUS_LABEL[user.status] }}
                </span>
              </span>
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
      </div>
    </template>

    <ShareUserDetail
      :open="!!selected && !formOpen"
      :user="selected"
      :sites="sites"
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
    />
    <ShareUserForm :open="formOpen" :user="editing" :sites="sites" @close="formOpen = false" @saved="onSaved" />
    <ShareTextDialog :open="textOpen" :text="shareText" :created="justCreated" @close="textOpen = false" />
  </div>
</template>

<style scoped>
.head-acts { display: flex; align-items: center; gap: 8px; }
.kbd-on-inverse { margin-left: 4px; background: transparent; border-color: color-mix(in srgb, var(--inverse-text) 28%, transparent); color: color-mix(in srgb, var(--inverse-text) 72%, transparent); }

.kpi-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin: 0 0 18px; }
.kpi-card { padding: 16px 20px; border-radius: var(--radius-lg); background: var(--surface); border: 1px solid var(--border); box-shadow: var(--shadow-card); display: flex; flex-direction: column; gap: 6px; }
.kpi-card dt { display: inline-flex; align-items: center; gap: 7px; font-size: 12.5px; font-weight: 500; color: var(--text-2); }
.kpi-card dd { margin: 0; font-size: 24px; font-weight: 700; letter-spacing: -0.025em; }

.bento-table-card {
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
  overflow: hidden;
}

.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 14px 18px;
  border-bottom: 1px solid var(--border);
}
.seg {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 3px;
  border-radius: 999px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  overflow-x: auto;
  scrollbar-width: none;
}
.seg::-webkit-scrollbar { display: none; }
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
  white-space: nowrap;
  cursor: pointer;
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
.search input::placeholder { color: var(--text-3); }

.table { position: relative; padding: 6px 10px 10px; }
.row { display: grid; grid-template-columns: minmax(0, 2.3fr) 108px minmax(0, 1.5fr) 86px 76px 40px; align-items: center; gap: 14px; padding: 0 12px; }
.row.head { height: 36px; font-size: 12px; font-weight: 600; color: var(--text-3); }
.row.item { min-height: 62px; padding: 10px 12px; border-radius: var(--radius-sm); cursor: pointer; outline: none; transition: background var(--dur) var(--ease); }
.row.item + .row.item { box-shadow: inset 0 1px 0 var(--border); }
.row.item:hover, .row.item:focus-visible, .row.item.active { background: var(--surface-2); box-shadow: none; }
.row.item:hover + .row.item, .row.item.active + .row.item { box-shadow: none; }
.row.item.dragging { opacity: 0.24; background: var(--accent-soft); outline: 1px dashed var(--accent); box-shadow: none; }
.row.dim .who .name { color: var(--text-2); }

.who-cell { display: flex; align-items: center; gap: 8px; min-width: 0; }
.drag-handle {
  display: inline-grid;
  place-items: center;
  width: 24px;
  height: 24px;
  margin-left: -4px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--text-3);
  cursor: grab;
  touch-action: none;
  flex: 0 0 auto;
  transition: color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.drag-handle:hover { color: var(--text); background: var(--surface-hover); }
.drag-handle:active { cursor: grabbing; }
.drag-handle svg { width: 15px; height: 15px; }
.who { display: flex; flex-direction: column; gap: 3px; min-width: 0; flex: 1; }
.who-top { display: flex; align-items: center; gap: 7px; min-width: 0; }
.name { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.emby-chip {
  flex: 0 0 auto;
  padding: 1px 8px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 11px;
  font-weight: 600;
}
.sub { font-size: 12px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 23px;
  padding: 0 9px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
  background: var(--surface-2);
  color: var(--text-2);
}
.status-pill.good { background: var(--good-soft); color: var(--good); }
.status-pill.warn { background: var(--warn-soft); color: var(--warn); }
.status-pill.bad { background: var(--bad-soft); color: var(--bad); }

.traffic { display: flex; flex-direction: column; gap: 6px; font-size: 13px; }
.expiry, .devices { font-size: 13px; font-weight: 500; }
.expiry.warn { color: var(--warn); }
.expiry.bad { color: var(--bad); }
.act { opacity: 0.45; transition: opacity var(--dur) var(--ease); }
.row.item:hover .act, .row.item.active .act, .row.item:focus-within .act { opacity: 1; }
.mobile-meta { display: none; }
.skeleton { height: 60px; }
.skeleton span { grid-column: 1 / -1; height: 14px; width: 42%; border-radius: 6px; background: var(--surface-hover); animation: pulse 1.2s ease-in-out infinite; }
@keyframes pulse { 50% { opacity: 0.4; } }
.none { padding: 36px 0; text-align: center; color: var(--text-3); font-size: 13.5px; }
.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 64px 20px; text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 700; }
.empty p { max-width: 380px; margin: 0 0 8px; color: var(--text-2); }

@media (max-width: 900px) {
  .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .toolbar { flex-direction: column-reverse; align-items: stretch; }
  .search { width: 100%; }
  .search kbd { display: none; }
  .row.head { display: none; }
  .row.item { grid-template-columns: minmax(0, 1fr) auto; grid-template-areas: 'who status' 'meta meta'; gap: 6px 12px; padding: 12px; }
  .who-cell { grid-area: who; }
  .status { grid-area: status; }
  .traffic, .expiry, .devices, .act { display: none; }
  .who .sub { display: none; }
  .mobile-meta { grid-area: meta; display: block; font-size: 12px; color: var(--text-3); }
}
</style>
