import { verifyDesktopBundle, verifyDesktopSigningCandidate, PUBLIC_ACCEPTANCE_RECORDS } from './verify-release.mjs';
import { KeyObject, createHash, createPrivateKey, createPublicKey, sign, verify } from 'node:crypto';
import { readFile, writeFile, mkdir, lstat } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

import { parsePublicConfiguration, assertDedicatedKeys } from './linux-sandbox-package.mjs';
import { appcast, signedAppcast, sparkleVersion, macRotationOf, macUpdateKind, sparkleFeedKey } from './feed-format.mjs';
export { appcast, signedAppcast, sparkleVersion } from './feed-format.mjs';

/** In-memory signing keys: `metadata` and `sandbox` are Ed25519 private KeyObjects, `sparkleSeed` the 32-byte Sparkle seed. */
function validKeys(keys) {
 const ed25519 = key => key instanceof KeyObject && key.type === 'private' && key.asymmetricKeyType === 'ed25519';
 if (!keys || typeof keys !== 'object' || !ed25519(keys.metadata) || !ed25519(keys.sandbox) || !Buffer.isBuffer(keys.sparkleSeed)
  || !(keys.feedSparkleSeed === undefined || Buffer.isBuffer(keys.feedSparkleSeed))) throw new Error('Signing keys must be Ed25519 private KeyObjects and Sparkle seed Buffers');
 return keys;
}

// The PKCS8 DER prefix of an Ed25519 private key; the 32-byte seed follows it.
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex');
/** The Sparkle private key from its 32-byte seed, imported as PKCS8 DER so the seed never becomes a
 * (non-zeroable) string; the DER Buffer is zeroed after import. The caller zeroes the seed. */
export function sparkleKeyFromSeed(seed) {
 if (!Buffer.isBuffer(seed) || seed.length !== 32) throw new Error('Sparkle signer must use its 32-byte seed format');
 const der = Buffer.concat([ED25519_PKCS8_PREFIX, seed]);
 try { return createPrivateKey({ key: der, format: 'der', type: 'pkcs8' }); } finally { der.fill(0); }
}

/** Signs an accepted candidate. Keys come either from a keys directory (offline
 * signing) or as in-memory key objects (the release workflow), never both; with
 * `keys`, nothing is read from or written to a keys directory. Publishing
 * consumes only the output directory, never keys.
 *
 * `macUpdate` is 'zip' (every release) or 'dmg' (a channel's rotation release of a
 * pending macOS key change, feed-format.mjs): the DMG becomes the darwin update,
 * its archive is signed by the active Sparkle key (the one its Info.plist carries)
 * and its appcast by the key the installed Macs trust: `keys.feedSparkleSeed`, or
 * `sparkle-rotation-private.key` in the keys directory. */
