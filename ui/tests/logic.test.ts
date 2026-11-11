import { describe, expect, it } from 'vitest'
import { daysUntil, formatBytes } from '../src/utils/format'
import { diagLatency, normalizedIngressMode, siteAccessAddress } from '../src/components/sites/siteLogic'

describe('formatBytes', () => {
  it('rounds bytes and scales units', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1019.6)).toBe('1020 B')
    expect(formatBytes(1536)).toBe('1.50 KB')
    expect(formatBytes(50 * 1024 ** 3)).toBe('50.0 GB')
  })
})

describe('daysUntil', () => {
  const now = new Date(2026, 10, 11, 15, 0, 0)
  it('counts the expiry day as still valid', () => {
    expect(daysUntil('2026-11-11', now)).toBe(0)
    expect(daysUntil('2026-11-12', now)).toBe(1)
    expect(daysUntil('2026-11-10', now)).toBe(-1)
    expect(daysUntil('', now)).toBeNull()
  })
})

describe('site access address', () => {
  const loc = { protocol: 'https:', hostname: 'panel.example.com', host: 'panel.example.com', port: '' }
  it('builds path, port and host ingress addresses', () => {
    expect(siteAccessAddress({ ingress_mode: 'path', path_prefix: '/cinema/' } as any, {}, loc)).toBe('https://panel.example.com/cinema/')
    expect(siteAccessAddress({ ingress_mode: 'port', listen_port: 18097 } as any, { site_listener_scheme: 'http' }, loc)).toBe('http://panel.example.com:18097')
    expect(siteAccessAddress({ ingress_mode: 'host', public_host: 'a.example.com' } as any, { panel_tls_enabled: true, domain_prefix_port: 8443 }, loc)).toBe('https://a.example.com:8443')
    expect(siteAccessAddress({ ingress_mode: 'unset' } as any, {}, loc)).toBe('')
  })
  it('infers legacy ingress modes', () => {
    expect(normalizedIngressMode({ public_host: 'x.example.com' } as any)).toBe('host')
    expect(normalizedIngressMode({} as any)).toBe('port')
  })
})

describe('diagLatency', () => {
  it('reads the primary upstream health', () => {
    expect(diagLatency({ upstreams: { primary: { health: { status: 'online', latency_ms: 42 } } } })).toEqual({ ok: true, ms: 42 })
    expect(diagLatency({ health: { status: 'offline', error: 'timeout' } })).toEqual({ ok: false, error: 'timeout' })
  })
})
