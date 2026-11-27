<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Copy, Pencil, Eye, EyeOff, MonitorSmartphone, ChevronRight, X, GripVertical, Plus, ArrowUpToLine } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { shareApi, shareUserToInput, type DanmakuSource, type ShareSession, type ShareUser, type Site } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, daysUntil, dateOnly, formatBytes, relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from './shareStatus'

interface DraftSite {
  site_id: number
  site_account_id: number
}

const props = withDefaults(
  defineProps<{ open: boolean; user: ShareUser | null; sites?: Site[]; danmakuSources?: DanmakuSource[] }>(),
  { sites: () => [], danmakuSources: () => [] },
)
const emit = defineEmits<{ close: []; edit: []; changed: []; deleted: []; copyShare: [] }>()
const detail = ref<ShareUser | null>(null)
const draftSites = ref<DraftSite[]>([])
const baselineAccounts = ref<Record<number, number>>({})
const busy = ref('')
const reveal = ref(false)
const draggingIdx = ref<number | null>(null)
const dragOverIdx = ref<number | null>(null)
const dragListEl = ref<HTMLElement | null>(null)
let detailReqGen = 0

const siteMap = computed(() => new Map(props.sites.map(s => [s.id, s])))

const assignedSites = computed(() =>
  draftSites.value
    .map(d => ({ draft: d, site: siteMap.value.get(d.site_id) }))
    .filter((entry): entry is { draft: DraftSite; site: Site } => !!entry.site),
)

const unassignedSites = computed(() => {
  const assignedSet = new Set(draftSites.value.map(d => d.site_id))
  return props.sites.filter(s => !assignedSet.has(s.id))
})

const assignedDanmakuIds = computed(() => new Set(detail.value?.danmaku_source_ids || []))
const assignedDanmakuList = computed(() => {
  const urlById = new Map((detail.value?.danmaku_sources || []).map(d => [d.id, d.url]))
  return props.danmakuSources
    .filter(d => assignedDanmakuIds.value.has(d.id))
    .map(d => ({ source: d, url: urlById.get(d.id) || '' }))
})
const unassignedDanmakuList = computed(() => props.danmakuSources.filter(d => !assignedDanmakuIds.value.has(d.id)))

async function assignDanmaku(dm: DanmakuSource) {
  const u = detail.value
  if (!u || busy.value) return
  const nextIds = [...new Set([...(u.danmaku_source_ids || []), dm.id])]
  await run(`dm-add-${dm.id}`, () => shareApi.update(u.id, shareUserToInput(u, { danmaku_source_ids: nextIds })), `已分配弹幕「${dm.name}」`)
}

async function unassignDanmaku(dm: DanmakuSource) {
  const u = detail.value
  if (!u || busy.value) return
  const nextIds = (u.danmaku_source_ids || []).filter(id => id !== dm.id)
  await run(`dm-rm-${dm.id}`, () => shareApi.update(u.id, shareUserToInput(u, { danmaku_source_ids: nextIds })), `已取消分配「${dm.name}」`)
}

function syncDraftSites(u: ShareUser | null) {
  const next: DraftSite[] = []
  const accountsMap: Record<number, number> = {}
  for (const s of u?.sites || []) {
    const acct = s.site_account_id || 0
    accountsMap[s.site_id] = acct
    next.push({ site_id: s.site_id, site_account_id: acct })
  }
  baselineAccounts.value = accountsMap
  draftSites.value = next
}

const sitesDirty = computed(() => {
  const orig = detail.value?.sites || []
  if (draftSites.value.length !== orig.length) return true
  return draftSites.value.some(
    (d, i) => d.site_id !== orig[i].site_id || (d.site_account_id || 0) !== (orig[i].site_account_id || 0),
  )
})

watch(
  () => [props.open, props.user] as const,
  async ([open, user], prev) => {
    const gen = ++detailReqGen
    if (!open || !user) return
    const userChanged = !prev?.[0] || detail.value?.id !== user.id
    if (detail.value?.id !== user.id) reveal.value = false
    detail.value = { ...user, sessions: detail.value?.id === user.id ? detail.value.sessions : undefined }
    if (userChanged || !sitesDirty.value) syncDraftSites(detail.value)
    try {
      const fresh = await shareApi.get(user.id)
      if (gen !== detailReqGen || !props.open || detail.value?.id !== user.id) return
      detail.value = fresh
      if (userChanged || !sitesDirty.value) syncDraftSites(fresh)
    } catch {
      /* list data is enough */
    }
  },
  { immediate: true },
)

