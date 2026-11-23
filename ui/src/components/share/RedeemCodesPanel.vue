<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { Ban, Copy, Plus, Trash2 } from '@lucide/vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiDialog from '@/components/ui/UiDialog.vue'
import UiField from '@/components/ui/UiField.vue'
import { redeemApi, type RedeemCodeKind, type RedeemCodeStatus, type ShareRedeemCode, type Site } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'
import { GB, copyText, dateOnly, formatBytes } from '@/utils/format'

const props = defineProps<{ sites: Site[] }>()

const codes = ref<ShareRedeemCode[]>([])
const loading = ref(true)
const loadError = ref('')
const statusFilter = ref<'all' | RedeemCodeStatus>('all')

const createOpen = ref(false)
const saving = ref(false)
const createdOpen = ref(false)
const createdCodes = ref<ShareRedeemCode[]>([])

const form = reactive({
  kind: 'renew' as RedeemCodeKind,
  count: '1',
  days: '30',
  quotaGB: '',
  resetDay: 0,
  maxDevices: '',
  speed: '',
  expiresAt: '',
  note: '',
  siteIds: [] as number[],
})

const STATUS_LABEL: Record<string, string> = {
  unused: '未使用',
  used: '已使用',
  revoked: '已作废',
  expired: '已过期',
}
const STATUS_TONE: Record<string, string> = {
  unused: 'good',
  used: 'muted',
  revoked: 'bad',
  expired: 'warn',
}

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    codes.value = await redeemApi.list()
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取兑换码失败')
  } finally {
    loading.value = false
  }
}

onMounted(() => load())

const counts = computed(() => {
  const res: Record<string, number> = { all: codes.value.length, unused: 0, used: 0, revoked: 0, expired: 0 }
  for (const c of codes.value) res[c.status] = (res[c.status] || 0) + 1
  return res
})

const visible = computed(() => {
  if (statusFilter.value === 'all') return codes.value
  return codes.value.filter(c => c.status === statusFilter.value)
})

const unusedCodesText = computed(() =>
  codes.value
    .filter(c => c.status === 'unused')
    .map(c => c.code)
    .join('\n'),
)

function openCreate() {
  Object.assign(form, {
    kind: 'renew',
    count: '1',
    days: '30',
    quotaGB: '',
    resetDay: 0,
    maxDevices: '',
    speed: '',
    expiresAt: '',
    note: '',
    siteIds: props.sites.filter(s => s.enabled).map(s => s.id),
  })
  createOpen.value = true
}

function toggleSite(siteId: number) {
  const idx = form.siteIds.indexOf(siteId)
  if (idx >= 0) form.siteIds.splice(idx, 1)
  else form.siteIds.push(siteId)
}

function specsSummary(c: ShareRedeemCode): string {
  const parts: string[] = []
  if (c.days > 0) parts.push(`${c.days} 天`)
  else if (c.kind === 'register') parts.push('永久')
  if (c.traffic_quota > 0) parts.push(formatBytes(c.traffic_quota) + (c.traffic_reset_day ? ` / 每月 ${c.traffic_reset_day} 日重置` : ''))
  if (c.max_devices > 0) parts.push(`${c.max_devices} 台设备`)
  if (c.speed_limit > 0) parts.push(`${c.speed_limit} Mbps`)
  if (c.site_names?.length) parts.push(c.site_names.join('、'))
  if (c.expires_at) parts.push(`截止 ${dateOnly(c.expires_at)}`)
  if (c.note) parts.push(c.note)
  return parts.join(' · ') || '—'
}

