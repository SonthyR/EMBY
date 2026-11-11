<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { Dices } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { shareApi, sitesApi, type ShareUser, type ShareUserInput, type Site, type SiteAccount } from '@/api'
import { GB, dateOnly } from '@/utils/format'
import { errorMessage, toast } from '@/stores/toast'

const props = defineProps<{ open: boolean; user: ShareUser | null; sites: Site[] }>()
const emit = defineEmits<{ close: []; saved: [user: ShareUser, created: boolean] }>()

const form = reactive({
  username: '',
  password: '',
  note: '',
  enabled: true,
  expires: '',
  quotaGB: '' as string | number,
  resetDay: 0,
  maxDevices: '' as string | number,
  speed: '' as string | number,
})
const selected = reactive<Record<number, { on: boolean; account: number }>>({})
const accounts = reactive<Record<number, SiteAccount[]>>({})
const saving = ref(false)
const errors = reactive<Record<string, string>>({})
const editing = computed(() => !!props.user)

function usable(siteId: number) {
  return (accounts[siteId] || []).filter(a => a.password || a.password_configured)
}
async function loadAccounts() {
  await Promise.all(
    props.sites.map(async site => {
      if (accounts[site.id]) return
      try {
        accounts[site.id] = await sitesApi.accounts(site.id)
      } catch {
        accounts[site.id] = []
      }
    }),
  )
}

watch(
  () => props.open,
  open => {
    if (!open) return
    const user = props.user
    Object.assign(form, {
      username: user?.username || '',
      password: '',
      note: user?.note || '',
      enabled: user ? user.enabled : true,
      expires: dateOnly(user?.expires_at),
      quotaGB: user && user.traffic_quota > 0 ? +(user.traffic_quota / GB).toFixed(2) : '',
      resetDay: user?.traffic_reset_day || 0,
      maxDevices: user?.max_devices || '',
      speed: user?.speed_limit || '',
    })
    for (const key of Object.keys(errors)) delete errors[key]
    for (const site of props.sites) {
      const assigned = user?.sites.find(s => s.site_id === site.id)
      selected[site.id] = { on: !!assigned || (!user && props.sites.length === 1), account: assigned?.site_account_id || 0 }
    }
    loadAccounts()
  },
  { immediate: true },
)

function setExpiry(days: number | null) {
  if (days === null) return (form.expires = '')
  const base = form.expires && new Date(form.expires + 'T00:00:00') > new Date() ? new Date(form.expires + 'T00:00:00') : new Date()
  base.setDate(base.getDate() + days)
  form.expires = `${base.getFullYear()}-${String(base.getMonth() + 1).padStart(2, '0')}-${String(base.getDate()).padStart(2, '0')}`
}
function randomPassword() {
  const alphabet = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  form.password = Array.from(crypto.getRandomValues(new Uint32Array(10)), v => alphabet[v % alphabet.length]).join('')
}
const toInt = (value: string | number) => (String(value).trim() === '' ? 0 : Number(String(value).trim()))

function validate(): ShareUserInput | null {
  for (const key of Object.keys(errors)) delete errors[key]
  const username = form.username.trim()
  if (!username) errors.username = '请填写用户名'
  else if (/\s/.test(username) || username.length > 64) errors.username = '1–64 个字符，不能有空格'
  if (form.password && [...form.password].length < 4) errors.password = '至少 4 个字符'
  const quota = toInt(form.quotaGB)
  if (!Number.isFinite(quota) || quota < 0) errors.quota = '请输入有效的数字'
  const devices = toInt(form.maxDevices)
  if (!Number.isInteger(devices) || devices < 0 || devices > 100) errors.devices = '0–100 的整数'
  const speed = toInt(form.speed)
  if (!Number.isInteger(speed) || speed < 0 || speed > 100000) errors.speed = '0–100000 的整数'
  const sites = props.sites.filter(s => selected[s.id]?.on).map(s => ({ site_id: s.id, site_account_id: selected[s.id].account || 0 }))
  if (!sites.length) errors.sites = '至少选择一个站点'
  if (Object.keys(errors).length) return null
  return {
    username,
    password: form.password,
    note: form.note.trim(),
    enabled: form.enabled,
    expires_at: form.expires,
    traffic_quota: Math.round(quota * GB),
    max_devices: devices,
    speed_limit: speed,
    traffic_reset_day: Number(form.resetDay) || 0,
    sites,
  }
}

