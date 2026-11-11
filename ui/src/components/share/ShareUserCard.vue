<script setup lang="ts">
import { computed, ref } from 'vue'
import { Copy, Link, MoreHorizontal, Pencil, Eye, EyeOff, MonitorSmartphone, Gauge, CalendarClock } from '@lucide/vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import type { ShareUser } from '@/api'
import { daysUntil, dateOnly, formatBytes } from '@/utils/format'
import { STATUS_LABEL, STATUS_TONE } from './shareStatus'

const props = defineProps<{ user: ShareUser }>()
defineEmits<{ edit: []; manage: []; copyShare: []; copyPortal: [] }>()

const reveal = ref(false)
const expiry = computed(() => {
  if (!props.user.expires_at) return { text: '永久有效', tone: 'faint' }
  const days = daysUntil(props.user.expires_at)
  const date = dateOnly(props.user.expires_at)
  if (days === null) return { text: date, tone: 'faint' }
  if (days < 0) return { text: `${date} 已过期`, tone: 'bad' }
  if (days === 0) return { text: `${date} 今天到期`, tone: 'warn' }
  return { text: `${date} · 剩 ${days} 天`, tone: days <= 7 ? 'warn' : 'faint' }
})
const quota = computed(() => props.user.traffic_quota > 0)
</script>

<template>
  <article class="card" :class="{ dim: user.status !== 'active' }">
    <header class="head">
      <div class="title">
        <span class="avatar">{{ user.username.slice(0, 1).toUpperCase() }}</span>
        <div class="names">
          <strong :title="user.username">{{ user.username }}</strong>
          <span v-if="user.note" class="note" :title="user.note">{{ user.note }}</span>
        </div>
      </div>
      <UiBadge :tone="STATUS_TONE[user.status]" dot>{{ STATUS_LABEL[user.status] }}</UiBadge>
    </header>

    <div class="usage">
      <div class="usage-line">
        <span class="used">{{ formatBytes(user.traffic_used) }}</span>
        <span class="faint">/ {{ quota ? formatBytes(user.traffic_quota) : '不限流量' }}</span>
      </div>
      <UiProgress v-if="quota" :value="user.traffic_used" :max="user.traffic_quota" />
    </div>

    <ul class="facts">
      <li :class="expiry.tone"><CalendarClock />{{ expiry.text }}</li>
      <li><MonitorSmartphone />在线 {{ user.session_count }}{{ user.max_devices ? ` / ${user.max_devices}` : '' }} 台</li>
      <li v-if="user.speed_limit"><Gauge />限速 {{ user.speed_limit }} Mbps</li>
      <li v-if="user.next_reset_at" class="faint">{{ user.next_reset_at }} 重置流量</li>
    </ul>

    <div class="sites">
      <UiBadge v-for="site in user.sites" :key="site.site_id" tone="accent">{{ site.site_name || `站点 ${site.site_id}` }}</UiBadge>
    </div>

    <div class="secret">
      <span class="faint">密码</span>
      <span class="mono">{{ reveal ? user.password || '（无法读取）' : '••••••••' }}</span>
      <button type="button" class="eye" :aria-label="reveal ? '隐藏密码' : '显示密码'" @click="reveal = !reveal">
        <EyeOff v-if="reveal" /><Eye v-else />
      </button>
    </div>

    <footer class="actions">
      <UiButton variant="primary" size="sm" class="grow" @click="$emit('copyShare')"><Copy />复制分享信息</UiButton>
      <UiButton size="sm" aria-label="复制自助查询链接" title="复制自助查询链接" @click="$emit('copyPortal')"><Link /></UiButton>
      <UiButton size="sm" aria-label="编辑" title="编辑" @click="$emit('edit')"><Pencil /></UiButton>
      <UiButton size="sm" aria-label="管理" title="管理：续期、停用、设备…" @click="$emit('manage')"><MoreHorizontal /></UiButton>
    </footer>
  </article>
</template>

<style scoped>
.card {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 18px;
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
  transition: border-color var(--dur) var(--ease), transform var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
}
.card:hover { border-color: var(--border-strong); box-shadow: 0 6px 20px rgba(0, 0, 0, 0.12); }
.card.dim .usage, .card.dim .facts, .card.dim .sites { opacity: 0.7; }
.head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
.title { display: flex; align-items: center; gap: 10px; min-width: 0; }
.avatar {
  display: grid;
  place-items: center;
  flex: 0 0 36px;
  width: 36px;
  height: 36px;
  border-radius: 11px;
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}
.names { display: flex; flex-direction: column; min-width: 0; }
.names strong { font-size: 15px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.note { font-size: 12px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.usage { display: flex; flex-direction: column; gap: 8px; }
.usage-line { display: flex; align-items: baseline; gap: 6px; }
.used { font-size: 20px; font-weight: 600; letter-spacing: -0.01em; }
.facts { display: flex; flex-wrap: wrap; gap: 6px 14px; margin: 0; padding: 0; list-style: none; font-size: 13px; color: var(--text-2); }
.facts li { display: inline-flex; align-items: center; gap: 5px; }
.facts svg { width: 15px; height: 15px; color: var(--text-3); }
.facts .warn { color: var(--warn); }
.facts .bad { color: var(--bad); }
.sites { display: flex; flex-wrap: wrap; gap: 6px; }
.secret { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border-radius: var(--radius-sm); background: var(--surface-2); font-size: 13px; }
.secret .mono { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
.eye { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 6px; background: transparent; color: var(--text-3); cursor: pointer; }
.eye:hover { background: var(--surface-hover); color: var(--text); }
.eye svg { width: 15px; height: 15px; }
.actions { display: flex; align-items: center; gap: 6px; margin-top: auto; }
.actions .grow { flex: 1; }
.actions :deep(.btn-sm:not(.grow)) { width: 34px; padding: 0; }
</style>
