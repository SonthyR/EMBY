<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { RotateCcw } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { ispProbeApi, type ISPProbeSettings } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const loading = ref(false)
const saving = ref(false)
const resetting = ref(false)

const form = reactive({
  enabled: true,
  interval_sec: '30',
  recovery_interval_sec: '180',
  fail_threshold: '3',
  recover_threshold: '2',
  ct_targets: '',
  cu_targets: '',
  cm_targets: '',
  icmp_ct_targets: '',
  icmp_cu_targets: '',
  icmp_cm_targets: '',
})

const errors = reactive<Record<string, string>>({})

function clearErrors() {
  for (const k of Object.keys(errors)) delete errors[k]
}

function applySettings(cfg: ISPProbeSettings) {
  form.enabled = !!cfg.enabled
  form.interval_sec = String(cfg.interval_sec || 30)
  form.recovery_interval_sec = String(cfg.recovery_interval_sec || 180)
  form.fail_threshold = String(cfg.fail_threshold || 3)
  form.recover_threshold = String(cfg.recover_threshold || 2)
  form.ct_targets = (cfg.targets?.ct || []).join('\n')
  form.cu_targets = (cfg.targets?.cu || []).join('\n')
  form.cm_targets = (cfg.targets?.cm || []).join('\n')
  form.icmp_ct_targets = (cfg.icmp_targets?.ct || []).join('\n')
  form.icmp_cu_targets = (cfg.icmp_targets?.cu || []).join('\n')
  form.icmp_cm_targets = (cfg.icmp_targets?.cm || []).join('\n')
}

async function load() {
  loading.value = true
  clearErrors()
  try {
    const cfg = await ispProbeApi.get()
    applySettings(cfg)
  } catch (err) {
    toast.error(errorMessage(err, '读取阻断探测配置失败'))
  } finally {
    loading.value = false
  }
}

watch(
  () => props.open,
  val => {
    if (val) load()
  },
  { immediate: true },
)

function parseLines(raw: string): string[] {
  return raw
    .split(/\r?\n|,/)
    .map(v => v.trim())
    .filter(Boolean)
}

async function save() {
  clearErrors()
  const intervalSec = Number.parseInt(String(form.interval_sec ?? '').trim(), 10)
  const recoveryIntervalSec = Number.parseInt(String(form.recovery_interval_sec ?? '').trim(), 10)
  const failThreshold = Number.parseInt(String(form.fail_threshold ?? '').trim(), 10)
  const recoverThreshold = Number.parseInt(String(form.recover_threshold ?? '').trim(), 10)

  if (!Number.isInteger(intervalSec) || intervalSec < 10 || intervalSec > 600) {
    errors.interval_sec = '应为 10–600 秒'
  }
  if (!Number.isInteger(recoveryIntervalSec) || recoveryIntervalSec < 30 || recoveryIntervalSec > 3600) {
    errors.recovery_interval_sec = '应为 30–3600 秒'
  }
  if (!Number.isInteger(failThreshold) || failThreshold < 1 || failThreshold > 10) {
    errors.fail_threshold = '应为 1–10 次'
  }
  if (!Number.isInteger(recoverThreshold) || recoverThreshold < 1 || recoverThreshold > 10) {
    errors.recover_threshold = '应为 1–10 次'
  }

  const ct = parseLines(form.ct_targets)
  const cu = parseLines(form.cu_targets)
  const cm = parseLines(form.cm_targets)
  const icmpCt = parseLines(form.icmp_ct_targets)
  const icmpCu = parseLines(form.icmp_cu_targets)
  const icmpCm = parseLines(form.icmp_cm_targets)
  const hasAnyIcmpInput = icmpCt.length > 0 || icmpCu.length > 0 || icmpCm.length > 0
  if (form.enabled) {
    if (!ct.length) errors.ct_targets = '请至少填写 1 个电信探测目标'
    if (!cu.length) errors.cu_targets = '请至少填写 1 个联通探测目标'
    if (!cm.length) errors.cm_targets = '请至少填写 1 个移动探测目标'
    if (hasAnyIcmpInput) {
      if (!icmpCt.length) errors.icmp_ct_targets = '请至少填写 1 个电信 ICMP 目标'
      if (!icmpCu.length) errors.icmp_cu_targets = '请至少填写 1 个联通 ICMP 目标'
      if (!icmpCm.length) errors.icmp_cm_targets = '请至少填写 1 个移动 ICMP 目标'
    }
  }
  if (Object.keys(errors).length) return

  saving.value = true
  try {
    const payload: Partial<ISPProbeSettings> = {
      enabled: form.enabled,
      interval_sec: intervalSec,
      recovery_interval_sec: recoveryIntervalSec,
      fail_threshold: failThreshold,
      recover_threshold: recoverThreshold,
      targets: { ct, cu, cm },
    }
    if (hasAnyIcmpInput) {
      payload.icmp_targets = { ct: icmpCt, cu: icmpCu, cm: icmpCm }
    }
    const saved = await ispProbeApi.update(payload)
    applySettings(saved)
    toast.success('阻断探测与自动熔断配置已保存')
    emit('saved')
    emit('close')
  } catch (err) {
    toast.error(errorMessage(err, '保存配置失败'))
  } finally {
    saving.value = false
  }
}

