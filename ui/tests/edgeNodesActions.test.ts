import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import EdgeNodesView from '@/views/EdgeNodesView.vue'
import type { EdgeNode } from '@/api'

const edgeApi = vi.hoisted(() => ({
  list: vi.fn(),
  get: vi.fn(),
  upgrade: vi.fn(),
}))

const masterNodeApi = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
  probe: vi.fn(),
  resetTraffic: vi.fn(),
}))

const edgeAutoHttpsApi = vi.hoisted(() => ({
  get: vi.fn(),
  update: vi.fn(),
  issue: vi.fn(),
  sync: vi.fn(),
}))

const confirmMock = vi.hoisted(() => vi.fn())
const copyTextMock = vi.hoisted(() => vi.fn())

vi.mock('@/api', () => ({ edgeApi, masterNodeApi, edgeAutoHttpsApi }))
vi.mock('@/stores/confirm', () => ({ confirm: confirmMock }))
vi.mock('@/utils/format', async importOriginal => {
  const actual = await importOriginal<typeof import('@/utils/format')>()
  return { ...actual, copyText: copyTextMock }
})

enableAutoUnmount(afterEach)

function makeMasterNode(overrides: Partial<EdgeNode> = {}): EdgeNode {
  return {
    id: 0,
    is_master: true,
    name: '主机',
    public_port: 0,
    listen_port: 0,
    public_host: '',
    public_ip: '',
    detected_ip: '',
    tls_mode: '',
    enabled: true,
    traffic_quota: 0,
    traffic_used: 512 * 1024 * 1024,
    traffic_reset_day: 0,
    note: '',
    agent_version: 'v1.5.0',
    master_version: 'v1.5.0',
    upgrade_available: false,
    upgrade_status: '',
    last_seen_at: new Date().toISOString(),
    online: true,
    tls_active: false,
    active_streams: 1,
    current_bps: 12_000_000,
    memory_bytes: 42 * 1024 * 1024,
    uptime_sec: 3600,
    upstream_latencies: [{ site_name: '主力站', latency_ms: 18 }],
    public_url: 'https://panel.example.com',
    status: 'online',
    created_at: '',
    updated_at: '',
    ...overrides,
  }
}

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
    master_version: 'v1.5.0',
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
  masterNodeApi.get.mockResolvedValue(makeMasterNode())
  masterNodeApi.probe.mockResolvedValue(makeMasterNode({ upstream_latencies: [{ site_name: '主力站', latency_ms: 12 }] }))
  masterNodeApi.resetTraffic.mockResolvedValue(makeMasterNode({ traffic_used: 0 }))
  masterNodeApi.update.mockResolvedValue(makeMasterNode({ name: '美国主机' }))
  edgeAutoHttpsApi.get.mockResolvedValue({
    enabled: true,
    base_domain: 'example.com',
    wildcard_domain: '*.example.com',
    subdomain_prefix: 'node',
    acme_email: 'ops@example.com',
    token_configured: true,
    cert_configured: true,
    cert_valid: true,
    cert_matches_domain: true,
    cert_days_remaining: 80,
    issuing: false,
    auto_nodes_count: 1,
    synced_nodes_count: 1,
  })
})

