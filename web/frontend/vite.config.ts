import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev server proxies /api to the Go backend so the frontend runs
// same-origin during development (no CORS needed).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8787',
        changeOrigin: true,
      },
    },
  },
  build: {
    // Output straight into web/dist so the Go binary embeds it via
    // //go:embed dist (see web/server.go).
    outDir: '../dist',
    emptyOutDir: true,
  },
})