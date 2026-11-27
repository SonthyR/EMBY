<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ArrowUpToLine, Copy, GripVertical, LogOut, MessageSquareText, MonitorSmartphone, Moon, ShieldBan, Sun, SunMoon, Wifi, X } from '@lucide/vue'
import logo from '@/assets/logo.svg'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
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
const redeem = reactive({ code: '', busy: false, error: '' })

const draggingId = ref<number | null>(null)
const dragOverId = ref<number | null>(null)
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
const usagePct = computed(() => {
  const u = me.value
  if (!u || !u.traffic_quota) return 0
  return Math.min(100, Math.max(0, (u.traffic_used / u.traffic_quota) * 100))
})

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
  if (!u?.expires_at) return { main: '永久有效', sub: '' }
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
  dragOverId.value = server.site_id
  dragOrderBefore.value = me.value.servers.map(item => item.site_id)
  if (event.dataTransfer) {
    event.dataTransfer.effectAllowed = 'move'
    event.dataTransfer.setData('text/plain', String(server.site_id))
  }
}

function onDragOver(event: DragEvent, target: MyAccountServer) {
  if (draggingId.value === null) return
  event.preventDefault()
  dragOverId.value = target.site_id
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
  dragOverId.value = null
  dragOrderBefore.value = []
  await commitOrder(prev, prevActive)
}

function onTouchStart(event: TouchEvent, server: MyAccountServer) {
  if (!canReorder.value || savingOrder.value || !me.value || event.touches.length !== 1) return
  touchDragActive = true
  draggingId.value = server.site_id
  dragOverId.value = server.site_id
  dragOrderBefore.value = me.value.servers.map(item => item.site_id)
}

