import { api } from './client'
import type {
  AuthCheck,
  ClientFilterConfig,
  DashboardAnalyticsResponse,
  EdgeAutoHTTPSInput,
  EdgeAutoHTTPSStatus,
  EdgeNode,
  EdgeNodeInput,
  MasterNodeInput,
  IP2RegionDBStatus,
  IP2RegionLookupResult,
  IP2RegionStatus,
  ISPProbeSettings,
  ISPRouteSchedulePatch,
  ISPRouteStrategy,
  ISPRoutesResponse,
  LiveShareSession,
  ShareAlertSettings,
  ShareRedeemCode,
  ShareRedeemCodeCreateInput,
  ShareSelfNetwork,
  ShareUser,
  ShareUserInput,
  Site,
  SiteAccount,
  SpeedtestCustomTarget,
  SpeedtestCustomTargetInput,
  SpeedtestDirectReport,
  SpeedtestDirectTarget,
  SpeedtestJob,
  SpeedtestTargetsResponse,
} from './types'

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

export interface RequestLog {
  id: number
  site_id: number
  site_name: string
  resource_category: string
  status_code: number
  client_ip: string
  client_region: string
  inbound_colo: string
  outbound_colo: string
  user_agent: string
  upstream_user_agent: string
  backend_address: string
  method: string
  path: string
  recorded_at_ms: number
}

export const logsApi = {
  list: (filter: Record<string, string | number>) => {
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(filter)) if (value !== '' && value !== undefined) query.set(key, String(value))
    return api.get<{ logs: RequestLog[]; dropped_logs: number }>('/api/request-logs?' + query.toString())
  },
  clear: () => api.del<unknown>('/api/request-logs'),
  clearAssetCache: () => api.del<unknown>('/api/asset-cache'),
}

export const dashboardApi = {
  snapshot: () => api.get<TrafficSnapshot>('/api/dashboard'),
  insights: () => api.get<DashboardInsights>('/api/dashboard-insights'),
  analytics: () => api.get<DashboardAnalyticsResponse>('/api/dashboard-analytics'),
  trends: (siteId: string, range: string) => api.get<TrendResponse>(`/api/dashboard-trends?site_id=${encodeURIComponent(siteId)}&range=${encodeURIComponent(range)}`),
}

export interface MyAccountServer {
  site_id: number
  name: string
  enabled: boolean
  active: boolean
  addresses: string[]
}

export interface MyAccount extends ShareUser {
  servers: MyAccountServer[]
  network?: ShareSelfNetwork
}

export interface MyAccountOrderResponse extends MyAccount {
  revoked: number
  draining: number
}

export interface MyAccountRedeemResponse extends MyAccount {
  redeem_summary: string
}

export const meApi = {
  login: (username: string, password: string) => api.post<{ username: string; share_account: boolean }>('/api/me/login', { username, password }),
  logout: () => api.post<{ logged_out: boolean }>('/api/me/logout'),
  get: () => api.get<MyAccount>('/api/me'),
  reorderSites: (siteIds: number[]) => api.put<MyAccountOrderResponse>('/api/me/sites/order', { site_ids: siteIds }),
  kick: () => api.post<{ revoked: number }>('/api/me/kick'),
  kickSession: (sessionId: string) => api.post<{ revoked: boolean }>(`/api/me/sessions/${encodeURIComponent(sessionId)}/kick`),
  redeem: (code: string) => api.post<MyAccountRedeemResponse>('/api/me/redeem', { code }),
  password: (current: string, next: string) => api.post<{ changed: boolean }>('/api/me/password', { current_password: current, new_password: next }),
}

