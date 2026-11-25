import { describe, expect, it } from 'vitest'
import { computeGridTargetIndex, computeListTargetIndex, type PointerSortItemRect } from '@/composables/usePointerSort'

function rect(top: number, height: number, left = 0, width = 320): PointerSortItemRect['rect'] {
  return { left, right: left + width, top, bottom: top + height, width, height }
}

describe('usePointerSort 高度不一致防抖动与 2D 网格算法', () => {
  it('单行主机(44px)向下拖过双行从机(76px)后绝不回弹抖动', () => {
    // 复现用户视频中的场景：#0 从机(76px), #1 主机(44px), #2 从机(76px), #3 从机(76px), gap=8px
    const beforeSwap: PointerSortItemRect[] = [
      { key: 1, rect: rect(0, 76) },
      { key: 0, rect: rect(84, 44) }, // 主机 (index 1, 44px)
      { key: 2, rect: rect(136, 76) }, // 懒猫美国NAT (index 2, 76px, bottom=212)
      { key: 3, rect: rect(220, 76) }, // PPF (index 3, 76px)
    ]

    // 光标刚进入 #2 上半区 (y=170) 时未达阈值 (212 - 22 = 190)，不换位
    expect(computeListTargetIndex({ items: beforeSwap, activeKey: 0, pointerY: 170 })).toBe(1)

    // 光标越过 190 (y=192) 时，主机换到 index 2
    expect(computeListTargetIndex({ items: beforeSwap, activeKey: 0, pointerY: 192 })).toBe(2)

    // 换位后的新布局：#2(76px) 上移到 84..160，主机(44px) 下移到 168..212
    const afterSwap: PointerSortItemRect[] = [
      { key: 1, rect: rect(0, 76) },
      { key: 2, rect: rect(84, 76) }, // bottom=160
      { key: 0, rect: rect(168, 44) }, // 主机现在在 index 2
      { key: 3, rect: rect(220, 76) },
    ]

    // 光标留在同一位置 y=192 甚至稍微回抬到 y=140，仍远大于上移阈值 (84 + 22 = 106)，绝不回弹！
    expect(computeListTargetIndex({ items: afterSwap, activeKey: 0, pointerY: 192 })).toBe(2)
    expect(computeListTargetIndex({ items: afterSwap, activeKey: 0, pointerY: 140 })).toBe(2)

    // 只有真正向上拖过 #2 顶部死区 (y=100 < 106) 才会换回 index 1
    expect(computeListTargetIndex({ items: afterSwap, activeKey: 0, pointerY: 100 })).toBe(1)
  })

  it('双行从机(76px)向下拖过单行主机(44px)同样保持稳定无抖动', () => {
    const beforeSwap: PointerSortItemRect[] = [
      { key: 2, rect: rect(0, 76) }, // dragging
      { key: 0, rect: rect(84, 44) }, // 主机 (bottom=128, threshold=106)
    ]
    expect(computeListTargetIndex({ items: beforeSwap, activeKey: 2, pointerY: 100 })).toBe(0)
    expect(computeListTargetIndex({ items: beforeSwap, activeKey: 2, pointerY: 108 })).toBe(1)

    const afterSwap: PointerSortItemRect[] = [
      { key: 0, rect: rect(0, 44) },
      { key: 2, rect: rect(52, 76) },
    ]
    expect(computeListTargetIndex({ items: afterSwap, activeKey: 2, pointerY: 108 })).toBe(1)
    expect(computeListTargetIndex({ items: afterSwap, activeKey: 2, pointerY: 18 })).toBe(0)
  })

  it('2 列网格支持同行横向换位与跨行纵向换位', () => {
    // 2列网格：card 1(左上), card 2(右上), card 3(左下), card 4(右下)
    const grid: PointerSortItemRect[] = [
      { key: 1, rect: rect(0, 180, 0, 300) },
      { key: 2, rect: rect(0, 180, 316, 300) },
      { key: 3, rect: rect(196, 180, 0, 300) },
      { key: 4, rect: rect(196, 180, 316, 300) },
    ]

    // 从左上(0)向右拖到右上(1)中心右侧 -> 换到 index 1
    expect(computeGridTargetIndex({ items: grid, activeKey: 1, pointerX: 500, pointerY: 90 })).toBe(1)

    // 从左上(0)向下拖到左下(2)下半区 -> 换到 index 2
    expect(computeGridTargetIndex({ items: grid, activeKey: 1, pointerX: 120, pointerY: 310 })).toBe(2)

    // 从右下(3)向左上方拖到左上(0)上半区 -> 换到 index 0
    expect(computeGridTargetIndex({ items: grid, activeKey: 4, pointerX: 100, pointerY: 60 })).toBe(0)
  })
})
