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

export interface IngressCapabilities {
  panel_tls_enabled?: boolean
  site_listener_scheme?: string
  domain_prefix_port?: number
  app_version?: string
  [key: string]: unknown
}

export interface AccountReminderRow {
  site_id: number
  site_name: string
  account: SiteAccount & { reminder_days?: number | null }
  days_left: number
  status: string
  reminder_enabled: boolean
}

export interface ReminderRecord {
  account_id: number
  site_name: string
  account_name: string
  expires_at: string
  reminder_days: number
  status: 'sent' | 'failed'
  error?: string
  attempted_at: string
  sent_at?: string
  action?: string
}

export interface ImportPreviewItem {
  index: number
  name: string
  target_url: string
  account_count: number
  duplicate_names: string[]
  action: 'new' | 'update' | 'skip'
  ingress_mode: string
  path_prefix?: string
  public_host?: string
  listen_port: number
  error?: string
}

export interface ImportPreview {
  items: ImportPreviewItem[]
  valid: boolean
  preview_token: string
}

export interface ImportResult {
  created_sites: number
  updated_sites: number
  skipped_sites: number
  failed_sites: number
  items: { name: string; error?: string }[]
}

export interface SiteTraffic {
  id: number
  name: string
  running: boolean
  traffic_quota: number
  traffic_used: number
  monthly_traffic: number
  requests: number
}
export interface TrafficSnapshot {
  total_sites: number
  running_sites: number
  total_traffic: number
  monthly_traffic: number
  billing_mode: string
  traffic_reset_day: number
  total_requests: number
  uptime_seconds: number
  live_sites: SiteTraffic[]
}
export interface TrendPoint {
  timestamp_ms: number
  traffic_bytes: number
  bytes_in: number
  bytes_out: number
  requests: number
  download_bps: number
  upload_bps: number
}
export interface TrendResponse {
  range: string
  bucket_seconds: number
  points: TrendPoint[]
}
export interface DashboardInsights {
  log_healthy: boolean
  latest_log_ms: number
  log_count_today: number
  dropped_logs: number
  schedule_enabled: boolean
  schedule_label: string
  hourly_requests: number[]
}

export const dashboardApi = {
  snapshot: () => api.get<TrafficSnapshot>('/api/dashboard'),
  insights: () => api.get<DashboardInsights>('/api/dashboard-insights'),
  trends: (siteId: string, range: string) => api.get<TrendResponse>(`/api/dashboard-trends?site_id=${encodeURIComponent(siteId)}&range=${encodeURIComponent(range)}`),
}

export interface MyAccount extends ShareUser {
  servers: { name: string; addresses: string[] }[]
}

export const meApi = {
  login: (username: string, password: string) => api.post<{ username: string; share_account: boolean }>('/api/me/login', { username, password }),
  logout: () => api.post<{ logged_out: boolean }>('/api/me/logout'),
  get: () => api.get<MyAccount>('/api/me'),
  kick: () => api.post<{ revoked: number }>('/api/me/kick'),
  password: (current: string, next: string) => api.post<{ changed: boolean }>('/api/me/password', { current_password: current, new_password: next }),
}

export const sitesApi = {
  list: () => api.get<Site[]>('/api/sites'),
  capabilities: () => api.get<IngressCapabilities>('/api/ingress-capabilities'),
  accounts: (siteId: number) => api.get<SiteAccount[]>(`/api/sites/${id(siteId)}/accounts`),
  toggle: (siteId: number) => api.post<{ enabled: boolean }>(`/api/sites/${id(siteId)}/toggle`),
  remove: (siteId: number) => api.del<unknown>(`/api/sites/${id(siteId)}`),
  reorder: (siteIds: number[]) => api.put<unknown>('/api/sites/reorder', { site_ids: siteIds }),
  diag: (siteId: number) => api.get<any>(`/api/sites/${id(siteId)}/diag`),
  reminders: (status = '') => api.get<AccountReminderRow[]>('/api/account-reminders' + (status ? `?status=${encodeURIComponent(status)}` : '')),
  reminderHistory: () => api.get<ReminderRecord[]>('/api/account-reminders/history'),
  reminderAction: (record: ReminderRecord, action: 'handled' | 'ignored' | 'snoozed') =>
    api.post<unknown>('/api/account-reminders/actions', { account_id: record.account_id, expires_at: record.expires_at, reminder_days: record.reminder_days, action }),
  importPreview: (format: string, data: string, duplicateAction: string, actions: Record<string, string>) =>
    api.post<ImportPreview>('/api/site-data/import', { format, data, preview: true, duplicate_action: duplicateAction, actions }),
  importConfirm: (format: string, data: string, duplicateAction: string, actions: Record<string, string>, previewToken: string) =>
    api.post<ImportResult>('/api/site-data/import', { format, data, duplicate_action: duplicateAction, actions, preview_token: previewToken }),
  async export(format: string, mode: string, includeAccounts: boolean, includePasswords: boolean): Promise<Blob> {
    const query = new URLSearchParams({ format, include_accounts: String(includeAccounts), include_passwords: String(includePasswords) })
    if (mode) query.set('line_mode', mode)
    const response = await fetch('/api/site-data/export?' + query.toString(), { credentials: 'same-origin' })
    if (!response.ok) {
      let message = '导出失败'
      try {
        message = (await response.json()).error || message
      } catch {
        /* keep default */
      }
      throw new Error(message)
    }
    return response.blob()
  },
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
