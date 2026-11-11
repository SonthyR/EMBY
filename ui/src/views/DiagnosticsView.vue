<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import PageHeader from '@/components/ui/PageHeader.vue'
import { sitesApi, type Site } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const sites = ref<Site[]>([])
const siteId = ref<number | null>(null)
const result = ref<any>(null)
const running = ref(false)

onMounted(async () => {
  try {
    sites.value = await sitesApi.list()
    if (sites.value.length) siteId.value = sites.value[0].id
  } catch (error) {
    toast.error(errorMessage(error))
  }
})

async function run() {
  if (!siteId.value) return
  running.value = true
  result.value = null
  try {
    result.value = await sitesApi.diag(siteId.value)
  } catch (error) {
    toast.error(errorMessage(error, '诊断失败'))
  } finally {
    running.value = false
  }
}

const upstreams = computed(() => {
  const u = result.value?.upstreams || {}
  const list: { label: string; data: any }[] = []
  if (u.primary) list.push({ label: '主回源', data: u.primary })
  else if (result.value?.health) list.push({ label: '主回源', data: { health: result.value.health, effective_url: '' } })
  if (u.playback && u.playback.show_health !== false && u.playback.health) list.push({ label: '播放回源', data: u.playback })
  ;(Array.isArray(u.failovers) ? u.failovers : []).forEach((f: any, i: number) => list.push({ label: `备用线路 ${i + 1}`, data: f }))
  return list
})
const healthTone = (h: any) => (h?.status === 'online' ? (h.warning ? 'warn' : 'good') : 'bad')
const healthText = (h: any) => (h?.status === 'online' ? '可连接' : h?.status === 'offline' ? '无法连接' : h?.status || '未知')
</script>

<template>
  <div>
    <PageHeader title="诊断" description="检查站点的回源连通性、证书和代理状态。" />

    <div class="pick">
      <select v-model.number="siteId" class="select" aria-label="选择站点">
        <option v-for="s in sites" :key="s.id" :value="s.id">{{ s.name }}</option>
      </select>
      <UiButton variant="primary" :loading="running" :disabled="!siteId" @click="run">开始诊断</UiButton>
    </div>

    <p v-if="!result && !running" class="faint hint">选择站点后点击“开始诊断”。诊断不会模拟用户登录、转码和客户端网络，最终以实际播放为准。</p>

    <template v-if="result">
      <section v-for="u in upstreams" :key="u.label" class="section">
        <h2><span class="dot" :class="healthTone(u.data.health)" />{{ u.label }} · {{ healthText(u.data.health) }}</h2>
        <dl class="kv">
          <div v-if="u.data.effective_url"><dt>地址</dt><dd class="mono">{{ u.data.effective_url }}</dd></div>
          <div v-if="u.data.health?.latency_ms !== undefined"><dt>延迟</dt><dd class="num">{{ u.data.health.latency_ms }} ms</dd></div>
          <div v-if="u.data.health?.emby_version"><dt>Emby 版本</dt><dd>{{ u.data.health.emby_version }}</dd></div>
          <div v-if="u.data.health?.probe?.http_status"><dt>HTTP 状态</dt><dd class="num">{{ u.data.health.probe.http_status }}</dd></div>
          <div v-if="u.data.health?.error"><dt>错误</dt><dd class="bad">{{ u.data.health.error }}</dd></div>
          <div v-if="u.data.health?.warning"><dt>提示</dt><dd class="warn">{{ u.data.health.warning }}</dd></div>
          <template v-if="u.data.tls?.enabled">
            <div><dt>证书</dt><dd><span class="dot" :class="u.data.tls.valid ? 'good' : 'bad'" /> {{ u.data.tls.valid ? '有效' : '无效' }}<span v-if="u.data.tls.days_left !== undefined" class="faint"> · 剩 {{ u.data.tls.days_left }} 天</span></dd></div>
            <div v-if="u.data.tls.issuer"><dt>签发者</dt><dd>{{ u.data.tls.issuer }}</dd></div>
            <div v-if="u.data.tls.error"><dt>证书错误</dt><dd class="bad">{{ u.data.tls.error }}</dd></div>
          </template>
        </dl>
      </section>

      <section v-if="result.proxy" class="section">
        <h2><span class="dot" :class="result.proxy.running ? 'good' : 'bad'" />代理 · {{ result.proxy.running ? '运行中' : '未运行' }}</h2>
        <dl class="kv">
          <div><dt>入口</dt><dd>{{ { port: '独立端口', path: '路径', host: '域名前缀', both: '域名前缀（兼容）', unset: '未配置' }[String(result.proxy.ingress_mode)] || result.proxy.ingress_mode }}</dd></div>
          <div v-if="result.proxy.listen_port"><dt>端口</dt><dd class="num">{{ result.proxy.listen_port }}<span class="faint">{{ result.proxy.port_listening === false ? ' · 未在监听' : '' }}</span></dd></div>
          <div v-if="result.proxy.path_prefix"><dt>路径</dt><dd class="mono">{{ result.proxy.path_prefix }}</dd></div>
          <div v-if="result.proxy.public_host"><dt>域名</dt><dd class="mono">{{ result.proxy.public_host }}</dd></div>
          <div><dt>累计请求</dt><dd class="num">{{ result.proxy.total_requests ?? 0 }}</dd></div>
          <div v-if="result.proxy.uptime"><dt>运行时间</dt><dd>{{ result.proxy.uptime }}</dd></div>
        </dl>
      </section>

      <section v-if="result.headers" class="section">
        <h2>客户端身份</h2>
        <dl class="kv">
          <div><dt>模式</dt><dd>{{ result.headers.passthrough ? '透传客户端的身份' : '改写为固定身份' }}</dd></div>
          <div v-if="result.headers.current_ua"><dt>User-Agent</dt><dd class="mono small">{{ result.headers.current_ua }}</dd></div>
          <div v-if="result.headers.client_field"><dt>Client</dt><dd>{{ result.headers.client_field }}</dd></div>
          <div v-if="result.headers.version_field"><dt>Version</dt><dd>{{ result.headers.version_field }}</dd></div>
          <div v-if="result.headers.profile_error"><dt>错误</dt><dd class="bad">{{ result.headers.profile_error }}</dd></div>
        </dl>
      </section>
    </template>
  </div>
</template>

<style scoped>
.pick { display: flex; gap: 8px; max-width: 480px; margin-bottom: 20px; }
.pick .select { flex: 1; }
.hint { font-size: 13.5px; max-width: 560px; }
.section { margin-bottom: 32px; max-width: 720px; }
h2 { display: flex; align-items: center; gap: 10px; margin: 0; padding-bottom: 10px; border-bottom: 1px solid var(--border); font-size: 14px; font-weight: 600; }
.kv { margin: 0; }
.kv div { display: flex; gap: 16px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 13.5px; }
.kv dt { flex: 0 0 96px; color: var(--text-3); }
.kv dd { flex: 1; margin: 0; min-width: 0; overflow-wrap: anywhere; display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.bad { color: var(--bad); }
.warn { color: var(--warn); }
.small { font-size: 12px; }
</style>