const expiry = computed(() => {
  const u = detail.value
  if (!u?.expires_at) return { text: '永久有效', sub: '' }
  const days = daysUntil(u.expires_at)
  return { text: dateOnly(u.expires_at), sub: days === null ? '' : days < 0 ? '已过期' : days === 0 ? '今天到期' : `还剩 ${days} 天` }
})

async function run(key: string, action: () => Promise<unknown>, success: string): Promise<boolean> {
  const gen = ++detailReqGen
  const targetUserId = detail.value?.id
  busy.value = key
  try {
    await action()
    toast.success(success)
    emit('changed')
    if (targetUserId) {
      const fresh = await shareApi.get(targetUserId)
      if (gen === detailReqGen && props.open && detail.value?.id === targetUserId) {
        detail.value = fresh
      }
    }
    return true
  } catch (error) {
    toast.error(errorMessage(error))
    return false
  } finally {
    if (busy.value === key) busy.value = ''
  }
}
const toggle = () => {
  const u = detail.value!
  return run('toggle', () => shareApi.update(u.id, shareUserToInput(u, { enabled: !u.enabled })), u.enabled ? '已停用，在线设备已断开' : '已启用')
}
const renew = (days: number) => run('renew', () => shareApi.renew(detail.value!.id, days), days === 365 ? '已续期 1 年' : `已续期 ${days} 天`)
async function resetTraffic() {
  if (await confirm({ title: '清零已用流量？', message: '本周期的已用流量会归零，额度重新计算。', confirmText: '清零' })) run('reset', () => shareApi.resetTraffic(detail.value!.id), '已清零')
}
async function resetDanmakuToken() {
  const u = detail.value
  if (!u) return
  if (
    await confirm({
      title: `重置 ${u.username} 的弹幕链接？`,
      message: '旧的专属弹幕地址会立即失效，需在播放器中重新粘贴新地址；其他分享账号不受影响。',
      confirmText: '重置链接',
    })
  ) {
    await run('reset-danmaku', () => shareApi.resetDanmakuToken(u.id), '专属弹幕链接已重置')
  }
}
async function copyDanmakuURL(url: string) {
  if (url && (await copyText(url))) toast.success('专属弹幕地址已复制')
}
const kick = () => run('kick', () => shareApi.kick(detail.value!.id), '已断开所有设备')
async function kickOne(session: ShareSession) {
  if (!detail.value || !session.session_id) return
  await run(`kick-${session.session_id}`, () => shareApi.kickSession(detail.value!.id, session.session_id!), `已断开「${session.device_name || '该设备'}」`)
}
async function remove() {
  const u = detail.value!
  if (!(await confirm({ title: `删除 ${u.username}？`, message: '在线设备会立刻断开。这个操作无法撤销。', confirmText: '删除', danger: true }))) return
  busy.value = 'delete'
  try {
    await shareApi.remove(u.id)
    toast.success('已删除')
    emit('deleted')
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
  }
}
async function copyPassword() {
  if (detail.value?.password && (await copyText(detail.value.password))) toast.success('密码已复制')
}
async function copyRouteURL() {
  const url = detail.value?.route_url || detail.value?.route_prefix
  if (url && (await copyText(url))) toast.success('专属地址已复制')
}

function addDraftSite(site: Site) {
  if (busy.value) return
  if (draftSites.value.some(d => d.site_id === site.id)) return
  const acct = baselineAccounts.value[site.id] || 0
  draftSites.value = [...draftSites.value, { site_id: site.id, site_account_id: acct }]
}

function removeDraftSite(site: Site) {
  if (busy.value) return
  const remaining = draftSites.value.filter(s => s.site_id !== site.id)
  if (!remaining.length) {
    toast.error('至少保留一个源站')
    return
  }
  if (
    detail.value?.route_prefix &&
    site.enabled &&
    !remaining.some(item => siteMap.value.get(item.site_id)?.enabled)
  ) {
    toast.error('专属入口账号至少保留一个已启用源站')
    return
  }
  draftSites.value = remaining
}

