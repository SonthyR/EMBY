<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { Search, CornerDownLeft } from '@lucide/vue'
import { commandOpen } from '@/stores/command'
import { cycleTheme } from '@/stores/theme'
import { logout } from '@/stores/session'

interface Command {
  id: string
  label: string
  group: string
  hint?: string
  run: () => void
}

const router = useRouter()
const query = ref('')
const active = ref(0)
const input = ref<HTMLInputElement | null>(null)

const commands = computed<Command[]>(() => [
  { id: 'go-dashboard', group: '前往', label: '仪表盘', hint: 'G D', run: () => router.push('/dashboard') },
  { id: 'go-sites', group: '前往', label: '站点', hint: 'G S', run: () => router.push('/sites') },
  { id: 'go-share', group: '前往', label: '分享账号', hint: 'G U', run: () => router.push('/share') },
  { id: 'go-logs', group: '前往', label: '日志', run: () => router.push('/logs') },
  { id: 'go-settings', group: '前往', label: '设置', run: () => router.push('/settings') },
  { id: 'new-share', group: '操作', label: '新建分享账号', hint: 'N', run: () => router.push({ path: '/share', query: { new: '1' } }) },
  { id: 'new-site', group: '操作', label: '添加站点', run: () => (location.href = '/#sites/new') },
  { id: 'theme', group: '操作', label: '切换主题', run: cycleTheme },
  { id: 'logout', group: '操作', label: '退出登录', run: logout },
])
const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  return q ? commands.value.filter(c => c.label.toLowerCase().includes(q) || c.group.includes(q)) : commands.value
})
watch(filtered, () => (active.value = 0))
watch(commandOpen, async open => {
  if (!open) return
  query.value = ''
  active.value = 0
  await nextTick()
  input.value?.focus()
})

function run(command?: Command) {
  if (!command) return
  commandOpen.value = false
  command.run()
}
function onKey(event: KeyboardEvent) {
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    active.value = (active.value + 1) % Math.max(1, filtered.value.length)
  } else if (event.key === 'ArrowUp') {
    event.preventDefault()
    active.value = (active.value - 1 + filtered.value.length) % Math.max(1, filtered.value.length)
  } else if (event.key === 'Enter') {
    event.preventDefault()
    run(filtered.value[active.value])
  } else if (event.key === 'Escape') {
    commandOpen.value = false
  }
}

let pendingG = false
function onGlobal(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    commandOpen.value = !commandOpen.value
    return
  }
  const target = event.target as HTMLElement
  if (commandOpen.value || event.metaKey || event.ctrlKey || event.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName) || target.isContentEditable) return
  if (document.querySelector('[role="dialog"]')) return
  const key = event.key.toLowerCase()
  if (pendingG) {
    pendingG = false
    const map: Record<string, string> = { d: '/dashboard', s: '/sites', u: '/share' }
    if (map[key]) router.push(map[key])
    return
  }
  if (key === 'g') {
    pendingG = true
    setTimeout(() => (pendingG = false), 900)
  } else if (key === '/') {
    const search = document.querySelector<HTMLInputElement>('[data-page-search]')
    if (search) {
      event.preventDefault()
      search.focus()
    }
  }
}
onMounted(() => document.addEventListener('keydown', onGlobal))
onBeforeUnmount(() => document.removeEventListener('keydown', onGlobal))
</script>

<template>
  <Teleport to="body">
    <Transition name="palette">
      <div v-if="commandOpen" class="layer" @mousedown.self="commandOpen = false">
        <div class="palette" role="dialog" aria-label="命令面板">
          <div class="search">
            <Search />
            <input ref="input" v-model="query" placeholder="搜索页面或操作…" @keydown="onKey" />
            <kbd>Esc</kbd>
          </div>
          <ul class="list" role="listbox">
            <li
              v-for="(c, i) in filtered"
              :key="c.id"
              role="option"
              :aria-selected="i === active"
              :class="{ active: i === active }"
              @mousemove="active = i"
              @click="run(c)"
            >
              <span class="group">{{ c.group }}</span>
              <span class="label">{{ c.label }}</span>
              <span v-if="c.hint" class="hint"><kbd v-for="k in c.hint.split(' ')" :key="k">{{ k }}</kbd></span>
              <CornerDownLeft v-if="i === active" class="enter" />
            </li>
            <li v-if="!filtered.length" class="empty">没有匹配的结果</li>
          </ul>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.layer { position: fixed; inset: 0; z-index: 150; display: flex; justify-content: center; padding-top: 14vh; background: var(--overlay); }
.palette { width: min(560px, calc(100% - 24px)); height: fit-content; background: var(--bg-elev); border-radius: var(--radius-lg); box-shadow: var(--shadow); overflow: hidden; }
.search { display: flex; align-items: center; gap: 10px; padding: 0 16px; border-bottom: 1px solid var(--border); }
.search svg { width: 17px; height: 17px; color: var(--text-3); }
.search input { flex: 1; height: 52px; border: 0; background: transparent; outline: none; font-size: 15px; }
.list { max-height: 360px; overflow: auto; margin: 0; padding: 6px; list-style: none; }
.list li { display: flex; align-items: center; gap: 10px; height: 40px; padding: 0 12px; border-radius: var(--radius-sm); cursor: pointer; font-size: 14px; }
.list li.active { background: var(--surface-hover); }
.group { width: 36px; font-size: 12px; color: var(--text-3); }
.label { flex: 1; }
.hint { display: flex; gap: 3px; }
.enter { width: 14px; height: 14px; color: var(--text-3); }
.empty { color: var(--text-3); justify-content: center; cursor: default; }
.palette-enter-active, .palette-leave-active { transition: opacity 140ms var(--ease); }
.palette-enter-active .palette, .palette-leave-active .palette { transition: transform 200ms var(--ease); }
.palette-enter-from, .palette-leave-to { opacity: 0; }
.palette-enter-from .palette, .palette-leave-to .palette { transform: scale(0.98) translateY(-6px); }
</style>
