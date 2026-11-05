import { generateKeyPairSync, createPrivateKey, createPublicKey, createHash, sign, verify } from 'node:crypto';
import { spawn as childSpawn, execFileSync } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { createInterface } from 'node:readline/promises';
import { join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { validateConfiguration, assertDedicatedKeys, parsePublicConfiguration } from './linux-sandbox-package.mjs';
import { macRotationOf } from './feed-format.mjs';
import { REPOSITORY } from './ci-run.mjs';
import { macUpdateFor } from './release-gate.mjs';
import { trustAtTag } from './pages-feed.mjs';
import { isMain } from './is-main.mjs';

/**
 * One-time key setup and staged rotation for the keyed desktop release workflow.
 *
 * Private keys exist only in this process: generated in memory, encoded into
 * Buffers (never strings), written to `gh secret set` on stdin (never argv,
 * never an environment variable, never a file), and zeroed. Only public keys
 * are written to the repository. Secrets go to the `desktop-release`
 * environment only, in slot A or B; the committed `signing` pointer in
 * release-trust.json names the active (and staged) slot.
 *
 *   setup  --reviewed-sha SHA [--accept-unprotected-main] [--repo owner/name]
 *   rotate --reviewed-sha SHA [--repo owner/name]   stage new keys in the inactive slot
 *   rotate --promote                                flip the active slot and record the pending macOS key change (public files only)
 *   rotate --retire [--repo owner/name]             delete the old slot's secrets and clear the pending macOS key change
 *   import-public PUBLIC_JSON
 */
export const ENVIRONMENT = 'desktop-release';
export const DEFAULT_REPO = 'dilla-chat/dilla';
const SLOTS = ['A', 'B'];
const KINDS = ['METADATA', 'SPARKLE', 'SANDBOX'];
export const secretNamesFor = slot => KINDS.map(kind => `DESKTOP_${kind}_KEY_${slot}`);
const SECRET_NAME = /^DESKTOP_(METADATA|SPARKLE|SANDBOX)_KEY_[AB]$/;
const SHA = /^[a-f0-9]{40}$/;
const OK_INTEGRATION = 15368;
// The aggregate checks main requires: one per CI workflow (server.yml, desktop-ci.yml).
const REQUIRED_CHECKS = Object.freeze(['server-ok', 'desktop-ok']);
// The only deployment policy the desktop-release environment may have (pinned by scripts/check-ci-workflow.mjs).
const MAIN_POLICY = Object.freeze({ name: 'main', type: 'branch' });
const DAY = 86400000;
const ED25519_PKCS8_PREFIX = Buffer.from('302e020100300506032b657004220420', 'hex');
const DESKTOP_ROOT = fileURLToPath(new URL('..', import.meta.url));
const TRUST_PATH = 'packages/desktop/release-trust.json';
const PUBLIC_FILES = ['packages/desktop/release-trust.json', 'packages/desktop/src/update-keys.ts', 'packages/desktop/linux-sandbox-component.json'];

// --- Buffer-only base64 -------------------------------------------------------------
const ALPHABET = Buffer.from('ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/');
/** Standard, single-line, padded base64 of `bytes`, built in a Buffer: no string ever holds it. */
export function encodeBase64Buffer(bytes) {
 if (!Buffer.isBuffer(bytes)) throw new Error('encodeBase64Buffer takes a Buffer');
 const out = Buffer.alloc(Math.ceil(bytes.length / 3) * 4);
 for (let i = 0, o = 0; i < bytes.length; i += 3) {
  const n = (bytes[i] << 16) | ((i + 1 < bytes.length ? bytes[i + 1] : 0) << 8) | (i + 2 < bytes.length ? bytes[i + 2] : 0);
  out[o++] = ALPHABET[(n >> 18) & 63]; out[o++] = ALPHABET[(n >> 12) & 63];
  out[o++] = i + 1 < bytes.length ? ALPHABET[(n >> 6) & 63] : 61;
  out[o++] = i + 2 < bytes.length ? ALPHABET[n & 63] : 61;
 }
 return out;
}
function decodeBase64Buffer(ascii) {
 const out = Buffer.alloc(Math.floor(ascii.length / 4) * 3);
 let o = 0;
 for (let i = 0; i < ascii.length; i += 4) {
  const v = [0, 1, 2, 3].map(k => ascii[i + k] === 61 ? 0 : ALPHABET.indexOf(ascii[i + k]));
  if (v.some(x => x < 0)) { out.fill(0); throw new Error('Invalid base64 key encoding'); }
  const n = (v[0] << 18) | (v[1] << 12) | (v[2] << 6) | v[3];
  out[o++] = (n >> 16) & 255;
  if (ascii[i + 2] !== 61) out[o++] = (n >> 8) & 255;
  if (ascii[i + 3] !== 61) out[o++] = n & 255;
 }
 const result = Buffer.alloc(o); out.copy(result, 0, 0, o); out.fill(0);
 return result;
}

// --- Key material -------------------------------------------------------------------
const rawPublicOf = key => createPublicKey(key).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
const fingerprint = bytes => createHash('sha256').update(bytes).digest('hex');

/** Fresh metadata, Sparkle and sandbox keys in the shapes the sign job reads (release-sign-job.mjs loadKeys). */
export function generateKeyMaterial() {
 const buffers = [];
 try {
  const [metadata, sparkle, sandbox] = [0, 1, 2].map(() => {
   const pair = generateKeyPairSync('ed25519');
   return { pair, der: pair.privateKey.export({ type: 'pkcs8', format: 'der' }) };
  });
  const metadataBase64 = encodeBase64Buffer(metadata.der); buffers.push(metadataBase64); metadata.der.fill(0);
  const seed = Buffer.alloc(32); sparkle.der.copy(seed, 0, ED25519_PKCS8_PREFIX.length); sparkle.der.fill(0);
  const seedBase64 = encodeBase64Buffer(seed); buffers.push(seedBase64); seed.fill(0);
  const sandboxBase64 = encodeBase64Buffer(sandbox.der); buffers.push(sandboxBase64); sandbox.der.fill(0);
  return {
   metadata: { derBase64: metadataBase64, publicPem: createPublicKey(metadata.pair.privateKey).export({ type: 'spki', format: 'pem' }).toString() },
   sparkle: { seedBase64, publicBase64: rawPublicOf(sparkle.pair.privateKey) },
   sandbox: { derBase64: sandboxBase64, publicBase64: rawPublicOf(sandbox.pair.privateKey) },
   zero() { for (const bytes of buffers) bytes.fill(0); },
  };
 } catch (error) { for (const bytes of buffers) bytes.fill(0); throw error; }
}

/** Re-imports each private key from its secret encoding and proves it signs for its published public key. */
function selfTest(material) {
 const probe = Buffer.from('dilla release key self-test');
 const check = (name, privateKey, publicKey) => { if (!verify(null, probe, publicKey, sign(null, probe, privateKey))) throw new Error(`The generated ${name} key failed its self-test`); };
 const rawKey = raw => createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(raw, 'base64').toString('base64url') }, format: 'jwk' });
 const decoded = [];
 try {
  const metadataDer = decodeBase64Buffer(material.metadata.derBase64); decoded.push(metadataDer);
  const sandboxDer = decodeBase64Buffer(material.sandbox.derBase64); decoded.push(sandboxDer);
  const seed = decodeBase64Buffer(material.sparkle.seedBase64); decoded.push(seed);
  if (metadataDer.length !== 48 || sandboxDer.length !== 48 || seed.length !== 32) throw new Error('A generated key has an unexpected size');
  const sparkleDer = Buffer.concat([ED25519_PKCS8_PREFIX, seed]); decoded.push(sparkleDer);
  check('metadata', createPrivateKey({ key: metadataDer, format: 'der', type: 'pkcs8' }), createPublicKey(material.metadata.publicPem));
  check('sandbox', createPrivateKey({ key: sandboxDer, format: 'der', type: 'pkcs8' }), rawKey(material.sandbox.publicBase64));
  check('Sparkle', createPrivateKey({ key: sparkleDer, format: 'der', type: 'pkcs8' }), rawKey(material.sparkle.publicBase64));
 } finally { for (const bytes of decoded) bytes.fill(0); }
 if (new Set([rawPublicOf(material.metadata.publicPem), material.sparkle.publicBase64, material.sandbox.publicBase64]).size !== 3) throw new Error('The generated keys of different kinds must differ');
}
const fingerprints = material => ({
 metadata: fingerprint(createPublicKey(material.metadata.publicPem).export({ type: 'spki', format: 'der' })),
 sparkle: fingerprint(Buffer.from(material.sparkle.publicBase64, 'base64')),
 sandbox: fingerprint(Buffer.from(material.sandbox.publicBase64, 'base64')),
});

