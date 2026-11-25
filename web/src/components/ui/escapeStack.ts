// Pure LIFO stack for Escape-key handlers so stacked overlays (modals, job drawer, mobile nav)
// dismiss only the topmost active layer per Escape press.

export type EscapeStack = {
  push: (handler: () => void) => () => void
  handleKey: (key: string) => boolean
  size: () => number
}

export function createEscapeStack(): EscapeStack {
  const stack: (() => void)[] = []
  return {
    push(handler: () => void) {
      stack.push(handler)
      return () => {
        const idx = stack.lastIndexOf(handler)
        if (idx >= 0) stack.splice(idx, 1)
      }
    },
    handleKey(key: string) {
      if (key !== 'Escape' || stack.length === 0) return false
      const top = stack[stack.length - 1]
      top()
      return true
    },
    size() {
      return stack.length
    },
  }
}
