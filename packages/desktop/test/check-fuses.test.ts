import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
test('fuse checker selects the stock Windows Electron executable', async () => {
 // @ts-expect-error build tool is JavaScript
 const { desktopFuseTargets } = await import('../scripts/check-fuses.mjs');
 const root = await mkdtemp(join(tmpdir(), 'dilla-fuse-target-'));
 try {
  await mkdir(join(root, 'win-unpacked'));
  await writeFile(join(root, 'win-unpacked/dilla-desktop.exe'), 'Electron PE fuse wire');
  assert.deepEqual(await desktopFuseTargets(root), [join(root, 'win-unpacked/dilla-desktop.exe')]);
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
