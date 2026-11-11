<script setup lang="ts">
import UiDialog from './ui/UiDialog.vue'
import UiButton from './ui/UiButton.vue'
import { confirmState, settleConfirm } from '@/stores/confirm'
</script>

<template>
  <UiDialog :open="confirmState.open" :title="confirmState.title" :width="420" @close="settleConfirm(false)">
    <p v-if="confirmState.message" class="message">{{ confirmState.message }}</p>
    <template #footer>
      <UiButton @click="settleConfirm(false)">取消</UiButton>
      <UiButton :variant="confirmState.danger ? 'danger' : 'primary'" autofocus @click="settleConfirm(true)">{{ confirmState.confirmText }}</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.message { margin: 0; color: var(--text-2); line-height: 1.7; }
</style>
