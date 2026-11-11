<script setup lang="ts">
import { CircleCheck, CircleAlert, Info } from '@lucide/vue'
import { dismiss, toasts } from '@/stores/toast'
</script>

<template>
  <div class="toasts" aria-live="polite">
    <TransitionGroup name="toast">
      <button v-for="item in toasts" :key="item.id" type="button" class="toast" :class="item.kind" @click="dismiss(item.id)">
        <CircleCheck v-if="item.kind === 'success'" />
        <CircleAlert v-else-if="item.kind === 'error'" />
        <Info v-else />
        <span>{{ item.message }}</span>
      </button>
    </TransitionGroup>
  </div>
</template>

<style scoped>
.toasts {
  position: fixed;
  z-index: 200;
  top: 16px;
  left: 50%;
  transform: translateX(-50%);
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  width: min(440px, calc(100% - 32px));
  pointer-events: none;
}
.toast {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  max-width: 100%;
  padding: 10px 14px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--bg-elev);
  box-shadow: var(--shadow);
  text-align: left;
  font-size: 14px;
  cursor: pointer;
  pointer-events: auto;
}
.toast svg { width: 18px; height: 18px; flex: 0 0 18px; margin-top: 1px; }
.success svg { color: var(--good); }
.error svg { color: var(--bad); }
.info svg { color: var(--accent); }
.toast-enter-active, .toast-leave-active { transition: all 220ms var(--ease); }
.toast-enter-from, .toast-leave-to { opacity: 0; transform: translateY(-8px) scale(0.98); }
</style>
