import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';
import { verifyRelease } from '../src/update.ts';
import { verifySparkleFeed } from './verify-release.mjs';
import { REPOSITORY, createGithubApi, hardenedGit, resolveTagCommit } from './ci-run.mjs';
import { DESKTOP_TAG, compareDesktopVersions } from './release-notes.mjs';
import { listReleaseAssets } from './standard-release-upload.mjs';

/**
 * The channel feed lives on GitHub Pages, not in a release: with immutable
 * releases a published `desktop-<channel>` release can never take new assets.
 * The keyless `feed` job builds the whole site from published, immutable
 * versioned releases (review batch B, I3). This channel's files are the signed
 * output of this run, and must equal the assets of the channel's latest
 * published release, which must be this version: that is the forward-only
 * baseline. The other channel's files come from its latest published release,
 * verified under the pins of the commit that release was signed for. The
 * live Pages site is never read before the deploy: it is only overwritten,
 * then checked to serve the signed bytes. So a defaced live file, or a live
 * feed signed by keys that a later rotation retired, cannot block a release.
 */
export const FEED_SITE = 'https://dilla-chat.github.io/dilla/';
const CHANNELS = ['beta', 'stable'];
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const MAX_FILE = 1024 ** 2;
const R = `/repos/${REPOSITORY}`;
const DOWNLOAD_ORIGINS = new Set(['https://github.com', 'https://release-assets.githubusercontent.com', 'https://objects.githubusercontent.com']);
const channelOf = version => version.includes('-beta.') ? 'beta' : 'stable';

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

/** The channel's newest published (non-draft) desktop-v* release, or null; it must be immutable. */
export async function latestPublished(api, channel) {
 validChannel(channel);
 let latest = null;
 for (let page = 1; ; page++) {
  const list = await api(`${R}/releases?per_page=100&page=${page}`);
  if (!Array.isArray(list)) throw new Error('Malformed release list');
  for (const release of list) {
   const match = release?.draft === false && typeof release.tag_name === 'string' ? DESKTOP_TAG.exec(release.tag_name) : null;
   if (!match || channelOf(match[1]) !== channel) continue;
   if (!latest || compareDesktopVersions(match[1], latest.version) > 0) latest = { release, version: match[1] };
  }
  if (list.length < 100) break;
  if (page >= 100) throw new Error('Too many releases');
 }
 if (latest && (latest.release.immutable !== true || latest.release.prerelease !== (channel === 'beta'))) throw new Error(`The latest published ${channel} release (${latest.version}) is not an immutable ${channel} release`);
 return latest;
}

/** The three channel files a published release carries, each checked against GitHub's size and sha256 for the asset. */
async function releaseChannelFiles({ api, fetchImpl, release, channel }) {
 const assets = await listReleaseAssets(api, release.id);
 const files = new Map();
 for (const name of channelFiles(channel)) {
  const matches = assets.filter(asset => asset?.name === name);
  if (matches.length !== 1) throw new Error(`The published release ${release.tag_name} lacks ${name}`);
  const [asset] = matches, expected = `https://github.com/${REPOSITORY}/releases/download/${release.tag_name}/${name}`;
  if (asset.browser_download_url !== expected || !Number.isSafeInteger(asset.size) || asset.size > MAX_FILE) throw new Error('Unexpected published feed asset: ' + name);
  const response = await fetchImpl(expected, { redirect: 'follow', signal: AbortSignal.timeout(60000) });
  if (!response.ok) throw new Error(`Cannot download ${name} from ${release.tag_name} (${response.status})`);
  if (!DOWNLOAD_ORIGINS.has(new URL(response.url).origin)) throw new Error('The feed asset download redirected to an unexpected host: ' + name);
  const bytes = Buffer.from(await response.arrayBuffer());
  if (bytes.length !== asset.size || 'sha256:' + createHash('sha256').update(bytes).digest('hex') !== asset.digest) throw new Error('A published feed asset does not match its release digest: ' + name);
  files.set(name, bytes);
 }
 return files;
}

async function readSigned(signedDirectory, channel) {
 const files = new Map();
 for (const name of channelFiles(channel)) files.set(name, await readFile(join(signedDirectory, name)));
 return files;
}

/**
 * `trust` is main's pins (this release's). `trustAt(tag)` returns the pins of
 * the commit another published release was signed for.
 */
