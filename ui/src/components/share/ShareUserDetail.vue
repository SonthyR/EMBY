<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Check, Copy, Pencil, Eye, EyeOff, MonitorSmartphone, ChevronRight, X } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { shareApi, shareUserToInput, type ShareSession, type ShareUser, type Site } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, daysUntil, dateOnly, formatBytes, relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from './shareStatus'

interface DraftSite {
  site_id: number
  site_account_id: number
}

const props = withDefaults(
  defineProps<{ open: boolean; user: ShareUser | null; sites?: Site[] }>(),
  { sites: () => [] },
)
const emit = defineEmits<{ close: []; edit: []; changed: []; deleted: []; copyShare: [] }>()
const detail = ref<ShareUser | null>(null)
const draftSites = ref<DraftSite[]>([])
const baselineAccounts = ref<Record<number, number>>({})
const busy = ref('')
const reveal = ref(false)
let detailReqGen = 0

const siteMap = computed(() => new Map(props.sites.map(s => [s.id, s])))

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

function assignedIndex(siteId: number): number {
  return draftSites.value.findIndex(s => s.site_id === siteId)
}

function toggleDraftSite(site: Site) {
  const idx = assignedIndex(site.id)
  if (idx >= 0) {
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
  } else {
    const acct = baselineAccounts.value[site.id] || 0
    draftSites.value = [...draftSites.value, { site_id: site.id, site_account_id: acct }]
  }
}

function promoteDraftSite(siteId: number) {
  if (!siteMap.value.get(siteId)?.enabled) {
    toast.error('已停用的源站无法设为首选生效站')
    return
  }
  const existing = draftSites.value.find(s => s.site_id === siteId)
  const target: DraftSite = existing || { site_id: siteId, site_account_id: baselineAccounts.value[siteId] || 0 }
  draftSites.value = [target, ...draftSites.value.filter(s => s.site_id !== siteId)]
}

function resetDraftSites() {
  syncDraftSites(detail.value)
}

async function saveSites() {
  const u = detail.value
  if (!u || !draftSites.value.length) return
  const ok = await run('sites', () => shareApi.update(u.id, shareUserToInput(u, { sites: draftSites.value })), '源站分配已更新')
  if (ok && detail.value?.id === u.id) {
    syncDraftSites(detail.value)
  }
}
</script>

