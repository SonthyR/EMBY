<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { MessageSquareText, Plus, Zap, Pencil, Trash2, ShieldCheck, Globe } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import {
  danmakuApi,
  type DanmakuSource,
  type DanmakuTestResult,
  type EdgeNode,
} from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'

const props = withDefaults(
  defineProps<{ open: boolean; edgeNodes?: EdgeNode[] }>(),
  { edgeNodes: () => [] },
)
const emit = defineEmits<{ close: []; changed: [] }>()

const sources = ref<DanmakuSource[]>([])
const loading = ref(false)
const saving = ref(false)
const editingId = ref<number | null>(null)
const formVisible = ref(false)
const testingId = ref<number | null>(null)
const testingDraft = ref(false)
const draftTestResult = ref<DanmakuTestResult | null>(null)
const rowTestResults = ref<Record<number, DanmakuTestResult>>({})

const form = reactive({
  name: '',
  upstream_url: '',
  egress_node_id: 0,
  enabled: true,
  note: '',
})
const errors = reactive<Record<string, string>>({})

function clearErrors() {
  for (const k of Object.keys(errors)) delete errors[k]
}

function resetForm() {
  clearErrors()
  editingId.value = null
  draftTestResult.value = null
  form.name = ''
  form.upstream_url = ''
  form.egress_node_id = 0
  form.enabled = true
  form.note = ''
}

function openCreate() {
  resetForm()
  formVisible.value = true
}

function openEdit(item: DanmakuSource) {
  clearErrors()
  editingId.value = item.id
  draftTestResult.value = null
  form.name = item.name
  form.upstream_url = item.upstream_url
  form.egress_node_id = item.egress_node_id || 0
  form.enabled = item.enabled
  form.note = item.note || ''
  formVisible.value = true
}

function cancelForm() {
  formVisible.value = false
  resetForm()
}

async function load() {
  loading.value = true
  try {
    sources.value = await danmakuApi.list()
  } catch (err) {
    toast.error(errorMessage(err, '读取弹幕源失败'))
  } finally {
    loading.value = false
  }
}

watch(
  () => props.open,
  val => {
    if (val) {
      formVisible.value = false
      resetForm()
      load()
    }
  },
  { immediate: true },
)

function validateForm(): boolean {
  clearErrors()
  if (!form.name.trim()) errors.name = '请填写弹幕源名称'
  else if ([...form.name.trim()].length > 64) errors.name = '名称不能超过 64 个字符'
  const u = form.upstream_url.trim()
  if (!u) errors.upstream_url = '请填写上游弹幕 API 地址'
  else if (!/^https?:\/\/\S+/i.test(u)) errors.upstream_url = '必须以 http:// 或 https:// 开头'
  return Object.keys(errors).length === 0
}

async function testDraft() {
  clearErrors()
  const u = form.upstream_url.trim()
  if (!u || !/^https?:\/\/\S+/i.test(u)) {
    errors.upstream_url = '请先输入有效的 http:// 或 https:// 上游地址'
    return
  }
  testingDraft.value = true
  draftTestResult.value = null
  try {
    draftTestResult.value = await danmakuApi.testDraft(u, Number(form.egress_node_id) || 0)
  } catch (err) {
    draftTestResult.value = {
      ok: false,
      status_code: 0,
      latency_ms: 0,
      egress_name: '',
      error: errorMessage(err, '测试失败'),
    }
  } finally {
    testingDraft.value = false
  }
}

async function testRow(item: DanmakuSource) {
  testingId.value = item.id
  try {
    const res = await danmakuApi.test(item.id)
    rowTestResults.value = { ...rowTestResults.value, [item.id]: res }
    if (res.ok) toast.success(`${item.name} 连通正常（HTTP ${res.status_code} · ${res.latency_ms} ms）`)
    else toast.error(res.error || `${item.name} 连通失败`)
  } catch (err) {
    toast.error(errorMessage(err, '测试失败'))
  } finally {
    if (testingId.value === item.id) testingId.value = null
  }
}

