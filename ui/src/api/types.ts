export interface AuthCheck {
  needs_setup: boolean
  authenticated?: boolean
  username?: string
  jwt_secret_ephemeral?: boolean
  mode?: string
}

export interface Site {
  id: number
  name: string
  enabled: boolean
  running?: boolean
  target_url: string
  ingress_mode: string
  listen_port: number
  public_host: string
  path_prefix: string
  edge_routing_mode?: 'global' | 'master' | string
  traffic_used?: number
  monthly_traffic?: number
  [key: string]: unknown
}

export interface SiteAccount {
  id: number
  site_id: number
  account_name: string
  username: string
  password?: string
  password_configured?: boolean
  security_password?: string
  expires_at: string
  note?: string
  reminder_days?: number | null
}

export interface ShareUserSite {
  site_id: number
  site_account_id: number
  site_name?: string
  account_name?: string
  site_enabled?: boolean
  sort_order?: number
  active?: boolean
}

export interface ShareSession {
  session_id?: string
  site_id: number
  site_name: string
  device_name: string
  client: string
  client_ip: string
  client_region?: string
  node_name?: string
  created_at: string
  last_seen_at: string
  draining?: boolean
  playing?: boolean
}

export interface LiveShareSession {
  session_id: string
  share_user_id: number
  username: string
  site_id: number
  site_name: string
  device_name: string
  client: string
  client_ip: string
  client_region?: string
  node_name?: string
  playing?: boolean
  draining?: boolean
  created_at?: string
  last_seen_at: string
}

export interface ShareSelfNetwork {
  client_ip: string
  region?: string
  isp?: string
  isp_group?: string
  isp_label?: string
  non_mainland_cn?: boolean
  node_name?: string
  blocked?: boolean
  block_reason?: 'non_mainland' | 'group' | string
}

export type ShareStatus = 'active' | 'disabled' | 'expired' | 'quota'

export interface ShareUser {
  id: number
  username: string
  password?: string
  route_prefix?: string
  route_url?: string
  note: string
  enabled: boolean
  expires_at: string
  traffic_quota: number
  traffic_used: number
  status: ShareStatus
  session_count: number
  draining_sessions?: number
  max_devices: number
  speed_limit: number
  traffic_reset_day: number
  next_reset_at?: string
  active_site_id?: number
  active_site_name?: string
  sites: ShareUserSite[]
  sessions?: ShareSession[]
  created_at: string
  updated_at: string
}

export interface ShareUserInput {
  username: string
  password: string
  route_prefix: string
  note: string
  enabled: boolean
  expires_at: string
  traffic_quota: number
  max_devices: number
  speed_limit: number
  traffic_reset_day: number
  sites: { site_id: number; site_account_id: number }[]
}

export type EdgeNodeStatus = 'online' | 'offline' | 'disabled' | 'quota' | 'waiting'
export type EdgeNodeTLSMode = 'auto' | 'external' | 'http'

export interface EdgeNodeSpeedMetric {
  isp_group: 'ct' | 'cu' | 'cm' | 'other' | string
  isp_label: string
  avg_return_bps: number
  avg_return_mbps: number
  avg_outbound_bps: number
  avg_outbound_mbps: number
  avg_return_rtt_ms: number
  avg_outbound_rtt_ms: number
  total_retrans: number
  sample_count: number
  tested_at: string
}

export interface EdgeSiteLatency {
  site_name: string
  latency_ms: number
}

export interface EdgeNode {
  id: number
  name: string
  public_port: number
  listen_port: number
  public_host: string
  public_ip: string
  detected_ip: string
  tls_mode: EdgeNodeTLSMode | string
  enabled: boolean
  traffic_quota: number
  traffic_used: number
  traffic_reset_day: number
  next_reset_at?: string
  note: string
  agent_version: string
  master_version?: string
  upgrade_available?: boolean
  upgrade_target?: string
  upgrade_status?: string
  last_seen_at: string
  online: boolean
  tls_active: boolean
  active_streams: number
  current_bps: number
  memory_bytes?: number
  uptime_sec?: number
  upstream_latencies?: EdgeSiteLatency[]
  speed_metrics?: EdgeNodeSpeedMetric[]
  public_url: string
  status: EdgeNodeStatus
  install_command?: string
  uninstall_command?: string
  created_at: string
  updated_at: string
}

export interface EdgeNodeInput {
  name: string
  public_port: number
  listen_port?: number
  public_host?: string
  public_ip?: string
  tls_mode?: EdgeNodeTLSMode | string
  enabled?: boolean
  traffic_quota?: number
  traffic_reset_day?: number
  note?: string
}

export type ISPRouteStrategy = 'priority' | 'auto' | 'balance'

export interface ISPRouteNode {
  node_id: number
  name: string
  is_master: boolean
  enabled: boolean
  online: boolean
  status: EdgeNodeStatus | string
  active: boolean
  public_url?: string
  speed_metric?: EdgeNodeSpeedMetric
}

export interface ISPRouteGroup {
  group: 'ct' | 'cu' | 'cm' | 'other' | string
  label: string
  strategy: ISPRouteStrategy | string
  block_connect: boolean
  nodes: ISPRouteNode[]
}

export interface ISPRoutesResponse {
  block_non_mainland: boolean
  groups: ISPRouteGroup[]
}

export interface IP2RegionDBStatus {
  version: 'v4' | 'v6' | string
  downloaded: boolean
  size_bytes: number
  updated_at?: string
}

export interface IP2RegionStatus {
  v4: IP2RegionDBStatus
  v6: IP2RegionDBStatus
  mirrors: string[]
}

