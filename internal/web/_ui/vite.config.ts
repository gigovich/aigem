import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
import { readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'

const outDir = fileURLToPath(new URL('../dist', import.meta.url))

// emptyOutDir cannot be used: outDir is outside the Vite root, and the committed
// .gitkeep there is what lets `go install` work with no node toolchain. Without
// this, content-hashed names mean every rebuild leaves the previous bundle
// behind for `//go:embed all:dist` to compile into the binary.
function emptyDistKeepingGitkeep() {
  return {
    name: 'aigem-empty-dist',
    buildStart() {
      let entries: string[]
      try {
        entries = readdirSync(outDir)
      } catch {
        return
      }
      for (const name of entries) {
        if (name !== '.gitkeep') rmSync(join(outDir, name), { recursive: true, force: true })
      }
    },
  }
}

const daemon = process.env.AIGEM_ADDR ?? 'http://127.0.0.1:7777'

export default defineConfig({
  plugins: [emptyDistKeepingGitkeep(), react(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    // The bundle is embedded from internal/web/dist by assets.go. Emptied by the
    // plugin above rather than by emptyOutDir, which would take .gitkeep with it.
    outDir,
    emptyOutDir: false,
  },
  server: {
    // `make web-dev` runs Vite while `aigem web` runs on another port, so the
    // API and the websockets have to be proxied rather than served from here.
    // `aigem web` picks a port from the kernel by default, so the daemon has to
    // be started on this one - `aigem web --addr 127.0.0.1:7777` - or AIGEM_ADDR
    // pointed at wherever it landed.
    proxy: {
      '/api': { target: daemon, changeOrigin: true, ws: true },
      '/healthz': { target: daemon, changeOrigin: true },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
})
