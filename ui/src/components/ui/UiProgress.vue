<script setup lang="ts">
import { computed } from 'vue'
const props = defineProps<{ value: number; max: number }>()
const percent = computed(() => (props.max > 0 ? Math.min(100, Math.max(0, (props.value / props.max) * 100)) : 0))
const tone = computed(() => (percent.value >= 100 ? 'bad' : percent.value >= 85 ? 'warn' : 'normal'))
</script>

<template>
  <div class="progress" role="progressbar" :aria-valuenow="Math.round(percent)" aria-valuemin="0" aria-valuemax="100">
    <span :class="tone" :style="{ width: percent + '%' }" />
  </div>
</template>

<style scoped>
.progress { height: 3px; border-radius: 999px; background: var(--border); overflow: hidden; }
span { display: block; height: 100%; border-radius: inherit; transition: width 500ms var(--ease); }
.normal { background: var(--text-2); }
.warn { background: var(--warn); }
.bad { background: var(--bad); }
</style>
