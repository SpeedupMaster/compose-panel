import crypto from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import zlib from 'node:zlib'
import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// In development, API calls go to a running panel (PANEL_URL, default http://localhost:7800).
const target = process.env.PANEL_URL || 'http://localhost:7800'

// Unique per-build stamp injected into both the app bundle and sw.ts so pwa.ts can reliably
// detect whether a newly activated worker belongs to a different build even with code splitting.
const buildId = process.env.PANEL_BUILD_ID || `${Date.now().toString(36)}-${crypto.randomBytes(4).toString('hex')}`

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

// Recreates empty dist/.keep after emptyOutDir so go:embed and git status stay clean.
function preserveDistKeep(): Plugin {
  let outDir = 'dist'
  return {
    name: 'preserve-dist-keep',
    apply: 'build',
    configResolved(cfg) {
      outDir = path.resolve(cfg.root, cfg.build.outDir)
    },
    closeBundle: {
      order: 'post',
      sequential: true,
      handler() {
        fs.mkdirSync(outDir, { recursive: true })
        fs.writeFileSync(path.join(outDir, '.keep'), '')
      },
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
  define: {
    __BUILD_ID__: JSON.stringify(buildId),
  },
  plugins: [
    dropLegacyWoff(),
    react(),
    VitePWA({
      // Custom worker (src/sw.ts): network-first page loads, core-shell precache, runtime asset cache, no API caching.
      strategies: 'injectManifest',
      srcDir: 'src',
      filename: 'sw.ts',
      // Registered by src/pwa.ts so update handling stays in our hands.
      injectRegister: false,
      injectManifest: {
        // Precache only the core shell, vendor-react, fonts, manifest and icons; lazy route chunks,
        // CodeMirror and xterm are cached lazily at runtime in sw.ts instead of eagerly on /login.
        // manifest.webmanifest and manifest icons are already injected by VitePWA automatically.
        globPatterns: [
          'index.html',
          'favicon.{svg,ico}',
          'apple-touch-icon.png',
          'assets/index-*.{js,css}',
          'assets/vendor-react-*.js',
          'assets/*.woff2',
        ],
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
    preserveDistKeep(),
  ],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    rollupOptions: {
      output: {
        manualChunks: {
          'vendor-react': ['react', 'react-dom', 'react-router-dom'],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': { target, ws: true, changeOrigin: true },
      '/hooks': { target, changeOrigin: true },
    },
  },
})
