import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, readFile, writeFile, rm, symlink, rename, readdir, lstat } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { prepareInstalledUpdate, readInstalledUpdate } from '../src/update-plan.ts';
import { updateFS } from '../src/update-fs.ts';

type Options = Parameters<typeof prepareInstalledUpdate>[0];
type Trust = Parameters<typeof prepareInstalledUpdate>[1];
async function fixture(run: (options: Options, trust: Trust, root: string) => Promise<void>, platform: 'linux' | 'win32' = 'linux') {
 const root = await mkdtemp(join(tmpdir(), 'dilla-update-plan-'));
 try {
  const keys = generateKeyPairSync('ed25519'), bytes = Buffer.from('authenticated-new-AppImage');
  const suffix = platform === 'linux' ? '.AppImage' : '.exe';
  const installed = join(root, 'applications/dilla' + suffix), downloaded = join(root, 'cache/new' + suffix);
  await mkdir(join(root, 'applications')); await mkdir(join(root, 'cache')); await writeFile(installed, 'working-old-AppImage', { mode: 0o700 }); await writeFile(downloaded, bytes);
  if (platform === 'win32') { await mkdir(join(root, 'applications/resources')); await writeFile(join(root, 'applications/resources/app.asar'), 'working-old-ASAR'); await writeFile(join(root, 'applications/icudtl.dat'), 'working-old-runtime'); await mkdir(join(root, 'applications/resources/app.asar.unpacked')); await writeFile(join(root, 'applications/resources/app.asar.unpacked/native.node'), 'working-old-native'); }
  const artifact = { platform, arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-' + (platform === 'linux' ? 'linux-x64.AppImage' : 'win-x64.exe'), size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
  const now = Date.now(), metadata = Buffer.from(JSON.stringify({ format: 1, version: '1.0.1', channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [artifact] }));
  const trust = { publicKeys: [keys.publicKey.export({ type: 'spki', format: 'pem' }).toString()], platform, arch: 'x64', version: '1.0.0', channel: 'stable' as const, now };
  await run({ userData: join(root, 'profile'), installed, downloaded, metadata, signature: sign(null, metadata, keys.privateKey).toString('base64'), parentPid: process.pid }, trust, root);
 } finally { await rm(root, { recursive: true, force: true }); }
}

test('preparation stages verified bytes and a working helper without changing the installed app or device data', async () => fixture(async (options, trust) => {
 await mkdir(options.userData); await writeFile(join(options.userData, 'device-sentinel'), 'encrypted-device-data');
 const plan = await prepareInstalledUpdate(options, trust);
 assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
 assert.equal(await readFile(join(options.userData, 'device-sentinel'), 'utf8'), 'encrypted-device-data');
 assert.equal(await readFile(plan.artifactFile, 'utf8'), 'authenticated-new-AppImage');
 assert.equal(await readFile(plan.watchdog, 'utf8'), 'working-old-AppImage');
 const accepted = await readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog });
 assert.equal(accepted.release.version, '1.0.1');
 assert.equal(accepted.installed, options.installed);
}));

test('the helper rejects forged metadata, modified staged bytes, a different helper and a moved working installation', async () => fixture(async (options, trust, root) => {
 await assert.rejects(prepareInstalledUpdate({ ...options, signature: 'A'.repeat(86) + '==' }, trust), /signature/);
 const plan = await prepareInstalledUpdate(options, trust);
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: join(root, 'other.AppImage') }), /helper/);
 await writeFile(plan.artifactFile, 'tampered');
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /verification/);
 await writeFile(plan.artifactFile, await readFile(options.downloaded));
 await writeFile(options.installed, 'another-installation');
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /working installation/);
}));

test('a plan cannot overlap the device-data directory or be redirected by editing its paths', async () => fixture(async (options, trust, root) => {
 await assert.rejects(prepareInstalledUpdate({ ...options, userData: options.installed }, trust), /overlap/);
 const plan = await prepareInstalledUpdate(options, trust);
 const file = join(plan.health.directory, 'plan.json'), json = JSON.parse(await readFile(file, 'utf8'));
 json.artifactFile = options.downloaded;
 await writeFile(file, JSON.stringify(json));
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /paths/);
}));


test('Windows keeps an independent renamed watchdog and fingerprints the ASAR as well as Electron', async () => fixture(async (options, trust, root) => {
 const plan = await prepareInstalledUpdate(options, trust);
 assert.equal(plan.watchdog, join(plan.backup, 'dilla-update-watchdog.exe'));
 assert.equal(await readFile(join(plan.backup, 'resources/app.asar'), 'utf8'), 'working-old-ASAR');
 assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
 await readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog });
 // Updating only app.asar leaves electron.exe unchanged; an old plan must still
 // be refused, rather than replayed against a different installed release.
 await writeFile(join(root, 'applications/resources/app.asar'), 'another-installed-version');
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /working installation/);
}, 'win32'));


test('Linux accepts an AppImage beside the home config directory', async () => fixture(async (options, trust, root) => {
 const installed = join(root, 'Dilla.AppImage'), userData = join(root, '.config/Dilla');
 await rename(options.installed, installed);
 const plan = await prepareInstalledUpdate({ ...options, installed, userData }, trust);
 await readInstalledUpdate(userData, plan.health.nonce, { ...trust, executable: plan.watchdog });
 assert.equal(await readFile(installed, 'utf8'), 'working-old-AppImage');
}));

test('Windows refuses device data anywhere inside its program tree', async () => fixture(async (options, trust, root) => {
 await assert.rejects(prepareInstalledUpdate({ ...options, userData: join(root, 'applications/profile') }, trust), /overlap/);
}, 'win32'));

