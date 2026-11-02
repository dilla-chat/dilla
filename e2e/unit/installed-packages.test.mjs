import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, mkdtemp, mkdir, realpath, symlink, writeFile, rm } from 'node:fs/promises';
import { join, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { generateKeyPairSync } from 'node:crypto';
import { createRequire } from 'node:module';
import { runInNewContext } from 'node:vm';
import { createServer } from 'node:net';
import { installedAccountDiagnostics } from '../web/support/installed-diagnostics.ts';
import { fixtureMainSource, INSTALLED_FUSES } from '../web/support/installed-packages.ts';
import * as fixtures from '../web/support/installed-packages.ts';
import { finalizeWindowsProgram } from '../../packages/desktop/scripts/windows-package.mjs';

test('installed failure diagnostics retain actual process, update, bundle and endpoint facts without private data', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-diagnostic-'))), reports = join(root, 'reports'), profile = join(root, 'profile');
  const app = join(root, 'Applications/OwnedFixture.app'), installed = join(app, 'Contents/MacOS/dilla-desktop');
  const server = createServer(socket => socket.end());
  try {
    await mkdir(reports); await mkdir(profile); await mkdir(join(app, 'Contents/MacOS'), { recursive: true });
    await writeFile(installed, 'fixture executable');
    await writeFile(join(app, 'Contents/Info.plist'), '<plist><dict><key>CFBundleVersion</key><string>1.0.1</string><key>PRIVATE_SECRET</key><string>hidden</string></dict></plist>');
    await writeFile(join(reports, 'processes.jsonl'), JSON.stringify({ pid: process.pid, role: 'ordinary', version: '1.0.1', executable: '/PRIVATE_SECRET/profile', token: 'PRIVATE_SECRET' }) + '\n');
    await writeFile(join(reports, 'update-1.0.0.json'), JSON.stringify({ version: '1.0.0', state: { phase: 'installing', message: 'https://PRIVATE_SECRET/?token=SECRET' } }));
    await writeFile(join(reports, 'native-error.json'), 'PRIVATE_SECRET raw native error');
    await new Promise(done => server.listen(0, '127.0.0.1', done));
    const port = server.address().port, guid = '01234567-89ab-4cde-8fab-0123456789ab';
    await writeFile(join(profile, 'DevToolsActivePort'), `${port}\n/devtools/browser/${guid}`);
    const result = await installedAccountDiagnostics({ reports, profile, installed, platform: 'darwin', expectedVersion: '1.0.1', previousPort: port === 1 ? 2 : 1 });
    assert.deepEqual(result.processes.records, [{ pid: process.pid, role: 'ordinary', version: '1.0.1', alive: true }]);
    assert.equal(result.processes.status, 'ok');
    assert.deepEqual(result.updates, [{ version: '1.0.0', status: 'ok', phase: 'installing' }, { version: '1.0.1', status: 'missing', phase: null }]);
    assert.equal(result.candidateVersion, '1.0.1'); assert.equal(result.hasNativeError, true);
    assert.deepEqual(result.installed, { bundleExists: true, executableExists: true, bundleVersion: '1.0.1', plistStatus: 'ok' });
    assert.deepEqual(result.devTools, { present: true, valid: true, changed: true, live: true, status: 'ok' });
    const safe = JSON.stringify(result); assert.doesNotMatch(safe, /PRIVATE_SECRET|https:|token|devtools\/browser|profile/); assert.ok(!safe.includes(guid));
    await new Promise(done => server.close(done));
    const stale = await installedAccountDiagnostics({ reports, profile, installed, platform: 'darwin', expectedVersion: '1.0.1', previousPort: port });
    assert.deepEqual(stale.devTools, { present: true, valid: true, changed: false, live: false, status: 'ok' });
  } finally { if (server.listening) await new Promise(done => server.close(done)); await rm(root, { recursive: true, force: true }); }
});

test('installed failure diagnostics fail closed on malformed, oversized and symlinked private files', async () => {
  const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-account-diagnostic-invalid-'))), reports = join(root, 'reports'), profile = join(root, 'profile');
  const input = { reports, profile, installed: join(root, 'missing'), platform: 'linux', expectedVersion: '1.0.1' };
  try {
    await mkdir(reports); await mkdir(profile);
    const journal = join(reports, 'processes.jsonl');
    await writeFile(journal, '{PRIVATE_SECRET malformed');
    await writeFile(join(reports, 'update-1.0.0.json'), '{PRIVATE_SECRET malformed');
    await writeFile(join(profile, 'DevToolsActivePort'), 'not-a-port\nPRIVATE_SECRET');
    let result = await installedAccountDiagnostics(input);
    assert.equal(result.processes.status, 'malformed'); assert.deepEqual(result.processes.records, []);
    assert.equal(result.updates[0].status, 'malformed'); assert.equal(result.devTools.valid, false); assert.equal(result.devTools.live, false);
    await writeFile(journal, 'PRIVATE_SECRET'.repeat(6000));
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'oversized');
    await rm(journal); const secret = join(root, 'PRIVATE_SECRET'); await mkdir(secret);
    await symlink(secret, journal, process.platform === 'win32' ? 'junction' : 'dir');
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'unsafe'); assert.deepEqual(result.processes.records, []);
    await rm(journal); await writeFile(journal, (JSON.stringify({ pid: process.pid, role: 'ordinary', version: '1.0.1' }) + '\n').repeat(100));
    result = await installedAccountDiagnostics(input); assert.equal(result.processes.status, 'truncated'); assert.equal(result.processes.records.length, 64);
    assert.doesNotMatch(JSON.stringify(result), /PRIVATE_SECRET|not-a-port/);
  } finally { await rm(root, { recursive: true, force: true }); }
});

test('macOS account fixture locates the exact bundle produced by its declared executable name', async () => {
  const require = createRequire(import.meta.url), { AppInfo } = require('app-builder-lib/out/appInfo.js');
  const source = await readFile(new URL('../web/support/installed-packages.ts', import.meta.url), 'utf8');
  const executableName = /executableName: '([^']+)'/.exec(source)?.[1];
  assert.equal(executableName, 'dilla-desktop');
  // Evaluate the fixture's actual bundle lookup and compare it to the pinned
  // builder's naming contract. No macOS executable is launched on this host.
  const lookup = /const app = (join\(out,[^\n]+?\)), bins = await readdir/.exec(source)?.[1];
  assert.ok(lookup, 'the actual bundle lookup must be covered');
  const product = 'DillaAccountAcceptanceIsolatedFixture';
  const base = { productName: product, executableName };
  const info = new AppInfo({ config: base, metadata: { name: 'account-fixture', version: '1.0.0' } });
  assert.equal(info.productName, product);
  assert.equal(info.productFilename, executableName);
  for (const arch of ['arm64', 'x64']) {
    const out = join(tmpdir(), 'owned-account-packaging-output');
    const actual = runInNewContext(lookup, { join, out, product, base, process: { arch } });
    assert.equal(actual, join(out, arch === 'arm64' ? 'mac-arm64' : 'mac', info.productFilename + '.app'));
    assert.notEqual(actual, join(out, arch === 'arm64' ? 'mac-arm64' : 'mac', product + '.app'));
  }
});

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
