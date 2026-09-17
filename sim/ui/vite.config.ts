import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Build output goes into the Go package that embeds it (internal/api/uidist).
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../../internal/api/uidist', emptyOutDir: true },
  server: {
    port: 5173,
    proxy: { '/api': { target: process.env.SIM_API ?? 'http://localhost:8600', changeOrigin: true } },
  },
})
