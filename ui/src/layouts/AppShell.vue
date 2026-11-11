<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { LogOut, Moon, Sun, SunMoon, Search } from '@lucide/vue'
import logo from '@/assets/logo.svg'
import NavIcon from '@/components/NavIcon.vue'
import CommandPalette from '@/components/CommandPalette.vue'
import { navRoutes, type NavMeta } from '@/router'
import { logout, session } from '@/stores/session'
import { cycleTheme, themeMode } from '@/stores/theme'
import { commandOpen } from '@/stores/command'

const route = useRoute()
const meta = computed(() => route.meta as unknown as NavMeta)
const mainNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'main')
const manageNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'manage')
const mobileNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).mobile)
const themeLabel = computed(() => ({ system: '跟随系统', light: '浅色', dark: '深色' })[themeMode.value])
const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">
        <img :src="logo" alt="" />
        <span>云渡</span>
      </div>
      <button type="button" class="search" @click="commandOpen = true">
        <Search />
        <span>搜索或跳转</span>
        <kbd>{{ isMac ? '⌘' : 'Ctrl' }}</kbd><kbd>K</kbd>
      </button>
      <nav aria-label="主导航">
        <RouterLink v-for="item in mainNav" :key="item.path" :to="item.path" class="link">
          <NavIcon :name="(item.meta as any).icon" />
          <span>{{ (item.meta as any).title }}</span>
        </RouterLink>
        <p class="section">管理</p>
        <RouterLink v-for="item in manageNav" :key="item.path" :to="item.path" class="link">
          <NavIcon :name="(item.meta as any).icon" />
          <span>{{ (item.meta as any).title }}</span>
        </RouterLink>
      </nav>
      <div class="foot">
        <RouterLink to="/account" class="user">
          <span class="avatar">{{ (session.username || 'A').slice(0, 1).toUpperCase() }}</span>
          <span class="name">{{ session.username || '管理员' }}</span>
        </RouterLink>
        <button type="button" class="icon" :title="`主题：${themeLabel}`" :aria-label="`主题：${themeLabel}`" @click="cycleTheme">
          <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
        </button>
        <button type="button" class="icon" title="退出登录" aria-label="退出登录" @click="logout"><LogOut /></button>
      </div>
    </aside>

    <div class="main">
      <header class="mobile-bar">
        <span class="mobile-title"><img :src="logo" alt="" />云渡</span>
        <button type="button" class="icon" aria-label="搜索" @click="commandOpen = true"><Search /></button>
        <button type="button" class="icon" :aria-label="`主题：${themeLabel}`" @click="cycleTheme">
          <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
        </button>
      </header>
      <main class="content">
        <RouterView v-slot="{ Component }">
          <Transition name="page" mode="out-in">
            <component :is="Component" :key="route.path" />
          </Transition>
        </RouterView>
      </main>
    </div>

    <nav class="tabbar" aria-label="快捷导航">
      <RouterLink v-for="item in mobileNav" :key="item.path" :to="item.path" class="tab">
        <NavIcon :name="(item.meta as any).icon" />
        <span>{{ (item.meta as any).title }}</span>
      </RouterLink>
    </nav>
    <CommandPalette />
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.sidebar {
  position: sticky;
  top: 0;
  display: flex;
  flex-direction: column;
  flex: 0 0 228px;
  height: 100vh;
  padding: 16px 12px 12px;
  border-right: 1px solid var(--border);
}
.brand { display: flex; align-items: center; gap: 9px; padding: 4px 8px 16px; font-weight: 600; font-size: 15px; letter-spacing: 0.02em; }
.brand img { width: 22px; height: 22px; }
.search {
  display: flex;
  align-items: center;
  gap: 8px;
  height: 32px;
  margin: 0 0 14px;
  padding: 0 8px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-3);
  font-size: 13px;
  cursor: pointer;
  transition: border-color var(--dur) var(--ease);
}
.search:hover { border-color: var(--border-strong); }
.search svg { width: 14px; height: 14px; }
.search span { flex: 1; text-align: left; }
.section { margin: 18px 8px 6px; font-size: 12px; color: var(--text-3); }
.link {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 32px;
  padding: 0 8px;
  border-radius: var(--radius-sm);
  color: var(--text-2);
  font-size: 13.5px;
  transition: background var(--dur) var(--ease), color var(--dur) var(--ease);
}
.link :deep(svg) { width: 16px; height: 16px; stroke-width: 1.7; }
.link:hover { background: var(--surface-hover); color: var(--text); }
.link.router-link-active { background: var(--surface-hover); color: var(--text); font-weight: 500; }
.foot { margin-top: auto; display: flex; align-items: center; gap: 2px; padding-top: 10px; border-top: 1px solid var(--border); }
.user { flex: 1; min-width: 0; display: flex; align-items: center; gap: 8px; height: 34px; padding: 0 6px; border-radius: var(--radius-sm); }
.user:hover { background: var(--surface-hover); }
.avatar { display: grid; place-items: center; width: 22px; height: 22px; border-radius: 50%; background: var(--inverse-bg); color: var(--inverse-text); font-size: 11px; font-weight: 600; }
.name { font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.icon { display: grid; place-items: center; width: 30px; height: 30px; border: 0; border-radius: var(--radius-xs); background: transparent; color: var(--text-3); cursor: pointer; }
.icon:hover { background: var(--surface-hover); color: var(--text); }
.icon :deep(svg) { width: 16px; height: 16px; stroke-width: 1.7; }

.main { flex: 1; min-width: 0; }
.content { max-width: 1120px; margin: 0 auto; padding: 44px 40px 64px; }
.mobile-bar, .tabbar { display: none; }

@media (max-width: 860px) {
  .sidebar { display: none; }
  .mobile-bar {
    position: sticky;
    top: 0;
    z-index: 20;
    display: flex;
    align-items: center;
    gap: 2px;
    height: 50px;
    padding: env(safe-area-inset-top) 10px 0 18px;
    background: color-mix(in srgb, var(--bg) 88%, transparent);
    backdrop-filter: saturate(1.4) blur(14px);
    border-bottom: 1px solid var(--border);
  }
  .mobile-title { flex: 1; display: flex; align-items: center; gap: 8px; font-weight: 600; font-size: 15px; letter-spacing: 0.02em; }
  .mobile-title img { width: 20px; height: 20px; }
  .content { padding: 20px 16px calc(88px + env(safe-area-inset-bottom)); }
  .tabbar {
    position: fixed;
    left: 0;
    right: 0;
    bottom: 0;
    z-index: 30;
    display: flex;
    padding: 6px 8px calc(6px + env(safe-area-inset-bottom));
    background: color-mix(in srgb, var(--bg) 88%, transparent);
    backdrop-filter: saturate(1.4) blur(14px);
    border-top: 1px solid var(--border);
  }
  .tab { flex: 1; display: flex; flex-direction: column; align-items: center; gap: 3px; padding: 5px 0; color: var(--text-3); font-size: 10.5px; }
  .tab :deep(svg) { width: 20px; height: 20px; stroke-width: 1.6; }
  .tab.router-link-active { color: var(--text); }
}
</style>
