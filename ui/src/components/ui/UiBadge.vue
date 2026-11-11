<script setup lang="ts">
withDefaults(defineProps<{ tone?: 'neutral' | 'accent' | 'good' | 'warn' | 'bad'; dot?: boolean }>(), { tone: 'neutral' })
</script>

<template>
  <span class="badge" :class="`tone-${tone}`"><span v-if="dot" class="dot" /><slot /></span>
</template>

<style scoped>
.badge {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 22px;
  padding: 1px 8px;
  border-radius: 999px;
  font-size: 12px;
  font-weight: 500;
  white-space: nowrap;
}
.dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.tone-neutral { background: var(--surface-hover); color: var(--text-2); }
.tone-accent { background: var(--accent-soft); color: var(--accent); }
.tone-good { background: var(--good-soft); color: var(--good); }
.tone-warn { background: var(--warn-soft); color: var(--warn); }
.tone-bad { background: var(--bad-soft); color: var(--bad); }
</style>
