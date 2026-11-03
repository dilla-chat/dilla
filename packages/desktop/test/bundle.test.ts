import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, mkdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createHash } from 'node:crypto';
import { loadBundle, bundledResponse } from '../src/bundle.ts';

test('bundled documents and scripts never come from the instance; APIs use a separate path', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'dilla-bundle-'));
  try {
    await mkdir(join(dir, 'assets'));
    const data = { 'index.html': '<html>installed client</html>', 'assets/entry.js': 'globalThis.bundled = true;' };
    for (const [name, body] of Object.entries(data)) await writeFile(join(dir, name), body);
    await writeFile(join(dir, 'dilla-manifest.json'), JSON.stringify({ v: 1, files: Object.entries(data).map(([path, body]) => ({ path, size: Buffer.byteLength(body), sha256: createHash('sha256').update(body).digest('hex') })) }));
    const bundle = await loadBundle(dir);
    const page = bundledResponse(bundle, new Request('https://chat.test/settings/voice'), 'https://chat.test/');
    assert.equal(await page!.text(), '<html>installed client</html>');
    assert.match(page!.headers.get('content-security-policy')!, /script-src https:\/\/chat.test\/assets\/ 'wasm-unsafe-eval'/);
    assert.match(page!.headers.get('content-security-policy')!, /frame-src 'none'/);
    assert.equal(await bundledResponse(bundle, new Request('https://chat.test/assets/entry.js'), 'https://chat.test/')!.text(), data['assets/entry.js']);
    assert.equal(bundledResponse(bundle, new Request('https://chat.test/v1/instance'), 'https://chat.test/'), null);
    assert.equal(bundledResponse(bundle, new Request('https://chat.test/assets/server-injected.js'), 'https://chat.test/')!.status, 404);
    assert.equal(bundledResponse(bundle, new Request('https://chat.test/unknown'), 'https://chat.test/')!.status, 404);
    assert.equal(bundledResponse(bundle, new Request('https://evil.test/assets/code.js'), 'https://chat.test/')!.status, 403);
    // A conflicting base path must never turn the CSP's executable asset namespace into a server fallback.
    assert.equal(bundledResponse(bundle, new Request('https://chat.test/assets/v1/evil.js'), 'https://chat.test/assets/')?.status, 404);
    await writeFile(join(dir, 'assets/entry.js'), 'tampered');
    await assert.rejects(loadBundle(dir), /integrity/);
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test('a manifest cannot escape the installed bundle or list executable HTML outside the entry', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'dilla-manifest-'));
  try {
    for (const path of ['../outside.js', '/absolute.js', 'assets/../../evil.js', 'assets\\evil.js', 'assets/evil.html']) {
      await writeFile(join(dir, 'dilla-manifest.json'), JSON.stringify({ v: 1, files: [{ path, size: 0, sha256: '0'.repeat(64) }] }));
      await assert.rejects(loadBundle(dir));
    }
  } finally { await rm(dir, { recursive: true, force: true }); }
});
