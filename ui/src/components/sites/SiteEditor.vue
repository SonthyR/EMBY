<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Plus, Trash2, Zap, ChevronDown, ClipboardPaste } from '@lucide/vue'
import UiDrawer from '@/components/ui/UiDrawer.vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import UiSwitch from '@/components/ui/UiSwitch.vue'
import SiteAccounts from './SiteAccounts.vue'
import { sitesApi, type IngressCapabilities, type Site, type SiteAccount } from '@/api'
import { errorMessage, toast } from '@/stores/toast'
import { normalizedIngressMode } from './siteLogic'

const props = defineProps<{ open: boolean; site: Site | null; capabilities: IngressCapabilities }>()
const emit = defineEmits<{ close: []; saved: [] }>()

type Line = { name: string; url: string; enabled: boolean; test?: string }
type Header = { name: string; value: string; configured: boolean }
interface EditorState {
  name: string; target: string; lines: Line[]; ingress: string; pathPrefix: string; routePrefix: string; publicHost: string; port: string | number
  routing: string; profile: string; rules: { type: string; value: string }[]; playbackURL: string; streamHosts: string[]
  ua: string; customUA: string; customClient: string; customVersion: string; clientIP: string; headers: Header[]
  cache: boolean; cacheHours: string | number; cacheMB: string | number; cacheRules: string; quotaGB: string | number; speed: string | number
  extremeAck: boolean; extremeName: string
}
const cfg = ref<EditorState>({
  name: '',
  target: '',
  lines: [] as Line[],
  ingress: 'path',
  pathPrefix: '',
  routePrefix: '',
  publicHost: '',
  port: '' as string | number,
  routing: 'auto',
  profile: 'compatible',
  rules: [] as { type: string; value: string }[],
  playbackURL: '',
  streamHosts: [] as string[],
  ua: 'passthrough',
  customUA: '',
  customClient: '',
  customVersion: '',
  clientIP: 'both',
  headers: [] as Header[],
  cache: false,
  cacheHours: 24 as string | number,
  cacheMB: 512 as string | number,
  cacheRules: '',
  quotaGB: '' as string | number,
  speed: '' as string | number,
  extremeAck: false,
  extremeName: '',
})
const accounts = ref<SiteAccount[]>([])
const dynamicAvailable = ref(true)
const initialProfile = ref('')
const saving = ref(false)
const testing = ref('')
const primaryTest = ref('')
const showAdvanced = ref(false)
const pasteText = ref('')
const pasteOpen = ref(false)
const editing = computed(() => !!props.site)
const caps = computed(() => props.capabilities as Record<string, any>)
const routeDomain = computed(() => String(caps.value.route_domain || ''))
const hostAvailable = computed(() => caps.value.host_only_available !== false && caps.value.domain_prefix_available === true)

