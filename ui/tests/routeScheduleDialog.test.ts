import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RouteScheduleDialog from '@/components/edge/RouteScheduleDialog.vue'
import type { ISPRouteGroup, ISPRouteNode } from '@/api'

const mockRoutesApi = vi.hoisted(() => ({
  setSchedule: vi.fn(),
}))

vi.mock('@/api', () => ({
  ispRoutesApi: {
    setSchedule: mockRoutesApi.setSchedule,
  },
}))

vi.mock('@/stores/toast', () => ({
  toast: { success: vi.fn(), error: vi.fn() },
  errorMessage: (_err: unknown, fallback = 'error') => fallback,
}))

describe('RouteScheduleDialog 线路节点时段/避峰策略 (v1.5.5)', () => {
  const natNode: ISPRouteNode = {
    node_id: 2,
    name: '懒猫美国NAT',
    is_master: false,
    enabled: true,
    online: true,
    status: 'online',
    active: true,
    schedule_mode: 'always',
    max_balance_streams: 2,
    active_sessions: 0,
  }

  const allGroups: ISPRouteGroup[] = [
    { group: 'ct', label: '电信', strategy: 'priority', block_connect: false, nodes: [natNode] },
    { group: 'cu', label: '联通', strategy: 'priority', block_connect: false, nodes: [natNode] },
    { group: 'cm', label: '移动', strategy: 'priority', block_connect: false, nodes: [natNode] },
    { group: 'other', label: '其他', strategy: 'priority', block_connect: false, nodes: [natNode] },
  ]

  beforeEach(() => {
    vi.clearAllMocks()
    mockRoutesApi.setSchedule.mockResolvedValue({
      block_non_mainland: false,
      groups: allGroups,
    })
  })

  it('支持切换避峰模式、选择快捷时段并同步应用到多个运营商分组', async () => {
    const wrapper = mount(RouteScheduleDialog, {
      props: {
        open: true,
        groupKey: 'cu',
        groupLabel: '联通',
        node: natNode,
        allGroups,
      },
      global: {
        stubs: {
          Teleport: true,
        },
      },
    })
    await flushPromises()

    const avoidRadio = wrapper.find('input[type="radio"][value="avoid"]')
    await avoidRadio.setValue(true)

    const presetBtn = wrapper.findAll('.preset-chip').find(b => b.text().includes('20:00–00:00'))
    expect(presetBtn).toBeTruthy()
    await presetBtn!.trigger('click')

    // Also check 移动 group to sync both 联通 + 移动 in one save.
    const cmCheckbox = wrapper.findAll('.group-check').find(l => l.text().includes('移动'))?.find('input[type="checkbox"]')
    expect(cmCheckbox).toBeTruthy()
    await cmCheckbox!.setValue(true)

    const saveBtn = wrapper.findAll('button').find(b => b.text().includes('保存策略'))
    expect(saveBtn).toBeTruthy()
    await saveBtn!.trigger('click')
    await flushPromises()

    expect(mockRoutesApi.setSchedule).toHaveBeenCalledWith({
      node_id: 2,
      groups: ['cu', 'cm'],
      schedule_mode: 'avoid',
      schedule_start: '20:00',
      schedule_end: '00:00',
    })
    expect(wrapper.emitted('saved')).toBeTruthy()
    expect(wrapper.emitted('close')).toBeTruthy()
  })
})
