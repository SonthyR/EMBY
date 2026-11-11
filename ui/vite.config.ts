import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

// The build is embedded into the Go binary under web/static/app. The page is
// served at / (the legacy panel moved to /legacy/), so assets use an absolute
// /app/ base to resolve the same from any page URL.
export default defineConfig({
  base: '/app/',
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
    rollupOptions: {
      output: {
        // go:embed skips files whose names start with "_" or ".", so keep
        // every emitted chunk name alphanumeric at the start.
        chunkFileNames: info => `assets/${(info.name || 'chunk').replace(/^[_.]+/, '')}-[hash].js`,
        entryFileNames: 'assets/[name]-[hash].js',
        assetFileNames: info => `assets/${(info.names?.[0] || 'asset').replace(/^[_.]+/, '').replace(/\.[^.]+$/, '')}-[hash][extname]`,
      },
    },
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
