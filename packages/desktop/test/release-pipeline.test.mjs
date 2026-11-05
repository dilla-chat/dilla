import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { pathToFileURL } from 'node:url';
import { runGate } from '../scripts/release-gate.mjs';
import { EXPECTED_ARTIFACTS } from '../scripts/ci-run.mjs';
import { buildCandidate, writeCandidateDigest, verifyCandidate } from '../scripts/release-candidate.mjs';
import { writeDigest, verifyDigest } from '../scripts/candidate-digest.mjs';
import { runPublisher } from '../scripts/publish-release.mjs';
import { verifyPublished } from '../scripts/verify-published.mjs';
import { buildReleaseFixture, isolatedReleaseTools } from './support/release-fixture.mjs';
import { fakeGithub } from './support/fake-github.mjs';
import { artifactHost } from './support/artifact-host.mjs';

const R = '/repos/dilla-chat/dilla';
const der = key => key.export({ type: 'pkcs8', format: 'der' });

/**
 * The keyed workflow end to end, in-process: gate (real git, mocked API) -> sign's candidate build (the
 * gate-digested CI zips fetched as blobs and read in-process; notes; acceptance) -> signInJob (keys only in
 * env) -> signed and candidate digests -> publish's digest checks -> publish plan and publish (in-memory
 * GitHub) -> verify-published. collect is an early-failure check only and is not on this path.
 */
