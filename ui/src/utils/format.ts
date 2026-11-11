export const GB = 1024 * 1024 * 1024

export function formatBytes(value: number | undefined | null): string {
  const bytes = Number(value) || 0
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let size = bytes
  let unit = 0
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024
    unit++
  }
  if (unit === 0) return `${Math.round(bytes)} B`
  return `${size >= 100 ? size.toFixed(0) : size >= 10 ? size.toFixed(1) : size.toFixed(2)} ${units[unit]}`
}

export function dateOnly(value: string | undefined | null): string {
  const raw = String(value || '').trim()
  return raw ? raw.slice(0, 10) : ''
}

/** Whole days from today (local) until a YYYY-MM-DD date, inclusive of that day. */
export function daysUntil(value: string | undefined | null, now = new Date()): number | null {
  const date = dateOnly(value)
  const match = date.match(/^(\d{4})-(\d{2})-(\d{2})$/)
  if (!match) return null
  const end = new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]) + 1)
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  return Math.round((end.getTime() - start.getTime()) / 86400000) - 1
}

export function relativeTime(value: string | undefined | null, now = Date.now()): string {
  const raw = String(value || '').trim()
  if (!raw) return '—'
  const parsed = Date.parse(raw.includes('T') ? raw : raw.replace(' ', 'T') + 'Z')
  if (!Number.isFinite(parsed)) return raw
  const seconds = Math.round((now - parsed) / 1000)
  if (seconds < 60) return '刚刚'
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall back below */
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.opacity = '0'
    document.body.appendChild(area)
    area.select()
    const ok = document.execCommand('copy')
    area.remove()
    return ok
  } catch {
    return false
  }
}