describe('EdgeNodesView 节点总览、批量升级与卸载命令复制', () => {
  it('顶部全部升级按钮仅统计并升级在线且支持远程更新的旧版从机，不计入主机', async () => {
    const nodes = [
      makeNode({ id: 1, name: '香港 CN2', agent_version: 'v1.4.5', online: true, upgrade_available: true }),
      makeNode({ id: 2, name: '日本移动', agent_version: 'v1.4.6', online: true, upgrade_available: true }),
      makeNode({ id: 3, name: '离线从机', agent_version: 'v1.4.5', online: false, status: 'offline', upgrade_available: true }),
      makeNode({ id: 4, name: '远古版本', agent_version: 'v1.1.0', online: true, upgrade_available: true }),
      makeNode({ id: 5, name: '升级中从机', agent_version: 'v1.4.5', online: true, upgrade_available: true, upgrade_status: 'updating' }),
    ]
    edgeApi.list.mockResolvedValue(nodes)

    const wrapper = mount(EdgeNodesView, { global: { stubs: { Teleport: true, SpeedtestDrawer: true, RouterLink: true } } })
    await flushPromises()

    expect(wrapper.text()).toContain('1 主 · 5 从')
    const cards = wrapper.findAll('article.card')
    expect(cards[0].classes()).toContain('master')
    expect(cards[0].text()).toContain('本机运行')
    expect(cards[0].text()).not.toContain('安装命令')

    const batchBtn = wrapper.findAll('button').find(b => b.text().includes('全部升级'))
    expect(batchBtn).toBeTruthy()
    expect(batchBtn!.text()).toContain('全部升级（2）')

    await batchBtn!.trigger('click')
    await flushPromises()

    expect(confirmMock).toHaveBeenCalledTimes(1)
    expect(edgeApi.upgrade).toHaveBeenCalledTimes(2)
    expect(edgeApi.upgrade).toHaveBeenCalledWith(1, 'v1.5.0')
    expect(edgeApi.upgrade).toHaveBeenCalledWith(2, 'v1.5.0')
  })

  it('主机卡片支持刷新延迟、编辑配置与清零本期流量', async () => {
    edgeApi.list.mockResolvedValue([])
    const wrapper = mount(EdgeNodesView, { global: { stubs: { Teleport: true, SpeedtestDrawer: true, RouterLink: true } } })
    await flushPromises()

    expect(wrapper.text()).toContain('1 主 · 0 从')
    expect(wrapper.text()).toContain('主力站 18ms')

    const probeBtn = wrapper.findAll('button').find(b => b.text().includes('刷新延迟'))
    expect(probeBtn).toBeTruthy()
    await probeBtn!.trigger('click')
    await flushPromises()
    expect(masterNodeApi.probe).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('主力站 12ms')

    const resetBtn = wrapper.find('article.card.master button[title="清零本期流量"]')
    expect(resetBtn.exists()).toBe(true)
    await resetBtn.trigger('click')
    await flushPromises()
    expect(masterNodeApi.resetTraffic).toHaveBeenCalledTimes(1)

    const editBtn = wrapper.findAll('article.card.master button').find(b => b.text().includes('编辑'))
    expect(editBtn).toBeTruthy()
    await editBtn!.trigger('click')
    await flushPromises()

    await wrapper.find('#mn-name').setValue('美国主机')
    const saveBtn = wrapper.findAll('button').find(b => b.text().includes('保存更改'))
    expect(saveBtn).toBeTruthy()
    await saveBtn!.trigger('click')
    await flushPromises()

    expect(masterNodeApi.update).toHaveBeenCalledWith({
      name: '美国主机',
      traffic_quota: 0,
      traffic_reset_day: 0,
      max_balance_streams: 2,
      note: '',
    })
    expect(wrapper.find('article.card.master h3').text()).toBe('美国主机')
  })

  it('安装命令弹窗展示卸载命令并支持一键复制卸载命令', async () => {
    const node = makeNode({ id: 7, name: '新加坡节点', agent_version: 'v1.5.0', upgrade_available: false })
    edgeApi.list.mockResolvedValue([node])
    edgeApi.get.mockResolvedValue({
      ...node,
      install_command: 'bash <(curl -fsSL https://example.com/install.sh) agent --id 7',
      uninstall_command: 'bash <(curl -fsSL https://raw.githubusercontent.com/SonthyR/EMBY/master/install.sh) agent --uninstall',
    })

    const wrapper = mount(EdgeNodesView, { global: { stubs: { Teleport: true, SpeedtestDrawer: true, RouterLink: true } } })
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
