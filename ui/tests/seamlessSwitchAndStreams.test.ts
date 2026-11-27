import { mount } from '@vue/test-utils'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import MyAccountView from '@/views/MyAccountView.vue'
import ShareUserForm from '@/components/share/ShareUserForm.vue'
import { meApi, shareApi, sitesApi, type MyAccount, type Site } from '@/api'

function flush(): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, 25))
}

function makeAccount(overrides: Partial<MyAccount> = {}): MyAccount {
  return {
    id: 1,
    username: 'alice',
    route_prefix: '/alice',
    route_url: 'https://emby.example.com/alice',
    note: '',
    enabled: true,
    expires_at: '2027-01-01',
    traffic_quota: 100 * 1024 * 1024 * 1024,
    traffic_used: 10 * 1024 * 1024 * 1024,
    status: 'active',
    session_count: 2,
    active_streams: 1,
    max_devices: 5,
    max_concurrent_streams: 2,
    speed_limit: 0,
    traffic_reset_day: 1,
    active_site_id: 10,
    active_site_name: 'Alpha 服',
    sites: [
      { site_id: 10, site_account_id: 0, site_name: 'Alpha 服', site_enabled: true, sort_order: 1, active: true },
      { site_id: 20, site_account_id: 0, site_name: 'Beta 服', site_enabled: true, sort_order: 2, active: false },
    ],
    servers: [
      { site_id: 10, name: 'Alpha 服', enabled: true, active: true, addresses: [] },
      { site_id: 20, name: 'Beta 服', enabled: true, active: false, addresses: [] },
    ],
    sessions: [],
    created_at: '2026-11-01',
    updated_at: '2026-11-27',
    ...overrides,
  }
}

describe('v1.6.0 无感切服引导与同时播放路数限制', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
  })

  it('MyAccountView 展示同时播放额度，置顶切服后展示免重登引导卡片', async () => {
    const initial = makeAccount()
    const switched = makeAccount({
      active_site_id: 20,
      active_site_name: 'Beta 服',
      servers: [
        { site_id: 20, name: 'Beta 服', enabled: true, active: true, addresses: [] },
        { site_id: 10, name: 'Alpha 服', enabled: true, active: false, addresses: [] },
      ],
    })
    vi.spyOn(meApi, 'get').mockResolvedValue(initial)
    vi.spyOn(meApi, 'reorderSites').mockResolvedValue({
      ...switched,
      revoked: 0,
      draining: 0,
      seamless: 1,
    })

    const wrapper = mount(MyAccountView)
    await flush()

    expect(wrapper.text()).toContain('同时播放')
    expect(wrapper.text()).toContain('最多 2 路')
    expect(wrapper.text()).toContain('无需重新登录')

    const promoteBtn = wrapper.findAll('button').find(b => b.text().includes('设为首选'))
    expect(promoteBtn).toBeTruthy()
    await promoteBtn!.trigger('click')
    await flush()

    expect(wrapper.find('.switch-guide').exists()).toBe(true)
    expect(wrapper.find('.switch-guide').text()).toContain('已将「Beta 服」设为首选')
    expect(wrapper.find('.switch-guide').text()).toContain('下拉刷新即可看到新片库')
  })

  it('ShareUserForm 支持配置并提交 max_concurrent_streams', async () => {
    const sites: Site[] = [
      {
        id: 10,
        name: 'Alpha 服',
        listen_port: 8001,
        ingress_mode: 'path',
        path_prefix: '/alpha',
        public_host: '',
        target_url: 'https://a.example.com',
        playback_mode: 'direct',
        stream_hosts: '[]',
        ua_mode: 'passthrough',
        enabled: true,
        running: true,
        traffic_used: 0,
        traffic_quota: 0,
        speed_limit: 0,
      } as Site,
    ]
    vi.spyOn(sitesApi, 'accounts').mockResolvedValue([])
    const createSpy = vi.spyOn(shareApi, 'create').mockResolvedValue(makeAccount({ max_concurrent_streams: 2 }))

    const wrapper = mount(ShareUserForm, {
      props: { open: true, user: null, sites, danmakuSources: [] },
      global: { stubs: { Teleport: true } },
    })
    await flush()

    await wrapper.find('#su-name').setValue('bob')
    await wrapper.find('#su-dev').setValue('5')
    await wrapper.find('#su-streams').setValue('2')
    const saveBtn = wrapper.findAll('button').find(b => b.text().includes('创建账号'))
    expect(saveBtn).toBeTruthy()
    await saveBtn!.trigger('click')
    await flush()

    expect(createSpy).toHaveBeenCalledTimes(1)
    expect(createSpy.mock.calls[0][0].max_devices).toBe(5)
    expect(createSpy.mock.calls[0][0].max_concurrent_streams).toBe(2)
  })
})
