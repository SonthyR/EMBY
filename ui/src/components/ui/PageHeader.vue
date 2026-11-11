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
.page-header { display: flex; align-items: flex-end; justify-content: space-between; gap: 16px; margin-bottom: 28px; }
.text { min-width: 0; }
h1 { margin: 0; font-size: 24px; font-weight: 600; letter-spacing: -0.025em; line-height: 1.2; }
p { margin: 6px 0 0; color: var(--text-2); font-size: 14px; }
.actions { display: flex; gap: 8px; flex: 0 0 auto; }
@media (max-width: 720px) {
  .page-header { margin-bottom: 20px; }
  h1 { font-size: 21px; }
  .actions { display: none; }
}
</style>
