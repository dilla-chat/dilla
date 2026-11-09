import { defineConfig } from 'vitest/config';

// The guard over the built app (dist/): run after npm run build -w @dilla/web.
export default defineConfig({ test: { environment: 'node', include: ['src/**/*.dist.test.ts'], exclude: ['**/node_modules/**', '**/target/**'] } });
