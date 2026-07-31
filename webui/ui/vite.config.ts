import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Build output is embedded into the Go binary from internal/webui/assets.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../../internal/webui/assets',
    emptyOutDir: true,
    assetsDir: 'static',
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:7070',
      '/v1': 'http://127.0.0.1:7070',
    },
  },
})