function reorderDraft(fromIndex: number, toIndex: number) {
  if (fromIndex === toIndex || fromIndex < 0 || toIndex < 0) return
  if (fromIndex >= draftSites.value.length || toIndex >= draftSites.value.length) return
  const next = [...draftSites.value]
  const [moved] = next.splice(fromIndex, 1)
  next.splice(toIndex, 0, moved)
  draftSites.value = next
}

function promoteDraftSite(siteId: number) {
  if (!siteMap.value.get(siteId)?.enabled) {
    toast.error('已停用的源站无法设为首选生效站')
    return
  }
  const idx = draftSites.value.findIndex(s => s.site_id === siteId)
  if (idx > 0) reorderDraft(idx, 0)
}

function onDragStart(index: number, event: DragEvent) {
  if (busy.value || draftSites.value.length < 2) return
  draggingIdx.value = index
  dragOverIdx.value = index
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(index))
  }
}

function onDragOver(index: number, event: DragEvent) {
  event.preventDefault()
  if (draggingIdx.value === null) return
  dragOverIdx.value = index
  if (draggingIdx.value !== index) {
    reorderDraft(draggingIdx.value, index)
    draggingIdx.value = index
  }
}

function onDragEnd() {
  draggingIdx.value = null
  dragOverIdx.value = null
}

function onTouchStart(index: number) {
  if (busy.value || draftSites.value.length < 2) return
  draggingIdx.value = index
  dragOverIdx.value = index
}

function onTouchMove(event: TouchEvent) {
  if (draggingIdx.value === null || !dragListEl.value) return
  const touch = event.touches[0]
  if (!touch) return
  const rows = Array.from(dragListEl.value.querySelectorAll<HTMLElement>('[data-drag-row]'))
  for (let i = 0; i < rows.length; i++) {
    const rect = rows[i].getBoundingClientRect()
    if (touch.clientY >= rect.top && touch.clientY <= rect.bottom) {
      dragOverIdx.value = i
      if (i !== draggingIdx.value) {
        reorderDraft(draggingIdx.value, i)
        draggingIdx.value = i
      }
      break
    }
  }
}

function onTouchEnd() {
  draggingIdx.value = null
  dragOverIdx.value = null
}

function resetDraftSites() {
  syncDraftSites(detail.value)
}

async function saveSites() {
  const u = detail.value
  if (!u || !draftSites.value.length) return
  if (u.route_prefix && draftSites.value.length > 0 && !siteMap.value.get(draftSites.value[0].site_id)?.enabled) {
    toast.error('首选生效 Emby 源站必须为已启用状态，请拖入已启用的源站到首位')
    return
  }
  const msg = u.route_prefix ? '首选 Emby 服已保存（客户端回首页下拉刷新即可无感切换）' : '首选 Emby 服与源站分配已保存'
  const ok = await run('sites', () => shareApi.update(u.id, shareUserToInput(u, { sites: draftSites.value })), msg)
  if (ok && detail.value?.id === u.id) {
    syncDraftSites(detail.value)
  }
}
</script>

