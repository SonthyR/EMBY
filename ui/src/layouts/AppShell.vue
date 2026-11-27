<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ChevronDown, LogOut, Menu, Moon, Search, Sun, SunMoon } from '@lucide/vue'
import logo from '@/assets/logo.svg'
import NavIcon from '@/components/NavIcon.vue'
import CommandPalette from '@/components/CommandPalette.vue'
import { navRoutes, type NavMeta } from '@/router'
import { logout, session } from '@/stores/session'
import { cycleTheme, themeMode } from '@/stores/theme'
import { commandOpen } from '@/stores/command'

const route = useRoute()
const router = useRouter()
const mainNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'main')
const manageNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'manage')
const mobileNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).mobile)
const mobileOverflowNav = [
  ...navRoutes.filter(r => !(r.meta as unknown as NavMeta).mobile),
  { path: '/account', meta: { title: '账户与密码', icon: 'account' } },
]
const themeLabel = computed(() => ({ system: '跟随系统', light: '浅色', dark: '深色' })[themeMode.value])
const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)
const moreOpen = ref(false)
const mobileMoreOpen = ref(false)
const moreRoot = ref<HTMLElement | null>(null)
const mobileMoreRoot = ref<HTMLElement | null>(null)

const isMoreActive = computed(() => manageNav.some(r => route.path.startsWith(r.path)))
const activeMoreTitle = computed(() => {
  const hit = manageNav.find(r => route.path.startsWith(r.path))
  return hit ? (hit.meta as unknown as NavMeta).title : '更多'
})

function onDocumentClick(event: MouseEvent) {
  const target = event.target as Node
  if (moreOpen.value && moreRoot.value && !moreRoot.value.contains(target)) moreOpen.value = false
  if (mobileMoreOpen.value && mobileMoreRoot.value && !mobileMoreRoot.value.contains(target)) mobileMoreOpen.value = false
}
function onKey(event: KeyboardEvent) {
  if (event.key === 'Escape') {
    moreOpen.value = false
    mobileMoreOpen.value = false
  }
}

watch([moreOpen, mobileMoreOpen], ([a, b]) => {
  if (a || b) {
    document.addEventListener('mousedown', onDocumentClick)
    document.addEventListener('keydown', onKey)
  } else {
    document.removeEventListener('mousedown', onDocumentClick)
    document.removeEventListener('keydown', onKey)
  }
})
watch(() => route.path, () => {
  moreOpen.value = false
  mobileMoreOpen.value = false
})
onBeforeUnmount(() => {
  document.removeEventListener('mousedown', onDocumentClick)
  document.removeEventListener('keydown', onKey)
})

function go(path: string) {
  moreOpen.value = false
  mobileMoreOpen.value = false
  router.push(path)
}
</script>

