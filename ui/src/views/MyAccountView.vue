<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ArrowUpToLine, Copy, GripVertical, LogOut, MonitorSmartphone, Moon, ShieldBan, Sun, SunMoon, Wifi, X } from '@lucide/vue'
import logo from '@/assets/logo.svg'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { meApi, type MyAccount, type MyAccountServer, type ShareSession } from '@/api'
import { logout, session } from '@/stores/session'
import { confirm } from '@/stores/confirm'
import { cycleTheme, themeMode } from '@/stores/theme'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, daysUntil, dateOnly, formatBytes, relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from '@/components/share/shareStatus'

const me = ref<MyAccount | null>(null)
const loadError = ref('')
const pw = reactive({ open: false, current: '', next: '', busy: false, error: '' })

const draggingId = ref<number | null>(null)
const dragOrderBefore = ref<number[]>([])
const savingOrder = ref(false)
const kickingSessionId = ref('')
let touchDragActive = false

const networkLabel = computed(() => {
  const n = me.value?.network
  if (!n) return ''
  return [n.region, n.isp].filter(Boolean).join(' · ') || n.client_ip || ''
})

const fixedAddress = computed(() => {
  const u = me.value
  if (!u?.route_prefix) return ''
  return u.route_url || (typeof window !== 'undefined' ? `${window.location.origin}${u.route_prefix}` : u.route_prefix)
})

const hasPersonalRoute = computed(() => !!fixedAddress.value)
const canReorder = computed(() => (me.value?.servers?.length || 0) > 1)

async function load() {
  if (draggingId.value !== null || savingOrder.value) return
  try {
    me.value = await meApi.get()
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取失败')
  }
}

let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  load()
  timer = setInterval(() => document.visibilityState === 'visible' && load(), 30000)
})
onBeforeUnmount(() => clearInterval(timer))

const headline = computed(() => {
  const u = me.value
  if (!u) return ''
  if (u.status === 'active') return '账号正常，可以随时观看'
  if (u.status === 'expired') return '账号已过期，请联系分享人续期'
  if (u.status === 'quota') return '本期流量已用完'
  return '账号已被停用'
})

const expiry = computed(() => {
  const u = me.value
  if (!u?.expires_at) return { main: '永久', sub: '' }
  const days = daysUntil(u.expires_at)
  return { main: dateOnly(u.expires_at), sub: days === null ? '' : days < 0 ? '已过期' : days === 0 ? '今天到期' : `还剩 ${days} 天` }
})

function reorderLocal(sourceId: number, targetId: number) {
  if (!me.value || sourceId === targetId) return
  const list = [...me.value.servers]
  const from = list.findIndex(item => item.site_id === sourceId)
  const to = list.findIndex(item => item.site_id === targetId)
  if (from < 0 || to < 0) return
  const [moved] = list.splice(from, 1)
  list.splice(to, 0, moved)
  me.value = { ...me.value, servers: list }
}

async function commitOrder(previousIds: number[], previousActiveId?: number) {
  if (!me.value) return
  const nextIds = me.value.servers.map(item => item.site_id)
  if (nextIds.length === previousIds.length && nextIds.every((id, idx) => id === previousIds[idx])) return
  savingOrder.value = true
  try {
    const res = await meApi.reorderSites(nextIds)
    me.value = res
    const activeName = res.active_site_name || res.servers.find(s => s.active)?.name || ''
    if (res.draining > 0 && activeName) {
      toast.success(`已将「${activeName}」设为首选，当前视频关闭后自动生效`)
    } else if (res.revoked > 0 && activeName) {
      toast.success(`已切换为「${activeName}」，回客户端重新登录即可`)
    } else if (hasPersonalRoute.value && res.active_site_id && res.active_site_id !== previousActiveId && activeName) {
      toast.success(`已切换为「${activeName}」，回客户端重新登录即可`)
    } else {
      toast.success('服务器顺序已更新')
    }
  } catch (error) {
    toast.error(errorMessage(error, '排序保存失败'))
    await load()
  } finally {
    savingOrder.value = false
  }
}

function onDragStart(event: DragEvent, server: MyAccountServer) {
  if (!canReorder.value || savingOrder.value || !me.value) return
  draggingId.value = server.site_id
  dragOrderBefore.value = me.value.servers.map(item => item.site_id)
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(server.site_id))
  }
}

function onDragOver(event: DragEvent, target: MyAccountServer) {
  if (draggingId.value === null) return
  event.preventDefault()
  if (event.dataTransfer) event.dataTransfer.dropEffect = 'move'
  if (draggingId.value !== target.site_id) reorderLocal(draggingId.value, target.site_id)
}

