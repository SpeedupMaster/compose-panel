import { useCallback, useEffect, useRef, useState } from 'react'
import { createEscapeStack } from './escapeStack'

export { createEscapeStack, type EscapeStack } from './escapeStack'

// ---------- data loading hook ----------

export function useLoad<T>(fn: () => Promise<T>, deps: unknown[] = [], intervalMs = 0) {
  const [data, setData] = useState<T | undefined>()
  const [error, setError] = useState<unknown>()
  const [loading, setLoading] = useState(true)
  const fnRef = useRef(fn)
  fnRef.current = fn
  const reqIdRef = useRef(0)
  const inFlightRef = useRef(false)
  const mountedRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      reqIdRef.current++
    }
  }, [])

  const reload = useCallback(async () => {
    const id = ++reqIdRef.current
    inFlightRef.current = true
    try {
      const d = await fnRef.current()
      if (!mountedRef.current || id !== reqIdRef.current) return
      setData(d)
      setError(undefined)
    } catch (e) {
      if (!mountedRef.current || id !== reqIdRef.current) return
      setError(e)
    } finally {
      if (id === reqIdRef.current) {
        inFlightRef.current = false
        if (mountedRef.current) setLoading(false)
      }
    }
  }, [])

  useEffect(() => {
    setLoading(true)
    void reload()
    if (intervalMs > 0) {
      const refreshIfVisible = () => {
        if (document.visibilityState === 'visible' && !inFlightRef.current) {
          void reload()
        }
      }
      const t = setInterval(refreshIfVisible, intervalMs)
      document.addEventListener('visibilitychange', refreshIfVisible)
      return () => {
        clearInterval(t)
        document.removeEventListener('visibilitychange', refreshIfVisible)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  return { data, error, loading, reload, setData }
}

export function useDebouncedValue<T>(value: T, delayMs = 250): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(t)
  }, [value, delayMs])
  return debounced
}

// ---------- viewport & body scroll ----------

let scrollLockCount = 0
let savedBodyOverflow = ''

/** Reference-counted body scroll lock shared by modals, sheets, and mobile navigation drawer. */
export function lockBodyScroll() {
  if (scrollLockCount++ === 0) {
    savedBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'
  }
  return () => {
    if (scrollLockCount > 0 && --scrollLockCount === 0) {
      document.body.style.overflow = savedBodyOverflow
    }
  }
}

export function getViewportBounds() {
  const docEl = document.documentElement
  const vv = window.visualViewport
  const width = docEl.clientWidth || window.innerWidth
  const height = vv && Math.abs(vv.scale - 1) < 0.02 ? vv.height : (docEl.clientHeight || window.innerHeight)
  return { width, height }
}

export function useMediaQuery(query: string) {
  const [matches, setMatches] = useState(() =>
    typeof window !== 'undefined' && typeof window.matchMedia === 'function' ? window.matchMedia(query).matches : false,
  )
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const mq = window.matchMedia(query)
    const onChange = () => setMatches(mq.matches)
    onChange()
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [query])
  return matches
}

// ---------- LIFO Escape-key layer stack ----------

const globalEscapeStack = createEscapeStack()
let escListening = false

function ensureEscListener() {
  if (escListening || typeof window === 'undefined') return
  escListening = true
  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return
    if (globalEscapeStack.handleKey(e.key)) {
      e.preventDefault()
    }
  })
}

export function useEscapeLayer(active: boolean, onEscape: () => void) {
  const ref = useRef(onEscape)
  ref.current = onEscape
  useEffect(() => {
    if (!active) return
    ensureEscListener()
    return globalEscapeStack.push(() => ref.current())
  }, [active])
}
