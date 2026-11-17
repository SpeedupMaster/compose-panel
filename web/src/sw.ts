/// <reference lib="webworker" />
// Service worker: precaches the hashed app shell so the panel opens instantly (and offline),
// but never touches API traffic. Anything not matched by a route below (all of /api/, the SSE
// log streams, file downloads, webhooks) goes straight to the network, and WebSocket terminal
// traffic never passes through a service worker at all.
import { clientsClaim } from 'workbox-core'
import { cleanupOutdatedCaches, matchPrecache, precacheAndRoute } from 'workbox-precaching'
import { NavigationRoute, registerRoute } from 'workbox-routing'

declare let self: ServiceWorkerGlobalScope

// A new build takes over as soon as it is installed instead of waiting for every tab to close.
self.skipWaiting()
clientsClaim()
cleanupOutdatedCaches()

// Hashed assets (JS, CSS, fonts, icons) are served cache-first. directoryIndex is disabled so
// "/" is not answered from the precached index.html: page loads are handled below.
precacheAndRoute(self.__WB_MANIFEST, { directoryIndex: '' })

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
      let timer: ReturnType<typeof setTimeout> | undefined
      const network = fetch(request)
      const timeout = new Promise<'timeout'>((resolve) => {
        timer = setTimeout(() => resolve('timeout'), SHELL_TIMEOUT_MS)
      })
      try {
        const res = await Promise.race([network, timeout])
        if (res !== 'timeout') return res
        return (await fromShell()) ?? (await network)
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
