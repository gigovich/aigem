import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { fileURLToPath, URL } from 'node:url'
import { readdirSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import type { Plugin as PostcssPlugin } from 'postcss'

const outDir = fileURLToPath(new URL('../dist', import.meta.url))

// emptyOutDir cannot be used: outDir is outside the Vite root, and the committed
// .gitkeep there is what lets `go install` work with no node toolchain. Without
// this, content-hashed names mean every rebuild leaves the previous bundle
// behind for `//go:embed all:dist` to compile into the binary.
function emptyDistKeepingGitkeep() {
  return {
    name: 'aigem-empty-dist',
    // Builds only. vitest and the dev server run buildStart too, so without this
    // `npm test` empties the bundle a previous `make web` produced and the next
    // `make build` silently ships a binary with no UI.
    apply: 'build' as const,
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

// The static IBM Plex Mono package lists .woff beside .woff2 in one src:. No
// browser that can run this app will ever fetch the fallback, and every byte
// here is compiled into the Go binary - 87 kB, a sixth of the bundle. Rewriting
// fontsource's own generated declaration rather than hand-writing @font-face
// keeps this independent of its internal file layout, and it has to be a postcss
// plugin because that CSS arrives through @import and is never its own module.
const dropLegacyWoff: PostcssPlugin = {
  postcssPlugin: 'aigem-drop-legacy-woff',
  Declaration: {
    src(decl) {
      if (!decl.value.includes('.woff2')) return
      const kept = decl.value
        .split(',')
        .filter((source) => !/format\((["']?)woff\1\)/.test(source))
        .join(',')
      if (kept.trim()) decl.value = kept
    },
  },
}

export default defineConfig({
  plugins: [emptyDistKeepingGitkeep(), react(), tailwindcss()],
  css: { postcss: { plugins: [dropLegacyWoff] } },
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    // The bundle is embedded from internal/web/dist by assets.go. Emptied by the
    // plugin above rather than by emptyOutDir, which would take .gitkeep with it.
    outDir,
    emptyOutDir: false,
    // Never inline a font as a data: URI. The daemon's CSP allows data: for
    // images only, so a subset that happened to fall under the default 4 kB
    // threshold would be blocked with nothing but a console violation to show
    // for it.
    assetsInlineLimit: 0,
  },
  server: {
    // `make web-dev` runs Vite while `aigem web` runs on another port, so the
    // API and the websockets have to be proxied rather than served from here.
    // `aigem web` picks a port from the kernel by default, so the daemon has to
    // be started on this one - `aigem web --addr 127.0.0.1:7777` - or AIGEM_ADDR
    // pointed at wherever it landed.
    //
    // Origin is rewritten as well as Host. changeOrigin only does the latter, so
    // the daemon would see Origin: http://localhost:5173 - a name its allowlist
    // has never heard of - and answer 403 to every API call in the dev cycle.
    // Sending the daemon's own origin is what the browser would send if the page
    // were served from it, which in production it is.
    proxy: {
      '/api': { target: daemon, changeOrigin: true, ws: true, headers: { Origin: daemon } },
      '/healthz': { target: daemon, changeOrigin: true, headers: { Origin: daemon } },
    },
  },
  test: {
    // test/ holds the checks that read the source files themselves and so need
    // Node's fs; src/ holds the component tests.
    include: ['src/**/*.test.{ts,tsx}', 'test/**/*.test.ts'],
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
  },
})
