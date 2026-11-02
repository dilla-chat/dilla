// Actual Windows kernel/NSIS lifecycle acceptance. Linux compilation cannot
// establish these image-lock, known-folder, registry, or rename guarantees.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile, stat, realpath, lstat } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createReadStream } from 'node:fs';
import { spawn, execFileSync } from 'node:child_process';
import { createHash, generateKeyPairSync, randomUUID, sign } from 'node:crypto';
import { createRequire } from 'node:module';
import { setTimeout as pause } from 'node:timers/promises';
import { compileWindowsNSIS, withMaintenanceTimingFixture, withInitialInstallDiagnostics, initialInstallDiagnostic, withInitialSlotFlushFailure } from '../support/windows-nsis.mjs';
import { finalizeWindowsProgram } from '../../scripts/windows-package.mjs';
import { createWindowsProgramManifest } from '../../src/windows-manifest.ts';
const require = createRequire(import.meta.url);
const { archive } = require('app-builder-lib/out/targets/archive');
const repository = resolve(import.meta.dirname, '../../../..');
const sha = raw => createHash('sha512').update(raw).digest('hex');
async function snapshot(root) {
 const result = {};
 async function visit(directory, prefix) {
  for (const entry of (await readdir(directory, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name))) {
   const path = join(directory, entry.name), key = prefix + entry.name;
   assert.equal(entry.isSymbolicLink(), false);
   if (entry.isDirectory()) { result[key + '/'] = 'directory'; await visit(path, key + '/'); }
   else result[key] = sha(await readFile(path));
  }
 }
 await visit(root, ''); return result;
}
async function exists(path) { return stat(path).then(() => true, error => { if (error.code === 'ENOENT') return false; throw error; }); }
async function eventually(check, label, timeout = 30_000) {
 const deadline = Date.now() + timeout;
 while (Date.now() < deadline) { if (await check()) return; await pause(50); }
 throw new Error('Timed out: ' + label);
}
function start(file, args = []) {
 const child = spawn(file, args, { stdio: 'ignore', windowsHide: false });
 child.on('error', () => {}); return child;
}
async function stop(child) {
 if (!child?.pid) return;
 if (child && child.exitCode === null && child.signalCode === null) {
  child.kill(); await eventually(() => Promise.resolve(child.exitCode !== null || child.signalCode !== null), 'owned fixture process exit');
 }
}
const ps = () => join(process.env.SystemRoot, 'System32/WindowsPowerShell/v1.0/powershell.exe');
function run(file, args = [], options = {}) { return execFileSync(file, args, { timeout: 120_000, windowsHide: true, stdio: 'pipe', ...options }); }

