<script setup lang="ts">
const model = defineModel<boolean>({ required: true })
defineProps<{ label?: string; disabled?: boolean }>()
</script>

<template>
  <label class="switch" :class="{ disabled }">
    <input v-model="model" type="checkbox" role="switch" :disabled="disabled" />
    <span class="track"><span class="thumb" /></span>
    <span v-if="label" class="text">{{ label }}</span>
  </label>
</template>

<style scoped>
.switch { display: inline-flex; align-items: center; gap: 10px; cursor: pointer; user-select: none; }
.switch.disabled { opacity: 0.55; cursor: not-allowed; }
input { position: absolute; opacity: 0; width: 1px; height: 1px; }
.track {
  position: relative;
  width: 38px;
  height: 22px;
  border-radius: 999px;
  background: var(--border-strong);
  transition: background var(--dur) var(--ease);
}
.thumb {
  position: absolute;
  top: 3px;
  left: 3px;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: #fff;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.3);
  transition: transform var(--dur) var(--ease);
}
input:checked + .track { background: var(--accent-strong); }
input:checked + .track .thumb { transform: translateX(16px); }
input:focus-visible + .track { outline: 2px solid var(--accent); outline-offset: 2px; }
.text { font-size: 14px; }
</style>