watch(
  () => props.open,
  async open => {
    if (!open) return
    const s = (props.site || {}) as Record<string, any>
    const mode = props.site ? normalizedIngressMode(props.site) : 'path'
    const domain = routeDomain.value
    const host = String(s.public_host || '')
    Object.assign(cfg.value, {
      name: s.name || '',
      target: s.target_url || '',
      lines: (Array.isArray(s.failover_lines) ? s.failover_lines : []).map((l: any) => ({ name: l.name || '', url: l.url || '', enabled: l.enabled !== false })),
      ingress: mode === 'unset' || mode === 'both' ? (mode === 'both' ? 'host' : 'path') : mode,
      pathPrefix: s.path_prefix || '',
      routePrefix: domain && host.endsWith('.' + domain) ? host.slice(0, -(domain.length + 1)) : '',
      publicHost: host,
      port: s.listen_port || '',
      routing: s.main_video_stream_mode === 'direct' ? 'direct' : props.site ? (s.dynamic_discovery_enabled ? 'auto' : 'manual') : 'auto',
      profile: ['safe', 'compatible', 'extreme'].includes(s.dynamic_profile) ? s.dynamic_profile : 'compatible',
      rules: Array.isArray(s.dynamic_domain_rules) ? s.dynamic_domain_rules.map((r: any) => ({ type: r.type || 'suffix', value: r.value || '' })) : [],
      playbackURL: s.playback_target_url || '',
      streamHosts: Array.isArray(s.stream_hosts) ? [...s.stream_hosts] : [],
      ua: s.ua_mode || 'passthrough',
      customUA: s.custom_user_agent || '',
      customClient: s.custom_client || '',
      customVersion: s.custom_version || '',
      clientIP: ['both', 'real_ip', 'none'].includes(s.client_ip_mode) ? s.client_ip_mode : 'both',
      headers: (Array.isArray(s.upstream_headers) ? s.upstream_headers : []).map((h: any) => ({ name: h.name || '', value: '', configured: true })),
      cache: !!s.asset_cache_enabled,
      cacheHours: s.asset_cache_ttl_sec ? Math.round(s.asset_cache_ttl_sec / 3600) : 24,
      cacheMB: s.asset_cache_max_bytes ? Math.round(s.asset_cache_max_bytes / 1048576) : 512,
      cacheRules: s.asset_cache_rules || '',
      quotaGB: s.traffic_quota ? +(s.traffic_quota / 1073741824).toFixed(2) : '',
      speed: s.speed_limit || '',
      extremeAck: false,
      extremeName: '',
    })
    initialProfile.value = s.dynamic_discovery_enabled ? s.dynamic_profile || '' : ''
    primaryTest.value = ''
    showAdvanced.value = false
    pasteOpen.value = !props.site
    pasteText.value = ''
    accounts.value = []
    if (props.site) {
      try {
        accounts.value = await sitesApi.accounts(props.site.id)
      } catch {
        accounts.value = []
      }
    }
    try {
      const profiles = await sitesApi.dynamicProfiles()
      dynamicAvailable.value = profiles?.available === true
    } catch {
      dynamicAvailable.value = false
    }
  },
)

function normalizeURL(value: string) {
  let raw = String(value || '').trim().replaceAll('：', ':')
  if (!raw || /[\\\s]/.test(raw)) return ''
  const explicit = raw.includes('://')
  try {
    const parsed = new URL(explicit ? raw : `http://${raw}`)
    if (!explicit && parsed.port === '443') parsed.protocol = 'https:'
    if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.hostname || parsed.username || parsed.password || parsed.hash) return ''
    return parsed.href
  } catch {
    return ''
  }
}

async function testURL(url: string): Promise<string> {
  const target = normalizeURL(url)
  if (!target) return '地址无效'
  try {
    const r = await sitesApi.testUpstream(target)
    const h = r?.health || r || {}
    if (h.status === 'online' || r?.ok) return `可连接${h.latency_ms !== undefined ? ` · ${h.latency_ms} ms` : ''}${h.emby_version ? ` · Emby ${h.emby_version}` : ''}`
    return `无法连接${h.error ? '：' + h.error : ''}`
  } catch (error) {
    return errorMessage(error, '无法连接')
  }
}
async function testPrimary() {
  testing.value = 'primary'
  primaryTest.value = await testURL(cfg.value.target)
  testing.value = ''
}
async function testLine(line: Line) {
  line.test = '测试中…'
  line.test = await testURL(line.url)
}

async function applyPaste() {
  if (!pasteText.value.trim()) return
  try {
    const r = await sitesApi.parseMessage(pasteText.value)
    cfg.value.name = r.name || cfg.value.name
    cfg.value.target = r.target_url || cfg.value.target
    cfg.value.lines = (r.failover_targets || []).map((url: string, i: number) => ({ name: `线路${i + 2}`, url, enabled: true }))
    if (r.path_prefix && !cfg.value.pathPrefix) cfg.value.pathPrefix = r.path_prefix
    if (Array.isArray(r.accounts)) {
      accounts.value = [...accounts.value, ...r.accounts.map((a: any, i: number) => ({ ...a, id: -1000 - i }))]
    }
    pasteOpen.value = false
    toast.success('已识别并填入')
  } catch (error) {
    toast.error(errorMessage(error, '没识别出来'))
  }
}

