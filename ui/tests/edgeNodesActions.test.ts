import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import EdgeNodesView from '@/views/EdgeNodesView.vue'
import type { EdgeNode } from '@/api'

const edgeApi = vi.hoisted(() => ({
  list: vi.fn(),
  get: vi.fn(),
  upgrade: vi.fn(),
}))

const confirmMock = vi.hoisted(() => vi.fn())
const copyTextMock = vi.hoisted(() => vi.fn())

vi.mock('@/api', () => ({ edgeApi }))
vi.mock('@/stores/confirm', () => ({ confirm: confirmMock }))
vi.mock('@/utils/format', async importOriginal => {
  const actual = await importOriginal<typeof import('@/utils/format')>()
  return { ...actual, copyText: copyTextMock }
})

enableAutoUnmount(afterEach)

function makeNode(overrides: Partial<EdgeNode>): EdgeNode {
  return {
    id: 1,
    name: '香港节点',
    public_port: 19090,
    listen_port: 19090,
    public_host: '',
    public_ip: '',
    detected_ip: '1.2.3.4',
    tls_mode: 'http',
    enabled: true,
    traffic_quota: 0,
    traffic_used: 0,
    traffic_reset_day: 0,
    note: '',
    agent_version: 'v1.4.5',
    master_version: 'v1.4.7',
    upgrade_available: true,
    upgrade_status: '',
    last_seen_at: new Date().toISOString(),
    online: true,
    tls_active: false,
    active_streams: 0,
    current_bps: 0,
    memory_bytes: 0,
    uptime_sec: 120,
    public_url: 'http://1.2.3.4:19090',
    status: 'online',
    created_at: '',
    updated_at: '',
    ...overrides,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  confirmMock.mockResolvedValue(true)
  copyTextMock.mockResolvedValue(true)
  edgeApi.upgrade.mockResolvedValue({})
})

describe('EdgeNodesView 批量升级与卸载命令复制', () => {
  it('顶部全部升级按钮仅统计并升级在线且支持远程更新的旧版从机', async () => {
    const nodes = [
      makeNode({ id: 1, name: '香港 CN2', agent_version: 'v1.4.5', online: true, upgrade_available: true }),
      makeNode({ id: 2, name: '日本移动', agent_version: 'v1.4.6', online: true, upgrade_available: true }),
      makeNode({ id: 3, name: '离线从机', agent_version: 'v1.4.5', online: false, status: 'offline', upgrade_available: true }),
      makeNode({ id: 4, name: '远古版本', agent_version: 'v1.1.0', online: true, upgrade_available: true }),
      makeNode({ id: 5, name: '升级中从机', agent_version: 'v1.4.5', online: true, upgrade_available: true, upgrade_status: 'updating' }),
    ]
    edgeApi.list.mockResolvedValue(nodes)

    const wrapper = mount(EdgeNodesView, { global: { stubs: { Teleport: true, SpeedtestDrawer: true } } })
    await flushPromises()

    const batchBtn = wrapper.findAll('button').find(b => b.text().includes('全部升级'))
    expect(batchBtn).toBeTruthy()
    expect(batchBtn!.text()).toContain('全部升级（2）')

    await batchBtn!.trigger('click')
    await flushPromises()

    expect(confirmMock).toHaveBeenCalledTimes(1)
    expect(edgeApi.upgrade).toHaveBeenCalledTimes(2)
    expect(edgeApi.upgrade).toHaveBeenCalledWith(1, 'v1.4.7')
    expect(edgeApi.upgrade).toHaveBeenCalledWith(2, 'v1.4.7')
  })

  it('安装命令弹窗展示卸载命令并支持一键复制卸载命令', async () => {
    const node = makeNode({ id: 7, name: '新加坡节点', agent_version: 'v1.4.7', upgrade_available: false })
    edgeApi.list.mockResolvedValue([node])
    edgeApi.get.mockResolvedValue({
      ...node,
      install_command: 'bash <(curl -fsSL https://example.com/install.sh) agent --id 7',
      uninstall_command: 'bash <(curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh) agent --uninstall',
    })

    const wrapper = mount(EdgeNodesView, { global: { stubs: { Teleport: true, SpeedtestDrawer: true } } })
    await flushPromises()

    const installBtn = wrapper.findAll('button').find(b => b.text().includes('安装命令'))
    expect(installBtn).toBeTruthy()
    await installBtn!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('卸载该从机')
    expect(wrapper.text()).toContain('agent --uninstall')

    const copyUninstallBtn = wrapper.findAll('button').find(b => b.text().includes('复制卸载命令'))
    expect(copyUninstallBtn).toBeTruthy()
    await copyUninstallBtn!.trigger('click')
    await flushPromises()

    expect(copyTextMock).toHaveBeenCalledWith(
      'bash <(curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh) agent --uninstall',
    )
  })
})
