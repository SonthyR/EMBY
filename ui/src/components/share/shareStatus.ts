import type { ShareStatus } from '@/api'

export const STATUS_LABEL: Record<ShareStatus, string> = {
  active: '正常',
  disabled: '已停用',
  expired: '已过期',
  quota: '流量用完',
}

export const STATUS_TONE: Record<ShareStatus, 'good' | 'neutral' | 'warn' | 'bad'> = {
  active: 'good',
  disabled: 'neutral',
  expired: 'bad',
  quota: 'warn',
}
