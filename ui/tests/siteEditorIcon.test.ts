import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import SiteEditor from '@/components/sites/SiteEditor.vue'

const mockSitesApi = vi.hoisted(() => ({
  accounts: vi.fn(),
  iconMeta: vi.fn(),
  iconUrl: vi.fn((siteId: number, rev = 0) => `/api/sites/${siteId}/icon?v=${rev}`),
  dynamicProfiles: vi.fn(),
  uploadIcon: vi.fn(),
  fetchIcon: vi.fn(),
  deleteIcon: vi.fn(),
  update: vi.fn(),
  create: vi.fn(),
  testUpstream: vi.fn(),
  parseMessage: vi.fn(),
}))

const mockEdgeApi = vi.hoisted(() => ({
  list: vi.fn(),
}))

const mockToast = vi.hoisted(() => ({
  success: vi.fn(),
  error: vi.fn(),
  info: vi.fn(),
}))

vi.mock('@/api', () => ({
  sitesApi: mockSitesApi,
  edgeApi: mockEdgeApi,
}))

vi.mock('@/stores/toast', () => ({
  toast: mockToast,
  errorMessage: (err: unknown, fallback = 'error') => (err instanceof Error ? err.message : fallback),
}))

const baseSiteA: any = {
  id: 11,
  name: 'SiteA',
  target_url: 'https://a.example.com',
  ingress_mode: 'path',
  path_prefix: '/a',
  enabled: true,
}

const baseSiteB: any = {
  id: 22,
  name: 'SiteB',
  target_url: 'https://b.example.com',
  ingress_mode: 'path',
  path_prefix: '/b',
  enabled: true,
}

describe('SiteEditor 站点图标 CSP 转换与账号头像预览确认 (v1.8.0)', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockSitesApi.accounts.mockResolvedValue([])
    mockSitesApi.iconMeta.mockResolvedValue({ hasIcon: false, source: 'default' })
    mockSitesApi.dynamicProfiles.mockResolvedValue({ available: true })
    mockEdgeApi.list.mockResolvedValue([])
    mockSitesApi.uploadIcon.mockResolvedValue({ ok: true, source: 'custom', hash: 'abc' })
  })

  it('uploads WebP via FileReader data: URL (never blob:) and isolates site switches mid-upload', async () => {
    const createObjectURLSpy = vi.spyOn(URL, 'createObjectURL').mockImplementation(() => {
      throw new Error('blob: URL must not be used under img-src self data:')
    })

    // Stub Image + Canvas so jsdom can complete data: URL decoding deterministically.
    const imgLoaders: Array<() => void> = []
    const seenImageSrcs: string[] = []
    class FakeImage {
      naturalWidth = 512
      naturalHeight = 512
      onload: (() => void) | null = null
      onerror: (() => void) | null = null
      set src(v: string) {
        seenImageSrcs.push(v)
        imgLoaders.push(() => this.onload?.())
      }
    }
    vi.stubGlobal('Image', FakeImage as unknown as typeof Image)

    const getCtxSpy = vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue({
      drawImage: vi.fn(),
    } as unknown as CanvasRenderingContext2D)
    const toBlobSpy = vi.spyOn(HTMLCanvasElement.prototype, 'toBlob').mockImplementation(cb => {
      cb(new Blob(['png-bytes'], { type: 'image/png' }))
    })

    try {
      const wrapper = mount(SiteEditor, {
        props: {
          open: false,
          site: baseSiteA,
          capabilities: { host_only_available: true, domain_prefix_available: true },
          edgeNodes: [],
        },
        global: { stubs: { teleport: true, SiteAccounts: true } },
      })
      await wrapper.setProps({ open: true })
      await flushPromises()

      const input = wrapper.find('input[type="file"]')
      const webpFile = new File([new Uint8Array([0x52, 0x49, 0x46, 0x46])], 'logo.webp', { type: 'image/webp' })
      Object.defineProperty(input.element, 'files', { value: [webpFile], configurable: true })
      await input.trigger('change')
      await flushPromises()

      // FileReader produced a data: URL and URL.createObjectURL was NEVER called.
      await vi.waitFor(() => {
        expect(seenImageSrcs.length).toBe(1)
      })
      expect(createObjectURLSpy).not.toHaveBeenCalled()
      expect(seenImageSrcs[0]).toMatch(/^data:image\/webp;base64,/)

      // Switch drawer to Site B BEFORE Site A's async conversion/upload completes.
      await wrapper.setProps({ site: baseSiteB })
      await flushPromises()
      imgLoaders[0]?.()
      await flushPromises()

      // Upload MUST still target Site A (11), NOT Site B (22), and Site B stays at default icon!
      expect(mockSitesApi.uploadIcon).toHaveBeenCalledTimes(1)
      expect(mockSitesApi.uploadIcon.mock.calls[0][0]).toBe(11)
      expect(wrapper.find('.icon-badge').text()).toBe('内置默认图标')
    } finally {
      createObjectURLSpy.mockRestore()
      getCtxSpy.mockRestore()
      toBlobSpy.mockRestore()
      vi.unstubAllGlobals()
    }
  })

  it('requires explicit admin confirmation when manual fetch falls back to an account avatar', async () => {
    const sampleDataUrl = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='
    mockSitesApi.fetchIcon.mockResolvedValue({
      ok: true,
      source: 'default',
      fetched_type: 'image/png',
      fetched_kind: 'account_avatar',
      requires_confirm: true,
      preview_data_url: sampleDataUrl,
    })

    const wrapper = mount(SiteEditor, {
      props: {
        open: false,
        site: baseSiteA,
        capabilities: { host_only_available: true, domain_prefix_available: true },
        edgeNodes: [],
      },
      global: { stubs: { teleport: true, SiteAccounts: true } },
    })
    await wrapper.setProps({ open: true })
    await flushPromises()

    const fetchBtn = wrapper.findAll('button').find(b => b.text().includes('抓取源站图标'))
    expect(fetchBtn).toBeTruthy()
    await fetchBtn!.trigger('click')
    await flushPromises()

    // Preview box is rendered using data: URL and uploadIcon has NOT been called yet.
    expect(wrapper.find('.icon-confirm img').attributes('src')).toBe(sampleDataUrl)
    expect(mockSitesApi.uploadIcon).not.toHaveBeenCalled()

    // Clicking "确认设为站标" uploads the confirmed avatar to Site A.
    const confirmBtn = wrapper.findAll('button').find(b => b.text().includes('确认设为站标'))
    expect(confirmBtn).toBeTruthy()
    await confirmBtn!.trigger('click')
    await flushPromises()

    expect(mockSitesApi.uploadIcon).toHaveBeenCalledTimes(1)
    expect(mockSitesApi.uploadIcon.mock.calls[0][0]).toBe(11)
    expect(wrapper.find('.icon-confirm').exists()).toBe(false)
    expect(wrapper.find('.icon-badge').text()).toBe('自定义图标')
  })
})
