<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { Dices } from '@lucide/vue'
import UiDialog from '@/components/ui/UiDialog.vue'
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
  if (days === null) {
    form.expires = ''
    return
  }
  const base = form.expires && new Date(form.expires + 'T00:00:00') > new Date() ? new Date(form.expires + 'T00:00:00') : new Date()
  base.setDate(base.getDate() + days)
  form.expires = `${base.getFullYear()}-${String(base.getMonth() + 1).padStart(2, '0')}-${String(base.getDate()).padStart(2, '0')}`
}

function randomPassword() {
  const alphabet = 'abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  const values = crypto.getRandomValues(new Uint32Array(10))
  form.password = Array.from(values, v => alphabet[v % alphabet.length]).join('')
}

function toInt(value: string | number) {
  const text = String(value).trim()
  return text === '' ? 0 : Number(text)
}

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
  <UiDialog :open="open" :title="editing ? '编辑分享账号' : '新建分享账号'" :width="620" :dismissible="!saving" @close="emit('close')">
    <form class="form" novalidate @submit.prevent="save">
      <section>
        <h3>登录信息</h3>
        <div class="grid">
          <UiField label="用户名" for="su-name" :error="errors.username" help="朋友在 Emby 客户端里填的用户名。">
            <input id="su-name" v-model="form.username" class="input" maxlength="64" autocapitalize="none" spellcheck="false" autocomplete="off" :autofocus="!editing" />
          </UiField>
          <UiField label="密码" for="su-pass" :error="errors.password" :help="editing ? '不修改请留空；修改后已登录设备会掉线。' : '留空自动生成。'">
            <div class="with-btn">
              <input id="su-pass" v-model="form.password" class="input mono" maxlength="72" autocapitalize="none" spellcheck="false" autocomplete="off" :placeholder="editing ? '不修改' : '自动生成'" />
              <UiButton size="sm" variant="ghost" aria-label="随机生成密码" title="随机生成" @click="randomPassword"><Dices /></UiButton>
            </div>
          </UiField>
        </div>
      </section>

      <section>
        <h3>有效期与流量</h3>
        <UiField label="到期日期" for="su-exp" help="留空表示永久有效。">
          <div class="expiry">
            <input id="su-exp" v-model="form.expires" class="input" type="date" />
            <div class="chips">
              <button type="button" @click="setExpiry(30)">+30 天</button>
              <button type="button" @click="setExpiry(90)">+90 天</button>
              <button type="button" @click="setExpiry(365)">+1 年</button>
              <button type="button" @click="setExpiry(null)">永久</button>
            </div>
          </div>
        </UiField>
        <div class="grid">
          <UiField label="流量额度（GB）" for="su-quota" :error="errors.quota" help="上下行合计，留空不限。">
            <input id="su-quota" v-model="form.quotaGB" class="input" type="number" min="0" step="0.01" placeholder="不限" inputmode="decimal" />
          </UiField>
          <UiField label="每月自动重置" for="su-reset" help="到这天自动把已用流量清零。">
            <select id="su-reset" v-model.number="form.resetDay" class="select">
              <option :value="0">不自动重置</option>
              <option v-for="day in 28" :key="day" :value="day">每月 {{ day }} 日</option>
            </select>
          </UiField>
        </div>
      </section>

      <section>
        <h3>使用限制</h3>
        <div class="grid">
          <UiField label="同时在线设备" for="su-dev" :error="errors.devices" help="超出时踢掉最久没用的设备。">
            <input id="su-dev" v-model="form.maxDevices" class="input" type="number" min="0" max="100" step="1" placeholder="不限" inputmode="numeric" />
          </UiField>
          <UiField label="限速（Mbps）" for="su-speed" :error="errors.speed" help="所有设备合计。1080p 约 10–20，4K 约 40–80。">
            <input id="su-speed" v-model="form.speed" class="input" type="number" min="0" step="1" placeholder="不限" inputmode="numeric" />
          </UiField>
        </div>
      </section>

      <section>
        <h3>可用站点</h3>
        <p v-if="!sites.length" class="faint">还没有站点，请先在“站点”里添加。</p>
        <div class="sites">
          <div v-for="site in sites" :key="site.id" class="site" :class="{ on: selected[site.id]?.on }">
            <label class="site-check">
              <input v-model="selected[site.id].on" type="checkbox" />
              <span>{{ site.name }}</span>
              <small v-if="!site.enabled" class="faint">已停用</small>
            </label>
            <select v-if="selected[site.id]?.on" v-model.number="selected[site.id].account" class="select" :disabled="!usable(site.id).length" aria-label="代登用的源站账号">
              <option :value="0">自动选择源站账号</option>
              <option v-for="account in usable(site.id)" :key="account.id" :value="account.id">{{ account.account_name || account.username }}</option>
            </select>
            <p v-if="selected[site.id]?.on && accounts[site.id] && !usable(site.id).length" class="warn">这个站点还没保存带密码的源站账号，朋友会登录失败。</p>
          </div>
        </div>
        <p v-if="errors.sites" class="err">{{ errors.sites }}</p>
      </section>

      <section class="last">
        <UiField label="备注" for="su-note">
          <input id="su-note" v-model="form.note" class="input" maxlength="1000" placeholder="比如朋友的名字" />
        </UiField>
        <UiSwitch v-model="form.enabled" label="启用这个账号" />
      </section>
      <button type="submit" hidden />
    </form>
    <template #footer>
      <UiButton :disabled="saving" @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="saving" @click="save">{{ editing ? '保存' : '创建' }}</UiButton>
    </template>
  </UiDialog>
</template>

<style scoped>
.form { display: flex; flex-direction: column; gap: 22px; }
section { display: flex; flex-direction: column; gap: 12px; }
h3 { margin: 0; font-size: 13px; font-weight: 600; color: var(--text-3); letter-spacing: 0.04em; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.with-btn { display: flex; gap: 6px; }
.expiry { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.expiry .input { width: 180px; flex: 0 0 auto; }
.chips { display: flex; flex-wrap: wrap; gap: 6px; }
.chips button {
  padding: 6px 10px;
  border: 1px solid var(--border);
  border-radius: 999px;
  background: var(--surface-2);
  font-size: 12px;
  cursor: pointer;
  transition: all var(--dur) var(--ease);
}
.chips button:hover { border-color: var(--accent); color: var(--accent); }
.sites { display: flex; flex-direction: column; gap: 8px; }
.site { display: flex; flex-direction: column; gap: 8px; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius); transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease); }
.site.on { border-color: color-mix(in srgb, var(--accent) 50%, var(--border)); background: var(--accent-soft); }
.site-check { display: flex; align-items: center; gap: 10px; cursor: pointer; }
.warn { margin: 0; font-size: 12px; color: var(--warn); }
.err { margin: 0; font-size: 12px; color: var(--bad); }
.last { gap: 16px; }
@media (max-width: 640px) {
  .grid { grid-template-columns: minmax(0, 1fr); }
  .expiry .input { width: 100%; }
}
</style>
