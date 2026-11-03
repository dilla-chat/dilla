import { readFile, lstat } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { isAbsolute, join, resolve, sep } from 'node:path';
import { pathToFileURL } from 'node:url';
import { verifyRelease } from '../src/update.ts';
import * as releaseVerifier from './verify-release.mjs';
import { REPOSITORY, createGithubApi, fetchCiJobs, resolveTagCommit } from './ci-run.mjs';
import { DESKTOP_TAG } from './release-notes.mjs';
import { createReleaseAssetUploader, listReleaseAssets, releaseAssetDigest } from './standard-release-upload.mjs';

/**
 * The keyless `publish` job of desktop-release.yml. It re-verifies the signed
 * release handed over by `sign` against main's pins and the accepted CI run,
 * then creates (or adopts and normalises) the versioned draft, uploads exactly
 * the signed inventory plus public records, publishes it and re-checks the
 * published result. The channel feed is not a release: a later keyless job
 * deploys it to GitHub Pages.
 */
const R = `/repos/${REPOSITORY}`;
const COMMIT = /^[a-f0-9]{40}$/;
const ROTATION_PROOF = 'rotation-proof.json';

/** The run context; DESKTOP_TAG and DESKTOP_SHA come from the sign job's validated outputs. */
export function publicationContext(env) {
 if (env.GITHUB_ACTIONS !== 'true' || env.GITHUB_REPOSITORY !== REPOSITORY || env.GITHUB_EVENT_NAME !== 'workflow_run' || env.GITHUB_REF !== 'refs/heads/main') {
  throw new Error('Desktop publication runs only as the main-branch workflow_run of desktop-release.yml');
 }
 const match = DESKTOP_TAG.exec(env.DESKTOP_TAG ?? '');
 if (!match) throw new Error('Invalid DESKTOP_TAG');
 if (!COMMIT.test(env.DESKTOP_SHA ?? '')) throw new Error('Invalid DESKTOP_SHA');
 if (env.DESKTOP_STAGED !== 'true' && env.DESKTOP_STAGED !== 'false') throw new Error('Invalid DESKTOP_STAGED');
 const version = match[1];
 return { tag: match[0], version, channel: version.includes('-beta.') ? 'beta' : 'stable', sha: env.DESKTOP_SHA, staged: env.DESKTOP_STAGED === 'true' };
}

/** An existing, normalised, absolute directory strictly inside $RUNNER_TEMP. */
async function runnerDirectory(env, name) {
 const temp = env.RUNNER_TEMP, value = env[name];
 if (typeof temp !== 'string' || !isAbsolute(temp) || resolve(temp) !== temp) throw new Error('RUNNER_TEMP must be an absolute path');
 if (typeof value !== 'string' || !isAbsolute(value) || resolve(value) !== value || !value.startsWith(temp + sep)) throw new Error(`${name} must be an absolute path inside RUNNER_TEMP`);
 let info;
 try { info = await lstat(value); } catch { throw new Error(`${name} does not exist`); }
 if (info.isSymbolicLink() || !info.isDirectory()) throw new Error(`${name} must be a directory`);
 return value;
}

async function findDraft(api, tag) {
 if (await api(`${R}/releases/tags/${encodeURIComponent(tag)}`, {}, true)) throw new Error(`desktop ${tag} is already published`);
 const matches = [];
 for (let page = 1; ; page++) {
  const list = await api(`${R}/releases?per_page=100&page=${page}`);
  if (!Array.isArray(list)) throw new Error('Malformed release list');
  matches.push(...list.filter(release => release?.tag_name === tag));
  if (list.length < 100) break;
  if (page >= 100) throw new Error('Too many releases');
 }
 if (matches.some(release => release.draft !== true)) throw new Error(`desktop ${tag} is already published`);
 if (matches.length > 1) throw new Error(`Refusing to publish: more than one draft exists for ${tag}`);
 return matches[0] ?? null;
}

/** Asserts a release's assets are exactly `expected` (name -> bytes): same names, sizes and digests. */
export function assertExactAssets(assets, expected, digest) {
 const names = assets.map(asset => asset?.name).sort(), wanted = [...expected.keys()].sort();
 if (names.length !== wanted.length || names.some((name, i) => name !== wanted[i])) throw new Error('The published asset set differs from the signed set');
 for (const asset of assets) {
  const bytes = expected.get(asset.name);
  if (asset.size !== bytes.length || asset.digest !== digest(bytes)) throw new Error('The published asset set differs from the signed set: ' + asset.name);
 }
}

