<script setup lang="ts">
defineProps<{ label?: string; help?: string; error?: string; for?: string }>()
</script>

<template>
  <div class="field" :class="{ invalid: !!error }">
    <label v-if="label" :for="$props.for" class="label">{{ label }}</label>
    <slot />
    <p v-if="error" class="msg error">{{ error }}</p>
    <p v-else-if="help" class="msg">{{ help }}</p>
  </div>
</template>

<style scoped>
.field { display: flex; flex-direction: column; gap: 6px; min-width: 0; }
.label { font-size: 13px; font-weight: 500; color: var(--text-2); }
.msg { margin: 0; font-size: 12px; color: var(--text-3); line-height: 1.5; }
.msg.error { color: var(--bad); }
.invalid :deep(.input), .invalid :deep(.select) { border-color: var(--bad); }
</style>
