import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, rm } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repo = resolve(root, '../..');
execFileSync(process.execPath, [resolve(root, 'scripts/build-native.mjs')], { stdio: 'inherit' });
await rm(resolve(root, 'dist'), { recursive: true, force: true });
await mkdir(resolve(root, 'dist'), { recursive: true });
if (!process.argv.includes('--existing-client')) {
  if (!process.env.npm_execpath) throw new Error('Run this build through npm run build -w @dilla/desktop');
  execFileSync(process.execPath, [process.env.npm_execpath, 'run', 'build', '-w', '@dilla/web'], { cwd: repo, stdio: 'inherit' });
}
await cp(resolve(repo, 'packages/web/dist'), resolve(root, 'dist/client'), { recursive: true });
await build({ entryPoints: [resolve(root, 'src/main.ts')], outfile: resolve(root, 'dist/main.cjs'), bundle: true, platform: 'node', target: 'node24', format: 'cjs', external: ['electron', 'electron-updater', 'electron-updater/*'] });
await build({ entryPoints: [resolve(root, 'src/preload.ts')], outfile: resolve(root, 'dist/preload.cjs'), bundle: true, platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
await build({ entryPoints: [resolve(root, 'src/screen-capture-preload.ts')], outfile: resolve(root, 'dist/screen-capture-preload.cjs'), bundle: true, platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
await build({ entryPoints: [resolve(root, 'src/screen-capture-broker.ts')], outfile: resolve(root, 'dist/screen-capture-broker.js'), bundle: true, platform: 'browser', target: 'es2022', format: 'iife' });
await cp(resolve(root, 'src/screen-capture.html'), resolve(root, 'dist/screen-capture.html'));
await cp(resolve(root, 'src/manager.html'), resolve(root, 'dist/manager.html'));
await build({ entryPoints: [resolve(root, 'src/manager.ts')], outfile: resolve(root, 'dist/manager.js'), bundle: true, platform: 'browser', target: 'es2022' });
await cp(resolve(root, 'src/manager.css'), resolve(root, 'dist/manager.css'));
await cp(resolve(root, 'assets/tray.png'), resolve(root, 'dist/tray.png'));

await cp(resolve(root, 'assets/icon.png'), resolve(root, 'dist/icon.png'));
