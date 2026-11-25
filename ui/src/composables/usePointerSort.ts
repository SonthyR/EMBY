import { onBeforeUnmount, ref } from 'vue'

export type PointerSortKey = string | number

export interface PointerSortItemRect {
  key: PointerSortKey
  rect: Pick<DOMRect, 'left' | 'right' | 'top' | 'bottom' | 'width' | 'height'>
}

export interface ComputeListTargetOptions {
  items: PointerSortItemRect[]
  activeKey: PointerSortKey
  pointerY: number
}

export interface ComputeGridTargetOptions {
  items: PointerSortItemRect[]
  activeKey: PointerSortKey
  pointerX: number
  pointerY: number
}

/**
 * Computes the next index for a 1D vertical list using height-aware hysteresis
 * deadbands. For any pair of item heights (D, T) and gap G, the forward and
 * reverse swap thresholds are separated by `max(D, T) + G > 0`, making
 * height-mismatch oscillation mathematically impossible.
 */
export function computeListTargetIndex({ items, activeKey, pointerY }: ComputeListTargetOptions): number {
  const curIdx = items.findIndex(item => item.key === activeKey)
  if (curIdx < 0 || items.length <= 1) return curIdx

  const dragHeight = items[curIdx].rect.height

  // Check downward movement past items below curIdx
  let targetIdx = curIdx
  for (let i = curIdx + 1; i < items.length; i++) {
    const r = items[i].rect
    const margin = Math.min(dragHeight, r.height) * 0.5
    if (pointerY > r.bottom - margin) {
      targetIdx = i
    } else {
      break
    }
  }
  if (targetIdx !== curIdx) return targetIdx

  // Check upward movement past items above curIdx
  for (let i = curIdx - 1; i >= 0; i--) {
    const r = items[i].rect
    const margin = Math.min(dragHeight, r.height) * 0.5
    if (pointerY < r.top + margin) {
      targetIdx = i
    } else {
      break
    }
  }
  return targetIdx
}

/**
 * Computes the next index for a 2D responsive card grid (1-col or multi-col).
 * Uses horizontal deadbands within the same visual row and height-aware vertical
 * deadbands across rows so cards of slightly different heights never flicker.
 */
export function computeGridTargetIndex({ items, activeKey, pointerX, pointerY }: ComputeGridTargetOptions): number {
  const curIdx = items.findIndex(item => item.key === activeKey)
  if (curIdx < 0 || items.length <= 1) return curIdx

  const dragRect = items[curIdx].rect
  // Detect single-column layout (e.g. mobile viewport) and delegate to 1D list logic.
  const isSingleColumn = items.every(
    item => Math.abs(item.rect.left - dragRect.left) < Math.max(24, dragRect.width * 0.25),
  )
  if (isSingleColumn) {
    return computeListTargetIndex({ items, activeKey, pointerY })
  }

  for (let i = 0; i < items.length; i++) {
    if (i === curIdx) continue
    const r = items[i].rect
    if (pointerX < r.left || pointerX > r.right || pointerY < r.top || pointerY > r.bottom) {
      continue
    }
    const sameRow = Math.abs(r.top - dragRect.top) < Math.min(r.height, dragRect.height) * 0.5
    if (sameRow) {
      const marginX = Math.min(dragRect.width, r.width) * 0.3
      if (i > curIdx && pointerX > r.right - marginX) return i
      if (i < curIdx && pointerX < r.left + marginX) return i
      // Also allow smooth horizontal swap once pointer crosses target center horizontally.
      const centerX = r.left + r.width * 0.5
      if (i > curIdx && pointerX >= centerX) return i
      if (i < curIdx && pointerX <= centerX) return i
      return curIdx
    }
    // Cross-row movement uses the same hysteresis-safe vertical deadband.
    const marginY = Math.min(dragRect.height, r.height) * 0.5
    if (i > curIdx && pointerY > r.bottom - marginY) return i
    if (i < curIdx && pointerY < r.top + marginY) return i
    return curIdx
  }

  return curIdx
}

