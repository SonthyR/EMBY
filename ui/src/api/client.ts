// Thin JSON client for the panel API. Every call is same-origin with the
// session cookie; a 401 outside the auth endpoints signals an expired session.

export class ApiError extends Error {
  status: number
  retryAfter?: number
  constructor(message: string, status: number, retryAfter?: number) {
    super(message)
    this.status = status
    this.retryAfter = retryAfter
  }
}

type UnauthorizedHandler = () => void
let onUnauthorized: UnauthorizedHandler | null = null
export function setUnauthorizedHandler(handler: UnauthorizedHandler) {
  onUnauthorized = handler
}

const AUTH_PATHS = ['/api/auth/login', '/api/auth/setup', '/api/auth/check', '/api/me/login']

export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined) {
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  let response: Response
  try {
    response = await fetch(path, init)
  } catch {
    throw new ApiError('网络连接失败，请检查服务是否在运行', 0)
  }
  if (response.status === 401 && !AUTH_PATHS.includes(path)) {
    onUnauthorized?.()
    throw new ApiError('登录已过期，请重新登录', 401)
  }
  let data: any = null
  const text = await response.text()
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = null
    }
  }
  if (!response.ok) {
    const retry = Number(data?.retry_after_seconds || response.headers.get('Retry-After') || 0)
    throw new ApiError(translateError(data?.error) || response.statusText || '请求失败', response.status, retry > 0 ? retry : undefined)
  }
  return data as T
}

const ERROR_TEXT: Record<string, string> = {
  'session expired or invalid': '登录已过期，请重新登录',
  'invalid request': '提交的内容格式不正确',
  'method not allowed': '不支持的操作',
  'same-origin request required': '请求来源不受信任，请刷新页面后重试',
  'site not found': '站点不存在',
  'account not found': '账号不存在',
}

export function translateError(message?: string): string {
  if (!message) return ''
  return ERROR_TEXT[message] || message
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  post: <T>(path: string, body: unknown = {}) => request<T>('POST', path, body),
  put: <T>(path: string, body: unknown) => request<T>('PUT', path, body),
  del: <T>(path: string) => request<T>('DELETE', path),
}
