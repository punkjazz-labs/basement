import { defineConfig } from 'vitest/config'

// Minimal config: no browser environment needed. The tested modules are pure,
// and the one component test renders to a string rather than to a DOM.
export default defineConfig({
  test: {
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
})