function onDrop(event: DragEvent) {
  event.preventDefault()
}

async function onDragEnd() {
  if (draggingId.value === null) return
  const prev = [...dragOrderBefore.value]
  const prevActive = me.value?.active_site_id
  draggingId.value = null
  dragOrderBefore.value = []
  await commitOrder(prev, prevActive)
}

function onTouchStart(event: TouchEvent, server: MyAccountServer) {
  if (!canReorder.value || savingOrder.value || !me.value || event.touches.length !== 1) return
  touchDragActive = true
  draggingId.value = server.site_id
  dragOrderBefore.value = me.value.servers.map(item => item.site_id)
}

function onTouchMove(event: TouchEvent) {
  if (!touchDragActive || draggingId.value === null || event.touches.length !== 1) return
  event.preventDefault()
  const touch = event.touches[0]
  const element = document.elementFromPoint(touch.clientX, touch.clientY)?.closest<HTMLElement>('[data-site-id]')
  const targetId = element ? Number(element.dataset.siteId) : 0
  if (targetId && targetId !== draggingId.value) reorderLocal(draggingId.value, targetId)
}

async function onTouchEnd() {
  if (!touchDragActive) return
  touchDragActive = false
  await onDragEnd()
}

async function promoteToTop(server: MyAccountServer) {
  if (!me.value || savingOrder.value) return
  const prev = me.value.servers.map(item => item.site_id)
  const prevActive = me.value.active_site_id
  if (prev[0] === server.site_id) return
  const list = [server, ...me.value.servers.filter(item => item.site_id !== server.site_id)]
  me.value = { ...me.value, servers: list }
  await commitOrder(prev, prevActive)
}

async function copy(value: string) {
  if (await copyText(value)) toast.success('已复制')
}

async function kick() {
  if (!(await confirm({ title: '断开所有设备？', message: '所有设备上的 Emby 都会退出登录，需要重新输入账号密码。', confirmText: '断开' }))) return
  try {
    await meApi.kick()
    toast.success('已断开')
    load()
  } catch (error) {
    toast.error(errorMessage(error))
  }
}

async function kickOne(sessionItem: ShareSession) {
  if (!sessionItem.session_id || kickingSessionId.value) return
  kickingSessionId.value = sessionItem.session_id
  try {
    await meApi.kickSession(sessionItem.session_id)
    toast.success(`已断开「${sessionItem.device_name || '该设备'}」`)
    await load()
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    kickingSessionId.value = ''
  }
}

async function changePassword() {
  pw.error = ''
  if (!pw.current) return (pw.error = '请输入当前密码')
  if ([...pw.next].length < 4) return (pw.error = '新密码至少 4 个字符')
  pw.busy = true
  try {
    await meApi.password(pw.current, pw.next)
    toast.success('密码已修改，请在 Emby 客户端用新密码重新登录')
    Object.assign(pw, { open: false, current: '', next: '' })
    load()
  } catch (error) {
    pw.error = errorMessage(error)
  } finally {
    pw.busy = false
  }
}
</script>

