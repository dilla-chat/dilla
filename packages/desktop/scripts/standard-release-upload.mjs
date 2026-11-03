import { createHash } from 'node:crypto';

/**
 * Uploads one asset to a draft versioned desktop release with a plain `fetch`
 * to uploads.github.com: no electron-builder, no npm-installed code under the
 * publish job's write token. Redirects are never followed; the stored size and
 * digest are re-read through the API after every upload.
 */
const REPOSITORY = 'dilla-chat/dilla';
const UPLOAD_ORIGIN = 'https://uploads.github.com';
const VERSION_TAG = /^desktop-v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-beta\.(?:0|[1-9]\d*))?$/;
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const MAX_ASSET = 2 * 1024 ** 3;

export function releaseAssetDigest(bytes) { return 'sha256:' + createHash('sha256').update(bytes).digest('hex'); }

/** All assets of a release, through the API's pagination. */
export async function listReleaseAssets(api, id) {
 const assets = [];
 for (let page = 1; ; page++) {
  const list = await api(`/repos/${REPOSITORY}/releases/${id}/assets?per_page=100&page=${page}`);
  if (!Array.isArray(list)) throw new Error('Malformed release asset list');
  assets.push(...list);
  if (list.length < 100) return assets;
  if (page >= 20) throw new Error('Too many release assets');
 }
}

export function createReleaseAssetUploader({ api, token, fetchImpl = fetch }) {
 if (typeof token !== 'string' || !token) throw new Error('Uploading requires the workflow’s scoped GitHub token');
 return async function upload(target, name, bytes) {
  if (typeof name !== 'string' || !NAME.test(name) || name.includes('..')) throw new Error('Invalid release asset name');
  if (typeof target?.tag_name !== 'string' || !VERSION_TAG.test(target.tag_name)) throw new Error('Invalid desktop release tag');
  if (!Number.isSafeInteger(target.id) || target.id < 1 || target.upload_url !== `${UPLOAD_ORIGIN}/repos/${REPOSITORY}/releases/${target.id}/assets{?name,label}`) throw new Error('Invalid publication upload destination');
  if (target.draft !== true) throw new Error('Assets are uploaded only to a draft release');
  if (!Buffer.isBuffer(bytes) || bytes.length < 1 || bytes.length > MAX_ASSET) throw new Error('Invalid release asset bytes: ' + name);
  const digest = releaseAssetDigest(bytes);
  const existing = (await listReleaseAssets(api, target.id)).filter(asset => asset?.name === name);
  if (existing.length === 1 && existing[0].size === bytes.length && existing[0].digest === digest) return;
  for (const asset of existing) await api(`/repos/${REPOSITORY}/releases/assets/${asset.id}`, { method: 'DELETE' });
  const url = new URL(`/repos/${REPOSITORY}/releases/${target.id}/assets`, UPLOAD_ORIGIN);
  url.searchParams.set('name', name);
  if (url.origin !== UPLOAD_ORIGIN) throw new Error('Invalid publication upload destination');
  const response = await fetchImpl(url.href, {
   method: 'POST', body: bytes, redirect: 'error', signal: AbortSignal.timeout(300000),
   headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', 'Content-Type': 'application/octet-stream', 'Content-Length': String(bytes.length) },
  });
  if (response.status !== 201) throw new Error(`Release asset upload failed (${response.status}): ${name}`);
  const stored = (await listReleaseAssets(api, target.id)).filter(asset => asset?.name === name);
  if (stored.length !== 1 || stored[0].size !== bytes.length || stored[0].digest !== digest) throw new Error('GitHub stored an unexpected asset digest: ' + name);
 };
}
