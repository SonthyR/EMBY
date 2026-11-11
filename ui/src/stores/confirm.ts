import { reactive } from 'vue'

interface ConfirmState {
  open: boolean
  title: string
  message: string
  confirmText: string
  danger: boolean
  resolve: ((value: boolean) => void) | null
}

export const confirmState = reactive<ConfirmState>({
  open: false,
  title: '',
  message: '',
  confirmText: '确定',
  danger: false,
  resolve: null,
})

export function confirm(options: { title: string; message?: string; confirmText?: string; danger?: boolean }): Promise<boolean> {
  confirmState.resolve?.(false)
  return new Promise(resolve => {
    Object.assign(confirmState, {
      open: true,
      title: options.title,
      message: options.message || '',
      confirmText: options.confirmText || '确定',
      danger: !!options.danger,
      resolve,
    })
  })
}

export function settleConfirm(value: boolean) {
  const resolve = confirmState.resolve
  confirmState.open = false
  confirmState.resolve = null
  resolve?.(value)
}
