import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// In development, API calls go to a running panel (PANEL_URL, default http://localhost:7800).
const target = process.env.PANEL_URL || 'http://localhost:7800'

export default defineConfig({
  plugins: [react()],
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1200 },
  server: {
    proxy: {
      '/api': { target, ws: true, changeOrigin: true },
      '/hooks': { target, changeOrigin: true },
    },
  },
})
