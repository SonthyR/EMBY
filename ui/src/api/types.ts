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
  site_id: number
  site_name: string
  device_name: string
  client: string
  client_ip: string
  created_at: string
  last_seen_at: string
  draining?: boolean
  playing?: boolean
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
  last_seen_at: string
  online: boolean
  tls_active: boolean
  active_streams: number
  current_bps: number
  public_url: string
  status: EdgeNodeStatus
  install_command?: string
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

export interface ISPRouteNode {
  node_id: number
  name: string
  is_master: boolean
  enabled: boolean
  online: boolean
  status: EdgeNodeStatus | string
  active: boolean
  public_url?: string
}

export interface ISPRouteGroup {
  group: 'ct' | 'cu' | 'cm' | 'other' | string
  label: string
  nodes: ISPRouteNode[]
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
  node_id: number
  node_name: string
  public_url?: string
}
