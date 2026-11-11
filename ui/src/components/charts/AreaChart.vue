<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

export interface Series {
  name: string
  color: string
  values: number[]
}
const props = withDefaults(
  defineProps<{ labels: number[]; series: Series[]; height?: number; format?: (v: number) => string; timeFormat?: (ms: number) => string }>(),
  { height: 220 },
)

const root = ref<HTMLElement | null>(null)
const width = ref(600)
const hover = ref<number | null>(null)
let observer: ResizeObserver | undefined
onMounted(() => {
  observer = new ResizeObserver(entries => {
    width.value = Math.max(240, entries[0].contentRect.width)
  })
  if (root.value) observer.observe(root.value)
})
onBeforeUnmount(() => observer?.disconnect())

const pad = { top: 12, right: 8, bottom: 24, left: 56 }
const innerW = computed(() => width.value - pad.left - pad.right)
const innerH = computed(() => props.height - pad.top - pad.bottom)
const count = computed(() => props.labels.length)
const max = computed(() => {
  let m = 0
  for (const s of props.series) for (const v of s.values) m = Math.max(m, v || 0)
  return m > 0 ? niceMax(m) : 1
})

function niceMax(value: number) {
  const exp = Math.pow(10, Math.floor(Math.log10(value)))
  const f = value / exp
  return (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * exp
}
function x(i: number) {
  return pad.left + (count.value <= 1 ? innerW.value / 2 : (i / (count.value - 1)) * innerW.value)
}
function y(v: number) {
  return pad.top + innerH.value - (Math.max(0, v || 0) / max.value) * innerH.value
}
function linePath(values: number[]) {
  return values.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join('')
}
function areaPath(values: number[]) {
  if (!values.length) return ''
  return `${linePath(values)}L${x(values.length - 1).toFixed(1)},${pad.top + innerH.value}L${x(0).toFixed(1)},${pad.top + innerH.value}Z`
}
const ticks = computed(() => [0, 0.25, 0.5, 0.75, 1].map(f => ({ value: max.value * f, y: y(max.value * f) })))
const xTicks = computed(() => {
  const n = count.value
  if (n < 2) return []
  const step = Math.max(1, Math.round(n / Math.max(2, Math.floor(innerW.value / 110))))
  const out: number[] = []
  for (let i = 0; i < n; i += step) out.push(i)
  return out
})
const fmt = (v: number) => (props.format ? props.format(v) : String(Math.round(v)))
const tfmt = (ms: number) => (props.timeFormat ? props.timeFormat(ms) : new Date(ms).toLocaleTimeString())

function onMove(event: PointerEvent) {
  if (!root.value || count.value === 0) return
  const rect = root.value.getBoundingClientRect()
  const px = event.clientX - rect.left - pad.left
  const i = Math.round((px / innerW.value) * (count.value - 1))
  hover.value = Math.min(count.value - 1, Math.max(0, i))
}
const tip = computed(() => {
  if (hover.value === null) return null
  const i = hover.value
  const left = x(i)
  return { left: Math.min(Math.max(left, 90), width.value - 90), time: tfmt(props.labels[i]), rows: props.series.map(s => ({ name: s.name, color: s.color, value: fmt(s.values[i] || 0) })) }
})
</script>

<template>
  <div ref="root" class="chart" @pointermove="onMove" @pointerleave="hover = null">
    <svg :width="width" :height="height" role="img" aria-label="流量趋势图">
      <defs>
        <linearGradient v-for="(s, k) in series" :id="`g${k}`" :key="k" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" :stop-color="s.color" stop-opacity="0.28" />
          <stop offset="100%" :stop-color="s.color" stop-opacity="0" />
        </linearGradient>
      </defs>
      <g class="grid">
        <g v-for="t in ticks" :key="t.value">
          <line :x1="pad.left" :x2="width - pad.right" :y1="t.y" :y2="t.y" />
          <text :x="pad.left - 8" :y="t.y + 4" text-anchor="end">{{ fmt(t.value) }}</text>
        </g>
        <text v-for="i in xTicks" :key="'x' + i" :x="x(i)" :y="height - 6" text-anchor="middle">{{ tfmt(labels[i]) }}</text>
      </g>
      <g v-for="(s, k) in series" :key="s.name">
        <path :d="areaPath(s.values)" :fill="`url(#g${k})`" />
        <path :d="linePath(s.values)" fill="none" :stroke="s.color" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" />
      </g>
      <g v-if="hover !== null">
        <line class="cursor" :x1="x(hover)" :x2="x(hover)" :y1="pad.top" :y2="pad.top + innerH" />
        <circle v-for="s in series" :key="s.name" :cx="x(hover)" :cy="y(s.values[hover] || 0)" r="4" :fill="s.color" stroke="var(--surface)" stroke-width="2" />
      </g>
    </svg>
    <div v-if="tip" class="tip" :style="{ left: tip.left + 'px' }">
      <strong>{{ tip.time }}</strong>
      <div v-for="row in tip.rows" :key="row.name"><span class="dot" :style="{ background: row.color }" />{{ row.name }}<b>{{ row.value }}</b></div>
    </div>
  </div>
</template>

<style scoped>
.chart { position: relative; width: 100%; user-select: none; touch-action: pan-y; }
svg { display: block; }
.grid line { stroke: var(--border); stroke-dasharray: 3 4; }
.grid text { fill: var(--text-3); font-size: 11px; }
.cursor { stroke: var(--text-3); stroke-dasharray: 3 3; }
.tip {
  position: absolute;
  top: 6px;
  transform: translateX(-50%);
  min-width: 150px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg-elev);
  box-shadow: var(--shadow);
  font-size: 12px;
  pointer-events: none;
}
.tip strong { display: block; margin-bottom: 4px; font-weight: 500; color: var(--text-2); }
.tip div { display: flex; align-items: center; gap: 6px; }
.tip b { margin-left: auto; font-weight: 600; }
.dot { width: 8px; height: 8px; border-radius: 50%; }
</style>
