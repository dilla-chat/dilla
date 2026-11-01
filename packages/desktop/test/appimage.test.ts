import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { replaceAppImage } from '../src/appimage.ts';

test('AppImage replacement preserves its filename/autostart and a working backup; failed launch rolls back', { skip: process.platform === 'win32' }, async () => {
  const dir = await mkdtemp(join(tmpdir(), 'dilla-appimage-'));
  const destination = join(dir, 'Dilla-0.1.0.AppImage');
  const source = join(dir, 'Dilla-0.1.1.AppImage');
  const bytes = Buffer.from('new verified app');
  const artifact = { size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
  try {
    await writeFile(destination, 'old working app'); await writeFile(source, bytes);
    await assert.rejects(replaceAppImage(source, destination, artifact, async () => { throw new Error('exec failed'); }), /exec failed/);
    assert.equal(await readFile(destination, 'utf8'), 'old working app');
    let launched = '';
    await replaceAppImage(source, destination, artifact, async path => { launched = path; });
    assert.equal(launched, destination);
    assert.equal(await readFile(destination, 'utf8'), 'new verified app');
    assert.equal(await readFile(destination + '.previous', 'utf8'), 'old working app');
    await writeFile(source, 'tampered cache');
    await assert.rejects(replaceAppImage(source, destination, artifact, async () => {}), /verification/);
    assert.equal(await readFile(destination, 'utf8'), 'new verified app');
  } finally { await rm(dir, { recursive: true, force: true }); }
});
