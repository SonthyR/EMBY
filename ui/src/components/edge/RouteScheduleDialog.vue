<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Clock3 } from '@lucide/vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiButton from '@/components/ui/UiButton.vue'
import {
  ispRoutesApi,
  type ISPRouteGroup,
  type ISPRouteNode,
  type ISPRouteScheduleMode,
  type ISPRoutesResponse,
} from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const props = defineProps<{
  open: boolean
  groupKey: string
  groupLabel: string
  node: ISPRouteNode | null
  allGroups: ISPRouteGroup[]
}>()

const emit = defineEmits<{
  close: []
  saved: [res: ISPRoutesResponse]
}>()

const PRESETS = [
  { label: '19:30–00:30', start: '19:30', end: '00:30' },
  { label: '20:00–00:00', start: '20:00', end: '00:00' },
  { label: '20:00–01:00', start: '20:00', end: '01:00' },
]

const mode = ref<ISPRouteScheduleMode>('always')
const startTime = ref('19:30')
const endTime = ref('00:30')
const selectedGroups = ref<string[]>([])
const saving = ref(false)
const formError = ref('')

watch(
  () => [props.open, props.groupKey, props.node?.node_id] as const,
  ([isOpen]) => {
    if (!isOpen || !props.node) return
    formError.value = ''
    const rawMode = props.node.schedule_mode
    mode.value = rawMode === 'avoid' || rawMode === 'only' ? rawMode : 'always'
    startTime.value = props.node.schedule_start || '19:30'
    endTime.value = props.node.schedule_end || '00:30'
    selectedGroups.value = props.groupKey ? [props.groupKey] : []
  },
  { immediate: true },
)

const title = computed(() => {
  if (!props.node) return '时段策略'
  return `${props.node.name} · ${props.groupLabel}时段策略`
})

const isCrossMidnight = computed(() => {
  if (!startTime.value || !endTime.value) return false
  return startTime.value > endTime.value
})

const summaryText = computed(() => {
  if (mode.value === 'always') {
    return '24 小时正常参与分流，不按时段休眠。'
  }
  const endLabel = isCrossMidnight.value ? `次日 ${endTime.value}` : endTime.value
  if (mode.value === 'avoid') {
    return `每天北京时间 ${startTime.value} 至 ${endLabel} 自动休眠跳过（由下一顺位节点接管），其余时间正常参与分流。`
  }
  return `仅每天北京时间 ${startTime.value} 至 ${endLabel} 参与分流，其余时间自动休眠跳过。`
})

function toggleGroup(key: string) {
  if (selectedGroups.value.includes(key)) {
    selectedGroups.value = selectedGroups.value.filter(g => g !== key)
  } else {
    selectedGroups.value = [...selectedGroups.value, key]
  }
}

function applyPreset(start: string, end: string) {
  startTime.value = start
  endTime.value = end
  formError.value = ''
}

