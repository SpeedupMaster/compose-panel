import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { VitePWA } from 'vite-plugin-pwa'

// In development, API calls go to a running panel (PANEL_URL, default http://localhost:7800).
const target = process.env.PANEL_URL || 'http://localhost:7800'

export default defineConfig({
  plugins: [
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
  ],
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1200 },
  server: {
    proxy: {
      '/api': { target, ws: true, changeOrigin: true },
      '/hooks': { target, changeOrigin: true },
    },
  },
})
