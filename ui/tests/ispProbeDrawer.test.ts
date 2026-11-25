import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import IspProbeDrawer from '@/components/edge/IspProbeDrawer.vue'

const mockProbeApi = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
}))

vi.mock('@/api', () => ({
  ispProbeApi: mockProbeApi,
}))

vi.mock('@/stores/toast', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
  errorMessage: (_err: unknown, fallback = 'error') => fallback,
}))

describe('IspProbeDrawer 三网阻断探测与熔断设置 (v1.5.4)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockProbeApi.get.mockResolvedValue({
      enabled: true,
      interval_sec: 30,
      recovery_interval_sec: 180,
      fail_threshold: 3,
      recover_threshold: 2,
      targets: {
        ct: ['bj-ct-v4.ip.zstaticcdn.com:80', 'sh-ct-v4.ip.zstaticcdn.com:80', 'gd-ct-v4.ip.zstaticcdn.com:80'],
        cu: ['bj-cu-v4.ip.zstaticcdn.com:80', 'sh-cu-v4.ip.zstaticcdn.com:80', 'gd-cu-v4.ip.zstaticcdn.com:80'],
        cm: ['bj-cm-v4.ip.zstaticcdn.com:80', 'sh-cm-v4.ip.zstaticcdn.com:80', 'gd-cm-v4.ip.zstaticcdn.com:80'],
      },
    })
  })

  it('加载北上广默认目标并支持修改参数后保存', async () => {
    mockProbeApi.update.mockResolvedValue({
      enabled: true,
      interval_sec: 45,
      recovery_interval_sec: 240,
      fail_threshold: 4,
      recover_threshold: 2,
      targets: {
        ct: ['bj-ct-v4.ip.zstaticcdn.com:80'],
        cu: ['bj-cu-v4.ip.zstaticcdn.com:80'],
        cm: ['bj-cm-v4.ip.zstaticcdn.com:80'],
      },
    })

    const wrapper = mount(IspProbeDrawer, {
      props: { open: true },
      global: { stubs: { Teleport: true } },
    })
    await flushPromises()

    expect((wrapper.find('#ip-ct').element as HTMLTextAreaElement).value).toContain('bj-ct-v4.ip.zstaticcdn.com:80')
    expect((wrapper.find('#ip-iv').element as HTMLInputElement).value).toBe('30')

    await wrapper.find('#ip-iv').setValue('45')
    await wrapper.find('#ip-rec-iv').setValue('240')
    await wrapper.find('#ip-fail').setValue('4')
    await wrapper.find('#ip-ct').setValue('bj-ct-v4.ip.zstaticcdn.com:80')
    await wrapper.find('#ip-cu').setValue('bj-cu-v4.ip.zstaticcdn.com:80')
    await wrapper.find('#ip-cm').setValue('bj-cm-v4.ip.zstaticcdn.com:80')

    const saveBtn = wrapper.findAll('button').find(b => b.text().includes('保存配置'))
    expect(saveBtn).toBeTruthy()
    await saveBtn!.trigger('click')
    await flushPromises()

    expect(mockProbeApi.update).toHaveBeenCalledWith({
      enabled: true,
      interval_sec: 45,
      recovery_interval_sec: 240,
      fail_threshold: 4,
      recover_threshold: 2,
      targets: {
        ct: ['bj-ct-v4.ip.zstaticcdn.com:80'],
        cu: ['bj-cu-v4.ip.zstaticcdn.com:80'],
        cm: ['bj-cm-v4.ip.zstaticcdn.com:80'],
      },
    })
  })
})
