<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Copy, Pencil, Eye, EyeOff, MonitorSmartphone, ChevronRight } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import { shareApi, shareUserToInput, type ShareUser } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { copyText, daysUntil, dateOnly, formatBytes, relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from './shareStatus'

const props = defineProps<{ open: boolean; user: ShareUser | null }>()
const emit = defineEmits<{ close: []; edit: []; changed: []; deleted: []; copyShare: [] }>()
const detail = ref<ShareUser | null>(null)
const busy = ref('')
const reveal = ref(false)

watch(
  () => [props.open, props.user] as const,
  async ([open, user]) => {
    if (!open || !user) return
    if (detail.value?.id !== user.id) reveal.value = false
    detail.value = { ...user, sessions: detail.value?.id === user.id ? detail.value.sessions : undefined }
    try {
      detail.value = await shareApi.get(user.id)
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

async function run(key: string, action: () => Promise<unknown>, success: string) {
  busy.value = key
  try {
    await action()
    toast.success(success)
    emit('changed')
    if (detail.value) detail.value = await shareApi.get(detail.value.id)
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
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
        <UiButton icon aria-label="编辑" title="编辑" @click="emit('edit')"><Pencil /></UiButton>
      </div>

      <section class="usage">
        <div class="big num">{{ formatBytes(detail.traffic_used) }}</div>
        <div class="faint">{{ detail.traffic_quota ? `共 ${formatBytes(detail.traffic_quota)}` : '流量不限' }}<template v-if="detail.next_reset_at"> · {{ detail.next_reset_at }} 重置</template></div>
        <UiProgress v-if="detail.traffic_quota" :value="detail.traffic_used" :max="detail.traffic_quota" />
      </section>

      <dl class="props">
        <div><dt>到期</dt><dd>{{ expiry.text }}<span v-if="expiry.sub" class="faint"> · {{ expiry.sub }}</span></dd></div>
        <div><dt>在线设备</dt><dd class="num">{{ detail.session_count }}{{ detail.max_devices ? ` / ${detail.max_devices} 台` : ' 台' }}</dd></div>
        <div><dt>限速</dt><dd>{{ detail.speed_limit ? `${detail.speed_limit} Mbps` : '不限' }}</dd></div>
        <div><dt>站点</dt><dd>{{ detail.sites.map(s => s.site_name).join('、') || '—' }}</dd></div>
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

      <section class="block">
        <h3>已登录设备</h3>
        <ul v-if="detail.sessions?.length" class="devices">
          <li v-for="(s, i) in detail.sessions" :key="i">
            <MonitorSmartphone />
            <div>
              <span>{{ s.device_name || '未知设备' }}</span>
              <small class="faint">{{ [s.client, s.site_name, s.client_ip].filter(Boolean).join(' · ') }}</small>
            </div>
            <small class="faint">{{ relativeTime(s.last_seen_at) }}</small>
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
h3 { margin: 0 0 8px; font-size: 12px; font-weight: 500; color: var(--text-3); }
.devices { margin: 0; padding: 0; list-style: none; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 9px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.devices li:last-child { border-bottom: 0; }
.devices svg { width: 16px; height: 16px; color: var(--text-3); flex: 0 0 16px; }
.devices div { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.devices small { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.none { margin: 0; font-size: 13px; }
.menu { display: flex; flex-direction: column; margin: 0 -10px; }
.menu button { display: flex; align-items: center; justify-content: space-between; height: 38px; padding: 0 10px; border: 0; border-radius: var(--radius-sm); background: transparent; font-size: 13.5px; text-align: left; cursor: pointer; }
.menu button:hover:not(:disabled) { background: var(--surface-hover); }
.menu button:disabled { opacity: 0.4; cursor: default; }
.menu svg { width: 15px; height: 15px; color: var(--text-3); }
.menu .danger { color: var(--bad); }
</style>
