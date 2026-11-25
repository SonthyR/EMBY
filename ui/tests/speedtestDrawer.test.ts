import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import SpeedtestDrawer from '@/components/edge/SpeedtestDrawer.vue'

const speedtestApi = vi.hoisted(() => ({
  jobs: vi.fn(),
  start: vi.fn(),
  targets: vi.fn(),
  directTargets: vi.fn(),
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

  it('本机直测支持主机与 HTTPS 从机单行单独直测', async () => {
    speedtestApi.directTargets.mockResolvedValue({
      targets: [
        { node_id: 0, name: '主机', is_master: true, online: true, scheme: 'https', stream_url: '/api/speedtest/direct-stream', size_bytes: 1048576 },
        {
          node_id: 8,
          name: '美西 NAT',
          is_master: false,
          online: true,
          public_url: 'https://node-8.example.com:19090',
          scheme: 'https',
          stream_url: 'https://node-8.example.com:19090/_yundu/st/ticket',
          size_bytes: 1048576,
        },
      ],
    })
    const chunk = new Uint8Array(64 * 1024)
    const fetchSpy = vi.spyOn(globalThis, 'fetch').mockResolvedValue({
      ok: true,
      status: 200,
      body: {
        getReader: () => {
          let readCount = 0
          return {
            read: async () => (readCount++ === 0 ? { done: false, value: chunk } : { done: true, value: undefined }),
          }
        },
      },
    } as unknown as Response)

    try {
      const wrapper = await openDrawer()
      const directTab = wrapper.findAll('.st-tabs button').find(b => b.text().includes('本机直测'))
      expect(directTab).toBeTruthy()
      await directTab!.trigger('click')
      await flushPromises()

      const rows = wrapper.findAll('.direct-table tbody tr')
      expect(rows).toHaveLength(2)
      expect(rows[0].text()).toContain('单独直测')
      expect(rows[1].text()).toContain('单独直测')

      const httpsSingleBtn = rows[1].findAll('button').find(b => b.text().includes('单独直测'))
      await httpsSingleBtn!.trigger('click')
      await flushPromises()

      expect(fetchSpy).toHaveBeenCalledTimes(1)
      expect(fetchSpy).toHaveBeenCalledWith(
        'https://node-8.example.com:19090/_yundu/st/ticket',
        expect.objectContaining({ method: 'GET', credentials: 'omit' }),
      )
      expect(rows[0].text()).toContain('待测')
      expect(rows[1].text()).toContain('完成')
    } finally {
      fetchSpy.mockRestore()
    }
  })
})