function fail(message: string) {
  toast.error(message)
  return null
}
function buildPayload(): Record<string, unknown> | null {
  const name = cfg.value.name.trim()
  if (!name) return fail('请填写站点名称')
  const target = normalizeURL(cfg.value.target)
  if (!target) return fail('请填写有效的回源地址')
  const lines = cfg.value.lines.map((l, i) => ({ name: l.name.trim() || `线路${i + 2}`, url: normalizeURL(l.url), enabled: l.enabled }))
  if (lines.some(l => !l.url)) return fail('有备用线路地址无效，请修改或删除')
  const port = Number(String(cfg.value.port).trim() || 0)
  const ingress: Record<string, unknown> = { ingress_mode: cfg.value.ingress, listen_port: Number.isInteger(port) ? port : 0, public_host: '', path_prefix: '' }
  if (cfg.value.ingress === 'path') {
    if (!cfg.value.pathPrefix.trim()) return fail('请填写访问路径')
    ingress.path_prefix = cfg.value.pathPrefix.trim()
  } else if (cfg.value.ingress === 'host') {
    const prefix = cfg.value.routePrefix.trim().toLowerCase()
    if (routeDomain.value && prefix) {
      ingress.route_prefix = prefix
      ingress.public_host = `${prefix}.${routeDomain.value}`
    } else if (cfg.value.publicHost.trim()) ingress.public_host = cfg.value.publicHost.trim()
    else return fail('请填写域名前缀')
  } else if (!port || port < 1 || port > 65535) return fail('请填写 1–65535 的端口')
  const dynamicOn = cfg.value.routing === 'auto' && dynamicAvailable.value
  const profile = cfg.value.routing === 'direct' ? 'compatible' : cfg.value.profile
  if (dynamicOn && profile === 'safe' && !cfg.value.rules.some(r => r.value.trim())) return fail('安全档位需要至少一条域名规则')
  if (dynamicOn && profile === 'extreme' && initialProfile.value !== 'extreme') {
    if (!cfg.value.extremeAck) return fail('启用极限档位前请勾选风险确认')
    if (cfg.value.extremeName.trim() !== name) return fail('请准确输入站点名称以确认极限档位')
  }
  if (cfg.value.ua === 'custom' && (!cfg.value.customUA.trim() || !cfg.value.customClient.trim() || !cfg.value.customVersion.trim())) return fail('请完整填写自定义 User-Agent、Client 和 Version')
  const headers = cfg.value.headers.filter(h => h.configured || h.name.trim() || h.value.trim())
  if (headers.some(h => !h.name.trim() || (!h.configured && !h.value.trim()))) return fail('请完整填写请求头名称和值')
  if (lines.some(l => l.enabled) && headers.length) return fail('备用线路不能和固定请求头同时使用')
  const quota = Number(String(cfg.value.quotaGB).trim() || 0)
  const speed = Number(String(cfg.value.speed).trim() || 0)
  if (!Number.isFinite(quota) || quota < 0 || !Number.isInteger(speed) || speed < 0) return fail('流量额度或限速无效')
  const hours = Math.min(720, Math.max(1, Number(cfg.value.cacheHours) || 24))
  const mb = Math.min(20480, Math.max(1, Number(cfg.value.cacheMB) || 512))
  return {
    name,
    target_url: target,
    primary_line_name: (props.site as any)?.primary_line_name || '主线路',
    failover_lines: lines,
    failover_targets: lines.filter(l => l.enabled).map(l => l.url),
    playback_target_url: cfg.value.routing === 'manual' ? cfg.value.playbackURL.trim() : '',
    playback_mode: 'direct',
    main_video_stream_mode: cfg.value.routing === 'direct' ? 'direct' : 'proxy',
    stream_hosts: cfg.value.routing === 'manual' ? cfg.value.streamHosts.map(s => s.trim()).filter(Boolean) : [],
    ...ingress,
    upstream_headers: headers.map(h => ({ name: h.name.trim(), value: h.value.trim() })),
    ua_mode: cfg.value.ua,
    client_ip_mode: cfg.value.clientIP,
    custom_user_agent: cfg.value.ua === 'custom' ? cfg.value.customUA.trim() : '',
    custom_client: cfg.value.ua === 'custom' ? cfg.value.customClient.trim() : '',
    custom_version: cfg.value.ua === 'custom' ? cfg.value.customVersion.trim() : '',
    dynamic_discovery_enabled: dynamicOn,
    dynamic_profile: profile,
    dynamic_domain_rules: cfg.value.rules.filter(r => r.value.trim()).map(r => ({ type: r.type, value: r.value.trim().toLowerCase() })),
    dynamic_allow_https_downgrade: profile !== 'safe',
    asset_cache_enabled: cfg.value.cache,
    asset_cache_ttl_sec: hours * 3600,
    asset_cache_max_bytes: mb * 1048576,
    asset_cache_rules: cfg.value.cacheRules.trim(),
    traffic_quota: Math.round(quota * 1073741824),
    speed_limit: speed,
  }
}

