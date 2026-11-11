<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { sitesApi } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ close: [] }>()
const format = ref('json')
const mode = ref('multi')
const accounts = ref(true)
const passwords = ref(false)
const busy = ref(false)
watch(accounts, value => {
  if (!value) passwords.value = false
})
watch(
  () => props.open,
  open => {
    if (open) busy.value = false
  },
)
const ext = computed(() => (format.value === 'csv' ? 'csv' : format.value === 'client' ? 'txt' : 'json'))

async function download() {
  busy.value = true
  try {
    const blob = await sitesApi.export(format.value, format.value === 'client' ? mode.value : '', accounts.value, passwords.value && accounts.value)
    const link = document.createElement('a')
    link.href = URL.createObjectURL(blob)
    link.download = `yundu-sites.${ext.value}`
    link.click()
    setTimeout(() => URL.revokeObjectURL(link.href), 1000)
    emit('close')
  } catch (error) {
    toast.error(errorMessage(error, '导出失败'))
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <UiDialog :open="open" title="导出站点" :width="460" @close="emit('close')">
    <div class="opts">
      <div class="seg" role="radiogroup" aria-label="导出格式">
        <button type="button" :class="{ on: format === 'json' }" @click="format = 'json'">JSON 配置</button>
        <button type="button" :class="{ on: format === 'csv' }" @click="format = 'csv'">CSV 表格</button>
        <button type="button" :class="{ on: format === 'client' }" @click="format = 'client'">客户端文本</button>
      </div>
      <div v-if="format === 'client'" class="seg small" role="radiogroup" aria-label="线路">
        <button type="button" :class="{ on: mode === 'multi' }" @click="mode = 'multi'">多线路</button>
        <button type="button" :class="{ on: mode === 'original' }" @click="mode = 'original'">原线路</button>
        <button type="button" :class="{ on: mode === 'proxy' }" @click="mode = 'proxy'">反代线路</button>
      </div>
      <p class="muted">所有站点都会导出，包括没有账号的站点。</p>
      <UiSwitch v-model="accounts" label="包含账号信息" />
      <UiSwitch v-model="passwords" :disabled="!accounts" label="包含登录密码和安全密码" />
    </div>
    <template #footer>
      <UiButton @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="busy" @click="download">下载文件</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.opts { display: flex; flex-direction: column; gap: 14px; }
.muted { margin: 0; font-size: 13px; }
.seg { display: flex; gap: 4px; padding: 3px; border: 1px solid var(--border); border-radius: 10px; background: var(--surface-2); }
.seg button { flex: 1; padding: 8px 6px; border: 0; border-radius: 7px; background: transparent; color: var(--text-2); font-size: 13px; cursor: pointer; }
.seg button.on { background: var(--surface); color: var(--text); box-shadow: 0 1px 3px rgba(0, 0, 0, 0.15); font-weight: 500; }
.seg.small button { padding: 6px; font-size: 12px; }
</style>