async function save() {
  if (!validateForm()) return
  saving.value = true
  try {
    const payload = {
      name: form.name.trim(),
      upstream_url: form.upstream_url.trim(),
      egress_node_id: Number(form.egress_node_id) || 0,
      enabled: form.enabled,
      note: form.note.trim(),
    }
    if (editingId.value) {
      await danmakuApi.update(editingId.value, payload)
      toast.success('弹幕源已更新')
    } else {
      await danmakuApi.create(payload)
      toast.success('弹幕源已添加，可在「分享账号」中按用户灵活分配')
    }
    formVisible.value = false
    resetForm()
    await load()
    emit('changed')
  } catch (err) {
    toast.error(errorMessage(err, '保存弹幕源失败'))
  } finally {
    saving.value = false
  }
}

async function toggleEnabled(item: DanmakuSource) {
  try {
    await danmakuApi.update(item.id, {
      name: item.name,
      upstream_url: item.upstream_url,
      egress_node_id: item.egress_node_id,
      enabled: !item.enabled,
      note: item.note || '',
    })
    toast.success(item.enabled ? `已停用「${item.name}」` : `已启用「${item.name}」`)
    await load()
    emit('changed')
  } catch (err) {
    toast.error(errorMessage(err, '切换状态失败'))
  }
}

async function remove(item: DanmakuSource) {
  if (
    !(await confirm({
      title: `删除弹幕源「${item.name}」？`,
      message: '删除后，所有分享账号对应的该弹幕链接会立刻失效。此操作无法撤销。',
      confirmText: '删除',
      danger: true,
    }))
  ) {
    return
  }
  try {
    await danmakuApi.remove(item.id)
    toast.success('弹幕源已删除')
    if (editingId.value === item.id) cancelForm()
    await load()
    emit('changed')
  } catch (err) {
    toast.error(errorMessage(err, '删除失败'))
  }
}
</script>

