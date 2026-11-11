<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import { api } from '@/api'
import { logout, session } from '@/stores/session'
import { errorMessage, toast } from '@/stores/toast'

const created = ref('')
const form = reactive({ username: '', current: '', next: '', confirm: '' })
const saving = ref(false)
const error = ref('')

onMounted(async () => {
  try {
    const account = await api.get<{ username: string; created_at?: string }>('/api/account')
    form.username = account.username
    created.value = account.created_at || ''
  } catch {
    form.username = session.username
  }
})

const bytes = (v: string) => new TextEncoder().encode(v).length
const TRANSLATE: Record<string, string> = {
  'current password is incorrect': '当前密码不正确',
  'username must be 1-64 characters': '用户名需要 1–64 个字符',
  'password must contain at least 6 characters and at most 72 bytes': '新密码至少 6 个字符，且不超过 72 字节',
  'no account changes requested': '用户名和密码都没有修改',
}

async function save() {
  error.value = ''
  const username = form.username.trim()
  if (!username) return (error.value = '请填写用户名')
  if (!form.current) return (error.value = '请输入当前密码')
  if (form.next && ([...form.next].length < 6 || bytes(form.next) > 72)) return (error.value = '新密码至少 6 个字符，且不超过 72 字节')
  if (form.next !== form.confirm) return (error.value = '两次输入的新密码不一致')
  saving.value = true
  try {
    const result = await api.put<{ username: string }>('/api/account', { username, current_password: form.current, new_password: form.next })
    session.username = result.username || username
    Object.assign(form, { current: '', next: '', confirm: '' })
    toast.success('已保存')
  } catch (err) {
    const message = errorMessage(err, '保存失败')
    error.value = TRANSLATE[message] || message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page">
    <PageHeader title="账户" :description="created ? `管理员 · 创建于 ${created.slice(0, 10)}` : '管理员'" />
    <form class="form" @submit.prevent="save">
      <UiField label="用户名" for="ac-user"><input id="ac-user" v-model="form.username" class="input" maxlength="64" autocomplete="username" /></UiField>
      <UiField label="当前密码" for="ac-cur" help="修改任何内容都需要先验证当前密码。"><input id="ac-cur" v-model="form.current" class="input" type="password" autocomplete="current-password" /></UiField>
      <div class="grid">
        <UiField label="新密码" for="ac-new" help="不修改请留空。"><input id="ac-new" v-model="form.next" class="input" type="password" autocomplete="new-password" /></UiField>
        <UiField label="确认新密码" for="ac-confirm"><input id="ac-confirm" v-model="form.confirm" class="input" type="password" autocomplete="new-password" /></UiField>
      </div>
      <p v-if="error" class="err" role="alert">{{ error }}</p>
      <div class="actions">
        <UiButton type="submit" variant="primary" :loading="saving">保存</UiButton>
      </div>
    </form>
    <section class="danger">
      <div><strong>退出登录</strong><p class="faint">只退出当前设备，其他设备不受影响。</p></div>
      <UiButton @click="logout">退出</UiButton>
    </section>
  </div>
</template>

<style scoped>
.page { max-width: 560px; }
.form { display: flex; flex-direction: column; gap: 18px; padding-bottom: 32px; border-bottom: 1px solid var(--border); }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.actions { display: flex; justify-content: flex-end; }
.err { margin: 0; color: var(--bad); font-size: 13px; }
.danger { display: flex; align-items: center; justify-content: space-between; gap: 16px; padding-top: 24px; }
.danger strong { font-weight: 500; }
.danger p { margin: 2px 0 0; font-size: 13px; }
@media (max-width: 520px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