// --- gh -----------------------------------------------------------------------------
/** The `gh` CLI as release-keys uses it. Key bytes only ever reach `gh secret set` on stdin. */
export function createGh({ repo = DEFAULT_REPO, environment = ENVIRONMENT, spawn = childSpawn } = {}) {
 if (!/^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/.test(repo)) throw new Error('Invalid repository');
 const run = (argv, input, stdio, label) => new Promise((resolvePromise, reject) => {
  const child = spawn('gh', argv, { stdio });
  const out = [], err = [];
  child.stdout?.on('data', chunk => out.push(chunk));
  child.stderr?.on('data', chunk => err.push(chunk));
  child.on('error', reject);
  child.on('close', code => {
   if (code === 0) return resolvePromise(Buffer.concat(out).toString());
   const stderr = Buffer.concat(err).toString().trim();
   const error = new Error(`gh ${label} failed: ${stderr.slice(0, 300)}`);
   error.status = Number(/HTTP (\d{3})/.exec(stderr)?.[1]) || undefined;
   reject(error);
  });
  if (child.stdin) { if (input) child.stdin.write(input); child.stdin.end(); }
 });
 const headers = ['-H', 'Accept: application/vnd.github+json', '-H', 'X-GitHub-Api-Version: 2022-11-28'];
 const api = async (method, path, body) => {
  const input = body === undefined ? null : Buffer.from(JSON.stringify(body));
  const text = await run(['api', '--method', method, ...headers, path, ...(input ? ['--input', '-'] : [])], input, [input ? 'pipe' : 'ignore', 'pipe', 'pipe'], `api ${method} ${path}`);
  return text.trim() ? JSON.parse(text) : null;
 };
 const names = async path => {
  const result = await api('GET', `${path}?per_page=100`);
  if (!Array.isArray(result?.secrets)) throw new Error('Malformed secret list: ' + path);
  return result.secrets.map(secret => secret.name);
 };
 return {
  api,
  async secretSet(name, buffer) {
   if (!SECRET_NAME.test(name) || !Buffer.isBuffer(buffer)) throw new Error('Invalid secret');
   // The value goes on stdin; stdout is ignored; argv holds only the name, environment and repository.
   await run(['secret', 'set', name, '--env', environment, '--repo', repo], buffer, ['pipe', 'ignore', 'pipe'], `secret set ${name}`);
  },
  async secretDelete(name) {
   if (!SECRET_NAME.test(name)) throw new Error('Invalid secret');
   await run(['secret', 'delete', name, '--env', environment, '--repo', repo], null, ['ignore', 'ignore', 'pipe'], `secret delete ${name}`);
  },
  secretNames(scope) {
   if (scope === 'env') return names(`repos/${repo}/environments/${environment}/secrets`);
   if (scope === 'repo') return names(`repos/${repo}/actions/secrets`);
   if (scope === 'org') return names(`repos/${repo}/actions/organization-secrets`);
   throw new Error('Invalid secret scope');
  },
 };
}

