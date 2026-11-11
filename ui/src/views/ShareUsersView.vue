<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Plus, Search, Users, Activity, MonitorSmartphone, HardDriveDownload } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import ShareUserCard from '@/components/share/ShareUserCard.vue'
import ShareUserForm from '@/components/share/ShareUserForm.vue'
import ShareUserManage from '@/components/share/ShareUserManage.vue'
import ShareTextDialog from '@/components/share/ShareTextDialog.vue'
import { shareApi, sitesApi, type ShareStatus, type ShareUser, type Site } from '@/api'
import { copyText, formatBytes } from '@/utils/format'
import { errorMessage, toast } from '@/stores/toast'
import { STATUS_LABEL } from '@/components/share/shareStatus'

const users = ref<ShareUser[]>([])
const sites = ref<Site[]>([])
const loading = ref(true)
const loadError = ref('')
const query = ref('')
const filter = ref<'all' | ShareStatus>('all')

const formOpen = ref(false)
const editing = ref<ShareUser | null>(null)
const manageOpen = ref(false)
const managing = ref<ShareUser | null>(null)
const textOpen = ref(false)
const shareText = ref('')
const justCreated = ref(false)

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

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  // Usage and online devices change while friends watch; refresh quietly.
  timer = setInterval(() => {
    if (document.visibilityState === 'visible' && !formOpen.value) load(true)
  }, 15000)
})
onBeforeUnmount(() => clearInterval(timer))

const counts = computed(() => {
  const result: Record<string, number> = { all: users.value.length, active: 0, disabled: 0, expired: 0, quota: 0 }
  for (const user of users.value) result[user.status]++
  return result
})
const stats = computed(() => ({
  online: users.value.reduce((sum, u) => sum + (u.session_count || 0), 0),
  traffic: users.value.reduce((sum, u) => sum + (u.traffic_used || 0), 0),
}))
const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  return users.value.filter(user => {
    if (filter.value !== 'all' && user.status !== filter.value) return false
    if (!q) return true
    return [user.username, user.note, ...user.sites.map(s => s.site_name || '')].some(v => (v || '').toLowerCase().includes(q))
  })
})

function openCreate() {
  editing.value = null
  formOpen.value = true
}
function openEdit(user: ShareUser) {
  editing.value = user
  formOpen.value = true
}
function openManage(user: ShareUser) {
  managing.value = user
  manageOpen.value = true
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
      toast.success('分享账号已创建')
    }
  } else {
    toast.success('已保存')
  }
}

async function copyShare(user: ShareUser) {
  try {
    const { text } = await shareApi.text(user.id)
    if (await copyText(text)) toast.success('分享信息已复制，可以直接发给朋友')
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
  if (await copyText(link)) toast.success('查询链接已复制，朋友打开就能看到到期和流量')
  else toast.error('复制失败：' + link)
}

const filters: { key: 'all' | ShareStatus; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'active', label: STATUS_LABEL.active },
  { key: 'quota', label: STATUS_LABEL.quota },
  { key: 'expired', label: STATUS_LABEL.expired },
  { key: 'disabled', label: STATUS_LABEL.disabled },
]
</script>

