<script setup lang="ts">
defineProps<{ title: string; description?: string }>()
</script>

<template>
  <header class="page-header">
    <div class="text">
      <h1>{{ title }}</h1>
      <p v-if="description">{{ description }}</p>
    </div>
    <div v-if="$slots.default" class="actions"><slot /></div>
  </header>
</template>

<style scoped>
.page-header { display: flex; align-items: flex-end; justify-content: space-between; gap: 16px; margin-bottom: 24px; }
.text { min-width: 0; }
h1 { margin: 0; font-size: 26px; font-weight: 700; letter-spacing: -0.03em; line-height: 1.18; }
p { margin: 6px 0 0; color: var(--text-2); font-size: 13.5px; }
.actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; flex: 0 0 auto; }
@media (max-width: 720px) {
  .page-header { flex-direction: column; align-items: stretch; gap: 12px; margin-bottom: 18px; }
  h1 { font-size: 22px; }
  .actions { justify-content: flex-start; }
}
</style>
