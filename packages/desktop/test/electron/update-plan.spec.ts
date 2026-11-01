import { test, expect, _electron, type ElectronApplication } from '@playwright/test';
import { build } from 'esbuild';
import { createPackage } from '@electron/asar';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

test('installed update preparation preserves the real ASAR archive through Electron filesystem virtualization', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-update-electron-'));
 let application: ElectronApplication | undefined;
 try {
  const profile = join(root, 'profile'), program = join(root, 'program'), resources = join(program, 'resources');
  await mkdir(profile); await mkdir(resources, { recursive: true }); await mkdir(join(root, 'archive-source'));
  await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: [], autostart: false }));
  await writeFile(join(root, 'archive-source/package.json'), JSON.stringify({ name: 'dilla-backup-fixture', version: '1.0.0' }));
  await createPackage(join(root, 'archive-source'), join(resources, 'app.asar'));
  const old = await readFile(join(resources, 'app.asar'));
  await writeFile(join(program, 'dilla.exe'), 'old-working-executable');
  const bytes = Buffer.from('new-authenticated-installer'), downloaded = join(root, 'update.exe'); await writeFile(downloaded, bytes);
  const keys = generateKeyPairSync('ed25519'), now = Date.now();
  const metadata = Buffer.from(JSON.stringify({ format: 1, version: '1.0.1', channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [{ platform: 'win32', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-win-x64.exe', size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') }] }));
  const bundle = join(root, 'update-plan.cjs');
  await build({ entryPoints: [resolve('src/update-plan.ts')], outfile: bundle, bundle: true, platform: 'node', target: 'node24', format: 'cjs' });
  application = await _electron.launch({ chromiumSandbox: true, args: [resolve('.'), '--profile=' + profile] });
  await application.firstWindow();
  const result = await application.evaluate(async (_electron, options) => {
   const load = process.getBuiltinModule('module').createRequire(options.bundle);
   const { prepareInstalledUpdate, readInstalledUpdate } = load(options.bundle);
   const trust = { publicKeys: [options.key], platform: 'win32', arch: 'x64', version: '1.0.0', channel: 'stable', now: options.now };
   const plan = await prepareInstalledUpdate({ userData: options.userData, installed: options.installed, downloaded: options.downloaded, metadata: Buffer.from(options.metadata, 'base64'), signature: options.signature, parentPid: process.pid }, trust);
   const verified = await readInstalledUpdate(options.userData, plan.health.nonce, { ...trust, executable: plan.watchdog });
   return { version: verified.release.version, archive: plan.backup + '/resources/app.asar' };
  }, { bundle, installed: join(program, 'dilla.exe'), userData: join(root, 'device-data'), downloaded, key: keys.publicKey.export({ type: 'spki', format: 'pem' }).toString(), metadata: metadata.toString('base64'), signature: sign(null, metadata, keys.privateKey).toString('base64'), now });
  expect(result.version).toBe('1.0.1');
  expect(await readFile(result.archive)).toEqual(old);
  expect(await readFile(join(resources, 'app.asar'))).toEqual(old);
 } finally { await application?.close(); await rm(root, { recursive: true, force: true }); }
});
