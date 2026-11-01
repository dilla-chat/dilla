import { verifyDesktopBundle, PUBLIC_ACCEPTANCE_RECORDS } from './verify-release.mjs';
import { createHash, createPrivateKey, createPublicKey, sign, verify } from 'node:crypto';
import { readFile, writeFile, mkdir, lstat } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

import { verifyWindowsProgramManifest } from '../src/windows-manifest.ts';
import { appcast, signedAppcast, sparkleVersion } from './feed-format.mjs';
export { appcast, signedAppcast, sparkleVersion } from './feed-format.mjs';

/** Offline signing. Publishing consumes only this output directory, never keys. */
export async function signDesktopRelease({ version, artifactsDirectory, keysDirectory, outputDirectory, now = new Date() }) {
 sparkleVersion(version);
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 const trust = JSON.parse(await readFile(new URL('../release-trust.json', import.meta.url), 'utf8'));
 const metadataKey = createPrivateKey(await readFile(join(keysDirectory, 'metadata-private.pem')));
 const metadataPublic = createPublicKey(metadataKey).export({ type: 'spki', format: 'pem' }).toString();
 if (!trust.metadataPublicKeys.includes(metadataPublic)) throw new Error('Metadata signer does not match installed trust');
 const seed = Buffer.from((await readFile(join(keysDirectory, 'sparkle-private.key'), 'utf8')).trim(), 'base64');
 if (seed.length !== 32) throw new Error('Sparkle signer must use its 32-byte seed format');
 const sparkleKey = createPrivateKey({ key: { kty: 'OKP', crv: 'Ed25519', d: seed.toString('base64url'), x: Buffer.from(trust.sparklePublicKey, 'base64').toString('base64url') }, format: 'jwk' });
 seed.fill(0);
 if (createPublicKey(sparkleKey).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') !== trust.sparklePublicKey) throw new Error('Sparkle signer does not match installed trust');
 const source = JSON.parse(await readFile(join(artifactsDirectory, 'acceptance.json'), 'utf8'));
 const run = JSON.parse(await readFile(join(artifactsDirectory, 'ci-run.json'), 'utf8'));
 const jobs = JSON.parse(await readFile(join(artifactsDirectory, 'ci-jobs.json'), 'utf8'));
 const release = { format: 1, version, channel, publishedAt: now.toISOString(), expiresAt: new Date(now.getTime() + 90 * 86400000).toISOString(), source, assets: [], artifacts: [] };
 for (const [platform, arch, os, ext] of [['linux', 'x64', 'linux', 'AppImage'], ['win32', 'x64', 'win', 'exe'], ['darwin', 'x64', 'mac', 'zip'], ['darwin', 'arm64', 'mac', 'zip']]) {
  const name = `dilla-${version}-${os}-${arch}.${ext}`, file = join(artifactsDirectory, name);
  if (!(await lstat(file)).isFile()) throw new Error('Installer must be a regular file: ' + name);
  const bytes = await readFile(file);
  if (!bytes.length) throw new Error('Empty installer: ' + name);
  const artifact = { platform, arch, url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${name}`, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
  if (platform === 'darwin') {
   artifact.sparkleSignature = sign(null, bytes, sparkleKey).toString('base64');
   if (!verify(null, bytes, createPublicKey(sparkleKey), Buffer.from(artifact.sparkleSignature, 'base64'))) throw new Error('Sparkle archive signature self-check failed');
  }
  if (platform === 'win32') {
   const manifestName = `dilla-${version}-win-${arch}.manifest.json`, manifestFile = join(artifactsDirectory, manifestName), info = await lstat(manifestFile);
   if (!info.isFile() || info.isSymbolicLink() || info.size < 1 || info.size > 1048576) throw new Error('Invalid final Windows program manifest');
   const manifestBytes = await readFile(manifestFile), sha512 = createHash('sha512').update(manifestBytes).digest('base64');
   verifyWindowsProgramManifest(manifestBytes, sha512, { version, channel, arch });
   artifact.programManifest = { url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${manifestName}`, size: manifestBytes.length, sha512 };
   release.assets.push({ name: manifestName, size: manifestBytes.length, sha512 });
  }
  release.artifacts.push(artifact);
  release.assets.push({ name, size: bytes.length, sha512: artifact.sha512 });
 }
 for (const arch of ['x64', 'arm64']) {
  const name = `dilla-${version}-mac-${arch}.dmg`, file = join(artifactsDirectory, name);
  if (!(await lstat(file)).isFile()) throw new Error('Installer must be a regular file: ' + name);
  const bytes = await readFile(file);
  release.assets.push({ name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') });
 }
 await mkdir(outputDirectory, { recursive: true, mode: 0o700 });
 const bytes = Buffer.from(JSON.stringify(release, null, 2) + '\n');
 const signature = sign(null, bytes, metadataKey);
 if (!verify(null, bytes, metadataPublic, signature)) throw new Error('Metadata signature self-check failed');
 const feeds = Object.fromEntries(release.artifacts.filter(a => a.platform === 'darwin').map(artifact => [artifact.arch, signedAppcast(appcast(release, artifact, artifact.sparkleSignature), sparkleKey)]));
 await verifyDesktopBundle({ metadata: bytes, signature: signature.toString('base64'), trust, directory: artifactsDirectory, run, jobs, version, feeds, now: now.getTime() });
 await writeFile(join(outputDirectory, `desktop-${channel}.json`), bytes, { flag: 'wx' });
 await writeFile(join(outputDirectory, `desktop-${channel}.json.sig`), signature.toString('base64') + '\n', { flag: 'wx' });
 for (const name of PUBLIC_ACCEPTANCE_RECORDS) {
  await writeFile(join(outputDirectory, name), await readFile(join(artifactsDirectory, name)), { flag: 'wx' });
 }
 for (const artifact of release.artifacts.filter(a => a.platform === 'darwin')) {
  const feed = feeds[artifact.arch];
  await writeFile(join(outputDirectory, `appcast-${artifact.arch}.xml`), feed, { flag: 'wx' });
 }
 return release;
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
 const [version, artifactsDirectory, keysDirectory, outputDirectory] = process.argv.slice(2);
 if (!outputDirectory || process.argv.length !== 6) throw new Error('Usage: release-feed.mjs VERSION ARTIFACTS_DIR PRIVATE_KEYS_DIR OUTPUT_DIR');
 const result = await signDesktopRelease({ version, artifactsDirectory: resolve(artifactsDirectory), keysDirectory: resolve(keysDirectory), outputDirectory: resolve(outputDirectory) });
 console.log(`Signed ${result.channel} ${result.version}: ${result.artifacts.length} installers; signing keys were not copied to the output.`);
}
