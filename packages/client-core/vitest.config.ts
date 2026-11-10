import { defineConfig } from 'vitest/config';

// Node-only unit tests: no wasm, no browser (ruling 15). The root `npm test` runs these.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
    exclude: ['**/node_modules/**', '**/target/**'],
  },
});
