import { defineConfig } from 'vitest/config'

// Minimal config: no browser environment needed, the tested modules are pure.
export default defineConfig({
  test: {
    include: ['src/**/*.test.ts'],
  },
})