<template>
  <div class="shell">
    <header class="topbar">
      <div class="topbar-inner">
        <RouterLink to="/dashboard" class="brand">
          <span class="brand-mark"><img :src="logo" alt="" /></span>
          <span class="brand-name">云渡</span>
          <span class="brand-ver mono">v1.5.10</span>
        </RouterLink>

        <nav class="nav-capsule" aria-label="主导航">
          <RouterLink v-for="item in mainNav" :key="item.path" :to="item.path" class="nav-pill">
            <NavIcon :name="(item.meta as any).icon" />
            <span>{{ (item.meta as any).title }}</span>
          </RouterLink>

          <div ref="moreRoot" class="more-wrap">
            <button
              type="button"
              class="nav-pill more-btn"
              :class="{ active: isMoreActive || moreOpen }"
              :aria-expanded="moreOpen"
              aria-haspopup="menu"
              @click="moreOpen = !moreOpen"
            >
              <span>{{ activeMoreTitle }}</span>
              <ChevronDown class="chev" :class="{ rotated: moreOpen }" />
            </button>
            <Transition name="pop">
              <div v-if="moreOpen" class="more-menu" role="menu" aria-label="更多功能">
                <button
                  v-for="item in manageNav"
                  :key="item.path"
                  type="button"
                  role="menuitem"
                  class="more-item"
                  :class="{ current: route.path.startsWith(item.path) }"
                  @click="go(item.path)"
                >
                  <NavIcon :name="(item.meta as any).icon" />
                  <span>{{ (item.meta as any).title }}</span>
                </button>
                <hr />
                <button type="button" role="menuitem" class="more-item" :class="{ current: route.path.startsWith('/account') }" @click="go('/account')">
                  <NavIcon name="account" />
                  <span>管理员账户</span>
                </button>
              </div>
            </Transition>
          </div>
        </nav>

        <div class="top-actions">
          <button type="button" class="search-pill" title="打开命令面板" @click="commandOpen = true">
            <Search />
            <span class="search-text">搜索</span>
            <kbd>{{ isMac ? '⌘' : 'Ctrl' }}K</kbd>
          </button>
          <button type="button" class="icon-pill" :title="`主题：${themeLabel}`" :aria-label="`主题：${themeLabel}`" @click="cycleTheme">
            <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
          </button>
          <RouterLink to="/account" class="avatar-pill" :title="`账户：${session.username || '管理员'}`">
            <span class="avatar">{{ (session.username || 'A').slice(0, 1).toUpperCase() }}</span>
          </RouterLink>
          <button type="button" class="icon-pill" title="退出登录" aria-label="退出登录" @click="logout">
            <LogOut />
          </button>
        </div>

        <div class="mobile-actions">
          <button type="button" class="icon-pill" aria-label="搜索" @click="commandOpen = true"><Search /></button>
          <button type="button" class="icon-pill" :aria-label="`主题：${themeLabel}`" @click="cycleTheme">
            <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
          </button>
          <div ref="mobileMoreRoot" class="more-wrap">
            <button type="button" class="icon-pill" aria-label="更多菜单" :aria-expanded="mobileMoreOpen" @click="mobileMoreOpen = !mobileMoreOpen">
              <Menu />
            </button>
            <Transition name="pop">
              <div v-if="mobileMoreOpen" class="more-menu" role="menu">
                <button
                  v-for="item in mobileOverflowNav"
                  :key="item.path"
                  type="button"
                  role="menuitem"
                  class="more-item"
                  :class="{ current: route.path.startsWith(item.path) }"
                  @click="go(item.path)"
                >
                  <NavIcon :name="(item.meta as any).icon" />
                  <span>{{ (item.meta as any).title }}</span>
                </button>
                <hr />
                <button type="button" role="menuitem" class="more-item danger" @click="logout">
                  <LogOut />
                  <span>退出登录</span>
                </button>
              </div>
            </Transition>
          </div>
        </div>
      </div>
    </header>

    <main class="content">
      <RouterView v-slot="{ Component }">
        <Transition name="page" mode="out-in">
          <component :is="Component" :key="route.path" />
        </Transition>
      </RouterView>
    </main>

    <nav class="tabbar" aria-label="快捷导航">
      <div class="tabbar-inner">
        <RouterLink v-for="item in mobileNav" :key="item.path" :to="item.path" class="tab">
          <NavIcon :name="(item.meta as any).icon" />
          <span>{{ (item.meta as any).title }}</span>
        </RouterLink>
      </div>
    </nav>
    <CommandPalette />
  </div>
</template>

<style scoped>
.shell { min-height: 100vh; display: flex; flex-direction: column; }
.topbar { position: sticky; top: 0; z-index: 40; background: var(--surface-glass); backdrop-filter: saturate(1.7) blur(20px); -webkit-backdrop-filter: saturate(1.7) blur(20px); border-bottom: 1px solid var(--border); }
.topbar-inner { max-width: 1240px; margin: 0 auto; height: 62px; padding: 0 28px; display: flex; align-items: center; justify-content: space-between; gap: 16px; }

.brand { display: inline-flex; align-items: center; gap: 10px; flex: 0 0 auto; }
.brand-mark { display: grid; place-items: center; width: 30px; height: 30px; border-radius: 10px; background: var(--hero-gradient); box-shadow: 0 3px 10px rgba(249, 87, 37, 0.22); }
.brand-mark img { width: 17px; height: 17px; filter: brightness(0) invert(1); }
.brand-name { font-size: 16px; font-weight: 700; letter-spacing: -0.02em; }
.brand-ver { padding: 2px 7px; border-radius: 999px; background: var(--surface-2); border: 1px solid var(--border); font-size: 10.5px; color: var(--text-3); }

.nav-capsule { display: inline-flex; align-items: center; gap: 3px; padding: 4px; border-radius: 999px; background: var(--surface-2); border: 1px solid var(--border); }
.nav-pill { display: inline-flex; align-items: center; gap: 6px; height: 32px; padding: 0 13px; border: 0; border-radius: 999px; background: transparent; color: var(--text-2); font-size: 13px; font-weight: 500; white-space: nowrap; cursor: pointer; transition: background var(--dur) var(--ease), color var(--dur) var(--ease), box-shadow var(--dur) var(--ease); }
.nav-pill :deep(svg) { width: 14.5px; height: 14.5px; stroke-width: 1.85; }
.nav-pill:hover { color: var(--text); background: color-mix(in srgb, var(--surface) 65%, transparent); }
.nav-pill.router-link-active, .nav-pill.active { background: var(--surface); color: var(--text); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08), 0 0 0 1px var(--border); }
.nav-pill.router-link-active :deep(svg) { color: var(--accent); }