<template>
  <UiDrawer
    :open="open"
    title="弹幕服务"
    subtitle="反代上游弹幕 API 并可绑定指定从机出口；在「分享账号」中可按用户灵活分配弹幕源（跨站通用，不限所看 Emby 服），账号到期或取消分配实时失效。"
    :width="560"
    :dismissible="!saving"
    @close="emit('close')"
  >
    <div class="dm-wrap">
      <div class="dm-bar">
        <span class="dm-count">共 {{ sources.length }} 个弹幕源</span>
        <UiButton v-if="!formVisible" variant="primary" size="sm" @click="openCreate"><Plus />添加弹幕源</UiButton>
      </div>

      <form v-if="formVisible" class="dm-form" novalidate @submit.prevent="save">
        <div class="dm-form-head">
          <h3>{{ editingId ? '编辑弹幕源' : '新增弹幕源' }}</h3>
          <UiButton size="sm" variant="ghost" :disabled="saving" @click="cancelForm">收起</UiButton>
        </div>

        <UiField label="弹幕源名称" for="dm-name" :error="errors.name" help="展示在分享用户的自助页中，如「赠送弹幕库」">
          <input id="dm-name" v-model="form.name" class="input" maxlength="64" placeholder="例如 赠送弹幕库" />
        </UiField>

        <UiField label="上游弹幕 API 地址" for="dm-url" :error="errors.upstream_url" help="支持带路径或密钥的上游地址，仅服务端保存，绝不向用户暴露">
          <input id="dm-url" v-model="form.upstream_url" class="input mono" spellcheck="false" placeholder="https://danmu.example.com 或带 token 路径" />
        </UiField>

        <UiField label="出口节点" for="dm-egress" help="若弹幕 API 限制 IP（如仅限 JPCO 访问），请绑定对应从机">
          <select id="dm-egress" v-model.number="form.egress_node_id" class="input">
            <option :value="0">主机直出（默认）</option>
            <option v-for="node in edgeNodes" :key="node.id" :value="node.id">
              {{ node.name }}{{ node.online ? '（在线）' : node.enabled ? '（离线）' : '（已停用）' }}
            </option>
          </select>
        </UiField>

        <div v-if="form.egress_node_id > 0" class="egress-tip">
          <ShieldCheck />
          <span>强绑定防泄露：请求仅经该从机加密出口转发，从机离线时自动拒绝，绝不回落主机直连。</span>
        </div>

        <UiField label="用户提示备注（可选）" for="dm-note" help="会显示在用户的弹幕卡片下方，例如填写说明">
          <input id="dm-note" v-model="form.note" class="input" maxlength="200" placeholder="例如：支持 SenPlayer / Forward / Hills / Yamby" />
        </UiField>

        <UiSwitch v-model="form.enabled" label="启用该弹幕源" description="停用后用户自助页不再展示，已有链接请求会立刻返回不可用。" />

        <div v-if="draftTestResult" class="test-banner" :class="draftTestResult.ok ? 'good' : 'bad'">
          <template v-if="draftTestResult.ok">
            连通正常 · HTTP {{ draftTestResult.status_code }} · {{ draftTestResult.latency_ms }} ms（出口：{{ draftTestResult.egress_name }}）
          </template>
          <template v-else>
            连通失败{{ draftTestResult.egress_name ? `（出口：${draftTestResult.egress_name}）` : '' }}：{{ draftTestResult.error }}
          </template>
        </div>

        <div class="dm-form-foot">
          <UiButton type="button" variant="secondary" size="sm" :loading="testingDraft" :disabled="saving" @click="testDraft">
            <Zap />测试连通性
          </UiButton>
          <div class="dm-form-right">
            <UiButton type="button" size="sm" :disabled="saving" @click="cancelForm">取消</UiButton>
            <UiButton type="submit" variant="primary" size="sm" :loading="saving">{{ editingId ? '保存修改' : '添加弹幕源' }}</UiButton>
          </div>
        </div>
      </form>

      <div v-if="loading" class="dm-empty faint">正在加载弹幕源…</div>
      <div v-else-if="!sources.length && !formVisible" class="dm-empty">
        <MessageSquareText class="empty-ico" />
        <strong>还没有配置弹幕源</strong>
        <p class="faint">添加上游弹幕 API 后，所有处于有效期内的分享账号会自动获得专属弹幕地址，支持国内直连与到期自动停用。</p>
        <UiButton variant="primary" size="sm" @click="openCreate"><Plus />添加第一个弹幕源</UiButton>
      </div>
      <ul v-else class="dm-list">
        <li v-for="item in sources" :key="item.id" class="dm-card" :class="{ off: !item.enabled }">
          <div class="dm-card-head">
            <div class="dm-title-row">
              <span class="dm-name">{{ item.name }}</span>
              <span class="dm-pill" :class="item.enabled ? 'good' : 'off'">
                <span class="dot" :class="item.enabled ? 'good' : ''" />
                {{ item.enabled ? '已启用' : '已停用' }}
              </span>
              <span class="dm-pill" :class="item.egress_node_id > 0 ? (item.egress_online ? 'accent' : 'bad') : ''">
                <Globe />
                {{ item.egress_name || (item.egress_node_id > 0 ? `节点 #${item.egress_node_id}` : '主机直出') }}
                <template v-if="item.egress_node_id > 0 && !item.egress_online"> · 离线</template>
              </span>
            </div>
            <div class="dm-acts">
              <UiButton
                variant="secondary"
                size="sm"
                :loading="testingId === item.id"
                title="测试出口连通性"
                @click="testRow(item)"
              >
                <Zap />测通
              </UiButton>
              <UiButton variant="ghost" size="sm" icon aria-label="编辑" title="编辑" @click="openEdit(item)"><Pencil /></UiButton>
              <UiButton variant="ghost" size="sm" icon aria-label="删除" title="删除" @click="remove(item)"><Trash2 /></UiButton>
            </div>
          </div>

          <div class="dm-url mono" :title="item.upstream_url">{{ item.upstream_url }}</div>
          <p v-if="item.note" class="dm-note">{{ item.note }}</p>

          <div class="dm-card-foot">
            <span class="dm-path mono">/dm/{{ item.id }}/&lt;用户专属凭证&gt; · 已分配 {{ item.assigned_user_count || 0 }} 人</span>
            <div class="dm-foot-right">
              <span v-if="rowTestResults[item.id]" class="dm-test-chip" :class="rowTestResults[item.id].ok ? 'good' : 'bad'">
                <template v-if="rowTestResults[item.id].ok">
                  HTTP {{ rowTestResults[item.id].status_code }} · {{ rowTestResults[item.id].latency_ms }} ms
                </template>
                <template v-else>{{ rowTestResults[item.id].error || '不可达' }}</template>
              </span>
              <button type="button" class="link-btn" @click="toggleEnabled(item)">
                {{ item.enabled ? '停用' : '启用' }}
              </button>
            </div>
          </div>
        </li>
      </ul>
    </div>
  </UiDrawer>
