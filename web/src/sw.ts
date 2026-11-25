/// <reference lib="webworker" />
// Service worker: precaches the core app shell so the panel opens instantly (and offline),
// caches hashed lazy chunks on demand, and never touches API traffic. Anything not matched by a
// route below (all of /api/, SSE log streams, file downloads, webhooks) goes straight to the
// network, and WebSocket terminal traffic never passes through a service worker at all.
import { clientsClaim } from 'workbox-core'
import { cleanupOutdatedCaches, matchPrecache, precacheAndRoute } from 'workbox-precaching'
import { NavigationRoute, registerRoute } from 'workbox-routing'

declare let self: ServiceWorkerGlobalScope

// Retained on self so Rollup never tree-shakes __BUILD_ID__ out of /sw.js; pwa.ts reads /sw.js
// to check whether a newly activated worker belongs to a different build than the running tab.
const BUILD_ID = __BUILD_ID__
;(self as unknown as { __PANEL_BUILD_ID__?: string }).__PANEL_BUILD_ID__ = BUILD_ID

// A new build takes over as soon as it is installed instead of waiting for every tab to close.
self.skipWaiting()
clientsClaim()
cleanupOutdatedCaches()

// Core shell assets (index.html, entry JS/CSS, vendor-react, fonts, icons) are precached.
// directoryIndex is disabled so "/" is not answered from precached index.html: navigations are handled below.
precacheAndRoute(self.__WB_MANIFEST, { directoryIndex: '' })

// Hashed immutable chunks under /assets/* (lazy route pages, CodeMirror, xterm) use Cache-First
// so first visit to /login stays lightweight while visited routes load instantaneously offline.
const ASSET_CACHE = 'panel-assets-v1'

registerRoute(
  ({ url, request }) =>
    request.method === 'GET' &&
    url.origin === self.location.origin &&
    url.pathname.startsWith('/assets/'),
  async ({ request }) => {
    const cache = await caches.open(ASSET_CACHE)
    const cached = await cache.match(request)
    if (cached) return cached
    const res = await fetch(request)
    const ct = res.headers.get('content-type') || ''
    // Never cache error responses or SPA HTML fallbacks under /assets/*.
    if (res.ok && !ct.includes('text/html')) {
      await cache.put(request, res.clone())
    }
    return res
  },
)

// Page loads are network-first, so an online user always gets the current index.html (and with
// it the current asset hashes) and can never be stuck on an old build. The precached shell is
// only a fallback when the panel is unreachable.
const SHELL_TIMEOUT_MS = 6000

async function fromShell(): Promise<Response | undefined> {
  return matchPrecache('/index.html')
}

registerRoute(
  new NavigationRoute(
    async ({ request }) => {
      const ctrl = new AbortController()
      let timer: ReturnType<typeof setTimeout> | undefined
      const network = fetch(request, { signal: ctrl.signal })
      // Attach a rejection handler so a slow failed fetch after the timeout fallback never raises
      // an unhandled rejection in the service worker.
      network.catch(() => undefined)
      const timeout = new Promise<'timeout'>((resolve) => {
        timer = setTimeout(() => resolve('timeout'), SHELL_TIMEOUT_MS)
      })
      try {
        const res = await Promise.race([network, timeout])
        if (res !== 'timeout') return res
        const shell = await fromShell()
        if (shell) {
          ctrl.abort()
          return shell
        }
        return await network
      } catch (err) {
        const shell = await fromShell()
        if (shell) return shell
        throw err
      } finally {
        clearTimeout(timer)
      }
    },
    { denylist: [/^\/api\//, /^\/hooks\//] },
  ),
)