async function save() {
  const input = validate()
  if (!input) return
  saving.value = true
  try {
    const saved = editing.value ? await shareApi.update(props.user!.id, input) : await shareApi.create(input)
    emit('saved', saved, !editing.value)
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UiDrawer :open="open" :title="editing ? '编辑分享账号' : '新建分享账号'" :subtitle="editing ? user?.username : '朋友会用这个账号登录 Emby 客户端'" :width="520" :dismissible="!saving" @close="emit('close')">
    <form class="form" novalidate @submit.prevent="save">
      <fieldset>
        <legend>登录</legend>
        <UiField label="用户名" for="su-name" :error="errors.username">
          <input id="su-name" v-model="form.username" class="input" maxlength="64" autocapitalize="none" spellcheck="false" autocomplete="off" :autofocus="!editing" />
        </UiField>
        <UiField label="密码" for="su-pass" :error="errors.password" :help="editing ? '留空则不修改。修改后已登录的设备会掉线。' : '留空会自动生成。'">
          <div class="with-btn">
            <input id="su-pass" v-model="form.password" class="input mono" maxlength="72" autocapitalize="none" spellcheck="false" autocomplete="off" :placeholder="editing ? '不修改' : '自动生成'" />
            <UiButton icon aria-label="随机生成密码" title="随机生成" @click="randomPassword"><Dices /></UiButton>
          </div>
        </UiField>
      </fieldset>

      <fieldset>
        <legend>有效期</legend>
        <UiField label="到期日期" for="su-exp" help="留空表示永久有效。">
          <input id="su-exp" v-model="form.expires" class="input" type="date" />
          <div class="chips">
            <button type="button" @click="setExpiry(30)">+ 30 天</button>
            <button type="button" @click="setExpiry(90)">+ 90 天</button>
            <button type="button" @click="setExpiry(365)">+ 1 年</button>
            <button type="button" @click="setExpiry(null)">永久</button>
          </div>
        </UiField>
      </fieldset>

      <fieldset>
        <legend>额度与限制</legend>
        <div class="grid">
          <UiField label="流量额度" for="su-quota" :error="errors.quota" help="GB，上下行合计">
            <input id="su-quota" v-model="form.quotaGB" class="input num" type="number" min="0" step="0.01" placeholder="不限" inputmode="decimal" />
          </UiField>
          <UiField label="每月重置" for="su-reset" help="到这天清零已用流量">
            <select id="su-reset" v-model.number="form.resetDay" class="select">
              <option :value="0">不重置</option>
              <option v-for="day in 28" :key="day" :value="day">每月 {{ day }} 日</option>
            </select>
          </UiField>
          <UiField label="同时在线" for="su-dev" :error="errors.devices" help="台；超出时踢掉最久没用的">
            <input id="su-dev" v-model="form.maxDevices" class="input num" type="number" min="0" max="100" step="1" placeholder="不限" inputmode="numeric" />
          </UiField>
          <UiField label="限速" for="su-speed" :error="errors.speed" help="Mbps，所有设备合计">
            <input id="su-speed" v-model="form.speed" class="input num" type="number" min="0" step="1" placeholder="不限" inputmode="numeric" />
          </UiField>
        </div>
      </fieldset>

      <fieldset>
        <legend>可用站点</legend>
        <p v-if="!sites.length" class="faint">还没有站点。</p>
        <div class="sites">
          <div v-for="site in sites" :key="site.id" class="site">
            <label class="check">
              <input v-model="selected[site.id].on" type="checkbox" />
              <span class="site-name">{{ site.name }}</span>
              <span v-if="!site.enabled" class="faint">已停用</span>
            </label>
            <select v-if="selected[site.id]?.on" v-model.number="selected[site.id].account" class="select compact" :disabled="!usable(site.id).length" aria-label="代登用的源站账号">
              <option :value="0">自动选择源站账号</option>
              <option v-for="account in usable(site.id)" :key="account.id" :value="account.id">{{ account.account_name || account.username }}</option>
            </select>
            <p v-if="selected[site.id]?.on && accounts[site.id] && !usable(site.id).length" class="warn">这个站点还没保存带密码的源站账号，朋友会登录失败。</p>
          </div>
        </div>
        <p v-if="errors.sites" class="err">{{ errors.sites }}</p>
      </fieldset>

      <fieldset>
        <legend>其他</legend>
        <UiField label="备注" for="su-note">
          <input id="su-note" v-model="form.note" class="input" maxlength="1000" placeholder="比如朋友的名字" />
        </UiField>
        <UiSwitch v-model="form.enabled" label="启用" description="关闭后无法登录，在线设备会立即断开" />
      </fieldset>
      <button type="submit" hidden />
    </form>
    <template #footer>
      <UiButton :disabled="saving" @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="saving" @click="save">{{ editing ? '保存更改' : '创建账号' }}</UiButton>
    </template>
  </UiDrawer>
</template>

<style scoped>
.form { display: flex; flex-direction: column; gap: 4px; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 18px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
fieldset:first-of-type { padding-top: 0; }
fieldset:last-of-type { border-bottom: 0; }
legend { float: left; width: 100%; margin-bottom: 14px; padding: 0; font-size: 12px; font-weight: 500; color: var(--text-3); }
legend + * { clear: both; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }
.with-btn { display: flex; gap: 6px; }
.chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 2px; }
.chips button { height: 26px; padding: 0 10px; border: 1px solid var(--border); border-radius: 999px; background: transparent; color: var(--text-2); font-size: 12px; cursor: pointer; transition: all var(--dur) var(--ease); }
.chips button:hover { border-color: var(--border-strong); color: var(--text); }
.sites { display: flex; flex-direction: column; }
.site { display: flex; flex-direction: column; gap: 8px; padding: 10px 0; border-bottom: 1px solid var(--border); }
.site:last-child { border-bottom: 0; }
.check { display: flex; align-items: center; gap: 10px; cursor: pointer; font-size: 14px; }
.site-name { flex: 1; }
.select.compact { height: 32px; font-size: 13px; margin-left: 25px; width: calc(100% - 25px); }
.warn { margin: 0 0 0 25px; font-size: 12px; color: var(--warn); }
.err { margin: 0; font-size: 12px; color: var(--bad); }
@media (max-width: 520px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
