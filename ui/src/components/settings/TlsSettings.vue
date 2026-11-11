<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref, watch } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import { ApiError, api } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

interface Check { state: string; message: string; restart_only?: boolean }
interface Validation { can_save: boolean; can_restart: boolean; checks: Check[] }

const status = ref<any>(null)
const loadError = ref('')
const ep = reactive({ domain: '', wildcard: '', port: '' as string | number })
const saved = reactive({ domain: '', wildcard: '', port: 0 })
const validation = ref<Validation | null>(null)
const validationText = ref('')
const busy = ref('')
const source = ref('acme')
const acme = reactive({ email: '', token: '', staging: false })
const paths = reactive({ cert: '', key: '' })
const files = reactive({ cert: null as File | null, key: null as File | null })
const certResult = ref('')
let checkSeq = 0
const currentOrigin = () => window.location.origin

async function load() {
  try {
    const s = await api.get<any>('/api/panel-certificate')
    status.value = s
    const port = Number(s.listen_port) || Number(s.active_listen_port) || Number(location.port) || 9090
    Object.assign(ep, { domain: s.panel_domain || '', wildcard: s.wildcard_domain || '', port })
    Object.assign(saved, { domain: s.panel_domain || '', wildcard: s.wildcard_domain || '', port: Number(s.listen_port) })
    source.value = ['acme', 'path', 'upload'].includes(s.certificate_source) ? s.certificate_source : 'acme'
    acme.email = s.acme_email || ''
    paths.cert = s.certificate_path || ''
    paths.key = s.private_key_path || ''
    await nextTick()
    check()
  } catch (error) {
    loadError.value = errorMessage(error, '读取 TLS 状态失败')
  }
}
onMounted(load)

const payload = () => ({ panel_domain: ep.domain.trim(), wildcard_domain: ep.wildcard.trim(), listen_port: Number(ep.port) })
const dirty = computed(() => ep.domain.trim().toLowerCase() !== saved.domain.toLowerCase() || ep.wildcard.trim().toLowerCase() !== saved.wildcard.toLowerCase() || Number(ep.port) !== saved.port)
watch(() => [ep.domain, ep.wildcard, ep.port], () => {
  checkSeq++
  validation.value = null
  validationText.value = '内容已修改，请重新检查并保存。'
})

async function check() {
  const seq = ++checkSeq
  validation.value = null
  validationText.value = '正在检查配置…'
  try {
    const result = await api.post<Validation>('/api/panel-settings/check', payload())
    if (seq === checkSeq) {
      validation.value = result
      validationText.value = ''
    }
  } catch (error) {
    if (seq !== checkSeq) return
    if (error instanceof ApiError && error.data?.validation) {
      validation.value = error.data.validation
      validationText.value = ''
    } else validationText.value = errorMessage(error)
  }
}
async function saveEndpoint() {
  const p = payload()
  if (!p.panel_domain) return toast.error('请填写面板域名')
  if (!Number.isInteger(p.listen_port) || p.listen_port < 1 || p.listen_port > 65535) return toast.error('端口需要在 1–65535 之间')
  busy.value = 'save'
  try {
    await api.post('/api/panel-settings', p)
    toast.success('已保存，应用前需要重启')
    await load()
  } catch (error) {
    if (error instanceof ApiError && error.data?.validation) validation.value = error.data.validation
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
  }
}

