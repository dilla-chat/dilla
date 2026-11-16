import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { cargoVersion, checkFeed, linuxAssets, parseSums, runGate } from './desktop-gpui-gate.mjs';
import { assetNames, build } from './desktop-gpui-feed.mjs';

const REPO = 'o/r';
const SHA = 'a'.repeat(40);
const TAG = 'desktop-gpui-v0.2.0-alpha.3';
const VERSION = '0.2.0-alpha.3';
const RUN = '4242';
const hex = (bytes) => createHash('sha256').update(bytes).digest('hex');

/** A consistent world: the draft's files, the API's view of them, git, gh and the attestations. */
function world(over = {}) {
  const version = over.version ?? VERSION;
  const tag = `desktop-gpui-v${version}`;
  const files = Object.fromEntries(linuxAssets(version).map((name) => [name, Buffer.from(`bytes of ${name}`)]));
  files.SHA256SUMS = Buffer.from(linuxAssets(version).map((name) => `${hex(files[name])}  ${name}\n`).join(''));
  over.files?.(files);
  const assets = Object.entries(files).map(([name, bytes], i) => ({ id: i + 1, name, size: bytes.length, state: 'uploaded', digest: `sha256:${hex(bytes)}`, uploader: { login: 'github-actions[bot]' } }));
  over.assets?.(assets);
  const release = { id: 99, tag_name: tag, draft: true, prerelease: true, author: { login: 'github-actions[bot]', type: 'Bot' }, assets };
  over.release?.(release);
  const trigger = { id: Number(RUN), name: 'desktop gpui release', path: '.github/workflows/desktop-gpui-release.yml', event: 'push', head_branch: tag, head_sha: SHA, status: 'completed', conclusion: 'success', repository: { full_name: REPO }, head_repository: { full_name: REPO } };
  over.trigger?.(trigger);
  const signRuns = { queued: [], in_progress: [{ id: 7 }] };
  over.signRuns?.(signRuns);
  const table = {
    [`/repos/${REPO}/commits/refs/tags/${tag}`]: { sha: over.tagSha ?? SHA },
    [`/repos/${REPO}/actions/runs/${RUN}`]: trigger,
    [`/repos/${REPO}/releases?per_page=100&page=1`]: over.releases ?? [{ id: 1, tag_name: 'v1.0.0', draft: false }, release],
  };
  for (const status of ['queued', 'in_progress', 'waiting', 'requested', 'pending']) table[`/repos/${REPO}/actions/workflows/desktop-gpui-sign.yml/runs?status=${status}&per_page=100`] = { workflow_runs: signRuns[status] ?? [] };
  const seen = [];
  const api = async (path) => {
    seen.push(path);
    if (!Object.hasOwn(table, path)) throw new Error(`GitHub API GET ${path} failed (404)`);
    return structuredClone(table[path]);
  };
  const cargo = `[package]\nname = "dilla-desktop"\nversion = "${over.cargoVersion ?? version}"\n\n[dependencies]\nversion = "nope"\n`;
  const git = (args) => {
    if (args[0] === 'merge-base') { if (over.offMain) throw new Error('exit 1'); return ''; }
    if (args[0] === 'show' && args[1] === `${SHA}:clients/desktop/dilla-desktop/Cargo.toml`) return cargo;
    throw new Error(`unexpected git ${args.join(' ')}`);
  };
  const downloaded = over.downloaded ?? files;
  const subjects = linuxAssets(version).map((name) => ({ name, digest: { sha256: hex(files[name]) } }));
  over.subjects?.(subjects);
  const attestation = [{ verificationResult: { statement: { subject: subjects }, signature: { certificate: { runInvocationURI: `https://github.com/${REPO}/actions/runs/${over.attestRun ?? RUN}/attempts/1` } } } }];
  const ghCalls = [];
  const gh = (args) => {
    ghCalls.push(args);
    if (args[0] === 'release' && args[1] === 'download') {
      const dir = args[args.indexOf('--dir') + 1];
      for (const [name, bytes] of Object.entries(downloaded)) writeFileSync(join(dir, name), bytes);
      return '';
    }
    if (args[0] === 'attestation' && args[1] === 'verify') return JSON.stringify(attestation);
    throw new Error(`unexpected gh ${args.join(' ')}`);
  };
  const dir = mkdtempSync(join(tmpdir(), 'dilla-gpui-gate-'));
  const env = {
    GITHUB_REPOSITORY: REPO, GITHUB_EVENT_NAME: 'workflow_run', GITHUB_REF: 'refs/heads/main', GITHUB_WORKFLOW_REF: `${REPO}/.github/workflows/desktop-gpui-sign.yml@refs/heads/main`,
    GITHUB_RUN_ATTEMPT: '1', GITHUB_RUN_ID: '7', GITHUB_OUTPUT: join(dir, 'output'), GH_TOKEN: 't', HINT_TAG: tag, HINT_RUN: RUN, ...over.env,
  };
  const greenCalls = [];
  const greenRun = async (genv, sha, opts) => { greenCalls.push({ genv, sha, workflow: opts.workflow }); if (over.notGreen) throw new Error('There is no green desktop-gpui push run on main'); return {}; };
  const gate = () => runGate({ env, api, git, gh, greenRun, downloadDir: join(dir, 'draft'), log: () => {} });
  return { gate, env, dir, seen, ghCalls, greenCalls, files };
}

