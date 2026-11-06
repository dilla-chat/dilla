import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';
import { noiseBuild } from '../../scripts/noise-build.mjs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// The developer loop only: a dev server on 5180 proxied to a dilla-testhost on 8443/8444
// (`go run ./cmd/dilla-testhost`). No gate runs it. The page must look same-origin to dillad,
// which has no CORS and checks the gateway Origin (F4), so the proxy rewrites Host and Origin.
const DEV_TARGET = 'http://127.0.0.1:8443';
const toHost = { target: DEV_TARGET, changeOrigin: true, headers: { origin: DEV_TARGET } };

const here = dirname(fileURLToPath(import.meta.url));
const packages = resolve(here, '..');

/**
 * One core wasm (web-3 decision 5). @dilla/media's worker imports its own copy of the core glue
 * (`../../wasm/dilla_core_wasm.js`) and its manager posts the URL from `src/wasm-url.ts`. Both are redirected to the
 * core build the core worker already loads, so Vite emits one dilla_core_wasm_bg-*.wasm. The match is on the
 * resolved path, not the import specifier, because both imports are relative and `resolve.alias` sees only the
 * specifier.
 */
const ONE_CORE_WASM: Array<[RegExp, string]> = [
  [/^.*\/packages\/media\/wasm\/dilla_core_wasm\.js$/, resolve(packages, 'core-wasm/pkg/dilla_core_wasm.js')],
  [/^.*\/packages\/media\/src\/wasm-url(\.ts)?$/, resolve(here, 'src/call/wasm-url.ts')],
];
function oneCoreWasm(): Plugin {
  return {
    name: 'dilla-one-core-wasm',
    enforce: 'pre',
    resolveId(source, importer) {
      if (!importer || !source.startsWith('.')) return null;
      const path = resolve(dirname(importer.split('?')[0]), source).replaceAll('\\', '/');
      for (const [pattern, target] of ONE_CORE_WASM) if (pattern.test(path)) return target;
      return null;
    },
  };
}

/**
 * As packages/media/vite.config.ts: the worklet compiles the extracted RNNoise wasm, so the 1.92 MB data URI that
 * rnnoise-sync.js inlines is dead weight, and the manifest check refuses a data URI in any served file.
 */
const RNNOISE_SYNC = /rnnoise-sync\.js$/;
function stripRnnoiseInlineWasm(): Plugin {
  return {
    name: 'dilla-strip-rnnoise-inline-wasm',
    transform(source, id) {
      if (!RNNOISE_SYNC.test(id)) return null;
      const stripped = source
        .replace(/wasmBinaryFile\s*=\s*"data:application\/octet-stream;base64,[A-Za-z0-9+/=]+"/, 'wasmBinaryFile = ""')
        .replace(/dataURIPrefix\s*=\s*"data:application\/octet-stream;base64,"/, 'dataURIPrefix = "data:,dilla-unused"');
      if (!stripped.includes('wasmBinaryFile = ""') || !stripped.includes('"data:,dilla-unused"')) {
        throw new Error('RNNoise dependency changed: the unused inline wasm was not found');
      }
      return stripped;
    },
  };
}

// The mobile app's build flag (packages/client-core/src/worker/mobile-build.ts): packages/mobile/scripts/build.mjs sets
// DILLA_MOBILE_BUILD=1 and builds into its own directory. Every other build (web, desktop) defines it false, so the
// worker's mobile-only branches (a cross-origin instance, per-URL storage keys) are folded out of the bundle.
const MOBILE_BUILD = process.env.DILLA_MOBILE_BUILD === '1';

export default defineConfig({
  base: '/',
  define: { __DILLA_MOBILE__: JSON.stringify(MOBILE_BUILD) },
  plugins: [noiseBuild(), react(), oneCoreWasm(), stripRnnoiseInlineWasm()],
  resolve: {
    // @dilla/media ships TypeScript sources and no package entry; tsconfig.json maps the same path for tsc.
    alias: [{ find: /^@dilla\/media$/, replacement: resolve(packages, 'media/src/index.ts') }],
  },
  build: {
    target: 'es2022', outDir: 'dist', emptyOutDir: true, sourcemap: false, assetsInlineLimit: 0,
    // The shell imports call/load.ts (task 9), whose dynamic import is the only way to the call chunks; there is one input.
    rollupOptions: { input: { index: resolve(here, 'index.html') } },
  },
  worker: { format: 'es', plugins: () => [oneCoreWasm(), stripRnnoiseInlineWasm()] },
  server: {
    host: '127.0.0.1', port: 5180, strictPort: true,
    proxy: { '/v1': toHost, '/i/': toHost, '/gateway': { ...toHost, ws: true } },
  },
});