async function save() {
  const data = buildPayload()
  if (!data) return
  if (data.dynamic_profile === 'extreme' && initialProfile.value !== 'extreme' && data.dynamic_discovery_enabled) {
    if (!window.confirm('极限档位会放宽自动发现的大部分限制，可能把请求体重放到上游指定的公网地址。确定继续吗？')) return
  }
  saving.value = true
  try {
    if (props.site) {
      await sitesApi.update(props.site.id, data)
      toast.success('站点已保存')
    } else {
      const pending = accounts.value.map(a => ({
        account_name: a.account_name || '',
        username: a.username || '',
        password: a.password || '',
        security_password: a.security_password || '',
        expires_at: a.expires_at || '',
        note: a.note || '',
        reminder_days: a.reminder_days ?? null,
      }))
      await sitesApi.create({ ...data, accounts: pending })
      toast.success('站点已创建')
    }
    emit('saved')
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <UiDrawer :open="open" :title="editing ? '编辑站点' : '添加站点'" :subtitle="site?.name" :width="600" :dismissible="!saving" @close="emit('close')">
    <form class="form" novalidate @submit.prevent="save">
      <section v-if="!editing" class="paste">
        <button type="button" class="paste-toggle" @click="pasteOpen = !pasteOpen"><ClipboardPaste />粘贴开服消息自动填写<ChevronDown :class="{ flip: pasteOpen }" /></button>
        <div v-if="pasteOpen" class="paste-body">
          <textarea v-model="pasteText" class="textarea" rows="5" placeholder="把服主发的开服消息整段粘贴进来，会自动识别地址、账号和密码" />
          <div class="row-end"><UiButton size="sm" @click="applyPaste">识别并填入</UiButton></div>
        </div>
      </section>

      <fieldset>
        <legend>源站</legend>
        <UiField label="站点名称" for="se-name"><input id="se-name" v-model="cfg.name" class="input" maxlength="64" :autofocus="!editing" /></UiField>
        <UiField label="回源地址" for="se-target" :help="primaryTest || '源站的 Emby 地址，例如 https://emby.example.com:443'">
          <div class="with-btn">
            <input id="se-target" v-model="cfg.target" class="input mono" placeholder="https://" spellcheck="false" autocapitalize="none" />
            <UiButton :loading="testing === 'primary'" @click="testPrimary"><Zap />测试</UiButton>
          </div>
        </UiField>
        <div class="sub">
          <span class="label">备用线路</span>
          <div v-for="(line, i) in cfg.lines" :key="i" class="line">
            <input v-model="line.name" class="input name" :placeholder="`线路${i + 2}`" />
            <input v-model="line.url" class="input mono" placeholder="https://" spellcheck="false" autocapitalize="none" />
            <input v-model="line.enabled" type="checkbox" :aria-label="`启用线路 ${i + 2}`" title="启用" />
            <UiButton variant="ghost" size="sm" icon aria-label="测试" @click="testLine(line)"><Zap /></UiButton>
            <UiButton variant="ghost" size="sm" icon aria-label="删除" @click="cfg.lines.splice(i, 1)"><Trash2 /></UiButton>
            <small v-if="line.test" class="faint test">{{ line.test }}</small>
          </div>
          <UiButton size="sm" variant="ghost" @click="cfg.lines.push({ name: '', url: '', enabled: true })"><Plus />添加备用线路</UiButton>
        </div>
      </fieldset>

      <fieldset>
        <legend>访问入口</legend>
        <div class="seg">
          <button type="button" :class="{ on: cfg.ingress === 'path' }" @click="cfg.ingress = 'path'">路径</button>
          <button type="button" :class="{ on: cfg.ingress === 'host' }" :disabled="!hostAvailable && cfg.ingress !== 'host'" @click="cfg.ingress = 'host'">域名前缀</button>
          <button type="button" :class="{ on: cfg.ingress === 'port' }" @click="cfg.ingress = 'port'">独立端口</button>
        </div>
        <UiField v-if="cfg.ingress === 'path'" label="访问路径" for="se-path" help="复用面板的域名和端口，客户端地址要带上这段路径。">
          <input id="se-path" v-model="cfg.pathPrefix" class="input mono" placeholder="例如 cinema" />
        </UiField>
        <UiField v-else-if="cfg.ingress === 'host'" label="域名前缀" for="se-host" :help="routeDomain ? `完整域名：${cfg.routePrefix || '前缀'}.${routeDomain}` : '填写完整域名'">
          <input v-if="routeDomain" id="se-host" v-model="cfg.routePrefix" class="input mono" placeholder="例如 cinema" />
          <input v-else id="se-host" v-model="cfg.publicHost" class="input mono" placeholder="cinema.example.com" />
        </UiField>
        <UiField v-else label="监听端口" for="se-port" help="需要在防火墙和安全组里放行；走 CDN 时确认 CDN 支持这个端口。">
          <input id="se-port" v-model="cfg.port" class="input num" type="number" min="1" max="65535" />
        </UiField>
      </fieldset>

      <fieldset>
        <legend>播放</legend>
        <div class="seg">
          <button type="button" :class="{ on: cfg.routing === 'auto' }" :disabled="!dynamicAvailable" @click="cfg.routing = 'auto'">自动发现</button>
          <button type="button" :class="{ on: cfg.routing === 'manual' }" @click="cfg.routing = 'manual'">手动回源</button>
          <button type="button" :class="{ on: cfg.routing === 'direct' }" @click="cfg.routing = 'direct'">直连</button>
        </div>
        <p class="faint p">
          <template v-if="cfg.routing === 'auto'">自动识别重定向和播放地址，视频由服务器中转。适合大多数源站。</template>
          <template v-else-if="cfg.routing === 'manual'">指定固定的播放回源地址，视频由服务器中转。</template>
          <template v-else>客户端直接连源站的视频地址，不经过服务器。分享账号的流量统计和限速对视频不生效。</template>
        </p>
        <template v-if="cfg.routing === 'auto'">
          <UiField label="档位" for="se-profile">
            <select id="se-profile" v-model="cfg.profile" class="select">
              <option value="safe">安全：只允许 HTTPS:443，需要域名规则</option>
              <option value="compatible">兼容（推荐）</option>
              <option value="extreme">极限：高风险</option>
            </select>
          </UiField>
          <div v-if="cfg.profile === 'safe'" class="sub">
            <span class="label">域名规则</span>
            <div v-for="(r, i) in cfg.rules" :key="i" class="line rule">
              <select v-model="r.type" class="select"><option value="suffix">后缀</option><option value="exact">精确</option></select>
              <input v-model="r.value" class="input mono" placeholder="cdn.example.com" />
              <UiButton variant="ghost" size="sm" icon aria-label="删除" @click="cfg.rules.splice(i, 1)"><Trash2 /></UiButton>
            </div>
            <UiButton size="sm" variant="ghost" @click="cfg.rules.push({ type: 'suffix', value: '' })"><Plus />添加规则</UiButton>
          </div>
          <div v-if="cfg.profile === 'extreme' && initialProfile !== 'extreme'" class="danger-box">
            <label class="check"><input v-model="cfg.extremeAck" type="checkbox" />我了解极限档位会放宽大部分安全限制</label>
            <UiField label="输入站点名称确认" for="se-ext"><input id="se-ext" v-model="cfg.extremeName" class="input" :placeholder="cfg.name" /></UiField>
          </div>
        </template>
        <template v-if="cfg.routing === 'manual'">
          <UiField label="播放回源地址" for="se-play" help="留空则沿用回源地址。"><input id="se-play" v-model="cfg.playbackURL" class="input mono" placeholder="https://" /></UiField>
          <div class="sub">
            <span class="label">额外播放节点</span>
            <div v-for="(h, i) in cfg.streamHosts" :key="i" class="line">
              <input v-model="cfg.streamHosts[i]" class="input mono" placeholder="https://cdn.example.com:443" />
              <UiButton variant="ghost" size="sm" icon aria-label="删除" @click="cfg.streamHosts.splice(i, 1)"><Trash2 /></UiButton>
            </div>
            <UiButton size="sm" variant="ghost" @click="cfg.streamHosts.push('')"><Plus />添加节点</UiButton>
          </div>
        </template>
      </fieldset>

      <fieldset>
        <legend>限制</legend>
        <div class="grid">
          <UiField label="流量额度" for="se-quota" help="GB，留空不限"><input id="se-quota" v-model="cfg.quotaGB" class="input num" type="number" min="0" step="0.01" placeholder="不限" /></UiField>
          <UiField label="限速" for="se-speed" help="Mbps，留空不限"><input id="se-speed" v-model="cfg.speed" class="input num" type="number" min="0" placeholder="不限" /></UiField>
        </div>
      </fieldset>

      <fieldset>
        <legend>源站账号</legend>
        <SiteAccounts v-model="accounts" :site-id="site?.id" />
      </fieldset>

      <button type="button" class="adv-toggle" @click="showAdvanced = !showAdvanced">高级设置<ChevronDown :class="{ flip: showAdvanced }" /></button>
      <template v-if="showAdvanced">
        <fieldset>
          <legend>客户端身份</legend>
          <UiField label="User-Agent" for="se-ua">
            <select id="se-ua" v-model="cfg.ua" class="select">
              <option value="passthrough">透传客户端的身份（推荐）</option>
              <option value="infuse">Infuse</option>
              <option value="web">Web</option>
              <option value="client">Emby 客户端</option>
              <option value="custom">自定义</option>
            </select>
          </UiField>
          <div v-if="cfg.ua === 'custom'" class="grid three">
            <UiField label="User-Agent" for="se-cua"><input id="se-cua" v-model="cfg.customUA" class="input" /></UiField>
            <UiField label="Client" for="se-ccl"><input id="se-ccl" v-model="cfg.customClient" class="input" /></UiField>
            <UiField label="Version" for="se-cve"><input id="se-cve" v-model="cfg.customVersion" class="input" /></UiField>
          </div>
          <UiField label="客户端 IP" for="se-ip">
            <select id="se-ip" v-model="cfg.clientIP" class="select">
              <option value="both">透传 X-Real-IP 和 X-Forwarded-For（推荐）</option>
              <option value="real_ip">只保留 X-Real-IP</option>
              <option value="none">不透传</option>
            </select>
          </UiField>
        </fieldset>
        <fieldset>
          <legend>固定请求头</legend>
          <p class="faint p">每次回源都会带上，值加密保存。和备用线路不能同时使用。</p>
          <div v-for="(h, i) in cfg.headers" :key="i" class="line">
            <input v-model="h.name" class="input mono name" placeholder="名称" />
            <input v-model="h.value" class="input mono" type="password" :placeholder="h.configured ? '已配置，留空不变' : '值'" autocomplete="new-password" />
            <UiButton variant="ghost" size="sm" icon aria-label="删除" @click="cfg.headers.splice(i, 1)"><Trash2 /></UiButton>
          </div>
          <UiButton size="sm" variant="ghost" :disabled="caps.upstream_headers_available === false" @click="cfg.headers.push({ name: '', value: '', configured: false })"><Plus />添加请求头</UiButton>
        </fieldset>
        <fieldset>
          <legend>缓存</legend>
          <UiSwitch v-model="cfg.cache" label="缓存海报和静态资源" description="减少回源请求，加快封面加载。" />
          <div v-if="cfg.cache" class="grid">
            <UiField label="缓存时间" for="se-ch" help="小时"><input id="se-ch" v-model="cfg.cacheHours" class="input num" type="number" min="1" max="720" /></UiField>
            <UiField label="容量上限" for="se-cm" help="MB"><input id="se-cm" v-model="cfg.cacheMB" class="input num" type="number" min="1" max="20480" /></UiField>
          </div>
          <UiField v-if="cfg.cache" label="额外规则" for="se-cr" help="每行一个路径前缀，可留空。"><textarea id="se-cr" v-model="cfg.cacheRules" class="textarea mono" rows="3" /></UiField>
        </fieldset>
      </template>
      <button type="submit" hidden />
    </form>
    <template #footer>
      <UiButton :disabled="saving" @click="emit('close')">取消</UiButton>
      <UiButton variant="primary" :loading="saving" @click="save">{{ editing ? '保存' : '创建站点' }}</UiButton>
    </template>
  </UiDrawer>
</template>

<style scoped>
.form { display: flex; flex-direction: column; }
fieldset { display: flex; flex-direction: column; gap: 14px; margin: 0; padding: 20px 0; border: 0; border-bottom: 1px solid var(--border); min-width: 0; }
legend { float: left; width: 100%; margin-bottom: 14px; padding: 0; font-size: 12px; font-weight: 500; color: var(--text-3); }
legend + * { clear: both; }
.grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.grid.three { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.with-btn { display: flex; gap: 6px; }
.with-btn .input { flex: 1; }
.sub { display: flex; flex-direction: column; gap: 8px; align-items: flex-start; }
.sub .label { font-size: 13px; font-weight: 500; color: var(--text-2); }
.line { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; width: 100%; }
.line .input { flex: 1; min-width: 0; }
.line .name { flex: 0 0 96px; }
.line.rule .select { flex: 0 0 88px; }
.test { width: 100%; font-size: 12px; }
.seg { display: flex; gap: 2px; padding: 3px; border: 1px solid var(--border); border-radius: 9px; }
.seg button { flex: 1; height: 30px; border: 0; border-radius: 6px; background: transparent; color: var(--text-2); font-size: 13px; cursor: pointer; }
.seg button.on { background: var(--surface-hover); color: var(--text); font-weight: 500; }
.seg button:disabled { opacity: 0.4; cursor: not-allowed; }
.p { margin: -4px 0 0; font-size: 13px; }
.danger-box { display: flex; flex-direction: column; gap: 12px; padding: 12px; border: 1px solid color-mix(in srgb, var(--bad) 35%, transparent); border-radius: var(--radius); }
.check { display: flex; align-items: center; gap: 8px; font-size: 13.5px; }
.paste { padding-bottom: 16px; border-bottom: 1px solid var(--border); }
.paste-toggle, .adv-toggle { display: flex; align-items: center; gap: 8px; width: 100%; padding: 0; border: 0; background: transparent; color: var(--text-2); font-size: 13.5px; cursor: pointer; }
.adv-toggle { padding: 18px 0 2px; }
.paste-toggle svg, .adv-toggle svg { width: 15px; height: 15px; }
.paste-toggle svg:last-child, .adv-toggle svg { margin-left: auto; transition: transform var(--dur) var(--ease); }
.flip { transform: rotate(180deg); }
.paste-body { display: flex; flex-direction: column; gap: 8px; margin-top: 12px; }
.row-end { display: flex; justify-content: flex-end; }
@media (max-width: 560px) { .grid, .grid.three { grid-template-columns: minmax(0, 1fr); } .line .name { flex-basis: 100%; } }
</style>
