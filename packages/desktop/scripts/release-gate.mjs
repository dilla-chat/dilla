import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { DOC_PATTERNS } from '../../../scripts/ci-areas.mjs';
import { REPOSITORY, createGithubApi, findGreenPushRun, fetchCiJobs, resolveTagCommit, listRunArtifacts, selectReleaseArtifacts } from './ci-run.mjs';
import { DESKTOP_TAG, compareDesktopVersions, previousDesktopVersion } from './release-notes.mjs';

/**
 * The keyed workflow's first step. Everything is re-derived from the GitHub API
 * and git objects; the workflow_run payload (HINT_TAG, HINT_RUN) is an untrusted
 * hint. Nothing from the tagged commit is read except its package.json version
 * string and its tree hash, and nothing from it is executed.
 */
export const TAG = DESKTOP_TAG;
const R = `/repos/${REPOSITORY}`;
const COMMIT = /^[a-f0-9]{40}$/;
const VERSION = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/;
const WORKFLOW_REF = `${REPOSITORY}/.github/workflows/desktop-release.yml@refs/heads/main`;
const REQUEST_WORKFLOW = '.github/workflows/desktop-release-request.yml';
const CI_OK_INTEGRATION = 15368;
const SANDBOX_MINIMUM_VALIDITY = 30 * 86400000;

const OUTPUTS = Object.freeze({
 tag: value => TAG.test(value),
 version: value => VERSION.test(value),
 channel: value => /^(beta|stable)$/.test(value),
 sha: value => COMMIT.test(value),
 tree: value => COMMIT.test(value),
 run: value => /^[1-9]\d*$/.test(value),
 artifact_ids: value => /^[1-9]\d*(,[1-9]\d*)*$/.test(value),
 previous: value => value === '' || TAG.test(value),
});
/** Validates every value before appending anything: a newline or "=" can never add an output. */
export function writeOutputs(file, values) {
 if (typeof file !== 'string' || !file) throw new Error('Missing Actions output file');
 const keys = Object.keys(values);
 if (keys.length !== Object.keys(OUTPUTS).length || keys.some((key, i) => key !== Object.keys(OUTPUTS)[i])) throw new Error('Unexpected gate output set');
 for (const [key, check] of Object.entries(OUTPUTS)) if (typeof values[key] !== 'string' || !check(values[key])) throw new Error('Invalid gate output: ' + key);
 appendFileSync(file, keys.map(key => `${key}=${values[key]}\n`).join(''));
}

function succeeds(git, args) { try { git(args); return true; } catch { return false; } }
/** True when the two commits have no difference outside the repository's documentation patterns. */
function codeEqual(git, a, b) {
 return succeeds(git, ['diff', '--quiet', '--no-renames', '--no-ext-diff', '--no-textconv', a, b, '--', '.', ...DOC_PATTERNS.map(pattern => `:(exclude)${pattern}`)]);
}
async function posture(label, read) {
 try { return await read(); } catch (error) { throw new Error(`Cannot read the ${label} posture: ${error.message}`); }
}