<template>
  <UiDrawer :open="open" :width="460" @close="emit('close')">
    <template #title>
      <div v-if="detail" class="title">
        <h2>{{ detail.username }}</h2>
        <span class="state"><span class="dot" :class="STATUS_TONE[detail.status]" />{{ STATUS_LABEL[detail.status] }}</span>
      </div>
    </template>

    <template v-if="detail">
      <div class="primary-actions">
        <UiButton variant="primary" @click="emit('copyShare')"><Copy />复制分享信息</UiButton>
        <UiButton icon aria-label="编辑" title="编辑" :disabled="!!busy" @click="emit('edit')"><Pencil /></UiButton>
      </div>

      <section class="usage">
        <div class="big num">{{ formatBytes(detail.traffic_used) }}</div>
        <div class="faint">{{ detail.traffic_quota ? `共 ${formatBytes(detail.traffic_quota)}` : '流量不限' }}<template v-if="detail.next_reset_at"> · {{ detail.next_reset_at }} 重置</template></div>
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
          <dd>{{ detail.active_site_name }}</dd>
        </div>
        <div><dt>到期</dt><dd>{{ expiry.text }}<span v-if="expiry.sub" class="faint"> · {{ expiry.sub }}</span></dd></div>
        <div><dt>在线设备</dt><dd class="num">{{ detail.session_count }}{{ detail.max_devices ? ` / ${detail.max_devices} 台` : ' 台' }}</dd></div>
        <div><dt>限速</dt><dd>{{ detail.speed_limit ? `${detail.speed_limit} Mbps` : '不限' }}</dd></div>
        <div>
          <dt>密码</dt>
          <dd class="secret">
            <span class="mono">{{ reveal ? detail.password || '无法读取' : '••••••••' }}</span>
            <button type="button" :aria-label="reveal ? '隐藏' : '显示'" @click="reveal = !reveal"><EyeOff v-if="reveal" /><Eye v-else /></button>
            <button v-if="detail.password" type="button" aria-label="复制密码" @click="copyPassword"><Copy /></button>
          </dd>
        </div>
        <div v-if="detail.note"><dt>备注</dt><dd>{{ detail.note }}</dd></div>
      </dl>

      <section class="block sites-block">
        <div class="block-head">
          <h3>分配源站</h3>
          <span v-if="detail.route_prefix && draftSites.length > 1" class="faint hint">首位为首选生效源站</span>
        </div>
        <div v-if="sites.length" class="site-list">
          <div
            v-for="site in sites"
            :key="site.id"
            class="site-row"
            :class="{ on: assignedIndex(site.id) >= 0, off: !site.enabled }"
          >
            <button
              type="button"
              class="site-toggle"
              :disabled="!!busy"
              @click="toggleDraftSite(site)"
            >
              <span class="site-check" :class="{ checked: assignedIndex(site.id) >= 0 }">
                <Check v-if="assignedIndex(site.id) >= 0" />
              </span>
              <span class="site-name" :title="site.name">{{ site.name }}</span>
              <span v-if="!site.enabled" class="off-tag">停用</span>
              <span v-else-if="detail.route_prefix && detail.active_site_id === site.id && assignedIndex(site.id) >= 0" class="active-tag">生效</span>
              <span v-else-if="detail.route_prefix && draftSites.length > 1 && assignedIndex(site.id) === 0" class="prio-tag">首选</span>
            </button>
            <button
              v-if="detail.route_prefix && draftSites.length > 1 && assignedIndex(site.id) > 0 && site.enabled"
              type="button"
              class="pin-btn"
              :disabled="!!busy"
              title="设为首选生效源站"
              @click="promoteDraftSite(site.id)"
            >
              设为生效
            </button>
          </div>
        </div>
        <p v-else class="faint none">{{ detail.sites.map(s => s.site_name).join('、') || '暂无可用源站' }}</p>
        <div v-if="sitesDirty" class="site-save-bar">
          <UiButton size="sm" variant="primary" :loading="busy === 'sites'" :disabled="!!busy && busy !== 'sites'" @click="saveSites">保存源站分配</UiButton>
          <UiButton size="sm" variant="ghost" :disabled="!!busy" @click="resetDraftSites">取消</UiButton>
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
        <h3>管理</h3>
        <div class="menu">
          <button type="button" :disabled="!!busy" @click="renew(30)">续期 30 天<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="renew(365)">续期 1 年<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="resetTraffic">清零已用流量<ChevronRight /></button>
          <button type="button" :disabled="!!busy || !detail.session_count" @click="kick">断开所有设备<ChevronRight /></button>
          <button type="button" :disabled="!!busy" @click="toggle">{{ detail.enabled ? '停用账号' : '启用账号' }}<ChevronRight /></button>
          <button type="button" class="danger" :disabled="!!busy" @click="remove">删除账号<ChevronRight /></button>
        </div>
      </section>
    </template>
  </UiDrawer>
</template>