async function generate() {
  const count = Number(form.count)
  if (!Number.isInteger(count) || count < 1 || count > 100) return toast.error('生成数量应为 1–100')
  const days = Number(form.days || 0)
  if (!Number.isInteger(days) || days < 0 || days > 3650) return toast.error('有效天数应为 0–3650')
  const quotaGB = Number(form.quotaGB || 0)
  if (!Number.isFinite(quotaGB) || quotaGB < 0) return toast.error('流量额度无效')
  const maxDevices = Number(form.maxDevices || 0)
  if (!Number.isInteger(maxDevices) || maxDevices < 0 || maxDevices > 100) return toast.error('最大设备数应为 0–100')
  const speed = Number(form.speed || 0)
  if (!Number.isInteger(speed) || speed < 0) return toast.error('限速值无效')
  if (form.kind === 'register' && form.siteIds.length === 0) return toast.error('开号兑换码至少需要选择一个站点')
  if (form.kind === 'renew' && days === 0 && quotaGB <= 0 && form.siteIds.length === 0) {
    return toast.error('续期码至少需要设置天数、流量额度或站点之一')
  }

  saving.value = true
  try {
    const list = await redeemApi.create({
      kind: form.kind,
      count,
      days,
      traffic_quota: quotaGB > 0 ? Math.round(quotaGB * GB) : 0,
      traffic_reset_day: quotaGB > 0 ? Number(form.resetDay) || 0 : 0,
      max_devices: maxDevices,
      speed_limit: speed,
      site_ids: form.kind === 'register' ? [...form.siteIds] : [],
      expires_at: form.expiresAt.trim(),
      note: form.note.trim(),
    })
    createOpen.value = false
    createdCodes.value = list
    createdOpen.value = true
    toast.success(`已生成 ${list.length} 个兑换码`)
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '生成失败'))
  } finally {
    saving.value = false
  }
}

async function copyOne(code: string) {
  if (await copyText(code)) toast.success('兑换码已复制')
}

async function copyList(list: ShareRedeemCode[]) {
  const text = list.map(c => c.code).join('\n')
  if (text && (await copyText(text))) toast.success(`已复制 ${list.length} 个兑换码`)
}

async function copyAllUnused() {
  if (!unusedCodesText.value) return toast.error('当前没有未使用的兑换码')
  if (await copyText(unusedCodesText.value)) toast.success('已复制全部未使用的兑换码')
}

async function revoke(c: ShareRedeemCode) {
  if (!(await confirm({ title: `作废兑换码 ${c.code}？`, message: '作废后将无法再被兑换。', confirmText: '作废', danger: true }))) return
  try {
    await redeemApi.revoke(c.id)
    toast.success('已作废')
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '作废失败'))
  }
}

async function remove(c: ShareRedeemCode) {
  if (!(await confirm({ title: `删除记录 ${c.code}？`, message: '将从列表中移除该兑换码记录。', confirmText: '删除', danger: true }))) return
  try {
    await redeemApi.remove(c.id)
    toast.success('已删除')
    await load(true)
  } catch (error) {
    toast.error(errorMessage(error, '删除失败'))
  }
}

const FILTERS: { key: 'all' | RedeemCodeStatus; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'unused', label: '未使用' },
  { key: 'used', label: '已使用' },
  { key: 'revoked', label: '已作废' },
  { key: 'expired', label: '已过期' },
]
</script>