.more-wrap { position: relative; display: inline-flex; }
.more-btn { padding-right: 10px; }
.chev { width: 14px !important; height: 14px !important; color: var(--text-3); transition: transform var(--dur) var(--ease); }
.chev.rotated { transform: rotate(180deg); }
.more-menu { position: absolute; right: 0; top: calc(100% + 10px); z-index: 60; min-width: 176px; padding: 6px; background: var(--bg-elev); border: 1px solid var(--border); border-radius: var(--radius); box-shadow: var(--shadow); }
.more-item { display: flex; align-items: center; gap: 9px; width: 100%; padding: 9px 11px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-2); font-size: 13px; font-weight: 500; text-align: left; cursor: pointer; transition: background var(--dur) var(--ease), color var(--dur) var(--ease); }
.more-item :deep(svg) { width: 15px; height: 15px; stroke-width: 1.8; }
.more-item:hover { background: var(--surface-2); color: var(--text); }
.more-item.current { background: var(--accent-soft); color: var(--accent); font-weight: 600; }
.more-item.danger { color: var(--bad); }
.more-item.danger:hover { background: var(--bad-soft); }
.more-menu hr { border: 0; border-top: 1px solid var(--border); margin: 5px 4px; }

.top-actions { display: flex; align-items: center; gap: 7px; flex: 0 0 auto; }
.search-pill { display: inline-flex; align-items: center; gap: 7px; height: 34px; padding: 0 10px 0 11px; border: 1px solid var(--border); border-radius: 999px; background: var(--surface-2); color: var(--text-2); font-size: 12.5px; cursor: pointer; transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), color var(--dur) var(--ease); }
.search-pill:hover { border-color: var(--border-strong); background: var(--surface); color: var(--text); }
.search-pill svg { width: 14px; height: 14px; color: var(--text-3); }
.icon-pill { display: grid; place-items: center; width: 34px; height: 34px; border: 1px solid var(--border); border-radius: 50%; background: var(--surface-2); color: var(--text-2); cursor: pointer; transition: border-color var(--dur) var(--ease), background var(--dur) var(--ease), color var(--dur) var(--ease); }
.icon-pill:hover { border-color: var(--border-strong); background: var(--surface); color: var(--text); }
.icon-pill :deep(svg) { width: 15.5px; height: 15.5px; stroke-width: 1.8; }
.avatar-pill { display: grid; place-items: center; width: 34px; height: 34px; border-radius: 50%; padding: 2px; border: 1px solid var(--border); transition: border-color var(--dur) var(--ease); }
.avatar-pill:hover, .avatar-pill.router-link-active { border-color: var(--accent); }
.avatar { display: grid; place-items: center; width: 100%; height: 100%; border-radius: 50%; background: var(--inverse-bg); color: var(--inverse-text); font-size: 12px; font-weight: 700; }

.content { flex: 1; width: 100%; max-width: 1240px; margin: 0 auto; padding: 32px 28px 68px; }
.mobile-actions, .tabbar { display: none; }
.pop-enter-active, .pop-leave-active { transition: opacity 130ms var(--ease), transform 130ms var(--ease); }
.pop-enter-from, .pop-leave-to { opacity: 0; transform: translateY(-6px) scale(0.98); }

@media (max-width: 980px) {
  .brand-ver, .search-text { display: none; }
  .topbar-inner { padding: 0 18px; }
  .content { padding: 24px 18px 64px; }
}
@media (max-width: 820px) {
  .topbar-inner { height: 54px; padding: env(safe-area-inset-top) 14px 0; }
  .nav-capsule, .top-actions { display: none; }
  .mobile-actions { display: flex; align-items: center; gap: 6px; }
  .content { padding: 18px 14px calc(92px + env(safe-area-inset-bottom)); }
  .tabbar { position: fixed; left: 12px; right: 12px; bottom: calc(10px + env(safe-area-inset-bottom)); z-index: 40; display: block; }
  .tabbar-inner { display: flex; padding: 5px; border-radius: 999px; background: var(--surface-glass); backdrop-filter: saturate(1.7) blur(20px); -webkit-backdrop-filter: saturate(1.7) blur(20px); border: 1px solid var(--border); box-shadow: var(--shadow); }
  .tab { flex: 1; display: flex; flex-direction: column; align-items: center; gap: 2px; padding: 6px 0; border-radius: 999px; color: var(--text-3); font-size: 10.5px; font-weight: 500; transition: background var(--dur) var(--ease), color var(--dur) var(--ease); }
  .tab :deep(svg) { width: 18px; height: 18px; stroke-width: 1.75; }
  .tab.router-link-active { background: var(--surface); color: var(--accent); font-weight: 600; box-shadow: 0 1px 3px rgba(15, 23, 42, 0.08); }
}
</style>
