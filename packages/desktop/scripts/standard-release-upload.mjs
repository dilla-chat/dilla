import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';

// Resolve the publisher shipped with our pinned electron-builder dependency.
const builderRequire = createRequire(createRequire(import.meta.url).resolve('electron-builder/package.json'));
const publisherRequire = createRequire(builderRequire.resolve('app-builder-lib/package.json'));
const { GitHubPublisher } = publisherRequire('electron-publish/out/gitHubPublisher');
const { CancellationToken } = builderRequire('builder-util-runtime');
const repository = 'dilla-chat/dilla';

export function createReleaseAssetUploader({ api, token, Publisher = GitHubPublisher }) {
 return async function upload(target, name, bytes, replace) {
  if (!/^[a-zA-Z0-9._-]+$/.test(name) || name === '.' || name === '..') throw new Error('Invalid release asset name');
  const versionTag = /^desktop-v((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)(?:-beta\.(?:0|[1-9]\d*))?)$/.exec(target.tag_name);
  if (!versionTag && !/^desktop-(beta|stable)$/.test(target.tag_name)) throw new Error('Invalid desktop release tag');
  if (!Number.isSafeInteger(target.id) || target.id < 1 || target.upload_url !== `https://uploads.github.com/repos/${repository}/releases/${target.id}/assets{?name,label}`) throw new Error('Invalid publication upload destination');
  if (versionTag && !target.draft && replace) throw new Error('Cannot replace an immutable release asset');
  const existing = target.assets?.find(asset => asset.name === name);
  const sha256 = 'sha256:' + createHash('sha256').update(bytes).digest('hex');
  if (existing?.size === bytes.length && existing.digest === sha256) return;
  if (existing) {
   if (!replace) throw new Error('An immutable release asset already differs: ' + name);
   await api(`/repos/${repository}/releases/assets/${existing.id}`, { method: 'DELETE' });
  }
  class AcceptedReleasePublisher extends Publisher {
   // The caller already selected/created this exact release after all trust
   // gates. Binding it avoids discovery, creation and the standard two-hour
   // cutoff, so old published channel aliases remain promotable.
   async getOrCreateRelease() { return target; }
   async overwriteArtifact(fileName, release) {
    // Standard 422 recovery must never replace a published version asset,
    // including a competing upload after our initial inventory check.
    if (!replace) throw new Error('An immutable release asset already differs: ' + fileName);
    return super.overwriteArtifact(fileName, release);
   }
  }
  const publisher = new AcceptedReleasePublisher({ cancellationToken: new CancellationToken() },
   { provider: 'github', owner: 'dilla-chat', repo: 'dilla', token, tagPrefix: versionTag ? 'desktop-v' : 'desktop-', timeout: 300000 },
   versionTag ? versionTag[1] : target.tag_name.slice('desktop-'.length));
  await publisher.upload({ file: name, safeArtifactName: name, fileContent: bytes, timeout: 300000 });
  // Standard upload() can resolve without uploading. Verify actual remote
  // storage rather than treating a void result or a logged skip as success.
  const assets = await api(`/repos/${repository}/releases/${target.id}/assets?per_page=100`);
  const result = assets.find(asset => asset.name === name);
  if (result?.size !== bytes.length || result.digest !== sha256) throw new Error('GitHub stored an unexpected asset digest: ' + name);
 };
}
