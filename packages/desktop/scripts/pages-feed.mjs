import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';
import { newerVersion, verifyRelease } from '../src/update.ts';
import { verifySparkleFeed } from './verify-release.mjs';

/**
 * The channel feed lives on GitHub Pages, not in a release: with immutable
 * releases a published `desktop-<channel>` release can never take new assets.
 * The keyless `feed` job builds the whole site from the verified signed output
 * of this run plus the other channel re-fetched from the live site and
 * re-verified under main's pins, deploys it, then checks the live bytes.
 */
export const FEED_SITE = 'https://dilla-chat.github.io/dilla/';
const CHANNELS = ['beta', 'stable'];
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const MAX_FILE = 1024 ** 2;

function validChannel(channel) { if (!CHANNELS.includes(channel)) throw new Error('Invalid desktop channel'); return channel; }
export function channelFiles(channel) {
 validChannel(channel);
 return [`desktop-${channel}.json`, `desktop-${channel}.json.sig`, 'appcast-arm64.xml'];
}
export function feedUrl(channel, file) {
 if (!channelFiles(channel).includes(file) && !/^appcast-(arm64|x64)\.xml$/.test(file)) throw new Error('Invalid feed file');
 return new URL(`${channel}/${file}`, FEED_SITE).href;
}

/** Authenticates one channel's files under the pins. `current` is false when they are authentic but outside their validity window. */
export function authenticateChannel(files, channel, trust, now) {
 const [json, sig, feed] = channelFiles(channel);
 const metadata = files.get(json), signature = files.get(sig)?.toString('utf8').trim() ?? '';
 const context = at => ({ publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now: at });
 let release, current = true;
 try { release = verifyRelease(metadata, signature, context(now)); }
 catch (error) {
  // Re-check at the document's own publication time: an authentic but expired feed is not an attack.
  let published;
  try { published = Date.parse(JSON.parse(metadata.toString('utf8')).publishedAt); } catch { throw error; }
  if (!Number.isFinite(published)) throw error;
  try { release = verifyRelease(metadata, signature, context(published)); } catch { throw error; }
  current = false;
 }
 const darwin = release.artifacts.find(artifact => artifact.platform === 'darwin' && artifact.arch === 'arm64');
 if (!darwin) throw new Error('The feed has no macOS arm64 update');
 verifySparkleFeed(release, darwin, files.get(feed), trust);
 return { release, current };
}

/** One channel from the live site: null when it was never deployed (metadata 404). */
export async function fetchLiveChannel({ channel, fetchImpl = fetch, now = Date.now() }) {
 const files = new Map();
 for (const name of channelFiles(channel)) {
  const url = new URL(feedUrl(channel, name)); url.searchParams.set('t', String(now));
  const response = await fetchImpl(url.href, { redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(30000) });
  if (response.status === 404) { files.set(name, null); continue; }
  if (!response.ok) throw new Error(`Cannot read the live ${channel} feed (${response.status})`);
  const bytes = Buffer.from(await response.arrayBuffer());
  if (bytes.length > MAX_FILE) throw new Error('Live feed file is too large: ' + name);
  files.set(name, bytes);
 }
 const present = [...files.values()].filter(Boolean).length;
 if (present === 0) return null;
 if (present !== files.size) throw new Error(`The live ${channel} feed is incomplete`);
 return files;
}

async function readSigned(signedDirectory, channel) {
 const files = new Map();
 for (const name of channelFiles(channel)) files.set(name, await readFile(join(signedDirectory, name)));
 return files;
}

export async function buildSite({ signedDirectory, channel, version, siteDirectory, fetchImpl = fetch, trust, now = Date.now() }) {
 validChannel(channel);
 if (typeof version !== 'string' || !VERSION.test(version)) throw new Error('Invalid desktop version');
 const signed = await readSigned(signedDirectory, channel);
 const { release, current } = authenticateChannel(signed, channel, trust, now);
 if (!current || release.version !== version) throw new Error('The signed feed does not carry the current version ' + version);

 // The same channel may only move forward (or be re-deployed byte for byte).
 const live = await fetchLiveChannel({ channel, fetchImpl, now });
 if (live) {
  const deployed = authenticateChannel(live, channel, trust, now).release;
  const identical = [...signed].every(([name, bytes]) => live.get(name).equals(bytes));
  if (!identical && !newerVersion(version, deployed.version)) throw new Error(`The live ${channel} feed (${deployed.version}) is newer than or equal to ${version}; refusing to roll it back`);
 }
 const site = new Map([[channel, signed]]), channels = { [channel]: version }, dropped = [];
 for (const other of CHANNELS.filter(item => item !== channel)) {
  const files = await fetchLiveChannel({ channel: other, fetchImpl, now });
  if (!files) continue;
  const result = authenticateChannel(files, other, trust, now);
  if (!result.current) { dropped.push(other); continue; }
  site.set(other, files); channels[other] = result.release.version;
 }
 await mkdir(siteDirectory, { mode: 0o755 });
 for (const [name, files] of site) {
  await mkdir(join(siteDirectory, name));
  for (const [file, bytes] of files) await writeFile(join(siteDirectory, name, file), bytes, { flag: 'wx' });
 }
 return { siteDirectory, channels, dropped };
}

/** After deployment: the live site must serve exactly the signed files (Pages and its CDN take a moment). */
export async function verifyLiveFeed({ signedDirectory, channel, fetchImpl = fetch, attempts = 20, sleep = () => delay(15000) }) {
 const signed = await readSigned(signedDirectory, channel);
 for (let attempt = 1; ; attempt++) {
  const live = await fetchLiveChannel({ channel, fetchImpl, now: Date.now() + attempt });
  if (live && [...signed].every(([name, bytes]) => live.get(name).equals(bytes))) return;
  if (attempt >= attempts) throw new Error(`GitHub Pages does not serve the signed ${channel} feed`);
  await sleep();
 }
}

export async function cli(argv, { trust } = {}) {
 const [command, ...args] = argv;
 trust ??= JSON.parse(await readFile(new URL('../release-trust.json', import.meta.url), 'utf8'));
 if (command === 'build' && args.length === 4) {
  const [signedDirectory, channel, version, siteDirectory] = args;
  const result = await buildSite({ signedDirectory: resolve(signedDirectory), channel, version, siteDirectory: resolve(siteDirectory), trust });
  return `Built the desktop feed site: ${Object.entries(result.channels).map(([name, value]) => `${name} ${value}`).join(', ')}${result.dropped.length ? `; dropped expired ${result.dropped.join(', ')}` : ''}.`;
 }
 if (command === 'verify' && args.length === 2) {
  await verifyLiveFeed({ signedDirectory: resolve(args[0]), channel: args[1] });
  return `GitHub Pages serves the signed ${args[1]} feed.`;
 }
 throw new Error('Usage: pages-feed.mjs build SIGNED_DIR CHANNEL VERSION SITE_DIR | verify SIGNED_DIR CHANNEL');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2)));
