import { useSyncExternalStore } from 'react'

// Service worker registration. The worker itself (src/sw.ts) is built by vite-plugin-pwa.
// When a newer build takes control of an already-open page, `updated` flips so the UI can
// offer a reload; page loads are network-first, so the next launch is current either way.

let updated = false
const listeners = new Set<() => void>()

function markUpdated() {
  if (updated) return
  updated = true
  listeners.forEach((l) => l())
}

const PRELOAD_RELOAD_KEY = 'panel:preload-reload'

function registerPreloadRecovery() {
  if (import.meta.env.DEV) return
  // If an open tab navigates to an unvisited lazy route after the server was upgraded and
  // removed old hashed chunks, reload once to pick up the new index.html and asset manifest.
  window.addEventListener('vite:preloadError', (event) => {
    try {
      if (sessionStorage.getItem(PRELOAD_RELOAD_KEY) === __BUILD_ID__) return
      sessionStorage.setItem(PRELOAD_RELOAD_KEY, __BUILD_ID__)
    } catch {
      return
    }
    event.preventDefault()
    window.location.reload()
  })
}

export function registerServiceWorker() {
  registerPreloadRecovery()
  if (import.meta.env.DEV || !('serviceWorker' in navigator)) return
  // The first install also fires controllerchange (clientsClaim); flip hadController to true
  // once claimed so long-lived first-install sessions still detect subsequent updates.
  let hadController = !!navigator.serviceWorker.controller
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (!hadController) {
      hadController = true
      return
    }
    // Page loads are network-first, so this page may already be the new build. Compare the
    // running bundle's __BUILD_ID__ against /sw.js to see whether a reload is actually needed.
    fetch('/sw.js', { cache: 'no-store' })
      .then((r) => r.text())
      .then((sw) => !sw.includes(__BUILD_ID__) && markUpdated())
      .catch(markUpdated)
  })
  const start = () => {
    navigator.serviceWorker
      .register('/sw.js', { scope: '/', updateViaCache: 'none' })
      .then((reg) => {
        // Installed apps can stay open for days: look for a new build when brought back to the
        // foreground and once an hour.
        const check = () => {
          if (document.visibilityState === 'visible' && navigator.onLine) reg.update().catch(() => {})
        }
        document.addEventListener('visibilitychange', check)
        setInterval(check, 60 * 60 * 1000)
      })
      .catch(() => {})
  }
  if (document.readyState === 'complete') start()
  else window.addEventListener('load', start, { once: true })
}

export function useAppUpdated() {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => updated,
  )
}
