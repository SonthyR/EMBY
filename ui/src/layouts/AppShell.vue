<script setup lang="ts">
import logo from '@/assets/logo.svg'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { LogOut, Moon, Sun, SunMoon } from '@lucide/vue'
import NavIcon from '@/components/NavIcon.vue'
import { navRoutes, type NavMeta } from '@/router'
import { logout, session } from '@/stores/session'
import { cycleTheme, themeMode } from '@/stores/theme'

const route = useRoute()
const meta = computed(() => route.meta as unknown as NavMeta)
const mainNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'main')
const manageNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).nav === 'manage')
const mobileNav = navRoutes.filter(r => (r.meta as unknown as NavMeta).mobile)
const themeLabel = computed(() => ({ system: '跟随系统', light: '浅色', dark: '深色' })[themeMode.value])
const version = 'v1.0.0'
</script>

<template>
  <div class="shell">
    <aside class="sidebar">
      <div class="brand">
        <img :src="logo" alt="" />
        <div>
          <strong>云渡</strong>
          <small>{{ version }}</small>
        </div>
      </div>
      <nav aria-label="主导航">
        <p class="section">服务</p>
        <RouterLink v-for="item in mainNav" :key="item.path" :to="item.path" class="nav-link">
          <NavIcon :name="(item.meta as any).icon" />
          <span>{{ (item.meta as any).title }}</span>
        </RouterLink>
        <p class="section">管理</p>
        <RouterLink v-for="item in manageNav" :key="item.path" :to="item.path" class="nav-link">
          <NavIcon :name="(item.meta as any).icon" />
          <span>{{ (item.meta as any).title }}</span>
        </RouterLink>
      </nav>
      <div class="sidebar-foot">
        <button class="nav-link" type="button" :title="`主题：${themeLabel}`" @click="cycleTheme">
          <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
          <span>主题：{{ themeLabel }}</span>
        </button>
        <RouterLink to="/account" class="nav-link account">
          <span class="avatar">{{ (session.username || 'A').slice(0, 1).toUpperCase() }}</span>
          <span class="who">{{ session.username || '管理员' }}</span>
        </RouterLink>
        <button class="nav-link" type="button" @click="logout"><LogOut /><span>退出登录</span></button>
      </div>
    </aside>

    <div class="main">
      <header class="topbar">
        <h1>{{ meta.title }}</h1>
        <div class="top-actions">
          <button class="icon-btn mobile-only" type="button" :aria-label="`主题：${themeLabel}`" @click="cycleTheme">
            <SunMoon v-if="themeMode === 'system'" /><Sun v-else-if="themeMode === 'light'" /><Moon v-else />
          </button>
          <RouterLink to="/account" class="icon-btn mobile-only" aria-label="账户">
            <span class="avatar sm">{{ (session.username || 'A').slice(0, 1).toUpperCase() }}</span>
          </RouterLink>
        </div>
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
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.sidebar {
  position: sticky;
  top: 0;
  display: flex;
  flex-direction: column;
  flex: 0 0 232px;
  height: 100vh;
  padding: 18px 12px;
  border-right: 1px solid var(--border);
  background: var(--bg-elev);
}
.brand { display: flex; align-items: center; gap: 10px; padding: 4px 10px 18px; }
.brand img { width: 30px; height: 30px; }
.brand strong { display: block; font-size: 17px; letter-spacing: 0.04em; }
.brand small { color: var(--text-3); font-size: 11px; }
.section { margin: 14px 10px 6px; font-size: 11px; color: var(--text-3); letter-spacing: 0.08em; }
.nav-link {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  min-height: 38px;
  padding: 0 10px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  font-size: 14px;
  text-align: left;
  cursor: pointer;
  transition: background var(--dur) var(--ease), color var(--dur) var(--ease);
}
.nav-link :deep(svg) { width: 18px; height: 18px; flex: 0 0 18px; }
.nav-link:hover { background: var(--surface-hover); color: var(--text); }
.nav-link.router-link-active { background: var(--accent-soft); color: var(--accent); font-weight: 500; }
.sidebar-foot { margin-top: auto; display: flex; flex-direction: column; gap: 2px; padding-top: 12px; border-top: 1px solid var(--border); }
.avatar {
  display: grid;
  place-items: center;
  width: 24px;
  height: 24px;
  border-radius: 50%;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 12px;
  font-weight: 600;
}
.avatar.sm { width: 28px; height: 28px; }
.who { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.topbar {
  position: sticky;
  top: 0;
  z-index: 20;
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 60px;
  padding: 0 28px;
  background: color-mix(in srgb, var(--bg) 85%, transparent);
  backdrop-filter: blur(10px);
  border-bottom: 1px solid var(--border);
}
.topbar h1 { margin: 0; font-size: 18px; font-weight: 600; }
.top-actions { display: flex; gap: 6px; }
.icon-btn {
  display: grid;
  place-items: center;
  width: 36px;
  height: 36px;
  border: 0;
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-2);
  cursor: pointer;
}
.icon-btn :deep(svg) { width: 19px; height: 19px; }
.content { flex: 1; width: 100%; max-width: 1280px; margin: 0 auto; padding: 24px 28px 48px; }
.mobile-only, .tabbar { display: none; }

@media (max-width: 860px) {
  .sidebar { display: none; }
  .topbar { height: 54px; padding: 0 16px; padding-top: env(safe-area-inset-top); }
  .mobile-only { display: grid; }
  .content { padding: 16px 16px calc(84px + env(safe-area-inset-bottom)); }
  .tabbar {
    position: fixed;
    left: 0;
    right: 0;
    bottom: 0;
    z-index: 30;
    display: flex;
    padding: 6px 6px calc(6px + env(safe-area-inset-bottom));
    background: color-mix(in srgb, var(--bg-elev) 92%, transparent);
    backdrop-filter: blur(12px);
    border-top: 1px solid var(--border);
  }
  .tab {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
    padding: 6px 0;
    border-radius: var(--radius-sm);
    color: var(--text-3);
    font-size: 11px;
  }
  .tab :deep(svg) { width: 21px; height: 21px; }
  .tab.router-link-active { color: var(--accent); }
}
</style>