<template>
  <div class="me">
    <header class="bar">
      <div class="brand"><img :src="logo" alt="" />云渡</div>
      <div class="right">
        <span class="who">{{ session.username }}</span>
        <button type="button" class="icon" aria-label="切换主题" @click="cycleTheme">
          <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
        </button>
        <button type="button" class="icon" aria-label="退出登录" title="退出登录" @click="logout"><LogOut /></button>
      </div>
    </header>

    <main class="content">
      <p v-if="loadError && !me" class="faint">{{ loadError }}</p>
      <template v-if="me">
        <section class="hero">
          <span class="state"><span class="dot" :class="STATUS_TONE[me.status]" />{{ STATUS_LABEL[me.status] }}</span>
          <h1>{{ headline }}</h1>
          <div v-if="me.network?.blocked" class="net-alert">
            <ShieldBan class="net-ico" />
            <div>
              <strong>当前网络已被禁止连接 Emby</strong>
              <p v-if="me.network.block_reason === 'non_mainland'">
                检测到当前访问来源为「{{ networkLabel }}」（非中国大陆 IP）。如果你开启了海外代理或 VPN，请先关闭或将本站点加入直连规则后再播放。
              </p>
              <p v-else>
                检测到当前运营商线路「{{ me.network.isp_label || networkLabel }}」已被管理员禁止接入，请联系分享人处理。
              </p>
            </div>
          </div>
          <div v-else-if="me.network && (networkLabel || me.network.node_name)" class="net-pill">
            <Wifi class="net-ico" />
            <span>
              当前网络：<strong>{{ networkLabel || '已识别' }}</strong>
              <template v-if="me.network.node_name"> · 播放节点：<strong>{{ me.network.node_name }}</strong></template>
            </span>
          </div>
        </section>

        <section class="usage">
          <div class="label">本期已用</div>
          <div class="big num">{{ formatBytes(me.traffic_used) }}<span class="of">{{ me.traffic_quota ? ` / ${formatBytes(me.traffic_quota)}` : '' }}</span></div>
          <UiProgress v-if="me.traffic_quota" :value="me.traffic_used" :max="me.traffic_quota" />
          <p class="faint">{{ me.traffic_quota ? (me.next_reset_at ? `${me.next_reset_at} 自动清零` : '流量额度不会自动清零') : '流量不限' }}</p>
        </section>

        <dl class="facts">
          <div><dt>到期</dt><dd>{{ expiry.main }}<small v-if="expiry.sub">{{ expiry.sub }}</small></dd></div>
          <div><dt>在线设备</dt><dd class="num">{{ me.session_count }}<small>{{ me.max_devices ? `最多 ${me.max_devices} 台` : '不限台数' }}</small></dd></div>
          <div><dt>速度</dt><dd>{{ me.speed_limit ? `${me.speed_limit} Mbps` : '不限速' }}</dd></div>
        </dl>

        <template v-if="hasPersonalRoute">
          <section class="block">
            <h2>客户端固定地址</h2>
            <p class="faint hint">在 Emby 客户端里只填这一个地址，用你的账号密码登录。想换服务器时把它拖到下方第一位，回客户端重新登录即可。</p>
            <div class="fixed-addr">
              <span class="mono">{{ fixedAddress }}</span>
              <UiButton variant="primary" size="sm" @click="copy(fixedAddress)"><Copy />复制</UiButton>
            </div>
            <p v-if="me.draining_sessions" class="drain-note">
              有设备正在播放视频，不会立即切断；关闭视频后将自动切换为「{{ me.active_site_name || '新服务器' }}」，届时在客户端重新登录即可。
            </p>
          </section>

          <section class="block">
            <div class="block-head">
              <h2>我的服务器</h2>
              <span v-if="canReorder" class="faint sub-hint">拖动排序 · 首位生效</span>
            </div>
            <p v-if="!me.servers?.length" class="faint none">暂无可用服务器</p>
            <TransitionGroup v-else name="list" tag="ul" class="sortable-servers">
              <li
                v-for="(server, index) in me.servers"
                :key="server.site_id"
                :data-site-id="server.site_id"
                class="server-card"
                :class="{ current: server.active, disabled: !server.enabled, dragging: draggingId === server.site_id, movable: canReorder }"
                :draggable="canReorder && !savingOrder"
                @dragstart="onDragStart($event, server)"
                @dragover="onDragOver($event, server)"
                @drop="onDrop"
                @dragend="onDragEnd"
              >
                <span
                  v-if="canReorder"
                  class="grip"
                  aria-label="拖动排序"
                  title="按住拖动排序"
                  @touchstart="onTouchStart($event, server)"
                  @touchmove="onTouchMove"
                  @touchend="onTouchEnd"
                  @touchcancel="onTouchEnd"
                >
                  <GripVertical />
                </span>
                <span class="rank num">{{ index + 1 }}</span>
                <div class="server-info">
                  <span class="server-name">{{ server.name }}</span>
                  <small v-if="server.active" class="server-state good"><span class="dot good" />当前生效</small>
                  <small v-else-if="!server.enabled" class="faint">暂不可用</small>
                  <small v-else class="faint">拖到第一位即可切换</small>
                </div>
                <UiButton
                  v-if="index > 0 && server.enabled"
                  variant="ghost"
                  size="sm"
                  :disabled="savingOrder"
                  @click="promoteToTop(server)"
                >
                  <ArrowUpToLine />设为当前
                </UiButton>
              </li>
            </TransitionGroup>
          </section>
        </template>

        <section v-else class="block">
          <h2>服务器地址</h2>
          <p class="faint hint">在 Emby 客户端里添加下面的地址，用你的账号和密码登录。</p>
          <ul class="servers">
            <li v-for="server in me.servers.filter(s => s.enabled)" :key="server.site_id">
              <span class="name">{{ server.name }}</span>
              <div v-for="addr in server.addresses" :key="addr" class="addr">
                <span class="mono">{{ addr }}</span>
                <UiButton variant="ghost" size="sm" icon aria-label="复制地址" @click="copy(addr)"><Copy /></UiButton>
              </div>
            </li>
            <li v-if="!me.servers?.some(s => s.enabled)" class="faint">暂无可用服务器</li>
          </ul>
        </section>

        <section class="block">
          <div class="block-head">
            <h2>已登录设备</h2>
            <UiButton v-if="me.sessions?.length" variant="ghost" size="sm" @click="kick">全部断开</UiButton>
          </div>
          <ul class="devices">
            <li v-for="(s, i) in me.sessions || []" :key="s.session_id || i">
              <MonitorSmartphone />
              <div>
                <span>
                  {{ s.device_name || '未知设备' }}
                  <small v-if="s.draining" class="tag warn">观影后切换</small>
                  <small v-else-if="s.playing" class="tag good">播放中{{ s.node_name ? ` · ${s.node_name}` : '' }}</small>
                  <small v-else-if="s.node_name" class="tag">{{ s.node_name }}</small>
                </span>
                <small class="faint">{{ [s.client, s.site_name, s.client_region].filter(Boolean).join(' · ') }}</small>
              </div>
              <small class="faint">{{ relativeTime(s.last_seen_at) }}</small>
              <button
                v-if="s.session_id"
                type="button"
                class="kick-one"
                :disabled="kickingSessionId === s.session_id"
                title="断开该设备"
                aria-label="断开该设备"
                @click="kickOne(s)"
              >
                <X />
              </button>
            </li>
            <li v-if="!me.sessions?.length" class="faint none">没有设备登录</li>
          </ul>
        </section>

        <section class="block">
          <div class="block-head">
            <h2>密码</h2>
            <UiButton v-if="!pw.open" variant="ghost" size="sm" @click="pw.open = true">修改</UiButton>
          </div>
          <form v-if="pw.open" class="pw" @submit.prevent="changePassword">
            <UiField label="当前密码" for="me-cur"><input id="me-cur" v-model="pw.current" class="input" type="password" autocomplete="current-password" /></UiField>
            <UiField label="新密码" for="me-new" help="修改后所有设备需要用新密码重新登录。"><input id="me-new" v-model="pw.next" class="input" type="password" autocomplete="new-password" /></UiField>
            <p v-if="pw.error" class="err">{{ pw.error }}</p>
            <div class="pw-actions">
              <UiButton @click="pw.open = false">取消</UiButton>
              <UiButton variant="primary" type="submit" :loading="pw.busy">保存</UiButton>
            </div>
          </form>
          <p v-else class="faint none">Emby 客户端和这个页面用的是同一个密码。</p>
        </section>
      </template>
    </main>
  </div>
