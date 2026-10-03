import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: { css: true },
  server: { proxy: { '/api': 'http://localhost:8000', '/auth': 'http://localhost:8000' } },
})
