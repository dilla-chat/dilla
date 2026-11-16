#!/usr/bin/env node
// The gate of desktop-gpui-sign.yml (GPUI release plan, Task 14a; Review Focus 3 and 4).
//
// desktop-gpui-sign.yml is a workflow_run workflow: GitHub always runs main's copy, so only main's code is ever near
// the desktop-release environment's keys. The run that triggered it (desktop-gpui-release.yml, a tag run) is an
// untrusted hint: its head_branch names a tag and its id names a run, nothing more. This gate re-derives everything
// from the API and git, and refuses unless all of it holds:
//   1. it runs as main's desktop-gpui-sign.yml workflow_run, on its first attempt (a partial re-run would reuse this
//      gate's outputs without re-running it), and no other desktop-gpui-sign run is queued or in progress;
//   2. the hint is a desktop-gpui-v<X.Y.Z>-alpha.<N> tag; the API resolves it to its commit;
//   3. the triggering run is desktop-gpui-release.yml of this repository, for exactly that tag and commit, and green;
//   4. the commit is an ancestor of origin/main, and dilla-desktop's Cargo version there is the tag's;
//   5. the commit has a green desktop-gpui push run on main (scripts/server-release-gate.mjs --workflow=desktop-gpui);
//   6. the tag's release is a draft prerelease made by github-actions[bot], holding exactly the five Linux assets and
//      SHA256SUMS, each uploaded by github-actions[bot];
//   7. the downloaded files' SHA-256 equal the API's asset digests and SHA256SUMS, and each Linux asset verifies under
//      `gh attestation verify` (signed by desktop-gpui-release.yml at that tag and commit, in the triggering run, on a
//      GitHub-hosted runner) with an attestation whose subjects are exactly the five assets and their digests.
// Outputs (GITHUB_OUTPUT): tag, version, sha, run, release_id, sums_sha256, dry_run. The verified files stay in the
// download directory for the workflow to pass on as an artifact.
//
// `check-feed` is the publish job's cross-check: a signed feed verifies under the pinned key, names the release's
// version, and lists exactly the files in a directory by size and sha512.
//
// Usage:
//   GH_TOKEN=... HINT_TAG=... HINT_RUN=... desktop-gpui-gate.mjs gate <download-dir>
//   desktop-gpui-gate.mjs check-feed <feed.json> <feed.json.sig> <version> <assets-dir>
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { appendFileSync, readdirSync, readFileSync, mkdirSync, lstatSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';
import { run as greenRunGate } from './server-release-gate.mjs';
import { assetNames, loadTrust, verify as verifyFeed } from './desktop-gpui-feed.mjs';

export const SIGN_WORKFLOW = '.github/workflows/desktop-gpui-sign.yml';
export const RELEASE_WORKFLOW = '.github/workflows/desktop-gpui-release.yml';
export const RELEASE_WORKFLOW_NAME = 'desktop gpui release';
export const TAG = /^desktop-gpui-v((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)-alpha\.(?:0|[1-9]\d*))$/;
/** D9: the throwaway dry-run tag's version. Everything runs except taking the release out of draft and the feed. */
export const DRY_RUN_VERSION = '0.0.0-alpha.0';
export const LINUX_SUFFIXES = Object.freeze(['linux-x64.AppImage', 'linux-x64.tar.gz', 'linux-x64.deb', 'linux-x64.rpm', 'linux-x64.aur.tar.gz']);
export const BOT = 'github-actions[bot]';
export const ACTIVE_STATUSES = Object.freeze(['queued', 'in_progress', 'waiting', 'requested', 'pending']);
export const linuxAssets = (version) => LINUX_SUFFIXES.map((suffix) => `dilla-${version}-${suffix}`);

const API_ORIGIN = 'https://api.github.com';
const RETRY_DELAYS = [1000, 3000];
const COMMIT = /^[a-f0-9]{40}$/;
const RUN_ID = /^[1-9]\d{0,19}$/;
const SHA256 = /^[a-f0-9]{64}$/;

/** A GitHub API GET returning JSON. A 5xx or network error is retried; a 4xx is not; redirects are refused. */
export function githubApi({ token, fetchImpl = fetch, sleep = (ms) => delay(ms), log = (line) => console.log(line) }) {
  return async (path) => {
    const attempts = RETRY_DELAYS.length + 1;
    for (let attempt = 1; ; attempt++) {
      const last = attempt >= attempts;
      let response;
      try {
        response = await fetchImpl(new URL(path, API_ORIGIN), {
          method: 'GET',
          headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
          redirect: 'error',
          signal: AbortSignal.timeout(30000),
        });
      } catch (error) {
        if (last) throw error;
        log(`GitHub API GET ${path.split('?')[0]} network error (${error?.name ?? 'Error'}); retrying`);
        await sleep(RETRY_DELAYS[attempt - 1]);
        continue;
      }
      if (response.status >= 500 && !last) {
        await response.body?.cancel();
        log(`GitHub API GET ${path.split('?')[0]} failed (${response.status}); retrying`);
        await sleep(RETRY_DELAYS[attempt - 1]);
        continue;
      }
      if (!response.ok) throw new Error(`GitHub API GET ${path.split('?')[0]} failed (${response.status})`);
      return response.json();
    }
  };
}

const sha256 = (bytes) => createHash('sha256').update(bytes).digest('hex');

/** dilla-desktop's `[package]` version from its Cargo.toml text. */
export function cargoVersion(text) {
  const pkg = /^\[package\]\s*$([\s\S]*?)(?=^\[|(?![\s\S]))/m.exec(String(text));
  const version = pkg && /^version\s*=\s*"([^"\n]+)"\s*$/m.exec(pkg[1]);
  if (!version) throw new Error('dilla-desktop\'s Cargo.toml has no [package] version');
  return version[1];
}

