import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, mkdtemp, mkdir, realpath, symlink, writeFile, rm } from 'node:fs/promises';
import { join, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { generateKeyPairSync } from 'node:crypto';
import { fixtureMainSource, INSTALLED_FUSES } from '../web/support/installed-packages.ts';
import * as fixtures from '../web/support/installed-packages.ts';
import { finalizeWindowsProgram } from '../../packages/desktop/scripts/windows-package.mjs';

test('installed account packaging resolves temporary aliases before strict Windows tree validation', async () => {
  const parent = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-path-')));
  try {
    const physical = join(parent, 'physical'), alias = join(parent, 'alias'); await mkdir(physical);
    await symlink(physical, alias, process.platform === 'win32' ? 'junction' : 'dir');
    const root = await fixtures.createInstalledFixtureRoot(alias), outDir = join(root, 'out'), appOutDir = join(outDir, 'win-unpacked');
    await mkdir(join(appOutDir, 'resources'), { recursive: true });
    await writeFile(join(appOutDir, 'dilla-desktop.exe'), 'portable packaging fixture'); await writeFile(join(appOutDir, 'resources/app.asar'), 'portable ASAR fixture');
    // Exercise the actual packaging/tree-validation boundary. This portable
    // compiler stand-in makes no claim about native Windows execution.
    const publicKey = generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString();
    await finalizeWindowsProgram({ appOutDir, outDir, version: '1.0.0', arch: 'x64', productName: 'DillaAccountAcceptanceFixture', appId: 'dev.dilla.desktop.account-acceptance.fixture', publicKeys: [publicKey],
      compileLauncher: async ({ file }) => { await writeFile(file, 'portable launcher fixture'); } });
    assert.equal(root, await realpath(root));
    assert.ok(root.startsWith(physical + sep), 'fixture identity and cleanup must use its physical root');
  } finally { await rm(parent, { recursive: true, force: true }); }
});

test('Windows account fixture replaces both actual compiled identity boundaries and rejects missing boundaries', async () => {
  const source = await readFile(new URL('../../packages/desktop/src/windows-installation.ts', import.meta.url), 'utf8');
  const identity = 'dev.dilla.desktop.account-acceptance.fixture';
  const compiled = fixtures.fixtureWindowsInstallationSource(source, identity);
  assert.equal((compiled.match(/description\.appId\s*!==\s*"dev\.dilla\.desktop\.account-acceptance\.fixture"/g) ?? []).length, 2);
  assert.doesNotMatch(compiled, /description\.appId\s*!==\s*'dev\.dilla\.desktop'/);
  assert.throws(() => fixtures.fixtureWindowsInstallationSource(source.replaceAll('description.appId', 'description.changed'), identity), /identity boundary/);
});

test('installed account automation retains production fuses and cannot enable Node inspection', () => {
  assert.deepEqual(INSTALLED_FUSES, { runAsNode: false, enableCookieEncryption: true,
    enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false,
    enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false });
  const input = "app.setName('Dilla');\nconst allowLoopback = !app.isPackaged && process.argv.includes('--allow-loopback');\nconst updater = createUpdater(state => {\n";
  const compiled = fixtureMainSource(input, { product: 'DillaAccountFixture', profile: '/private/fixture-profile', reports: '/private/reports', scheme: 'dilla-account-fixture', version: '1.0.0' });
  assert.match(compiled, /remote-debugging-port/);
  assert.match(compiled, /--update-watchdog/);
  assert.match(compiled, /const allowLoopback = true/);
  assert.doesNotMatch(compiled, /devTools:\s*true|--inspect|no-sandbox|nodeIntegration:\s*true/);
  assert.throws(() => fixtureMainSource(input.replace('const updater =', 'const renamed ='), { product: 'DillaAccountFixture', profile: '/private/fixture-profile', reports: '/private/reports', scheme: 'dilla-account-fixture', version: '1.0.0' }), /boundary/);
});
