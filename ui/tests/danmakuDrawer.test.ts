import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import DanmakuDrawer from '@/components/sites/DanmakuDrawer.vue'
import MyAccountView from '@/views/MyAccountView.vue'

const mockDanmakuApi = vi.hoisted(() => ({
  list: vi.fn(),
  create: vi.fn(),
  update: vi.fn(),
  remove: vi.fn(),
  test: vi.fn(),
  testDraft: vi.fn(),
}))

const mockMeApi = vi.hoisted(() => ({
  get: vi.fn(),
  reorderSites: vi.fn(),
  kick: vi.fn(),
  kickSession: vi.fn(),
  redeem: vi.fn(),
  password: vi.fn(),
}))

vi.mock('@/api', () => ({
  danmakuApi: mockDanmakuApi,
  meApi: mockMeApi,
}))

vi.mock('@/stores/toast', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
  errorMessage: (_err: unknown, fallback = 'error') => fallback,
}))

vi.mock('@/stores/confirm', () => ({
  confirm: vi.fn().mockResolvedValue(true),
}))

vi.mock('@/stores/session', () => ({
  session: { username: 'alice' },
  logout: vi.fn(),
}))

vi.mock('@/stores/theme', () => ({
  themeMode: { value: 'system' },
  cycleTheme: vi.fn(),
}))

