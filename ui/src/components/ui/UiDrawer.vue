<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { X } from '@lucide/vue'

// Side sheet on desktop, bottom sheet on phones. Used for details and forms so
// the list stays in view and context is never lost behind a centered modal.
const props = withDefaults(defineProps<{ open: boolean; title?: string; subtitle?: string; width?: number; dismissible?: boolean }>(), {
  width: 480,
  dismissible: true,
})
const emit = defineEmits<{ close: [] }>()
const panel = ref<HTMLElement | null>(null)
let previous: Element | null = null

function onKey(event: KeyboardEvent) {
  if (event.key === 'Escape' && props.dismissible) {
    event.stopPropagation()
    emit('close')
  }
}
watch(
  () => props.open,
  async open => {
    if (open) {
      previous = document.activeElement
      document.addEventListener('keydown', onKey)
      await nextTick()
      ;(panel.value?.querySelector<HTMLElement>('[autofocus]') || panel.value)?.focus({ preventScroll: true })
    } else {
      document.removeEventListener('keydown', onKey)
      if (previous instanceof HTMLElement) previous.focus({ preventScroll: true })
    }
  },
  { immediate: true },
)
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <Transition name="drawer">
      <div v-if="open" class="layer" @mousedown.self="dismissible && emit('close')">
        <aside ref="panel" class="drawer" role="dialog" aria-modal="true" :aria-label="title" tabindex="-1" :style="{ '--w': width + 'px' }">
          <header class="head">
            <div class="titles">
              <slot name="title"><h2>{{ title }}</h2></slot>
              <p v-if="subtitle" class="sub">{{ subtitle }}</p>
            </div>
            <slot name="actions" />
            <button v-if="dismissible" type="button" class="close" aria-label="关闭" @click="emit('close')"><X /></button>
          </header>
          <div class="body"><slot /></div>
          <footer v-if="$slots.footer" class="foot"><slot name="footer" /></footer>
        </aside>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.layer { position: fixed; inset: 0; z-index: 100; background: var(--overlay); }
.drawer {
  position: absolute;
  top: 8px;
  right: 8px;
  bottom: 8px;
  display: flex;
  flex-direction: column;
  width: min(var(--w), calc(100% - 16px));
  background: var(--bg-elev);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow);
  outline: none;
}
.head { display: flex; align-items: flex-start; gap: 8px; padding: 18px 18px 14px 22px; border-bottom: 1px solid var(--border); }
.titles { flex: 1; min-width: 0; }
.titles :deep(h2), h2 { margin: 0; font-size: 16px; font-weight: 600; letter-spacing: -0.01em; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sub { margin: 2px 0 0; font-size: 13px; color: var(--text-3); }
.close { display: grid; place-items: center; width: 30px; height: 30px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; }
.close:hover { background: var(--surface-hover); color: var(--text); }
.close svg { width: 17px; height: 17px; }
.body { flex: 1; overflow: auto; padding: 18px 22px 24px; overscroll-behavior: contain; }
.foot { display: flex; justify-content: flex-end; gap: 8px; padding: 14px 22px; border-top: 1px solid var(--border); }

.drawer-enter-active, .drawer-leave-active { transition: background 220ms var(--ease); }
.drawer-enter-active .drawer, .drawer-leave-active .drawer { transition: transform 280ms var(--ease), opacity 200ms var(--ease); }
.drawer-enter-from, .drawer-leave-to { background: transparent; }
.drawer-enter-from .drawer, .drawer-leave-to .drawer { transform: translateX(24px); opacity: 0; }

@media (max-width: 720px) {
  .drawer { top: auto; left: 0; right: 0; bottom: 0; width: 100%; max-height: 92vh; border-radius: 16px 16px 0 0; padding-bottom: env(safe-area-inset-bottom); }
  .drawer-enter-from .drawer, .drawer-leave-to .drawer { transform: translateY(32px); }
  .foot > :deep(*) { flex: 1; }
}
</style>
