import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { GitHubPublisher } from 'electron-publish/out/gitHubPublisher.js';
// @ts-expect-error publishing tooling is JavaScript
import { createReleaseAssetUploader } from '../scripts/standard-release-upload.mjs';

const repository = 'dilla-chat/dilla';
const bytes = Buffer.from('accepted native installer\0\xff', 'latin1');
const digest = 'sha256:' + createHash('sha256').update(bytes).digest('hex');
const release = (draft = false, tag = 'desktop-v1.2.3') => ({ id: 42, tag_name: tag, draft, prerelease: false,
 published_at: '2020-01-01T00:00:00Z', upload_url: `https://uploads.github.com/repos/${repository}/releases/42/assets{?name,label}`, assets: [] as any[] });

function fixture({ skip = false, wrongDigest = false, conflict = false } = {}) {
 const events: string[] = [];
 let stored: any[] = [];
 // Keep standard upload(), lazy release resolution and request-body processing;
 // replace only the final network request. Any discovery/API request is a bug.
 // @ts-expect-error the pinned publisher's JavaScript network seam is private in its declarations
 class OfflinePublisher extends GitHubPublisher {
  async githubRequest() { throw new Error('Unexpected publisher release discovery or mutation'); }
  async doUploadFile(_attempt: number, url: any, name: string, size: number, processRequest: any, target: any) {
   events.push('upload');
   assert.equal(url.hostname, 'uploads.github.com');
   assert.equal(url.path, `/repos/${repository}/releases/42/assets?name=${name}`);
   assert.equal(size, bytes.length);
   let sent: Buffer | undefined;
   processRequest({ setTimeout() {}, end(value: Buffer) { sent = value; } }, (error: Error) => { throw error; });
   assert.deepEqual(sent, bytes, 'standard publisher receives exact accepted bytes');
   if (conflict) await (this as any).overwriteArtifact(name, target);
   if (!skip) stored = [{ id: 7, name, size, digest: wrongDigest ? 'sha256:wrong' : digest }];
  }
 }
 const api = async (path: string, options: any = {}) => {
  events.push(options.method ?? 'GET');
  if (options.method === 'DELETE') {
   assert.equal(path, `/repos/${repository}/releases/assets/7`); stored = []; return null;
  }
  assert.equal(path, `/repos/${repository}/releases/42/assets?per_page=100`);
  return stored;
 };
 return { events, upload: createReleaseAssetUploader({ api, token: 'workflow-token', Publisher: OfflinePublisher }) };
}

test('standard publisher uploads exact accepted bytes to an old published channel and checks the remote digest', async () => {
 const { upload, events } = fixture();
 await upload(release(false, 'desktop-stable'), 'dilla-linux-x64.AppImage', bytes, true);
 assert.deepEqual(events, ['upload', 'GET']);
});

test('identical immutable assets are idempotent and differing published version assets cannot be overwritten', async () => {
 const { upload, events } = fixture();
 const target = release(); target.assets = [{ id: 7, name: 'dilla.exe', size: bytes.length, digest }];
 await upload(target, 'dilla.exe', bytes, false);
 target.assets[0].digest = 'sha256:different';
 await assert.rejects(upload(target, 'dilla.exe', bytes, false), /immutable/i);
 await assert.rejects(upload(target, 'dilla.exe', bytes, true), /immutable/i);
 assert.deepEqual(events, []);
});

test('draft replacement deletes the differing asset before standard upload and remote verification', async () => {
 const { upload, events } = fixture();
 const target = release(true); target.assets = [{ id: 7, name: 'dilla.exe', size: 1, digest: 'sha256:old' }];
 await upload(target, 'dilla.exe', bytes, true);
 assert.deepEqual(events, ['DELETE', 'upload', 'GET']);
});

test('standard publisher conflict recovery cannot delete a published immutable asset', async () => {
 const { upload, events } = fixture({ conflict: true });
 await assert.rejects(upload(release(), 'dilla.exe', bytes, false), /immutable/i);
 assert.deepEqual(events, ['upload']);
});

test('silent publisher skips and incorrect GitHub digests fail publication', async () => {
 for (const options of [{ skip: true }, { wrongDigest: true }]) {
  const { upload, events } = fixture(options);
  await assert.rejects(upload(release(), 'dilla.exe', bytes, false), /digest/i);
  assert.deepEqual(events, ['upload', 'GET']);
 }
});

test('standard transport cannot upload outside the approved repository, release or asset namespace', async () => {
 for (const change of [
  { upload_url: 'https://example.com/repos/dilla-chat/dilla/releases/42/assets{?name,label}' },
  { upload_url: 'https://uploads.github.com/repos/other/repo/releases/42/assets{?name,label}' },
  { upload_url: `https://uploads.github.com/repos/${repository}/releases/99/assets{?name,label}` },
  { tag_name: 'v1.2.3' },
 ]) {
  const { upload, events } = fixture();
  await assert.rejects(upload({ ...release(), ...change }, 'dilla.exe', bytes, false), /destination|tag/i);
  assert.deepEqual(events, []);
 }
 const { upload, events } = fixture();
 await assert.rejects(upload(release(), '../secret', bytes, false), /asset name/i);
 assert.deepEqual(events, []);
});
