import { defineConfig } from 'vitest/config';

// Node-only unit tests: no wasm, no browser. `npm test` at the repository root runs these.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts'],
    exclude: ['test/**/*.wasm.test.ts', '**/node_modules/**', '**/target/**'],
  },
});