</template>

<style scoped>
.dm-wrap { display: flex; flex-direction: column; gap: 14px; }
.dm-bar { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.dm-count { font-size: 12.5px; font-weight: 600; color: var(--text-2); }

.dm-form {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 16px;
  border-radius: var(--radius);
  border: 1px solid var(--accent-border);
  background: var(--surface-2);
}
.dm-form-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.dm-form-head h3 { margin: 0; font-size: 14px; font-weight: 700; }
.egress-tip {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 10px 12px;
  border-radius: var(--radius-sm);
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 12px;
  line-height: 1.5;
}
.egress-tip svg { width: 15px; height: 15px; flex: 0 0 15px; margin-top: 2px; }
.test-banner {
  padding: 9px 12px;
  border-radius: var(--radius-sm);
  font-size: 12.5px;
  line-height: 1.45;
}
.test-banner.good { background: var(--good-soft); color: var(--good); }
.test-banner.bad { background: var(--bad-soft); color: var(--bad); }
.dm-form-foot { display: flex; align-items: center; justify-content: space-between; gap: 10px; padding-top: 4px; }
.dm-form-right { display: flex; align-items: center; gap: 8px; }

.dm-empty {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
  text-align: center;
  padding: 36px 20px;
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius);
}
.dm-empty .empty-ico { width: 28px; height: 28px; color: var(--text-3); }
.dm-empty strong { font-size: 14px; }
.dm-empty p { margin: 0 0 6px; font-size: 12.5px; max-width: 380px; line-height: 1.55; }

.dm-list { margin: 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 10px; }
.dm-card {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 14px 15px;
  border-radius: var(--radius);
  border: 1px solid var(--border);
  background: var(--surface-2);
}
.dm-card.off { opacity: 0.68; }
.dm-card-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
.dm-title-row { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; min-width: 0; }
.dm-name { font-size: 14px; font-weight: 700; color: var(--text); }
.dm-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 21px;
  padding: 0 8px;
  border-radius: 999px;
  background: var(--surface);
  border: 1px solid var(--border);
  font-size: 11px;
  font-weight: 600;
  color: var(--text-2);
}
.dm-pill svg { width: 11px; height: 11px; }
.dm-pill.good { background: var(--good-soft); border-color: transparent; color: var(--good); }
.dm-pill.accent { background: var(--accent-soft); border-color: transparent; color: var(--accent); }
.dm-pill.bad { background: var(--bad-soft); border-color: transparent; color: var(--bad); }
.dm-acts { display: flex; align-items: center; gap: 4px; flex: 0 0 auto; }

.dm-url {
  font-size: 12px;
  color: var(--text-2);
  overflow-wrap: anywhere;
  padding: 6px 9px;
  border-radius: var(--radius-sm);
  background: var(--surface);
  border: 1px solid var(--border);
}
.dm-note { margin: 0; font-size: 12px; color: var(--text-2); }
.dm-card-foot { display: flex; align-items: center; justify-content: space-between; gap: 8px; flex-wrap: wrap; font-size: 11.5px; color: var(--text-3); }
.dm-path { font-size: 11.5px; }
.dm-foot-right { display: flex; align-items: center; gap: 10px; }
.dm-test-chip { font-weight: 600; }
.dm-test-chip.good { color: var(--good); }
.dm-test-chip.bad { color: var(--bad); }
.link-btn {
  border: 0;
  padding: 0;
  background: transparent;
  color: var(--accent);
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.link-btn:hover { text-decoration: underline; }
.mono { font-family: var(--mono); }
.faint { color: var(--text-3); }
</style>
