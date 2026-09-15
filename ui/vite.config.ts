import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/assets/dist',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/ping':      { target: 'http://localhost:8888', changeOrigin: true },
      '/info':      { target: 'http://localhost:8888', changeOrigin: true },
      '/workers':   { target: 'http://localhost:8888', changeOrigin: true },
      '/elections': { target: 'http://localhost:8888', changeOrigin: true },
    },
  },
})
