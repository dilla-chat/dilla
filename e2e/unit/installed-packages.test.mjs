import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { fixtureMainSource, INSTALLED_FUSES } from '../web/support/installed-packages.ts';
import * as fixtures from '../web/support/installed-packages.ts';

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