export async function runPublisher({ mode, env, git, fetchImpl = fetch, trust, verifier = releaseVerifier, now = Date.now() }) {
 if (mode !== 'plan' && mode !== 'publish') throw new Error('Usage: publish-release.mjs plan | publish');
 const { tag, version, channel, sha, staged } = publicationContext(env);
 const candidate = await runnerDirectory(env, 'DESKTOP_RELEASE_CANDIDATE');
 const directory = await runnerDirectory(env, 'DESKTOP_INSTALLERS');
 const api = createGithubApi({ token: env.GH_TOKEN, fetchImpl });

 // The signed metadata, verified under main's pins, must name exactly DESKTOP_SHA.
 const metadataName = `desktop-${channel}.json`;
 const metadata = await readFile(join(candidate, metadataName));
 const signature = (await readFile(join(candidate, metadataName + '.sig'), 'utf8')).trim();
 const release = verifyRelease(metadata, signature, { publicKeys: trust.metadataPublicKeys, channel, platform: 'linux', arch: 'x64', currentVersion: '0.0.0', now });
 const source = release.source;
 if (release.version !== version || !source || !Number.isSafeInteger(source.ciRun) || source.ciRun < 1 || !COMMIT.test(source.tree ?? '')) throw new Error('Missing authenticated release source');
 if (source.commit !== sha || source.head !== sha) throw new Error('The signed source commit is not DESKTOP_SHA');
 if (await resolveTagCommit(api, tag) !== sha) throw new Error('The tag resolves to another commit than DESKTOP_SHA');
 try { git(['merge-base', '--is-ancestor', sha, 'origin/main']); } catch { throw new Error('DESKTOP_SHA is not an ancestor of main'); }
 if (git(['rev-parse', `${sha}^{tree}`]) !== source.tree) throw new Error('The signed source tree differs from DESKTOP_SHA’s tree');

 // The signed records join the independently collected CI files; then the whole bundle is re-verified.
 await verifier.stagePublicReleaseRecords({ candidate, directory, channel, version });
 const run = await api(`${R}/actions/runs/${source.ciRun}`);
 const jobs = await fetchCiJobs(api, source.ciRun);
 await verifier.verifyDesktopBundle({ metadata, signature, trust, directory, run, jobs, version, now });
 const expected = new Map();
 for (const name of [...release.assets.map(asset => asset.name), ...verifier.publicReleaseRecords(channel)]) expected.set(name, await readFile(join(directory, name)));
 // The rotation proof is public and permanent once published: it must verify before anything is written.
 let proof = null;
 try { proof = await readFile(join(candidate, ROTATION_PROOF)); } catch (error) { if (error?.code !== 'ENOENT') throw error; }
 if ((proof !== null) !== staged) throw new Error(staged ? 'The staged release lacks its rotation proof' : 'A rotation proof is present although nothing is staged');
 if (staged) {
  const manifest = `dilla-${version}-linux-x64.sandbox.json`, archive = `dilla-${version}-mac-arm64.zip`;
  await verifier.verifyRotationProof(proof, { trust, version, channel, metadataSignature: signature, sandboxSignature: expected.get(`dilla-${version}-linux-x64.sandbox.sig`),
   files: new Map([[metadataName, metadata], [manifest, expected.get(manifest)], [archive, expected.get(archive)]]) });
  expected.set(ROTATION_PROOF, proof);
 }
 if (mode === 'plan') return { release, expected: [...expected.keys()] };

 // A pre-existing draft is adopted only after it is normalised to exactly what was signed.
 const body = (await readFile(join(directory, 'release-notes.md'))).toString('utf8');
 const fields = { name: `Dilla desktop ${version}`, body, prerelease: channel === 'beta', target_commitish: sha };
 let target = await findDraft(api, tag);
 if (target) {
  target = await api(`${R}/releases/${target.id}`, { method: 'PATCH', body: { ...fields, draft: true } });
  for (const asset of await listReleaseAssets(api, target.id)) {
   if (!expected.has(asset.name)) await api(`${R}/releases/assets/${asset.id}`, { method: 'DELETE' });
  }
 } else {
  target = await api(`${R}/releases`, { method: 'POST', body: { tag_name: tag, ...fields, draft: true } });
 }
 if (target?.tag_name !== tag || target.draft !== true) throw new Error('GitHub returned an unexpected release draft');
 const upload = createReleaseAssetUploader({ api, token: env.GH_TOKEN, fetchImpl });
 for (const [name, bytes] of expected) await upload(target, name, bytes);
 assertExactAssets(await listReleaseAssets(api, target.id), expected, releaseAssetDigest);
 await api(`${R}/releases/${target.id}`, { method: 'PATCH', body: { draft: false } });

 // Re-read what is now public: exact assets, immutability, and the tag still on DESKTOP_SHA.
 const after = await api(`${R}/releases/${target.id}`);
 if (after?.draft !== false || after.tag_name !== tag) throw new Error('The versioned release did not publish');
 assertExactAssets(await listReleaseAssets(api, target.id), expected, releaseAssetDigest);
 if (after.immutable !== true) throw new Error('The published release is not immutable; enable immutable releases');
 if (await resolveTagCommit(api, tag) !== sha) throw new Error('The tag moved off DESKTOP_SHA after publishing');
 return { release, published: after };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 if (process.argv.length !== 3) throw new Error('Usage: publish-release.mjs plan | publish');
 const root = resolve(import.meta.dirname, '../../..');
 const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
 const trust = JSON.parse(await readFile(join(root, 'packages/desktop/release-trust.json'), 'utf8'));
 const result = await runPublisher({ mode: process.argv[2], env: process.env, git, trust });
 console.log(process.argv[2] === 'plan' ? `Verified the signed desktop ${result.release.version} release for publication.` : `Published desktop ${result.release.version}.`);
}