/** SHA256SUMS text -> Map(name -> hex). Exactly `<64 hex>  <name>` lines, each name once. */
export function parseSums(text) {
  const sums = new Map();
  const lines = String(text).split('\n');
  if (lines.at(-1) === '') lines.pop();
  for (const line of lines) {
    const m = /^([a-f0-9]{64}) {2}([A-Za-z0-9._-]+)$/.exec(line);
    if (!m || sums.has(m[2])) throw new Error(`SHA256SUMS has a malformed or repeated line: ${JSON.stringify(line)}`);
    sums.set(m[2], m[1]);
  }
  return sums;
}

/** The subjects of one verified attestation as "name sha256" lines, sorted. */
const subjectSet = (result) => (result?.verificationResult?.statement?.subject ?? [])
  .map((s) => `${s?.name} ${s?.digest?.sha256}`).sort().join('\n');

/**
 * The gate. `api(path)` reads the GitHub API, `git(args)` runs git in the main checkout (throws on a non-zero exit),
 * `gh(args)` runs the GitHub CLI and returns its stdout. Returns the outputs after writing them to GITHUB_OUTPUT.
 */
export async function runGate({ env, api, git, gh, downloadDir, greenRun = greenRunGate, greenRunOptions = {}, log = (line) => console.log(line) }) {
  // 1. main's workflow_run, first attempt, alone.
  const repo = env.GITHUB_REPOSITORY ?? '';
  if (!/^[\w.-]+\/[\w.-]+$/.test(repo)) throw new Error('GITHUB_REPOSITORY must be owner/name');
  if (env.GITHUB_EVENT_NAME !== 'workflow_run' || env.GITHUB_REF !== 'refs/heads/main' || env.GITHUB_WORKFLOW_REF !== `${repo}/${SIGN_WORKFLOW}@refs/heads/main`) {
    throw new Error(`the gate runs only as main's workflow_run of ${SIGN_WORKFLOW}`);
  }
  if (env.GITHUB_RUN_ATTEMPT !== '1') throw new Error(`the gate refuses a re-run (attempt ${JSON.stringify(env.GITHUB_RUN_ATTEMPT ?? null)}): a keyed run runs once; see clients/desktop/RELEASING.md for recovery`);
  if (!RUN_ID.test(env.GITHUB_RUN_ID ?? '')) throw new Error('invalid GITHUB_RUN_ID');
  if (!env.GITHUB_OUTPUT) throw new Error('GITHUB_OUTPUT is not set');
  if (!env.GH_TOKEN) throw new Error('GH_TOKEN is required');
  for (const status of ACTIVE_STATUSES) {
    const result = await api(`/repos/${repo}/actions/workflows/desktop-gpui-sign.yml/runs?status=${status}&per_page=100`);
    if (!Array.isArray(result?.workflow_runs)) throw new Error('malformed workflow run list');
    const other = result.workflow_runs.find((r) => String(r?.id) !== env.GITHUB_RUN_ID);
    if (other) throw new Error(`another desktop-gpui-sign run (${other.id}) is ${status}; one release at a time`);
  }

  // 2. The tag hint is a name to look up; its commit comes from the API.
  const hint = TAG.exec(env.HINT_TAG ?? '');
  if (!hint) throw new Error(`the triggering run is not for a desktop-gpui-v<X.Y.Z>-alpha.<N> tag: ${JSON.stringify(env.HINT_TAG ?? null)}`);
  if (!RUN_ID.test(env.HINT_RUN ?? '')) throw new Error('invalid triggering run id');
  const [tag, version] = hint;
  const commit = await api(`/repos/${repo}/commits/refs/tags/${tag}`);
  const sha = commit?.sha;
  if (typeof sha !== 'string' || !COMMIT.test(sha)) throw new Error(`cannot resolve ${tag} to a commit`);

  // 3. The triggering run.
  const trigger = await api(`/repos/${repo}/actions/runs/${env.HINT_RUN}`);
  if (!trigger || String(trigger.id) !== env.HINT_RUN || trigger.name !== RELEASE_WORKFLOW_NAME || trigger.path !== RELEASE_WORKFLOW
    || !['push', 'workflow_dispatch'].includes(trigger.event) || trigger.head_branch !== tag || trigger.head_sha !== sha
    || trigger.status !== 'completed' || trigger.conclusion !== 'success'
    || trigger.repository?.full_name !== repo || trigger.head_repository?.full_name !== repo) {
    throw new Error(`run ${env.HINT_RUN} is not a green ${RELEASE_WORKFLOW} run of ${tag} at ${sha} in ${repo}`);
  }

  // 4. On main, and the version is the tagged Cargo version (read as data, never run).
  try { git(['merge-base', '--is-ancestor', sha, 'origin/main']); } catch { throw new Error(`${tag} points at ${sha}, which is not on main`); }
  const cargo = cargoVersion(git(['show', `${sha}:clients/desktop/dilla-desktop/Cargo.toml`]));
  if (cargo !== version) throw new Error(`dilla-desktop is ${cargo} at ${sha}, the tag says ${version}`);

  // 5. A green desktop-gpui push run on main for exactly this commit.
  await greenRun({ GH_TOKEN: env.GH_TOKEN, GITHUB_REPOSITORY: repo }, sha, { workflow: 'desktop-gpui', log, ...greenRunOptions });

  // 6. The draft release.
  const releases = [];
  for (let page = 1; ; page++) {
    const list = await api(`/repos/${repo}/releases?per_page=100&page=${page}`);
    if (!Array.isArray(list)) throw new Error('malformed release list');
    releases.push(...list.filter((r) => r?.tag_name === tag));
    if (list.length < 100) break;
    if (page >= 50) throw new Error('too many releases to list');
  }
  if (releases.length !== 1) throw new Error(`expected exactly one release of ${tag}, found ${releases.length}`);
  const [release] = releases;
  if (release.draft !== true || release.prerelease !== true) throw new Error(`the release of ${tag} must be a draft prerelease (draft=${release.draft}, prerelease=${release.prerelease}); a published release is immutable`);
  if (release.author?.login !== BOT || release.author?.type !== 'Bot') throw new Error(`the draft of ${tag} was made by ${JSON.stringify(release.author?.login ?? null)}, not ${BOT}`);
  if (!Number.isSafeInteger(release.id) || release.id < 1) throw new Error('invalid release id');
  const linux = linuxAssets(version);
  const expected = [...linux, 'SHA256SUMS'].sort();
  const assets = Array.isArray(release.assets) ? release.assets : [];
  const names = assets.map((a) => a?.name).sort();
  if (JSON.stringify(names) !== JSON.stringify(expected)) throw new Error(`the draft must hold exactly ${expected.join(', ')}; it holds ${names.join(', ') || 'nothing'}`);
  const apiDigest = new Map();
  for (const asset of assets) {
    if (asset.state !== 'uploaded' || asset.uploader?.login !== BOT) throw new Error(`draft asset ${asset.name} was not uploaded by ${BOT}`);
    const digest = /^sha256:([a-f0-9]{64})$/.exec(asset.digest ?? '')?.[1];
    if (!digest) throw new Error(`draft asset ${asset.name} has no SHA-256 digest`);
    apiDigest.set(asset.name, { digest, size: asset.size });
  }

  // 7. The files: digests, SHA256SUMS, attestations.
  mkdirSync(downloadDir, { recursive: true });
  if (readdirSync(downloadDir).length) throw new Error(`${downloadDir} is not empty`);
  gh(['release', 'download', tag, '--repo', repo, '--dir', downloadDir]);
  const files = readdirSync(downloadDir).sort();
  if (JSON.stringify(files) !== JSON.stringify(expected)) throw new Error(`downloaded ${files.join(', ')}, expected ${expected.join(', ')}`);
  const local = new Map();
  for (const name of files) {
    const path = join(downloadDir, name);
    if (!lstatSync(path).isFile()) throw new Error(`${name} is not a regular file`);
    const bytes = readFileSync(path);
    const want = apiDigest.get(name);
    local.set(name, sha256(bytes));
    if (local.get(name) !== want.digest || bytes.length !== want.size) throw new Error(`${name} differs from the draft's recorded digest or size`);
  }
  const sums = parseSums(readFileSync(join(downloadDir, 'SHA256SUMS'), 'utf8'));
  if (JSON.stringify([...sums.keys()].sort()) !== JSON.stringify([...linux].sort())) throw new Error('SHA256SUMS must list exactly the five Linux assets');
  for (const name of linux) if (sums.get(name) !== local.get(name)) throw new Error(`${name} does not match SHA256SUMS`);
  const subjects = linux.map((name) => `${name} ${local.get(name)}`).sort().join('\n');
  const runPrefix = `https://github.com/${repo}/actions/runs/${env.HINT_RUN}/`;
  for (const name of linux) {
    const out = gh(['attestation', 'verify', join(downloadDir, name), '--repo', repo, '--signer-workflow', `${repo}/${RELEASE_WORKFLOW}`,
      '--source-ref', `refs/tags/${tag}`, '--source-digest', sha, '--deny-self-hosted-runners', '--format', 'json']);
    let results;
    try { results = JSON.parse(out); } catch { throw new Error(`gh attestation verify printed no JSON for ${name}`); }
    const match = Array.isArray(results) && results.some((r) => subjectSet(r) === subjects
      && String(r?.verificationResult?.signature?.certificate?.runInvocationURI ?? '').startsWith(runPrefix));
    if (!match) throw new Error(`${name}: no attestation from run ${env.HINT_RUN} whose subjects are exactly the five Linux assets of the draft`);
  }

  const outputs = { tag, version, sha, run: env.HINT_RUN, release_id: String(release.id), sums_sha256: local.get('SHA256SUMS'), dry_run: String(version === DRY_RUN_VERSION) };
  for (const [key, value] of Object.entries(outputs)) {
    if (/[\r\n]/.test(value)) throw new Error(`output ${key} has a line break`);
  }
  appendFileSync(env.GITHUB_OUTPUT, Object.entries(outputs).map(([k, v]) => `${k}=${v}\n`).join(''));
  log(`gate passed: ${tag} at ${sha}, release run ${env.HINT_RUN}, draft ${release.id}${outputs.dry_run === 'true' ? ' (dry run: no publish, no feed)' : ''}`);
  return outputs;
}

