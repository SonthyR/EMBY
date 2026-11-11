<script setup lang="ts">
import { ref, watch } from 'vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import { sitesApi, type ReminderRecord } from '@/api'
import { errorMessage, toast } from '@/stores/toast'
import { dateOnly } from '@/utils/format'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ close: []; changed: [] }>()
const records = ref<ReminderRecord[]>([])
const loading = ref(false)
const busy = ref(-1)

async function load() {
  loading.value = true
  try {
    records.value = await sitesApi.reminderHistory()
  } catch (error) {
    toast.error(errorMessage(error, '提醒记录读取失败'))
  } finally {
    loading.value = false
  }
}
watch(
  () => props.open,
  open => open && load(),
)

async function act(index: number, action: 'handled' | 'ignored' | 'snoozed') {
  busy.value = index
  try {
    await sitesApi.reminderAction(records.value[index], action)
    toast.success(action === 'handled' ? '已标记处理' : action === 'snoozed' ? '明天再提醒' : '已忽略本次提醒')
    emit('changed')
    await load()
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = -1
  }
}
const ACTION: Record<string, string> = { handled: '已处理', ignored: '已忽略', snoozed: '已延后' }
</script>

<template>
  <UiDialog :open="open" title="提醒发送记录" :width="600" @close="emit('close')">
    <p class="muted">每次通过 Telegram 发送到期提醒的结果都在这里，记录里不会出现账号密码。处理或忽略后，这一批到期就不会再提醒。</p>
    <p v-if="loading && !records.length" class="muted">正在读取…</p>
    <p v-else-if="!records.length" class="muted">暂无提醒发送记录。</p>
    <ul v-else class="list">
      <li v-for="(r, i) in records" :key="i">
        <div class="top">
          <strong>{{ r.site_name }} · {{ r.account_name }}</strong>
          <UiBadge :tone="r.status === 'failed' ? 'bad' : 'good'">{{ r.status === 'failed' ? '发送失败' : '已发送' }}</UiBadge>
          <UiBadge v-if="r.action">{{ ACTION[r.action] || r.action }}</UiBadge>
        </div>
        <span class="faint">到期 {{ dateOnly(r.expires_at) }} · 提前 {{ r.reminder_days }} 天 · {{ r.attempted_at || r.sent_at }}</span>
        <span v-if="r.error" class="err">{{ r.error }}</span>
        <div v-if="r.status === 'sent' && !r.action" class="acts">
          <UiButton size="sm" :disabled="busy >= 0" @click="act(i, 'handled')">已处理</UiButton>
          <UiButton size="sm" :disabled="busy >= 0" @click="act(i, 'ignored')">忽略本次</UiButton>
          <UiButton size="sm" :disabled="busy >= 0" @click="act(i, 'snoozed')">明天再提醒</UiButton>
        </div>
      </li>
    </ul>
  </UiDialog>
</template>

<style scoped>
.muted { margin: 0 0 12px; font-size: 13px; }
.list { display: flex; flex-direction: column; gap: 8px; margin: 0; padding: 0; list-style: none; }
.list li { display: flex; flex-direction: column; gap: 4px; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius); font-size: 13px; }
.top { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.err { color: var(--bad); }
.acts { display: flex; gap: 6px; flex-wrap: wrap; margin-top: 4px; }
</style>
