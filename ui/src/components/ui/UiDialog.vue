<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { X } from '@lucide/vue'

const props = withDefaults(
  defineProps<{ open: boolean; title?: string; width?: number; dismissible?: boolean; sheet?: boolean }>(),
  { width: 520, dismissible: true, sheet: true },
)
const emit = defineEmits<{ close: [] }>()
const panel = ref<HTMLElement | null>(null)
let previousFocus: Element | null = null

function close() {
  emit('close')
}
function onKey(event: KeyboardEvent) {
  if (!props.open) return
  if (event.key === 'Escape' && props.dismissible) {
    event.stopPropagation()
    close()
  }
  if (event.key === 'Tab' && panel.value) {
    const focusable = [...panel.value.querySelectorAll<HTMLElement>('button, a[href], input, select, textarea, [tabindex]:not([tabindex="-1"])')].filter(
      el => !el.hasAttribute('disabled') && el.offsetParent !== null,
    )
    if (!focusable.length) return
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault()
      last.focus()
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault()
      first.focus()
    }
  }
}

watch(
  () => props.open,
  async open => {
    if (open) {
      previousFocus = document.activeElement
      document.addEventListener('keydown', onKey)
      document.body.style.overflow = 'hidden'
      await nextTick()
      const autofocus = panel.value?.querySelector<HTMLElement>('[autofocus]')
      ;(autofocus || panel.value)?.focus({ preventScroll: true })
    } else {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = ''
      if (previousFocus instanceof HTMLElement) previousFocus.focus({ preventScroll: true })
    }
  },
  { immediate: true },
)
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKey)
  document.body.style.overflow = ''
})
</script>

<template>
  <Teleport to="body">
    <Transition name="dialog">
      <div v-if="open" class="overlay" :class="{ sheet }" @mousedown.self="dismissible && close()">
        <div ref="panel" class="panel" role="dialog" aria-modal="true" :aria-label="title" tabindex="-1" :style="{ '--w': width + 'px' }">
          <header v-if="title || $slots.header" class="head">
            <slot name="header"><h2>{{ title }}</h2></slot>
            <button v-if="dismissible" class="close" type="button" aria-label="关闭" @click="close"><X /></button>
          </header>
          <div class="body"><slot /></div>
          <footer v-if="$slots.footer" class="foot"><slot name="footer" /></footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.overlay {
  position: fixed;
  inset: 0;
  z-index: 100;
  display: grid;
  place-items: center;
  padding: 24px 16px;
  background: var(--overlay);
  backdrop-filter: blur(3px);
}
.panel {
  display: flex;
  flex-direction: column;
  width: min(var(--w), 100%);
  max-height: min(88vh, 900px);
  background: var(--bg-elev);
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow);
  outline: none;
}
.head { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 18px 20px 6px; }
.head h2 { margin: 0; font-size: 17px; font-weight: 600; }
.close {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
}
.close:hover { background: var(--surface-hover); color: var(--text); }
.close svg { width: 18px; height: 18px; }
.body { padding: 12px 20px 16px; overflow: auto; overscroll-behavior: contain; }
.foot { display: flex; justify-content: flex-end; gap: 10px; padding: 12px 20px 18px; border-top: 1px solid var(--border); }

.dialog-enter-active, .dialog-leave-active { transition: opacity 180ms var(--ease); }
.dialog-enter-active .panel, .dialog-leave-active .panel { transition: transform 220ms var(--ease); }
.dialog-enter-from, .dialog-leave-to { opacity: 0; }
.dialog-enter-from .panel, .dialog-leave-to .panel { transform: translateY(10px) scale(0.98); }

@media (max-width: 640px) {
  .overlay.sheet { place-items: end stretch; padding: 0; }
  .sheet .panel { width: 100%; max-height: 92vh; border-radius: var(--radius-lg) var(--radius-lg) 0 0; padding-bottom: env(safe-area-inset-bottom); }
  .sheet.dialog-enter-from .panel, .sheet.dialog-leave-to .panel { transform: translateY(40px); }
  .foot { flex-wrap: wrap; }
  .foot :deep(.btn) { flex: 1; }
}
</style>
