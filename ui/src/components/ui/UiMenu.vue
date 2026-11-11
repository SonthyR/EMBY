<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'

export interface MenuItem {
  key: string
  label: string
  danger?: boolean
  divider?: boolean
}
defineProps<{ items: MenuItem[]; label?: string }>()
const emit = defineEmits<{ select: [key: string] }>()
const open = ref(false)
const root = ref<HTMLElement | null>(null)

function onDocument(event: MouseEvent) {
  if (root.value && !root.value.contains(event.target as Node)) open.value = false
}
function onKey(event: KeyboardEvent) {
  if (event.key === 'Escape') open.value = false
}
watch(open, value => {
  if (value) {
    document.addEventListener('mousedown', onDocument)
    document.addEventListener('keydown', onKey)
  } else {
    document.removeEventListener('mousedown', onDocument)
    document.removeEventListener('keydown', onKey)
  }
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocument)
  document.removeEventListener('keydown', onKey)
})
function choose(key: string) {
  open.value = false
  emit('select', key)
}
</script>

<template>
  <div ref="root" class="menu">
    <span @click="open = !open"><slot /></span>
    <Transition name="pop">
      <div v-if="open" class="list" role="menu" :aria-label="label">
        <template v-for="item in items" :key="item.key">
          <hr v-if="item.divider" />
          <button v-else type="button" role="menuitem" :class="{ danger: item.danger }" @click="choose(item.key)">{{ item.label }}</button>
        </template>
      </div>
    </Transition>
  </div>
</template>

<style scoped>
.menu { position: relative; display: inline-flex; }
.list {
  position: absolute;
  right: 0;
  top: calc(100% + 6px);
  z-index: 50;
  min-width: 168px;
  padding: 6px;
  background: var(--bg-elev);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  box-shadow: var(--shadow);
}
button {
  display: block;
  width: 100%;
  padding: 8px 10px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  text-align: left;
  font-size: 13px;
  cursor: pointer;
}
button:hover { background: var(--surface-hover); }
button.danger { color: var(--bad); }
hr { border: 0; border-top: 1px solid var(--border); margin: 4px 0; }
.pop-enter-active, .pop-leave-active { transition: opacity 120ms var(--ease), transform 120ms var(--ease); }
.pop-enter-from, .pop-leave-to { opacity: 0; transform: translateY(-4px); }
</style>