</template>

<style scoped>
.me { min-height: 100vh; }
.bar { position: sticky; top: 0; z-index: 10; display: flex; align-items: center; justify-content: space-between; height: 56px; padding: env(safe-area-inset-top) 20px 0; background: color-mix(in srgb, var(--bg) 88%, transparent); backdrop-filter: blur(14px); border-bottom: 1px solid var(--border); }
.brand { display: flex; align-items: center; gap: 9px; font-weight: 600; letter-spacing: 0.03em; }
.brand img { width: 22px; height: 22px; }
.right { display: flex; align-items: center; gap: 2px; }
.who { margin-right: 8px; font-size: 13px; color: var(--text-2); }
.icon { display: grid; place-items: center; width: 32px; height: 32px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; }
.icon:hover { background: var(--surface-hover); color: var(--text); }
.icon svg { width: 16px; height: 16px; }
.content { max-width: 620px; margin: 0 auto; padding: 48px 20px 80px; }
.hero { margin-bottom: 36px; }
.state { display: inline-flex; align-items: center; gap: 7px; font-size: 13px; color: var(--text-2); }
h1 { margin: 10px 0 0; font-size: 26px; font-weight: 600; letter-spacing: -0.03em; line-height: 1.25; }
.net-alert { display: flex; align-items: flex-start; gap: 10px; margin-top: 16px; padding: 12px 14px; border-radius: var(--radius); border: 1px solid color-mix(in srgb, var(--bad) 36%, var(--border)); background: color-mix(in srgb, var(--bad) 8%, var(--surface)); font-size: 13px; line-height: 1.55; }
.net-alert .net-ico { width: 17px; height: 17px; color: var(--bad); flex: 0 0 17px; margin-top: 2px; }
.net-alert strong { display: block; color: var(--bad); margin-bottom: 2px; }
.net-alert p { margin: 0; color: var(--text-2); }
.net-pill { display: inline-flex; align-items: center; gap: 8px; margin-top: 14px; padding: 7px 12px; border-radius: var(--radius-sm); border: 1px solid var(--border); background: var(--surface); font-size: 12.5px; color: var(--text-2); }
.net-pill .net-ico { width: 14px; height: 14px; color: var(--good); flex: 0 0 14px; }
.net-pill strong { font-weight: 500; color: var(--text); }
.usage { padding-bottom: 28px; border-bottom: 1px solid var(--border); }
.label { font-size: 12.5px; color: var(--text-3); margin-bottom: 4px; }
.big { font-size: 40px; font-weight: 600; letter-spacing: -0.035em; line-height: 1.1; margin-bottom: 14px; }
.of { font-size: 18px; font-weight: 500; color: var(--text-3); letter-spacing: -0.01em; }
.usage p { margin: 10px 0 0; font-size: 13px; }
.facts { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; margin: 0; padding: 24px 0; border-bottom: 1px solid var(--border); }
.facts dt { font-size: 12.5px; color: var(--text-3); }
.facts dd { margin: 4px 0 0; font-size: 16px; font-weight: 500; display: flex; flex-direction: column; }
.facts small { font-size: 12.5px; font-weight: 400; color: var(--text-3); }
.block { padding: 26px 0 0; }
.block-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
h2 { margin: 0; font-size: 14px; font-weight: 600; }
.hint { margin: 4px 0 12px; font-size: 13px; }
.sub-hint { font-size: 12px; }