function onTouchMove(event: TouchEvent) {
  if (!touchDragActive || draggingId.value === null || event.touches.length !== 1) return
  event.preventDefault()
  const touch = event.touches[0]
  const element = document.elementFromPoint(touch.clientX, touch.clientY)?.closest<HTMLElement>('[data-site-id]')
  const targetId = element ? Number(element.dataset.siteId) : 0
  if (targetId) {
    dragOverId.value = targetId
    if (targetId !== draggingId.value) reorderLocal(draggingId.value, targetId)
  }
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

async function submitRedeem() {
  redeem.error = ''
  const code = redeem.code.trim()
  if (!code) return (redeem.error = '请输入续期兑换码')
  redeem.busy = true
  try {
    const res = await meApi.redeem(code)
    me.value = res
    redeem.code = ''
    toast.success(res.redeem_summary || '兑换成功')
  } catch (error) {
    redeem.error = errorMessage(error, '兑换失败')
  } finally {
    redeem.busy = false
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
      <div class="bar-inner">
        <div class="brand">
          <span class="brand-mark"><img :src="logo" alt="" /></span>
          <span>云渡</span>
        </div>
        <div class="right">
          <span class="who">{{ session.username }}</span>
          <button type="button" class="icon" aria-label="切换主题" @click="cycleTheme">
            <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
          </button>
          <button type="button" class="icon" aria-label="退出登录" title="退出登录" @click="logout"><LogOut /></button>
        </div>
      </div>
    </header>

    <main class="content">
      <p v-if="loadError && !me" class="faint">{{ loadError }}</p>
      <template v-if="me">
        <section class="pass-card">
          <div class="pass-top">
            <span class="pass-state"><span class="dot" :class="STATUS_TONE[me.status]" />{{ STATUS_LABEL[me.status] }}</span>
            <span v-if="me.network && !me.network.blocked && (networkLabel || me.network.node_name)" class="pass-net">
              <Wifi />
              <span>{{ networkLabel || '已识别' }}<template v-if="me.network.node_name"> · {{ me.network.node_name }}</template></span>
            </span>
          </div>

          <h1>{{ headline }}</h1>

          <div class="pass-usage">
            <div class="usage-label">本期已用流量</div>
            <div class="big num">{{ formatBytes(me.traffic_used) }}<span class="of">{{ me.traffic_quota ? ` / ${formatBytes(me.traffic_quota)}` : ' · 流量不限' }}</span></div>
            <div v-if="me.traffic_quota" class="pass-bar"><span :style="{ width: usagePct + '%' }" /></div>
            <p class="pass-sub">{{ me.traffic_quota ? (me.next_reset_at ? `${me.next_reset_at} 自动清零` : '流量额度不会自动清零') : '畅享无限制观影' }}</p>
          </div>

          <dl class="pass-facts">
            <div><dt>有效期</dt><dd>{{ expiry.main }}<small v-if="expiry.sub">{{ expiry.sub }}</small></dd></div>
            <div><dt>在线设备</dt><dd class="num">{{ me.session_count }}<small>{{ me.max_devices ? `最多 ${me.max_devices} 台` : '不限台数' }}</small></dd></div>
            <div><dt>速率限制</dt><dd>{{ me.speed_limit ? `${me.speed_limit} Mbps` : '不限速' }}</dd></div>
          </dl>
        </section>

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

        <template v-if="hasPersonalRoute">
          <section class="bento-section">
            <h2>客户端固定地址</h2>
            <p class="faint hint">在 Emby 客户端里只需填写这一个地址，用你的账号密码登录。想换 Emby 服时直接把想看的服拖到下方第 1 位即可。</p>
            <div class="fixed-addr">
              <span class="mono">{{ fixedAddress }}</span>
              <UiButton variant="primary" size="sm" @click="copy(fixedAddress)"><Copy />复制地址</UiButton>
            </div>
            <p v-if="me.draining_sessions" class="drain-note">
              有设备正在播放视频，不会立即切断；关闭视频后将自动切换为「{{ me.active_site_name || '新服务器' }}」，届时在客户端重新登录即可。
            </p>
          </section>

          <section class="bento-section">
            <div class="block-head">
              <h2>我的 Emby 服务器</h2>
              <span v-if="canReorder" class="drag-hint">按住拖拽 · 第 1 位为首选生效</span>
            </div>
            <p v-if="!me.servers?.length" class="faint none">暂无可用服务器</p>
            <TransitionGroup v-else name="list" tag="ul" class="sortable-servers">
              <li
                v-for="(server, index) in me.servers"
                :key="server.site_id"
                :data-site-id="server.site_id"
                class="server-card"
                :class="{
                  current: server.active || index === 0,
                  disabled: !server.enabled,
                  dragging: draggingId === server.site_id,
                  over: dragOverId === server.site_id && draggingId !== null && draggingId !== server.site_id,
                  movable: canReorder,
                }"
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
                <span class="rank num" :class="{ primary: index === 0 }">{{ index === 0 ? '首选' : `#${index + 1}` }}</span>
                <div class="server-info">
                  <span class="server-name">{{ server.name }}</span>
                  <small v-if="server.active" class="server-state good"><span class="dot good" />当前生效中</small>
                  <small v-else-if="!server.enabled" class="faint">暂不可用</small>
                  <small v-else class="faint">拖到第 1 位即可切换</small>
                </div>
                <UiButton
                  v-if="index > 0 && server.enabled"
                  variant="secondary"
                  size="sm"
                  :disabled="savingOrder"
                  @click="promoteToTop(server)"
                >
                  <ArrowUpToLine />设为首选
                </UiButton>
              </li>
            </TransitionGroup>
          </section>
        </template>

        <section v-else class="bento-section">
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

        <section v-if="me.status === 'active' && me.danmaku_sources?.length" class="bento-section danmaku-section">
          <div class="block-head">
            <h2 class="dm-heading"><MessageSquareText />弹幕服务</h2>
            <span class="dm-badge">账号联动 · 国内直连</span>
          </div>
          <p class="faint hint">你的专属弹幕 API 地址，自动跟随当前账号有效期生效；账号过期或停用后自动失效，请勿外传。</p>
          <ul class="dm-sources">
            <li v-for="dm in me.danmaku_sources" :key="dm.id" class="dm-source-item">
              <div class="dm-source-head">
                <span class="dm-source-name">{{ dm.name }}</span>
                <small v-if="dm.note" class="faint">{{ dm.note }}</small>
              </div>
              <div class="fixed-addr">
                <span class="mono">{{ dm.url }}</span>
                <UiButton variant="primary" size="sm" @click="copy(dm.url)"><Copy />复制地址</UiButton>
              </div>
            </li>
          </ul>
          <details class="dm-guide">
            <summary>播放器填写指引（SenPlayer / Forward / Hills / Yamby）</summary>
            <ul>
              <li><strong>SenPlayer</strong>：设置 → 弹幕 → 弹幕服务器 / 自定义 API → 粘贴上方完整地址。</li>
              <li><strong>Forward</strong>：设置 → 弹幕设置 → 自定义弹幕源 → 粘贴上方完整地址。</li>
              <li><strong>Hills / Yamby</strong>：设置 → 弹幕 → 弹幕 API 地址 → 粘贴上方完整地址并开启。</li>
            </ul>
          </details>
        </section>

        <section class="bento-section">
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
            <li v-if="!me.sessions?.length" class="faint none">当前没有已登录设备</li>
          </ul>
        </section>

        <section class="bento-section">
          <h2>兑换码续期</h2>
          <p class="faint hint">输入分享人发放的续期码，可立即为当前账号续期或更新流量额度。</p>
          <form class="redeem-form" @submit.prevent="submitRedeem">
            <input
              v-model="redeem.code"
              class="input mono"
              maxlength="64"
              autocapitalize="characters"
              spellcheck="false"
              placeholder="例如 YD-XXXX-XXXX-XXXX"
            />
            <UiButton variant="primary" type="submit" :loading="redeem.busy">立即兑换</UiButton>
          </form>
          <p v-if="redeem.error" class="err redeem-err">{{ redeem.error }}</p>
        </section>

        <section class="bento-section">
          <div class="block-head">
            <h2>账号密码</h2>
            <UiButton v-if="!pw.open" variant="secondary" size="sm" @click="pw.open = true">修改密码</UiButton>
          </div>
          <form v-if="pw.open" class="pw" @submit.prevent="changePassword">
            <UiField label="当前密码" for="me-cur"><input id="me-cur" v-model="pw.current" class="input" type="password" autocomplete="current-password" /></UiField>
            <UiField label="新密码" for="me-new" help="修改后所有设备需要用新密码重新登录。"><input id="me-new" v-model="pw.next" class="input" type="password" autocomplete="new-password" /></UiField>
            <p v-if="pw.error" class="err">{{ pw.error }}</p>
            <div class="pw-actions">
              <UiButton @click="pw.open = false">取消</UiButton>
              <UiButton variant="primary" type="submit" :loading="pw.busy">保存密码</UiButton>
            </div>
          </form>
          <p v-else class="faint none">Emby 客户端和本自助页使用同一套账号密码。</p>
        </section>
      </template>
    </main>
  </div>
</template>

<style scoped>
.me { min-height: 100vh; }
.bar {
  position: sticky;
  top: 0;
  z-index: 20;
  background: var(--surface-glass);
  backdrop-filter: saturate(1.7) blur(20px);
  -webkit-backdrop-filter: saturate(1.7) blur(20px);
  border-bottom: 1px solid var(--border);
}
.bar-inner {
  max-width: 680px;
  margin: 0 auto;
  height: 58px;
  padding: env(safe-area-inset-top) 20px 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.brand { display: flex; align-items: center; gap: 10px; font-weight: 700; font-size: 16px; letter-spacing: -0.02em; }
.brand-mark { display: grid; place-items: center; width: 28px; height: 28px; border-radius: 9px; background: var(--hero-gradient); }
.brand-mark img { width: 16px; height: 16px; filter: brightness(0) invert(1); }
.right { display: flex; align-items: center; gap: 6px; }
.who { margin-right: 4px; font-size: 13px; font-weight: 500; color: var(--text-2); }
.icon { display: grid; place-items: center; width: 32px; height: 32px; border: 1px solid var(--border); border-radius: 50%; background: var(--surface-2); color: var(--text-2); cursor: pointer; }
.icon:hover { background: var(--surface); color: var(--text); }
.icon svg { width: 15px; height: 15px; }

.content { max-width: 680px; margin: 0 auto; padding: 28px 20px 76px; display: flex; flex-direction: column; gap: 16px; }

.pass-card {
  padding: 24px 26px;
  border-radius: 24px;
  background: var(--hero-gradient);
  color: #ffffff;
  box-shadow: 0 18px 44px -16px rgba(249, 87, 37, 0.35);
}
.pass-top { display: flex; align-items: center; justify-content: space-between; gap: 10px; flex-wrap: wrap; }
.pass-state {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 4px 11px;
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.14);
  font-size: 12px;
  font-weight: 600;
}
.pass-net {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  background: rgba(255, 255, 255, 0.12);
  font-size: 12px;
  color: rgba(255, 255, 255, 0.9);
}
.pass-net svg { width: 13px; height: 13px; color: #34d399; }
.pass-card h1 { margin: 14px 0 0; font-size: 22px; font-weight: 700; letter-spacing: -0.025em; }

.pass-usage { margin-top: 20px; padding-top: 18px; border-top: 1px solid rgba(255, 255, 255, 0.14); }
.usage-label { font-size: 12px; color: rgba(255, 255, 255, 0.72); }
.big { margin-top: 4px; font-size: 34px; font-weight: 700; letter-spacing: -0.03em; line-height: 1.15; }
.of { font-size: 15px; font-weight: 500; color: rgba(255, 255, 255, 0.72); }
.pass-bar { height: 6px; margin-top: 12px; border-radius: 999px; background: rgba(255, 255, 255, 0.18); overflow: hidden; }
.pass-bar span { display: block; height: 100%; border-radius: inherit; background: #ffffff; transition: width 400ms var(--ease); }
.pass-sub { margin: 8px 0 0; font-size: 12px; color: rgba(255, 255, 255, 0.72); }

.pass-facts {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  margin: 18px 0 0;
}
.pass-facts > div {
  padding: 10px 12px;
  border-radius: 14px;
  background: rgba(255, 255, 255, 0.10);
  backdrop-filter: blur(8px);
}
.pass-facts dt { font-size: 11px; color: rgba(255, 255, 255, 0.72); }
.pass-facts dd { margin: 3px 0 0; font-size: 14.5px; font-weight: 600; display: flex; flex-direction: column; }
.pass-facts small { font-size: 11px; font-weight: 400; color: rgba(255, 255, 255, 0.72); }

.net-alert { display: flex; align-items: flex-start; gap: 10px; padding: 14px 16px; border-radius: var(--radius-lg); border: 1px solid color-mix(in srgb, var(--bad) 36%, var(--border)); background: var(--bad-soft); font-size: 13px; line-height: 1.55; }
.net-alert .net-ico { width: 17px; height: 17px; color: var(--bad); flex: 0 0 17px; margin-top: 2px; }
.net-alert strong { display: block; color: var(--bad); margin-bottom: 2px; }
.net-alert p { margin: 0; color: var(--text-2); }

.bento-section {
  padding: 20px 22px;
  border-radius: var(--radius-lg);
  background: var(--surface);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-card);
}
.block-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
h2 { margin: 0; font-size: 15.5px; font-weight: 700; letter-spacing: -0.015em; }
.hint { margin: 6px 0 12px; font-size: 13px; }
.drag-hint {
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 11.5px;
  font-weight: 600;
}

.fixed-addr {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
}
.fixed-addr .mono { overflow-wrap: anywhere; font-size: 13px; }
.dm-heading { display: inline-flex; align-items: center; gap: 7px; }
.dm-heading svg { width: 17px; height: 17px; color: var(--accent); }
.dm-badge {
  padding: 3px 10px;
  border-radius: 999px;
  background: var(--good-soft);
  color: var(--good);
  font-size: 11.5px;
  font-weight: 600;
}
.dm-sources { margin: 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 10px; }
.dm-source-item { display: flex; flex-direction: column; gap: 6px; }
.dm-source-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; flex-wrap: wrap; }
.dm-source-name { font-size: 13.5px; font-weight: 600; color: var(--text); }
.dm-guide {
  margin-top: 12px;
  padding: 10px 13px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  border: 1px solid var(--border);
  font-size: 12.5px;
  color: var(--text-2);
}
.dm-guide summary { cursor: pointer; font-weight: 600; color: var(--text); list-style: none; }
.dm-guide summary::-webkit-details-marker { display: none; }
.dm-guide summary::before { content: '▸ '; color: var(--accent); }
.dm-guide[open] summary::before { content: '▾ '; }
.dm-guide ul { margin: 8px 0 0; padding-left: 18px; display: flex; flex-direction: column; gap: 4px; line-height: 1.55; }
.drain-note {
  margin: 10px 0 0;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--warn-soft);
  color: var(--warn);
  font-size: 12.5px;
}

.sortable-servers {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 9px;
  margin: 14px 0 0;
  padding: 0;
  list-style: none;
}
.server-card {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 13px 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  transition: transform 160ms var(--ease), border-color 160ms var(--ease), background 160ms var(--ease), box-shadow 160ms var(--ease), opacity 160ms var(--ease);
}
.server-card.movable { cursor: grab; user-select: none; }
.server-card.movable:active { cursor: grabbing; }
.server-card.current {
  border-color: var(--accent-border);
  background: linear-gradient(90deg, var(--accent-soft) 0%, var(--surface) 75%);
}
.server-card.disabled { opacity: 0.6; }
.server-card.dragging {
  opacity: 0.52;
  transform: scale(0.985);
  border-style: dashed;
  border-color: var(--accent);
}
.server-card.over {
  border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-soft);
}
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
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 36px;
  height: 23px;
  padding: 0 7px;
  border-radius: 999px;
  background: var(--surface);
  border: 1px solid var(--border);
  color: var(--text-2);
  font-size: 11px;
  font-weight: 700;
  flex: 0 0 auto;
}
.rank.primary { background: var(--accent); border-color: var(--accent); color: #ffffff; }
.server-info { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.server-name { font-size: 14px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.server-state { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--good); font-weight: 500; }

.servers, .devices { margin: 12px 0 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 8px; }
.servers li { padding: 12px 14px; border-radius: var(--radius-sm); background: var(--surface-2); }
.servers .name { font-size: 13px; font-weight: 600; color: var(--text); }
.addr { display: flex; align-items: center; justify-content: space-between; gap: 8px; margin-top: 4px; }
.addr .mono { overflow-wrap: anywhere; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 11px 13px; border-radius: var(--radius-sm); background: var(--surface-2); font-size: 13.5px; }
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.devices small { font-size: 12px; }
.tag { display: inline-block; margin-left: 6px; padding: 0 7px; border-radius: 999px; font-size: 10.5px; font-weight: 600; vertical-align: 1px; background: var(--surface); color: var(--text-2); }
.tag.warn { background: var(--warn-soft); color: var(--warn); }
.tag.good { background: var(--good-soft); color: var(--good); }
.kick-one { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 50%; background: transparent; color: var(--text-3); cursor: pointer; flex: 0 0 26px; }
.kick-one:hover:not(:disabled) { background: var(--bad-soft); color: var(--bad); }
.kick-one:disabled { opacity: 0.4; cursor: default; }
.kick-one svg { width: 14px; height: 14px; color: inherit; }
.none { font-size: 13px; margin: 10px 0 0; padding: 4px 0; }
.pw { display: flex; flex-direction: column; gap: 14px; margin-top: 14px; }
.pw-actions { display: flex; justify-content: flex-end; gap: 8px; }
.redeem-form { display: flex; gap: 8px; }
.redeem-form .input { flex: 1; }
.redeem-err { margin-top: 8px; }
.err { margin: 0; color: var(--bad); font-size: 13px; }

@media (max-width: 560px) {
  .content { padding: 18px 14px 64px; }
  .pass-card { padding: 20px; border-radius: 20px; }
  .pass-facts { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .big { font-size: 28px; }
  .fixed-addr { flex-direction: column; align-items: stretch; }
}
</style>
