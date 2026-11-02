import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

async function buildFixture(argv: string[], platform = 'linux') {
 const source = await readFile(new URL('../scripts/build.mjs', import.meta.url), 'utf8');
 const operations: string[] = [], builds: any[] = [];
 // Execute the actual build body with its external build/filesystem/process
 // dependencies captured; no native/web build or output directory is touched.
 const body = source.replace(/^import .*;\n/gm, '').replaceAll('import.meta.url', 'buildModuleURL');
 const context = vm.createContext({
  process: { platform, argv: ['node', 'build.mjs', ...argv], env: { npm_execpath: 'fixture-npm', DILLA_LINUX_INSTALL_KIND: 'system-package' }, execPath: 'node' },
  build: async (options: any) => { builds.push(options); }, execFileSync: () => operations.push('exec'),
  cp: async () => operations.push('cp'), mkdir: async () => operations.push('mkdir'), rm: async () => operations.push('rm'),
  dirname, resolve, fileURLToPath, buildModuleURL: new URL('../scripts/build.mjs', import.meta.url).href,
 });
 return { operations, builds, run: () => vm.runInContext('(async () => {' + body + '\n})()', context) };
}
test('production build fixes Linux AppImage ownership and explicit native builds change the compiled literal only at build time', async () => {
 for (const [argv, expected] of [[[], 'appimage'], [['--linux-install-kind=system-package'], 'system-package'], [['--linux-install-kind=appimage'], 'appimage']] as const) {
  const fixture = await buildFixture([...argv]); await fixture.run();
  assert.equal(fixture.builds[0].define?.__DILLA_LINUX_INSTALL_KIND__, JSON.stringify(expected));
 }
});
test('invalid/duplicate/native-on-nonLinux selectors fail before builds or filesystem changes', async () => {
 for (const [argv, platform] of [ [['--linux-install-kind=unknown'], 'linux'], [['--linux-install-kind=appimage', '--linux-install-kind=system-package'], 'linux'], [['--linux-install-kind=system-package'], 'darwin'] ] as [string[], string][]) {
  const fixture = await buildFixture(argv, platform);
  await assert.rejects(fixture.run(), /install.kind|Linux/);
  assert.deepEqual(fixture.operations, []); assert.deepEqual(fixture.builds, []);
 }
});

test('nonLinux builds remain unmarked and preserve their own updater authority', async () => {
 for (const platform of ['win32', 'darwin']) {
  const fixture = await buildFixture([], platform); await fixture.run();
  assert.equal(fixture.builds[0].define?.__DILLA_LINUX_INSTALL_KIND__, undefined);
 }
});
