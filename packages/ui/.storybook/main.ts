import { fileURLToPath } from 'node:url';
import type { StorybookConfig } from '@storybook/react-vite';
const config: StorybookConfig = {
  stories: ['../src/**/*.stories.tsx'],
  addons: ['@storybook/addon-a11y'],
  framework: { name: '@storybook/react-vite', options: {} },
  // Task 11 (rule 3d): whole-screen stories render the web's own screens, which reach @dilla/client-core and its worker module;
  // a code-split build needs the worker as an ES module (the web app's own Vite config does the same).
  // The CI ui job does not build packages/core-wasm/pkg, and no story runs the real core: Storybook resolves the package to a
  // stub (its init throws) and its wasm asset to an empty file. Exact-match aliases keep the two specifiers independent.
  viteFinal: config => ({
    ...config,
    worker: { ...config.worker, format: 'es' },
    resolve: {
      ...config.resolve,
      alias: [
        ...(Array.isArray(config.resolve?.alias) ? config.resolve.alias
          : Object.entries(config.resolve?.alias ?? {}).map(([find, replacement]) => ({ find, replacement }))),
        { find: /^@dilla\/core-wasm\/wasm$/, replacement: fileURLToPath(new URL('./core-wasm-stub.wasm', import.meta.url)) },
        { find: /^@dilla\/core-wasm$/, replacement: fileURLToPath(new URL('./core-wasm-stub.ts', import.meta.url)) },
      ],
    },
  }),
};
export default config;
