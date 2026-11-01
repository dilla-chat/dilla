import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, stat, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createPrivateKey, createPublicKey, sign, verify } from 'node:crypto';
// @ts-expect-error build tooling is JavaScript
import { createReleaseKeys } from '../scripts/release-keys.mjs';

test('release keys stay private, are separate, and cannot overwrite installed trust', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-release-keys-'));
 try {
  const result = await createReleaseKeys(root);
  const trust = JSON.parse(await readFile(join(root, 'release-trust.json'), 'utf8')) as { metadataPublicKeys: string[]; sparklePublicKey: string };
  const privateKey = createPrivateKey(await readFile(join(root, 'metadata-private.pem')));
  const bytes = Buffer.from('release descriptor');
  assert.ok(verify(null, bytes, trust.metadataPublicKeys[0], sign(null, bytes, privateKey)));
  const sparkle = Buffer.from((await readFile(join(root, 'sparkle-private.key'), 'utf8')).trim(), 'base64');
  assert.equal(sparkle.length, 64);
  assert.equal(sparkle.subarray(32).toString('base64'), trust.sparklePublicKey);
  assert.notEqual(createPublicKey(privateKey).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'), trust.sparklePublicKey);
  assert.match(result.metadataFingerprint, /^[a-f0-9]{64}$/);
  if (process.platform !== 'win32') assert.equal((await stat(join(root, 'metadata-private.pem'))).mode & 0o777, 0o600);
  await assert.rejects(createReleaseKeys(root), /EEXIST/);
 } finally { await rm(root, { recursive: true, force: true }); }
});