export const sitesApi = {
  list: () => api.get<Site[]>('/api/sites'),
  capabilities: () => api.get<IngressCapabilities>('/api/ingress-capabilities'),
  accounts: (siteId: number) => api.get<SiteAccount[]>(`/api/sites/${id(siteId)}/accounts`),
  create: (data: Record<string, unknown>) => api.post<Site>('/api/sites', data),
  update: (siteId: number, data: Record<string, unknown>) => api.put<Site>(`/api/sites/${id(siteId)}`, data),
  testUpstream: (targetURL: string) => api.post<any>('/api/upstream-test', { target_url: targetURL }),
  parseMessage: (text: string) => api.post<any>('/api/site-data/parse-message', { text }),
  dynamicProfiles: () => api.get<any>('/api/dynamic-profiles'),
  createAccount: (siteId: number, data: Record<string, unknown>) => api.post<SiteAccount>(`/api/sites/${id(siteId)}/accounts`, data),
  updateAccount: (siteId: number, accountId: number, data: Record<string, unknown>) => api.put<SiteAccount>(`/api/sites/${id(siteId)}/accounts/${id(accountId)}`, data),
  deleteAccount: (siteId: number, accountId: number) => api.del<unknown>(`/api/sites/${id(siteId)}/accounts/${id(accountId)}`),
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
  liveSessions: () => api.get<LiveShareSession[]>('/api/live-sessions'),
  get: (userId: number) => api.get<ShareUser>(`/api/share-users/${id(userId)}`),
  create: (input: ShareUserInput) => api.post<ShareUser>('/api/share-users', input),
  update: (userId: number, input: ShareUserInput) => api.put<ShareUser>(`/api/share-users/${id(userId)}`, input),
  remove: (userId: number) => api.del<{ deleted: boolean }>(`/api/share-users/${id(userId)}`),
  text: (userId: number) => api.get<{ text: string }>(`/api/share-users/${id(userId)}/text`),
  kick: (userId: number) => api.post<{ revoked: number }>(`/api/share-users/${id(userId)}/kick`),
  kickSession: (userId: number, sessionId: string) =>
    api.post<{ revoked: boolean }>(`/api/share-users/${id(userId)}/sessions/${encodeURIComponent(sessionId)}/kick`),
  resetTraffic: (userId: number) => api.post<ShareUser>(`/api/share-users/${id(userId)}/reset-traffic`),
  renew: (userId: number, days: number) => api.post<ShareUser>(`/api/share-users/${id(userId)}/renew`, { days }),
  reorder: (userIds: number[]) => api.put<{ user_ids: number[] }>('/api/share-users/reorder', { user_ids: userIds }),
}

