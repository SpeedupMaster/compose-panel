import fs from 'node:fs'
import path from 'node:path'
import zlib from 'node:zlib'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// In development, API calls go to a running panel (PANEL_URL, default http://localhost:7800).
const target = process.env.PANEL_URL || 'http://localhost:7800'

// Modern browsers all support woff2; drop redundant .woff fallbacks emitted by @fontsource (~94 KiB).
function dropLegacyWoff(): Plugin {
  return {
    name: 'drop-legacy-woff',
    enforce: 'pre',
    transform(code, id) {
      if (!id.includes('@fontsource') || !id.endsWith('.css')) return null
      const next = code.replace(/,\s*url\([^)]+\.woff\)\s*format\(['"]woff['"]\)/g, '')
      return next === code ? null : { code: next, map: null }
    },
  }
}

// Precompress text assets in outDir to .gz (removing originals) after VitePWA finishes so go:embed
// stores compressed bytes (~1 MiB smaller binary) and Go serves them directly or gunzips on fallback.
function precompressEmbeddedDist(): Plugin {
  let outDir = 'dist'
  const exts = new Set(['.js', '.mjs', '.css', '.html', '.svg', '.webmanifest'])
  const walk = (dir: string) => {
    for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, ent.name)
      if (ent.isDirectory()) {
        walk(full)
        continue
      }
      if (!exts.has(path.extname(ent.name).toLowerCase())) continue
      const raw = fs.readFileSync(full)
      const gz = zlib.gzipSync(raw, { level: 9 })
      if (gz.length < raw.length) {
        fs.writeFileSync(full + '.gz', gz)
        fs.unlinkSync(full)
      }
    }
  }
  return {
    name: 'precompress-embedded-dist',
    apply: 'build',
    configResolved(cfg) {
      outDir = path.resolve(cfg.root, cfg.build.outDir)
    },
    closeBundle: {
      order: 'post',
      sequential: true,
      handler() {
        if (fs.existsSync(outDir)) walk(outDir)
      },
    },
  }
}

export default defineConfig({
  plugins: [
    dropLegacyWoff(),
    react(),
    VitePWA({
      // Custom worker (src/sw.ts): network-first page loads, precached assets, no API caching.
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',
      // Registered by src/pwa.ts so update handling stays in our hands.
      injectRegister: false,
      injectManifest: {
        // .woff duplicates the .woff2 fonts; every browser that runs a service worker takes woff2.
        globPatterns: ['**/*.{js,css,html,woff2,png,svg,ico}'],
      },
      manifest: {
        id: '/',
        name: 'Compose 面板',
        short_name: 'Compose',
        description: 'Docker Compose 项目、容器、数据库与站点管理面板',
        lang: 'zh-CN',
        start_url: '/',
        scope: '/',
        display: 'standalone',
        theme_color: '#17181B',
        background_color: '#17181B',
        categories: ['developer', 'utilities'],
        icons: [
          { src: '/pwa-192x192.png', sizes: '192x192', type: 'image/png', purpose: 'any' },
          { src: '/pwa-512x512.png', sizes: '512x512', type: 'image/png', purpose: 'any' },
          { src: '/maskable-512x512.png', sizes: '512x512', type: 'image/png', purpose: 'maskable' },
        ],
      },
      devOptions: { enabled: false },
    }),
    precompressEmbeddedDist(),
  ],
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1200 },
  server: {
    proxy: {
      '/api': { target, ws: true, changeOrigin: true },
      '/hooks': { target, changeOrigin: true },
    },
  },
})