for (const entry of ['icudtl.dat', 'resources/app.asar.unpacked/native.node']) {
 test(`Windows refuses a damaged rollback runtime file: ${entry}`, async () => fixture(async (options, trust) => {
  const plan = await prepareInstalledUpdate(options, trust);
  await writeFile(join(plan.backup, entry), 'damaged-runtime');
  await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /working installation|helper/);
 }, 'win32'));
}

test('Windows refuses a linked staged installer even when its target has authenticated bytes', async t => fixture(async (options, trust) => {
 const plan = await prepareInstalledUpdate(options, trust);
 await rm(plan.artifactFile);
 try { await symlink(options.downloaded, plan.artifactFile); }
 catch (error) {
  if (process.platform === 'win32' && (error as NodeJS.ErrnoException).code === 'EPERM') { t.skip('File symlinks require Windows developer mode or admin rights'); return; }
  throw error;
 }
 await assert.rejects(readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog }), /verification|link|artifact/);
}, 'win32'));

// Fault only the external flush boundary. Reads, copies, hashing, cleanup, and
// plan preparation remain real; omitting a required flush makes preparation
// resolve and fails the rejection assertion.
for (const target of ['update.exe', 'previous-app/dilla.exe', 'previous-app/icudtl.dat', 'previous-app/resources/app.asar', 'previous-app/resources/app.asar.unpacked/native.node', 'previous-app/dilla-update-watchdog.exe', ...(process.platform === 'win32' ? [] : ['previous-app/resources', 'previous-app', 'startup', 'updates', 'profile'])]) {
 test(`a failed preparation flush leaves the working installation untouched: ${target}`, async t => fixture(async (options, trust) => {
  const original = updateFS.promises.open;
  t.mock.method(updateFS.promises, 'open', async (...args: Parameters<typeof original>) => {
   const handle = await original(...args), path = String(args[0]).replaceAll('\\', '/');
   if ((target === 'startup' && /\/startup-[a-f0-9]{64}$/.test(path)) || path.endsWith('/' + target)) {
    handle.sync = async () => { throw new Error('injected flush failure'); };
   }
   return handle;
  });
  try {
   await assert.rejects(prepareInstalledUpdate(options, trust), /injected flush failure/);
   assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
   assert.equal(await readFile(join(options.installed, '..', 'icudtl.dat'), 'utf8'), 'working-old-runtime');
   assert.deepEqual(await readdir(join(options.userData, 'updates')), []);
  } finally { t.mock.restoreAll(); }
 }, 'win32'));
}


for (const target of ['update.AppImage', 'previous.AppImage']) {
 test(`a failed Linux preparation flush retains the working AppImage: ${target}`, async t => fixture(async (options, trust) => {
  const original = updateFS.promises.open;
  t.mock.method(updateFS.promises, 'open', async (...args: Parameters<typeof original>) => {
   const handle = await original(...args);
   if (String(args[0]).replaceAll('\\', '/').endsWith('/' + target)) handle.sync = async () => { throw new Error('injected flush failure'); };
   return handle;
  });
  try {
   await assert.rejects(prepareInstalledUpdate(options, trust), /injected flush failure/);
   assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
   assert.deepEqual(await readdir(join(options.userData, 'updates')), []);
  } finally { t.mock.restoreAll(); }
 }));
}


for (const existing of [true, false]) {
 test(`a Windows profile alias into the program tree is rejected before staging (profile exists: ${existing})`, async t => fixture(async (options, trust, root) => {
  const programProfile = join(root, 'applications/device-profile');
  if (existing) await mkdir(programProfile);
  const alias = join(root, 'profile-link');
  // An ancestor alias also covers a profile that does not exist yet.
  await symlink(join(root, 'applications'), alias, process.platform === 'win32' ? 'junction' : 'dir');
  const before = await readdir(join(root, 'applications'));
  const original = updateFS.promises.open;
  t.mock.method(updateFS.promises, 'open', async (...args: Parameters<typeof original>) => {
   if (String(args[0]).includes('startup-')) throw new Error('staging began inside the program tree');
   return original(...args);
  });
  try {
   await assert.rejects(prepareInstalledUpdate({ ...options, userData: join(alias, 'device-profile') }, trust), /overlap/);
   assert.deepEqual(await readdir(join(root, 'applications')), before);
   if (existing) assert.deepEqual(await readdir(programProfile), []);
   else await assert.rejects(lstat(programProfile), { code: 'ENOENT' });
   assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
  } finally { t.mock.restoreAll(); }
 }, 'win32'));
}

for (const existing of [true, false]) {
 test(`an honest profile alias prepares and reads one canonical plan (profile exists: ${existing})`, async () => fixture(async (options, trust, root) => {
  const profiles = join(root, 'profiles'); await mkdir(profiles);
  const userData = join(profiles, 'Dilla');
  if (existing) await mkdir(userData);
  const alias = join(root, 'profile-link');
  await symlink(profiles, alias, process.platform === 'win32' ? 'junction' : 'dir');
  const profileAlias = join(alias, 'Dilla');
  const plan = await prepareInstalledUpdate({ ...options, userData: profileAlias }, trust);
  assert.equal(plan.userData, userData);
  const canonical = await readInstalledUpdate(userData, plan.health.nonce, { ...trust, executable: plan.watchdog });
  const aliased = await readInstalledUpdate(profileAlias, plan.health.nonce, { ...trust, executable: plan.watchdog });
  assert.deepEqual(aliased, canonical);
  assert.equal(await readFile(options.installed, 'utf8'), 'working-old-AppImage');
 }, 'win32'));
}