async function applyCert() {
  if (dirty.value) return toast.error('请先保存域名和端口')
  let body: Record<string, unknown>
  let path = '/api/panel-certificate/import'
  if (source.value === 'acme') {
    if (!saved.wildcard) return toast.error('申请泛域名证书前，请先填写并保存节点泛域名')
    if (!acme.email.trim()) return toast.error('请填写 ACME 邮箱')
    if (!acme.token.trim() && !status.value?.dns_api_token_configured) return toast.error('请填写 Cloudflare DNS API Token')
    body = { email: acme.email.trim(), dns_provider: 'cloudflare', dns_api_token: acme.token.trim(), staging: acme.staging }
    path = '/api/panel-certificate/issue'
  } else if (source.value === 'path') {
    if (!paths.cert.trim() || !paths.key.trim()) return toast.error('请填写证书和私钥路径')
    body = { source: 'path', certificate_path: paths.cert.trim(), private_key_path: paths.key.trim() }
  } else {
    if (!files.cert || !files.key) return toast.error('请选择证书链和私钥文件')
    if (files.cert.size > 512 * 1024 || files.key.size > 64 * 1024) return toast.error('证书不能超过 512 KB，私钥不能超过 64 KB')
    body = { source: 'upload', certificate_pem: await files.cert.text(), private_key_pem: await files.key.text() }
  }
  busy.value = 'cert'
  certResult.value = source.value === 'acme' ? '正在申请证书，DNS 验证最多需要 2 分钟，请不要重复提交…' : '正在校验并应用证书…'
  try {
    const updated = await api.post<any>(path, body)
    if (updated.test_issued) {
      certResult.value = '测试签发通过，当前证书没有改变。取消“测试签发”后可以申请正式证书。'
      return
    }
    certResult.value = updated.restart_required ? '证书已保存，重启后启用 HTTPS。' : '证书已应用。'
    toast.success(certResult.value)
    acme.token = ''
    await load()
  } catch (error) {
    certResult.value = '证书操作失败：' + errorMessage(error)
  } finally {
    busy.value = ''
  }
}

async function restart() {
  if (!window.confirm('重启会短暂中断面板和所有站点的连接，确定继续吗？')) return
  busy.value = 'restart'
  try {
    const result = await api.post<{ redirect_url: string }>('/api/system/restart', {})
    const target = new URL(result.redirect_url || '/', location.href)
    toast.info('服务正在重启，等待新地址就绪…')
    const deadline = Date.now() + 90000
    while (Date.now() < deadline) {
      try {
        const r = await fetch(new URL('/api/panel-ready', target).href, { mode: 'cors', credentials: 'omit', cache: 'no-store', redirect: 'error', signal: AbortSignal.timeout(5000) })
        if (r.ok && (await r.json()).ready === true) {
          location.replace(new URL('/', target).href)
          return
        }
      } catch {
        /* keep waiting */
      }
      await new Promise(resolve => setTimeout(resolve, 1500))
    }
    toast.error(`重启超时，请检查域名解析和端口后打开 ${target.origin}`)
  } catch (error) {
    toast.error(errorMessage(error))
  } finally {
    busy.value = ''
  }
}

const preview = computed(() => {
  const s = status.value
  if (!s) return ''
  const port = Number(ep.port)
  const domain = ep.domain.trim()
  if (!domain || !Number.isInteger(port)) return '—'
  const proto = s.external_proxy ? location.protocol : s.saved_tls_enabled ? 'https:' : 'http:'
  const publicPort = s.external_proxy ? Number(location.port || (location.protocol === 'https:' ? 443 : 80)) : port
  return `${proto}//${domain}${publicPort === (proto === 'https:' ? 443 : 80) ? '' : ':' + publicPort}`
})
const fmt = (v?: string) => (v ? new Date(v).toLocaleString('zh-CN', { hour12: false }) : '—')
const CHECK_LABEL = (c: Check) => (c.state === 'ok' ? '通过' : c.state === 'blocked' && !c.restart_only ? '阻止' : '待处理')
</script>

