import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import SpeedtestDrawer from '@/components/edge/SpeedtestDrawer.vue'

const speedtestApi = vi.hoisted(() => ({
  jobs: vi.fn(),
  start: vi.fn(),
  targets: vi.fn(),
}))

vi.mock('@/api', () => ({ speedtestApi }))

enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.clearAllMocks()
  speedtestApi.jobs.mockResolvedValue({ jobs: [] })
  speedtestApi.start.mockResolvedValue({ job: { id: 'job', status: 'done', results: [] } })
})

async function openDrawer(customTargets: { id: number; name: string; enabled: boolean }[] = []) {
  speedtestApi.targets.mockResolvedValue({
    nodes: [{ id: 0, name: '主机', is_master: true, online: true }],
    provinces: [{ code: 'bj', label: '北京' }],
    isps: [{ group: 'ct', label: '电信' }],
    custom_targets: customTargets,
  })
  const wrapper = mount(SpeedtestDrawer, { props: { open: false }, global: { stubs: { Teleport: true } } })
  await wrapper.setProps({ open: true })
  await flushPromises()
  return wrapper
}

describe('测速目标选择', () => {
  it('默认测试国内三网，选择 Apple CDN 后收起地区与运营商', async () => {
    const wrapper = await openDrawer()
    expect(wrapper.findAll('.target-options label').map(option => option.text())).toEqual(['国内三网', 'Apple CDN'])
    expect(wrapper.get('.picker').text()).toContain('北京')
    expect(wrapper.find('.include-custom').exists()).toBe(false)

    await wrapper.get('.custom-bar .btn-primary').trigger('click')
    await flushPromises()
    expect(speedtestApi.start).toHaveBeenLastCalledWith(expect.objectContaining({ mode: 'builtin', custom_target_ids: [] }))

    await wrapper.get('input[value="apple"]').setValue(true)
    expect(wrapper.find('.picker').exists()).toBe(false)
    await wrapper.get('.custom-bar .btn-primary').trigger('click')
    await flushPromises()
    expect(speedtestApi.start).toHaveBeenLastCalledWith(expect.objectContaining({ mode: 'apple', custom_target_ids: [] }))
  })

  it('保留单独自定义测速和与国内三网一起测试的入口', async () => {
    const wrapper = await openDrawer([{ id: 9, name: '自定义节点', enabled: true }])
    expect(wrapper.find('#st-custom-panel').exists()).toBe(false)
    await wrapper.get('.custom-bar button[aria-expanded="false"]').trigger('click')
    expect(wrapper.get('.custom-bar button[aria-expanded="true"]').attributes('aria-controls')).toBe('st-custom-panel')
    await wrapper.get('.ct-check input').setValue(true)
    await wrapper.get('.custom-test-actions button').trigger('click')
    await flushPromises()
    expect(speedtestApi.start).toHaveBeenLastCalledWith(expect.objectContaining({ mode: 'custom', custom_target_ids: [9] }))

    await wrapper.get('.include-custom input').setValue(true)
    await wrapper.get('.custom-bar .btn-primary').trigger('click')
    await flushPromises()
    expect(speedtestApi.start).toHaveBeenLastCalledWith(expect.objectContaining({ mode: 'all', custom_target_ids: [9] }))
  })
})