export interface IP2RegionLookupResult {
  ip: string
  region: string
  isp: string
  isp_group: string
  isp_label: string
  non_mainland_cn?: boolean
  node_id: number
  node_name: string
  public_url?: string
  blocked?: boolean
  block_reason?: 'non_mainland' | 'group' | string
}

export type RedeemCodeKind = 'renew' | 'register'
export type RedeemCodeStatus = 'unused' | 'used' | 'revoked' | 'expired'

export interface ShareRedeemCode {
  id: number
  code: string
  kind: RedeemCodeKind | string
  days: number
  traffic_quota: number
  traffic_reset_day: number
  max_devices: number
  speed_limit: number
  site_ids: number[]
  site_names?: string[]
  expires_at: string
  note: string
  status: RedeemCodeStatus | string
  used_by_user_id?: number
  used_by_username?: string
  used_at?: string
  created_at: string
}

export interface ShareRedeemCodeCreateInput {
  kind: RedeemCodeKind
  count: number
  days: number
  traffic_quota: number
  traffic_reset_day: number
  max_devices: number
  speed_limit: number
  site_ids: number[]
  expires_at: string
  note: string
}

export type ClientFilterMode = 'off' | 'blacklist' | 'whitelist'

export interface ClientFilterConfig {
  mode: ClientFilterMode | string
  rules: string[]
  blacklist_presets?: string[]
  whitelist_presets?: string[]
}

export interface ShareAlertSettings {
  enabled: boolean
  expiry_days: number
  quota_warning_pct: number
}

export interface SpeedtestProvince {
  code: string
  label: string
}

export interface SpeedtestISP {
  group: string
  label: string
}

export interface SpeedtestNodeSummary {
  id: number
  name: string
  is_master: boolean
  online: boolean
  status: string
  public_url?: string
  detected_ip?: string
  speed_metrics?: EdgeNodeSpeedMetric[]
}

export interface SpeedtestCustomTarget {
  id: number
  name: string
  isp_group: string
  isp_label: string
  province: string
  upload_url: string
  download_url: string
  method: string
  enabled: boolean
  created_at?: string
}

export interface SpeedtestCustomTargetInput {
  name: string
  isp_group: string
  province?: string
  upload_url?: string
  download_url?: string
  method?: string
  enabled?: boolean
}

export interface SpeedtestTargetsResponse {
  provinces: SpeedtestProvince[]
  isps: SpeedtestISP[]
  default_provinces: string[]
  default_isps: string[]
  custom_targets: SpeedtestCustomTarget[]
  nodes: SpeedtestNodeSummary[]
  catalog_count?: number
  catalog_updated_at?: string
}

export interface SpeedtestTargetResult {
  target_key: string
  label: string
  province: string
  isp_group: string
  isp_label: string
  target_kind: 'builtin' | 'apple' | 'custom' | string
  resolved_node?: string
  resolved_ip?: string
  status: 'pending' | 'running' | 'done' | 'error' | string
  error?: string
  return_mbps: number
  return_bps: number
  outbound_mbps: number
  outbound_bps: number
  return_retrans: number
  return_rtt_ms: number
  outbound_rtt_ms: number
  tested_at?: string
}

export interface SpeedtestJob {
  id: string
  node_id: number
  node_name: string
  mode: 'builtin' | 'apple' | 'custom' | 'all' | string
  provinces: string[]
  isps: string[]
  status: 'pending' | 'running' | 'done' | 'failed' | 'canceled' | string
  total_targets: number
  completed_targets: number
  results: SpeedtestTargetResult[]
  error?: string
  created_at: string
  updated_at: string
}

export interface SpeedtestDirectTarget {
  node_id: number
  name: string
  is_master: boolean
  online: boolean
  public_url?: string
  scheme: string
  stream_url: string
  popup_url?: string
  report_id?: string
  supports_popup?: boolean
  size_bytes: number
  unavailable_reason?: string
}

export interface SpeedtestDirectReport {
  id: string
  node_id: number
  status: 'idle' | 'running' | 'done' | 'error' | string
  ttfb_ms: number
  mbps: number
  received_bytes: number
  error?: string
  terminal?: boolean
}

export interface DashboardAnalyticsSummary {
  total_traffic: number
  monthly_traffic: number
  master_traffic: number
  edge_traffic: number
  offload_percent: number
  total_requests: number
  active_streams: number
  online_sessions: number
  share_user_count: number
  active_edge_nodes: number
  total_edge_nodes: number
}

export interface AnalyticsNodeTrafficItem {
  node_id: number
  name: string
  is_master: boolean
  online: boolean
  status: string
  active_streams: number
  current_bps: number
  traffic_used: number
  traffic_quota: number
  share_percent: number
}

export interface AnalyticsShareUserItem {
  id: number
  username: string
  note: string
  status: string
  traffic_used: number
  traffic_quota: number
  session_count: number
  playing_count: number
  share_percent: number
}

export interface AnalyticsSiteTrafficItem {
  id: number
  name: string
  running: boolean
  traffic_used: number
  monthly_traffic: number
  cumulative_bytes_in: number
  cumulative_bytes_out: number
  requests: number
  traffic_quota: number
  share_percent: number
}

export interface AnalyticsDailyPoint {
  date: string
  label: string
  traffic_bytes: number
  bytes_in: number
  bytes_out: number
  requests: number
}

export interface DashboardAnalyticsResponse {
  generated_at: string
  billing_mode: string
  summary: DashboardAnalyticsSummary
  node_breakdown: AnalyticsNodeTrafficItem[]
  top_share_users: AnalyticsShareUserItem[]
  site_breakdown: AnalyticsSiteTrafficItem[]
  daily_activity: AnalyticsDailyPoint[]
}
