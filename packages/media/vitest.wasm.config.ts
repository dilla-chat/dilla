import { defineConfig } from 'vitest/config';

// Real dilla-core-wasm under Node (`npm run build:wasm` first). The rust-wasm-node CI job runs these.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.wasm.test.ts'],
    exclude: ['**/node_modules/**', '**/target/**'],
    testTimeout: 120_000,
  },
});
