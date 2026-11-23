<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import UiButton from '@/components/ui/UiButton.vue'
import UiField from '@/components/ui/UiField.vue'
import { clientFilterApi, type ClientFilterMode } from '@/api'
import { errorMessage, toast } from '@/stores/toast'

const loaded = ref(false)
const saving = ref(false)
const blacklistPresets = ref<string[]>([])
const whitelistPresets = ref<string[]>([])

const form = reactive({
  mode: 'off' as ClientFilterMode,
  rulesText: '',
})

async function load() {
  try {
    const cfg = await clientFilterApi.get()
    form.mode = (cfg.mode === 'blacklist' || cfg.mode === 'whitelist' ? cfg.mode : 'off') as ClientFilterMode
    form.rulesText = (cfg.rules || []).join('\n')
    blacklistPresets.value = cfg.blacklist_presets || []
    whitelistPresets.value = cfg.whitelist_presets || []
    loaded.value = true
  } catch (error) {
    toast.error(errorMessage(error, '读取客户端限制设置失败'))
  }
}

onMounted(load)

function applyPresets(list: string[]) {
  const existing = form.rulesText
    .split(/[\r\n,，]+/)
    .map(v => v.trim().toLowerCase())
    .filter(Boolean)
  const merged = Array.from(new Set([...existing, ...list]))
  form.rulesText = merged.join('\n')
}

async function save() {
  const rules = form.rulesText
    .split(/[\r\n,，]+/)
    .map(v => v.trim())
    .filter(Boolean)
  if (form.mode !== 'off' && rules.length === 0) {
    toast.error(form.mode === 'whitelist' ? '白名单模式至少需要一条放行规则，否则会拦截所有客户端' : '黑名单模式请至少填写一条拦截规则')
    return
  }
  saving.value = true
  try {
    const saved = await clientFilterApi.update(form.mode, rules)
    form.mode = (saved.mode === 'blacklist' || saved.mode === 'whitelist' ? saved.mode : 'off') as ClientFilterMode
    form.rulesText = (saved.rules || []).join('\n')
    toast.success('客户端访问限制已保存')
  } catch (error) {
    toast.error(errorMessage(error, '保存失败'))
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div v-if="loaded">
    <section class="group">
      <h2>客户端访问限制</h2>
      <p class="faint p">
        根据请求的 <code>User-Agent</code>、<code>X-Emby-Client</code> 与认证头中的客户端标识过滤 Emby 代理请求，防止异常下载器或批量刮削脚本拖库。管理面板、朋友自助页与从机心跳永远不受限制。
      </p>

      <UiField label="限制模式" for="cf-mode">
        <select id="cf-mode" v-model="form.mode" class="select">
          <option value="off">不限制（默认）</option>
          <option value="blacklist">黑名单模式（拦截命中的下载器与脚本）</option>
          <option value="whitelist">白名单模式（仅放行命中的常见播放器）</option>
        </select>
      </UiField>

      <template v-if="form.mode !== 'off'">
        <div class="preset-bar">
          <span class="faint">快捷预设：</span>
          <UiButton v-if="form.mode === 'blacklist'" size="sm" @click="applyPresets(blacklistPresets)">填入常见下载器/脚本黑名单</UiButton>
          <UiButton v-if="form.mode === 'whitelist'" size="sm" @click="applyPresets(whitelistPresets)">填入主流 Emby 播放器白名单</UiButton>
          <UiButton size="sm" variant="ghost" @click="form.rulesText = ''">清空</UiButton>
        </div>

        <UiField
          :label="form.mode === 'whitelist' ? '放行关键词列表' : '拦截关键词列表'"
          for="cf-rules"
          help="每行一个关键词（不区分大小写，包含即命中）。白名单模式请确保包含你和朋友常用的播放器（如 infuse、vidhub、senplayer、hills、yamby、emby、mozilla）。"
        >
          <textarea
            id="cf-rules"
            v-model="form.rulesText"
            class="textarea mono"
            rows="8"
            spellcheck="false"
            :placeholder="form.mode === 'whitelist' ? 'infuse\nvidhub\nsenplayer\nhills\nemby' : 'curl\nwget\npython\naria2\nyt-dlp'"
          />
        </UiField>
      </template>

      <div class="actions">
        <UiButton variant="primary" :loading="saving" @click="save">保存</UiButton>
      </div>
    </section>
  </div>
  <p v-else class="faint">正在读取…</p>
</template>

<style scoped>
.group { display: flex; flex-direction: column; gap: 18px; }
h2 { margin: 0; font-size: 15px; font-weight: 600; }
.p { margin: -8px 0 0; font-size: 13px; line-height: 1.6; }
.p code { font-family: var(--mono); font-size: 12px; padding: 1px 4px; border-radius: 4px; background: var(--surface); }
.preset-bar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; font-size: 12.5px; }
.textarea.mono { font-family: var(--mono); font-size: 12.5px; line-height: 1.55; }
.actions { display: flex; justify-content: flex-end; }
</style>
