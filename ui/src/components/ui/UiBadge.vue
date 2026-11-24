<script setup lang="ts">
withDefaults(defineProps<{ tone?: 'neutral' | 'accent' | 'good' | 'warn' | 'bad'; dot?: boolean }>(), { tone: 'neutral' })
</script>

<template>
  <span class="badge" :class="`tone-${tone}`"><span v-if="dot" class="dot" :class="tone" /><slot /></span>
</template>

<style scoped>
.badge { display: inline-flex; align-items: center; gap: 6px; height: 23px; padding: 0 9px; border: 1px solid var(--border); border-radius: 999px; background: var(--surface-2); font-size: 11.5px; font-weight: 500; color: var(--text-2); white-space: nowrap; }
.tone-accent { background: var(--accent-soft); border-color: var(--accent-border); color: var(--accent); }
.tone-good { background: var(--good-soft); border-color: color-mix(in srgb, var(--good) 26%, transparent); color: var(--good); }
.tone-warn { background: var(--warn-soft); border-color: color-mix(in srgb, var(--warn) 28%, transparent); color: var(--warn); }
.tone-bad { background: var(--bad-soft); border-color: color-mix(in srgb, var(--bad) 28%, transparent); color: var(--bad); }
</style>
