import { assertPortableEsbuildFilter } from './support/portable-esbuild-filter.ts';
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import vm from 'node:vm';
import { createRequire } from 'node:module';
import { generateKeyPairSync, sign } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { resolve } from 'node:path';
const load = createRequire(import.meta.url);
const keysFilter = /[\\/]update-keys\.ts$/;
const pair = generateKeyPairSync('ed25519');
const publicKey = pair.publicKey.export({ type: 'spki', format: 'pem' }).toString();
const version = '0.1.1';
function releaseBytes() {
 return Buffer.from(JSON.stringify({ format: 1, version, channel: 'stable', publishedAt: new Date(Date.now() - 1000).toISOString(), expiresAt: new Date(Date.now() + 86400000).toISOString(), artifacts: [
  { platform: 'linux', arch: 'x64', url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-linux-x64.AppImage`, size: 1, sha512: Buffer.alloc(64).toString('base64') },
  { platform: 'win32', arch: 'x64', url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-windows-x64.exe`, size: 1, sha512: Buffer.alloc(64).toString('base64') },
  ...['x64', 'arm64'].map(arch => ({ platform: 'darwin', arch, url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-mac-${arch}.zip`, size: 1, sha512: Buffer.alloc(64).toString('base64'), sparkleSignature: Buffer.alloc(64).toString('base64') })),
 ] }));
}
async function compiledDriver(kind?: 'appimage' | 'system-package', invalidSignature = false) {
 const output = await build({ entryPoints: [resolve(import.meta.dirname, '../src/updater.ts')], bundle: true, write: false, platform: 'node', format: 'cjs', external: ['electron', 'electron-updater', 'electron-updater/*'],
  define: kind ? { __DILLA_LINUX_INSTALL_KIND__: JSON.stringify(kind) } : {}, plugins: [{ name: 'fixture-public-key', setup(plugin) {
   plugin.onLoad({ filter: keysFilter }, () => ({ contents: `export const RELEASE_PUBLIC_KEYS = ${JSON.stringify([publicKey])}`, loader: 'ts' }));
  } }] });
 const effects: string[] = [], phases: string[] = [];
 const app = Object.assign(new EventEmitter(), { isPackaged: true, getVersion: () => '0.1.0', getPath: () => { effects.push('profile'); return '/forbidden/profile'; }, exit: () => effects.push('exit') });
 const body = releaseBytes(), signature = sign(null, body, pair.privateKey).toString('base64');
 class AppImageUpdater extends EventEmitter {
  constructor() { super(); effects.push('AppImageUpdater'); }
  setFeedURL() { effects.push('setFeedURL'); }
  async checkForUpdates() { effects.push('checkForUpdates'); return { isUpdateAvailable: true }; }
  async downloadUpdate() { effects.push('downloadUpdate'); throw Error('fixture stops artifact download'); }
 }
 const module = { exports: {} as any };
 vm.runInNewContext(output.outputFiles![0].text, { module, exports: module.exports, require: (name: string) => {
  if (name === 'electron') return { app };
  if (name === 'electron-updater') return { AppImageUpdater, NsisUpdater: class {} };
  if (name.includes('/providers/Provider')) return { Provider: class {} };
  if (name === 'node:child_process') return { ...load(name), spawn: () => { effects.push('spawn'); throw Error('forbidden spawn'); } };
  return load(name);
 }, process: { platform: 'linux', arch: 'x64', env: { APPIMAGE: '/home/user/spoofed.AppImage' }, versions: {}, cwd: () => process.cwd() }, Buffer, TextDecoder, URL, AbortSignal, console, Error,
 fetch: async (url: string) => { effects.push('fetch:' + url); return new Response(url.endsWith('.sig') ? invalidSignature ? 'A'.repeat(86) + '==' : signature : body); },
 });
 const coordinator = module.exports.createUpdater((state: { phase: string }) => phases.push(state.phase), () => effects.push('failedQuit'));
 return { coordinator, effects, phases, release: JSON.parse(body.toString()) };
}
test('compiled native driver authenticates four-target release and never constructs or downloads an AppImage', async () => {
 const { coordinator, effects, phases } = await compiledDriver('system-package');
 await coordinator.check();
 assert.equal(coordinator.state.phase, 'available');
 assert.equal(coordinator.state.version, version);
 assert.deepEqual(phases, ['checking', 'available']);
 assert.equal(await coordinator.quit(true), false);
 assert.equal(effects.filter(item => item.startsWith('fetch:')).length, 2);
 assert.deepEqual(effects.filter(item => !item.startsWith('fetch:')), []);
});
test('compiled native driver rejects unauthenticated availability', async () => {
 const { coordinator, effects } = await compiledDriver('system-package', true);
 await coordinator.check(); assert.equal(coordinator.state.phase, 'error'); assert.match(coordinator.state.message, /signature/);
 assert.deepEqual(effects.filter(item => !item.startsWith('fetch:')), []);
});
test('unmarked packaged driver refuses updates even with spoofed APPIMAGE', async () => {
 const { coordinator, effects } = await compiledDriver();
 await coordinator.check(); assert.equal(coordinator.state.phase, 'error'); assert.match(coordinator.state.message, /installation|build/);
 assert.deepEqual(effects, []);
});
test('explicit compiled AppImage driver retains automatic check and download behavior', async () => {
 const { coordinator, effects, phases } = await compiledDriver('appimage');
 await coordinator.check();
 assert.equal(coordinator.state.phase, 'error'); assert.match(coordinator.state.message, /fixture stops artifact download/);
 assert.ok(effects.includes('AppImageUpdater')); assert.ok(effects.includes('downloadUpdate'));
 assert.deepEqual(phases, ['checking', 'downloading', 'error']);
});

test('compiled native driver refuses direct download/install calls before profile, process or filesystem activity', async () => {
 const { coordinator, effects, release } = await compiledDriver('system-package');
 // Exercise the real compiled driver defense independently of coordinator state.
 await assert.rejects(coordinator.driver.download(release), /package manager/);
 await assert.rejects(coordinator.driver.install(), /package manager/);
 coordinator.state = { phase: 'ready' };
 assert.equal(await coordinator.quit(true), false);
 assert.deepEqual(effects, []);
});

test('update-keys fixture intercepts both POSIX and Windows module paths', async () => {
 await assertPortableEsbuildFilter(keysFilter, 'update-keys');
});
