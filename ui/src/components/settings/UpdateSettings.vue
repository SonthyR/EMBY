<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import { api } from '@/api'
import { confirm } from '@/stores/confirm'
import { errorMessage, toast } from '@/stores/toast'

interface Release {
  current_version: string
  latest_version: string
  release_url: string
  notes: string
  update_available: boolean
  update_supported: boolean
  unsupported_reason?: string
}
interface Job {
  state: string
  version?: string
  message: string
}

const release = ref<Release | null>(null)
const summary = ref('')
const checking = ref(false)
const running = ref(false)
let alive = true

async function poll() {
  if (running.value) return
  running.value = true
  const deadline = Date.now() + 32 * 60 * 1000
  while (alive && Date.now() < deadline) {
    try {
      const job = await api.get<Job>('/api/system/update')
      summary.value = job.message
      if (job.state !== 'running') {
        running.value = false
        if (job.state === 'completed') {
          toast.success('更新完成')
          location.reload()
        } else if (job.state === 'failed') {
          toast.error(job.message)
          release.value = null
        }
        return
      }
    } catch {
      summary.value = '服务正在重启，等待重新连接…'
    }
    await new Promise(resolve => setTimeout(resolve, 2000))
  }
  running.value = false
  if (alive) summary.value = '等待超时，请刷新页面查看服务状态。'
}

async function check() {
  checking.value = true
  release.value = null
  try {
    const r = await api.get<Release>('/api/system/update/check')
    release.value = r
    summary.value = r.update_available ? `有新版本 ${r.latest_version}，当前 ${r.current_version}。` : `已是最新版本 ${r.current_version}。`
    if (r.update_available && !r.update_supported) summary.value += ` 不能在面板里直接更新：${r.unsupported_reason || '更新服务未安装'}`
  } catch (error) {
    summary.value = '检查失败：' + errorMessage(error)
  } finally {
    checking.value = false
  }
}

async function install() {
  const r = release.value
  if (!r) return
  if (!(await confirm({ title: `更新到 ${r.latest_version}？`, message: '面板和站点会短暂重启，配置和数据都会保留；更新失败会自动回滚。', confirmText: '更新' }))) return
  try {
    await api.post('/api/system/update', { version: r.latest_version })
    await poll()
  } catch (error) {
    toast.error(errorMessage(error, '更新启动失败'))
  }
}

onMounted(async () => {
  try {
    const job = await api.get<Job>('/api/system/update')
    if (job.state === 'running') {
      summary.value = job.message
      poll()
    } else if (job.state === 'failed') {
      summary.value = job.message
    }
  } catch {
    /* updater not installed: the check explains it */
  }
})
onBeforeUnmount(() => {
  alive = false
})
</script>

<template>
  <section class="group">
    <h2>版本更新</h2>
    <p class="faint p">从 GitHub 发布页检查新版本。更新前会自动备份，失败会自动回滚。</p>
    <p v-if="summary" class="summary">{{ summary }}</p>
    <div v-if="release?.notes" class="notes">
      <pre>{{ release.notes }}</pre>
      <a :href="release.release_url" target="_blank" rel="noopener noreferrer">查看发布页</a>
    </div>
    <div class="actions">
      <UiButton :loading="checking" :disabled="running" @click="check">检查更新</UiButton>
      <UiButton v-if="release?.update_available && release.update_supported" variant="primary" :loading="running" @click="install">更新到 {{ release.latest_version }}</UiButton>
    </div>
  </section>
  <section class="group">
    <h2>在服务器上更新</h2>
    <p class="faint p">面板里不能更新时，在服务器上执行：</p>
    <code class="cmd">bash &lt;(curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh) update -y</code>
  </section>
</template>

<style scoped>
.group { display: flex; flex-direction: column; gap: 14px; padding-bottom: 32px; margin-bottom: 32px; border-bottom: 1px solid var(--border); }
.group:last-child { border-bottom: 0; }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.p { margin: -4px 0 0; font-size: 13.5px; }
.summary { margin: 0; font-size: 13.5px; }
.notes { display: flex; flex-direction: column; gap: 8px; }
.notes pre { max-height: 240px; margin: 0; padding: 12px; overflow: auto; border: 1px solid var(--border); border-radius: var(--radius-sm); font-size: 12.5px; white-space: pre-wrap; }
.notes a { align-self: flex-start; font-size: 13px; color: var(--text-2); }
.actions { display: flex; justify-content: flex-end; gap: 8px; }
.cmd { display: block; padding: 10px 12px; border: 1px solid var(--border); border-radius: var(--radius-sm); font-size: 12.5px; word-break: break-all; user-select: all; }
</style>
