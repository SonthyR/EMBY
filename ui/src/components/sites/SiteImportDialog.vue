<script setup lang="ts">
import { ref, watch } from 'vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiBadge from '@/components/ui/UiBadge.vue'
import { sitesApi, type ImportPreview, type ImportResult } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const props = defineProps<{ open: boolean; format: string; data: string }>()
const emit = defineEmits<{ close: []; imported: [] }>()

const preview = ref<ImportPreview | null>(null)
const duplicateAction = ref('skip')
const actions = ref<Record<string, string>>({})
const busy = ref(false)
const result = ref<ImportResult | null>(null)
let generation = 0

async function refresh() {
  const current = ++generation
  busy.value = true
  try {
    const next = await sitesApi.importPreview(props.format, props.data, duplicateAction.value, actions.value)
    if (current === generation) preview.value = next
  } catch (error) {
    toast.error(errorMessage(error, '无法解析导入文件'))
    if (!preview.value) emit('close')
  } finally {
    if (current === generation) busy.value = false
  }
}

watch(
  () => props.open,
  open => {
    if (!open) return
    preview.value = null
    result.value = null
    duplicateAction.value = 'skip'
    actions.value = {}
    refresh()
  },
)

function setDuplicate(value: string) {
  duplicateAction.value = value
  actions.value = {}
  refresh()
}
function setAction(index: number, value: string) {
  actions.value = { ...actions.value, [index]: value }
  refresh()
}

async function confirmImport() {
  if (!preview.value) return
  busy.value = true
  try {
    const res = await sitesApi.importConfirm(props.format, props.data, duplicateAction.value, actions.value, preview.value.preview_token)
    const summary = `新增 ${res.created_sites}、更新 ${res.updated_sites}、跳过 ${res.skipped_sites}、失败 ${res.failed_sites}`
    emit('imported')
    if (res.failed_sites) result.value = res
    else {
      toast.success(summary)
      emit('close')
    }
  } catch (error) {
    toast.error(errorMessage(error, '导入失败'))
    refresh()
  } finally {
    busy.value = false
  }
}

const INGRESS: Record<string, string> = { path: '路径', host: '域名', both: '域名', port: '独立端口' }
</script>

<template>
  <UiDialog :open="open" :title="result ? '导入结果' : '导入预览'" :width="640" :dismissible="!busy" @close="emit('close')">
    <template v-if="result">
      <p>新增 {{ result.created_sites }}、更新 {{ result.updated_sites }}、跳过 {{ result.skipped_sites }}、失败 {{ result.failed_sites }}</p>
      <p v-for="item in result.items.filter(i => i.error)" :key="item.name" class="err">{{ item.name }}：{{ item.error }}</p>
    </template>
    <template v-else-if="preview">
      <p class="muted">共 {{ preview.items.length }} 个站点。新增的站点默认停用；更新会保留启停状态；没有提供密码时保留已有密码。</p>
      <label class="dup">
        <span>重复站点默认</span>
        <select class="select" :value="duplicateAction" :disabled="busy" @change="setDuplicate(($event.target as HTMLSelectElement).value)">
          <option value="skip">跳过</option>
          <option value="update">更新已有站点</option>
          <option value="new">另建站点</option>
        </select>
      </label>
      <ul class="items">
        <li v-for="item in preview.items" :key="item.index">
          <div class="info">
            <strong>{{ item.name }}</strong>
            <span class="mono faint">{{ item.target_url }}</span>
            <span class="faint">
              {{ item.account_count }} 个账号 ·
              <template v-if="item.duplicate_names.length">已匹配：{{ item.duplicate_names.join('、') }}</template>
              <template v-else>新站点</template>
              <template v-if="item.action !== 'skip'"> · {{ INGRESS[item.ingress_mode] || item.ingress_mode }} {{ item.path_prefix || item.public_host || '' }}</template>
            </span>
            <span v-if="item.error" class="err">{{ item.error }}</span>
          </div>
          <select class="select action" :value="item.action" :disabled="busy" aria-label="处理方式" @change="setAction(item.index, ($event.target as HTMLSelectElement).value)">
            <option value="new">新增</option>
            <option value="update" :disabled="item.duplicate_names.length !== 1">更新</option>
            <option value="skip">跳过</option>
          </select>
        </li>
      </ul>
    </template>
    <p v-else class="muted">正在解析…</p>
    <template #footer>
      <UiButton :disabled="busy" @click="emit('close')">{{ result ? '关闭' : '取消' }}</UiButton>
      <UiButton v-if="!result" variant="primary" :loading="busy" :disabled="!preview?.valid" @click="confirmImport">确认导入</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.dup { display: flex; align-items: center; gap: 10px; margin: 12px 0; font-size: 13px; }
.dup .select { width: auto; }
.items { display: flex; flex-direction: column; gap: 8px; margin: 0; padding: 0; list-style: none; }
.items li { display: flex; align-items: center; gap: 12px; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius); }
.info { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; font-size: 13px; }
.info .mono { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.action { width: 96px; flex: 0 0 auto; }
.err { color: var(--bad); font-size: 13px; margin: 0; }
</style>
