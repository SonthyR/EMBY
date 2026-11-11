<script setup lang="ts">
import { computed } from 'vue'
import { Copy, Pencil, Zap, ChevronRight } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiProgress from '@/components/ui/UiProgress.vue'
import type { Site } from '@/api'
import { formatBytes } from '@/utils/format'
import { INGRESS_LABEL, UA_LABEL, normalizedIngressMode } from './siteLogic'

const props = defineProps<{ open: boolean; site: Site | null; address: string; latency?: { busy?: boolean; ok?: boolean; ms?: number; error?: string } }>()
const emit = defineEmits<{ close: []; edit: []; toggle: []; remove: []; test: []; copy: [] }>()
const mode = computed(() => (props.site ? normalizedIngressMode(props.site) : 'unset'))
const quota = computed(() => Number(props.site?.traffic_quota) || 0)
const state = computed(() => {
  const s = props.site
  if (!s) return { text: '', tone: '' }
  if (s.running) return { text: '运行中', tone: 'good' }
  return s.enabled ? { text: '启动失败', tone: 'bad' } : { text: '已停用', tone: '' }
})
</script>

<template>
  <UiDrawer :open="open" :width="460" @close="emit('close')">
    <template #title>
      <div v-if="site" class="title">
        <h2>{{ site.name }}</h2>
        <span class="state"><span class="dot" :class="state.tone" />{{ state.text }}</span>
      </div>
    </template>
    <template v-if="site">
      <div class="primary">
        <UiButton variant="primary" :disabled="!address" @click="emit('copy')"><Copy />复制访问地址</UiButton>
        <UiButton :loading="latency?.busy" @click="emit('test')"><Zap />测速</UiButton>
        <UiButton icon aria-label="编辑" title="编辑" @click="emit('edit')"><Pencil /></UiButton>
      </div>

      <section class="usage">
        <div class="big num">{{ formatBytes(Number(site.traffic_used) || 0) }}</div>
        <div class="faint">{{ quota ? `额度 ${formatBytes(quota)}` : '流量不限' }}</div>
        <UiProgress v-if="quota" :value="Number(site.traffic_used) || 0" :max="quota" />
      </section>

      <dl class="props">
        <div><dt>访问地址</dt><dd class="mono">{{ address || '入口未配置' }}</dd></div>
        <div><dt>回源</dt><dd class="mono">{{ site.target_url }}</dd></div>
        <div><dt>入口</dt><dd>{{ INGRESS_LABEL[mode] }}</dd></div>
        <div><dt>客户端身份</dt><dd>{{ UA_LABEL[String(site.ua_mode)] || site.ua_mode }}</dd></div>
        <div>
          <dt>延迟</dt>
          <dd>
            <template v-if="latency?.busy">测速中…</template>
            <template v-else-if="latency?.ok"><span class="dot" :class="(latency.ms || 0) < 200 ? 'good' : (latency.ms || 0) < 800 ? 'warn' : 'bad'" /> {{ latency.ms }} ms</template>
            <template v-else-if="latency"><span class="bad-text">失败</span> <span class="faint">{{ latency.error }}</span></template>
            <template v-else><span class="faint">未测试</span></template>
          </dd>
        </div>
      </dl>

      <section class="block">
        <h3>管理</h3>
        <div class="menu">
          <button type="button" @click="emit('edit')">编辑站点设置<ChevronRight /></button>
          <button type="button" :disabled="mode === 'unset'" @click="emit('toggle')">{{ mode === 'unset' ? '请先配置入口' : site.enabled ? '停用站点' : '启用站点' }}<ChevronRight /></button>
          <button type="button" class="danger" @click="emit('remove')">删除站点<ChevronRight /></button>
        </div>
      </section>
    </template>
  </UiDrawer>
</template>

<style scoped>
.title { display: flex; align-items: center; gap: 12px; min-width: 0; }
.title h2 { margin: 0; font-size: 16px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.state { display: inline-flex; align-items: center; gap: 6px; font-size: 12.5px; color: var(--text-2); white-space: nowrap; }
.primary { display: flex; gap: 8px; margin-bottom: 24px; }
.primary > :first-child { flex: 1; }
.usage { display: flex; flex-direction: column; gap: 6px; padding-bottom: 20px; border-bottom: 1px solid var(--border); }
.big { font-size: 30px; font-weight: 600; letter-spacing: -0.03em; line-height: 1.15; }
.usage .faint { font-size: 13px; margin-bottom: 6px; }
.props { margin: 0; padding: 8px 0; border-bottom: 1px solid var(--border); }
.props div { display: flex; gap: 16px; padding: 9px 0; font-size: 13.5px; }
.props dt { flex: 0 0 84px; color: var(--text-3); }
.props dd { flex: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.bad-text { color: var(--bad); }
.block { padding-top: 20px; }
h3 { margin: 0 0 8px; font-size: 12px; font-weight: 500; color: var(--text-3); }
.menu { display: flex; flex-direction: column; margin: 0 -10px; }
.menu button { display: flex; align-items: center; justify-content: space-between; height: 38px; padding: 0 10px; border: 0; border-radius: var(--radius-sm); background: transparent; font-size: 13.5px; text-align: left; cursor: pointer; }
.menu button:hover:not(:disabled) { background: var(--surface-hover); }
.menu button:disabled { opacity: 0.4; cursor: default; }
.menu svg { width: 15px; height: 15px; color: var(--text-3); }
.menu .danger { color: var(--bad); }
</style>
