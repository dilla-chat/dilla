#!/usr/bin/env node
// The GitHub Pages site of the native Dilla desktop app's feed (GPUI release plan, Task 10; Review Focus 5).
//
// A Pages deployment replaces the whole site, and the live `beta/` files still serve installed beta.5 Electron apps
// and today's `dillad --desktop-downloads`. So a GPUI deploy rebuilds the site from:
//   - the live `beta/` files, fetched from the site, checked under beta.5's pins and copied byte for byte;
//   - the signed GPUI feed from Task 9 (`desktop-gpui-feed.mjs build`) under `gpui-<channel>/`.
//
// What the live site serves (checked 2026-11-16): exactly beta/desktop-beta.json, beta/desktop-beta.json.sig and
// beta/appcast-arm64.xml. The site root, a landing page, `stable/` and a custom 404 do not exist (Pages build_type
// "workflow", custom_404 false). Every deployment so far came from beta.5's `desktop release` feed job, which built
// the site from pages-feed.mjs's `channelFiles('beta')` only (no stable release was ever published). No workflow on
// main deploys Pages, so nothing else owns any part of the site. A future landing page must be added to this build.
//
// The beta files are checked, never trusted:
//   - desktop-beta.json: the detached Ed25519 signature over the exact bytes under the metadata keys pinned in
//     clients/desktop/release-trust.json (beta.5 key slot A, D8), then the document the way
//     internal/desktopdownload.Verify reads it (format 1, channel "beta", a beta version, sane dates). A feed that is
//     authentic but past its expiresAt is reported and kept as it is (it is not an attack, and re-signing it is
//     beta.5's job, not ours).
//   - appcast-arm64.xml: its Sparkle signature block under beta.5's pinned Sparkle key, and its one enclosure must be
//     the signed feed's macOS arm64 artifact (url, length, Sparkle signature) and version.
// Anything else (a bad signature, a missing file, a redirect, a server error) refuses the build.
//
// Usage:
//   desktop-gpui-pages.mjs build <signedDir> <siteDir>
//       signedDir holds desktop-gpui-<channel>.json and .sig; siteDir must be new or empty
//   desktop-gpui-pages.mjs verify <signedDir> [<siteDir>]
//       after the deploy: the live site serves the signed GPUI feed, the live beta files still check out, and (given
//       siteDir) every built file is served byte for byte
import { createPublicKey, verify as ed25519Verify } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { mkdir, readdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { feedName, loadTrust, verify as verifyGpuiFeed } from './desktop-gpui-feed.mjs';

export const SITE = 'https://dilla-chat.github.io/dilla/';

/**
 * The live beta.5 files: pages-feed.mjs `channelFiles('beta')` at tag desktop-v0.1.0-beta.5
 * (`desktop-${channel}.json`, `desktop-${channel}.json.sig`, `appcast-arm64.xml`).
 */
export const BETA_FILES = Object.freeze(['beta/desktop-beta.json', 'beta/desktop-beta.json.sig', 'beta/appcast-arm64.xml']);

/** beta.5's Sparkle public key (`sparklePublicKey` in packages/desktop/release-trust.json at desktop-v0.1.0-beta.5). */
export const BETA5_SPARKLE_KEY = 'WVmjsR8I0WySt+5FOQJboNvoc4evI+r8dW5qZq6d+EA=';

const MAX_FILE = 1 << 20;
const CLOCK_SKEW_MS = 5 * 60000;
const MAX_VALIDITY_MS = 93 * 86400000;
const SIGNATURE = /^[A-Za-z0-9+/]{86}==$/;
const BETA_VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-beta\.(0|[1-9]\d*)$/;
const UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$/;
const ESCAPED = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&apos;' };
const xml = (value) => String(value).replace(/[&<>"']/g, (c) => ESCAPED[c]);

/** The site paths of the signed GPUI feed of `channel`. */
export const gpuiFiles = (channel) => [`gpui-${channel}/${feedName(channel)}`, `gpui-${channel}/${feedName(channel)}.sig`];

/** Reads site files from the live site: 200 only, no redirects, no cache, at most 1 MiB each. */
export async function fetchLive(paths, { fetchImpl = fetch, now = Date.now() } = {}) {
  const files = new Map();
  for (const path of paths) {
    const url = new URL(path, SITE);
    url.searchParams.set('t', String(now));
    let response;
    try {
      response = await fetchImpl(url.href, { redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(30000) });
    } catch (error) {
      throw new Error(`cannot read ${path} from the live site: ${error.message}`);
    }
    if (response.status === 404) throw new Error(`the live site does not serve ${path}`);
    if (response.status !== 200) throw new Error(`cannot read ${path} from the live site (HTTP ${response.status})`);
    const bytes = Buffer.from(await response.arrayBuffer());
    if (bytes.length === 0 || bytes.length > MAX_FILE) throw new Error(`the live ${path} is empty or too large`);
    files.set(path, bytes);
  }
  return files;
}

function ed25519Key(base64) {
  const raw = Buffer.from(base64, 'base64');
  if (raw.length !== 32) throw new Error('the pinned Sparkle key is not an Ed25519 key');
  return createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: raw.toString('base64url') }, format: 'jwk' });
}

