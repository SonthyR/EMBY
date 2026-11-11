<script setup lang="ts">
import { reactive, ref } from 'vue'
import { Plus, Trash2, Pencil } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import { sitesApi, type SiteAccount } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { dateOnly } from '@/utils/format'

// Source-site accounts saved for one site. With a siteId the changes are saved
// immediately; without one (a new site) they stay local and the parent sends
// them along with the create request.
const props = defineProps<{ siteId?: number; modelValue: SiteAccount[] }>()
const emit = defineEmits<{ 'update:modelValue': [value: SiteAccount[]] }>()

const blank = () => ({ id: 0, account_name: '', username: '', password: '', security_password: '', expires_at: '', note: '', reminder_days: '' as string | number })
const editing = ref<number | null>(null) // account id, -1 for new
const draft = reactive(blank())
const busy = ref(false)
let tempId = -1

function open(account?: SiteAccount) {
  Object.assign(draft, blank(), account ? { ...account, password: account.password || '', security_password: account.security_password || '', expires_at: dateOnly(account.expires_at), reminder_days: account.reminder_days ?? '' } : {})
  editing.value = account ? account.id : -1
}

async function save() {
  if (!draft.username.trim() && !draft.account_name.trim()) return toast.error('至少填写用户名')
  const days = String(draft.reminder_days).trim()
  const payload: Record<string, unknown> = {
    account_name: draft.account_name.trim() || draft.username.trim(),
    username: draft.username.trim() || draft.account_name.trim(),
    password: draft.password,
    security_password: draft.security_password,
    expires_at: draft.expires_at,
    note: draft.note.trim(),
    ...(days === '' ? { clear_reminder: editing.value !== -1 } : { reminder_days: Number(days) }),
  }
  if (editing.value !== -1 && !draft.expires_at) payload.clear_expires_at = true
  if (!props.siteId) {
    const list = [...props.modelValue]
    const local = { ...payload, id: editing.value === -1 ? tempId-- : editing.value, reminder_days: days === '' ? null : Number(days) } as unknown as SiteAccount
    const index = list.findIndex(a => a.id === editing.value)
    if (index >= 0) list[index] = local
    else list.push(local)
    emit('update:modelValue', list)
    editing.value = null
    return
  }
  busy.value = true
  try {
    const saved = editing.value === -1 ? await sitesApi.createAccount(props.siteId, payload) : await sitesApi.updateAccount(props.siteId, editing.value!, payload)
    const list = props.modelValue.filter(a => a.id !== saved.id)
    emit('update:modelValue', editing.value === -1 ? [...list, saved] : props.modelValue.map(a => (a.id === saved.id ? saved : a)))
    editing.value = null
    toast.success('源站账号已保存')
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = false
  }
}

async function remove(account: SiteAccount) {
  if (!(await confirm({ title: `删除源站账号 ${account.account_name || account.username}？`, message: '用这个账号代登的分享账号会改为自动选择其他账号。', confirmText: '删除', danger: true }))) return
  if (props.siteId && account.id > 0) {
    try {
      await sitesApi.deleteAccount(props.siteId, account.id)
    } catch (error) {
      return toast.error(errorMessage(error))
    }
  }
  emit('update:modelValue', props.modelValue.filter(a => a.id !== account.id))
}
</script>

<template>
  <div class="accounts">
    <ul v-if="modelValue.length" class="list">
      <li v-for="a in modelValue" :key="a.id">
        <div class="info">
          <span>{{ a.account_name || a.username }}</span>
          <small class="faint">{{ a.username }}{{ a.expires_at ? ` · ${dateOnly(a.expires_at)} 到期` : '' }}{{ a.password || a.password_configured ? '' : ' · 未保存密码' }}</small>
        </div>
        <UiButton variant="ghost" size="sm" icon aria-label="编辑" @click="open(a)"><Pencil /></UiButton>
        <UiButton variant="ghost" size="sm" icon aria-label="删除" @click="remove(a)"><Trash2 /></UiButton>
      </li>
    </ul>
    <p v-else-if="editing === null" class="faint none">还没有保存源站账号。分享账号需要用它代为登录。</p>

    <div v-if="editing !== null" class="draft">
      <div class="grid">
        <UiField label="用户名" for="acc-user"><input id="acc-user" v-model="draft.username" class="input" autocomplete="off" autocapitalize="none" /></UiField>
        <UiField label="密码" for="acc-pass"><input id="acc-pass" v-model="draft.password" class="input" type="password" autocomplete="new-password" :placeholder="editing !== -1 ? '不修改请留空' : ''" /></UiField>
        <UiField label="显示名称" for="acc-name" help="留空则用用户名"><input id="acc-name" v-model="draft.account_name" class="input" /></UiField>
        <UiField label="安全密码" for="acc-sec" help="部分服务需要"><input id="acc-sec" v-model="draft.security_password" class="input" type="password" autocomplete="new-password" /></UiField>
        <UiField label="到期日期" for="acc-exp"><input id="acc-exp" v-model="draft.expires_at" class="input" type="date" /></UiField>
        <UiField label="提前提醒" for="acc-rem" help="天；留空不提醒"><input id="acc-rem" v-model="draft.reminder_days" class="input num" type="number" min="0" max="3650" /></UiField>
      </div>
      <UiField label="备注" for="acc-note"><input id="acc-note" v-model="draft.note" class="input" /></UiField>
      <div class="actions">
        <UiButton size="sm" @click="editing = null">取消</UiButton>
        <UiButton size="sm" variant="primary" :loading="busy" @click="save">{{ siteId ? '保存账号' : '添加' }}</UiButton>
      </div>
    </div>
    <UiButton v-else size="sm" @click="open()"><Plus />添加源站账号</UiButton>
  </div>
</template>

<style scoped>
.accounts { display: flex; flex-direction: column; gap: 10px; align-items: flex-start; }
.list { width: 100%; margin: 0; padding: 0; list-style: none; }
.list li { display: flex; align-items: center; gap: 4px; padding: 8px 0; border-bottom: 1px solid var(--border); }
.info { flex: 1; min-width: 0; display: flex; flex-direction: column; font-size: 13.5px; }
.info small { font-size: 12px; }
.none { margin: 0; font-size: 13px; }
.draft { width: 100%; display: flex; flex-direction: column; gap: 14px; padding: 14px; border: 1px solid var(--border); border-radius: var(--radius); }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.actions { display: flex; justify-content: flex-end; gap: 8px; }
@media (max-width: 520px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
