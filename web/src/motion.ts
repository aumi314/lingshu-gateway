// 动效的 JS 一侧：和 motion.css 同一组曲线和时长，给 Web Animations、拖拽库和 Ant Design 用。
import { useEffect, useState } from 'react'

export const EASE_OUT = 'cubic-bezier(0.2, 0.8, 0.2, 1)'
export const EASE_IN_OUT = 'cubic-bezier(0.4, 0, 0.2, 1)'
export const DUR = { instant: 100, fast: 160, base: 240, slow: 400 } as const

const QUERY = '(prefers-reduced-motion: reduce)'
export const prefersReducedMotion = () => window.matchMedia(QUERY).matches

/** 系统"减少动态效果"开关，中途切换也跟着变 */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(prefersReducedMotion)
  useEffect(() => {
    const mq = window.matchMedia(QUERY)
    const on = (e: MediaQueryListEvent) => setReduced(e.matches)
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [])
  return reduced
}
