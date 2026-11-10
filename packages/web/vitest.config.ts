import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

// Unit tests only: jsdom, no worker, no wasm. The real core runs in browsers (tasks 18 and 25).
export default defineConfig({
  plugins: [react()],
  // @dilla/media ships TypeScript sources and no package entry: the same mapping as vite.config.ts and tsconfig.json.
  resolve: { alias: [{ find: /^@dilla\/media$/, replacement: resolve(here, '../media/src/index.ts') }] },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}', 'scripts/**/*.test.mjs'],
    // The dist guard reads a build; it runs after one (npm run test:dist), not with the unit tests.
    exclude: ['**/node_modules/**', '**/target/**', 'src/**/*.dist.test.ts'],
    css: false,
  },
});
