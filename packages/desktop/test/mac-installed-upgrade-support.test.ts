import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer, get } from 'node:http';
import { mkdtemp, mkdir, writeFile, chmod, symlink, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { bundleDigest, serveArchive } from './support/mac-installed-upgrade.ts';

test('Mac fault transport changes signed archive bytes and interrupts an advertised complete download', async () => {
  const bytes = Buffer.from('a complete authenticated archive fixture');
  let fault: 'none' | 'archive' | 'download' = 'archive';
  const server = createServer((_request, response) => serveArchive(response, bytes, fault));
  await new Promise<void>(done => server.listen(0, '127.0.0.1', done));
  const address = server.address();
  assert.ok(address && typeof address !== 'string');
  const request = () => new Promise<{ bytes: Buffer; complete: boolean; expected: number }>((done, reject) => {
    get(`http://127.0.0.1:${address.port}`, response => {
      const chunks: Buffer[] = [];
      response.on('data', chunk => chunks.push(chunk));
      response.on('error', () => {}); // An interrupted response completes through close.
      response.on('close', () => done({ bytes: Buffer.concat(chunks), complete: response.complete, expected: Number(response.headers['content-length']) }));
    }).on('error', reject);
  });
  try {
    const altered = await request();
    assert.equal(altered.complete, true);
    assert.equal(altered.bytes.length, bytes.length);
    assert.notDeepEqual(altered.bytes, bytes);
    fault = 'download';
    const interrupted = await request();
    assert.equal(interrupted.expected, bytes.length);
    assert.equal(interrupted.complete, false);
    assert.ok(interrupted.bytes.length > 0 && interrupted.bytes.length < bytes.length);
    assert.deepEqual(interrupted.bytes, bytes.subarray(0, interrupted.bytes.length));
    fault = 'none';
    const retry = await request();
    assert.equal(retry.complete, true);
    assert.deepEqual(retry.bytes, bytes);
    assert.equal(bytes.toString(), 'a complete authenticated archive fixture', 'fault injection must not poison the retry fixture');
  } finally { await new Promise<void>(done => server.close(() => done())); }
});

test('Mac installed bundle fingerprint detects changed resources, executable modes, symlinks and added files', { skip: process.platform === 'win32' }, async () => {
  const root = await mkdtemp(join(tmpdir(), 'dilla-bundle-digest-'));
  try {
    await mkdir(join(root, 'Contents'));
    const file = join(root, 'Contents/main');
    await writeFile(file, 'working app', { mode: 0o700 });
    await symlink('main', join(root, 'Contents/current'));
    const original = await bundleDigest(root);
    await writeFile(file, 'altered app');
    assert.notEqual(await bundleDigest(root), original);
    await writeFile(file, 'working app');
    assert.equal(await bundleDigest(root), original);
    await chmod(file, 0o600);
    assert.notEqual(await bundleDigest(root), original);
    await chmod(file, 0o700);
    await rm(join(root, 'Contents/current'));
    await symlink('missing', join(root, 'Contents/current'));
    assert.notEqual(await bundleDigest(root), original);
    await rm(join(root, 'Contents/current'));
    await symlink('main', join(root, 'Contents/current'));
    assert.equal(await bundleDigest(root), original);
    await writeFile(join(root, 'Contents/partial-install'), 'unexpected');
    assert.notEqual(await bundleDigest(root), original);
  } finally { await rm(root, { recursive: true, force: true }); }
});
