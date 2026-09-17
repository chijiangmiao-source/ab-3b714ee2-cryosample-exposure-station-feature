import { defineConfig } from '@playwright/test'

// Target the Docker Compose web service by default (WEB_PORT), or a dev
// server via WEB_BASE_URL, e.g. WEB_BASE_URL=http://localhost:5173
const baseURL = process.env.WEB_BASE_URL || `http://localhost:${process.env.WEB_PORT || 8081}`

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  fullyParallel: false,
  retries: 0,
  reporter: [['list']],
  use: {
    baseURL,
    actionTimeout: 10_000,
    testIdAttribute: 'data-test'
  }
})
