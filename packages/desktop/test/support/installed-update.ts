import { fixtureRuntime, fixtureSandbox, fixtureSandboxCache } from './fixture-update-runtime.ts';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, rm, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import type { InstalledUpdatePlan, UpdateTrust } from '../../src/update-plan.ts';
const {prepareInstalledUpdate} = fixtureRuntime.plan;

export async function installedFixture(run: (plan: InstalledUpdatePlan, trust: UpdateTrust, root: string) => Promise<void>, platform: 'linux' | 'win32' = 'linux', oldProgram: string | Uint8Array = 'working-old-app') {
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-watchdog-')));
 try {
  const keys = generateKeyPairSync('ed25519'), bytes = Buffer.from('authenticated-new-app');
  const installed = join(root, 'applications', platform === 'linux' ? 'Dilla.AppImage' : 'dilla-desktop.exe'), downloaded = join(root, 'update-download');
  await mkdir(join(root, 'applications')); await writeFile(installed, oldProgram, { mode: 0o700 }); await writeFile(downloaded, bytes);
  if (platform === 'win32') { await mkdir(join(root, 'applications/resources')); await writeFile(join(root, 'applications/resources/app.asar'), 'old-asar'); }
  const now = Date.now(), sandbox = platform === 'linux' ? fixtureSandbox('1.0.1') : undefined;
  const metadata = Buffer.from(JSON.stringify({ format: 1, ...(sandbox ? {assets:sandbox.assets} : {}), version: '1.0.1', channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [{ platform, arch: 'x64', url: `https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1${platform === 'linux' ? '-linux-x64.AppImage' : '.exe'}`, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64'), ...(sandbox ? {linuxSandbox:sandbox.descriptor} : {}) }] }));
  const trust: UpdateTrust = { publicKeys: [keys.publicKey.export({ type: 'spki', format: 'pem' }).toString()], platform, arch: 'x64', version: '1.0.0', channel: 'stable', now };
  const userData = join(root, 'profile'); await mkdir(userData); await writeFile(join(userData, 'device-sentinel'), 'encrypted-device');
  const sandboxDirectory = sandbox ? await fixtureSandboxCache(userData) : undefined;
  const plan = await prepareInstalledUpdate({ userData, installed, downloaded, metadata, signature: sign(null, metadata, keys.privateKey).toString('base64'), parentPid: process.pid, sandboxDirectory }, trust);
  await run(plan, trust, root);
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }); }
}
