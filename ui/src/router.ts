import { createRouter, createWebHashHistory, type RouteRecordRaw } from 'vue-router'

export interface NavMeta {
  title: string
  nav?: 'main' | 'manage'
  icon?: string
  mobile?: boolean
}

const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/dashboard' },
  { path: '/dashboard', component: () => import('@/views/DashboardView.vue'), meta: { title: '仪表盘', nav: 'main', icon: 'dashboard', mobile: true } },
  { path: '/sites', component: () => import('@/views/SitesView.vue'), meta: { title: '站点', nav: 'main', icon: 'sites', mobile: true } },
  { path: '/share', component: () => import('@/views/ShareUsersView.vue'), meta: { title: '分享账号', nav: 'main', icon: 'share', mobile: true } },
  { path: '/logs', component: () => import('@/views/LegacyView.vue'), meta: { title: '日志', nav: 'main', icon: 'logs', legacy: 'request-logs' } },
  { path: '/settings', component: () => import('@/views/LegacyView.vue'), meta: { title: '设置', nav: 'manage', icon: 'settings', mobile: true, legacy: 'global-settings' } },
  { path: '/diagnostics', component: () => import('@/views/LegacyView.vue'), meta: { title: '诊断', nav: 'manage', icon: 'diag', legacy: 'diagnostics' } },
  { path: '/account', component: () => import('@/views/LegacyView.vue'), meta: { title: '账户', icon: 'account', legacy: 'account' } },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' },
]

export const router = createRouter({
  history: createWebHashHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

router.afterEach(to => {
  const title = (to.meta as unknown as NavMeta).title
  document.title = title ? `${title} · 云渡` : '云渡'
})

export const navRoutes = routes.filter(route => (route.meta as unknown as NavMeta | undefined)?.nav)
