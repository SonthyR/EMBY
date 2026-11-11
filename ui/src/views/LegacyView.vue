<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { Hammer } from '@lucide/vue'

// Pages that are not rebuilt yet stay reachable in the previous panel.
const route = useRoute()
const legacyHash = computed(() => String((route.meta as Record<string, unknown>).legacy || 'dashboard'))
</script>

<template>
  <section class="legacy">
    <Hammer />
    <h2>这个页面正在重做</h2>
    <p>新版界面会分批上线。在此之前，可以先在旧版面板里使用这个功能，数据是同一份。</p>
    <a class="open" :href="`/#${legacyHash}`">在旧版面板中打开</a>
  </section>
</template>

<style scoped>
.legacy {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  max-width: 460px;
  margin: 48px auto;
  padding: 36px 24px;
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-lg);
  text-align: center;
}
.legacy svg { width: 36px; height: 36px; color: var(--accent); }
h2 { margin: 8px 0 0; font-size: 18px; }
p { margin: 0; color: var(--text-2); line-height: 1.7; }
.open {
  margin-top: 12px;
  padding: 9px 16px;
  border-radius: var(--radius-sm);
  background: var(--accent-strong);
  color: var(--on-accent);
  font-weight: 500;
}
</style>
