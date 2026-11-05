import { defineConfig } from 'vite';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

// The probe imports real repo code by relative path (packages/client-core, packages/media); bare imports in those
// files (@dilla/core-wasm, @jitsi/rnnoise-wasm) resolve through the repo root's node_modules.
export default defineConfig({
  root: here,
  base: './',
  worker: { format: 'es' },
  server: { fs: { allow: [resolve(here, '../..')] } },
  build: {
    target: 'es2022',
    outDir: resolve(here, 'dist'),
    emptyOutDir: true,
    // Two pages: the capability probe (DILLA_PROBE) and the call probe (DILLA_CALL), kept apart.
    rollupOptions: { input: { probe: resolve(here, 'index.html'), call: resolve(here, 'call.html') } },
  },
});