<template>
  <div>
    <div class="bar-top">
      <nav class="tabs" aria-label="按状态筛选兑换码">
        <button v-for="f in FILTERS" :key="f.key" type="button" :class="{ on: statusFilter === f.key }" @click="statusFilter = f.key">
          {{ f.label }}<span v-if="counts[f.key]" class="n">{{ counts[f.key] }}</span>
        </button>
      </nav>
      <div class="bar-actions">
        <UiButton v-if="counts.unused" size="sm" @click="copyAllUnused"><Copy />复制未用卡密</UiButton>
        <UiButton size="sm" variant="primary" @click="openCreate"><Plus />批量生成</UiButton>
      </div>
    </div>

    <div v-if="loading" class="empty"><p class="faint">正在读取…</p></div>
    <div v-else-if="loadError" class="empty">
      <p>{{ loadError }}</p>
      <UiButton @click="load()">重试</UiButton>
    </div>
    <div v-else-if="!codes.length" class="empty">
      <h2>还没有兑换码</h2>
      <p>可批量生成「续期码」（朋友在自助页兑换续期）或「开号兑换码」（朋友在登录页自助开号并自动分配专属固定后缀）。</p>
      <UiButton variant="primary" @click="openCreate"><Plus />生成兑换码</UiButton>
    </div>
    <div v-else-if="!visible.length" class="empty"><p class="faint">当前筛选下没有兑换码。</p></div>
    <ul v-else class="code-list">
      <li v-for="c in visible" :key="c.id" class="code-row" :class="{ dim: c.status !== 'unused' }">
        <div class="code-main">
          <div class="code-title">
            <span class="mono code-val">{{ c.code }}</span>
            <span class="kind-pill" :class="c.kind === 'register' ? 'reg' : ''">{{ c.kind === 'register' ? '开号' : '续期' }}</span>
            <span class="st-pill" :class="STATUS_TONE[c.status]">{{ STATUS_LABEL[c.status] || c.status }}</span>
          </div>
          <div class="code-meta faint">
            <span>{{ specsSummary(c) }}</span>
            <span v-if="c.status === 'used' && c.used_by_username"> · 兑换人：{{ c.used_by_username }}</span>
          </div>
        </div>
        <div class="code-acts">
          <UiButton v-if="c.status === 'unused'" size="sm" variant="ghost" icon title="复制兑换码" aria-label="复制兑换码" @click="copyOne(c.code)"><Copy /></UiButton>
          <UiButton v-if="c.status === 'unused'" size="sm" variant="ghost" icon title="作废兑换码" aria-label="作废兑换码" @click="revoke(c)"><Ban /></UiButton>
          <UiButton size="sm" variant="ghost" icon title="删除记录" aria-label="删除记录" @click="remove(c)"><Trash2 /></UiButton>
        </div>
      </li>
    </ul>

    <UiDrawer :open="createOpen" title="批量生成兑换码" subtitle="支持朋友自助续期或自助开号" :width="500" :dismissible="!saving" @close="createOpen = false">
      <form class="form" novalidate @submit.prevent="generate">
        <fieldset>
          <legend>类型与数量</legend>
          <div class="grid">
            <UiField label="兑换码类型" for="rc-kind">
              <select id="rc-kind" v-model="form.kind" class="select">
                <option value="renew">续期码（自助页续期）</option>
                <option value="register">开号兑换码（登录页自助开号）</option>
              </select>
            </UiField>
            <UiField label="生成数量" for="rc-count" help="1–100 个">
              <input id="rc-count" v-model="form.count" class="input num" type="number" min="1" max="100" />
            </UiField>
          </div>
        </fieldset>

        <fieldset>
          <legend>权益配置</legend>
          <UiField label="有效天数" for="rc-days" :help="form.kind === 'register' ? '0 表示永久有效' : '叠加到原到期时间（从当前日期或原到期日较晚者起算）'">
            <input id="rc-days" v-model="form.days" class="input num" type="number" min="0" max="3650" />
            <div class="chips">
              <button type="button" @click="form.days = '30'">30 天</button>
              <button type="button" @click="form.days = '90'">90 天</button>
              <button type="button" @click="form.days = '365'">1 年</button>
              <button v-if="form.kind === 'register'" type="button" @click="form.days = '0'">永久</button>
            </div>
          </UiField>

          <div class="grid">
            <UiField label="流量额度（GB）" for="rc-quota" help="留空不改/不限">
              <input id="rc-quota" v-model="form.quotaGB" class="input num" type="number" min="0" step="1" placeholder="不限" />
            </UiField>
            <UiField label="每月重置日" for="rc-reset" help="设了流量额度时生效">
              <select id="rc-reset" v-model.number="form.resetDay" class="select">
                <option :value="0">不重置</option>
                <option v-for="d in 28" :key="d" :value="d">每月 {{ d }} 日</option>
              </select>
            </UiField>
            <UiField label="同时在线设备" for="rc-dev" help="留空不改/不限">
              <input id="rc-dev" v-model="form.maxDevices" class="input num" type="number" min="0" max="100" placeholder="不限" />
            </UiField>
            <UiField label="限速（Mbps）" for="rc-speed" help="留空不改/不限">
              <input id="rc-speed" v-model="form.speed" class="input num" type="number" min="0" placeholder="不限" />
            </UiField>
          </div>
        </fieldset>

        <fieldset v-if="form.kind === 'register'">
          <legend>开通站点（自助开号必选）</legend>
          <div v-if="sites.length" class="site-checks">
            <label v-for="s in sites" :key="s.id" class="check">
              <input type="checkbox" :checked="form.siteIds.includes(s.id)" @change="toggleSite(s.id)" />
              <span>{{ s.name }}</span>
              <small v-if="!s.enabled" class="faint">已停用</small>
            </label>
          </div>
          <p v-else class="faint">请先添加站点。</p>
        </fieldset>

        <fieldset>
          <legend>其他</legend>
          <div class="grid">
            <UiField label="卡密截止日期" for="rc-exp" help="可选，过期后无法兑换">
              <input id="rc-exp" v-model="form.expiresAt" class="input" type="date" />
            </UiField>
            <UiField label="备注" for="rc-note" help="可选，方便区分批次">
              <input id="rc-note" v-model="form.note" class="input" maxlength="120" placeholder="例如：月卡 / 朋友群福利" />
            </UiField>
          </div>
        </fieldset>
        <button type="submit" hidden />
      </form>
      <template #footer>
        <UiButton :disabled="saving" @click="createOpen = false">取消</UiButton>
        <UiButton variant="primary" :loading="saving" @click="generate">生成兑换码</UiButton>
      </template>
    </UiDrawer>

    <UiDialog :open="createdOpen" :title="`已生成 ${createdCodes.length} 个兑换码`" :width="520" @close="createdOpen = false">
      <p class="faint dlg-hint">发给朋友即可在登录页自助开号或在自助页兑换续期：</p>
      <pre class="code-box"><code>{{ createdCodes.map(c => c.code).join('\n') }}</code></pre>
      <template #footer>
        <UiButton @click="createdOpen = false">关闭</UiButton>
        <UiButton variant="primary" @click="copyList(createdCodes)"><Copy />复制全部</UiButton>
      </template>
    </UiDialog>
  </div>