export async function signDesktopRelease({ version, artifactsDirectory, keysDirectory, keys, outputDirectory, now = new Date(), macUpdate = 'zip' }) {
 if ((keysDirectory === undefined) === (keys === undefined)) throw new Error('Pass exactly one of keysDirectory or keys');
 if (keys !== undefined) validKeys(keys);
 sparkleVersion(version);
 macUpdateKind(macUpdate);
 if (keys !== undefined && (keys.feedSparkleSeed !== undefined) !== (macUpdate === 'dmg')) throw new Error('A separate appcast signer is passed exactly for a disk image rotation release');
 const channel = version.includes('-beta.') ? 'beta' : 'stable';
 const trust = JSON.parse(await readFile(new URL('../release-trust.json', import.meta.url), 'utf8'));
 if (macUpdate === 'dmg' && !macRotationOf(trust)) throw new Error('A macOS disk image update is only allowed for the rotation release of a pending macOS key change');
 const metadataKey = keys ? keys.metadata : createPrivateKey(await readFile(join(keysDirectory, 'metadata-private.pem')));
 const metadataPublic = createPublicKey(metadataKey).export({ type: 'spki', format: 'pem' }).toString();
 if (!trust.metadataPublicKeys.includes(metadataPublic)) throw new Error('Metadata signer does not match installed trust');
 // A private copy: the caller's seed Buffer stays the caller's to zero.
 const seed = keys ? Buffer.from(keys.sparkleSeed) : Buffer.from((await readFile(join(keysDirectory, 'sparkle-private.key'), 'utf8')).trim(), 'base64');
 if (seed.length !== 32) { seed.fill(0); throw new Error('Sparkle signer must use its 32-byte seed format'); }
 let sparkleKey;
 try { sparkleKey = sparkleKeyFromSeed(seed); } finally { seed.fill(0); }
 if (createPublicKey(sparkleKey).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') !== trust.sparklePublicKey) throw new Error('Sparkle signer does not match installed trust');
 // The appcast signer: the active key, or for a DMG rotation release the key the installed Macs trust.
 let feedKey = sparkleKey;
 if (macUpdate === 'dmg') {
  const feedSeed = keys ? Buffer.from(keys.feedSparkleSeed) : Buffer.from((await readFile(join(keysDirectory, 'sparkle-rotation-private.key'), 'utf8')).trim(), 'base64');
  try { feedKey = sparkleKeyFromSeed(feedSeed); } finally { feedSeed.fill(0); }
  if (createPublicKey(feedKey).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') !== sparkleFeedKey(trust, 'dmg')) throw new Error('The appcast signer is not the Sparkle key the rotation starts from');
 }
 const component = parsePublicConfiguration(await readFile(new URL('../linux-sandbox-component.json', import.meta.url), 'utf8'));
 assertDedicatedKeys(component, trust);
 const source = JSON.parse(await readFile(join(artifactsDirectory, 'acceptance.json'), 'utf8'));
 const run = JSON.parse(await readFile(join(artifactsDirectory, 'ci-run.json'), 'utf8'));
 const jobs = JSON.parse(await readFile(join(artifactsDirectory, 'ci-jobs.json'), 'utf8'));
 const release = { format: 1, version, channel, publishedAt: now.toISOString(), expiresAt: new Date(now.getTime() + 90 * 86400000).toISOString(), source, assets: [], artifacts: [] };
 for (const [platform, arch, os, ext] of [['linux', 'x64', 'linux', 'AppImage'], ['win32', 'x64', 'win', 'exe'], ['darwin', 'arm64', 'mac', macUpdate]]) {
  const name = `dilla-${version}-${os}-${arch}.${ext}`, file = join(artifactsDirectory, name);
  if (!(await lstat(file)).isFile()) throw new Error('Installer must be a regular file: ' + name);
  const bytes = await readFile(file);
  if (!bytes.length) throw new Error('Empty installer: ' + name);
  const artifact = { platform, arch, url: `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${name}`, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
  if (platform === 'linux') {
   const capsuleName = `dilla-${version}-linux-x64.sandbox`, manifestName = capsuleName + '.json';
   for (const [sidecar, limit] of [[capsuleName, 48 * 1024 ** 2], [manifestName, 65536]]) {
    const info = await lstat(join(artifactsDirectory, sidecar));
    if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1 || info.size < 1 || info.size > limit) throw new Error('Invalid accepted sandbox sidecar: ' + sidecar);
    const data = await readFile(join(artifactsDirectory, sidecar));
    if (data.length !== info.size) throw new Error('Sandbox sidecar changed: ' + sidecar);
    release.assets.push({ name: sidecar, size: data.length, sha512: createHash('sha512').update(data).digest('base64') });
   }
   const capsule = release.assets.find(a => a.name === capsuleName);
   artifact.linuxSandbox = { compatibilityEpoch: 1, minimumGeneration: component.generation, bundle: { url: `https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${capsuleName}`, size: capsule.size, sha512: capsule.sha512 } };
  }
  if (platform === 'darwin') {
   artifact.sparkleSignature = sign(null, bytes, sparkleKey).toString('base64');
   if (!verify(null, bytes, createPublicKey(sparkleKey), Buffer.from(artifact.sparkleSignature, 'base64'))) throw new Error('Sparkle archive signature self-check failed');
  }
  release.artifacts.push(artifact);
  release.assets.push({ name, size: bytes.length, sha512: artifact.sha512 });
 }
 // Both macOS archives are always release assets; only the update (above) differs.
 for (const suffix of [macUpdate === 'dmg' ? 'mac-arm64.zip' : 'mac-arm64.dmg', 'linux-x64.deb', 'linux-x64.rpm', 'linux-x64.aur.tar.gz']) {
  const name = `dilla-${version}-${suffix}`, file = join(artifactsDirectory, name);
  if (!(await lstat(file)).isFile()) throw new Error('Installer must be a regular file: ' + name);
  const bytes = await readFile(file);
  release.assets.push({ name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') });
 }
 // The preliminary feed carries a placeholder signature asset. The private
 // maintenance key is opened only after all other acceptance checks pass.
 const sandboxName = `dilla-${version}-linux-x64.sandbox`, signatureName = sandboxName + '.sig';
 const placeholder = Buffer.from(Buffer.alloc(64).toString('base64') + '\n');
 const signatureAsset = { name: signatureName, size: placeholder.length, sha512: createHash('sha512').update(placeholder).digest('base64') }; release.assets.push(signatureAsset);
 const makeFeeds = () => Object.fromEntries(release.artifacts.filter(a => a.platform === 'darwin').map(artifact => [artifact.arch, signedAppcast(appcast(release, artifact, artifact.sparkleSignature), feedKey)]));
 let bytes = Buffer.from(JSON.stringify(release, null, 2) + '\n'), signature = sign(null, bytes, metadataKey), feeds = makeFeeds();
 await verifyDesktopSigningCandidate({ metadata: bytes, signature: signature.toString('base64'), trust, directory: artifactsDirectory, run, jobs, version, feeds, now: now.getTime(), macUpdate });
 const manifest = await readFile(join(artifactsDirectory, sandboxName + '.json'));
 const maintenanceKey = keys ? keys.sandbox : createPrivateKey(await readFile(join(keysDirectory, 'linux-sandbox-private.pem')));
 const rawPublic = createPublicKey(maintenanceKey).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
 if (maintenanceKey.asymmetricKeyType !== 'ed25519' || !component.publicKeys.includes(rawPublic)) throw new Error('Sandbox signer does not match installed dedicated trust');
 const detached = Buffer.from(sign(null, Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'), manifest]), maintenanceKey).toString('base64') + '\n');
 signatureAsset.size = detached.length; signatureAsset.sha512 = createHash('sha512').update(detached).digest('base64');
 bytes = Buffer.from(JSON.stringify(release, null, 2) + '\n'); signature = sign(null, bytes, metadataKey); feeds = makeFeeds();
 if (!verify(null, bytes, metadataPublic, signature)) throw new Error('Metadata signature self-check failed');
 await verifyDesktopBundle({ metadata: bytes, signature: signature.toString('base64'), trust, directory: artifactsDirectory, run, jobs, version, feeds, now: now.getTime(), sandboxSignature: detached, macUpdate });
 await mkdir(outputDirectory, { recursive: true, mode: 0o700 });
 await writeFile(join(outputDirectory, signatureName), detached, { flag: 'wx' });
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
