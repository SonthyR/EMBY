<script setup lang="ts">
withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md'
    loading?: boolean
    disabled?: boolean
    type?: 'button' | 'submit'
    block?: boolean
  }>(),
  { variant: 'secondary', size: 'md', type: 'button' },
)
</script>

<template>
  <button
    :type="type"
    class="btn"
    :class="[`btn-${variant}`, `btn-${size}`, { 'btn-block': block, 'is-loading': loading }]"
    :disabled="disabled || loading"
  >
    <span v-if="loading" class="spinner" aria-hidden="true" />
    <slot />
  </button>
</template>

<style scoped>
.btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  font-weight: 500;
  white-space: nowrap;
  cursor: pointer;
  user-select: none;
  transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), color var(--dur) var(--ease), transform 80ms var(--ease);
}
.btn:active:not(:disabled) { transform: scale(0.97); }
.btn:disabled { opacity: 0.55; cursor: not-allowed; }
.btn-md { min-height: 38px; padding: 0 14px; font-size: 14px; }
.btn-sm { min-height: 32px; padding: 0 10px; font-size: 13px; }
.btn-block { width: 100%; }
.btn :deep(svg) { width: 16px; height: 16px; flex: 0 0 16px; }
.btn-primary { background: var(--accent-strong); color: var(--on-accent); }
.btn-primary:hover:not(:disabled) { background: var(--accent); }
.btn-secondary { background: var(--surface-2); border-color: var(--border); color: var(--text); }
.btn-secondary:hover:not(:disabled) { background: var(--surface-hover); border-color: var(--border-strong); }
.btn-ghost { background: transparent; color: var(--text-2); }
.btn-ghost:hover:not(:disabled) { background: var(--surface-hover); color: var(--text); }
.btn-danger { background: var(--bad-soft); color: var(--bad); }
.btn-danger:hover:not(:disabled) { background: var(--bad); color: #fff; }
.spinner {
  width: 14px;
  height: 14px;
  border: 2px solid currentColor;
  border-right-color: transparent;
  border-radius: 50%;
  animation: spin 700ms linear infinite;
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
