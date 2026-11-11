<script setup lang="ts">
import { Copy, PartyPopper } from '@lucide/vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiButton from '@/components/ui/UiButton.vue'
import { copyText } from '@/utils/format'
import { toast } from '@/stores/toast'

const props = defineProps<{ open: boolean; text: string; created?: boolean }>()
const emit = defineEmits<{ close: [] }>()

async function copy() {
  if (await copyText(props.text)) {
    toast.success('已复制，直接发给朋友就行')
    emit('close')
  } else {
    toast.error('浏览器不允许自动复制，请手动选中文字复制')
  }
}
</script>

<template>
  <UiDialog :open="open" :title="created ? '分享账号已创建' : '分享信息'" :width="520" @close="emit('close')">
    <p v-if="created" class="hint"><PartyPopper />把下面这段发给朋友，他就能在 Emby 客户端里登录了。</p>
    <pre class="text">{{ text }}</pre>
    <template #footer>
      <UiButton @click="emit('close')">关闭</UiButton>
      <UiButton variant="primary" autofocus @click="copy"><Copy />复制</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.hint { display: flex; align-items: center; gap: 8px; margin: 0 0 12px; color: var(--text-2); }
.hint svg { width: 18px; height: 18px; color: var(--accent); }
.text {
  margin: 0;
  padding: 14px 16px;
  border-radius: var(--radius);
  background: var(--surface-2);
  border: 1px solid var(--border);
  font: 13px/1.8 var(--font);
  white-space: pre-wrap;
  word-break: break-all;
  user-select: all;
}
</style>