export async function buildSite({ signedDirectory, channel, version, siteDirectory, api, fetchImpl = fetch, trust, trustAt, now = Date.now() }) {
 validChannel(channel);
 if (typeof version !== 'string' || !VERSION.test(version) || channelOf(version) !== channel) throw new Error('Invalid desktop version');
 const signed = await readSigned(signedDirectory, channel);
 const { release, current } = authenticateChannel(signed, channel, trust, now);
 if (!current || release.version !== version) throw new Error('The signed feed does not carry the current version ' + version);

 // Forward only: the channel's latest published immutable release must be exactly this one, carrying exactly these files.
 const latest = await latestPublished(api, channel);
 if (!latest || compareDesktopVersions(latest.version, version) < 0) throw new Error(`desktop ${version} is not published; the feed only follows published releases`);
 if (latest.version !== version) throw new Error(`The latest published ${channel} release (${latest.version}) is newer than ${version}; refusing to roll the feed back`);
 const published = await releaseChannelFiles({ api, fetchImpl, release: latest.release, channel });
 if (![...signed].every(([name, bytes]) => published.get(name).equals(bytes))) throw new Error(`The published ${version} release's channel files differ from the signed output`);

 const site = new Map([[channel, signed]]), channels = { [channel]: version }, dropped = [];
 for (const other of CHANNELS.filter(item => item !== channel)) {
  const otherLatest = await latestPublished(api, other);
  if (!otherLatest) continue;
  const files = await releaseChannelFiles({ api, fetchImpl, release: otherLatest.release, channel: other });
  const result = authenticateChannel(files, other, await trustAt(otherLatest.release.tag_name), now);
  if (result.release.version !== otherLatest.version) throw new Error(`The ${other} release ${otherLatest.release.tag_name} carries another version's feed`);
  if (!result.current) { dropped.push(other); continue; }
  site.set(other, files); channels[other] = otherLatest.version;
 }
 await mkdir(siteDirectory, { mode: 0o755 });
 for (const [name, files] of site) {
  await mkdir(join(siteDirectory, name));
  for (const [file, bytes] of files) await writeFile(join(siteDirectory, name, file), bytes, { flag: 'wx' });
 }
 return { siteDirectory, channels, dropped };
}

/** One channel from the live site: null when it was never deployed (metadata 404). Read only after the deploy. */
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

/** The pins a published release was signed for: release-trust.json at its tag's commit, which must be on main. */
export function trustAtTag({ api, git }) {
 return async tag => {
  if (!DESKTOP_TAG.test(tag ?? '')) throw new Error('Invalid desktop release tag');
  const sha = await resolveTagCommit(api, tag);
  try { git(['merge-base', '--is-ancestor', sha, 'origin/main']); } catch { throw new Error(`${tag} is not on main`); }
  return JSON.parse(git(['show', `${sha}:packages/desktop/release-trust.json`]));
 };
}

export async function cli(argv, { trust, env = process.env } = {}) {
 const [command, ...args] = argv;
 trust ??= JSON.parse(await readFile(new URL('../release-trust.json', import.meta.url), 'utf8'));
 if (command === 'build' && args.length === 4) {
  const [signedDirectory, channel, version, siteDirectory] = args;
  const api = createGithubApi({ token: env.GH_TOKEN });
  const git = hardenedGit(resolve(import.meta.dirname, '../../..'));
  const result = await buildSite({ signedDirectory: resolve(signedDirectory), channel, version, siteDirectory: resolve(siteDirectory), api, trust, trustAt: trustAtTag({ api, git }) });
  return `Built the desktop feed site: ${Object.entries(result.channels).map(([name, value]) => `${name} ${value}`).join(', ')}${result.dropped.length ? `; dropped expired ${result.dropped.join(', ')}` : ''}.`;
 }
 if (command === 'verify' && args.length === 2) {
  await verifyLiveFeed({ signedDirectory: resolve(args[0]), channel: args[1] });
  return `GitHub Pages serves the signed ${args[1]} feed.`;
 }
 throw new Error('Usage: pages-feed.mjs build SIGNED_DIR CHANNEL VERSION SITE_DIR | verify SIGNED_DIR CHANNEL');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) console.log(await cli(process.argv.slice(2)));
