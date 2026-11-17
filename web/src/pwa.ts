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

export function registerServiceWorker() {
  if (import.meta.env.DEV || !('serviceWorker' in navigator)) return
  // The first install also fires controllerchange (clientsClaim); only a replacement counts.
  const hadController = !!navigator.serviceWorker.controller
  navigator.serviceWorker.addEventListener('controllerchange', () => {
    if (!hadController) return
    // Page loads are network-first, so this page may already be the new build. The worker's
    // precache manifest lists the hashed bundles of its build: if this page's bundle is among
    // them, the page is current and there is nothing to reload.
    const bundle = new URL(import.meta.url).pathname.slice(1)
    fetch('/sw.js', { cache: 'no-store' })
      .then((r) => r.text())
      .then((sw) => !sw.includes(bundle) && markUpdated())
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
