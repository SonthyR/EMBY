<script setup lang="ts">
const model = defineModel<boolean>({ required: true })
defineProps<{ label?: string; description?: string; disabled?: boolean }>()
</script>

<template>
  <label class="switch" :class="{ disabled }">
    <span v-if="label" class="text">
      <span class="label">{{ label }}</span>
      <span v-if="description" class="desc">{{ description }}</span>
    </span>
    <input v-model="model" type="checkbox" role="switch" :disabled="disabled" />
    <span class="track"><span class="thumb" /></span>
  </label>
</template>

<style scoped>
.switch { display: flex; align-items: center; gap: 14px; cursor: pointer; user-select: none; }
.switch.disabled { opacity: 0.45; cursor: not-allowed; }
.text { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.label { font-size: 14px; }
.desc { font-size: 12.5px; color: var(--text-3); }
input { position: absolute; opacity: 0; width: 1px; height: 1px; }
.track { position: relative; flex: 0 0 34px; width: 34px; height: 20px; border-radius: 999px; background: var(--border-strong); transition: background var(--dur) var(--ease); }
.thumb { position: absolute; top: 2px; left: 2px; width: 16px; height: 16px; border-radius: 50%; background: var(--bg-elev); box-shadow: 0 1px 2px rgba(0, 0, 0, 0.25); transition: transform 220ms var(--ease); }
input:checked + .track { background: var(--inverse-bg); }
input:checked + .track .thumb { transform: translateX(14px); }
input:focus-visible + .track { outline: 2px solid color-mix(in srgb, var(--text) 50%, transparent); outline-offset: 2px; }
</style>