</template>

<style scoped>
.bar-top { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-bottom: 1px solid var(--border); margin-bottom: 14px; flex-wrap: wrap; }
.tabs { display: flex; gap: 20px; overflow-x: auto; scrollbar-width: none; }
.tabs::-webkit-scrollbar { display: none; }
.tabs button { position: relative; display: inline-flex; align-items: center; gap: 6px; height: 40px; padding: 0; border: 0; background: transparent; color: var(--text-3); font-size: 13.5px; white-space: nowrap; cursor: pointer; }
.tabs button:hover { color: var(--text-2); }
.tabs button.on { color: var(--text); }
.tabs button.on::after { content: ''; position: absolute; left: 0; right: 0; bottom: -1px; height: 1.5px; background: var(--text); }
.n { font-size: 11.5px; color: var(--text-3); }
.bar-actions { display: flex; align-items: center; gap: 8px; padding-bottom: 6px; }

.code-list { margin: 0; padding: 0; list-style: none; }
.code-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--border); }
.code-row.dim { opacity: 0.6; }
.code-main { min-width: 0; display: flex; flex-direction: column; gap: 4px; }
.code-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.code-val { font-size: 14px; font-weight: 600; letter-spacing: 0.03em; }
.kind-pill, .st-pill { display: inline-flex; align-items: center; height: 20px; padding: 0 7px; border-radius: 999px; font-size: 11px; background: var(--surface); color: var(--text-2); }
.kind-pill.reg { background: var(--accent-soft); color: var(--accent); }
.st-pill.good { background: var(--good-soft); color: var(--good); }
.st-pill.warn { background: var(--warn-soft); color: var(--warn); }
.st-pill.bad { background: var(--bad-soft); color: var(--bad); }
.code-meta { font-size: 12.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.code-acts { display: flex; align-items: center; gap: 4px; flex: 0 0 auto; }

.empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 64px 20px; text-align: center; }
.empty h2 { margin: 0; font-size: 17px; font-weight: 600; }
.empty p { max-width: 420px; margin: 0 0 8px; color: var(--text-2); font-size: 13.5px; line-height: 1.6; }

.form { display: flex; flex-direction: column; gap: 4px; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 18px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
fieldset:first-of-type { padding-top: 0; }
fieldset:last-of-type { border-bottom: 0; }
legend { float: left; width: 100%; margin-bottom: 14px; padding: 0; font-size: 12px; font-weight: 500; color: var(--text-3); }
legend + * { clear: both; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }
.chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 4px; }
.chips button { height: 26px; padding: 0 10px; border: 1px solid var(--border); border-radius: 999px; background: transparent; color: var(--text-2); font-size: 12px; cursor: pointer; }
.chips button:hover { border-color: var(--border-strong); color: var(--text); }
.site-checks { display: flex; flex-direction: column; gap: 10px; }
.check { display: flex; align-items: center; gap: 8px; font-size: 13.5px; cursor: pointer; }
.dlg-hint { margin: 0 0 10px; font-size: 13px; }
.code-box { margin: 0; padding: 12px 14px; border-radius: var(--radius-sm); background: var(--surface); border: 1px solid var(--border); font-family: var(--mono); font-size: 13px; line-height: 1.65; max-height: 280px; overflow-y: auto; }
@media (max-width: 520px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
