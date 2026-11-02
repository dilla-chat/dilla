import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { verifyArtifactFile } from '../src/appimage.ts';

test('native install cache verification rejects modified size or bytes', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-artifact-')), file = join(root, 'update');
 const bytes = Buffer.from('verified installer');
 const artifact = { size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
 try {
  await writeFile(file, bytes); await verifyArtifactFile(file, artifact);
  await writeFile(file, Buffer.alloc(bytes.length)); await assert.rejects(verifyArtifactFile(file, artifact), /hash/);
  await writeFile(file, 'short'); await assert.rejects(verifyArtifactFile(file, artifact), /size/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
