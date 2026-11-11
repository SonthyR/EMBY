<script setup lang="ts">
withDefaults(defineProps<{ tone?: 'neutral' | 'accent' | 'good' | 'warn' | 'bad'; dot?: boolean }>(), { tone: 'neutral' })
</script>

<template>
  <span class="badge" :class="`tone-${tone}`"><span v-if="dot" class="dot" :class="tone" /><slot /></span>
</template>

<style scoped>
.badge { display: inline-flex; align-items: center; gap: 7px; height: 22px; padding: 0 8px; border: 1px solid var(--border); border-radius: 999px; font-size: 12px; color: var(--text-2); white-space: nowrap; }
.tone-accent { color: var(--text); }
.tone-bad { color: var(--bad); border-color: color-mix(in srgb, var(--bad) 30%, transparent); }
.tone-warn { color: var(--warn); border-color: color-mix(in srgb, var(--warn) 30%, transparent); }
</style>
