<script setup lang="ts">
import { ref, watch } from 'vue'
import { Ban, CalendarPlus, CircleCheck, LogOut, RotateCcw, Link2Off, Trash2, MonitorSmartphone } from '@lucide/vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import { shareApi, shareUserToInput, type ShareUser } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { relativeTime } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from './shareStatus'

const props = defineProps<{ open: boolean; user: ShareUser | null }>()
const emit = defineEmits<{ close: []; changed: []; deleted: [] }>()
const detail = ref<ShareUser | null>(null)
const busy = ref('')

watch(
  () => [props.open, props.user?.id] as const,
  async ([open]) => {
    if (!open || !props.user) return
    detail.value = props.user
    try {
      detail.value = await shareApi.get(props.user.id)
    } catch {
      /* keep list data */
    }
  },
  { immediate: true },
)

async function run(key: string, action: () => Promise<unknown>, success: string) {
  busy.value = key
  try {
    await action()
    toast.success(success)
    emit('changed')
    if (props.user) detail.value = await shareApi.get(props.user.id)
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
  }
}

async function toggle() {
  const user = detail.value!
  await run('toggle', () => shareApi.update(user.id, shareUserToInput(user, { enabled: !user.enabled })), user.enabled ? '已停用，在线设备已断开' : '已启用')
}
async function renew(days: number) {
  await run(`renew${days}`, () => shareApi.renew(detail.value!.id, days), `已续期 ${days === 365 ? '1 年' : days + ' 天'}`)
}
async function resetTraffic() {
  if (!(await confirm({ title: '清零已用流量？', message: '本周期的已用流量会归零，额度重新开始计算。', confirmText: '清零' }))) return
  await run('reset', () => shareApi.resetTraffic(detail.value!.id), '已用流量已清零')
}
async function kick() {
  const result = { n: 0 }
  await run(
    'kick',
    async () => {
      result.n = (await shareApi.kick(detail.value!.id)).revoked
    },
    '已断开所有设备',
  )
}
async function resetPortal() {
  if (!(await confirm({ title: '重置查询链接？', message: '旧链接会立即失效，需要把新链接重新发给朋友。', confirmText: '重置' }))) return
  await run('portal', () => shareApi.resetPortal(detail.value!.id), '查询链接已重置')
}
async function remove() {
  const user = detail.value!
  if (!(await confirm({ title: `删除 ${user.username}？`, message: '在线设备会立刻断开，这个操作无法撤销。', confirmText: '删除', danger: true }))) return
  busy.value = 'delete'
  try {
    await shareApi.remove(user.id)
    toast.success('分享账号已删除')
    emit('deleted')
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <UiDialog :open="open" :width="560" @close="emit('close')">
    <template #header>
      <div v-if="detail" class="title">
        <h2>{{ detail.username }}</h2>
        <UiBadge :tone="STATUS_TONE[detail.status]" dot>{{ STATUS_LABEL[detail.status] }}</UiBadge>
      </div>
    </template>
    <div v-if="detail" class="body">
      <div class="actions">
        <button type="button" :disabled="!!busy" @click="toggle">
          <CircleCheck v-if="!detail.enabled" /><Ban v-else />
          <span>{{ detail.enabled ? '停用账号' : '启用账号' }}</span>
        </button>
        <button type="button" :disabled="!!busy" @click="renew(30)"><CalendarPlus /><span>续期 30 天</span></button>
        <button type="button" :disabled="!!busy" @click="renew(365)"><CalendarPlus /><span>续期 1 年</span></button>
        <button type="button" :disabled="!!busy" @click="resetTraffic"><RotateCcw /><span>清零已用流量</span></button>
        <button type="button" :disabled="!!busy" @click="kick"><LogOut /><span>断开所有设备</span></button>
        <button type="button" :disabled="!!busy" @click="resetPortal"><Link2Off /><span>重置查询链接</span></button>
      </div>

      <h3>已登录设备 · {{ detail.sessions?.length || 0 }}</h3>
      <ul v-if="detail.sessions?.length" class="devices">
        <li v-for="(s, index) in detail.sessions" :key="index">
          <MonitorSmartphone />
          <div>
            <strong>{{ s.device_name || '未知设备' }}</strong>
            <span class="faint">{{ [s.client, s.site_name, s.client_ip].filter(Boolean).join(' · ') }}</span>
          </div>
          <span class="faint when">{{ relativeTime(s.last_seen_at) }}</span>
        </li>
      </ul>
      <p v-else class="faint">暂时没有设备登录。</p>

      <button type="button" class="danger" :disabled="!!busy" @click="remove"><Trash2 /><span>删除这个分享账号</span></button>
    </div>
  </UiDialog>
</template>

<style scoped>
.title { display: flex; align-items: center; gap: 10px; min-width: 0; }
.title h2 { margin: 0; font-size: 17px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.body { display: flex; flex-direction: column; gap: 16px; }
.actions { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; }
.actions button, .danger {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 6px;
  padding: 14px 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--surface-2);
  font-size: 13px;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.actions button:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); background: var(--accent-soft); }
.actions svg, .danger svg { width: 19px; height: 19px; }
button:disabled { opacity: 0.55; cursor: progress; }
h3 { margin: 4px 0 0; font-size: 13px; color: var(--text-3); font-weight: 600; }
.devices { display: flex; flex-direction: column; gap: 6px; margin: 0; padding: 0; list-style: none; }
.devices li { display: flex; align-items: center; gap: 12px; padding: 10px 12px; border-radius: var(--radius-sm); background: var(--surface-2); }
.devices svg { width: 18px; height: 18px; color: var(--text-3); flex: 0 0 18px; }
.devices div { display: flex; flex-direction: column; min-width: 0; flex: 1; }
.devices strong { font-weight: 500; font-size: 14px; }
.devices .faint { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.when { flex: 0 0 auto; font-size: 12px; }
.danger { flex-direction: row; justify-content: center; color: var(--bad); background: transparent; border-style: dashed; }
.danger:hover:not(:disabled) { background: var(--bad-soft); border-color: var(--bad); }
@media (max-width: 520px) { .actions { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