/** beta.5's signed feed document, checked as desktopdownload.Verify does except that expiry is reported. */
function checkBetaFeed(json, sig, keys, now) {
  if (json.length === 0 || json.length > MAX_FILE) throw new Error('beta feed size out of bounds');
  const sigText = sig.toString('latin1').replace(/\n$/, '');
  if (!SIGNATURE.test(sigText)) throw new Error('malformed beta feed signature');
  const raw = Buffer.from(sigText, 'base64');
  if (!keys.some((key) => ed25519Verify(null, json, key, raw))) throw new Error('the beta feed signature does not verify under the pinned metadata keys');
  const doc = JSON.parse(json.toString('utf8'));
  if (!doc || typeof doc !== 'object' || doc.format !== 1 || doc.channel !== 'beta' || typeof doc.version !== 'string' || !BETA_VERSION.test(doc.version)) {
    throw new Error('the beta feed\'s format, channel or version is not a beta release');
  }
  if (!UTC.test(doc.publishedAt) || !UTC.test(doc.expiresAt)) throw new Error('a beta feed date is not a UTC timestamp');
  const published = Date.parse(doc.publishedAt);
  const expires = Date.parse(doc.expiresAt);
  if (!Number.isFinite(published) || !Number.isFinite(expires) || published > now + CLOCK_SKEW_MS || expires <= published || expires - published > MAX_VALIDITY_MS) {
    throw new Error('the beta feed\'s dates are invalid');
  }
  return { doc, expired: expires <= now };
}

/** beta.5's Sparkle appcast: signed under the pinned Sparkle key and pointing at the signed feed's macOS update. */
function checkAppcast(feed, doc, sparkleKey) {
  const marker = Buffer.from('<!-- sparkle-signatures:\n');
  const boundary = feed.lastIndexOf(marker);
  if (boundary < 0) throw new Error('the beta appcast is not signed');
  const block = /^<!-- sparkle-signatures:\nedSignature: ([A-Za-z0-9+/]{86}==)\nlength: (\d+)\n-->\n$/.exec(feed.subarray(boundary).toString('utf8'));
  const body = feed.subarray(0, boundary);
  if (!block || Number(block[2]) !== boundary || !ed25519Verify(null, body, ed25519Key(sparkleKey), Buffer.from(block[1], 'base64'))) {
    throw new Error('the beta appcast\'s Sparkle signature does not verify under the pinned key');
  }
  const darwin = Array.isArray(doc.artifacts) ? doc.artifacts.filter((a) => a && a.platform === 'darwin' && a.arch === 'arm64') : [];
  if (darwin.length !== 1) throw new Error('the beta feed has no single macOS arm64 update');
  const [artifact] = darwin;
  const text = body.toString('utf8');
  const enclosures = text.match(/<enclosure /g) ?? [];
  const expected = `<enclosure url="${xml(artifact.url)}" sparkle:edSignature="${xml(artifact.sparkleSignature)}" length="${artifact.size}" type="application/octet-stream" />`;
  if (enclosures.length !== 1 || !text.includes(expected) || !text.includes(`<sparkle:shortVersionString>${xml(doc.version)}</sparkle:shortVersionString>`)) {
    throw new Error('the beta appcast does not point at the signed feed\'s macOS update');
  }
}

/**
 * Checks the beta files (a Map of BETA_FILES to bytes). `keys` are the pinned metadata public KeyObjects.
 * Returns `{ version, expiresAt, expired }`; throws on anything but an authentic feed.
 */
export function checkBeta(files, { keys, sparkleKey = BETA5_SPARKLE_KEY, now = Date.now() }) {
  if (!Array.isArray(keys) || keys.length === 0) throw new Error('no pinned metadata key');
  const [json, sig, appcast] = BETA_FILES.map((path) => {
    const bytes = files.get(path);
    if (!Buffer.isBuffer(bytes)) throw new Error(`missing ${path}`);
    return bytes;
  });
  const { doc, expired } = checkBetaFeed(json, sig, keys, now);
  checkAppcast(appcast, doc, sparkleKey);
  return { version: doc.version, expiresAt: doc.expiresAt, expired };
}

