import { ref, watchEffect } from 'vue'

export type ThemeMode = 'system' | 'light' | 'dark'
const KEY = 'yundu-theme'

function stored(): ThemeMode {
  try {
    const value = localStorage.getItem(KEY)
    if (value === 'light' || value === 'dark' || value === 'system') return value
  } catch {
    /* storage unavailable */
  }
  return 'system'
}

export const themeMode = ref<ThemeMode>(stored())
const media = typeof matchMedia === 'function' ? matchMedia('(prefers-color-scheme: light)') : null
const systemLight = ref(!!media?.matches)
media?.addEventListener?.('change', event => {
  systemLight.value = event.matches
})

export function resolvedTheme(): 'light' | 'dark' {
  if (themeMode.value === 'system') return systemLight.value ? 'light' : 'dark'
  return themeMode.value
}

watchEffect(() => {
  const theme = resolvedTheme()
  document.documentElement.dataset.theme = theme
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', theme === 'light' ? '#f4f6fa' : '#0b0d12')
  try {
    localStorage.setItem(KEY, themeMode.value)
  } catch {
    /* storage unavailable */
  }
})

export function cycleTheme() {
  themeMode.value = themeMode.value === 'system' ? 'light' : themeMode.value === 'light' ? 'dark' : 'system'
}
