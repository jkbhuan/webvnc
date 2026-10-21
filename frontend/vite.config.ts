import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The build output is embedded into the Go binary with //go:embed, so it must be
// a plain static bundle with no external references.
//
// `base: './'` emits relative asset URLs, which keeps the app working when it is
// reached through a reverse proxy that mounts it under a sub-path.
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
    target: 'es2020',
  },
  server: {
    port: 5173,
    // `npm run dev` talks to a locally running webvnc.exe, so the UI can be
    // iterated on without rebuilding the Go binary each time.
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8080', ws: true },
    },
  },
})
