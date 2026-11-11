// Pure helpers ported from the legacy sites page.
import type { IngressCapabilities, Site } from '@/api'

export type IngressMode = 'unset' | 'port' | 'path' | 'host' | 'both'

export function normalizedIngressMode(site: Partial<Site> | null | undefined): IngressMode {
  const mode = String(site?.ingress_mode || '').trim().toLowerCase()
  if (mode === 'unset' || mode === 'port' || mode === 'path' || mode === 'host' || mode === 'both') return mode
  return site && String(site.public_host || '').trim() ? 'host' : 'port'
}

export const INGRESS_LABEL: Record<IngressMode, string> = {
  unset: '入口未配置',
  port: '独立端口',
  path: '路径',
  host: '域名前缀',
  both: '域名前缀（兼容）',
}

function boundedInt(value: unknown, min: number, max: number): number | null {
  const text = String(value ?? '').trim()
  if (!/^\d+$/.test(text)) return null
  const n = Number(text)
  return Number.isSafeInteger(n) && n >= min && n <= max ? n : null
}

/** Address a client puts into its Emby app for this site. */
export function siteAccessAddress(site: Site, capabilities: IngressCapabilities, loc: Pick<Location, 'protocol' | 'hostname' | 'host' | 'port'> = location): string {
  const mode = normalizedIngressMode(site)
  if (mode === 'unset') return ''
  const protocol = capabilities.panel_tls_enabled ? 'https' : loc.protocol.replace(':', '') || 'http'
  if (mode === 'port') {
    const host = loc.hostname || '127.0.0.1'
    const scheme = capabilities.site_listener_scheme === 'https' || capabilities.site_listener_scheme === 'http'
      ? capabilities.site_listener_scheme
      : capabilities.panel_tls_enabled
        ? 'https'
        : 'http'
    const port = boundedInt(site.listen_port, 1, 65535)
    return port === null ? '' : `${scheme}://${host.includes(':') && !host.startsWith('[') ? `[${host}]` : host}:${port}`
  }
  if (mode === 'path') {
    const base = `${loc.protocol || protocol + ':'}//${loc.host || loc.hostname || '127.0.0.1'}`.replace(/\/$/, '')
    const prefix = String(site.path_prefix || '').trim().replace(/^\/+|\/+$/g, '')
    return prefix ? `${base}/${prefix}/` : ''
  }
  const host = String(site.public_host || '').trim()
  const panelPort = boundedInt(capabilities.domain_prefix_port, 1, 65535) || loc.port || (protocol === 'https' ? '443' : '80')
  return host ? `${protocol}://${host}:${panelPort}` : ''
}

export const UA_LABEL: Record<string, string> = {
  infuse: 'Infuse',
  web: 'Web',
  client: '客户端',
  passthrough: '透传',
  custom: '自定义',
}

/** Extract the primary upstream latency from a /diag response. */
export function diagLatency(result: any): { ok: boolean; ms?: number; error?: string } {
  const health = result?.upstreams?.primary ? result.upstreams.primary.health || {} : result?.health || {}
  if (health.status === 'online' && Number.isFinite(Number(health.latency_ms))) return { ok: true, ms: Number(health.latency_ms) }
  return { ok: false, error: health.error || '回源不可用' }
}