<style scoped>
.title { display: flex; align-items: center; gap: 12px; min-width: 0; }
.title h2 { margin: 0; font-size: 16px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.state { display: inline-flex; align-items: center; gap: 6px; font-size: 12.5px; color: var(--text-2); white-space: nowrap; }
.primary-actions { display: flex; gap: 8px; margin-bottom: 24px; }
.primary-actions > :first-child { flex: 1; }
.usage { display: flex; flex-direction: column; gap: 6px; padding-bottom: 20px; border-bottom: 1px solid var(--border); }
.big { font-size: 30px; font-weight: 600; letter-spacing: -0.03em; line-height: 1.15; }
.usage .faint { font-size: 13px; margin-bottom: 6px; }
.props { margin: 0; padding: 8px 0; border-bottom: 1px solid var(--border); }
.props div { display: flex; gap: 16px; padding: 9px 0; font-size: 13.5px; }
.props dt { flex: 0 0 72px; color: var(--text-3); }
.props dd { flex: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; }
.secret { display: flex; align-items: center; gap: 4px; }
.secret .mono { flex: 1; }
.secret button { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; }
.secret button:hover { background: var(--surface-hover); color: var(--text); }
.secret svg { width: 14px; height: 14px; }
.block { padding-top: 20px; }
.block-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 8px; }
.block-head h3 { margin: 0; }
.hint { font-size: 11.5px; }
h3 { margin: 0 0 8px; font-size: 12px; font-weight: 500; color: var(--text-3); }
.sites-block { padding-bottom: 18px; border-bottom: 1px solid var(--border); }
.site-list { display: flex; flex-direction: column; gap: 6px; }
.site-row { display: flex; align-items: center; gap: 8px; min-height: 36px; padding: 0 10px; border: 1px solid var(--border); border-radius: var(--radius-sm); background: var(--bg); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease); }
.site-row:hover { border-color: var(--text-3); }
.site-row.on { border-color: color-mix(in srgb, var(--text) 32%, var(--border)); background: var(--surface); }
.site-row.off { opacity: 0.65; }
.site-toggle { flex: 1; min-width: 0; height: 34px; display: flex; align-items: center; gap: 8px; padding: 0; border: 0; background: transparent; color: var(--text-2); font-size: 13px; text-align: left; cursor: pointer; }
.site-row.on .site-toggle { color: var(--text); font-weight: 500; }
.site-toggle:disabled { opacity: 0.55; cursor: default; }
.site-check { width: 15px; height: 15px; flex: 0 0 15px; display: grid; place-items: center; border: 1px solid var(--border); border-radius: 4px; background: var(--bg-elev); color: var(--bg); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease); }
.site-check.checked { border-color: var(--text); background: var(--text); }
.site-check svg { width: 11px; height: 11px; }
.site-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.active-tag { flex: 0 0 auto; font-size: 10.5px; padding: 1px 6px; border-radius: 999px; background: var(--good-soft); color: var(--good); font-weight: 500; }
.prio-tag { flex: 0 0 auto; font-size: 10.5px; padding: 1px 6px; border-radius: 999px; background: var(--surface-hover); color: var(--text-2); font-weight: 500; }
.off-tag { flex: 0 0 auto; font-size: 10.5px; padding: 1px 6px; border-radius: 999px; background: color-mix(in srgb, var(--bad) 12%, transparent); color: var(--bad); font-weight: 500; }
.pin-btn { flex: 0 0 auto; height: 24px; padding: 0 9px; border: 1px solid var(--border); border-radius: 999px; background: var(--bg-elev); color: var(--text-2); font-size: 11.5px; cursor: pointer; white-space: nowrap; }
.pin-btn:hover:not(:disabled) { border-color: var(--text); color: var(--text); }
.pin-btn:disabled { opacity: 0.5; cursor: default; }
.site-save-bar { display: flex; align-items: center; gap: 8px; margin-top: 12px; }
.devices { margin: 0; padding: 0; list-style: none; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 9px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.devices li:last-child { border-bottom: 0; }
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.devices small { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tag { display: inline-block; margin-left: 6px; padding: 0 6px; border-radius: 999px; font-size: 11px; vertical-align: 1px; background: var(--surface); color: var(--text-2); }
.tag.warn { background: var(--warn-soft); color: var(--warn); }
.tag.good { background: var(--good-soft); color: var(--good); }
.kick-one { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; flex: 0 0 26px; }
.kick-one:hover:not(:disabled) { background: var(--surface-hover); color: var(--bad); }
.kick-one:disabled { opacity: 0.4; cursor: default; }
.kick-one svg { width: 14px; height: 14px; color: inherit; }
.none { margin: 0; font-size: 13px; }
.menu { display: flex; flex-direction: column; margin: 0 -10px; }
.menu button { display: flex; align-items: center; justify-content: space-between; height: 38px; padding: 0 10px; border: 0; border-radius: var(--radius-sm); background: transparent; font-size: 13.5px; text-align: left; cursor: pointer; }
.menu button:hover:not(:disabled) { background: var(--surface-hover); }
.menu button:disabled { opacity: 0.4; cursor: default; }
.menu svg { width: 15px; height: 15px; color: var(--text-3); }
.menu .danger { color: var(--bad); }
</style>