describe('DanmakuDrawer & MyAccountView 弹幕服务 (v1.5.10)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockDanmakuApi.list.mockResolvedValue([
      {
        id: 1,
        name: 'JPCO 赠送弹幕',
        upstream_url: 'https://danmu.provider.example/secret',
        egress_node_id: 2,
        egress_name: 'RFC-JPCO',
        egress_online: true,
        enabled: true,
        sort_order: 1,
        note: '支持 SenPlayer / Forward',
        created_at: '2026-11-27',
        updated_at: '2026-11-27',
      },
    ])
  })

  it('展示已有弹幕源并支持绑定从机出口新增与测通', async () => {
    mockDanmakuApi.test.mockResolvedValue({
      ok: true,
      status_code: 200,
      latency_ms: 48,
      egress_name: 'RFC-JPCO',
    })
    mockDanmakuApi.create.mockResolvedValue({
      id: 2,
      name: '备用弹幕',
      upstream_url: 'https://dm2.example.com',
      egress_node_id: 2,
      egress_name: 'RFC-JPCO',
      egress_online: true,
      enabled: true,
      sort_order: 2,
      note: '',
      created_at: '2026-11-27',
      updated_at: '2026-11-27',
    })

    const wrapper = mount(DanmakuDrawer, {
      props: {
        open: true,
        edgeNodes: [
          {
            id: 2,
            name: 'RFC-JPCO',
            online: true,
            enabled: true,
            public_port: 19090,
            listen_port: 19090,
            public_host: '',
            public_ip: '82.40.35.82',
            detected_ip: '82.40.35.82',
            tls_mode: 'auto',
            traffic_quota: 0,
            traffic_used: 0,
            traffic_reset_day: 0,
            note: '',
            agent_version: 'v1.5.9',
            last_seen_at: '',
            tls_active: true,
            active_streams: 0,
            current_bps: 0,
            public_url: 'https://edge.example.com',
            status: 'online',
            created_at: '',
            updated_at: '',
          },
        ],
      },
      global: { stubs: { Teleport: true } },
    })
    await flushPromises()

    expect(wrapper.text()).toContain('JPCO 赠送弹幕')
    expect(wrapper.text()).toContain('RFC-JPCO')

    const testBtn = wrapper.findAll('button').find(b => b.text().includes('测通'))
    expect(testBtn).toBeTruthy()
    await testBtn!.trigger('click')
    await flushPromises()
    expect(mockDanmakuApi.test).toHaveBeenCalledWith(1)
    expect(wrapper.text()).toContain('HTTP 200 · 48 ms')

    const addBtn = wrapper.findAll('button').find(b => b.text().includes('添加弹幕源'))
    expect(addBtn).toBeTruthy()
    await addBtn!.trigger('click')
    await wrapper.find('#dm-name').setValue('备用弹幕')
    await wrapper.find('#dm-url').setValue('https://dm2.example.com')
    await wrapper.find('#dm-egress').setValue('2')
    expect(wrapper.text()).toContain('强绑定防泄露')

    await wrapper.find('form.dm-form').trigger('submit')
    await flushPromises()
    expect(mockDanmakuApi.create).toHaveBeenCalledWith({
      name: '备用弹幕',
      upstream_url: 'https://dm2.example.com',
      egress_node_id: 2,
      enabled: true,
      note: '',
    })
  })

  it('有效分享用户在自助页看到弹幕服务卡片，过期用户隐藏', async () => {
    mockMeApi.get.mockResolvedValueOnce({
      id: 1,
      username: 'alice',
      route_prefix: '/alice',
      route_url: 'https://emby.example.com/alice',
      note: '',
      enabled: true,
      expires_at: '2027-01-01',
      traffic_quota: 0,
      traffic_used: 0,
      status: 'active',
      session_count: 0,
      max_devices: 0,
      speed_limit: 0,
      traffic_reset_day: 0,
      sites: [],
      servers: [],
      danmaku_sources: [
        {
          id: 1,
          name: 'JPCO 赠送弹幕',
          url: 'https://emby.example.com/dm/1/deadbeef00112233',
          note: '支持 SenPlayer / Forward',
        },
      ],
      created_at: '',
      updated_at: '',
    })

    const activeWrapper = mount(MyAccountView)
    await flushPromises()
    expect(activeWrapper.text()).toContain('弹幕服务')
    expect(activeWrapper.text()).toContain('https://emby.example.com/dm/1/deadbeef00112233')
    expect(activeWrapper.text()).toContain('播放器填写指引')
    activeWrapper.unmount()

    mockMeApi.get.mockResolvedValueOnce({
      id: 1,
      username: 'alice',
      route_prefix: '/alice',
      route_url: 'https://emby.example.com/alice',
      note: '',
      enabled: true,
      expires_at: '2027-01-01',
      traffic_quota: 0,
      traffic_used: 0,
      status: 'active',
      session_count: 0,
      max_devices: 0,
      speed_limit: 0,
      traffic_reset_day: 0,
      sites: [],
      servers: [],
      edge_ip_alert: {
        node_name: 'RFC-JPCO',
        expected_ip: '1.2.3.4',
        actual_ip: '45.6.7.8',
        actual_region: '中国 · 香港',
        actual_isp: 'Cloudflare',
        direct_rule: 'emby.example.com',
        occurred_at: new Date().toISOString(),
      },
      created_at: '',
      updated_at: '',
    })

    const alertWrapper = mount(MyAccountView)
    await flushPromises()
    expect(alertWrapper.text()).toContain('从机因客户端 IP 不一致拦截了播放')
    expect(alertWrapper.text()).toContain('RFC-JPCO')
    expect(alertWrapper.text()).toContain('DOMAIN-SUFFIX,emby.example.com,DIRECT')
    alertWrapper.unmount()

    mockMeApi.get.mockResolvedValueOnce({
      id: 1,
      username: 'alice',
      route_prefix: '/alice',
      route_url: 'https://emby.example.com/alice',
      note: '',
      enabled: true,
      expires_at: '2020-01-01',
      traffic_quota: 0,
      traffic_used: 0,
      status: 'expired',
      session_count: 0,
      max_devices: 0,
      speed_limit: 0,
      traffic_reset_day: 0,
      sites: [],
      servers: [],
      danmaku_sources: [],
      created_at: '',
      updated_at: '',
    })

    const expiredWrapper = mount(MyAccountView)
    await flushPromises()
    expect(expiredWrapper.text()).not.toContain('弹幕服务')
    expiredWrapper.unmount()
  })
})
