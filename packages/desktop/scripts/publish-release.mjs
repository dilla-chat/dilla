import { readFile, writeFile, appendFile, mkdir } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { join, resolve } from 'node:path';
import { verifyRelease } from '../src/update.ts';
import { verifyDesktopBundle } from './verify-release.mjs';

const repository = 'jonasthim/dilla';
const root = resolve(import.meta.dirname, '../../..');
const mode = process.argv[2];
const ref = process.env.GITHUB_REF_NAME ?? '';
const match = /^desktop-v((0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?)$/.exec(ref);
if (!['plan', 'verify', 'publish'].includes(mode) || process.env.GITHUB_ACTIONS !== 'true' || process.env.GITHUB_REPOSITORY !== repository || !match || process.env.GITHUB_REF !== `refs/tags/${ref}`) throw new Error('Desktop publication runs only for this repository’s desktop version tags');
const version = match[1], channel = version.includes('-beta.') ? 'beta' : 'stable';
const candidate = join(root, 'packages/desktop/releases', version);
const directory = join(root, 'desktop-release-artifacts');
const metadataName = `desktop-${channel}.json`;
const metadata = await readFile(join(candidate, metadataName));
const signature = (await readFile(join(candidate, metadataName + '.sig'), 'utf8')).trim();
const trust = JSON.parse(await readFile(join(root, 'packages/desktop/release-trust.json'), 'utf8'));
const release = verifyRelease(metadata, signature, { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now: Date.now() });
if (release.version !== version || !release.source || !Number.isSafeInteger(release.source.ciRun) || release.source.ciRun < 1
 || ![release.source.commit, release.source.head, release.source.tree].every(value => typeof value === 'string' && /^[a-f0-9]{40}$/.test(value))) throw new Error('Missing authenticated release source');
const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8' }).trim();
git(['merge-base', '--is-ancestor', process.env.GITHUB_SHA, 'origin/main']);
git(['fetch', 'origin', release.source.commit]);
if (git(['rev-parse', release.source.commit + '^{tree}']) !== release.source.tree) throw new Error('Accepted CI source tree mismatch');
// Only public signing/review/measurement records may follow the accepted build.
git(['diff', '--exit-code', release.source.commit, process.env.GITHUB_SHA, '--', '.', `:(exclude)packages/desktop/releases/${version}`]);
if (mode === 'plan') {
 if (!process.env.GITHUB_OUTPUT) throw new Error('Missing Actions output file');
 await appendFile(process.env.GITHUB_OUTPUT, `run=${release.source.ciRun}\nversion=${version}\nchannel=${channel}\n`);
 console.log('Authenticated release source and main-branch tag.');
 process.exit(0);
}
const token = process.env.GH_TOKEN;
if (!token) throw new Error('Publishing requires the workflow’s scoped GitHub token');
async function api(path, options = {}, optional404 = false) {
 const url = new URL(path, 'https://api.github.com');
 if (url.origin !== 'https://api.github.com' || !url.pathname.startsWith(`/repos/${repository}/`)) throw new Error('Invalid publication API destination');
 const response = await fetch(url, { method: options.method ?? 'GET', headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28', ...(options.body ? { 'Content-Type': 'application/json' } : {}) }, ...(options.body ? { body: JSON.stringify(options.body) } : {}), redirect: 'error', signal: AbortSignal.timeout(30000) });
 if (optional404 && response.status === 404) return null;
 if (!response.ok) throw new Error(`GitHub publication API failed (${response.status})`);
 return response.status === 204 ? null : response.json();
}
const run = await api(`/repos/${repository}/actions/runs/${release.source.ciRun}`);
const jobs = [];
for (let page = 1; ; page++) {
 const result = await api(`/repos/${repository}/actions/runs/${release.source.ciRun}/jobs?filter=latest&per_page=100&page=${page}`);
 jobs.push(...result.jobs); if (result.jobs.length < 100) break;
}
await mkdir(directory, { recursive: true });
const records = [metadataName, metadataName + '.sig', 'release-notes.md', 'security-review.md', ...['linux-x64', 'win-x64', 'mac-x64', 'mac-arm64'].map(target => `performance-${target}.json`), 'appcast-x64.xml', 'appcast-arm64.xml'];
for (const name of records) await writeFile(join(directory, name), await readFile(join(candidate, name)), { flag: 'wx' });
await verifyDesktopBundle({ metadata, signature, trust, directory, run, jobs, version });
console.log('Verified accepted native/client CI, review, performance reports and all six installer/update hashes.');
if (mode === 'verify') process.exit(0);

async function findRelease(tag) { return api(`/repos/${repository}/releases/tags/${encodeURIComponent(tag)}`, {}, true); }
async function upload(target, name, bytes, replace) {
 const existing = target.assets?.find(asset => asset.name === name);
 const sha256 = 'sha256:' + createHash('sha256').update(bytes).digest('hex');
 if (existing?.size === bytes.length && existing.digest === sha256) return;
 if (existing) {
  if (!replace) throw new Error('An immutable release asset already differs: ' + name);
  await api(`/repos/${repository}/releases/assets/${existing.id}`, { method: 'DELETE' });
 }
 const url = new URL(`https://uploads.github.com/repos/${repository}/releases/${target.id}/assets`); url.searchParams.set('name', name);
 const response = await fetch(url, { method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/octet-stream', Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' }, body: bytes, redirect: 'error', signal: AbortSignal.timeout(300000) });
 if (!response.ok) throw new Error(`Release asset upload failed (${response.status}): ${name}`);
 const result = await response.json();
 if (result.size !== bytes.length || result.digest !== sha256) throw new Error('GitHub stored an unexpected asset digest: ' + name);
}
const notes = await readFile(join(candidate, 'release-notes.md'), 'utf8');
let target = await findRelease(ref);
if (!target) target = await api(`/repos/${repository}/releases`, { method: 'POST', body: { tag_name: ref, target_commitish: process.env.GITHUB_SHA, name: `Dilla desktop ${version}`, body: notes, draft: true, prerelease: channel === 'beta' } });
for (const name of [...release.assets.map(asset => asset.name), ...records]) await upload(target, name, await readFile(join(directory, name)), target.draft === true);
if (target.draft) await api(`/repos/${repository}/releases/${target.id}`, { method: 'PATCH', body: { draft: false } });
// The channel feed moves only after the immutable version is downloadable.
const feedTag = `desktop-${channel}`;
let feed = await findRelease(feedTag);
if (!feed) feed = await api(`/repos/${repository}/releases`, { method: 'POST', body: { tag_name: feedTag, target_commitish: process.env.GITHUB_SHA, name: `Dilla desktop ${channel}`, body: `Authenticated desktop ${channel} update feed. Versioned artifacts remain in their desktop-v releases.`, draft: true, prerelease: true } });
for (const asset of release.assets) {
 const name = asset.name.replace(`dilla-${version}-`, 'dilla-');
 await upload(feed, name, await readFile(join(directory, asset.name)), true);
}
for (const name of [metadataName + '.sig', metadataName, 'appcast-x64.xml', 'appcast-arm64.xml']) await upload(feed, name, await readFile(join(directory, name)), true);
if (feed.draft) await api(`/repos/${repository}/releases/${feed.id}`, { method: 'PATCH', body: { draft: false } });
console.log(`Published desktop ${version} and promoted the authenticated ${channel} feed.`);