export async function runGate({ env, git, api, outputDirectory, now = Date.now() }) {
 // 1. Running as the keyed workflow from main, on the checked-out main commit.
 if (env.GITHUB_ACTIONS !== 'true' || env.GITHUB_EVENT_NAME !== 'workflow_run' || env.GITHUB_REPOSITORY !== REPOSITORY
  || env.GITHUB_REF !== 'refs/heads/main' || env.GITHUB_WORKFLOW_REF !== WORKFLOW_REF || !COMMIT.test(env.GITHUB_SHA ?? '')) throw new Error('The release gate runs only as the main-branch workflow_run of desktop-release.yml');
 if (git(['rev-parse', 'HEAD']) !== env.GITHUB_SHA) throw new Error('The checked-out HEAD differs from GITHUB_SHA');

 // 2. The tag hint is only a name to look up; the commit comes from the API.
 const hint = TAG.exec(env.HINT_TAG ?? '');
 if (!hint) throw new Error('The hint is not a desktop version tag');
 if (!/^[1-9]\d*$/.test(env.HINT_RUN ?? '') || !Number.isSafeInteger(Number(env.HINT_RUN))) throw new Error('Invalid triggering run id');
 const tag = hint[0], version = hint[1], channel = version.includes('-beta.') ? 'beta' : 'stable';
 const sha = await resolveTagCommit(api, tag);

 // 3. The triggering run must be this tag's request workflow push run.
 const trigger = await api(`${R}/actions/runs/${env.HINT_RUN}`, {}, true);
 if (!trigger || trigger.path !== REQUEST_WORKFLOW || trigger.event !== 'push' || trigger.head_sha !== sha || trigger.head_branch !== tag || trigger.conclusion !== 'success'
  || trigger.repository?.full_name !== REPOSITORY || trigger.head_repository?.full_name !== REPOSITORY) throw new Error('The triggering run is not this tag’s successful desktop release request push run');

 // 4. Ancestry of main.
 if (!succeeds(git, ['rev-parse', '--verify', '--quiet', 'origin/main^{commit}']) || !succeeds(git, ['cat-file', '-e', `${sha}^{commit}`])
  || !succeeds(git, ['merge-base', '--is-ancestor', sha, 'origin/main'])) throw new Error('The tagged commit is not an ancestor of origin/main');

 // 5. Main-equivalence: the tagged commit and the keyed checkout both match main's HEAD outside docs.
 if (!codeEqual(git, sha, 'origin/main')) throw new Error('The tagged commit has a code difference from main’s HEAD; tag the current main');
 if (!codeEqual(git, 'HEAD', 'origin/main')) throw new Error('The keyed workflow code is no longer main’s tip; start a new release request');

 // 6. The tagged package.json, read as data only.
 let tagged;
 try { tagged = JSON.parse(git(['show', `${sha}:packages/desktop/package.json`])).version; } catch { throw new Error('Cannot read the tagged desktop package version'); }
 if (tagged !== version) throw new Error('The tagged package.json version differs from the tag');
 const tree = git(['rev-parse', `${sha}^{tree}`]);
 if (!COMMIT.test(tree)) throw new Error('Invalid tagged tree');

 // 7. Repository posture.
 const immutable = await posture('immutable releases', () => api(`${R}/immutable-releases`, {}, true));
 if (immutable?.enabled !== true) throw new Error('Immutable releases must be enabled');
 const rules = await posture('main ruleset', () => api(`${R}/rules/branches/main`));
 const has = type => Array.isArray(rules) && rules.some(rule => rule?.type === type);
 const ciOk = Array.isArray(rules) && rules.some(rule => rule?.type === 'required_status_checks'
  && rule.parameters?.required_status_checks?.some?.(check => check?.context === 'ci-ok' && check.integration_id === CI_OK_INTEGRATION));
 for (const [ok, what] of [[has('pull_request'), 'pull_request'], [ciOk, `required ci-ok from integration ${CI_OK_INTEGRATION}`], [has('non_fast_forward'), 'non_fast_forward'], [has('deletion'), 'deletion']]) {
  if (!ok) throw new Error('The main ruleset lacks ' + what);
 }
 const environment = await posture('desktop-release environment', () => api(`${R}/environments/desktop-release`));
 const policies = await posture('desktop-release environment', () => api(`${R}/environments/desktop-release/deployment-branch-policies?per_page=100`));
 const list = policies?.branch_policies;
 if (environment?.deployment_branch_policy?.custom_branch_policies !== true || environment.deployment_branch_policy.protected_branches !== false
  || policies?.total_count !== 1 || !Array.isArray(list) || list.length !== 1 || list[0]?.name !== 'main' || list[0]?.type !== 'branch') throw new Error('The desktop-release environment must admit exactly the branch main');

 // 8. The green ci push run of exactly this commit, its jobs and its exact artifact set.
 const run = await findGreenPushRun(api, sha);
 const jobs = await fetchCiJobs(api, run.id);
 const { ids: artifactIds } = selectReleaseArtifacts(await listRunArtifacts(api, run.id));

 // 9. Not yet published, and newer than every published release of the channel.
 const published = [];
 for (let page = 1; ; page++) {
  const releases = await api(`${R}/releases?per_page=100&page=${page}`);
  if (!Array.isArray(releases)) throw new Error('Malformed release list');
  published.push(...releases.filter(release => release?.draft === false && typeof release.tag_name === 'string' && TAG.test(release.tag_name)).map(release => release.tag_name));
  if (releases.length < 100) break;
  if (page >= 100) throw new Error('Too many releases');
 }
 if (published.includes(tag)) throw new Error(`desktop ${version} is already published`);
 for (const name of published) {
  const other = TAG.exec(name)[1];
  if ((other.includes('-beta.') ? 'beta' : 'stable') === channel && compareDesktopVersions(version, other) <= 0) throw new Error(`desktop ${version} must be greater than every published ${channel} release (${other})`);
 }
 const refs = await api(`${R}/git/matching-refs/tags/desktop-v`);
 if (!Array.isArray(refs)) throw new Error('Malformed tag list');
 const previous = previousDesktopVersion(refs.map(ref => typeof ref?.ref === 'string' && ref.ref.startsWith('refs/tags/') ? ref.ref.slice('refs/tags/'.length) : '').filter(Boolean), version);

 // 10. Main's Linux sandbox component stays valid long enough to update into.
 let expiresAt;
 try { expiresAt = Date.parse(JSON.parse(git(['show', 'HEAD:packages/desktop/linux-sandbox-component.json'])).expiresAt); } catch { expiresAt = NaN; }
 if (!Number.isFinite(expiresAt) || expiresAt - Number(now) < SANDBOX_MINIMUM_VALIDITY) throw new Error('The Linux sandbox component expires within 30 days; refresh it first');

 // Every check passed: write the gate's records, then the validated outputs.
 await mkdir(outputDirectory, { recursive: true, mode: 0o700 });
 await writeFile(join(outputDirectory, 'ci-run.json'), JSON.stringify(run) + '\n', { flag: 'wx' });
 await writeFile(join(outputDirectory, 'ci-jobs.json'), JSON.stringify(jobs) + '\n', { flag: 'wx' });
 writeOutputs(env.GITHUB_OUTPUT, { tag, version, channel, sha, tree, run: String(run.id), artifact_ids: artifactIds.join(','), previous: previous ?? '' });
 return { tag, version, channel, sha, tree, ciRun: run.id, artifactIds, previous };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
 if (process.argv.length !== 3) throw new Error('Usage: release-gate.mjs GATE_DIR');
 const root = resolve(import.meta.dirname, '../../..');
 const git = args => execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
 const result = await runGate({ env: process.env, git, api: createGithubApi({ token: process.env.GH_TOKEN }), outputDirectory: resolve(process.argv[2]) });
 console.log(`Release gate passed: ${result.tag} at ${result.sha}, ci run ${result.ciRun}, ${result.artifactIds.length} artifacts.`);
}
