import { reactive } from 'vue'

export interface Toast {
  id: number
  kind: 'success' | 'error' | 'info'
  message: string
}

export const toasts = reactive<Toast[]>([])
let next = 1

function push(kind: Toast['kind'], message: string, duration = kind === 'error' ? 5000 : 2600) {
  const id = next++
  toasts.push({ id, kind, message })
  setTimeout(() => dismiss(id), duration)
}

export function dismiss(id: number) {
  const index = toasts.findIndex(toast => toast.id === id)
  if (index >= 0) toasts.splice(index, 1)
}

export const toast = {
  success: (message: string) => push('success', message),
  error: (message: string) => push('error', message),
  info: (message: string) => push('info', message),
}

export function errorMessage(error: unknown, fallback = '操作失败'): string {
  if (error instanceof Error && error.message) return error.message
  return fallback
}
