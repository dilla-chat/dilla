import { assertPortableEsbuildFilter } from './support/portable-esbuild-filter.ts';
import test from 'node:test';
import assert from 'node:assert/strict';
import { build } from 'esbuild';
import vm from 'node:vm';
import { createRequire } from 'node:module';
import { EventEmitter } from 'node:events';
import { resolve } from 'node:path';
const load = createRequire(import.meta.url);
const installationFilter = /[\\/]linux-installation\.ts$/;
const nonce = 'a'.repeat(64);
const flush = async () => { for (let index = 0; index < 8; index++) await new Promise<void>(done => setImmediate(done)); };
async function mainFixture(options: { kind?: 'system-package' | 'appimage'; argv?: string[]; env?: Record<string, string>; packaged?: boolean; validNative?: boolean; holdNative?: boolean; nativeLayout?: boolean; ownsLock?: boolean; gateError?: boolean; admissionManager?: string; admissionDigest?: string; identityManager?: string } = {}) {
 const effects: string[] = [], errors: string[] = [];
 let releaseValidation!: () => void;
 const validation = options.holdNative ? new Promise<void>(done => { releaseValidation = done; }) : Promise.resolve();
 const output = await build({ entryPoints: [resolve(import.meta.dirname, '../src/main.ts')], bundle: true, write: false, platform: 'node', format: 'cjs', external: ['electron', 'electron-updater', 'electron-updater/*'],
  define: options.kind ? { __DILLA_LINUX_INSTALL_KIND__: JSON.stringify(options.kind) } : {}, plugins: options.kind === 'system-package' ? [{ name: 'protected-native-result-only', setup(plugin) {
   if(options.validNative) plugin.onLoad({ filter: installationFilter }, () => ({ contents: `export async function resolveLinuxInstallation(context) { globalThis.recordNative(context); await globalThis.validation; return {installKind:'system-package'}; }`, loader: 'ts' }));
  } }] : [] });
 const app = Object.assign(new EventEmitter(), { isPackaged: options.packaged ?? true,
  getVersion: () => '0.1.0', getName: () => 'Dilla', getPath: () => { effects.push('profile'); throw Error('profile must not be accessed'); },
  setName: () => effects.push('normal-startup'), setDesktopName() {}, enableSandbox() {}, requestSingleInstanceLock: () => { effects.push('lock'); return options.ownsLock ?? false; },
  quit: () => effects.push('quit'), exit: (code: number) => effects.push('exit:' + code), whenReady: () => new Promise(() => {}),
 });
 const environment: Record<string, string> = { APPIMAGE: '/home/user/spoofed.AppImage', ...options.env };
 const electron = { app, protocol: { registerSchemesAsPrivileged() {} }, nativeTheme: {}, ipcMain: { handle() {}, on() {} } };
 const module = { exports: {} };
 vm.runInNewContext(output.outputFiles![0].text, { module, exports: module.exports, require: (name: string) => {
  if (name === 'electron') return electron;
  if (name === 'electron-updater') return { AppImageUpdater: class { constructor() { effects.push('AppImageUpdater'); } }, NsisUpdater: class {} };
  if (name.includes('/providers/Provider')) return { Provider: class {} };
  if (name === 'node:child_process') return { ...load(name), spawn: () => { effects.push('spawn'); throw Error('forbidden spawn'); } };
  return load(name);
 }, process: { platform: 'linux', arch: 'x64', env: environment, argv: ['dilla', ...(options.argv ?? [])], versions: {}, cwd: () => process.cwd(), pid: process.pid,
  execPath: options.nativeLayout || options.validNative ? '/opt/Dilla/dilla-desktop' : '/tmp/.mount-fixture/dilla-desktop',
  resourcesPath: options.nativeLayout || options.validNative ? '/opt/Dilla/resources' : '/tmp/.mount-fixture/resources' },
 __dirname: '/immutable', Buffer, TextDecoder, URL, AbortController, Error, setTimeout, clearTimeout,
 console: { error: (...values: unknown[]) => errors.push(values.map(String).join(' ')) },

 validation, recordNative: (context: { installKind: string }) => { effects.push('native-validation:' + context.installKind); },
 });
 await flush();
 return { effects, errors, environment, releaseValidation: async () => { releaseValidation(); await flush(); } };
}
test('compiled native main validates layout before ordinary single-instance startup', async () => {
 const fixture = await mainFixture({ kind: 'system-package', validNative: true, holdNative: true });
 assert.deepEqual(fixture.effects, ['native-validation:system-package']);
 await fixture.releaseValidation();
 assert.ok(fixture.effects.includes('lock')); assert.equal(fixture.effects.includes('profile'),false);
});
for (const argv of [['--sandbox-setup'], ['--setup-sandbox']]) {
 test('compiled native main rejects sandbox installation role before profile/spawn: ' + argv.join(' '), async () => {
  const fixture = await mainFixture({ kind: 'system-package', validNative: true, argv });
  assert.ok(fixture.errors.some(error => /package manager/.test(error)));
  assert.deepEqual(fixture.effects, ['native-validation:system-package', 'exit:1']);
 });
}
test('stock AppImage install-only launch exits before layout, profile, lock or UI startup', async () => {
 const fixture = await mainFixture({ kind: 'appimage', env: { APPIMAGE_EXIT_AFTER_INSTALL:'true' } });
 assert.deepEqual(fixture.effects,['exit:0']); assert.deepEqual(fixture.errors,[]);
});
test('unmarked normal development still reaches ordinary single-instance startup', async () => {
 const fixture = await mainFixture({ packaged: false, argv: ['--instance=https://chat.test', 'dilla://invite/example'] });
 assert.ok(fixture.effects.includes('lock')); assert.ok(fixture.effects.includes('quit')); assert.deepEqual(fixture.errors, []);
 assert.equal(fixture.effects.includes('profile'), false);
});
test('repacking compiled AppImage ASAR under the native fixed layout refuses startup', async () => {
 const fixture = await mainFixture({ kind: 'appimage', nativeLayout: true });
 assert.ok(fixture.errors.some(error => /layout/.test(error)));
 assert.deepEqual(fixture.effects, ['exit:1']);
});
test('repacking compiled native ASAR under an AppImage layout refuses startup', async () => {
 const fixture = await mainFixture({ kind: 'system-package' });
 assert.ok(fixture.errors.some(error => /layout/.test(error)));
 assert.deepEqual(fixture.effects, ['exit:1']);
});

for (const kind of ['system-package', undefined] as const) test('normal managed/unmarked packaged startup skips AppImage recovery discovery: ' + String(kind), async () => {
 const fixture = await mainFixture({ kind, validNative: kind === 'system-package', ownsLock: true });
 assert.ok(fixture.effects.includes('lock')); assert.equal(fixture.effects.includes('profile'), false); assert.deepEqual(fixture.errors, []);
});

test('linux-installation fixture intercepts both POSIX and Windows module paths', async () => {
 await assertPortableEsbuildFilter(installationFilter, 'linux-installation');
});
