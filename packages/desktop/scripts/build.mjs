import { build } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, rm } from 'node:fs/promises';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const selectors = process.argv.filter(value => value.startsWith('--linux-install-kind'));
if (selectors.length > 1 || (selectors.length === 1 && !['--linux-install-kind=appimage', '--linux-install-kind=system-package'].includes(selectors[0]))) throw new Error('Invalid Linux install-kind build selector');
const linuxInstallKind = selectors[0]?.slice('--linux-install-kind='.length) ?? 'appimage';
if (process.platform !== 'linux' && linuxInstallKind === 'system-package') throw new Error('Native Linux install-kind requires a Linux build');
const linuxDefine = process.platform === 'linux' ? { __DILLA_LINUX_INSTALL_KIND__: JSON.stringify(linuxInstallKind) } : {};

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const repo = resolve(root, '../..');
if (process.platform === 'linux') {
  const admissionStage = resolve(root, '.native-tools/native-admission-stage');
  await rm(admissionStage, { recursive: true, force: true });
  if (linuxInstallKind === 'system-package') execFileSync(process.execPath, [resolve(root, 'scripts/build-linux-admission.mjs'), resolve(admissionStage, 'dilla-admission.node')], { stdio: 'inherit' });
}
execFileSync(process.execPath, [resolve(root, 'scripts/build-native.mjs')], { stdio: 'inherit' });
await rm(resolve(root, 'dist'), { recursive: true, force: true });
await mkdir(resolve(root, 'dist'), { recursive: true });
if (!process.argv.includes('--existing-client')) {
  if (!process.env.npm_execpath) throw new Error('Run this build through npm run build -w @dilla/desktop');
  execFileSync(process.execPath, [process.env.npm_execpath, 'run', 'build', '-w', '@dilla/web'], { cwd: repo, stdio: 'inherit' });
}
await cp(resolve(repo, 'packages/web/dist'), resolve(root, 'dist/client'), { recursive: true });
const sandboxBootstrap = process.platform === 'linux' && linuxInstallKind === 'appimage';
await build({ entryPoints: [resolve(root, 'src/main.ts')], outfile: resolve(root, sandboxBootstrap ? 'dist/main-app.cjs' : 'dist/main.cjs'), define: linuxDefine, bundle: true, platform: 'node', target: 'node24', format: 'cjs', external: ['electron', 'electron-updater', 'electron-updater/*'] });
if (sandboxBootstrap) {
  for (const file of ['linux-sandbox-bootstrap.cjs', 'linux-sandbox-probe-arguments.cjs', 'linux-sandbox-probe-main.cjs', 'linux-sandbox-probe-preload.cjs', 'linux-sandbox-probe.html']) await cp(resolve(root, 'src', file), resolve(root, 'dist', file === 'linux-sandbox-bootstrap.cjs' ? 'main.cjs' : file));
}
await build({ entryPoints: [resolve(root, 'src/preload.ts')], outfile: resolve(root, 'dist/preload.cjs'), bundle: true, platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
await build({ entryPoints: [resolve(root, 'src/screen-capture-preload.ts')], outfile: resolve(root, 'dist/screen-capture-preload.cjs'), bundle: true, platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
await build({ entryPoints: [resolve(root, 'src/screen-capture-broker.ts')], outfile: resolve(root, 'dist/screen-capture-broker.js'), bundle: true, platform: 'browser', target: 'es2022', format: 'iife' });
await cp(resolve(root, 'src/screen-capture.html'), resolve(root, 'dist/screen-capture.html'));
await cp(resolve(root, 'src/manager.html'), resolve(root, 'dist/manager.html'));
await build({ entryPoints: [resolve(root, 'src/manager.ts')], outfile: resolve(root, 'dist/manager.js'), bundle: true, platform: 'browser', target: 'es2022' });
await cp(resolve(root, 'src/manager.css'), resolve(root, 'dist/manager.css'));
await cp(resolve(root, 'assets/tray.png'), resolve(root, 'dist/tray.png'));

await cp(resolve(root, 'assets/icon.png'), resolve(root, 'dist/icon.png'));
