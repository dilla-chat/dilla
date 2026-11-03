import { createHash, timingSafeEqual } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { verifyRelease } from '../src/update.ts';
import { BUILD_TARGETS, TARGETS, publicReleaseRecords, verifySparkleFeed } from './verify-release.mjs';
import { buildProvenanceName } from './build-provenance.mjs';
import { REPOSITORY, createGithubApi, resolveTagCommit } from './ci-run.mjs';
import { publicationContext } from './publish-release.mjs';
import { listReleaseAssets } from './standard-release-upload.mjs';

/**
 * The keyless `verify-published` job: re-downloads every asset of the published
 * versioned release and checks it against the signed metadata, verified under
 * main's pins. It holds `contents: read` only, so a failure alerts; an
 * immutable release cannot be withdrawn by it.
 */
const R = `/repos/${REPOSITORY}`;
const DOWNLOAD_ORIGINS = new Set(['https://github.com', 'https://release-assets.githubusercontent.com', 'https://objects.githubusercontent.com']);
const MAX_RECORD = 1024 ** 2, MAX_ASSET = 4 * 1024 ** 3;

/** Streams one asset, hashing it; small records are also kept in memory. */
async function download(fetchImpl, tag, asset, keep) {
 const expected = `https://github.com/${REPOSITORY}/releases/download/${tag}/${asset.name}`;
 if (asset.browser_download_url !== expected) throw new Error('Unexpected download URL for ' + asset.name);
 const response = await fetchImpl(expected, { redirect: 'follow', signal: AbortSignal.timeout(600000) });
 if (!response.ok || !response.body) throw new Error(`Download failed (${response.status}): ${asset.name}`);
 if (!DOWNLOAD_ORIGINS.has(new URL(response.url).origin)) throw new Error('Download redirected to an unexpected host: ' + asset.name);
 const sha256 = createHash('sha256'), sha512 = createHash('sha512'), chunks = [];
 let size = 0;
 for await (const chunk of response.body) {
  size += chunk.length;
  if (size > (keep ? MAX_RECORD : MAX_ASSET)) throw new Error('Published asset is too large: ' + asset.name);
  sha256.update(chunk); sha512.update(chunk);
  if (keep) chunks.push(chunk);
 }
 const digest = 'sha256:' + sha256.digest('hex');
 if (size !== asset.size || digest !== asset.digest) throw new Error('Published asset digest mismatch: ' + asset.name);
 return { size, sha512: sha512.digest(), bytes: keep ? Buffer.concat(chunks) : undefined };
}

const sha256hex = bytes => createHash('sha256').update(bytes).digest('hex');

export async function verifyPublished({ env, fetchImpl = fetch, trust, now = Date.now() }) {
 const { tag, version, channel, sha, staged } = publicationContext(env);
 const api = createGithubApi({ token: env.GH_TOKEN, fetchImpl });
 const published = await api(`${R}/releases/tags/${encodeURIComponent(tag)}`);
 if (published?.tag_name !== tag || published.draft !== false || published.prerelease !== (channel === 'beta')) throw new Error('The versioned release is not published as expected');
 if (published.immutable !== true) throw new Error('The published release is not immutable');
 if (await resolveTagCommit(api, tag) !== sha) throw new Error('The tag resolves to another commit than DESKTOP_SHA');
 const assets = await listReleaseAssets(api, published.id);
 const byName = new Map(assets.map(asset => [asset?.name, asset]));
 if (byName.size !== assets.length) throw new Error('The published asset set has duplicate names');

 // The signed metadata first: everything else is checked against it.
 const metadataName = `desktop-${channel}.json`;
 for (const name of [metadataName, metadataName + '.sig']) if (!byName.has(name)) throw new Error('The published asset set lacks ' + name);
 const metadata = (await download(fetchImpl, tag, byName.get(metadataName), true)).bytes;
 const signature = (await download(fetchImpl, tag, byName.get(metadataName + '.sig'), true)).bytes.toString('utf8').trim();
 const release = verifyRelease(metadata, signature, { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now });
 if (release.version !== version || release.source?.commit !== sha) throw new Error('The published signed metadata names another version or commit');

 const signedAssets = new Map(release.assets.map(asset => [asset.name, asset]));
 const records = [...publicReleaseRecords(channel), ...(staged ? ['rotation-proof.json'] : [])];
 const expected = [...signedAssets.keys(), ...records].sort(), present = [...byName.keys()].sort();
 if (expected.length !== present.length || expected.some((name, i) => name !== present[i])) throw new Error('The published asset set differs from the signed inventory');

 // Records whose sha256 the signed acceptance source carries.
 const source = release.source, recordDigests = new Map([['release-notes.md', source.notes?.sha256],
  ...TARGETS.map(target => [`performance-${target}.json`, source.performance?.[target]?.sha256]),
  ...TARGETS.map(target => [`installed-account-${target}.json`, source.installedAccounts?.[target]?.sha256]),
  ...BUILD_TARGETS.map(target => [buildProvenanceName(target), source.builds?.[target]?.sha256])]);
 for (const name of expected) {
  if (name === metadataName || name === metadataName + '.sig') continue;
  const signed = signedAssets.get(name), result = await download(fetchImpl, tag, byName.get(name), !signed);
  if (signed) {
   const hash = Buffer.from(signed.sha512, 'base64');
   if (result.size !== signed.size || hash.length !== result.sha512.length || !timingSafeEqual(hash, result.sha512)) throw new Error('Published asset does not match the signed metadata: ' + name);
  } else if (recordDigests.has(name)) {
   if (sha256hex(result.bytes) !== recordDigests.get(name)) throw new Error('Published record does not match the signed metadata: ' + name);
  } else if (name.startsWith('appcast-')) {
   const artifact = release.artifacts.find(item => item.platform === 'darwin' && name === `appcast-${item.arch}.xml`);
   if (!artifact) throw new Error('Published Sparkle feed for an unsigned target: ' + name);
   verifySparkleFeed(release, artifact, result.bytes, trust);
  } else if (name === 'rotation-proof.json') {
   const proof = JSON.parse(result.bytes.toString('utf8'));
   if (proof?.format !== 1 || proof.kind !== 'dilla-desktop-rotation-proof' || proof.version !== version || proof.channel !== channel
    || proof.metadata?.sha256 !== sha256hex(metadata)) throw new Error('Published rotation proof does not match the signed metadata');
  } else throw new Error('Unchecked published record: ' + name);
 }
 return { version, assets: expected.length };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 const root = resolve(import.meta.dirname, '../../..');
 const trust = JSON.parse(await readFile(join(root, 'packages/desktop/release-trust.json'), 'utf8'));
 const result = await verifyPublished({ env: process.env, trust });
 console.log(`Verified the published desktop ${result.version} release: ${result.assets} assets match the signed metadata.`);
}