async function resetDefaults() {
  resetting.value = true
  clearErrors()
  try {
    const saved = await ispProbeApi.update({ reset_defaults: true, enabled: form.enabled })
    applySettings(saved)
    toast.success('已恢复北上广默认探测目标')
    emit('saved')
  } catch (err) {
    toast.error(errorMessage(err, '恢复默认失败'))
  } finally {
    resetting.value = false
  }
}
</script>

<template>
  <UiDrawer
    :open="open"
    title="三网阻断探测与自动熔断"
    subtitle="通过北京、上海、广东三地 TCP 握手主动检测节点是否被阻断；同网全部目标连续失败即自动切流并推 TG 告警，解封后自动复活。"
    :width="540"
    :dismissible="!saving && !resetting"
    @close="emit('close')"
  >
    <div v-if="loading" class="loading-box faint">正在加载配置…</div>
    <form v-else class="form" novalidate @submit.prevent="save">
      <fieldset>
        <UiSwitch
          v-model="form.enabled"
          label="启用三网主动 TCP 阻断探测与自动熔断"
          description="从机升级至 v1.5.4 后自动运行；关闭后会立即解除所有节点的熔断状态。"
        />
      </fieldset>

      <fieldset>
        <legend>判定与恢复策略</legend>
        <div class="grid">
          <UiField label="正常探测间隔（秒）" for="ip-iv" :error="errors.interval_sec" help="10–600，默认 30 秒">
            <input id="ip-iv" v-model="form.interval_sec" class="input num" type="number" min="10" max="600" inputmode="numeric" />
          </UiField>
          <UiField label="熔断低频复测间隔（秒）" for="ip-rec-iv" :error="errors.recovery_interval_sec" help="30–3600，默认 180 秒（3 分钟）">
            <input id="ip-rec-iv" v-model="form.recovery_interval_sec" class="input num" type="number" min="30" max="3600" inputmode="numeric" />
          </UiField>
          <UiField label="连续失败熔断次数" for="ip-fail" :error="errors.fail_threshold" help="1–10，默认 3 次（三地全不通算 1 次）">
            <input id="ip-fail" v-model="form.fail_threshold" class="input num" type="number" min="1" max="10" inputmode="numeric" />
          </UiField>
          <UiField label="连续成功恢复次数" for="ip-rec" :error="errors.recover_threshold" help="1–10，默认 2 次后自动重新参与分流">
            <input id="ip-rec" v-model="form.recover_threshold" class="input num" type="number" min="1" max="10" inputmode="numeric" />
          </UiField>
        </div>
      </fieldset>

      <fieldset>
        <div class="legend-row">
          <legend>三网 TCP 握手目标（每行一个 域名或IP:端口）</legend>
          <UiButton size="sm" variant="ghost" :loading="resetting" @click="resetDefaults">
            <RotateCcw />恢复北上广默认
          </UiButton>
        </div>
        <UiField label="电信目标" for="ip-ct" :error="errors.ct_targets">
          <textarea id="ip-ct" v-model="form.ct_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
        <UiField label="联通目标" for="ip-cu" :error="errors.cu_targets">
          <textarea id="ip-cu" v-model="form.cu_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
        <UiField label="移动目标" for="ip-cm" :error="errors.cm_targets">
          <textarea id="ip-cm" v-model="form.cm_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
      </fieldset>

      <fieldset>
        <legend>三网 ICMP Ping 目标（设为 ICMP 模式的节点使用，每行一个 IP 或域名）</legend>
        <UiField label="电信 ICMP 目标" for="ip-icmp-ct" :error="errors.icmp_ct_targets">
          <textarea id="ip-icmp-ct" v-model="form.icmp_ct_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
        <UiField label="联通 ICMP 目标" for="ip-icmp-cu" :error="errors.icmp_cu_targets">
          <textarea id="ip-icmp-cu" v-model="form.icmp_cu_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
        <UiField label="移动 ICMP 目标" for="ip-icmp-cm" :error="errors.icmp_cm_targets">
          <textarea id="ip-icmp-cm" v-model="form.icmp_cm_targets" class="textarea mono" rows="3" spellcheck="false" />
        </UiField>
      </fieldset>

      <button type="submit" hidden />
    </form>

    <template #footer>
      <UiButton :disabled="saving || resetting" @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="saving" :disabled="loading || resetting" @click="save">保存配置</UiButton>
    </template>
  </UiDrawer>
</template>

<style scoped>
.loading-box { padding: 32px 0; text-align: center; font-size: 13px; }
.form { display: flex; flex-direction: column; gap: 4px; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 16px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
fieldset:first-of-type { padding-top: 0; }
fieldset:last-of-type { border-bottom: 0; }
legend { float: left; width: 100%; margin-bottom: 10px; padding: 0; font-size: 12.5px; font-weight: 600; color: var(--text-2); }
legend + * { clear: both; }
.legend-row { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.legend-row legend { float: none; width: auto; margin-bottom: 0; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.textarea {
  width: 100%;
  padding: 9px 11px;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--surface-2);
  color: var(--text);
  font-size: 12.5px;
  line-height: 1.5;
  resize: vertical;
  outline: none;
  box-sizing: border-box;
}
.textarea:focus { background: var(--surface); border-color: var(--accent); box-shadow: 0 0 0 3px var(--accent-soft); }
.mono { font-family: var(--mono); }
.faint { color: var(--text-3); }
@media (max-width: 520px) { .grid { grid-template-columns: 1fr; } }
</style>