<template>
  <p v-if="loadError" class="faint">{{ loadError }}</p>
  <div v-else-if="status">
    <section class="group">
      <h2>面板访问地址</h2>
      <dl class="kv">
        <div><dt>当前访问</dt><dd class="mono">{{ currentOrigin() }}</dd></div>
        <div><dt>HTTPS</dt><dd><span class="dot" :class="status.restart_required ? 'warn' : status.tls_enabled ? 'good' : ''" />{{ status.external_proxy ? '由外部反向代理提供' : status.restart_required ? '等待重启应用' : status.tls_enabled ? '已启用' : '未启用' }}</dd></div>
        <template v-if="status.active_certificate && !status.external_proxy">
          <div><dt>当前证书</dt><dd>{{ (status.active_certificate.dns_names || []).join('、') || '—' }}</dd></div>
          <div><dt>到期</dt><dd :class="{ warn: !status.active_certificate.valid }">{{ fmt(status.active_certificate.expires_at) }}</dd></div>
          <div><dt>自动续期</dt><dd>{{ status.certificate_source === 'path' ? '每分钟检查服务器文件' : status.certificate_source === 'upload' ? '需要到期前重新上传' : status.auto_renew_enabled ? '已启用，到期前 30 天续签' : '未启用' }}</dd></div>
          <div v-if="status.renewal?.last_error"><dt>最近失败</dt><dd class="bad">{{ status.renewal.last_error }}</dd></div>
        </template>
      </dl>
      <div class="grid">
        <UiField label="面板域名" for="tls-domain" help="不含协议和端口"><input id="tls-domain" v-model="ep.domain" class="input mono" placeholder="panel.example.com" /></UiField>
        <UiField label="节点泛域名" for="tls-wild" help="可选；用域名前缀入口时填写"><input id="tls-wild" v-model="ep.wildcard" class="input mono" placeholder="*.example.com" /></UiField>
        <UiField label="面板监听端口" for="tls-port" :help="status.external_proxy ? '这是反向代理的回源端口，需要同步修改代理配置' : '保存后需要重启；新端口要在防火墙放行'"><input id="tls-port" v-model="ep.port" class="input num" type="number" min="1" max="65535" /></UiField>
        <UiField label="应用后的地址"><div class="mono preview">{{ preview }}</div></UiField>
      </div>
      <div v-if="validation" class="checks">
        <strong>{{ !validation.can_save ? '这个配置不能保存' : !validation.can_restart ? '可以保存，但重启条件还不满足' : '检查通过' }}</strong>
        <ul>
          <li v-for="(c, i) in validation.checks" :key="i"><span class="dot" :class="c.state === 'ok' ? 'good' : c.state === 'blocked' && !c.restart_only ? 'bad' : 'warn'" /><b>{{ CHECK_LABEL(c) }}</b>{{ c.message }}</li>
        </ul>
      </div>
      <p v-else-if="validationText" class="faint p">{{ validationText }}</p>
      <div class="actions">
        <UiButton :disabled="!!busy" @click="check">检查</UiButton>
        <UiButton variant="primary" :loading="busy === 'save'" :disabled="!!busy || (validation ? !validation.can_save : false)" @click="saveEndpoint">检查并保存</UiButton>
        <UiButton v-if="status.restart_required" variant="danger" :loading="busy === 'restart'" :disabled="!!busy || dirty || !validation?.can_restart" @click="restart">应用并重启</UiButton>
      </div>
    </section>

    <section v-if="!status.external_proxy" class="group">
      <h2>HTTPS 证书</h2>
      <p class="faint p">路径入口会复用面板证书；只有用不同子域名的域名前缀入口，才需要覆盖这些域名的证书。</p>
      <div class="seg">
        <button type="button" :class="{ on: source === 'acme' }" @click="source = 'acme'">自动申请</button>
        <button type="button" :class="{ on: source === 'path' }" @click="source = 'path'">服务器文件</button>
        <button type="button" :class="{ on: source === 'upload' }" @click="source = 'upload'">上传</button>
      </div>
      <template v-if="source === 'acme'">
        <p class="faint p">用 Cloudflare DNS 验证申请泛域名证书，自动续期，不需要开放 80 端口。</p>
        <UiField label="ACME 邮箱" for="tls-email"><input id="tls-email" v-model="acme.email" class="input" type="email" autocomplete="email" /></UiField>
        <UiField label="Cloudflare DNS API Token" for="tls-token" help="需要 Zone:Read 和 DNS:Edit 权限"><input id="tls-token" v-model="acme.token" class="input mono" type="password" autocomplete="new-password" :placeholder="status.dns_api_token_configured ? '已保存，留空继续使用' : ''" /></UiField>
        <UiSwitch v-model="acme.staging" label="测试签发" description="只验证流程，不替换当前证书。" />
      </template>
      <template v-else-if="source === 'path'">
        <UiField label="完整证书链路径" for="tls-cp"><input id="tls-cp" v-model="paths.cert" class="input mono" placeholder="/etc/letsencrypt/live/example.com/fullchain.pem" /></UiField>
        <UiField label="私钥路径" for="tls-kp"><input id="tls-kp" v-model="paths.key" class="input mono" placeholder="/etc/letsencrypt/live/example.com/privkey.pem" /></UiField>
      </template>
      <template v-else>
        <UiField label="完整证书链文件" for="tls-cf"><input id="tls-cf" class="input file" type="file" accept=".pem,.crt,.cer" @change="files.cert = ($event.target as HTMLInputElement).files?.[0] || null" /></UiField>
        <UiField label="私钥文件" for="tls-kf"><input id="tls-kf" class="input file" type="file" accept=".pem,.key" @change="files.key = ($event.target as HTMLInputElement).files?.[0] || null" /></UiField>
      </template>
      <p v-if="certResult" class="faint p">{{ certResult }}</p>
      <div class="actions">
        <UiButton variant="primary" :loading="busy === 'cert'" :disabled="!!busy || dirty || status.issuing" @click="applyCert">
          {{ source === 'acme' ? (acme.staging ? '验证测试签发' : '申请并应用') : source === 'path' ? '读取并应用' : '上传并应用' }}
        </UiButton>
      </div>
    </section>
    <p v-else class="faint p">当前 HTTPS 由 Nginx、Caddy 等外部代理提供，证书请在代理里管理。</p>
  </div>
  <p v-else class="faint">正在读取…</p>
