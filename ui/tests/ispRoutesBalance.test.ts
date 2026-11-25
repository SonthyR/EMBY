import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import IspRoutesView from '@/views/IspRoutesView.vue'

const mockApi = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
  setStrategy: vi.fn(),
  setGroupBlock: vi.fn(),
  setBlockNonMainland: vi.fn(),
  ipStatus: vi.fn(),
}))

vi.mock('@/api', () => ({
  ispRoutesApi: {
    get: mockApi.get,
    update: mockApi.update,
    setStrategy: mockApi.setStrategy,
    setGroupBlock: mockApi.setGroupBlock,
    setBlockNonMainland: mockApi.setBlockNonMainland,
  },
  ip2regionApi: {
    status: mockApi.ipStatus,
    update: vi.fn(),
    remove: vi.fn(),
    lookup: vi.fn(),
  },
}))

vi.mock('@/stores/toast', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
  errorMessage: (_err: unknown, fallback = 'error') => fallback,
}))

vi.mock('@/stores/confirm', () => ({
  confirm: vi.fn(async () => true),
}))

describe('IspRoutesView 阶梯负载均衡与策略精简 (v1.5.3)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockApi.ipStatus.mockResolvedValue({
      v4: { version: 'v4', downloaded: true, size_bytes: 1024 },
      v6: { version: 'v6', downloaded: false, size_bytes: 0 },
      mirrors: [],
    })
    mockApi.get.mockResolvedValue({
      block_non_mainland: false,
      groups: [
        {
          group: 'ct',
          label: '电信',
          strategy: 'balance',
          block_connect: false,
          nodes: [
            {
              node_id: 1,
              name: '优化NAT-1',
              is_master: false,
              enabled: true,
              online: true,
              status: 'online',
              active: true,
              max_balance_streams: 2,
              active_sessions: 1,
              public_url: 'https://node-1.example.com:19090',
            },
            {
              node_id: 0,
              name: '主机',
              is_master: true,
              enabled: true,
              online: true,
              status: 'online',
              active: true,
              max_balance_streams: 2,
              active_sessions: 0,
            },
          ],
        },
      ],
    })
  })

  it('只显示「手动优先级」和「负载均衡」两个策略，不再显示「自动择优」，并支持内联修改每台承载流数', async () => {
    mockApi.update.mockResolvedValue({
      block_non_mainland: false,
      groups: [
        {
          group: 'ct',
          label: '电信',
          strategy: 'balance',
          block_connect: false,
          nodes: [
            {
              node_id: 1,
              name: '优化NAT-1',
              is_master: false,
              enabled: true,
              online: true,
              status: 'online',
              active: true,
              max_balance_streams: 3,
              active_sessions: 1,
            },
            {
              node_id: 0,
              name: '主机',
              is_master: true,
              enabled: true,
              online: true,
              status: 'online',
              active: true,
              max_balance_streams: 2,
              active_sessions: 0,
            },
          ],
        },
      ],
    })

    const wrapper = mount(IspRoutesView, {
      global: { stubs: { SpeedtestDrawer: true } },
    })
    await flushPromises()

    const segButtons = wrapper.findAll('.strategy-bar .seg button').map(b => b.text())
    expect(segButtons).toEqual(['手动优先级', '负载均衡'])
    expect(wrapper.text()).not.toContain('自动择优')
    expect(wrapper.text()).toContain('负载均衡 (2 台)')

    const capInputs = wrapper.findAll('input.cap-input')
    expect(capInputs).toHaveLength(2)
    expect((capInputs[0].element as HTMLInputElement).value).toBe('2')

    await capInputs[0].setValue('3')
    await capInputs[0].trigger('change')
    await flushPromises()

    expect(mockApi.update).toHaveBeenCalledWith('ct', [
      { node_id: 1, enabled: true, max_balance_streams: 3 },
      { node_id: 0, enabled: true },
    ])
  })
})