async function refuses(over, pattern) {
  const w = world(over);
  try { await assert.rejects(w.gate(), pattern); } finally { rmSync(w.dir, { recursive: true, force: true }); }
}

test('the gate passes a consistent draft and writes its outputs', async () => {
  const w = world();
  const out = await w.gate();
  assert.deepEqual(out, { tag: TAG, version: VERSION, sha: SHA, release_run: RUN, release_id: '99', sums_sha256: hex(w.files.SHA256SUMS), dry_run: 'false' });
  assert.equal(readFileSync(w.env.GITHUB_OUTPUT, 'utf8'), Object.entries(out).map(([k, v]) => `${k}=${v}\n`).join(''));
  assert.deepEqual(w.greenCalls, [{ genv: { GH_TOKEN: 't', GITHUB_REPOSITORY: REPO }, sha: SHA, workflow: 'desktop-gpui' }]);
  // One attestation check per Linux asset, pinned to the release workflow, the tag, the commit and hosted runners.
  const verifies = w.ghCalls.filter((a) => a[1] === 'verify');
  assert.equal(verifies.length, 5);
  for (const args of verifies) {
    for (const [flag, value] of [['--repo', REPO], ['--signer-workflow', `${REPO}/.github/workflows/desktop-gpui-release.yml`], ['--source-ref', `refs/tags/${TAG}`], ['--source-digest', SHA], ['--format', 'json']]) {
      assert.equal(args[args.indexOf(flag) + 1], value, flag);
    }
    assert.ok(args.includes('--deny-self-hosted-runners'));
  }
  rmSync(w.dir, { recursive: true, force: true });
});

test('the dry-run tag (D9) passes with dry_run=true', async () => {
  const w = world({ version: '0.0.0-alpha.0' });
  assert.equal((await w.gate()).dry_run, 'true');
  rmSync(w.dir, { recursive: true, force: true });
});