<template>
  <UiDrawer :open="open" :width="470" @close="emit('close')">
    <template #title>
      <div v-if="detail" class="title">
        <h2>{{ detail.username }}</h2>
        <span class="status-pill" :class="STATUS_TONE[detail.status]">
          <span class="dot" :class="STATUS_TONE[detail.status]" />
          {{ STATUS_LABEL[detail.status] }}
        </span>
      </div>
    </template>

    <template v-if="detail">
      <div class="primary-actions">
        <UiButton variant="primary" @click="emit('copyShare')"><Copy />复制分享信息</UiButton>
        <UiButton icon aria-label="编辑" title="编辑" :disabled="!!busy" @click="emit('edit')"><Pencil /></UiButton>
      </div>

      <section class="usage-card">
        <div class="big num">{{ formatBytes(detail.traffic_used) }}</div>
        <div class="faint">{{ detail.traffic_quota ? `总额度 ${formatBytes(detail.traffic_quota)}` : '流量不限' }}<template v-if="detail.next_reset_at"> · {{ detail.next_reset_at }} 重置</template></div>
        <UiProgress v-if="detail.traffic_quota" :value="detail.traffic_used" :max="detail.traffic_quota" />
      </section>

      <dl class="props">
        <div v-if="detail.route_prefix">
          <dt>专属地址</dt>
          <dd class="secret">
            <span class="mono">{{ detail.route_url || detail.route_prefix }}</span>
            <button type="button" aria-label="复制专属地址" @click="copyRouteURL"><Copy /></button>
          </dd>
        </div>
        <div v-if="detail.route_prefix && detail.active_site_name">
          <dt>当前生效</dt>
          <dd><span class="active-chip">{{ detail.active_site_name }}</span></dd>
        </div>
        <div><dt>到期时间</dt><dd>{{ expiry.text }}<span v-if="expiry.sub" class="faint"> · {{ expiry.sub }}</span></dd></div>
        <div><dt>登录设备</dt><dd class="num">{{ detail.session_count }}{{ detail.max_devices ? ` / ${detail.max_devices} 台` : ' 台' }}</dd></div>
        <div><dt>同时播放</dt><dd class="num">{{ detail.active_streams || 0 }}{{ detail.max_concurrent_streams ? ` / ${detail.max_concurrent_streams} 路` : ' 路（不限）' }}</dd></div>
        <div><dt>全局限速</dt><dd>{{ detail.speed_limit ? `${detail.speed_limit} Mbps` : '不限' }}</dd></div>
        <div>
          <dt>登录密码</dt>
          <dd class="secret">
            <span class="mono">{{ reveal ? detail.password || '无法读取' : '••••••••' }}</span>
            <button type="button" :aria-label="reveal ? '隐藏' : '显示'" @click="reveal = !reveal"><EyeOff v-if="reveal" /><Eye v-else /></button>
            <button v-if="detail.password" type="button" aria-label="复制密码" @click="copyPassword"><Copy /></button>
          </dd>
        </div>
        <div v-if="detail.danmaku_sources?.length">
          <dt>专属弹幕</dt>
          <dd class="dm-links">
            <div v-for="dm in detail.danmaku_sources" :key="dm.id" class="secret">
              <span class="mono" :title="dm.url">{{ dm.name }}：{{ dm.url }}</span>
              <button type="button" aria-label="复制弹幕地址" @click="copyDanmakuURL(dm.url)"><Copy /></button>
            </div>
          </dd>
        </div>
        <div v-if="detail.note"><dt>备注信息</dt><dd>{{ detail.note }}</dd></div>
      </dl>

      <section class="block sites-block">
        <div class="block-head">
          <h3>首选 Emby 服与源站优先级</h3>
          <span class="faint hint">{{ assignedSites.length > 1 ? '按住拖拽排序 · 第 1 位为首选' : '点击下方可继续添加' }}</span>
        </div>

        <div
          v-if="assignedSites.length"
          ref="dragListEl"
          class="drag-list"
          @touchmove.prevent="onTouchMove"
          @touchend="onTouchEnd"
          @touchcancel="onTouchEnd"
        >
          <div
            v-for="({ site }, index) in assignedSites"
            :key="site.id"
            data-drag-row
            class="drag-row"
            :class="{
              first: index === 0,
              off: !site.enabled,
              draggable: assignedSites.length > 1 && !busy,
              dragging: draggingIdx === index,
              over: dragOverIdx === index && draggingIdx !== null && draggingIdx !== index,
            }"
            :draggable="assignedSites.length > 1 && !busy"
            @dragstart="onDragStart(index, $event)"
            @dragover="onDragOver(index, $event)"
            @dragend="onDragEnd"
          >
            <span
              class="grip"
              :title="assignedSites.length > 1 ? '按住拖拽调整顺序' : ''"
              @touchstart.stop="onTouchStart(index)"
            >
              <GripVertical />
            </span>
            <span class="rank-pill num" :class="{ primary: index === 0 }">
              {{ index === 0 ? '首选' : `#${index + 1}` }}
            </span>
            <div class="site-info">
              <span class="site-name" :title="site.name">{{ site.name }}</span>
              <span v-if="!site.enabled" class="off-tag">停用</span>
              <span v-else-if="detail.route_prefix && detail.active_site_id === site.id && !sitesDirty" class="active-tag">生效中</span>
            </div>
            <div class="row-tools">
              <button
                v-if="index > 0 && site.enabled"
                type="button"
                class="tool-btn"
                :disabled="!!busy"
                title="一键置顶为首选"
                aria-label="置顶为首选"
                @click="promoteDraftSite(site.id)"
              >
                <ArrowUpToLine />
              </button>
              <button
                type="button"
                class="tool-btn remove"
                :disabled="!!busy"
                title="移出该源站"
                aria-label="移出该源站"
                @click="removeDraftSite(site)"
              >
                <X />
              </button>
            </div>
          </div>
        </div>
        <p v-else class="faint none">{{ detail.sites.map(s => s.site_name).join('、') || '暂无可用源站' }}</p>

        <div v-if="unassignedSites.length" class="unassigned-wrap">
          <span class="unassigned-label">可添加其他 Emby 源站</span>
          <div class="unassigned-chips">
            <button
              v-for="site in unassignedSites"
              :key="site.id"
              type="button"
              class="add-chip"
              :class="{ off: !site.enabled }"
              :disabled="!!busy"
              @click="addDraftSite(site)"
            >
              <Plus />
              <span>{{ site.name }}</span>
              <small v-if="!site.enabled">停用</small>
            </button>
          </div>
        </div>

        <div v-if="sitesDirty" class="site-save-bar">
          <UiButton size="sm" variant="accent" :loading="busy === 'sites'" :disabled="!!busy && busy !== 'sites'" @click="saveSites">保存源站顺序</UiButton>
          <UiButton size="sm" variant="ghost" :disabled="!!busy" @click="resetDraftSites">取消更改</UiButton>
        </div>
      </section>

      <section v-if="danmakuSources.length" class="block sites-block">
        <div class="block-head">
          <h3>分配的弹幕服务</h3>
          <span class="faint hint">跨站通用 · 随账号有效期联动</span>
        </div>
        <div v-if="assignedDanmakuList.length" class="drag-list">
          <div v-for="{ source, url } in assignedDanmakuList" :key="source.id" class="drag-row" :class="{ off: !source.enabled }">
            <div class="site-info">
              <span class="site-name" :title="source.name">{{ source.name }}</span>
              <span class="active-tag">{{ source.egress_name || (source.egress_node_id > 0 ? `节点 #${source.egress_node_id}` : '主机直出') }}</span>
              <span v-if="!source.enabled" class="off-tag">停用</span>
            </div>
            <div class="row-tools">
              <button
                v-if="url"
                type="button"
                class="tool-btn"
                :disabled="!!busy"
                title="复制该用户专属弹幕地址"
                aria-label="复制弹幕地址"
                @click="copyDanmakuURL(url)"
              >
                <Copy />
              </button>
              <button
                type="button"
                class="tool-btn remove"
                :disabled="!!busy"
                title="取消分配该弹幕源"
                aria-label="取消分配弹幕源"
                @click="unassignDanmaku(source)"
              >
                <X />
              </button>
            </div>
          </div>
        </div>
        <p v-else class="faint none">暂未分配弹幕服务</p>

        <div v-if="unassignedDanmakuList.length" class="unassigned-wrap">
          <span class="unassigned-label">点击分配弹幕服务</span>
          <div class="unassigned-chips">
            <button
              v-for="dm in unassignedDanmakuList"
              :key="dm.id"
              type="button"
              class="add-chip"
              :class="{ off: !dm.enabled }"
              :disabled="!!busy"
              @click="assignDanmaku(dm)"
            >
              <Plus />
              <span>{{ dm.name }}</span>
              <small v-if="!dm.enabled">停用</small>
            </button>
          </div>
        </div>
      </section>

      <section class="block">
        <h3>已登录设备</h3>
        <ul v-if="detail.sessions?.length" class="devices">
          <li v-for="(s, i) in detail.sessions" :key="s.session_id || i">
            <MonitorSmartphone />
            <div>
              <span>
                {{ s.device_name || '未知设备' }}
                <small v-if="s.draining" class="tag warn">观影后切换</small>
                <small v-else-if="s.playing" class="tag good">播放中{{ s.node_name ? ` · ${s.node_name}` : '' }}</small>
                <small v-else-if="s.node_name" class="tag">{{ s.node_name }}</small>
              </span>
              <small class="faint" :title="s.client_ip">{{ [s.client, s.site_name, s.client_region || s.client_ip].filter(Boolean).join(' · ') }}</small>
            </div>
            <small class="faint">{{ relativeTime(s.last_seen_at) }}</small>
            <button
              v-if="s.session_id"
              type="button"
              class="kick-one"
              :disabled="!!busy"
              title="断开该设备"
              aria-label="断开该设备"
              @click="kickOne(s)"
            >
              <X />
            </button>
          </li>
        </ul>
        <p v-else class="faint none">没有设备登录</p>
      </section>

      <section class="block">
        <h3>账号管理</h3>
        <div class="menu">
          <button type="button" :disabled="!!busy" @click="renew(30)">续期 30 天<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="renew(365)">续期 1 年<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="resetTraffic">清零已用流量<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="resetDanmakuToken">重置弹幕链接<ChevronRight /></button>
          <button type="button" :disabled="!!busy || !detail.session_count" @click="kick">断开所有设备<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="toggle">{{ detail.enabled ? '停用账号' : '启用账号' }}<ChevronRight /></button>
          <button type="button" class="danger" :disabled="!!busy" @click="remove">删除账号<ChevronRight /></button>
        </div>
      </section>
    </template>
  </UiDrawer>