<template>
  <div class="page">
    <section class="stats">
      <div class="stat"><Users /><div><strong>{{ counts.all }}</strong><span>分享账号</span></div></div>
      <div class="stat"><Activity /><div><strong>{{ counts.active }}</strong><span>正常使用</span></div></div>
      <div class="stat"><MonitorSmartphone /><div><strong>{{ stats.online }}</strong><span>在线设备</span></div></div>
      <div class="stat"><HardDriveDownload /><div><strong>{{ formatBytes(stats.traffic) }}</strong><span>本周期流量</span></div></div>
    </section>

    <section class="toolbar">
      <div class="search">
        <Search />
        <input v-model="query" class="input" type="search" placeholder="搜索用户名、备注或站点" aria-label="搜索分享账号" />
      </div>
      <div class="filters" role="tablist" aria-label="按状态筛选">
        <button
          v-for="f in filters"
          :key="f.key"
          type="button"
          role="tab"
          :aria-selected="filter === f.key"
          :class="{ active: filter === f.key }"
          @click="filter = f.key"
        >
          {{ f.label }}<span v-if="counts[f.key]" class="count">{{ counts[f.key] }}</span>
        </button>
      </div>
      <UiButton variant="primary" class="create" @click="openCreate"><Plus />新建分享账号</UiButton>
    </section>

    <div v-if="loading" class="grid">
      <div v-for="n in 3" :key="n" class="skeleton" />
    </div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!users.length" class="empty">
      <Users class="big" />
      <h2>还没有分享账号</h2>
      <p>给朋友建一个账号，他用这个账号在 Emby 客户端登录就能看片。服务器会替他登录源站，你的源站账号密码不会给出去。</p>
      <UiButton variant="primary" @click="openCreate"><Plus />新建第一个分享账号</UiButton>
    </div>
    <div v-else-if="!visible.length" class="empty small">
      <p>没有符合条件的分享账号。</p>
    </div>
    <TransitionGroup v-else name="list" tag="div" class="grid">
      <ShareUserCard
        v-for="user in visible"
        :key="user.id"
        :user="user"
        @edit="openEdit(user)"
        @manage="openManage(user)"
        @copy-share="copyShare(user)"
        @copy-portal="copyPortal(user)"
      />
    </TransitionGroup>

    <button type="button" class="fab" aria-label="新建分享账号" @click="openCreate"><Plus /></button>

    <ShareUserForm :open="formOpen" :user="editing" :sites="sites" @close="formOpen = false" @saved="onSaved" />
    <ShareUserManage
      :open="manageOpen"
      :user="managing"
      @close="manageOpen = false"
      @changed="load(true)"
      @deleted="
        () => {
          manageOpen = false
          load(true)
        }
      "
    />
    <ShareTextDialog :open="textOpen" :text="shareText" :created="justCreated" @close="textOpen = false" />
  </div>
</template>

<style scoped>
.page { display: flex; flex-direction: column; gap: 18px; position: relative; }
.stats { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
.stat {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 14px 16px;
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
}
.stat > svg { width: 20px; height: 20px; padding: 8px; box-sizing: content-box; border-radius: 10px; background: var(--accent-soft); color: var(--accent); }
.stat div { display: flex; flex-direction: column; min-width: 0; }
.stat strong { font-size: 19px; font-weight: 600; white-space: nowrap; }
.stat span { font-size: 12px; color: var(--text-3); }
.toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.search { position: relative; flex: 1 1 240px; max-width: 360px; }
.search svg { position: absolute; left: 11px; top: 50%; transform: translateY(-50%); width: 16px; height: 16px; color: var(--text-3); }
.search .input { padding-left: 34px; }
.filters { display: flex; gap: 4px; padding: 3px; border: 1px solid var(--border); border-radius: 10px; background: var(--surface); overflow-x: auto; scrollbar-width: none; }
.filters::-webkit-scrollbar { display: none; }
.filters button {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border: 0;
  border-radius: 7px;
  background: transparent;
  color: var(--text-2);
  font-size: 13px;
  white-space: nowrap;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.filters button.active { background: var(--accent-soft); color: var(--accent); font-weight: 500; }
.count { font-size: 11px; padding: 0 6px; border-radius: 999px; background: var(--surface-hover); color: var(--text-3); }
.filters button.active .count { background: var(--accent); color: var(--on-accent); }
.create { margin-left: auto; }
.grid { position: relative; display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 14px; }
.skeleton { height: 290px; border-radius: var(--radius-lg); background: linear-gradient(90deg, var(--surface) 0%, var(--surface-hover) 50%, var(--surface) 100%); background-size: 200% 100%; animation: shimmer 1.2s infinite linear; }
@keyframes shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }
.empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 10px;
  padding: 56px 20px;
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-lg);
  text-align: center;
}
.empty.small { padding: 32px 20px; }
.empty .big { width: 40px; height: 40px; color: var(--accent); }
.empty h2 { margin: 6px 0 0; font-size: 18px; }
.empty p { max-width: 440px; margin: 0; color: var(--text-2); line-height: 1.7; }
.fab { display: none; }

@media (max-width: 960px) { .stats { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 860px) {
  .create { display: none; }
  .search { max-width: none; flex-basis: 100%; }
  .filters { width: 100%; }
  .grid { grid-template-columns: minmax(0, 1fr); }
  .fab {
    position: fixed;
    right: 18px;
    bottom: calc(80px + env(safe-area-inset-bottom));
    z-index: 25;
    display: grid;
    place-items: center;
    width: 54px;
    height: 54px;
    border: 0;
    border-radius: 18px;
    background: var(--accent-strong);
    color: var(--on-accent);
    box-shadow: 0 8px 24px rgba(40, 80, 200, 0.4);
    cursor: pointer;
  }
  .fab svg { width: 24px; height: 24px; }
  .stat { padding: 12px; }
  .stat > svg { display: none; }
}
</style>
