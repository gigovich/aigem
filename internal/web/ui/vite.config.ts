import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    // The bundle is embedded from internal/web/dist by assets.go. emptyOutDir
    // stays off so the committed .gitkeep - the thing that lets `go install`
    // work with no node toolchain - survives a rebuild.
    outDir: '../dist',
    emptyOutDir: false,
  },
  server: {
    // `make web-dev` runs Vite while `aigem web` runs on another port, so the
    // API and the websockets have to be proxied rather than served from here.
    proxy: {
      '/api': { target: 'http://127.0.0.1:7777', changeOrigin: true, ws: true },
      '/healthz': { target: 'http://127.0.0.1:7777', changeOrigin: true },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
})