test('the release pipeline publishes exactly the signed inventory from a tagged main commit', async () => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-pipeline-'));
 try {
  // A main checkout with the tagged commit at its tip.
  const repo = join(root, 'repo'); await mkdir(repo);
  const git = args => execFileSync('git', ['-c', 'core.hooksPath=/dev/null', ...args], { cwd: repo, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
  git(['init', '-q', '-b', 'main']);
  for (const [key, value] of [['user.email', 'test@example.invalid'], ['user.name', 'test'], ['commit.gpgsign', 'false']]) git(['config', key, value]);
  const files = { 'packages/desktop/package.json': JSON.stringify({ name: '@dilla/desktop', version: '1.2.3' }) + '\n', 'packages/desktop/linux-sandbox-component.json': JSON.stringify({ generation: 1, expiresAt: '2027-01-31T00:00:00Z' }) + '\n', 'src/app.js': 'export {};\n',
   'packages/desktop/release-trust.json': JSON.stringify({ signing: { active: 'A', staged: null } }) + '\n' };
  for (const [path, content] of Object.entries(files)) { await mkdir(dirname(join(repo, path)), { recursive: true }); await writeFile(join(repo, path), content); }
  git(['add', '-A']); git(['commit', '-q', '-m', 'release']);
  const sha = git(['rev-parse', 'HEAD']), tree = git(['rev-parse', 'HEAD^{tree}']);
  git(['update-ref', 'refs/remotes/origin/main', sha]);

  // The CI-built artifacts of the green run on that commit.
  const fixtureDirectory = join(root, 'fixture'); await mkdir(fixtureDirectory);
  const fixture = await buildReleaseFixture({ directory: fixtureDirectory, commit: sha, head: sha, tree });
  const { version, channel } = fixture, tag = `desktop-v${version}`, now = fixture.context.now;
  const run = { ...fixture.run, run_number: 5, path: '.github/workflows/desktop-ci.yml', event: 'push', head_branch: 'main' };
  const trust = { ...fixture.trust, signing: { active: 'A', staged: null } };
  // The gate's record bytes are the API's run and jobs as JSON lines; the host stores the ci run's ten zips and the gate's record zip.
  const runBytes = Buffer.from(JSON.stringify(run) + '\n'), jobsBytes = Buffer.from(JSON.stringify(fixture.jobs) + '\n');
  const host = await artifactHost({ fixture, runBytes, jobsBytes });
  const pulls = [{ number: 7, title: 'Ship the desktop release [x](http://e)', merged_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z', merge_commit_sha: sha, base: { ref: 'main' } }];
  const routes = {
   [`${R}/git/ref/tags/${tag}`]: { ref: `refs/tags/${tag}`, object: { type: 'commit', sha } },
   [`${R}/actions/runs/900`]: { id: 900, path: '.github/workflows/desktop-release-request.yml', event: 'push', head_branch: tag, head_sha: sha, status: 'completed', conclusion: 'success', repository: { full_name: 'dilla-chat/dilla' }, head_repository: { full_name: 'dilla-chat/dilla' } },
   [`${R}/rules/branches/main`]: [{ type: 'deletion' }, { type: 'non_fast_forward' }, { type: 'pull_request' }, { type: 'required_status_checks', parameters: { required_status_checks: [{ context: 'server-ok', integration_id: 15368 }, { context: 'desktop-ok', integration_id: 15368 }] } }],
   [`${R}/environments/desktop-release`]: { deployment_branch_policy: { protected_branches: false, custom_branch_policies: true } },
   [`${R}/environments/desktop-release/deployment-branch-policies?per_page=100`]: { total_count: 1, branch_policies: [{ id: 1, name: 'main', type: 'branch' }] },
   [`${R}/actions/workflows/desktop-ci.yml/runs?head_sha=${sha}&event=push&branch=main&status=completed&per_page=100`]: { workflow_runs: [run] },
   [`${R}/actions/runs/${run.id}/jobs?filter=latest&per_page=100&page=1`]: { jobs: fixture.jobs },
   [`${R}/actions/runs/${run.id}/artifacts?per_page=100&page=1`]: { artifacts: EXPECTED_ARTIFACTS.map((name, i) => ({ id: host.ids[i], name, digest: host.digests[i], expired: false })) },
   [`${R}/releases?per_page=100&page=1`]: [{ tag_name: 'desktop-v1.2.2', draft: false }],
   [`${R}/compare/desktop-v1.2.2...${sha}?per_page=100&page=1`]: { commits: [{ sha, commit: { committer: { date: '2026-11-01T00:00:00Z' } } }] },
   [`${R}/pulls?state=closed&base=main&sort=updated&direction=desc&per_page=100&page=1`]: pulls,
   // No other desktop release run is active: only this one (5000).
   ...Object.fromEntries(['queued', 'in_progress', 'waiting', 'requested', 'pending'].map(status =>
    [`${R}/actions/workflows/desktop-release.yml/runs?status=${status}&per_page=100`, { workflow_runs: status === 'in_progress' ? [{ id: 5000 }] : [] }])),
  };
  const api = async (path, options = {}, optional404 = false) => {
   if (options.method && options.method !== 'GET') throw new Error('read-only');
   if (!Object.hasOwn(routes, path)) { if (optional404) return null; throw new Error('unexpected API call ' + path); }
   return structuredClone(routes[path]);
  };
  const gitCalls = [];
  const gateGit = args => { gitCalls.push(args); return git(args); };

  // gate
  const outputFile = join(root, 'github-output'); await writeFile(outputFile, '');
  const gateDirectory = join(root, 'gate');
  const gate = await runGate({ env: { GITHUB_ACTIONS: 'true', GITHUB_EVENT_NAME: 'workflow_run', GITHUB_REPOSITORY: 'dilla-chat/dilla', GITHUB_REF: 'refs/heads/main', GITHUB_WORKFLOW_REF: 'dilla-chat/dilla/.github/workflows/desktop-release.yml@refs/heads/main', GITHUB_SHA: sha, HINT_TAG: tag, HINT_RUN: '900', GITHUB_RUN_ID: '5000', GITHUB_RUN_ATTEMPT: '1', GITHUB_OUTPUT: outputFile }, git: gateGit, api, outputDirectory: gateDirectory, now });
  assert.equal(gate.previous, 'desktop-v1.2.2');
  for (const args of gitCalls) {
   assert.ok(!['checkout', 'switch', 'worktree', 'reset', 'restore'].includes(args[0]), 'the tagged commit is never checked out: ' + args.join(' '));
   if (args[0] === 'show') assert.ok([`${sha}:packages/desktop/package.json`, 'HEAD:packages/desktop/linux-sandbox-component.json', 'HEAD:packages/desktop/release-trust.json'].includes(args[1]), args.join(' '));
  }

  // The gate uploaded its records; the ci run's zips are what GitHub stores under the gate's ids and digests.
  for (const name of ['ci-run.json', 'ci-jobs.json']) assert.deepEqual(await readFile(join(gateDirectory, name)), name === 'ci-run.json' ? runBytes : jobsBytes);
  const outputs = Object.fromEntries((await readFile(outputFile, 'utf8')).trim().split('\n').map(line => line.split(/=(.*)/s).slice(0, 2)));
  assert.equal(outputs.artifact_digests, host.env.ARTIFACT_DIGESTS);
  assert.equal(outputs.mac_update, 'zip', 'no macOS key change is pending');

  // sign: fetch the gate-digested zips itself (RECORDS_DIGEST in upload-artifact's bare-hex `artifact-digest` form, as the workflow passes it) and build the candidate in-process; nothing from collect reaches it.
  const candidate = join(root, 'candidate');
  await buildCandidate({ api, git, fetchImpl: host.fetchImpl, zipDirectory: join(root, 'zips'), candidateDirectory: candidate, env: { GITHUB_SHA: sha, GITHUB_RUN_ID: '5000', GITHUB_RUN_ATTEMPT: '1', GH_TOKEN: 'workflow-token',
   VERSION: outputs.version, SHA: outputs.sha, TREE: outputs.tree, RUN: outputs.run, ARTIFACT_IDS: outputs.artifact_ids, ARTIFACT_DIGESTS: outputs.artifact_digests,
   RECORDS_ID: host.env.RECORDS_ID, RECORDS_DIGEST: host.env.RECORDS_DIGEST.replace(/^sha256:/, ''), RUN_RECORD: outputs.run_record, JOBS_RECORD: outputs.jobs_record, PREVIOUS: outputs.previous } });
  const notes = await readFile(join(candidate, 'release-notes.md'), 'utf8');
  assert.equal(notes, `# Dilla desktop ${version}\n\n- #7 \`Ship the desktop release [x](http://e)\`\n`);
  const tools = await isolatedReleaseTools(join(root, 'tools'), { trust, sandboxConfig: fixture.sandbox.config, files: ['scripts/release-sign-job.mjs'] });
  const job = await import(pathToFileURL(join(root, 'tools/scripts/release-sign-job.mjs')).href);
  const secrets = { DESKTOP_METADATA_KEY_A: der(fixture.key.privateKey).toString('base64'), DESKTOP_SPARKLE_KEY_A: der(fixture.sparkle.privateKey).subarray(-32).toString('base64'), DESKTOP_SANDBOX_KEY_A: der(fixture.sandbox.pair.privateKey).toString('base64') };
  const signEnv = { GITHUB_ACTIONS: 'true', GITHUB_REF: 'refs/heads/main', GITHUB_EVENT_NAME: 'workflow_run', MAC_UPDATE: outputs.mac_update, ...secrets };
  const signed = join(root, 'signed');
  assert.deepEqual(await job.signInJob({ env: signEnv, version, candidateDirectory: candidate, outputDirectory: signed, trust, now: new Date(now) }), { staged: false });
  for (const name of Object.keys(secrets)) assert.ok(!(name in signEnv), 'the key variables are removed');
  const digest = await writeDigest(signed, { channel, version, staged: false });
  const candidateDigest = await writeCandidateDigest(candidate, version);

  // publish takes only sign's two uploads, each checked against sign's digest.
  await verifyDigest(signed, digest, { channel, version, staged: false });
  await verifyCandidate({ directory: candidate, env: { VERSION: version, SHA: sha, TREE: gate.tree, RUN: String(gate.ciRun), RUN_RECORD: gate.runRecord, JOBS_RECORD: gate.jobsRecord, CANDIDATE_DIGEST: candidateDigest } });

  // publish: plan, then publish into an in-memory GitHub; then verify what was published.
  const github = fakeGithub({ tags: { [tag]: sha }, runs: { [run.id]: run }, jobs: { [run.id]: fixture.jobs } });
  const env = { GITHUB_ACTIONS: 'true', GITHUB_REPOSITORY: 'dilla-chat/dilla', GITHUB_EVENT_NAME: 'workflow_run', GITHUB_REF: 'refs/heads/main', GITHUB_SHA: sha, GITHUB_RUN_ID: '5000', GITHUB_RUN_ATTEMPT: '1', GH_TOKEN: 'workflow-token', RUNNER_TEMP: root,
   DESKTOP_TAG: tag, DESKTOP_SHA: sha, DESKTOP_STAGED: 'false', DESKTOP_MAC_UPDATE: outputs.mac_update, DESKTOP_RELEASE_CANDIDATE: signed, DESKTOP_INSTALLERS: candidate };
  for (const mode of ['plan', 'publish']) await runPublisher({ mode, env, git, fetchImpl: github.fetch, trust, verifier: tools.verifier, now });
  const release = github.state.releases.find(item => item.tag_name === tag);
  assert.equal(release.draft, false);
  const published = release.assets.map(item => item.name).sort();
  const signedFiles = (await readdir(signed)).filter(name => name !== 'SHA256SUMS');
  const metadata = JSON.parse(await readFile(join(signed, `desktop-${channel}.json`), 'utf8'));
  for (const name of signedFiles) assert.ok(published.includes(name), `${name} is published`);
  for (const artifact of metadata.artifacts) assert.ok(published.includes(artifact.url.split('/').at(-1)), artifact.url);
  const verified = await verifyPublished({ env, fetchImpl: github.fetch, trust, verifier: tools.verifier, now });
  assert.equal(verified.assets, published.length, 'verify-published accepts exactly the signed inventory');
  assert.equal(release.body, notes);

  // No key file and no key bytes anywhere under the temp roots.
  const keyBytes = [der(fixture.key.privateKey), der(fixture.sparkle.privateKey).subarray(-32), der(fixture.sandbox.pair.privateKey)];
  for (const entry of await readdir(root, { recursive: true, withFileTypes: true })) {
   if (!entry.isFile() || entry.parentPath.includes(`${join(root, 'repo', '.git')}`)) continue;
   assert.ok(!/private/i.test(entry.name), entry.name);
   const content = await readFile(join(entry.parentPath, entry.name));
   for (const bytes of keyBytes) { assert.equal(content.indexOf(bytes), -1, entry.name); assert.ok(!content.toString('latin1').includes(bytes.toString('base64')), entry.name); }
  }
 } finally { await rm(root, { recursive: true, force: true }); }
});
