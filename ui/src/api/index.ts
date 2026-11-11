import { api } from './client'
import type { AuthCheck, ShareUser, ShareUserInput, Site, SiteAccount } from './types'

export * from './client'
export * from './types'

const id = (value: number) => encodeURIComponent(String(value))

export const authApi = {
  check: () => api.get<AuthCheck>('/api/auth/check'),
  login: (username: string, password: string) => api.post<{ username: string }>('/api/auth/login', { username, password }),
  setup: (username: string, password: string, setupToken: string) =>
    api.post<{ username: string }>('/api/auth/setup', { username, password, setup_token: setupToken }),
  logout: () => api.post<{ logged_out: boolean }>('/api/auth/logout'),
}

export const sitesApi = {
  list: () => api.get<Site[]>('/api/sites'),
  accounts: (siteId: number) => api.get<SiteAccount[]>(`/api/sites/${id(siteId)}/accounts`),
}

export const shareApi = {
  list: () => api.get<ShareUser[]>('/api/share-users'),
  get: (userId: number) => api.get<ShareUser>(`/api/share-users/${id(userId)}`),
  create: (input: ShareUserInput) => api.post<ShareUser>('/api/share-users', input),
  update: (userId: number, input: ShareUserInput) => api.put<ShareUser>(`/api/share-users/${id(userId)}`, input),
  remove: (userId: number) => api.del<{ deleted: boolean }>(`/api/share-users/${id(userId)}`),
  text: (userId: number) => api.get<{ text: string }>(`/api/share-users/${id(userId)}/text`),
  kick: (userId: number) => api.post<{ revoked: number }>(`/api/share-users/${id(userId)}/kick`),
  resetTraffic: (userId: number) => api.post<ShareUser>(`/api/share-users/${id(userId)}/reset-traffic`),
  resetPortal: (userId: number) => api.post<ShareUser>(`/api/share-users/${id(userId)}/reset-portal`),
  renew: (userId: number, days: number) => api.post<ShareUser>(`/api/share-users/${id(userId)}/renew`, { days }),
}

export function shareUserToInput(user: ShareUser, patch: Partial<ShareUserInput> = {}): ShareUserInput {
  return {
    username: user.username,
    password: '',
    note: user.note || '',
    enabled: user.enabled,
    expires_at: user.expires_at || '',
    traffic_quota: user.traffic_quota || 0,
    max_devices: user.max_devices || 0,
    speed_limit: user.speed_limit || 0,
    traffic_reset_day: user.traffic_reset_day || 0,
    sites: user.sites.map(site => ({ site_id: site.site_id, site_account_id: site.site_account_id || 0 })),
    ...patch,
  }
}