.fixed-addr {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border: 1px solid var(--border-strong);
  border-radius: var(--radius);
  background: var(--surface);
}
.fixed-addr .mono { overflow-wrap: anywhere; font-size: 13.5px; }
.drain-note {
  margin: 10px 0 0;
  padding: 10px 12px;
  border: 1px solid color-mix(in srgb, var(--warn) 32%, var(--border));
  border-radius: var(--radius-sm);
  background: var(--warn-soft);
  color: var(--text-2);
  font-size: 12.5px;
}

.sortable-servers {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 12px 0 0;
  padding: 0;
  list-style: none;
}
.server-card {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface);
  transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), opacity var(--dur) var(--ease);
}
.server-card.movable { cursor: grab; user-select: none; }
.server-card.movable:hover { border-color: var(--border-strong); background: var(--surface-2); }
.server-card.current { border-color: color-mix(in srgb, var(--good) 40%, var(--border-strong)); background: color-mix(in srgb, var(--good) 4%, var(--surface)); }
.server-card.disabled { opacity: 0.6; }
.server-card.dragging { opacity: 0.45; cursor: grabbing; }
.grip {
  display: grid;
  place-items: center;
  width: 24px;
  height: 28px;
  color: var(--text-3);
  touch-action: none;
  flex: 0 0 24px;
}
.grip svg { width: 16px; height: 16px; }
.rank {
  display: grid;
  place-items: center;
  width: 22px;
  height: 22px;
  border-radius: 999px;
  background: var(--surface-hover);
  color: var(--text-2);
  font-size: 12px;
  font-weight: 600;
  flex: 0 0 22px;
}
.server-card.current .rank { background: var(--good-soft); color: var(--good); }
.server-info { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.server-name { font-size: 14px; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.server-state { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--good); }

.servers, .devices { margin: 8px 0 0; padding: 0; list-style: none; }
.servers li { padding: 10px 0; border-bottom: 1px solid var(--border); }
.servers li:last-child { border-bottom: 0; }
.servers .name { font-size: 13px; color: var(--text-2); }
.addr { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-top: 2px; }
.addr .mono { overflow-wrap: anywhere; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 14px; }
.devices li:last-child { border-bottom: 0; }
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.devices small { font-size: 12px; }
.tag { display: inline-block; margin-left: 6px; padding: 0 6px; border-radius: 999px; font-size: 11px; vertical-align: 1px; background: var(--surface); color: var(--text-2); }
.tag.warn { background: var(--warn-soft); color: var(--warn); }
.tag.good { background: var(--good-soft); color: var(--good); }
.kick-one { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; flex: 0 0 26px; }
.kick-one:hover:not(:disabled) { background: var(--surface-hover); color: var(--bad); }
.kick-one:disabled { opacity: 0.4; cursor: default; }
.kick-one svg { width: 14px; height: 14px; color: inherit; }
.none { font-size: 13px; padding: 6px 0; }
.pw { display: flex; flex-direction: column; gap: 14px; margin-top: 14px; }
.pw-actions { display: flex; justify-content: flex-end; gap: 8px; }
.err { margin: 0; color: var(--bad); font-size: 13px; }
@media (max-width: 560px) {
  .content { padding-top: 32px; }
  .facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  h1 { font-size: 22px; }
  .big { font-size: 34px; }
  .fixed-addr { flex-direction: column; align-items: stretch; }
}
</style>