</template>

<style scoped>
.group { display: flex; flex-direction: column; gap: 18px; padding-bottom: 32px; margin-bottom: 32px; border-bottom: 1px solid var(--border); }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.p { margin: -6px 0 0; font-size: 13px; }
.kv { margin: 0; }
.kv div { display: flex; gap: 16px; padding: 8px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.kv dt { flex: 0 0 80px; color: var(--text-3); }
.kv dd { margin: 0; display: flex; align-items: center; gap: 8px; min-width: 0; overflow-wrap: anywhere; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px 14px; }
.preview { min-height: 36px; display: flex; align-items: center; color: var(--text-2); }
.checks { padding: 12px 14px; border: 1px solid var(--border); border-radius: var(--radius); font-size: 13px; }
.checks ul { margin: 8px 0 0; padding: 0; list-style: none; display: flex; flex-direction: column; gap: 6px; }
.checks li { display: flex; align-items: baseline; gap: 8px; }
.checks b { flex: 0 0 auto; font-weight: 500; }
.actions { display: flex; justify-content: flex-end; gap: 8px; flex-wrap: wrap; }
.seg { display: flex; gap: 2px; padding: 3px; border: 1px solid var(--border); border-radius: 9px; }
.seg button { flex: 1; height: 30px; border: 0; border-radius: 6px; background: transparent; color: var(--text-2); font-size: 13px; cursor: pointer; }
.seg button.on { background: var(--surface-hover); color: var(--text); font-weight: 500; }
.input.file { height: auto; padding: 7px 10px; }
.warn { color: var(--warn); }
.bad { color: var(--bad); }
@media (max-width: 560px) { .grid { grid-template-columns: minmax(0, 1fr); } }
</style>
