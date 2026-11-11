import { reactive } from 'vue'
import { ApiError, authApi, meApi, setUnauthorizedHandler } from '@/api'

type Phase = 'checking' | 'setup' | 'login' | 'ready' | 'error'
type Role = 'admin' | 'share'

export const session = reactive({
  phase: 'checking' as Phase,
  role: 'admin' as Role,
  username: '',
  ephemeralSecret: false,
})

export async function checkSession() {
  session.phase = 'checking'
  try {
    const status = await authApi.check()
    session.ephemeralSecret = !!status.jwt_secret_ephemeral
    if (status.needs_setup) {
      session.phase = 'setup'
      return
    }
    if (status.authenticated) {
      session.role = 'admin'
      session.username = status.username || ''
      session.phase = 'ready'
      return
    }
    try {
      const me = await meApi.get()
      session.role = 'share'
      session.username = me.username
      session.phase = 'ready'
    } catch {
      session.phase = 'login'
    }
  } catch {
    session.phase = 'error'
  }
}

/** One login form for both roles: share accounts first, then the administrator. */
export async function login(username: string, password: string) {
  try {
    const result = await meApi.login(username, password)
    session.role = 'share'
    session.username = result.username || username
    session.phase = 'ready'
    return
  } catch (error) {
    if (!(error instanceof ApiError) || error.status !== 404) throw error
  }
  const result = await authApi.login(username, password)
  session.role = 'admin'
  session.username = result.username || username
  session.phase = 'ready'
}

export async function setup(username: string, password: string, token: string) {
  await authApi.setup(username, password, token)
  await checkSession()
  if (session.phase !== 'ready') session.phase = 'login'
}

export async function logout() {
  try {
    if (session.role === 'share') await meApi.logout()
    else await authApi.logout()
  } finally {
    session.username = ''
    session.phase = 'login'
  }
}

setUnauthorizedHandler(() => {
  if (session.phase === 'ready') {
    session.username = ''
    session.phase = 'login'
  }
})