// --- Preflight ----------------------------------------------------------------------
function defaultGit(args) {
 return execFileSync('git', ['-c', 'core.hooksPath=/dev/null', '-c', 'core.fsmonitor=', ...args], { cwd: resolve(DESKTOP_ROOT, '../..'), encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1' } }).trim();
}
/** M8: the code that holds the keys is exactly the reviewed commit, unmodified. */
function assertReviewedCode(git, reviewedSha, log) {
 if (typeof reviewedSha !== 'string' || !SHA.test(reviewedSha)) throw new Error('--reviewed-sha must be the 40-hex commit that passed the security review');
 const head = git(['rev-parse', 'HEAD']);
 if (head !== reviewedSha) throw new Error(`HEAD ${head} is not the reviewed commit ${reviewedSha}`);
 if (git(['status', '--porcelain', '--untracked-files=all']) !== '') throw new Error('The working tree is not clean; run from the reviewed commit only');
 log(`Running from the reviewed commit ${head} (clean tree).`);
}
async function read(gh, path, what) {
 try { return await gh.api('GET', path); } catch (error) { if (error.status === 404) return null; throw new Error(`Cannot read ${what}: ${error.message}`); }
}
function assertOnlyMain(environment, policies, name) {
 const list = policies?.branch_policies;
 if (environment?.deployment_branch_policy?.custom_branch_policies !== true || environment.deployment_branch_policy.protected_branches !== false
  || !Array.isArray(list) || list.length !== 1 || policies.total_count !== 1 || list[0]?.name !== MAIN_POLICY.name || list[0]?.type !== MAIN_POLICY.type) {
  throw new Error(`The ${name} environment must admit exactly the branch main`);
 }
}

/** The concrete repository posture (I4, M7), printed; any refusal aborts before anything changes. */
export async function preflight({ gh, repo = DEFAULT_REPO, acceptUnprotectedMain = false, log = console.log }) {
 const R = `repos/${repo}`;
 const user = await read(gh, 'user', 'the signed-in user');
 log(`gh user: ${user?.login ?? '(unknown)'}`);
 const repository = await read(gh, R, 'the repository');
 if (repository?.permissions?.admin !== true) throw new Error(`${user?.login ?? 'This user'} is not an admin of ${repo}`);

 const rules = await read(gh, `${R}/rules/branches/main`, 'the main branch rules');
 if (!Array.isArray(rules)) throw new Error('Cannot read the main branch rules');
 const has = type => rules.some(rule => rule?.type === type);
 const requires = context => rules.some(rule => rule?.type === 'required_status_checks' && rule.parameters?.required_status_checks?.some?.(check => check?.context === context && check.integration_id === OK_INTEGRATION));
 const missing = [['pull_request', has('pull_request')], ...REQUIRED_CHECKS.map(context => [`required status check ${context} (integration ${OK_INTEGRATION})`, requires(context)]), ['non_fast_forward', has('non_fast_forward')], ['deletion', has('deletion')]].filter(([, ok]) => !ok).map(([name]) => name);
 log(`main rules: ${rules.map(rule => rule?.type).join(', ') || '(none)'}`);
 for (const id of [...new Set(rules.map(rule => rule?.ruleset_id).filter(Number.isSafeInteger))]) {
  const ruleset = await read(gh, `${R}/rulesets/${id}`, `ruleset ${id}`);
  if (ruleset?.enforcement !== 'active') missing.push(`ruleset ${id} enforcement active`);
  log(`main ruleset ${id} (${ruleset?.name ?? '?'}): enforcement ${ruleset?.enforcement ?? '?'}; bypass actors ${JSON.stringify(ruleset?.bypass_actors ?? [])}`);
 }
 if (missing.length) {
  log(`The main ruleset lacks: ${missing.join('; ')}`);
  if (!acceptUnprotectedMain) throw new Error(`The main ruleset lacks ${missing.join('; ')} (pass --accept-unprotected-main to proceed anyway)`);
  log('Proceeding with an unprotected main because --accept-unprotected-main was given.');
 }
 log('Residual: any credential that can merge to main (0 approvals, admin bypass "always") can obtain the signing keys.');

 const collaborators = await read(gh, `${R}/collaborators?affiliation=all&per_page=100`, 'the collaborators');
 log(`Collaborators with push or more (confirm by hand): ${(Array.isArray(collaborators) ? collaborators : []).filter(c => c?.permissions?.push || c?.permissions?.admin).map(c => `${c.login} (${c.role_name ?? 'push'})`).join(', ') || '(none)'}`);
 let installations = null;
 try { installations = await gh.api('GET', `orgs/${repo.split('/')[0]}/installations?per_page=100`); } catch (error) { log(`Installed GitHub Apps: cannot list (${error.message}); check the organisation settings by hand.`); }
 if (installations) log(`Installed GitHub Apps (confirm by hand): ${(installations.installations ?? []).map(app => `${app.app_slug} (${app.repository_selection})`).join(', ') || '(none)'}`);

 const workflow = await read(gh, `${R}/actions/permissions/workflow`, 'the default workflow permissions');
 if (workflow?.default_workflow_permissions !== 'read' || workflow.can_approve_pull_request_reviews !== false) throw new Error('Default workflow permissions must be read with can_approve_pull_request_reviews false');
 log('Default GITHUB_TOKEN: read, cannot approve pull requests.');

 // M7: a DESKTOP_ secret outside the environment is readable by any branch workflow.
 for (const [scope, label] of [['repo', 'Repository'], ['org', 'Organisation']]) {
  let names;
  try { names = await gh.secretNames(scope); } catch (error) { throw new Error(`Cannot list ${label.toLowerCase()}-level secrets: ${error.message}`); }
  const found = names.filter(name => /^DESKTOP_/.test(name));
  if (found.length) throw new Error(`${label}-level DESKTOP_ secrets exist (${found.join(', ')}); delete them first: any branch workflow can read them`);
 }
 log('No repository- or organisation-level DESKTOP_* secret.');

 const immutable = await read(gh, `${R}/immutable-releases`, 'the immutable releases setting');
 if (immutable?.enabled !== true) throw new Error('Immutable releases must be enabled');
 log('Immutable releases: enabled.');

 const pages = await read(gh, `${R}/pages`, 'the Pages site');
 if (pages?.build_type !== 'workflow') throw new Error('GitHub Pages must be enabled with build_type workflow (see RELEASING.md)');
 assertOnlyMain(await read(gh, `${R}/environments/github-pages`, 'the github-pages environment'), await read(gh, `${R}/environments/github-pages/deployment-branch-policies?per_page=100`, 'the github-pages policies'), 'github-pages');
 log(`Pages: build_type workflow at ${pages.html_url ?? '?'}; github-pages admits branch main only.`);
}

/** Creates or narrows the desktop-release environment to the single branch policy main, then reads it back. */
async function configureEnvironment(gh, repo, log) {
 const E = `repos/${repo}/environments/${ENVIRONMENT}`;
 await gh.api('PUT', E, { deployment_branch_policy: { protected_branches: false, custom_branch_policies: true }, can_admins_bypass: false, reviewers: [], wait_timer: 0 });
 const current = await gh.api('GET', `${E}/deployment-branch-policies?per_page=100`);
 let hasMain = false;
 for (const policy of current?.branch_policies ?? []) {
  if (!hasMain && policy?.name === MAIN_POLICY.name && policy?.type === MAIN_POLICY.type) { hasMain = true; continue; }
  if (!Number.isSafeInteger(policy?.id)) throw new Error('Malformed deployment branch policy');
  await gh.api('DELETE', `${E}/deployment-branch-policies/${policy.id}`);
 }
 if (!hasMain) await gh.api('POST', `${E}/deployment-branch-policies`, { ...MAIN_POLICY });
 await assertEnvironment(gh, repo);
 log(`Environment ${ENVIRONMENT}: admits exactly the branch main (no tags); admins cannot bypass.`);
}
async function assertEnvironment(gh, repo) {
 const E = `repos/${repo}/environments/${ENVIRONMENT}`;
 const environment = await read(gh, E, `the ${ENVIRONMENT} environment`);
 assertOnlyMain(environment, await read(gh, `${E}/deployment-branch-policies?per_page=100`, `the ${ENVIRONMENT} policies`), ENVIRONMENT);
 if (environment.can_admins_bypass !== false) throw new Error(`Admins can bypass the ${ENVIRONMENT} environment`);
}

// --- Public files -------------------------------------------------------------------
function validTrust(trust) {
 const pemOk = pem => { try { return createPublicKey(pem).asymmetricKeyType === 'ed25519'; } catch { return false; } };
 const raw = value => typeof value === 'string' && Buffer.from(value, 'base64').length === 32 && Buffer.from(value, 'base64').toString('base64') === value;
 const s = trust?.signing;
 const keys = trust?.metadataPublicKeys;
 if (!Array.isArray(keys) || keys.length < 1 || keys.length > 4 || !keys.every(pemOk) || new Set(keys).size !== keys.length || !raw(trust.sparklePublicKey)
  || !s || !SLOTS.includes(s.active) || !(s.staged === null || (SLOTS.includes(s.staged) && s.staged !== s.active))
  || (s.staged === null) !== (trust.sparkleNextPublicKey === undefined)
  || (trust.sparkleNextPublicKey !== undefined && (!raw(trust.sparkleNextPublicKey) || trust.sparkleNextPublicKey === trust.sparklePublicKey || keys.some(pem => rawPublicOf(pem) === trust.sparkleNextPublicKey)))) {
  throw new Error('Invalid release public keys');
 }
 const rotation = macRotationOf(trust);
 if (rotation && keys.some(pem => rawPublicOf(pem) === rotation.fromSparklePublicKey)) throw new Error('Invalid release public keys');
 return trust;
}
/** Validates everything, then writes release-trust.json, src/update-keys.ts and (if given) linux-sandbox-component.json. */
export async function writePublicTrust(root, trust, component) {
 validTrust(trust);
 if (component !== undefined) {
  validateConfiguration(component); assertDedicatedKeys(component, trust);
  if (trust.sparkleNextPublicKey !== undefined && component.publicKeys.includes(trust.sparkleNextPublicKey)) throw new Error('Sandbox maintenance keys must be separate from release keys');
 }
 const rotation = macRotationOf(trust);
 const ordered = { metadataPublicKeys: trust.metadataPublicKeys, sparklePublicKey: trust.sparklePublicKey, ...(trust.sparkleNextPublicKey === undefined ? {} : { sparkleNextPublicKey: trust.sparkleNextPublicKey }),
  signing: { active: trust.signing.active, staged: trust.signing.staged, ...(rotation ? { macRotation: { fromSlot: rotation.fromSlot, fromSparklePublicKey: rotation.fromSparklePublicKey } } : {}) } };
 await writeFile(join(root, 'release-trust.json'), JSON.stringify(ordered, null, 2) + '\n');
 await writeFile(join(root, 'src/update-keys.ts'), '// Public trust generated by release-keys.mjs. Private keys stay outside Git.\nexport const RELEASE_PUBLIC_KEYS: readonly string[] = Object.freeze(' + JSON.stringify(trust.metadataPublicKeys, null, 2) + ');\n');
 if (component !== undefined) await writeFile(join(root, 'linux-sandbox-component.json'), JSON.stringify(component, null, 2) + '\n');
}
async function readPublic(root) {
 const trust = JSON.parse(await readFile(join(root, 'release-trust.json'), 'utf8'));
 const component = parsePublicConfiguration(await readFile(join(root, 'linux-sandbox-component.json'), 'utf8'));
 return { trust, component };
}
const toSecond = date => new Date(Math.floor(date.getTime() / 1000) * 1000).toISOString().replace('.000Z', 'Z');
const freshDates = now => ({ publishedAt: toSecond(now), expiresAt: toSecond(new Date(now.getTime() + 90 * DAY)) });

async function setSecrets(gh, slot, material, log) {
 const values = [material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64];
 const set = [];
 try {
  for (const [i, name] of secretNamesFor(slot).entries()) { await gh.secretSet(name, values[i]); set.push(name); }
 } catch (error) {
  log(`Setting the slot ${slot} secrets failed after: ${set.join(', ') || '(none)'}. Repository files are untouched. Delete those secrets (gh secret delete NAME --env ${ENVIRONMENT}) and run again.`);
  throw error;
 }
 log(`Set ${set.join(', ')} in environment ${ENVIRONMENT} (stdin only).`);
}
function report(log, material) {
 const prints = fingerprints(material);
 log(`Fingerprints (sha256 of the public keys): metadata ${prints.metadata}, Sparkle ${prints.sparkle}, sandbox ${prints.sandbox}`);
 log(`Commit these public files: ${PUBLIC_FILES.join(', ')}`);
 log('No private key was written to disk.');
 return prints;
}

// --- Commands -----------------------------------------------------------------------
export async function setupRelease({ repo = DEFAULT_REPO, reviewedSha, acceptUnprotectedMain = false, gh, git = defaultGit, now = new Date(), root = DESKTOP_ROOT, log = console.log, generate = generateKeyMaterial }) {
 assertReviewedCode(git, reviewedSha, log);
 await preflight({ gh, repo, acceptUnprotectedMain, log });
 await configureEnvironment(gh, repo, log);
 const existing = (await gh.secretNames('env')).filter(name => /^DESKTOP_/.test(name));
 if (existing.length) throw new Error(`The ${ENVIRONMENT} environment already holds ${existing.join(', ')}; setup runs once (use rotate, or delete them first)`);
 const { component: current } = await readPublic(root);
 const material = generate();
 try {
  selfTest(material);
  const trust = { metadataPublicKeys: [material.metadata.publicPem], sparklePublicKey: material.sparkle.publicBase64, signing: { active: 'A', staged: null } };
  const component = { goVersion: current.goVersion, generation: current.generation, ...freshDates(now), publicKeys: [material.sandbox.publicBase64], helper: current.helper };
  validTrust(trust); validateConfiguration(component); assertDedicatedKeys(component, trust);
  await setSecrets(gh, 'A', material, log);
  await writePublicTrust(root, trust, component);
  return { fingerprints: report(log, material) };
 } finally { material.zero(); }
}

/** gh's read-only REST access in the shape the release scripts' `api(path, options, optional404)` takes. */
function readOnlyApi(gh, repo) {
 const prefix = `/repos/${REPOSITORY}/`;
 return async (path, options = {}, optional404 = false) => {
  if ((options.method ?? 'GET') !== 'GET' || typeof path !== 'string' || !path.startsWith(prefix)) throw new Error('Only GET requests to the repository are allowed here');
  try { return await gh.api('GET', `repos/${repo}/${path.slice(prefix.length)}`); } catch (error) { if (optional404 && error.status === 404) return null; throw error; }
 };
}
/**
 * Final review, m6: retire only once each channel's rotation release is out. The gate's own rule decides: a channel
 * whose newest published release (immutable, workflow-made) was built for the old Sparkle key still needs its DMG
 * release ('dmg'); a channel with no published release, or one already on main's key, is done ('zip').
 */
async function assertRotationReleasesOut({ gh, repo, git, trust }) {
 const api = readOnlyApi(gh, repo);
 for (const channel of ['beta', 'stable']) {
  let kind;
  try { kind = await macUpdateFor({ trust, channel, api, trustAt: trustAtTag({ api, git }) }); } catch (error) { throw new Error(`Cannot confirm the ${channel} rotation release: ${error.message}`); }
  if (kind !== 'zip') throw new Error(`The ${channel} rotation release is not out yet: the newest published ${channel} release was built for the old Sparkle key. Release ${channel} once, then retire`);
 }
}

export const MAC_ROTATION_WARNING = 'macOS: Sparkle trusts a single EdDSA key per build, so the Sparkle key changes only at --promote. The first release of each channel after the promote is its rotation release: Macs update through a Developer-ID-signed, notarized DMG that carries the new key, with no reinstall (RELEASING.md). Keep the old slot until every channel has shipped it; a Mac that skips its channel\'s rotation release reinstalls once.';

export async function rotateRelease({ repo = DEFAULT_REPO, promote = false, retire = false, reviewedSha, gh, git = defaultGit, confirm, now = new Date(), root = DESKTOP_ROOT, log = console.log, generate = generateKeyMaterial }) {
 if (promote && retire) throw new Error('Pass at most one of --promote and --retire');
 const { trust, component } = await readPublic(root);
 validTrust(trust);
 const { active, staged } = trust.signing;
 const ask = async (question, expected) => { const answer = typeof confirm === 'function' ? await confirm(question) : null; if (typeof answer !== 'string' || answer.trim() !== expected) throw new Error(`${expected.split(' ')[0] === 'promote' ? 'Promotion' : 'Retirement'} was not confirmed`); };

 if (promote) {
  if (staged === null) throw new Error('Nothing is staged; run rotate first');
  await ask(`The transition release (signed by slot ${active}, proving slot ${staged}) must have been published for at least 60 days. Type "promote ${staged}" to continue: `, `promote ${staged}`);
  // The staged keys become first (active); the old active keys stay pinned for one more release. The pending macOS
  // key change names the slot and Sparkle key the installed Macs trust: each channel's next release is its DMG
  // rotation release, whose appcast that slot signs (feed-format.mjs).
  const next = { metadataPublicKeys: [trust.metadataPublicKeys.at(-1), trust.metadataPublicKeys[0]], sparklePublicKey: trust.sparkleNextPublicKey,
   signing: { active: staged, staged: null, macRotation: { fromSlot: active, fromSparklePublicKey: trust.sparklePublicKey } } };
  // Promote comes at least 60 days after rotate, so rotate's dates would leave the gate under 30 days: the promote
  // release refreshes them and bumps the generation, as a freshness refresh does (final review, m5).
  const nextComponent = { ...component, generation: component.generation + 1, ...freshDates(now), publicKeys: [component.publicKeys.at(-1), component.publicKeys[0]] };
  validateConfiguration(nextComponent);
  await writePublicTrust(root, next, nextComponent);
  log(`Promoted slot ${staged}: it signs from the next release; the slot ${active} metadata and sandbox keys stay pinned for one more release.`);
  log(MAC_ROTATION_WARNING);
  log(`Commit ${PUBLIC_FILES.join(', ')}; once that PR is merged and each channel has published its rotation release (the first after the promote), run rotate --retire.`);
  return { fingerprints: null };
 }

 if (retire) {
  if (staged !== null) throw new Error('A rotation is staged; promote it before retiring');
  const old = active === 'A' ? 'B' : 'A';
  let committed;
  try { committed = JSON.parse(git(['show', `origin/main:${TRUST_PATH}`])); } catch { throw new Error('Cannot read release-trust.json on origin/main (git fetch first)'); }
  if (committed?.signing?.active !== active || committed.signing.staged !== null) throw new Error(`origin/main does not have slot ${active} active with nothing staged; merge the promote PR first`);
  const names = await gh.secretNames('env');
  if (!secretNamesFor(active).every(name => names.includes(name))) throw new Error(`The active slot ${active} secrets are not all present; refusing to delete slot ${old}`);
  const rotation = macRotationOf(trust);
  if (rotation) await assertRotationReleasesOut({ gh, repo, git, trust });
  await ask(rotation
   ? `Each channel's rotation release (its first release signed by slot ${active}, updating macOS through a disk image) must already be published; Macs that have not taken it reinstall after this. Type "retire ${old}" to delete the slot ${old} secrets: `
   : `A release signed by slot ${active} must already be published. Type "retire ${old}" to delete the slot ${old} secrets: `, `retire ${old}`);
  for (const name of secretNamesFor(old)) if (names.includes(name)) await gh.secretDelete(name);
  log(`Deleted the slot ${old} secrets from environment ${ENVIRONMENT}.`);
  if (rotation) {
   await writePublicTrust(root, { ...trust, signing: { active, staged: null } });
   log('Cleared the pending macOS key change: every later release updates macOS with the zip. Commit packages/desktop/release-trust.json.');
  }
  return { fingerprints: null };
 }

 if (staged !== null) throw new Error(`Slot ${staged} is already staged; promote it first`);
 if (macRotationOf(trust)) throw new Error('A macOS key change is still pending; publish each channel\'s rotation release, then run rotate --retire first');
 const target = active === 'A' ? 'B' : 'A';
 assertReviewedCode(git, reviewedSha, log);
 await preflight({ gh, repo, log });
 await assertEnvironment(gh, repo);
 const names = await gh.secretNames('env');
 if (names.some(name => secretNamesFor(target).includes(name))) throw new Error(`Slot ${target} secrets already exist; run rotate --retire first`);
 if (!secretNamesFor(active).every(name => names.includes(name))) throw new Error(`The active slot ${active} secrets are not all present`);
 const material = generate();
 try {
  selfTest(material);
  // The active keys stay first; keys older than the active ones are dropped here.
  const next = { metadataPublicKeys: [trust.metadataPublicKeys[0], material.metadata.publicPem], sparklePublicKey: trust.sparklePublicKey, sparkleNextPublicKey: material.sparkle.publicBase64, signing: { active, staged: target } };
  const nextComponent = { ...component, generation: component.generation + 1, ...freshDates(now), publicKeys: [component.publicKeys[0], material.sandbox.publicBase64] };
  validTrust(next); validateConfiguration(nextComponent); assertDedicatedKeys(nextComponent, next);
  if (nextComponent.publicKeys.includes(next.sparkleNextPublicKey)) throw new Error('Sandbox maintenance keys must be separate from release keys');
  await setSecrets(gh, target, material, log);
  await writePublicTrust(root, next, nextComponent);
  log(`Staged slot ${target}. The next release is the transition release: signed by slot ${active}, with a rotation proof by slot ${target}.`);
  log(MAC_ROTATION_WARNING);
  return { fingerprints: report(log, material) };
 } finally { material.zero(); }
}

async function typed(question) {
 const rl = createInterface({ input: process.stdin, output: process.stderr });
 try { return await rl.question(question); } finally { rl.close(); }
}

const USAGE = 'Usage: release-keys.mjs setup --reviewed-sha SHA [--accept-unprotected-main] [--repo owner/name] | rotate --reviewed-sha SHA | rotate --promote | rotate --retire [--repo owner/name] | import-public PUBLIC_JSON';
export function parseArgs(argv) {
 const [command, ...rest] = argv;
 const options = { command, flags: new Set(), values: {} };
 for (let i = 0; i < rest.length; i++) {
  if (['--accept-unprotected-main', '--promote', '--retire'].includes(rest[i])) options.flags.add(rest[i]);
  else if (['--reviewed-sha', '--repo'].includes(rest[i]) && rest[i + 1] !== undefined) options.values[rest[i]] = rest[++i];
  else if (command === 'import-public' && options.file === undefined && !rest[i].startsWith('--')) options.file = rest[i];
  else throw new Error(USAGE);
 }
 return options;
}

if (isMain(import.meta.url)) {
 const { command, flags, values, file } = parseArgs(process.argv.slice(2));
 const repo = values['--repo'] ?? DEFAULT_REPO;
 if (command === 'setup' && !flags.has('--promote') && !flags.has('--retire')) await setupRelease({ repo, reviewedSha: values['--reviewed-sha'], acceptUnprotectedMain: flags.has('--accept-unprotected-main'), gh: createGh({ repo }) });
 else if (command === 'rotate' && !flags.has('--accept-unprotected-main')) await rotateRelease({ repo, promote: flags.has('--promote'), retire: flags.has('--retire'), reviewedSha: values['--reviewed-sha'], gh: createGh({ repo }), confirm: typed });
 else if (command === 'import-public' && file !== undefined && flags.size === 0) {
  const source = JSON.parse(await readFile(file, 'utf8'));
  await writePublicTrust(DESKTOP_ROOT, { ...source, signing: source.signing ?? { active: 'A', staged: null } });
  console.log('Imported public release trust; no private material was read.');
 } else throw new Error(USAGE);
}