/** The signed feed verifies, names `version`, and lists exactly the release's files in `dir` by size and sha512. */
export function checkFeed(json, sig, version, dir, trust, { now } = {}) {
  const doc = verifyFeed(json, sig, trust, now ? { now } : {});
  if (doc.version !== version) throw new Error(`the feed is for ${doc.version}, the release is ${version}`);
  const names = assetNames(version);
  for (const [i, asset] of doc.assets.entries()) {
    if (asset.name !== names[i]) throw new Error(`feed asset ${i} is ${asset.name}, expected ${names[i]}`);
    const bytes = readFileSync(join(dir, asset.name));
    if (bytes.length !== asset.size || createHash('sha512').update(bytes).digest('base64') !== asset.sha512) throw new Error(`${asset.name} differs from the signed feed`);
  }
  return doc;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'gate' && args.length === 1) {
      const root = resolve(new URL('..', import.meta.url).pathname);
      const run = (bin) => (argv) => execFileSync(bin, argv, { cwd: root, encoding: 'utf8', maxBuffer: 64 << 20, stdio: ['ignore', 'pipe', 'inherit'] });
      await runGate({ env: process.env, api: githubApi({ token: process.env.GH_TOKEN }), git: run('git'), gh: run('gh'), downloadDir: resolve(args[0]) });
    } else if (command === 'check-feed' && args.length === 4) {
      const doc = checkFeed(readFileSync(args[0]), readFileSync(args[1]), args[2], args[3], await loadTrust());
      console.log(`the ${doc.channel} feed for ${doc.version} verifies and matches the ${doc.assets.length} files in ${args[3]}`);
    } else {
      console.error('usage: desktop-gpui-gate.mjs gate DOWNLOAD_DIR | check-feed FEED_JSON FEED_SIG VERSION ASSETS_DIR');
      process.exit(2);
    }
  } catch (error) {
    console.error(`desktop-gpui-gate: ${error.message}`);
    process.exit(1);
  }
}