export interface StartPointerSortOptions<T> {
  event: PointerEvent
  itemEl: HTMLElement
  containerEl: HTMLElement
  itemSelector: string
  getKeyFromEl: (el: HTMLElement) => PointerSortKey | null
  activeKey: PointerSortKey
  scope?: string
  mode: 'list' | 'grid'
  getList: () => T[]
  getItemKey: (item: T) => PointerSortKey
  applyList: (next: T[]) => void
  onCommit: (next: T[], previous: T[]) => void | Promise<void>
  thresholdPx?: number
}

export function usePointerSort() {
  const draggingKey = ref<PointerSortKey | null>(null)
  const draggingScope = ref<string | null>(null)
  const isDragging = ref(false)

  let suppressClickUntil = 0
  let cleanupListeners: (() => void) | null = null
  let ghostEl: HTMLElement | null = null
  let scrollRafId: number | null = null

  function clearGhost() {
    if (ghostEl) {
      ghostEl.remove()
      ghostEl = null
    }
  }

  function stopAutoScroll() {
    if (scrollRafId !== null) {
      window.cancelAnimationFrame(scrollRafId)
      scrollRafId = null
    }
  }

  function resetBodyStyles() {
    document.body.style.userSelect = ''
    document.body.style.cursor = ''
  }

  function cancelActive() {
    if (cleanupListeners) {
      cleanupListeners()
      cleanupListeners = null
    }
    stopAutoScroll()
    clearGhost()
    resetBodyStyles()
    draggingKey.value = null
    draggingScope.value = null
    isDragging.value = false
  }

  function shouldSuppressClick(): boolean {
    return Date.now() < suppressClickUntil
  }

  function start<T>(opts: StartPointerSortOptions<T>) {
    const {
      event,
      itemEl,
      containerEl,
      itemSelector,
      getKeyFromEl,
      activeKey,
      scope = 'default',
      mode,
      getList,
      getItemKey,
      applyList,
      onCommit,
      thresholdPx = 4,
    } = opts

    // Only primary mouse button or touch/pen contact.
    if (event.pointerType === 'mouse' && event.button !== 0) return

    const initialList = [...getList()]
    if (initialList.length <= 1) return

    cancelActive()

    const pointerId = event.pointerId
    const startX = event.clientX
    const startY = event.clientY
    let lastX = startX
    let lastY = startY
    let activated = false
    let grabOffsetX = 0
    let grabOffsetY = 0

    // Prevent text selection / touch scroll while pointer is held on drag handle.
    if (event.cancelable) event.preventDefault()

    const createGhost = () => {
      const rect = itemEl.getBoundingClientRect()
      grabOffsetX = startX - rect.left
      grabOffsetY = startY - rect.top

      const clone = itemEl.cloneNode(true) as HTMLElement
      clone.removeAttribute('id')
      clone.querySelectorAll('[id]').forEach(el => el.removeAttribute('id'))
      clone.classList.remove('is-sort-placeholder')
      clone.classList.add('is-sort-floating')
      Object.assign(clone.style, {
        position: 'fixed',
        top: '0',
        left: '0',
        width: `${rect.width}px`,
        height: `${rect.height}px`,
        margin: '0',
        zIndex: '9999',
        pointerEvents: 'none',
        boxSizing: 'border-box',
        transform: `translate3d(${rect.left}px, ${rect.top}px, 0) scale(1.015)`,
        boxShadow: '0 18px 40px -12px rgba(15, 23, 42, 0.24), 0 0 0 1.5px var(--accent)',
        opacity: '0.96',
        borderRadius: window.getComputedStyle(itemEl).borderRadius || '12px',
        background: window.getComputedStyle(itemEl).backgroundColor || 'var(--surface)',
        transition: 'box-shadow 120ms ease, opacity 120ms ease',
        willChange: 'transform',
      })
      document.body.appendChild(clone)
      ghostEl = clone
    }

    const updateGhostPosition = (x: number, y: number) => {
      if (!ghostEl) return
      const tx = x - grabOffsetX
      const ty = y - grabOffsetY
      ghostEl.style.transform = `translate3d(${tx}px, ${ty}px, 0) scale(1.015)`
    }

    const evaluateSwap = (x: number, y: number) => {
      const elements = Array.from(containerEl.querySelectorAll<HTMLElement>(itemSelector))
      const rects: PointerSortItemRect[] = []
      for (const el of elements) {
        const key = getKeyFromEl(el)
        if (key !== null) {
          rects.push({ key, rect: el.getBoundingClientRect() })
        }
      }
      if (rects.length <= 1) return

      const currentIdx = rects.findIndex(item => item.key === activeKey)
      if (currentIdx < 0) return

      const nextIdx =
        mode === 'grid'
          ? computeGridTargetIndex({ items: rects, activeKey, pointerX: x, pointerY: y })
          : computeListTargetIndex({ items: rects, activeKey, pointerY: y })

      if (nextIdx < 0 || nextIdx === currentIdx) return

      const targetKey = rects[nextIdx].key
      const list = [...getList()]
      const from = list.findIndex(item => getItemKey(item) === activeKey)
      const to = list.findIndex(item => getItemKey(item) === targetKey)
      if (from < 0 || to < 0 || from === to) return

      const [moved] = list.splice(from, 1)
      list.splice(to, 0, moved)
      applyList(list)
    }

    const tickAutoScroll = () => {
      if (!activated) {
        scrollRafId = null
        return
      }
      const edge = 64
      const vh = window.innerHeight
      let delta = 0
      if (lastY < edge) {
        delta = -Math.ceil(((edge - lastY) / edge) * 14)
      } else if (lastY > vh - edge) {
        delta = Math.ceil(((lastY - (vh - edge)) / edge) * 14)
      }
      if (delta !== 0) {
        window.scrollBy(0, delta)
        evaluateSwap(lastX, lastY)
      }
      scrollRafId = window.requestAnimationFrame(tickAutoScroll)
    }

    const finish = (cancelled: boolean) => {
      const wasActivated = activated
      cancelActive()
      if (!wasActivated) return

      // Block the synthetic click event that immediately follows pointerup on cards/rows.
      suppressClickUntil = Date.now() + 260

      if (cancelled) {
        applyList(initialList)
        return
      }

      const finalList = [...getList()]
      const changed =
        finalList.length === initialList.length &&
        finalList.some((item, idx) => getItemKey(item) !== getItemKey(initialList[idx]))
      if (changed) {
        void onCommit(finalList, initialList)
      }
    }

    const onPointerMove = (e: PointerEvent) => {
      if (e.pointerId !== pointerId) return
      lastX = e.clientX
      lastY = e.clientY

      if (!activated) {
        const dist = Math.hypot(lastX - startX, lastY - startY)
        if (dist < thresholdPx) return
        activated = true
        createGhost()
        draggingKey.value = activeKey
        draggingScope.value = scope
        isDragging.value = true
        document.body.style.userSelect = 'none'
        document.body.style.cursor = 'grabbing'
        scrollRafId = window.requestAnimationFrame(tickAutoScroll)
      }

      if (e.cancelable) e.preventDefault()
      updateGhostPosition(lastX, lastY)
      evaluateSwap(lastX, lastY)
    }

    const onPointerUp = (e: PointerEvent) => {
      if (e.pointerId !== pointerId) return
      finish(false)
    }

    const onPointerCancel = (e: PointerEvent) => {
      if (e.pointerId !== pointerId) return
      finish(true)
    }

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.preventDefault()
        finish(true)
      }
    }

    const onBlur = () => {
      finish(false)
    }

    window.addEventListener('pointermove', onPointerMove, { passive: false })
    window.addEventListener('pointerup', onPointerUp)
    window.addEventListener('pointercancel', onPointerCancel)
    window.addEventListener('keydown', onKeyDown)
    window.addEventListener('blur', onBlur)

    cleanupListeners = () => {
      window.removeEventListener('pointermove', onPointerMove)
      window.removeEventListener('pointerup', onPointerUp)
      window.removeEventListener('pointercancel', onPointerCancel)
      window.removeEventListener('keydown', onKeyDown)
      window.removeEventListener('blur', onBlur)
    }
  }

  onBeforeUnmount(() => cancelActive())

  return {
    draggingKey,
    draggingScope,
    isDragging,
    start,
    cancel: cancelActive,
    shouldSuppressClick,
  }
}
