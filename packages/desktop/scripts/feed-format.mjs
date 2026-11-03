import { sign } from 'node:crypto';

/**
 * macOS key rotation (RELEASING.md, "Staged key rotation"). Sparkle's documented rule
 * (https://sparkle-project.org/documentation/, "Rotating signing keys"): "Sparkle allows rotating keys by issuing a
 * new update that changes either your Apple code signing certificate or your EdDSA keys (but not both). For
 * applications that opt into enabling SUVerifyUpdateBeforeExtraction, changing your EdDSA keys can only be done if
 * the update archive is a Developer ID code signed disk image (dmg)."
 *
 * How Sparkle 2 applies it (SUUpdateValidator, AppInstaller, SUAppcastDriver on the 2.x branch): the installed app
 * first checks the archive's EdDSA signature under its own SUPublicEDKey; when that fails, it falls back to the disk
 * image's Developer ID signature (same team as the installed app; a zip cannot carry one), and after extraction the
 * archive's EdDSA signature must then verify under the NEW SUPublicEDKey the update declares. The signed appcast
 * (SURequireSignedFeed) is checked under the installed app's key. So the rotation release's macOS update is a
 * Developer-ID-signed, notarized DMG carrying the new key; its archive signature is by the new (active) key, and its
 * appcast is signed by the old key the installed apps trust.
 *
 * `rotate --promote` records the pending change as `signing.macRotation` = { fromSlot, fromSparklePublicKey } (the
 * slot and Sparkle key the installed Macs trust); `rotate --retire` clears it. Absent or null: nothing pending.
 */
const RAW_KEY = /^[A-Za-z0-9+/]{43}=$/;
export function macRotationOf(trust) {
 const rotation = trust?.signing?.macRotation;
 if (rotation === undefined || rotation === null) return null;
 const exact = typeof rotation === 'object' && !Array.isArray(rotation) && Object.keys(rotation).length === 2
  && Object.hasOwn(rotation, 'fromSlot') && Object.hasOwn(rotation, 'fromSparklePublicKey');
 if (!exact || !['A', 'B'].includes(rotation.fromSlot) || rotation.fromSlot === trust.signing.active || trust.signing.staged !== null
  || trust.sparkleNextPublicKey !== undefined || typeof rotation.fromSparklePublicKey !== 'string' || !RAW_KEY.test(rotation.fromSparklePublicKey)
  || Buffer.from(rotation.fromSparklePublicKey, 'base64').length !== 32 || rotation.fromSparklePublicKey === trust.sparklePublicKey) {
  throw new Error('Invalid pending macOS key change (signing.macRotation) in release-trust.json');
 }
 return rotation;
}
/** The macOS update archive kind of a release: `dmg` for a channel's rotation release, `zip` for every other. */
export function macUpdateKind(value) {
 if (value !== 'zip' && value !== 'dmg') throw new Error('The macOS update kind must be zip or dmg');
 return value;
}
export const macUpdateName = (version, kind) => `dilla-${version}-mac-arm64.${macUpdateKind(kind)}`;
/** The kind of a signed darwin artifact, from its URL. */
export function macArtifactKind(artifact) {
 const name = typeof artifact?.url === 'string' ? artifact.url.split('/').at(-1) : '';
 if (name.endsWith('.dmg')) return 'dmg';
 if (name.endsWith('.zip')) return 'zip';
 throw new Error('The macOS update is neither a zip nor a disk image');
}
/** The raw Sparkle key that signs the appcast, which is the installed apps' key: for a DMG rotation release the key
 * the rotation starts from (a DMG is refused unless a change is pending), for every other release the active key. */
export function sparkleFeedKey(trust, kind) {
 if (macUpdateKind(kind) === 'zip') return trust.sparklePublicKey;
 const rotation = macRotationOf(trust);
 if (!rotation) throw new Error('A macOS disk image update is only allowed for the rotation release of a pending macOS key change');
 return rotation.fromSparklePublicKey;
}

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

