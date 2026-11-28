import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import { describe, expect, it, vi } from 'vitest'
import DashboardView from '@/views/DashboardView.vue'
import ShareUsersView from '@/views/ShareUsersView.vue'

const { list, liveSessions } = vi.hoisted(() => ({ list: vi.fn(), liveSessions: vi.fn() }))

vi.mock('@/api', async importOriginal => {
  const api = await importOriginal<typeof import('@/api')>()
  return {
    ...api,
    dashboardApi: {
      ...api.dashboardApi,
      snapshot: vi.fn(async () => null),
      insights: vi.fn(async () => null),
      trends: vi.fn(async () => null),
    },
    shareApi: { ...api.shareApi, list, liveSessions },
    sitesApi: { ...api.sitesApi, list: vi.fn(async () => []) },
    danmakuApi: { ...api.danmakuApi, list: vi.fn(async () => []) },
  }
})

describe('仪表盘用户播放详情', () => {
  it('显示零进度和暂停，并通过用户名打开正确的账号详情', async () => {
    list.mockResolvedValue([{
      id: 7, username: '小王', sites: [], status: 'active', session_count: 1,
      last_watched_at: '2026-11-28T09:02:00Z',
    }])
    liveSessions.mockResolvedValue([{
      session_id: 'test-session', share_user_id: 7, username: '小王',
      playing: true, now_playing_title: '三体 S01E03',
      now_playing_position_seconds: 0, now_playing_duration_seconds: 2762, now_playing_paused: true,
    }])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/', component: DashboardView },
        { path: '/share', component: ShareUsersView },
      ],
    })
    await router.push('/')
    await router.isReady()
    const wrapper = mount(RouterView, {
      global: {
        plugins: [router],
        stubs: {
          ShareUserDetail: { props: ['open', 'user'], template: '<div v-if="open" data-test="detail">{{ user.username }}</div>' },
        },
      },
    })
    try {
      await flushPromises()
      expect(wrapper.get('.live-media-title').text()).toBe('三体 S01E03')
      expect(wrapper.get('.live-media-time').text()).toBe('00:00 / 46:02')
      expect(wrapper.text()).toContain('已暂停')
      expect(wrapper.text()).toContain('1 已暂停')
      expect(wrapper.text()).not.toContain('1 播放中')
      expect(wrapper.get('.live-user').attributes('href')).toBe('/share?user=7')
      await wrapper.get('.live-user').trigger('click')
      await flushPromises()
      expect(router.currentRoute.value.path).toBe('/share')
      expect(wrapper.get('[data-test="detail"]').text()).toBe('小王')
      expect(wrapper.get('.last-watch').text()).toContain('最后观影')
      expect(wrapper.get('.last-watch').text()).not.toContain('暂无记录')
    } finally {
      wrapper.unmount()
    }
  })
})
