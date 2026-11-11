import { reactive } from 'vue'
import { authApi, setUnauthorizedHandler } from '@/api'

type Phase = 'checking' | 'setup' | 'login' | 'ready' | 'error'

export const session = reactive({
  phase: 'checking' as Phase,
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
    } else if (status.authenticated) {
      session.username = status.username || ''
      session.phase = 'ready'
    } else {
      session.phase = 'login'
    }
  } catch {
    session.phase = 'error'
  }
}

export async function login(username: string, password: string) {
  const result = await authApi.login(username, password)
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
    await authApi.logout()
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
