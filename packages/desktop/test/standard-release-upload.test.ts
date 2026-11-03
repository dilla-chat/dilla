import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
// @ts-expect-error publishing tooling is JavaScript
import { createReleaseAssetUploader } from '../scripts/standard-release-upload.mjs';

const repository = 'dilla-chat/dilla';
const bytes = Buffer.from('accepted native installer\0\xff', 'latin1');
const digest = 'sha256:' + createHash('sha256').update(bytes).digest('hex');
const draft = (tag = 'desktop-v1.2.3') => ({ id: 42, tag_name: tag, draft: true, upload_url: `https://uploads.github.com/repos/${repository}/releases/42/assets{?name,label}` });
type Asset = { id: number; name: string; size: number; digest: string };
const storeSent = (sent: Buffer): Asset[] => [{ id: 8, name: 'dilla.exe', size: sent.length, digest: 'sha256:' + createHash('sha256').update(sent).digest('hex') }];

function fixture({ initial = [] as Asset[], store = storeSent, status = 201 } = {}) {
 const events: string[] = [];
 let stored = initial;
 const api = async (path: string, options: { method?: string } = {}) => {
  events.push(`${options.method ?? 'GET'} ${path}`);
  if (options.method === 'DELETE') { stored = stored.filter(asset => path !== `/repos/${repository}/releases/assets/${asset.id}`); return null; }
  assert.equal(path, `/repos/${repository}/releases/42/assets?per_page=100&page=1`);
  return stored;
 };
 const fetchImpl = async (url: string, init: { redirect: string; headers: Record<string, string>; body: Buffer }) => {
  events.push(`UPLOAD ${url}`);
  assert.equal(init.redirect, 'error');
  assert.equal(init.headers.Authorization, 'Bearer workflow-token');
  assert.deepEqual(init.body, bytes, 'exact accepted bytes');
  if (status === 201) stored = store(init.body);
  return new Response('{}', { status });
 };
 return { events, upload: createReleaseAssetUploader({ api, token: 'workflow-token', fetchImpl }) };
}
const uploadUrl = `UPLOAD https://uploads.github.com/repos/${repository}/releases/42/assets?name=dilla.exe`;
const list = `GET /repos/${repository}/releases/42/assets?per_page=100&page=1`;

test('uploads exact bytes to uploads.github.com and checks the stored digest', async () => {
 const { upload, events } = fixture();
 await upload(draft(), 'dilla.exe', bytes);
 assert.deepEqual(events, [list, uploadUrl, list]);
});

test('an identical asset is idempotent; a differing draft asset is deleted before upload', async () => {
 const same = fixture({ initial: [{ id: 7, name: 'dilla.exe', size: bytes.length, digest }] });
 await same.upload(draft(), 'dilla.exe', bytes);
 assert.deepEqual(same.events, [list]);
 const differing = fixture({ initial: [{ id: 7, name: 'dilla.exe', size: 1, digest: 'sha256:old' }] });
 await differing.upload(draft(), 'dilla.exe', bytes);
 assert.deepEqual(differing.events, [list, `DELETE /repos/${repository}/releases/assets/7`, uploadUrl, list]);
});

test('a failed upload or an unexpected stored digest fails publication', async () => {
 await assert.rejects(fixture({ status: 422 }).upload(draft(), 'dilla.exe', bytes), /upload failed \(422\)/);
 await assert.rejects(fixture({ store: () => [] }).upload(draft(), 'dilla.exe', bytes), /digest/);
 await assert.rejects(fixture({ store: sent => [{ id: 8, name: 'dilla.exe', size: sent.length, digest: 'sha256:wrong' }] }).upload(draft(), 'dilla.exe', bytes), /digest/);
});

test('the uploader cannot write outside the approved repository, a draft versioned release or the asset namespace', async () => {
 for (const change of [
  { upload_url: 'https://example.com/repos/dilla-chat/dilla/releases/42/assets{?name,label}' },
  { upload_url: 'https://uploads.github.com/repos/other/repo/releases/42/assets{?name,label}' },
  { upload_url: `https://uploads.github.com/repos/${repository}/releases/99/assets{?name,label}` },
  { tag_name: 'v1.2.3' }, { tag_name: 'desktop-stable' }, { draft: false },
 ]) {
  const { upload, events } = fixture();
  await assert.rejects(upload({ ...draft(), ...change }, 'dilla.exe', bytes), /destination|tag|draft/i);
  assert.deepEqual(events, []);
 }
 for (const name of ['../secret', '.hidden', 'a/b']) {
  const { upload, events } = fixture();
  await assert.rejects(upload(draft(), name, bytes), /asset name/i);
  assert.deepEqual(events, []);
 }
});