async function readSigned(signedDir, channel) {
  const name = feedName(channel);
  const json = await readFile(join(signedDir, name)).catch(() => null);
  const sig = await readFile(join(signedDir, name + '.sig')).catch(() => null);
  if (!json || !sig) throw new Error(`the signed directory lacks ${name} or ${name}.sig`);
  return { json, sig };
}

/**
 * Builds the site in `outSite` (new or empty): the live beta files, checked and copied byte for byte, plus the signed
 * GPUI feed from `signedDir`, checked under the same pins. Writes nothing unless every check passes.
 * Returns `{ beta: { version, expiresAt, expired }, gpui: { channel, version }, files }`.
 */
export async function build(signedDir, outSite, { trust, fetchImpl = fetch, now = Date.now(), sparkleKey = BETA5_SPARKLE_KEY } = {}) {
  const pins = trust ?? (await loadTrust());
  const { json, sig } = await readSigned(signedDir, pins.channel);
  const gpui = verifyGpuiFeed(json, sig, pins, { channel: pins.channel, now: new Date(now) });
  const beta = await fetchLive(BETA_FILES, { fetchImpl, now });
  const report = checkBeta(beta, { keys: pins.keys, sparkleKey, now });

  const site = new Map(beta);
  const [jsonPath, sigPath] = gpuiFiles(pins.channel);
  site.set(jsonPath, json);
  site.set(sigPath, sig);
  await mkdir(outSite, { recursive: true, mode: 0o755 });
  if ((await readdir(outSite)).length) throw new Error(`the site directory is not empty: ${outSite}`);
  for (const [path, bytes] of site) {
    await mkdir(dirname(join(outSite, path)), { recursive: true, mode: 0o755 });
    await writeFile(join(outSite, path), bytes, { flag: 'wx', mode: 0o644 });
  }
  for (const [path, bytes] of site) {
    if (!(await readFile(join(outSite, path))).equals(bytes)) throw new Error(`the built ${path} differs from its source`);
  }
  return { beta: report, gpui: { channel: pins.channel, version: gpui.version }, files: [...site.keys()] };
}

/**
 * After the deploy: the live site serves the signed GPUI feed byte for byte, the live beta files still check out,
 * and, given `siteDir`, every built file is served byte for byte. Pages and its CDN take a moment, so it retries.
 */
export async function verify(signedDir, { siteDir, trust, fetchImpl = fetch, attempts = 20, sleep = () => delay(15000), sparkleKey = BETA5_SPARKLE_KEY } = {}) {
  const pins = trust ?? (await loadTrust());
  const { json, sig } = await readSigned(signedDir, pins.channel);
  const [jsonPath, sigPath] = gpuiFiles(pins.channel);
  const expected = new Map([[jsonPath, json], [sigPath, sig]]);
  if (siteDir) {
    for (const path of BETA_FILES) expected.set(path, await readFile(join(siteDir, path)));
  }
  for (let attempt = 1; ; attempt++) {
    let problem;
    try {
      const live = await fetchLive([...BETA_FILES, jsonPath, sigPath], { fetchImpl, now: Date.now() + attempt });
      const report = checkBeta(live, { keys: pins.keys, sparkleKey });
      const stale = [...expected].filter(([path, bytes]) => !live.get(path).equals(bytes)).map(([path]) => path);
      if (stale.length === 0) return report;
      problem = `the live site does not serve the built ${stale.join(', ')}`;
    } catch (error) {
      problem = error.message;
    }
    if (attempt >= attempts) throw new Error(`GitHub Pages does not serve the built site: ${problem}`);
    await sleep();
  }
}

const describeBeta = (beta) => `beta ${beta.version} (${beta.expired ? `EXPIRED at ${beta.expiresAt}, kept as it is` : `expires ${beta.expiresAt}`})`;

// Run directly, on every OS: a Windows path (D:\a\...) or one with a space never equals `file://` + argv[1].
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'build' && args.length === 2) {
      const result = await build(resolve(args[0]), resolve(args[1]));
      if (result.beta.expired) console.log(`::warning::the live beta.5 feed expired at ${result.beta.expiresAt}; it is kept byte for byte`);
      console.log(`built the Pages site: ${describeBeta(result.beta)}, gpui-${result.gpui.channel} ${result.gpui.version}`);
      for (const file of result.files) console.log(`  ${file}`);
    } else if (command === 'verify' && (args.length === 1 || args.length === 2)) {
      const beta = await verify(resolve(args[0]), { siteDir: args[1] && resolve(args[1]) });
      console.log(`GitHub Pages serves the built site; ${describeBeta(beta)}`);
    } else {
      console.error('usage: desktop-gpui-pages.mjs build SIGNED_DIR SITE_DIR | verify SIGNED_DIR [SITE_DIR]');
      process.exit(2);
    }
  } catch (error) {
    console.error(`desktop-gpui-pages: ${error.message}`);
    process.exit(1);
  }
}
