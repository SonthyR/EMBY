<script setup lang="ts">
withDefaults(
  defineProps<{
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md'
    loading?: boolean
    disabled?: boolean
    type?: 'button' | 'submit'
    block?: boolean
    icon?: boolean
  }>(),
  { variant: 'secondary', size: 'md', type: 'button' },
)
</script>

<template>
  <button
    :type="type"
    class="btn"
    :class="[`btn-${variant}`, `btn-${size}`, { 'btn-block': block, 'btn-icon': icon }]"
    :disabled="disabled || loading"
    :aria-busy="loading || undefined"
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
  letter-spacing: -0.005em;
  white-space: nowrap;
  cursor: pointer;
  user-select: none;
  transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), color var(--dur) var(--ease), opacity var(--dur) var(--ease);
}
.btn:disabled { opacity: 0.45; cursor: not-allowed; }
.btn-md { height: 34px; padding: 0 13px; font-size: 13.5px; }
.btn-sm { height: 28px; padding: 0 9px; font-size: 13px; border-radius: var(--radius-xs); }
.btn-icon.btn-md { width: 34px; padding: 0; }
.btn-icon.btn-sm { width: 28px; padding: 0; }
.btn-block { width: 100%; }
.btn :deep(svg) { width: 15px; height: 15px; flex: 0 0 15px; stroke-width: 1.8; }
.btn-primary { background: var(--inverse-bg); color: var(--inverse-text); }
.btn-primary:hover:not(:disabled) { opacity: 0.88; }
.btn-secondary { background: transparent; border-color: var(--border-strong); color: var(--text); }
.btn-secondary:hover:not(:disabled) { background: var(--surface-hover); }
.btn-ghost { background: transparent; color: var(--text-2); }
.btn-ghost:hover:not(:disabled) { background: var(--surface-hover); color: var(--text); }
.btn-danger { background: transparent; border-color: color-mix(in srgb, var(--bad) 40%, transparent); color: var(--bad); }
.btn-danger:hover:not(:disabled) { background: var(--bad-soft); }
.spinner { width: 13px; height: 13px; border: 1.5px solid currentColor; border-right-color: transparent; border-radius: 50%; animation: spin 700ms linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
</style>
