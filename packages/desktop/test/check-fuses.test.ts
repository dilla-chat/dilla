import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile, realpath } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { generateKeyPairSync } from 'node:crypto';
// @ts-expect-error build tool is JavaScript
import { finalizeWindowsProgram } from '../scripts/windows-package.mjs';
test('fuse checker selects manifest-bound immutable Electron and rejects changed manifest/tree/seed or ambiguous versions', async () => {
 // @ts-expect-error build tool is JavaScript
 const { desktopFuseTargets } = await import('../scripts/check-fuses.mjs');
 const root = await realpath(await mkdtemp(join(tmpdir(), 'dilla-fuse-target-'))), app = join(root, 'win-unpacked');
 try {
  await mkdir(join(app, 'resources'), { recursive: true });
  await writeFile(join(app, 'dilla-desktop.exe'), 'Electron PE fuse wire'); await writeFile(join(app, 'resources/app.asar'), 'ASAR');
  const state = await finalizeWindowsProgram({ appOutDir: app, outDir: root, version: '1.2.3', arch: 'x64', productName: 'DillaFixture', appId: 'dev.dilla.fixture', publicKeys: [generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString()], compileLauncher: async ({ file }: { file: string }) => writeFile(file, 'native Go has no Electron fuse wire') });
  const describeWindows = async () => ({ format: 1, ...state.config, initialManifestSha512: state.digest, trustHash: 'a'.repeat(64) });
  const options = { describeWindows };
  assert.deepEqual(await desktopFuseTargets(root, options), [join(state.programDirectory, 'dilla-desktop.exe')]);
  const seed = await readFile(join(app, 'initial-slot.json'));
  await writeFile(join(app, 'initial-slot.json'), seed.toString().replace('"generation":0', '"generation":1'));
  await assert.rejects(desktopFuseTargets(root, options), /initial|slot|bootstrap/);
  await writeFile(join(app, 'initial-slot.json'), seed);
  await mkdir(join(app, 'versions/v-' + 'b'.repeat(32)));
  await assert.rejects(desktopFuseTargets(root, options), /one|unique|version/);
  await rm(join(app, 'versions/v-' + 'b'.repeat(32)), { recursive: true });
  await writeFile(join(state.programDirectory, 'dilla-desktop.exe'), 'changed PE');
  await assert.rejects(desktopFuseTargets(root, options), /changed|manifest/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
test('fuse target discovery preserves Linux and macOS binary selection', async () => {
 // @ts-expect-error build tool is JavaScript
 const { desktopFuseTargets } = await import('../scripts/check-fuses.mjs');
 const root = await mkdtemp(join(tmpdir(), 'dilla-fuse-other-'));
 try {
  await mkdir(join(root, 'linux-unpacked')); await mkdir(join(root, 'mac/Dilla.app/Contents/MacOS'), { recursive: true });
  await writeFile(join(root, 'mac/Dilla.app/Contents/MacOS/dilla-desktop'), 'mac');
  assert.deepEqual(await desktopFuseTargets(root), [join(root, 'linux-unpacked/dilla-desktop'), join(root, 'mac/Dilla.app/Contents/MacOS/dilla-desktop')]);
 } finally { await rm(root, { recursive: true, force: true }); }
});
