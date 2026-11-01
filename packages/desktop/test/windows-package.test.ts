import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, mkdir, writeFile, readFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { createHash, generateKeyPairSync } from 'node:crypto';
// @ts-expect-error build tooling is JavaScript
import { finalizeWindowsProgram, assertWindowsProgramUnchanged } from '../scripts/windows-package.mjs';
test('post-sign packaging hashes final Electron bytes before compiling an independent stable launcher and guards against later mutation', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-windows-package-')), app = join(root, 'win-unpacked');
 try {
  await mkdir(join(app, 'resources'), { recursive: true });
  await writeFile(join(app, 'dilla-desktop.exe'), 'final PE metadata + fuses + signing'); await writeFile(join(app, 'resources/app.asar'), 'raw embedded ASAR integrity bytes');
  let config!: { initialManifestSha512: string };
  const state = await finalizeWindowsProgram({ appOutDir: app, outDir: root, version: '1.0.1', arch: 'x64', productName: 'DillaFixture', appId: 'dev.dilla.fixture', publicKeys: [generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString()],
   compileLauncher: async (options: { file: string; config: { initialManifestSha512: string } }) => { config = options.config; await writeFile(options.file, 'independent native launcher ' + options.config.initialManifestSha512); }, signLauncher: () => Promise.resolve() });
  const manifest = await readFile(state.manifestFile), digest = createHash('sha512').update(manifest).digest('base64');
  assert.equal(config.initialManifestSha512, digest); assert.equal(state.programDirectory, join(app, 'versions', 'v-' + Buffer.from(digest, 'base64').toString('hex').slice(0, 32)));
  assert.equal(await readFile(join(state.programDirectory, 'dilla-desktop.exe'), 'utf8'), 'final PE metadata + fuses + signing');
  assert.ok((await readFile(join(app, 'Dilla.exe'), 'utf8')).includes(digest));
  assert.deepEqual(JSON.parse(await readFile(join(app, 'initial-slot.json'), 'utf8')), { format: 1, kind: 'initial', generation: 0, version: '1.0.1', manifestSha512: digest });
  await assertWindowsProgramUnchanged(state);
  const launcher = await readFile(join(app, 'Dilla.exe')); await writeFile(join(app, 'Dilla.exe'), 'replaced stable bootstrap');
  await assert.rejects(assertWindowsProgramUnchanged(state), /bootstrap|launcher/); await writeFile(join(app, 'Dilla.exe'), launcher);
  const initial = await readFile(join(app, 'initial-slot.json')); await writeFile(join(app, 'initial-slot.json'), 'replaced initial slot');
  await assert.rejects(assertWindowsProgramUnchanged(state), /bootstrap|slot/); await writeFile(join(app, 'initial-slot.json'), initial);
  await writeFile(join(state.programDirectory, 'dilla-desktop.exe'), 'unexpected later PE mutation');
  await assert.rejects(assertWindowsProgramUnchanged(state), /changed|manifest/);
 } finally { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
});

test('packaging rejects private signing material before writing it into the native bootstrap', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-private-bootstrap-'));
 try {
  const key = generateKeyPairSync('ed25519').privateKey.export({ type:'pkcs8',format:'pem' }).toString();
  await assert.rejects(finalizeWindowsProgram({ appOutDir: root, outDir: root, version:'1.0.0', arch:'x64', productName:'Dilla', appId:'dev.dilla.desktop', publicKeys:[key],compileLauncher:()=>assert.fail('private key reached compiler') }), /identity|trust|public/i);
 } finally { await rm(root, { recursive:true,force:true }); }
});
