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
  expires_at: string
}

export interface ShareUserSite {
  site_id: number
  site_account_id: number
  site_name?: string
  account_name?: string
  site_enabled?: boolean
}

export interface ShareSession {
  site_id: number
  site_name: string
  device_name: string
  client: string
  client_ip: string
  created_at: string
  last_seen_at: string
}

export type ShareStatus = 'active' | 'disabled' | 'expired' | 'quota'

export interface ShareUser {
  id: number
  username: string
  password?: string
  note: string
  enabled: boolean
  expires_at: string
  traffic_quota: number
  traffic_used: number
  status: ShareStatus
  session_count: number
  max_devices: number
  speed_limit: number
  traffic_reset_day: number
  next_reset_at?: string
  portal_path: string
  portal_url?: string
  sites: ShareUserSite[]
  sessions?: ShareSession[]
  created_at: string
  updated_at: string
}

export interface ShareUserInput {
  username: string
  password: string
  note: string
  enabled: boolean
  expires_at: string
  traffic_quota: number
  max_devices: number
  speed_limit: number
  traffic_reset_day: number
  sites: { site_id: number; site_account_id: number }[]
}
