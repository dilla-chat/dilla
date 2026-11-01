import { mergeConfig } from 'vite';
import { resolve } from 'node:path';
import web from '../web/vite.config.ts';

export default mergeConfig(web, {
  root: resolve(import.meta.dirname, 'validation'), publicDir: false,
  build: { outDir: resolve(import.meta.dirname, 'validation-dist'), emptyOutDir: true,
    rollupOptions: { input: { index: resolve(import.meta.dirname, 'validation/index.html') } } },
});
