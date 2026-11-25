<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { edgeAutoHttpsApi, type EdgeAutoHTTPSStatus } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const status = ref<EdgeAutoHTTPSStatus | null>(null)
const loadError = ref('')
const saving = ref(false)
const issuing = ref(false)
const syncing = ref(false)
const actionMsg = ref('')

const form = reactive({
  enabled: true,
  base_domain: '',
  subdomain_prefix: 'node',
  acme_email: '',
  cf_api_token: '',
})

async function load() {
  try {
    const s = await edgeAutoHttpsApi.get()
    status.value = s
    form.enabled = s.enabled || (!s.base_domain && !s.token_configured)
    form.base_domain = s.wildcard_domain || s.base_domain || ''
    form.subdomain_prefix = s.subdomain_prefix || 'node'
    form.acme_email = s.acme_email || ''
    loadError.value = ''
  } catch (error) {
    loadError.value = errorMessage(error, '读取从机自动 HTTPS 配置失败')
  }
}

onMounted(load)

const cleanBaseDomain = computed(() => {
  const raw = form.base_domain.trim().toLowerCase().replace(/^https?:\/\//, '').replace(/\/.*$/, '').replace(/^\*\./, '').replace(/\.$/, '')
  return raw
})

const previewSubdomain = computed(() => {
  const prefix = form.subdomain_prefix.trim().toLowerCase() || 'node'
  return cleanBaseDomain.value ? `${prefix}-1.${cleanBaseDomain.value}` : 'node-1.example.com'
})

const certState = computed(() => {
  const s = status.value
  if (!s) return { tone: '', text: '—' }
  if (s.issuing || issuing.value) return { tone: 'warn', text: '正在通过 Cloudflare DNS 验证申请证书…' }
  if (!s.cert_configured) return { tone: 'warn', text: '尚未申请（保存配置后点击下方「申请泛域名证书」）' }
  if (!s.cert_valid) return { tone: 'bad', text: '证书已过期，请立即更新' }
  if (!s.cert_matches_domain) return { tone: 'warn', text: '证书与当前泛域名不匹配，请重新申请' }
  return { tone: 'good', text: `有效（剩余 ${s.cert_days_remaining ?? 0} 天，到期前 30 天自动续期）` }
})

const fmt = (v?: string) => (v ? new Date(v).toLocaleString('zh-CN', { hour12: false }) : '—')

async function saveConfig() {
  if (form.enabled) {
    if (!cleanBaseDomain.value) return toast.error('请填写从机泛域名（例如 *.example.com）')
    if (!form.acme_email.trim()) return toast.error('请填写 ACME 邮箱')
    if (!form.cf_api_token.trim() && !status.value?.token_configured) return toast.error('请填写 Cloudflare DNS API Token')
  }
  saving.value = true
  actionMsg.value = ''
  try {
    const updated = await edgeAutoHttpsApi.update({
      enabled: form.enabled,
      base_domain: form.base_domain.trim(),
      subdomain_prefix: form.subdomain_prefix.trim() || 'node',
      acme_email: form.acme_email.trim(),
      cf_api_token: form.cf_api_token.trim(),
    })
    status.value = updated
    form.base_domain = updated.wildcard_domain || updated.base_domain || ''
    form.subdomain_prefix = updated.subdomain_prefix || 'node'
    form.cf_api_token = ''
    toast.success('从机自动 HTTPS 配置已保存并通过 Cloudflare 校验')
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}

async function issueCert(force = false) {
  if (!status.value?.base_domain || !status.value?.token_configured) {
    return toast.error('请先填写并保存泛域名、邮箱与 Cloudflare API Token')
  }
  issuing.value = true
  actionMsg.value = '正在通过 Cloudflare DNS-01 验证申请泛域名证书（通常需要 30–90 秒），请勿关闭页面…'
  try {
    const updated = await edgeAutoHttpsApi.issue(force)
    status.value = updated
    actionMsg.value = `泛域名证书 ${updated.wildcard_domain} 已就绪，各从机将在 5 秒内自动同步启用 HTTPS。`
    toast.success('泛域名证书已申请并生效')
  } catch (error) {
    actionMsg.value = '证书申请失败：' + errorMessage(error)
    toast.error(errorMessage(error, '申请证书失败'))
    await load()
  } finally {
    issuing.value = false
  }
}

async function syncDNS() {
  syncing.value = true
  try {
    const res = await edgeAutoHttpsApi.sync()
    status.value = res.status
    toast.success(`已同步 ${res.synced} 台从机的 Cloudflare DNS 解析`)
  } catch (error) {
    toast.error(errorMessage(error, '同步 DNS 失败'))
  } finally {
    syncing.value = false
  }
}
</script>

<template>
  <p v-if="loadError" class="faint">{{ loadError }}</p>
  <div v-else-if="status">
    <section class="group">
      <h2>从机全自动 HTTPS（Cloudflare）</h2>
      <p class="faint p">
        专为 NAT 小鸡与普通从机设计，<strong>完全独立于主机端口与 1Panel / OpenResty 外部反代</strong>，不会改动主机 HTTP 监听。
        配置一次后，添加从机只需勾选「自动配置 HTTPS」：上线自动写入 Cloudflare 仅 DNS 解析（关闭小黄云）、自动下发泛域名证书、换 IP 自动 DDNS、删除节点自动清理记录。
      </p>

      <dl class="kv">
        <div>
          <dt>功能状态</dt>
          <dd>
            <span class="dot" :class="status.enabled && status.token_configured ? 'good' : ''" />
            {{ status.enabled && status.token_configured ? `已启用（${status.wildcard_domain}）` : '未启用' }}
          </dd>
        </div>
        <div>
          <dt>泛域名证书</dt>
          <dd>
            <span class="dot" :class="certState.tone" />
            <span>{{ certState.text }}</span>
          </dd>
        </div>
        <div v-if="status.cert_expires_at">
          <dt>证书到期</dt>
          <dd class="mono">{{ fmt(status.cert_expires_at) }}</dd>
        </div>
        <div>
          <dt>已接入从机</dt>
          <dd>{{ status.synced_nodes_count }} / {{ status.auto_nodes_count }} 台已完成 Cloudflare DNS 同步</dd>
        </div>
        <div v-if="status.last_cert_error">
          <dt>最近错误</dt>
          <dd class="bad">{{ status.last_cert_error }}</dd>
        </div>
      </dl>

      <UiSwitch
        v-model="form.enabled"
        label="启用从机自动 HTTPS"
        description="开启后可在「节点 → 添加/编辑从机」中一键勾选自动配置子域名解析与 HTTPS 证书。"
      />

      <div class="grid">
        <UiField label="从机泛域名" for="eah-domain" help="填 *.example.com 或 example.com，证书将签发给该泛域名">
          <input id="eah-domain" v-model="form.base_domain" class="input mono" placeholder="*.example.com" />
        </UiField>
        <UiField label="自动子域名前缀" for="eah-prefix" :help="`默认自动分配为 ${previewSubdomain}（单个节点也可自定义）`">
          <input id="eah-prefix" v-model="form.subdomain_prefix" class="input mono" maxlength="20" placeholder="node" />
        </UiField>
      </div>

      <UiField label="ACME 邮箱" for="eah-email" help="用于 Let's Encrypt 免费证书签发与到期提醒">
        <input id="eah-email" v-model="form.acme_email" class="input" type="email" autocomplete="email" placeholder="you@example.com" />
      </UiField>

      <UiField
        label="Cloudflare DNS API Token"
        for="eah-token"
        help="在 Cloudflare 后台「API 令牌 → 创建令牌 → 编辑区域 DNS」生成，需要 Zone:Read 与 DNS:Edit 权限；加密保存于本地数据库。"
      >
        <input
          id="eah-token"
          v-model="form.cf_api_token"
          class="input mono"
          type="password"
          autocomplete="new-password"
          :placeholder="status.token_configured ? '已加密保存，留空继续使用' : '粘贴 Cloudflare API Token'"
        />
      </UiField>

      <p v-if="actionMsg" class="faint p">{{ actionMsg }}</p>

      <div class="actions">
        <UiButton v-if="status.auto_nodes_count > 0" :loading="syncing" :disabled="saving || issuing" @click="syncDNS">
          立即同步从机 DNS
        </UiButton>
        <UiButton
          :loading="issuing"
          :disabled="saving || syncing || !status.base_domain || !status.token_configured"
          @click="issueCert(status.cert_configured && status.cert_valid && status.cert_matches_domain)"
        >
          {{ status.cert_configured && status.cert_valid && status.cert_matches_domain ? '强制重新签发证书' : '一键申请泛域名证书' }}
        </UiButton>
        <UiButton variant="primary" :loading="saving" :disabled="issuing || syncing" @click="saveConfig">
          校验并保存配置
        </UiButton>
      </div>
    </section>
  </div>
  <p v-else class="faint">正在读取…</p>
</template>

<style scoped>
.group { display: flex; flex-direction: column; gap: 18px; }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.p { margin: -6px 0 0; font-size: 13px; line-height: 1.6; }
.kv { margin: 0; }
.kv div { display: flex; gap: 16px; padding: 8px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.kv dt { flex: 0 0 88px; color: var(--text-3); }
.kv dd { margin: 0; display: flex; align-items: center; gap: 8px; min-width: 0; overflow-wrap: anywhere; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }
.actions { display: flex; justify-content: flex-end; gap: 8px; flex-wrap: wrap; }
.bad { color: var(--bad); }
@media (max-width: 560px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