test('the gate runs only as main\'s first workflow_run attempt, alone', async () => {
  await refuses({ env: { GITHUB_RUN_ATTEMPT: '2' } }, /refuses a re-run/);
  await refuses({ env: { GITHUB_EVENT_NAME: 'workflow_dispatch' } }, /only as main's workflow_run/);
  await refuses({ env: { GITHUB_REF: 'refs/tags/desktop-gpui-v0.2.0-alpha.3' } }, /only as main's workflow_run/);
  await refuses({ env: { GITHUB_WORKFLOW_REF: `${REPO}/.github/workflows/other.yml@refs/heads/main` } }, /only as main's workflow_run/);
  await refuses({ signRuns: (r) => { r.queued = [{ id: 8 }]; } }, /another desktop-gpui-sign run \(8\) is queued/);
});

test('the tag hint must be a GPUI alpha tag that resolves to the triggering run\'s commit', async () => {
  for (const tag of ['desktop-v0.1.0-beta.6', 'v1.2.3', 'desktop-gpui-v0.2.0-beta.1', 'desktop-gpui-v0.2.0-alpha.01', 'desktop-gpui-v0.2.0-alpha.1\n']) {
    await refuses({ env: { HINT_TAG: tag } }, /not for a desktop-gpui-v/, tag);
  }
  await refuses({ tagSha: 'b'.repeat(40) }, /is not a green .* run of/);
  await refuses({ env: { HINT_RUN: '12; rm' } }, /invalid triggering run id/);
});

test('the triggering run must be this repository\'s green desktop-gpui-release.yml run of the tag', async () => {
  for (const [field, value] of [['path', '.github/workflows/release.yml'], ['name', 'release'], ['event', 'pull_request'], ['head_branch', 'main'], ['conclusion', 'failure'], ['status', 'in_progress'], ['head_repository', { full_name: 'fork/r' }]]) {
    await refuses({ trigger: (t) => { t[field] = value; } }, /is not a green/, field);
  }
  const w = world({ trigger: (t) => { t.event = 'workflow_dispatch'; } });
  await w.gate();
  rmSync(w.dir, { recursive: true, force: true });
});

test('the commit must be on main, carry the tag\'s Cargo version and have a green desktop-gpui push run', async () => {
  await refuses({ offMain: true }, /not on main/);
  await refuses({ cargoVersion: '0.2.0-alpha.2' }, /dilla-desktop is 0\.2\.0-alpha\.2/);
  await refuses({ notGreen: true }, /no green desktop-gpui push run/);
});

test('the release must be one draft prerelease by github-actions[bot] with exactly the Linux files', async () => {
  await refuses({ release: (r) => { r.draft = false; } }, /must be a draft prerelease/);
  await refuses({ release: (r) => { r.prerelease = false; } }, /must be a draft prerelease/);
  await refuses({ release: (r) => { r.author = { login: 'someone', type: 'User' }; } }, /was made by "someone"/);
  await refuses({ releases: [] }, /exactly one release/);
  await refuses({ assets: (a) => { a.push({ ...a[0], id: 50, name: 'extra.bin' }); } }, /must hold exactly/);
  await refuses({ assets: (a) => { a.pop(); } }, /must hold exactly/);
  await refuses({ assets: (a) => { a[1].uploader = { login: 'someone' }; } }, /was not uploaded by github-actions\[bot\]/);
  await refuses({ assets: (a) => { a[0].digest = null; } }, /has no SHA-256 digest/);
});

test('the downloaded files must match the API digests, SHA256SUMS and the attestation subjects exactly', async () => {
  const deb = `dilla-${VERSION}-linux-x64.deb`;
  await refuses({ downloaded: (() => { const w = world(); const f = { ...w.files, [deb]: Buffer.from('swapped') }; rmSync(w.dir, { recursive: true, force: true }); return f; })() }, /differs from the draft's recorded digest/);
  // A SHA256SUMS that disagrees with its asset (consistent with the API, so only the sums check catches it).
  await refuses({ files: (f) => { f.SHA256SUMS = Buffer.from(String(f.SHA256SUMS).replace(/^[a-f0-9]{64}/, '0'.repeat(64))); } }, /does not match SHA256SUMS/);
  await refuses({ files: (f) => { f.SHA256SUMS = Buffer.from(String(f.SHA256SUMS).split('\n').slice(1).join('\n')); } }, /exactly the five Linux assets/);
  await refuses({ subjects: (s) => { s.pop(); } }, /no attestation from run/);
  await refuses({ subjects: (s) => { s.push({ name: 'other.bin', digest: { sha256: '0'.repeat(64) } }); } }, /no attestation from run/);
  await refuses({ subjects: (s) => { s[0].digest.sha256 = '0'.repeat(64); } }, /no attestation from run/);
  await refuses({ attestRun: '1' }, /no attestation from run 4242/);
});

test('cargoVersion reads the [package] version only, and parseSums is strict', () => {
  assert.equal(cargoVersion('[package]\nname = "x"\nversion = "1.2.3-alpha.4"\n[dependencies]\n'), '1.2.3-alpha.4');
  assert.throws(() => cargoVersion('[dependencies]\nversion = "1.0.0"\n'), /no \[package\] version/);
  assert.deepEqual([...parseSums(`${'a'.repeat(64)}  x.deb\n`)], [['x.deb', 'a'.repeat(64)]]);
  assert.throws(() => parseSums(`${'a'.repeat(64)} x.deb\n`), /malformed/);
  assert.throws(() => parseSums(`${'a'.repeat(64)}  x.deb\n${'b'.repeat(64)}  x.deb\n`), /repeated/);
});

test('checkFeed accepts the signed feed over the release files and refuses any difference', async () => {
  const { privateKey, publicKey } = generateKeyPairSync('ed25519');
  const trust = { channel: 'alpha', keys: [publicKey] };
  const dir = mkdtempSync(join(tmpdir(), 'dilla-gpui-feed-check-'));
  for (const name of assetNames(VERSION)) writeFileSync(join(dir, name), `bytes of ${name}`);
  const { json, sig } = await build(dir, VERSION, 'alpha', { privateKey, trust });
  assert.equal(checkFeed(json, sig, VERSION, dir, trust).version, VERSION);
  assert.throws(() => checkFeed(json, sig, '0.2.0-alpha.4', dir, trust), /the feed is for 0\.2\.0-alpha\.3/);
  writeFileSync(join(dir, `dilla-${VERSION}-win-x64-setup.exe`), 'other bytes');
  assert.throws(() => checkFeed(json, sig, VERSION, dir, trust), /win-x64-setup\.exe differs from the signed feed/);
  const other = generateKeyPairSync('ed25519').publicKey;
  assert.throws(() => checkFeed(json, sig, VERSION, dir, { channel: 'alpha', keys: [other] }), /does not verify/);
  rmSync(dir, { recursive: true, force: true });
});
