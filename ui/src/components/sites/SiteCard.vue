<script setup lang="ts">
import { computed, ref } from 'vue'
import { Copy, Eye, EyeOff, Pencil, Power, Trash2, Zap, ArrowUp, ArrowDown } from '@lucide/vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import type { Site } from '@/api'
import { formatBytes } from '@/utils/format'
import { INGRESS_LABEL, UA_LABEL, normalizedIngressMode } from './siteLogic'

const props = defineProps<{ site: Site; address: string; latency?: { busy?: boolean; ok?: boolean; ms?: number; error?: string }; sorting?: boolean; first?: boolean; last?: boolean }>()
defineEmits<{ edit: []; toggle: []; remove: []; test: []; copy: []; up: []; down: [] }>()

const reveal = ref(false)
const mode = computed(() => normalizedIngressMode(props.site))
const quota = computed(() => Number(props.site.traffic_quota) || 0)
const latencyTone = computed(() => {
  const l = props.latency
  if (!l || l.busy || l.ok === undefined) return 'faint'
  if (!l.ok) return 'bad'
  return (l.ms || 0) < 200 ? 'good' : (l.ms || 0) < 800 ? 'warn' : 'bad'
})
</script>

<template>
  <article class="card" :class="{ stopped: !site.running }">
    <header class="head">
      <div class="title">
        <strong :title="site.name">{{ site.name }}</strong>
        <div class="tags">
          <UiBadge>{{ INGRESS_LABEL[mode] }}</UiBadge>
          <UiBadge tone="accent">{{ UA_LABEL[String(site.ua_mode)] || site.ua_mode }}</UiBadge>
        </div>
      </div>
      <UiBadge :tone="site.running ? 'good' : 'neutral'" dot>{{ site.running ? '运行中' : site.enabled ? '启动失败' : '已停用' }}</UiBadge>
    </header>

    <div class="address">
      <span class="label">访问地址</span>
      <span class="mono value">{{ address ? (reveal ? address : '••••••••••••') : '入口未配置' }}</span>
      <button v-if="address" type="button" class="icon" :aria-label="reveal ? '隐藏' : '显示'" @click="reveal = !reveal"><EyeOff v-if="reveal" /><Eye v-else /></button>
      <button v-if="address" type="button" class="icon" aria-label="复制访问地址" @click="$emit('copy')"><Copy /></button>
    </div>

    <dl class="rows">
      <div><dt>回源</dt><dd class="mono" :title="site.target_url">{{ site.target_url }}</dd></div>
      <div>
        <dt>延迟</dt>
        <dd :class="latencyTone">
          <template v-if="latency?.busy">测速中…</template>
          <template v-else-if="latency?.ok">{{ latency.ms }} ms</template>
          <template v-else-if="latency">失败<span class="faint" :title="latency.error">（{{ latency.error }}）</span></template>
          <template v-else>未测试</template>
        </dd>
      </div>
      <div>
        <dt>流量</dt>
        <dd>{{ formatBytes(Number(site.traffic_used) || 0) }}<span v-if="quota" class="faint"> / {{ formatBytes(quota) }}</span></dd>
      </div>
    </dl>
    <UiProgress v-if="quota" :value="Number(site.traffic_used) || 0" :max="quota" />

    <footer class="actions">
      <template v-if="sorting">
        <UiButton size="sm" :disabled="first" aria-label="上移" @click="$emit('up')"><ArrowUp /></UiButton>
        <UiButton size="sm" :disabled="last" aria-label="下移" @click="$emit('down')"><ArrowDown /></UiButton>
      </template>
      <template v-else>
        <UiButton size="sm" :loading="latency?.busy" @click="$emit('test')"><Zap />测速</UiButton>
        <UiButton size="sm" :disabled="mode === 'unset'" :title="mode === 'unset' ? '请先编辑并配置入口' : ''" @click="$emit('toggle')">
          <Power />{{ mode === 'unset' ? '待配置' : site.enabled ? '停用' : '启用' }}
        </UiButton>
        <span class="spacer" />
        <UiButton size="sm" aria-label="编辑" title="编辑" @click="$emit('edit')"><Pencil /></UiButton>
        <UiButton size="sm" variant="ghost" aria-label="删除" title="删除" class="del" @click="$emit('remove')"><Trash2 /></UiButton>
      </template>
    </footer>
  </article>
</template>

<style scoped>
.card { display: flex; flex-direction: column; gap: 12px; padding: 16px 18px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--surface); transition: border-color var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.card:hover { border-color: var(--border-strong); }
.card.stopped { background: color-mix(in srgb, var(--surface) 70%, var(--bg)); }
.head { display: flex; align-items: flex-start; gap: 8px; }
.grip { display: grid; place-items: center; width: 20px; height: 36px; margin-left: -6px; color: var(--text-3); cursor: grab; touch-action: none; }
.grip svg { width: 16px; height: 16px; }
.title { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 6px; }
.title strong { font-size: 15px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tags { display: flex; gap: 6px; flex-wrap: wrap; }
.address { display: flex; align-items: center; gap: 6px; padding: 8px 10px; border-radius: var(--radius-sm); background: var(--surface-2); font-size: 13px; }
.address .label { color: var(--text-3); flex: 0 0 auto; }
.address .value { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.icon { display: grid; place-items: center; width: 26px; height: 26px; border: 0; border-radius: 6px; background: transparent; color: var(--text-3); cursor: pointer; }
.icon:hover { background: var(--surface-hover); color: var(--text); }
.icon svg { width: 15px; height: 15px; }
.rows { display: flex; flex-direction: column; gap: 6px; margin: 0; font-size: 13px; }
.rows div { display: flex; justify-content: space-between; gap: 12px; }
.rows dt { color: var(--text-3); flex: 0 0 auto; }
.rows dd { margin: 0; min-width: 0; text-align: right; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.good { color: var(--good); }
.warn { color: var(--warn); }
.bad { color: var(--bad); }
.actions { display: flex; align-items: center; gap: 6px; margin-top: auto; }
.spacer { flex: 1; }
.del:hover { color: var(--bad); }
</style>