</template>

<style scoped>
.title { display: flex; align-items: center; gap: 10px; min-width: 0; }
.title h2 { margin: 0; font-size: 17px; font-weight: 700; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 22px;
  padding: 0 9px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 600;
  white-space: nowrap;
  background: var(--surface-2);
  color: var(--text-2);
}
.status-pill.good { background: var(--good-soft); color: var(--good); }
.status-pill.warn { background: var(--warn-soft); color: var(--warn); }
.status-pill.bad { background: var(--bad-soft); color: var(--bad); }

.primary-actions { display: flex; gap: 8px; margin-bottom: 18px; }
.primary-actions > :first-child { flex: 1; }

.usage-card {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 16px 18px;
  border-radius: var(--radius);
  background: var(--surface-2);
  border: 1px solid var(--border);
  margin-bottom: 14px;
}
.big { font-size: 28px; font-weight: 700; letter-spacing: -0.03em; line-height: 1.15; }
.usage-card .faint { font-size: 12.5px; margin-bottom: 4px; }

.props { margin: 0; padding: 6px 0 12px; border-bottom: 1px solid var(--border); }
.props div { display: flex; align-items: center; gap: 14px; padding: 8px 0; font-size: 13.5px; }
.props dt { flex: 0 0 72px; color: var(--text-3); font-size: 12.5px; }
.props dd { flex: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; }
.active-chip {
  display: inline-flex;
  align-items: center;
  padding: 2px 9px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 12px;
  font-weight: 600;
}
.secret { display: flex; align-items: center; gap: 4px; }
.dm-links { display: flex; flex-direction: column; gap: 4px; }
.secret .mono { flex: 1; }
.secret button { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 50%; background: transparent; color: var(--text-3); cursor: pointer; }
.secret button:hover { background: var(--surface-hover); color: var(--text); }
.secret svg { width: 14px; height: 14px; }