test('native installer preserves every byte on mapped Go/Electron images, archives only update history, and retries interrupted first setup', { skip: process.platform !== 'win32', timeout: 900_000 }, async () => {
 const temporary = await realpath(await mkdtemp(join(tmpdir(), 'dilla-nsis-native-')));
 const identity = randomUUID().replaceAll('-', '').slice(0, 12), productName = 'DillaInstallerFixture' + identity, appId = 'dev.dilla.installer.fixture.' + identity;
 const program = join(process.env.LOCALAPPDATA, 'Programs', productName), profile = join(process.env.APPDATA, productName);
 const app = join(temporary, 'payload'), updates = join(profile, 'updates'), slot = join(updates, 'windows-active-0.json');
 const processes = [];
 let uninstaller;
 try {
  // Use the actual pinned Electron PE and DLLs for mapped-image kernel proof.
  await cp(join(repository, 'node_modules/electron/dist'), app, { recursive: true });
  const electron = join(app, 'electron.exe');
  const { rename } = await import('node:fs/promises'); await rename(electron, join(app, 'dilla-desktop.exe'));
  await mkdir(join(app, 'resources'), { recursive: true });
  await writeFile(join(app, 'resources/app.asar'), 'disposable kernel fixture, never ordinary app boot');
  await writeFile(join(app, 'resources/fixture-main.cjs'), "require('electron').app.whenReady().then(() => setInterval(() => {}, 1000));\n");
  const releaseKeys = generateKeyPairSync('ed25519');
  const keys = [releaseKeys.publicKey.export({ type: 'spki', format: 'pem' }).toString()];
  const state = await finalizeWindowsProgram({ appOutDir: app, outDir: temporary, version: '1.2.3', arch: 'x64', productName, appId, publicKeys: keys,
   compileLauncher: async ({ file, config }) => {
    const encoded = Buffer.from(JSON.stringify(config)).toString('base64');
    execFileSync('go', ['build', '-trimpath', '-ldflags', '-H=windowsgui -X main.bootstrapConfig=' + encoded, '-o', file, './packages/desktop/native/windows-launcher'], { cwd: repository, env: { ...process.env, CGO_ENABLED: '0' }, stdio: 'pipe' });
   }, signLauncher: async () => {} });
  const archiveFile = join(temporary, 'payload.7z');
  await archive('7z', archiveFile, app, { withoutDir: true, compression: 'normal', installTimeDecodable: true });
  const digest = Buffer.from(state.digest, 'base64').toString('hex');
  await writeFile(archiveFile + '.bootstrap.nsh', `!define DILLA_BOOTSTRAP_EXE "${join(app,'Dilla.exe')}"\n!define DILLA_MANIFEST_SHA512 "${state.digest}"\n!define DILLA_PROGRAM_ID "v-${digest.slice(0, 32)}"\n!define DILLA_MANIFEST_NAME "${digest}.json"\n`);
  const compile = (name, transform) => compileWindowsNSIS({ directory: temporary, archive: archiveFile, productName, appId, name, transform });
  let diagnosticSites;
  const installer = await compile('dilla-fixture-native.exe', source => {
   const diagnostic = withInitialInstallDiagnostics(withMaintenanceTimingFixture(source)); diagnosticSites = diagnostic.sites; return diagnostic.source;
  });
  await mkdir(profile, { recursive: true });
  const account = join(profile, 'opaque-account-keystore-and-preferences.sentinel');
  await writeFile(account, 'existing account / KEK / OPFS / preferences must survive');
  const reportInitialFailure = async label => {
   let diagnostic = { format: 1, phase: 'unavailable', nativeExit: null };
   try {
    const file = join(temporary, 'fixture-install-diagnostic.json'), info = await lstat(file);
    if (!info.isFile() || info.isSymbolicLink() || info.size < 1 || info.size > 1024) throw new Error('Invalid diagnostic file');
    const bytes = await readFile(file); if (bytes.length > 1024) throw new Error('Oversized diagnostic');
    diagnostic = initialInstallDiagnostic(JSON.parse(bytes), diagnosticSites);
   } catch { /* Report only fixed enum/numeric data, never malformed raw bytes. */ }
   console.error(label + ' ' + JSON.stringify(diagnostic));
  };
  try { run(installer, ['/S']); }
  catch (error) { await reportInitialFailure('INITIAL_INSTALL_FAILURE'); throw error; }
  assert.equal(await exists(slot), true);
  uninstaller = join(program, `Uninstall ${productName}.exe`);
  const tree = join(program, 'versions', 'v-' + digest.slice(0, 32)), runtime = join(tree, 'dilla-desktop.exe');
  const manifest = join(program, 'manifests', digest + '.json'), originalManifest = await readFile(manifest);
  const beforeProgram = await snapshot(program), beforeProfile = await snapshot(profile);
  const errorDialog = async () => {
   await writeFile(manifest, '{unverified');
   const child = start(join(program, 'Dilla.exe')); processes.push(child);
   await eventually(() => {
    if (child.exitCode !== null) throw new Error('Root Go exited before its verification error dialog');
    try { return Promise.resolve(run(ps(), ['-NoProfile', '-NonInteractive', '-Command', `(Get-Process -Id ${child.pid}).MainWindowTitle`]).toString().includes('Dilla could not start')); } catch { return Promise.resolve(false); }
   }, 'actual root Go startup verification dialog');
   await writeFile(manifest, originalManifest);
   return child;
  };
  const busyUninstall = async () => {
   let failure;
   try { run(uninstaller, ['/S', '_?=' + program]); } catch (error) { failure = error; }
   assert.ok(failure, 'mapped owned image must refuse uninstall');
   assert.equal(failure.status, 2);
   assert.deepEqual(await snapshot(program), beforeProgram, 'busy uninstall changed program bytes');
   assert.deepEqual(await snapshot(profile), beforeProfile, 'busy uninstall changed profile/history');
  };
  // Root native Go stays mapped in its actual error MessageBox path.
  let rootGo = await errorDialog(); await busyUninstall(); await stop(rootGo);
  // Then the actual Electron image, and both image classes together.
  const electronChild = start(runtime, [join(tree, 'resources/fixture-main.cjs')]); processes.push(electronChild);
  await pause(1000); assert.equal(electronChild.exitCode, null, 'pinned Electron fixture must remain mapped');
  await busyUninstall(); rootGo = await errorDialog(); await busyUninstall(); await stop(rootGo); await stop(electronChild);
  // Retained launcher write access also blocks the pre-mutex image mapping
  // that an ordinary shortcut would otherwise race against final file deletion.
  const held = spawn(uninstaller, ['/S', '_?=' + program], { stdio: 'ignore', windowsHide: true, env: { ...process.env, DILLA_FIXTURE_HOLD_ROOT: '1' } });
  processes.push(held); held.on('error', () => {});
  await eventually(() => exists(join(profile, 'fixture-hold-ready')), 'native retained launcher guard');
  const mapping = await new Promise((resolveMapping, rejectMapping) => {
   const blocked = error => {
    if (error.code === 'EBUSY') resolveMapping('blocked');
    else rejectMapping(error);
   };
   let child;
   try { child = spawn(join(program, 'Dilla.exe'), [], { stdio: 'ignore', windowsHide: true }); }
   catch (error) { blocked(error); return; }
   processes.push(child);
   child.once('error', blocked); child.once('spawn', () => resolveMapping('mapped'));
  });
  assert.equal(mapping, 'blocked', 'ordinary root PE mapping must be excluded for the entire uninstall mutation window');
  await stop(held); await rm(join(profile, 'fixture-hold-ready'));
  assert.deepEqual(await snapshot(program), beforeProgram);
  assert.deepEqual(await snapshot(profile), beforeProfile);
  // A parent can publish a pending nonce and exit between the first proof
  // and the last image scan. The second proof must preserve that transaction.
  assert.throws(() => run(uninstaller, ['/S', '_?=' + program], { env: { ...process.env, DILLA_FIXTURE_PUBLISH_DURING_SCAN: '1' } }));
  assert.deepEqual(await snapshot(program), beforeProgram);
  assert.equal(JSON.parse(await readFile(join(updates, 'active.json'))).nonce, 'd'.repeat(64));
  await rm(join(updates, 'active.json')); await rm(join(updates, 'startup-' + 'd'.repeat(64)), { recursive: true });
  assert.deepEqual(await snapshot(profile), beforeProfile);
  run(uninstaller, ['/S']);
  await eventually(async () => !await exists(program), 'successful explicit uninstall removes its fixed program root');
  assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
  assert.equal(await exists(updates), false);
  assert.ok((await readdir(profile)).some(name => name.startsWith('updates-uninstalled-{')));
  run(installer, ['/S']);
  assert.equal(await exists(slot), true, 'reinstall must seed without manual history/key deletion');
  assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
  run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'reinstall cleanup');
  // Explicit removal uses the embedded original read-only guard, even when
  // either Electron or the stable root Go program itself can no longer start.
  for (const damage of ['manifest', 'ASAR', 'version PE', 'missing version PE', 'root Go', 'missing root Go', 'missing updates', 'all versions missing', 'all manifests missing', 'torn published slot', 'nonterminal active']) {
   if (!await exists(program)) run(installer, ['/S']);
   if (damage === 'manifest') await writeFile(manifest, '{damaged');
   if (damage === 'ASAR') await writeFile(join(tree, 'resources/app.asar'), 'damaged ASAR');
   if (damage === 'version PE') await writeFile(runtime, 'damaged Electron PE');
   if (damage === 'missing version PE') await rm(runtime);
   if (damage === 'root Go') await writeFile(join(program, 'Dilla.exe'), 'damaged native Go PE');
   if (damage === 'missing root Go') await rm(join(program, 'Dilla.exe'));
   if (damage === 'missing updates') await rm(updates, { recursive: true });
   if (damage === 'all versions missing') await rm(join(program, 'versions'), { recursive: true });
   if (damage === 'all manifests missing') await rm(join(program, 'manifests'), { recursive: true });
   if (damage === 'torn published slot') { await writeFile(join(updates, 'windows-active-1.json'), '{torn'); assert.throws(() => run(join(program, 'Dilla.exe'), ['--state']), 'present corrupt slot must hold instead of selecting old'); }
   if (damage === 'nonterminal active') {
    const nonce = 'e'.repeat(64), ticket = join(updates, 'startup-' + nonce);
    await mkdir(ticket); await writeFile(join(ticket, 'old-slot.json'), await readFile(slot));
    await writeFile(join(ticket, 'journal.json'), JSON.stringify({ format: 1, nonce, phase: 'installing' }));
    await writeFile(join(updates, 'active.json'), JSON.stringify({ format: 1, nonce }));
    const beforeBlockedProgram = await snapshot(program), beforeBlockedProfile = await snapshot(profile);
    assert.throws(() => run(uninstaller, ['/S', '_?=' + program]), 'nonterminal update must block explicit removal');
    assert.deepEqual(await snapshot(program), beforeBlockedProgram); assert.deepEqual(await snapshot(profile), beforeBlockedProfile);
    await rm(join(updates, 'active.json')); await rm(ticket, { recursive: true });
   }
   run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'damaged ' + damage + ' removal');
   assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
   run(installer, ['/S']); assert.equal(await exists(slot), true, 'damaged ' + damage + ' reinstall');
   assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
  }
  run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'damaged fixture reinstall cleanup');
  // Simulate authenticated completed/failed transaction records using a fresh
  // fixture signing key, exact retained NSIS bytes and full immutable manifests.
  // Program damage must hold normal startup, while explicit removal remains safe.
  const nextRaw = await createWindowsProgramManifest(state.programDirectory, { version: '1.2.4', channel: 'stable', arch: 'x64' });
  const nextDigest = createHash('sha512').update(nextRaw).digest('base64'), nextHex = Buffer.from(nextDigest, 'base64').toString('hex');
  const nextTree = join(program, 'versions', 'v-' + nextHex.slice(0, 32)), nextManifest = join(program, 'manifests', nextHex + '.json');
  const installerHash = createHash('sha512'); for await (const chunk of createReadStream(installer)) installerHash.update(chunk);
  const installerDigest = installerHash.digest('base64'), installerSize = (await stat(installer)).size;
  const accepted = Date.now(), nonce = 'f'.repeat(64);
  const descriptor = Buffer.from(JSON.stringify({ format: 1, version: '1.2.4', channel: 'stable', publishedAt: new Date(accepted - 3_600_000).toISOString(), expiresAt: new Date(accepted + 86_400_000).toISOString(), artifacts: [{ platform: 'win32', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla-1.2.4-win-x64.exe', size: installerSize, sha512: installerDigest, programManifest: { url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.2.4/dilla-1.2.4-win-x64.manifest.json', size: nextRaw.length, sha512: nextDigest } }] }));
  const signature = sign(null, descriptor, releaseKeys.privateKey).toString('base64');
  for (const [phase, damage] of [['complete', 'new ASAR'], ['complete', 'new manifest'], ['complete', 'old tree'], ['complete', 'all manifests'], ['failed', 'old PE'], ['failed', 'old manifest'], ['failed', 'all versions']]) {
   if (!await exists(program)) run(installer, ['/S']);
   const oldSlot = await readFile(slot), ticket = join(updates, 'startup-' + nonce);
   await cp(tree, nextTree, { recursive: true }); await writeFile(nextManifest, nextRaw); await mkdir(ticket);
   await cp(installer, join(ticket, 'update.exe'));
   await writeFile(join(ticket, 'release.json'), descriptor); await writeFile(join(ticket, 'release.json.sig'), signature);
   await writeFile(join(ticket, 'old-slot.json'), oldSlot); await writeFile(join(ticket, 'program-manifest.json'), nextRaw);
   await writeFile(join(ticket, 'plan.json'), JSON.stringify({ verifiedAt: accepted }));
   await writeFile(join(ticket, 'journal.json'), JSON.stringify(phase === 'complete' ? { format: 1, nonce, phase, childPid: 0x7fffffff, version: '1.2.4' } : { format: 1, nonce, phase, failure: 'interrupted' }));
   if (phase === 'complete') await writeFile(join(updates, 'windows-active-1.json'), JSON.stringify({ format: 1, kind: 'release', generation: 1, version: '1.2.4', manifestSha512: nextDigest, nonce, acceptedAt: accepted }));
   await writeFile(join(updates, 'active.json'), JSON.stringify({ format: 1, nonce }));
   if (damage === 'new ASAR') await writeFile(join(nextTree, 'resources/app.asar'), 'damaged committed ASAR');
   if (damage === 'new manifest') await writeFile(nextManifest, '{damaged committed manifest');
   if (damage === 'old tree') await rm(tree, { recursive: true });
   if (damage === 'old PE') await writeFile(runtime, 'damaged previous PE');
   if (damage === 'old manifest') await writeFile(manifest, '{damaged previous manifest');
   if (damage === 'all versions') await rm(join(program, 'versions'), { recursive: true });
   if (damage === 'all manifests') await rm(join(program, 'manifests'), { recursive: true });
   assert.throws(() => run(join(program, 'Dilla.exe'), ['--state']), 'program damage must hold ordinary verification');
   run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'terminal ' + phase + '/' + damage + ' removal');
   assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
   run(installer, ['/S']); assert.equal(await exists(slot), true, 'terminal ' + phase + '/' + damage + ' reinstall');
   assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
  }
  run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'terminal signed fixture cleanup');
  const paused = await compile('dilla-fixture-interrupt.exe', source => source.replace('  CopyFiles /SILENT "$PLUGINSDIR\\payload\\initial-slot.json"', '  Sleep 300000\n  CopyFiles /SILENT "$PLUGINSDIR\\payload\\initial-slot.json"'));
  const interrupted = start(paused, ['/S']); processes.push(interrupted);
  await eventually(async () => await exists(join(profile, 'initial-install.json')) && await exists(join(program, 'Dilla.exe')) && await exists(updates) && !await exists(slot), 'durable owned marker and copied program before initial slot commit', 120_000);
  await stop(interrupted);
  const partialProgram = await snapshot(program), partialProfile = await snapshot(profile);
  await rm(join(temporary, 'fixture-install-diagnostic.json'), { force: true });
  assert.throws(() => run(installer, ['/S'], { env: { ...process.env, DILLA_FIXTURE_INITIAL_PUBLISH_DURING_SCAN: '1' } }));
  assert.deepEqual(await snapshot(program), partialProgram);
  try { assert.equal(JSON.parse(await readFile(join(updates, 'active.json'))).nonce, 'c'.repeat(64)); }
  catch (error) { await reportInitialFailure('INITIAL_RETRY_FAILURE'); throw error; }
  await rm(join(updates, 'active.json')); await rm(join(updates, 'startup-' + 'c'.repeat(64)), { recursive: true });
  assert.deepEqual(await snapshot(profile), partialProfile);
  run(installer, ['/S']);
  assert.equal(await exists(slot), true, 'same installer must recover owned interrupted first copy');
  assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
  run(uninstaller, ['/S']); await eventually(async () => !await exists(program), 'interrupted retry cleanup');
  const flushFailure = await compile('dilla-fixture-flush-failure.exe', withInitialSlotFlushFailure);
  assert.throws(() => run(flushFailure, ['/S']), 'injected seed flush failure must fail setup');
  assert.equal(await exists(join(profile, 'initial-install.json')), true);
  run(installer, ['/S']);
  assert.equal(await exists(slot), true, 'failed initial flush must leave a safe retry');
  assert.equal(await readFile(account, 'utf8'), 'existing account / KEK / OPFS / preferences must survive');
 } finally {
  for (const child of processes) await stop(child).catch(() => {});
  if (uninstaller && await exists(uninstaller)) { try { run(uninstaller, ['/S']); } catch {} }
  // These roots are generated fixture identities, never runtime-selected product paths.
  await rm(program, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  for (const entry of await readdir(join(process.env.LOCALAPPDATA, 'Programs')).catch(() => [])) if (entry.startsWith(productName + '-incomplete-{')) await rm(join(process.env.LOCALAPPDATA, 'Programs', entry), { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  await rm(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
  await rm(temporary, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 });
 }
});
