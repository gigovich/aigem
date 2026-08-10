import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

// The build lands in ../dist, which the Go binary embeds. It is gitignored:
// `go install` has to work on a machine with no node toolchain, so a plain
// build ships without a UI rather than with a stale one.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  build: {
    outDir: "../dist",
    emptyOutDir: false,
  },
});
