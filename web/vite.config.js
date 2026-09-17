import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      // dev-only: forward API calls to the Go service
      '/api': 'http://localhost:8080'
    }
  },
  test: {
    environment: 'jsdom'
  }
})
