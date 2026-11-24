import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import ShareUserDetail from '@/components/share/ShareUserDetail.vue'
import type { ShareUser, Site } from '@/api'

const shareApi = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
}))

vi.mock('@/api', async importOriginal => {
  const actual = await importOriginal<typeof import('@/api')>()
  return { ...actual, shareApi }
})

enableAutoUnmount(afterEach)

const SITES: Site[] = [
  { id: 1, name: '星云 Emby', enabled: true, running: true, target_url: 'https://a.example' } as Site,
  { id: 2, name: '极光 4K 服', enabled: true, running: true, target_url: 'https://b.example' } as Site,
  { id: 3, name: '备用动漫服', enabled: true, running: true, target_url: 'https://c.example' } as Site,
]

const USER: ShareUser = {
  id: 10,
  username: 'alice',
  route_prefix: '/alice',
  route_url: 'https://yundu.example/alice',
  note: '',
  enabled: true,
  status: 'active',
  traffic_used: 0,
  traffic_quota: 0,
  traffic_reset_day: 0,
  max_devices: 2,
  speed_limit: 0,
  session_count: 0,
  active_site_id: 1,
  active_site_name: '星云 Emby',
  sites: [
    { site_id: 1, site_name: '星云 Emby', site_account_id: 0 },
    { site_id: 2, site_name: '极光 4K 服', site_account_id: 0 },
  ],
} as ShareUser

beforeEach(() => {
  vi.clearAllMocks()
  shareApi.get.mockResolvedValue(USER)
  shareApi.update.mockResolvedValue({ ...USER, active_site_id: 2, active_site_name: '极光 4K 服' })
})

describe('ShareUserDetail 首选 Emby 服拖拽排序', () => {
  it('将第 2 个 Emby 源站拖到第 1 位并保存顺序', async () => {
    const wrapper = mount(ShareUserDetail, {
      props: { open: true, user: USER, sites: SITES },
      global: { stubs: { Teleport: true } },
    })
    await flushPromises()

    const rows = wrapper.findAll('[data-drag-row]')
    expect(rows.map(r => r.find('.site-name').text())).toEqual(['星云 Emby', '极光 4K 服'])
    expect(rows[0].find('.rank-pill').text()).toBe('首选')

    await rows[1].trigger('dragstart')
    await rows[0].trigger('dragover')
    await rows[0].trigger('dragend')

    const reordered = wrapper.findAll('[data-drag-row]')
    expect(reordered.map(r => r.find('.site-name').text())).toEqual(['极光 4K 服', '星云 Emby'])
    expect(reordered[0].find('.rank-pill').text()).toBe('首选')

    await wrapper.get('.site-save-bar button').trigger('click')
    await flushPromises()

    expect(shareApi.update).toHaveBeenCalledWith(
      10,
      expect.objectContaining({
        sites: [
          { site_id: 2, site_account_id: 0 },
          { site_id: 1, site_account_id: 0 },
        ],
      }),
    )
  })
})