export function shareUserToInput(user: ShareUser, patch: Partial<ShareUserInput> = {}): ShareUserInput {
  return {
    username: user.username,
    password: '',
    route_prefix: user.route_prefix || '',
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

export const edgeApi = {
  list: () => api.get<EdgeNode[]>('/api/edge-nodes'),
  get: (nodeId: number) => api.get<EdgeNode>(`/api/edge-nodes/${id(nodeId)}`),
  create: (input: EdgeNodeInput) => api.post<EdgeNode>('/api/edge-nodes', input),
  update: (nodeId: number, input: EdgeNodeInput) => api.put<EdgeNode>(`/api/edge-nodes/${id(nodeId)}`, input),
  remove: (nodeId: number) => api.del<{ deleted: boolean }>(`/api/edge-nodes/${id(nodeId)}`),
  resetToken: (nodeId: number) => api.post<EdgeNode>(`/api/edge-nodes/${id(nodeId)}/reset-token`),
  resetTraffic: (nodeId: number) => api.post<EdgeNode>(`/api/edge-nodes/${id(nodeId)}/reset-traffic`),
  upgrade: (nodeId: number, version = '') => api.post<EdgeNode>(`/api/edge-nodes/${id(nodeId)}/upgrade`, version ? { version } : {}),
  probeISP: (nodeId: number) => api.post<EdgeNode>(`/api/edge-nodes/${id(nodeId)}/probe-isp`, {}),
  resetCircuit: (nodeId: number) => api.post<EdgeNode>(`/api/edge-nodes/${id(nodeId)}/reset-circuit`, {}),
  reorder: (nodeIds: number[]) => api.put<{ node_ids: number[] }>('/api/edge-nodes/reorder', { node_ids: nodeIds }),
}

export const masterNodeApi = {
  get: () => api.get<EdgeNode>('/api/master-node'),
  update: (input: MasterNodeInput) => api.put<EdgeNode>('/api/master-node', input),
  resetTraffic: () => api.post<EdgeNode>('/api/master-node/reset-traffic'),
  probe: () => api.post<EdgeNode>('/api/master-node/probe'),
  probeISP: () => api.post<EdgeNode>('/api/master-node/probe-isp'),
}

export const ispProbeApi = {
  get: () => api.get<ISPProbeSettings>('/api/isp-probe-settings'),
  update: (input: Partial<ISPProbeSettings> & { reset_defaults?: boolean }) =>
    api.put<ISPProbeSettings>('/api/isp-probe-settings', input),
}

export const edgeAutoHttpsApi = {
  get: () => api.get<EdgeAutoHTTPSStatus>('/api/edge-auto-https'),
  update: (input: EdgeAutoHTTPSInput) => api.put<EdgeAutoHTTPSStatus>('/api/edge-auto-https', input),
  issue: (force = false) => api.post<EdgeAutoHTTPSStatus>('/api/edge-auto-https/issue', { force }),
  sync: () => api.post<{ synced: number; status: EdgeAutoHTTPSStatus }>('/api/edge-auto-https/sync', {}),
}

export const ispRoutesApi = {
  get: () => api.get<ISPRoutesResponse>('/api/isp-routes'),
  update: (group: string, nodes: { node_id: number; enabled: boolean; max_balance_streams?: number }[]) =>
    api.put<ISPRoutesResponse>('/api/isp-routes', { group, nodes }),
  setStrategy: (group: string, strategy: ISPRouteStrategy | string) =>
    api.put<ISPRoutesResponse>('/api/isp-routes', { group, strategy }),
  setGroupBlock: (group: string, blockConnect: boolean) =>
    api.put<ISPRoutesResponse>('/api/isp-routes', { group, block_connect: blockConnect }),
  setBlockNonMainland: (blockNonMainland: boolean) =>
    api.put<ISPRoutesResponse>('/api/isp-routes', { block_non_mainland: blockNonMainland }),
  setSchedule: (patch: ISPRouteSchedulePatch) =>
    api.put<ISPRoutesResponse>('/api/isp-routes', { schedule_patch: patch }),
}

export const speedtestApi = {
  targets: () => api.get<SpeedtestTargetsResponse>('/api/speedtest/targets'),
  listCustom: () => api.get<{ targets: SpeedtestCustomTarget[] }>('/api/speedtest/custom-targets'),
  createCustom: (input: SpeedtestCustomTargetInput) =>
    api.post<{ id: number; target: SpeedtestCustomTarget }>('/api/speedtest/custom-targets', input),
  updateCustom: (targetId: number, input: SpeedtestCustomTargetInput) =>
    api.put<{ updated: boolean }>(`/api/speedtest/custom-targets/${id(targetId)}`, input),
  removeCustom: (targetId: number) =>
    api.del<{ deleted: boolean }>(`/api/speedtest/custom-targets/${id(targetId)}`),
  start: (req: {
    node_id: number
    mode: 'builtin' | 'apple' | 'custom' | 'all'
    provinces?: string[]
    isps?: string[]
    custom_target_ids?: number[]
  }) => api.post<{ job: SpeedtestJob }>('/api/speedtest/start', req),
  jobs: (nodeId?: number) =>
    api.get<{ jobs: SpeedtestJob[] }>(
      nodeId !== undefined ? `/api/speedtest/jobs?node_id=${encodeURIComponent(String(nodeId))}` : '/api/speedtest/jobs',
    ),
  job: (jobId: string) => api.get<{ job: SpeedtestJob }>(`/api/speedtest/jobs/${encodeURIComponent(jobId)}`),
  cancel: (jobId: string) => api.post<{ canceled: boolean }>(`/api/speedtest/jobs/${encodeURIComponent(jobId)}/cancel`),
  directTargets: (nodeId?: number) =>
    api.get<{ targets: SpeedtestDirectTarget[] }>(
      nodeId !== undefined ? `/api/speedtest/direct-targets?node_id=${encodeURIComponent(String(nodeId))}` : '/api/speedtest/direct-targets',
    ),
  directReportPoll: (reportId: string) =>
    api.get<{ report: SpeedtestDirectReport }>(`/api/speedtest/direct-report/poll?id=${encodeURIComponent(reportId)}`),
}

export const ip2regionApi = {
  status: () => api.get<IP2RegionStatus>('/api/ip2region'),
  update: (version: 'v4' | 'v6') => api.post<{ item: IP2RegionDBStatus; status: IP2RegionStatus }>('/api/ip2region/update', { version }),
  remove: (version: 'v4' | 'v6') => api.del<IP2RegionStatus>(`/api/ip2region?version=${encodeURIComponent(version)}`),
  lookup: (ip: string) => api.post<IP2RegionLookupResult>('/api/ip2region/lookup', { ip }),
}

export const redeemApi = {
  list: () => api.get<ShareRedeemCode[]>('/api/redeem-codes'),
  create: (input: ShareRedeemCodeCreateInput) => api.post<ShareRedeemCode[]>('/api/redeem-codes', input),
  revoke: (codeId: number) => api.post<{ revoked: boolean }>(`/api/redeem-codes/${id(codeId)}/revoke`),
  remove: (codeId: number) => api.del<{ deleted: boolean }>(`/api/redeem-codes/${id(codeId)}`),
  register: (code: string, username: string, password: string) =>
    api.post<{ username: string; route_prefix: string; route_url: string; share_account: boolean }>('/api/redeem/register', { code, username, password }),
}

export const clientFilterApi = {
  get: () => api.get<ClientFilterConfig>('/api/client-filter'),
  update: (mode: string, rules: string[]) => api.put<ClientFilterConfig>('/api/client-filter', { mode, rules }),
}

export const shareAlertsApi = {
  get: () => api.get<ShareAlertSettings>('/api/share-alerts'),
  update: (input: ShareAlertSettings) => api.put<ShareAlertSettings>('/api/share-alerts', input),
}