.block { padding-top: 20px; }
.block-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 10px; }
.block-head h3 { margin: 0; }
.hint { font-size: 11.5px; }
h3 { margin: 0 0 10px; font-size: 12.5px; font-weight: 600; color: var(--text-2); }

.sites-block { padding-bottom: 20px; border-bottom: 1px solid var(--border); }
.drag-list { display: flex; flex-direction: column; gap: 8px; }
.drag-row {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  user-select: none;
  transition: transform 160ms var(--ease), border-color 160ms var(--ease), background 160ms var(--ease), box-shadow 160ms var(--ease), opacity 160ms var(--ease);
}
.drag-row.draggable { cursor: grab; }
.drag-row.draggable:active { cursor: grabbing; }
.drag-row.first {
  background: linear-gradient(90deg, var(--accent-soft) 0%, var(--surface) 75%);
  border-color: var(--accent-border);
}
.drag-row.off { opacity: 0.62; }
.drag-row.dragging {
  opacity: 0.55;
  transform: scale(0.985);
  border-style: dashed;
  border-color: var(--accent);
}
.drag-row.over {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
.grip {
  display: grid;
  place-items: center;
  color: var(--text-3);
  touch-action: none;
  flex: 0 0 auto;
}
.grip svg { width: 15px; height: 15px; }
.rank-pill {
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 34px;
  height: 22px;
  padding: 0 7px;
  border-radius: 999px;
  background: var(--surface);
  border: 1px solid var(--border);
  font-size: 11px;
  font-weight: 700;
  color: var(--text-2);
}
.rank-pill.primary {
  background: var(--accent);
  border-color: var(--accent);
  color: #ffffff;
}
.site-info {
  flex: 1;
  min-width: 0;
  display: flex;
  align-items: center;
  gap: 6px;
}
.site-name { font-size: 13.5px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.active-tag { flex: 0 0 auto; font-size: 10.5px; padding: 1px 7px; border-radius: 999px; background: var(--good-soft); color: var(--good); font-weight: 600; }
.off-tag { flex: 0 0 auto; font-size: 10.5px; padding: 1px 7px; border-radius: 999px; background: var(--bad-soft); color: var(--bad); font-weight: 600; }

.row-tools { display: flex; align-items: center; gap: 3px; flex: 0 0 auto; }
.tool-btn {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border: 0;
  border-radius: 50%;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
  transition: background var(--dur) var(--ease), color var(--dur) var(--ease);
}
.tool-btn:hover:not(:disabled) { background: var(--surface); color: var(--accent); }
.tool-btn.remove:hover:not(:disabled) { background: var(--bad-soft); color: var(--bad); }
.tool-btn:disabled { opacity: 0.4; cursor: default; }
.tool-btn svg { width: 14px; height: 14px; }

.unassigned-wrap { margin-top: 12px; display: flex; flex-direction: column; gap: 7px; }
.unassigned-label { font-size: 11.5px; color: var(--text-3); }
.unassigned-chips { display: flex; flex-wrap: wrap; gap: 6px; }
.add-chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 28px;
  padding: 0 11px;
  border: 1px dashed var(--border-strong);
  border-radius: 999px;
  background: transparent;
  color: var(--text-2);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.add-chip:hover:not(:disabled) { border-style: solid; border-color: var(--accent); background: var(--accent-soft); color: var(--accent); }
.add-chip.off { opacity: 0.6; }
.add-chip svg { width: 13px; height: 13px; }
.add-chip small { font-size: 10.5px; color: var(--bad); }

.site-save-bar {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 14px;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--accent-soft);
  border: 1px solid var(--accent-border);
}

.devices { margin: 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 6px; }
.devices li {
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: 13px;
}
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.devices small { font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tag { display: inline-block; margin-left: 6px; padding: 0 6px; border-radius: 999px; font-size: 10.5px; font-weight: 600; vertical-align: 1px; background: var(--surface); color: var(--text-2); }
.tag.warn { background: var(--warn-soft); color: var(--warn); }
.tag.good { background: var(--good-soft); color: var(--good); }
.kick-one { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 50%; background: transparent; color: var(--text-3); cursor: pointer; flex: 0 0 26px; }
.kick-one:hover:not(:disabled) { background: var(--bad-soft); color: var(--bad); }
.kick-one:disabled { opacity: 0.4; cursor: default; }
.kick-one svg { width: 14px; height: 14px; color: inherit; }
.none { margin: 0; font-size: 13px; }

.menu { display: flex; flex-direction: column; gap: 4px; }
.menu button {
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 40px;
  padding: 0 12px;
  border: 0;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  font-size: 13.5px;
  font-weight: 500;
  text-align: left;
  cursor: pointer;
  transition: background var(--dur) var(--ease);
}
.menu button:hover:not(:disabled) { background: var(--surface-hover); }
.menu button:disabled { opacity: 0.4; cursor: default; }
.menu svg { width: 15px; height: 15px; color: var(--text-3); }
.menu .danger { color: var(--bad); }
.menu .danger:hover:not(:disabled) { background: var(--bad-soft); }
</style>
