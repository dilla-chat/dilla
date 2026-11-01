import { createHash, createPrivateKey, createPublicKey, sign, verify } from 'node:crypto';
import { readFile, writeFile, mkdir, lstat } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';

export function sparkleVersion(version) {
 if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.test(version)) throw new Error('Invalid desktop version');
 return version.replace('-beta.', 'b');
}
export function signedAppcast(xml, key) {
 const content = Buffer.from(xml, 'utf8');
 // Sparkle 2.9+ SPUExtractSignedFeed / common_cli/Signing.swift format.
 return Buffer.concat([content, Buffer.from(`<!-- sparkle-signatures:\nedSignature: ${sign(null, content, key).toString('base64')}\nlength: ${content.length}\n-->\n`)]);
}
export function appcast(release, artifact, signature) {
 const safe = value => String(value).replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
 return `<?xml version="1.0" encoding="utf-8"?>\n<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><title>Dilla desktop ${safe(release.channel)}</title><item><title>Dilla ${safe(release.version)}</title><sparkle:version>${sparkleVersion(release.version)}</sparkle:version><sparkle:shortVersionString>${safe(release.version)}</sparkle:shortVersionString><sparkle:minimumSystemVersion>13.0.0</sparkle:minimumSystemVersion><pubDate>${new Date(release.publishedAt).toUTCString()}</pubDate><enclosure url="${safe(artifact.url)}" sparkle:edSignature="${safe(signature)}" length="${artifact.size}" type="application/octet-stream" /></item></channel></rss>\n`;
}

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
 const release = { format: 1, version, channel, publishedAt: now.toISOString(), expiresAt: new Date(now.getTime() + 90 * 86400000).toISOString(), artifacts: [] };
 const archives = new Map();
 for (const [platform, arch, os, ext] of [['linux', 'x64', 'linux', 'AppImage'], ['win32', 'x64', 'win', 'exe'], ['darwin', 'x64', 'mac', 'zip'], ['darwin', 'arm64', 'mac', 'zip']]) {
  const name = `dilla-${version}-${os}-${arch}.${ext}`, file = join(artifactsDirectory, name);
  if (!(await lstat(file)).isFile()) throw new Error('Installer must be a regular file: ' + name);
  const bytes = await readFile(file);
  if (!bytes.length) throw new Error('Empty installer: ' + name);
  const artifact = { platform, arch, url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${name}`, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
  if (platform === 'darwin') { artifact.sparkleSignature = sign(null, bytes, sparkleKey).toString('base64'); archives.set(arch, artifact.sparkleSignature); }
  release.artifacts.push(artifact);
 }
 await mkdir(outputDirectory, { recursive: true, mode: 0o700 });
 const bytes = Buffer.from(JSON.stringify(release, null, 2) + '\n');
 const signature = sign(null, bytes, metadataKey);
 if (!verify(null, bytes, metadataPublic, signature)) throw new Error('Metadata signature self-check failed');
 await writeFile(join(outputDirectory, `desktop-${channel}.json`), bytes, { flag: 'wx' });
 await writeFile(join(outputDirectory, `desktop-${channel}.json.sig`), signature.toString('base64') + '\n', { flag: 'wx' });
 for (const artifact of release.artifacts.filter(a => a.platform === 'darwin')) {
  const feed = signedAppcast(appcast(release, artifact, archives.get(artifact.arch)), sparkleKey);
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