async function save() {
  if (!props.node || saving.value) return
  formError.value = ''
  if (!selectedGroups.value.length) {
    formError.value = '请至少选择一个线路分组'
    return
  }
  if (mode.value !== 'always') {
    if (!startTime.value || !endTime.value) {
      formError.value = '请填写开始与结束时间'
      return
    }
    if (startTime.value === endTime.value) {
      formError.value = '开始时间与结束时间不能相同'
      return
    }
  }
  saving.value = true
  try {
    const res = await ispRoutesApi.setSchedule({
      node_id: props.node.node_id,
      groups: selectedGroups.value,
      schedule_mode: mode.value,
      schedule_start: mode.value === 'always' ? '' : startTime.value,
      schedule_end: mode.value === 'always' ? '' : endTime.value,
    })
    const groupNames = props.allGroups
      .filter(g => selectedGroups.value.includes(g.group))
      .map(g => g.label)
      .join('、')
    const modeDesc =
      mode.value === 'always'
        ? '全天生效'
        : mode.value === 'avoid'
          ? `避峰 ${startTime.value}–${endTime.value}`
          : `仅 ${startTime.value}–${endTime.value} 生效`
    toast.success(`「${props.node.name}」在${groupNames || props.groupLabel}已设为${modeDesc}`)
    emit('saved', res)
    emit('close')
  } catch (err) {
    formError.value = errorMessage(err, '保存时段策略失败')
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UiDialog :open="open" :title="title" :width="490" :dismissible="!saving" @close="emit('close')">
    <div class="sched-form">
      <div class="section-block">
        <div class="block-label">生效模式</div>
        <div class="mode-list" role="radiogroup" aria-label="生效模式">
          <label class="mode-card" :class="{ active: mode === 'always' }">
            <input v-model="mode" type="radio" value="always" />
            <div class="mode-copy">
              <strong>全天生效（默认）</strong>
              <span class="faint">24 小时按排序或负载均衡正常分流</span>
            </div>
          </label>
          <label class="mode-card" :class="{ active: mode === 'avoid' }">
            <input v-model="mode" type="radio" value="avoid" />
            <div class="mode-copy">
              <strong>避开指定时段（休眠避峰）</strong>
              <span class="faint">适合白天好用、晚高峰拥塞的 NAT / 普通节点</span>
            </div>
          </label>
          <label class="mode-card" :class="{ active: mode === 'only' }">
            <input v-model="mode" type="radio" value="only" />
            <div class="mode-copy">
              <strong>仅在指定时段生效（高峰专用）</strong>
              <span class="faint">适合高价优化线路排第 1 位，仅在晚高峰顶班省流量</span>
            </div>
          </label>
        </div>
      </div>

      <div v-if="mode !== 'always'" class="section-block">
        <div class="block-label">
          <span>时段设置（北京时间 UTC+8，支持跨午夜）</span>
        </div>
        <div class="time-row">
          <input v-model="startTime" type="time" class="input time-input mono" aria-label="开始时间" />
          <span class="time-sep faint">至</span>
          <input v-model="endTime" type="time" class="input time-input mono" aria-label="结束时间" />
        </div>
        <div class="presets">
          <span class="faint preset-label">快捷晚高峰：</span>
          <button
            v-for="p in PRESETS"
            :key="p.label"
            type="button"
            class="preset-chip mono"
            :class="{ on: startTime === p.start && endTime === p.end }"
            @click="applyPreset(p.start, p.end)"
          >
            {{ p.label }}
          </button>
        </div>
      </div>

      <div class="summary-box">
        <Clock3 class="summary-icon" />
        <span>{{ summaryText }}</span>
      </div>

      <div class="section-block">
        <div class="block-label">同步应用到该节点的线路分组</div>
        <div class="group-checks">
          <label v-for="g in allGroups" :key="g.group" class="group-check" :class="{ checked: selectedGroups.includes(g.group) }">
            <input
              type="checkbox"
              :checked="selectedGroups.includes(g.group)"
              @change="toggleGroup(g.group)"
            />
            <span>{{ g.label }}</span>
          </label>
        </div>
        <p class="faint tip">正在观看的会话不会被中途掐断，新起播、切集或拖动时自动按新时段切流；如整组均休眠会自动回落主机兜底。</p>
      </div>

      <p v-if="formError" class="err">{{ formError }}</p>
    </div>

    <template #footer>
      <UiButton :disabled="saving" @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="saving" @click="save">保存策略</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.sched-form { display: flex; flex-direction: column; gap: 16px; }
.section-block { display: flex; flex-direction: column; gap: 8px; }
.block-label { display: flex; align-items: center; justify-content: space-between; font-size: 12.5px; font-weight: 600; color: var(--text-2); }
.faint { color: var(--text-3); }
.mono { font-family: var(--font-mono); }

.mode-list { display: flex; flex-direction: column; gap: 8px; }
.mode-card {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 10px 12px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  cursor: pointer;
  transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease);
}
.mode-card:hover { border-color: var(--accent-border); }
.mode-card.active {
  border-color: var(--accent);
  background: color-mix(in srgb, var(--accent-soft) 72%, var(--surface));
}
.mode-card input { margin-top: 3px; accent-color: var(--accent); }
.mode-copy { display: flex; flex-direction: column; gap: 2px; font-size: 13px; }
.mode-copy span { font-size: 11.5px; }

.time-row { display: flex; align-items: center; gap: 10px; }
.time-input { flex: 1; min-width: 0; height: 36px; padding: 0 10px; font-size: 13.5px; font-weight: 600; }
.time-sep { font-size: 12.5px; flex-shrink: 0; }

.presets { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin-top: 2px; }
.preset-label { font-size: 11.5px; }
.preset-chip {
  height: 26px;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
  color: var(--text-2);
  font-size: 11.5px;
  font-weight: 500;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.preset-chip:hover,
.preset-chip.on {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent);
}

.summary-box {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  border: 1px solid var(--border);
  font-size: 12px;
  line-height: 1.55;
  color: var(--text-2);
}
.summary-icon { width: 15px; height: 15px; margin-top: 2px; color: var(--accent); flex-shrink: 0; }

.group-checks { display: flex; flex-wrap: wrap; gap: 8px; }
.group-check {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 32px;
  padding: 0 12px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
  font-size: 12.5px;
  font-weight: 500;
  color: var(--text-2);
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.group-check.checked {
  border-color: var(--accent);
  background: var(--accent-soft);
  color: var(--accent);
  font-weight: 600;
}
.group-check input { accent-color: var(--accent); }
.tip { margin: 2px 0 0; font-size: 11.5px; line-height: 1.5; }
.err { margin: 0; font-size: 12.5px; color: var(--bad); }
</style>
