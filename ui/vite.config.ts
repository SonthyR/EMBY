import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// The build is embedded into the Go binary under web/static/app and served at
// /app/ while the legacy panel still lives at /. Relative asset URLs keep the
// bundle working wherever it is mounted.
export default defineConfig({
  base: './',
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: '../web/static/app',
    emptyOutDir: true,
    target: 'es2020',
    cssCodeSplit: true,
    chunkSizeWarningLimit: 400,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://127.0.0.1:9090', changeOrigin: false },
      '/fonts': { target: 'http://127.0.0.1:9090' },
    },
  },
  test: {
    environment: 'jsdom',
  },
} as any)
