<script setup lang="ts">
withDefaults(
  defineProps<{
    variant?: 'primary' | 'accent' | 'secondary' | 'ghost' | 'danger'
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
  border-radius: 999px;
  font-weight: 500;
  letter-spacing: -0.005em;
  white-space: nowrap;
  cursor: pointer;
  user-select: none;
  transition: background var(--dur) var(--ease), border-color var(--dur) var(--ease), color var(--dur) var(--ease), transform 120ms var(--ease), opacity var(--dur) var(--ease), box-shadow var(--dur) var(--ease);
}
.btn:active:not(:disabled) { transform: scale(0.98); }
.btn:disabled { opacity: 0.45; cursor: not-allowed; }
.btn-md { height: 36px; padding: 0 15px; font-size: 13.5px; }
.btn-sm { height: 30px; padding: 0 12px; font-size: 12.5px; }
.btn-icon.btn-md { width: 36px; padding: 0; border-radius: 50%; }
.btn-icon.btn-sm { width: 30px; padding: 0; border-radius: 50%; }
.btn-block { width: 100%; }
.btn :deep(svg) { width: 15px; height: 15px; flex: 0 0 15px; stroke-width: 1.85; }
.btn-primary { background: var(--inverse-bg); color: var(--inverse-text); box-shadow: 0 2px 6px rgba(15, 23, 42, 0.14); }
.btn-primary:hover:not(:disabled) { opacity: 0.9; }
.btn-accent { background: var(--accent); color: #ffffff; box-shadow: 0 4px 14px color-mix(in srgb, var(--accent) 32%, transparent); }
.btn-accent:hover:not(:disabled) { filter: brightness(1.06); }
.btn-secondary { background: var(--surface); border-color: var(--border-strong); color: var(--text); }
.btn-secondary:hover:not(:disabled) { background: var(--surface-2); }
.btn-ghost { background: transparent; color: var(--text-2); }
.btn-ghost:hover:not(:disabled) { background: var(--surface-hover); color: var(--text); }
.btn-danger { background: transparent; border-color: color-mix(in srgb, var(--bad) 36%, transparent); color: var(--bad); }
.btn-danger:hover:not(:disabled) { background: var(--bad-soft); }
.spinner { width: 13px; height: 13px; border: 1.5px solid currentColor; border-right-color: transparent; border-radius: 50%; animation: spin 700ms linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
</style>
