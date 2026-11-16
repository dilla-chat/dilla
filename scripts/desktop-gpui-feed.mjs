#!/usr/bin/env node
// The signed update feed of the native Dilla desktop app (GPUI release plan, Task 9; D2, D8). Ported from beta.5's
// release-feed.mjs and feed-format.mjs (`git show desktop-v0.1.0-beta.5:packages/desktop/scripts/release-feed.mjs`)
// without the Sparkle appcast, the Linux sandbox capsule and the acceptance records.
//
// The document keeps beta.5's format 1, so `internal/desktopdownload` reads it:
//   { format: 1, channel, version, publishedAt, expiresAt (publishedAt + 90 days), assets: [{ name, size, sha512 }] }
// Its canonical bytes are `JSON.stringify(doc) + "\n"`, signed with Ed25519 under beta.5's metadata key slot A (D8).
// The detached signature is base64 plus a newline, in `<feed>.sig`. The pins (public keys and the channel) live in
// clients/desktop/release-trust.json.
//
// Feed domain separation (Review Focus 4): the same key signs beta.5's `beta` feed, so `channel` is what keeps the
// feeds apart. A GPUI feed is never `beta`: build refuses it, and verify refuses any channel but the expected one.
//
// The feed is published at https://dilla-chat.github.io/dilla/gpui-<channel>/desktop-gpui-<channel>.json (Task 10).
//
// Usage:
//   DESKTOP_METADATA_KEY_A=... desktop-gpui-feed.mjs build <assetsDir> <version> <outDir>
//       writes <outDir>/desktop-gpui-<channel>.json and .json.sig over the release's assets
//   desktop-gpui-feed.mjs verify <json> <sig>
//       checks a feed under the pinned keys and channel and prints its version
//
// DESKTOP_METADATA_KEY_A is the beta.5 secret as it is: single-line base64 of the key's 48-byte PKCS8 DER
// (release-sign-job.mjs in beta.5). A PKCS8 PEM block is accepted too. The key is never printed or written.
import { createHash, createPrivateKey, createPublicKey, KeyObject, sign, verify as ed25519Verify } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { lstat, mkdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';

export const TRUST_FILE = new URL('../clients/desktop/release-trust.json', import.meta.url);
export const FEED_BASE = 'https://dilla-chat.github.io/dilla';
export const VALIDITY_MS = 90 * 86400000;
const MAX_VALIDITY_MS = 93 * 86400000; // desktopdownload.Verify's bound
const CLOCK_SKEW_MS = 5 * 60000;
const MAX_FEED_BYTES = 1 << 20;
const MAX_SIGNATURE_BYTES = 128;

/** Every release asset the feed lists (D11 platforms), as `dilla-<version>-<suffix>`. */
export const ASSET_SUFFIXES = Object.freeze([
  'linux-x64.AppImage',
  'linux-x64.tar.gz',
  'linux-x64.deb',
  'linux-x64.rpm',
  'linux-x64.aur.tar.gz',
  'mac-arm64.dmg',
  'mac-arm64.zip',
  'win-x64-setup.exe',
]);

const SIGNATURE = /^[A-Za-z0-9+/]{86}==$/;
const SHA512 = /^[A-Za-z0-9+/]{86}==$/;
const UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$/;
const PKCS8_ED25519_BASE64 = /^[A-Za-z0-9+/]{64}$/; // 48 DER bytes
const DOC_KEYS = ['format', 'channel', 'version', 'publishedAt', 'expiresAt', 'assets'];
const ASSET_KEYS = ['name', 'size', 'sha512'];

/** The channel a GPUI feed may carry: a lowercase word that is never beta.5's `beta`. */
export function checkChannel(channel) {
  if (typeof channel !== 'string' || !/^[a-z]+$/.test(channel)) throw new Error('invalid feed channel');
  if (channel === 'beta') throw new Error('a GPUI feed is never the beta channel (that is beta.5\'s Electron feed)');
  return channel;
}

/** A release version of the channel: X.Y.Z-<channel>.N (D1). */
export function checkVersion(version, channel) {
  checkChannel(channel);
  const pattern = new RegExp(`^(0|[1-9]\\d*)\\.(0|[1-9]\\d*)\\.(0|[1-9]\\d*)-${channel}\\.(0|[1-9]\\d*)$`);
  if (typeof version !== 'string' || !pattern.test(version)) throw new Error(`version is not a ${channel} release: ${version}`);
  return version;
}

export const feedName = (channel) => `desktop-gpui-${checkChannel(channel)}.json`;
export const feedUrl = (channel) => `${FEED_BASE}/gpui-${checkChannel(channel)}/${feedName(channel)}`;
export const assetNames = (version) => ASSET_SUFFIXES.map((suffix) => `dilla-${version}-${suffix}`);

/** The pins of a release-trust.json: Ed25519 SPKI PEM keys and the channel. */
export function parseTrust(text) {
  const trust = JSON.parse(text);
  if (!trust || typeof trust !== 'object' || Object.keys(trust).sort().join() !== 'channel,metadataKeys') throw new Error('release-trust.json has unexpected fields');
  if (!Array.isArray(trust.metadataKeys) || trust.metadataKeys.length === 0) throw new Error('release-trust.json pins no metadata key');
  const keys = trust.metadataKeys.map((pem) => {
    const key = createPublicKey(pem);
    if (key.asymmetricKeyType !== 'ed25519') throw new Error('a pinned metadata key is not Ed25519');
    return key;
  });
  return { channel: checkChannel(trust.channel), metadataKeys: trust.metadataKeys, keys };
}

export async function loadTrust(file = TRUST_FILE) {
  return parseTrust(await readFile(file, 'utf8'));
}

/** The metadata signing key from DESKTOP_METADATA_KEY_A's value. Error messages never echo it. */
export function privateKeyFrom(value) {
  if (typeof value !== 'string' || value.length === 0) throw new Error('DESKTOP_METADATA_KEY_A is not set');
  let key;
  if (PKCS8_ED25519_BASE64.test(value.trim())) {
    const der = Buffer.from(value.trim(), 'base64');
    try {
      key = createPrivateKey({ key: der, format: 'der', type: 'pkcs8' });
    } catch {
      throw new Error('DESKTOP_METADATA_KEY_A is not a PKCS8 private key');
    } finally {
      der.fill(0);
    }
  } else if (value.includes('-----BEGIN PRIVATE KEY-----')) {
    try {
      key = createPrivateKey({ key: value, format: 'pem' });
    } catch {
      throw new Error('DESKTOP_METADATA_KEY_A is not a PKCS8 private key');
    }
  } else {
    throw new Error('DESKTOP_METADATA_KEY_A is neither base64 PKCS8 DER nor a PKCS8 PEM block');
  }
  if (key.asymmetricKeyType !== 'ed25519') throw new Error('DESKTOP_METADATA_KEY_A is not an Ed25519 key');
  return key;
}

async function assetOf(dir, name) {
  const file = join(dir, name);
  const info = await lstat(file).catch(() => null);
  if (!info || !info.isFile()) throw new Error(`release asset missing or not a regular file: ${name}`);
  const bytes = await readFile(file);
  if (bytes.length === 0 || bytes.length !== info.size) throw new Error(`release asset is empty or changed: ${name}`);
  return { name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') };
}

/** The canonical bytes of a document. */
export const canonical = (doc) => Buffer.from(JSON.stringify(doc) + '\n');

/**
 * Builds and signs the feed over every asset in ASSET_SUFFIXES (other files in assetsDir are not listed).
 * `privateKey` is an Ed25519 private KeyObject whose public key `trust` pins; `trust.channel` must equal `channel`.
 * Returns `{ json, sig }` as Buffers, after verifying them as a client would.
 */
export async function build(assetsDir, version, channel, { privateKey, trust, now = new Date() }) {
  checkVersion(version, channel);
  if (!trust || trust.channel !== channel) throw new Error(`channel ${channel} is not the pinned channel`);
  if (!(privateKey instanceof KeyObject) || privateKey.type !== 'private' || privateKey.asymmetricKeyType !== 'ed25519') throw new Error('the signer is not an Ed25519 private key');
  const spki = createPublicKey(privateKey).export({ type: 'spki', format: 'der' });
  if (!trust.keys.some((key) => key.export({ type: 'spki', format: 'der' }).equals(spki))) throw new Error('the signer is not a pinned metadata key');
  const assets = [];
  for (const name of assetNames(version)) assets.push(await assetOf(assetsDir, name));
  const doc = {
    format: 1,
    channel,
    version,
    publishedAt: now.toISOString(),
    expiresAt: new Date(now.getTime() + VALIDITY_MS).toISOString(),
    assets,
  };
  const json = canonical(doc);
  const sig = Buffer.from(sign(null, json, privateKey).toString('base64') + '\n');
  verify(json, sig, trust, { channel, now });
  return { json, sig };
}

/**
 * Verifies a feed the way a client must: the detached signature over the exact bytes under a pinned key, before
 * any parsing; then the canonical format-1 document of `channel` (never beta), its dates and its assets.
 * Returns the document.
 */
export function verify(json, sig, trust, { channel = trust?.channel, now = new Date() } = {}) {
  checkChannel(channel);
  if (!trust || !Array.isArray(trust.keys) || trust.keys.length === 0) throw new Error('no pinned metadata key');
  const bytes = Buffer.isBuffer(json) ? json : Buffer.from(json);
  const sigBytes = Buffer.isBuffer(sig) ? sig : Buffer.from(sig);
  if (bytes.length === 0 || bytes.length > MAX_FEED_BYTES) throw new Error('feed size out of bounds');
  if (sigBytes.length > MAX_SIGNATURE_BYTES) throw new Error('malformed feed signature');
  const sigText = sigBytes.toString('latin1').replace(/\n$/, '');
  if (!SIGNATURE.test(sigText)) throw new Error('malformed feed signature');
  const raw = Buffer.from(sigText, 'base64');
  if (!trust.keys.some((key) => ed25519Verify(null, bytes, key, raw))) throw new Error('feed signature does not verify under the pinned keys');

  const doc = JSON.parse(bytes.toString('utf8'));
  if (!doc || typeof doc !== 'object' || Array.isArray(doc) || Object.keys(doc).join() !== DOC_KEYS.join()) throw new Error('feed document does not have the format-1 fields');
  if (!canonical(doc).equals(bytes)) throw new Error('feed document is not canonical');
  if (doc.format !== 1) throw new Error('feed format is not 1');
  if (doc.channel !== channel) throw new Error(`feed channel ${JSON.stringify(doc.channel)} is not ${channel}`);
  checkVersion(doc.version, channel);
  if (!UTC.test(doc.publishedAt) || !UTC.test(doc.expiresAt)) throw new Error('feed date is not a UTC timestamp');
  const published = Date.parse(doc.publishedAt);
  const expires = Date.parse(doc.expiresAt);
  const at = now.getTime();
  if (!Number.isFinite(published) || !Number.isFinite(expires) || published > at + CLOCK_SKEW_MS || expires <= at || expires <= published || expires - published > MAX_VALIDITY_MS) {
    throw new Error('feed is expired or its dates are invalid');
  }
  if (!Array.isArray(doc.assets)) throw new Error('feed assets are not a list');
  const expected = assetNames(doc.version);
  if (doc.assets.length !== expected.length) throw new Error('feed does not list every release asset exactly once');
  doc.assets.forEach((asset, i) => {
    if (!asset || typeof asset !== 'object' || Object.keys(asset).join() !== ASSET_KEYS.join()) throw new Error('feed asset does not have name, size and sha512');
    if (asset.name !== expected[i]) throw new Error(`feed asset ${i} is not ${expected[i]}`);
    if (!Number.isSafeInteger(asset.size) || asset.size < 1) throw new Error(`feed asset size is invalid: ${asset.name}`);
    if (typeof asset.sha512 !== 'string' || !SHA512.test(asset.sha512)) throw new Error(`feed asset sha512 is invalid: ${asset.name}`);
  });
  return doc;
}

/** CLI build: signs with DESKTOP_METADATA_KEY_A under the pinned trust and writes the feed and signature. */
export async function buildCommand(assetsDir, version, outDir, { env = process.env, trust, now } = {}) {
  const pins = trust ?? (await loadTrust());
  const privateKey = privateKeyFrom(env.DESKTOP_METADATA_KEY_A);
  const { json, sig } = await build(resolve(assetsDir), version, pins.channel, { privateKey, trust: pins, now });
  await mkdir(outDir, { recursive: true });
  const out = join(resolve(outDir), feedName(pins.channel));
  await writeFile(out, json, { flag: 'wx' });
  await writeFile(out + '.sig', sig, { flag: 'wx' });
  return out;
}

// Run directly, on every OS: a Windows path (D:\a\...) or one with a space never equals `file://` + argv[1].
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'build' && args.length === 3) {
      const out = await buildCommand(args[0], args[1], args[2]);
      console.log(`signed ${out} (+ .sig)`);
    } else if (command === 'verify' && args.length === 2) {
      const trust = await loadTrust();
      const doc = verify(await readFile(args[0]), await readFile(args[1]), trust);
      console.log(`the ${doc.channel} feed for ${doc.version} verifies under the pinned key (expires ${doc.expiresAt})`);
    } else {
      console.error('usage: desktop-gpui-feed.mjs build ASSETS_DIR VERSION OUT_DIR | verify FEED_JSON FEED_SIG');
      process.exit(2);
    }
  } catch (error) {
    console.error(`desktop-gpui-feed: ${error.message}`);
    process.exit(1);
  }
}
