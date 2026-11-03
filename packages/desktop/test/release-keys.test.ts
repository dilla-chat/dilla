import { test, mock } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, writeFile, mkdir, copyFile, rm, readdir } from 'node:fs/promises';
import * as fsPromises from 'node:fs/promises';
import * as fs from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { createPublicKey, createPrivateKey, sign, verify } from 'node:crypto';
// @ts-expect-error build tooling is JavaScript
import { generateKeyMaterial, encodeBase64Buffer, setupRelease, rotateRelease, createGh, secretNamesFor, writePublicTrust, MAC_ROTATION_WARNING } from '../scripts/release-keys.mjs';
// @ts-expect-error build tooling is JavaScript
import { loadKeys } from '../scripts/release-sign-job.mjs';
// @ts-expect-error build tooling is JavaScript
import { validateConfiguration, assertDedicatedKeys, parsePublicConfiguration } from '../scripts/linux-sandbox-package.mjs';
import { fakeGh, goodPosture } from './support/fake-gh.mjs';

const desktop = resolve(import.meta.dirname, '..');
const SHA = 'c'.repeat(40);
const NOW = new Date('2026-11-03T12:00:00.500Z');
type Material = ReturnType<typeof generateKeyMaterial>;

/** A clean git checkout at SHA, as the fake git reports it. */
function fakeGit({ head = SHA, status = '', originTrust }: { head?: string; status?: string; originTrust?: string } = {}) {
 const calls: string[][] = [];
 const git = (args: string[]) => {
  calls.push(args);
  if (args[0] === 'rev-parse') return head;
  if (args[0] === 'status') return status;
  if (args[0] === 'show' && originTrust !== undefined) return originTrust;
  throw new Error('unexpected git ' + args.join(' '));
 };
 return Object.assign(git, { calls });
}
/** A copy of the three public files in a temp root. */
async function publicRoot() {
 const root = await mkdtemp(join(tmpdir(), 'dilla-release-keys-'));
 await mkdir(join(root, 'src'));
 for (const file of ['release-trust.json', 'linux-sandbox-component.json', 'src/update-keys.ts']) await copyFile(join(desktop, file), join(root, file));
 return root;
}
const snapshot = async (root: string) => Object.fromEntries(await Promise.all(['release-trust.json', 'linux-sandbox-component.json', 'src/update-keys.ts'].map(async file => [file, await readFile(join(root, file), 'utf8')])));
/** Wraps generateKeyMaterial to keep copies of what it produced (test-only) and the object to check zeroing. */
function capturing() {
 const seen: { material: Material; copies: Buffer[] }[] = [];
 const generate = () => { const material = generateKeyMaterial(); seen.push({ material, copies: [material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64].map(b => Buffer.from(b)) }); return material; };
 return { generate, seen };
}
async function setupScenario(overrides: Record<string, unknown> = {}, options: Record<string, unknown> = {}) {
 const root = await publicRoot();
 const github = fakeGh(overrides);
 const lines: string[] = [];
 const capture = capturing();
 const run = (extra: Record<string, unknown> = {}) => setupRelease({ reviewedSha: SHA, gh: createGh({ spawn: github.spawn }), git: fakeGit(), now: NOW, root, log: (line: string) => lines.push(line), generate: capture.generate, ...options, ...extra });
 return { root, github, lines, capture, run, cleanup: () => rm(root, { recursive: true, force: true }) };
}
const envForSlot = (slot: string, copies: Buffer[]) => Object.fromEntries(secretNamesFor(slot).map((name: string, i: number) => [name, copies[i].toString()]));

test('generateKeyMaterial produces keys in the formats the sign job reads (single-line base64)', () => {
 const material = generateKeyMaterial();
 try {
  for (const bytes of [material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64]) {
   assert.ok(Buffer.isBuffer(bytes));
   assert.match(bytes.toString(), /^[A-Za-z0-9+/]+=*$/);
  }
  const keys = loadKeys(envForSlot('A', [material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64]), 'A');
  assert.equal(createPublicKey(keys.metadata).export({ type: 'spki', format: 'pem' }), material.metadata.publicPem);
  assert.equal(createPublicKey(keys.sandbox).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'), material.sandbox.publicBase64);
  const sparkle = createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), keys.sparkleSeed]), format: 'der', type: 'pkcs8' });
  assert.equal(createPublicKey(sparkle).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'), material.sparkle.publicBase64);
  keys.zero();
  assert.equal(new Set([material.metadata.publicPem, material.sparkle.publicBase64, material.sandbox.publicBase64]).size, 3);
 } finally { material.zero(); }
});

test('encodeBase64Buffer equals Buffer.toString(\'base64\') and never yields a string', () => {
 for (let length = 0; length < 70; length++) {
  const bytes = Buffer.from(Array.from({ length }, (_, i) => (i * 37 + length) & 255));
  const out = encodeBase64Buffer(bytes);
  assert.ok(Buffer.isBuffer(out));
  assert.equal(out.toString(), bytes.toString('base64'));
 }
 assert.throws(() => encodeBase64Buffer('abc'), /Buffer/);
});

test('zero() clears every private Buffer', () => {
 const material = generateKeyMaterial();
 const buffers = [material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64];
 assert.ok(buffers.every(b => b.some((byte: number) => byte !== 0)));
 material.zero();
 assert.ok(buffers.every(b => b.every((byte: number) => byte === 0)));
});

test('setupRelease sends each key only on stdin', async () => {
 const s = await setupScenario();
 try {
  await s.run();
  const [{ copies, material }] = s.capture.seen;
  const sets = s.github.calls.filter((call: { argv: string[] }) => call.argv[0] === 'secret');
  assert.deepEqual(sets.map((call: { argv: string[] }) => call.argv), secretNamesFor('A').map((name: string) => ['secret', 'set', name, '--env', 'desktop-release', '--repo', 'dilla-chat/dilla']));
  sets.forEach((call: { stdin: Buffer; stdio: string[] }, i: number) => { assert.ok(call.stdin.equals(copies[i])); assert.deepEqual(call.stdio, ['pipe', 'ignore', 'pipe']); });
  for (const call of s.github.calls) {
   const argv = call.argv.join('\0');
   for (const copy of copies) assert.ok(!argv.includes(copy.toString()), 'key bytes in argv');
   if (call.argv[0] === 'api') for (const copy of copies) assert.equal(call.stdin.indexOf(copy), -1, 'key bytes in an API body');
  }
  assert.ok([material.metadata.derBase64, material.sparkle.seedBase64, material.sandbox.derBase64].every((b: Buffer) => b.every(byte => byte === 0)), 'zeroed after setup');
 } finally { await s.cleanup(); }
});

test('setupRelease writes no private key to disk, stdout or stderr', async () => {
 const s = await setupScenario();
 const writes: { path: string; data: Buffer }[] = [];
 const record = (path: unknown, data: unknown) => writes.push({ path: String(path), data: Buffer.from(typeof data === 'string' ? data : (data as Buffer) ?? '') });
 const output: string[] = [];
 const stdout = process.stdout.write.bind(process.stdout), stderr = process.stderr.write.bind(process.stderr);
 const realWriteFile = fsPromises.writeFile;
 mock.method((fsPromises as unknown as { default: typeof fsPromises }).default, 'writeFile', async (path: string, data: string | Buffer, options?: unknown) => { record(path, data); return realWriteFile(path, data, options as never); });
 for (const name of ['writeFileSync', 'appendFileSync'] as const) {
  const real = (fs as unknown as Record<string, (...a: unknown[]) => unknown>)[name];
  mock.method((fs as unknown as { default: typeof fs }).default, name, (path: string, data: unknown, ...rest: unknown[]) => { record(path, data); return real(path, data, ...rest); });
 }
 for (const name of ['appendFile', 'open', 'copyFile', 'rename'] as const) {
  mock.method((fsPromises as unknown as { default: typeof fsPromises }).default, name, (...args: unknown[]) => { record(args[0], ''); throw new Error(`setup must not ${name}`); });
 }
 syncBuiltinESMExports();
 (process.stdout as unknown as { write: unknown }).write = (chunk: unknown, ...rest: unknown[]) => { output.push(String(chunk)); return stdout(chunk as string, ...(rest as [])); };
 (process.stderr as unknown as { write: unknown }).write = (chunk: unknown, ...rest: unknown[]) => { output.push(String(chunk)); return stderr(chunk as string, ...(rest as [])); };
 try {
  await s.run({ log: (line: string) => { s.lines.push(line); console.log(line); } });
 } finally {
  process.stdout.write = stdout; process.stderr.write = stderr;
  mock.restoreAll(); syncBuiltinESMExports();
 }
 try {
  const [{ copies }] = s.capture.seen;
  const forms = copies.flatMap(copy => { const raw = Buffer.from(copy.toString(), 'base64'); return [copy.toString(), raw.toString('hex'), raw.toString('base64url'), raw.subarray(-32).toString('base64'), raw.subarray(-32).toString('hex')]; });
  assert.deepEqual(writes.map(w => w.path.slice(s.root.length + 1)).sort(), ['linux-sandbox-component.json', 'release-trust.json', 'src/update-keys.ts']);
  for (const { path, data } of writes) for (const [i, form] of forms.entries()) assert.ok(!data.toString('latin1').includes(form), `${path} holds key form ${i}`);
  const printed = output.join('') + s.lines.join('\n');
  for (const form of forms) assert.ok(!printed.includes(form), 'a key form was printed');
  assert.match(printed, /No private key was written to disk/);
 } finally { await s.cleanup(); }
});

test('setupRelease refuses a HEAD that differs from --reviewed-sha or a dirty tree', async () => {
 for (const [git, sha] of [[fakeGit({ head: 'd'.repeat(40) }), SHA], [fakeGit({ status: '?? notes.txt' }), SHA], [fakeGit(), undefined], [fakeGit(), 'HEAD']] as const) {
  const s = await setupScenario();
  try {
   await assert.rejects(s.run({ git, reviewedSha: sha }), /reviewed|clean/i);
   assert.equal(s.github.calls.length, 0, 'nothing is asked of GitHub');
   assert.equal(s.capture.seen.length, 0);
  } finally { await s.cleanup(); }
 }
});

test('setupRelease aborts and prints the missing rule when pull_request, ci-ok(15368), non_fast_forward or deletion is absent', async () => {
 const base = goodPosture().rules;
 const variants: [string, unknown[]][] = [
  ['pull_request', base.filter((r: { type: string }) => r.type !== 'pull_request')],
  ['ci-ok', base.map((r: { type: string }) => r.type === 'required_status_checks' ? { ...r, parameters: { required_status_checks: [{ context: 'ci-ok', integration_id: 1 }] } } : r)],
  ['non_fast_forward', base.filter((r: { type: string }) => r.type !== 'non_fast_forward')],
  ['deletion', base.filter((r: { type: string }) => r.type !== 'deletion')],
 ];
 for (const [missing, rules] of variants) {
  const s = await setupScenario({ rules });
  try {
   await assert.rejects(s.run(), new RegExp(missing));
   assert.ok(s.lines.some(line => line.includes(missing)), `prints ${missing}`);
   assert.equal(s.capture.seen.length, 0);
   assert.equal(s.github.state.environment, null, 'nothing was changed');
  } finally { await s.cleanup(); }
 }
 const inactive = await setupScenario({ rulesets: { 25709839: { id: 25709839, name: 'main', enforcement: 'evaluate', bypass_actors: [] } } });
 try { await assert.rejects(inactive.run(), /enforcement active/); } finally { await inactive.cleanup(); }
 const accepted = await setupScenario({ rules: variants[0][1] });
 try { await accepted.run({ acceptUnprotectedMain: true }); assert.ok(accepted.lines.some(line => /--accept-unprotected-main/.test(line))); } finally { await accepted.cleanup(); }
});

test('setupRelease aborts when a repository-level or organisation DESKTOP_ secret exists', async () => {
 for (const overrides of [{ repoSecrets: ['CSC_LINK', 'DESKTOP_METADATA_KEY_A'] }, { orgSecrets: ['DESKTOP_SPARKLE_KEY'] }, { orgSecrets: null }]) {
  const s = await setupScenario(overrides);
  try {
   await assert.rejects(s.run(), /DESKTOP_|organisation/i);
   assert.equal(s.capture.seen.length, 0);
   assert.equal(s.github.state.environment, null);
  } finally { await s.cleanup(); }
 }
});

test('setupRelease aborts when immutable releases are disabled', async () => {
 for (const immutable of [null, { enabled: false }]) {
  const s = await setupScenario({ immutable });
  try { await assert.rejects(s.run(), /Immutable releases/); assert.equal(s.capture.seen.length, 0); } finally { await s.cleanup(); }
 }
});

test('setupRelease aborts unless the admin, workflow-token and Pages posture hold', async () => {
 const variants = [
  { repo: { permissions: { admin: false } } },
  { workflow: { default_workflow_permissions: 'write', can_approve_pull_request_reviews: false } },
  { workflow: { default_workflow_permissions: 'read', can_approve_pull_request_reviews: true } },
  { pages: null }, { pages: { build_type: 'legacy' } },
  { pagesPolicies: [{ id: 1, name: 'main', type: 'branch' }, { id: 2, name: 'desktop-v*', type: 'tag' }] },
  { pagesEnvironment: { deployment_branch_policy: { protected_branches: true, custom_branch_policies: false } } },
 ];
 for (const overrides of variants) {
  const s = await setupScenario(overrides);
  try { await assert.rejects(s.run(), /admin|workflow permissions|Pages|github-pages/i, JSON.stringify(overrides)); assert.equal(s.capture.seen.length, 0); } finally { await s.cleanup(); }
 }
});

test('setupRelease prints collaborators, Apps and the bypass list', async () => {
 const s = await setupScenario();
 try {
  await s.run();
  const text = s.lines.join('\n');
  assert.match(text, /jonasthim \(admin\)/);
  assert.match(text, /claude \(all\)/);
  assert.match(text, /"bypass_mode":"always"/);
  assert.match(text, /any credential that can merge to main/i);
 } finally { await s.cleanup(); }
});

test('setupRelease leaves exactly one deployment policy, branch main, no tag policy', async () => {
 const s = await setupScenario({ environment: { name: 'desktop-release', can_admins_bypass: true, deployment_branch_policy: { protected_branches: true, custom_branch_policies: false } }, policies: [{ id: 1, name: 'desktop-v*', type: 'tag' }, { id: 2, name: 'dev', type: 'branch' }] });
 try {
  await s.run();
  assert.deepEqual(s.github.state.policies.map((p: { name: string; type: string }) => ({ name: p.name, type: p.type })), [{ name: 'main', type: 'branch' }]);
  assert.deepEqual(s.github.state.environment.deployment_branch_policy, { protected_branches: false, custom_branch_policies: true });
  assert.equal(s.github.state.environment.can_admins_bypass, false);
 } finally { await s.cleanup(); }
});

test('setupRelease fails if the policy read-back differs', async () => {
 const s = await setupScenario({ environment: { name: 'desktop-release' }, policies: [{ id: 1, name: 'desktop-v*', type: 'tag' }], ignorePolicyDelete: true });
 const before = await snapshot(s.root);
 try {
  await assert.rejects(s.run(), /exactly the branch main/);
  assert.equal(s.capture.seen.length, 0);
  assert.equal(s.github.state.envSecrets.size, 0);
  assert.deepEqual(await snapshot(s.root), before);
 } finally { await s.cleanup(); }
});

test('setupRelease refuses when any DESKTOP_ environment secret already exists and generates nothing', async () => {
 const s = await setupScenario({ envSecrets: { DESKTOP_SANDBOX_KEY_B: Buffer.from('x') } });
 try {
  await assert.rejects(s.run(), /already holds DESKTOP_SANDBOX_KEY_B/);
  assert.equal(s.capture.seen.length, 0);
  assert.ok(!s.github.calls.some((call: { argv: string[] }) => call.argv[0] === 'secret'));
 } finally { await s.cleanup(); }
});

test('setupRelease leaves repository files untouched when a secret write fails', async () => {
 const s = await setupScenario({ failSecretSet: 'DESKTOP_SPARKLE_KEY_A' });
 const before = await snapshot(s.root);
 try {
  await assert.rejects(s.run(), /failed/);
  assert.deepEqual(await snapshot(s.root), before);
  assert.ok(s.lines.some(line => /after: DESKTOP_METADATA_KEY_A\./.test(line)), s.lines.join('\n'));
  assert.ok(s.capture.seen[0].material.sparkle.seedBase64.every((byte: number) => byte === 0), 'zeroed after a failure');
 } finally { await s.cleanup(); }
});

test('setupRelease public output satisfies the verifier\'s pins', async () => {
 const s = await setupScenario();
 try {
  await s.run();
  const trust = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  const component = parsePublicConfiguration(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  assert.deepEqual(trust.signing, { active: 'A', staged: null });
  assert.equal(trust.sparkleNextPublicKey, undefined);
  validateConfiguration(component); assertDedicatedKeys(component, trust);
  assert.equal(component.publishedAt, '2026-11-03T12:00:00Z');
  assert.equal(component.expiresAt, '2027-02-01T12:00:00Z');
  const keys = loadKeys(envForSlot('A', s.capture.seen[0].copies), 'A');
  assert.deepEqual(trust.metadataPublicKeys, [createPublicKey(keys.metadata).export({ type: 'spki', format: 'pem' })]);
  assert.deepEqual(component.publicKeys, [createPublicKey(keys.sandbox).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64')]);
  const sparkle = createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), keys.sparkleSeed]), format: 'der', type: 'pkcs8' });
  assert.equal(createPublicKey(sparkle).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64'), trust.sparklePublicKey);
  const probe = Buffer.from('probe');
  assert.ok(verify(null, probe, trust.metadataPublicKeys[0], sign(null, probe, keys.metadata)));
  keys.zero();
  const updateKeys = await readFile(join(s.root, 'src/update-keys.ts'), 'utf8');
  assert.ok(updateKeys.includes(JSON.stringify(trust.metadataPublicKeys, null, 2)));
 } finally { await s.cleanup(); }
});

test('release pins agree', async () => {
 const trust = JSON.parse(await readFile(join(desktop, 'release-trust.json'), 'utf8'));
 const component = parsePublicConfiguration(await readFile(join(desktop, 'linux-sandbox-component.json'), 'utf8'));
 const { RELEASE_PUBLIC_KEYS } = await import('../src/update-keys.ts');
 assert.deepEqual([...RELEASE_PUBLIC_KEYS], trust.metadataPublicKeys);
 assert.ok(['A', 'B'].includes(trust.signing?.active));
 assert.ok(trust.signing.staged === null ? trust.sparkleNextPublicKey === undefined : typeof trust.sparkleNextPublicKey === 'string');
 validateConfiguration(component); assertDedicatedKeys(component, trust);
 const electron = await readFile(join(desktop, 'electron-builder.cjs'), 'utf8');
 assert.match(electron, /require\('\.\/release-trust\.json'\)/);
});

test('import-public still writes trust and update-keys', async () => {
 const root = await publicRoot();
 try {
  const keys = [0, 1].map(() => createPublicKey(createPrivateKey({ key: Buffer.concat([Buffer.from('302e020100300506032b657004220420', 'hex'), Buffer.alloc(32, Math.random() * 255 | 0)]), format: 'der', type: 'pkcs8' })));
  const source = { metadataPublicKeys: [keys[0].export({ type: 'spki', format: 'pem' })], sparklePublicKey: keys[1].export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') };
  await writeFile(join(root, 'public.json'), JSON.stringify(source));
  const module = { writePublicTrust: writePublicTrust as (root: string, trust: unknown) => Promise<void> };
  await module.writePublicTrust(root, { ...source, signing: { active: 'A', staged: null } });
  assert.deepEqual(JSON.parse(await readFile(join(root, 'release-trust.json'), 'utf8')), { ...source, signing: { active: 'A', staged: null } });
  assert.ok((await readFile(join(root, 'src/update-keys.ts'), 'utf8')).includes(JSON.stringify(source.metadataPublicKeys, null, 2)));
  const cli = spawnSync(process.execPath, [join(desktop, 'scripts/release-keys.mjs'), 'import-public'], { encoding: 'utf8' });
  assert.notEqual(cli.status, 0, 'usage');
  await assert.rejects(module.writePublicTrust(root, { ...source, sparklePublicKey: 'short' }), /Invalid release public keys/);
 } finally { await rm(root, { recursive: true, force: true }); }
});

// --- Rotation -------------------------------------------------------------------------
async function setUp() {
 const s = await setupScenario();
 await s.run();
 return s;
}
const rotate = (s: Awaited<ReturnType<typeof setupScenario>>, extra: Record<string, unknown> = {}) =>
 rotateRelease({ reviewedSha: SHA, gh: createGh({ spawn: s.github.spawn }), git: fakeGit(), now: NOW, root: s.root, log: (line: string) => s.lines.push(line), generate: s.capture.generate, ...extra });

test('rotate writes only the inactive slot and leaves the active slot\'s secrets untouched', async () => {
 const s = await setUp();
 try {
  const activeBefore = secretNamesFor('A').map((name: string) => Buffer.from(s.github.state.envSecrets.get(name)));
  await rotate(s);
  assert.deepEqual([...s.github.state.envSecrets.keys()].sort(), [...secretNamesFor('A'), ...secretNamesFor('B')].sort());
  secretNamesFor('A').forEach((name: string, i: number) => assert.ok(s.github.state.envSecrets.get(name).equals(activeBefore[i]), name));
  const sets = s.github.calls.filter((call: { argv: string[] }) => call.argv[0] === 'secret').slice(3);
  assert.deepEqual(sets.map((call: { argv: string[] }) => call.argv[2]), secretNamesFor('B'));
  assert.ok(s.lines.includes(MAC_ROTATION_WARNING) || s.lines.some(line => line.includes(MAC_ROTATION_WARNING)), 'rotate prints the macOS warning');
 } finally { await s.cleanup(); }
});

test('rotate adds the staged keys to metadataPublicKeys and the sandbox publicKeys, sets sparkleNextPublicKey and bumps the sandbox generation', async () => {
 const s = await setUp();
 try {
  const before = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  const componentBefore = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  await rotate(s);
  const after = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  const component = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  const staged = loadKeys(envForSlot('B', s.capture.seen[1].copies), 'B');
  assert.deepEqual(after.metadataPublicKeys, [before.metadataPublicKeys[0], createPublicKey(staged.metadata).export({ type: 'spki', format: 'pem' })]);
  assert.equal(after.sparklePublicKey, before.sparklePublicKey);
  assert.equal(after.sparkleNextPublicKey, s.capture.seen[1].material.sparkle.publicBase64);
  assert.deepEqual(after.signing, { active: 'A', staged: 'B' });
  assert.deepEqual(component.publicKeys, [componentBefore.publicKeys[0], createPublicKey(staged.sandbox).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64')]);
  assert.equal(component.generation, componentBefore.generation + 1);
  staged.zero();
  validateConfiguration(component); assertDedicatedKeys(component, after);
 } finally { await s.cleanup(); }
});

test('rotate refuses while a rotation is already staged', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const calls = s.github.calls.length;
  await assert.rejects(rotate(s), /already staged/);
  assert.equal(s.github.calls.length, calls);
  assert.equal(s.capture.seen.length, 2);
 } finally { await s.cleanup(); }
});

test('rotate --promote flips signing.active, clears staged and sparkleNextPublicKey, records the pending macOS key change and requires the typed confirmation', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const staged = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  const stagedComponent = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  const before = await snapshot(s.root);
  for (const confirm of [undefined, async () => 'yes', async () => 'promote A']) {
   await assert.rejects(rotate(s, { promote: true, confirm }), /not confirmed/);
   assert.deepEqual(await snapshot(s.root), before);
  }
  const calls = s.github.calls.length;
  await rotate(s, { promote: true, confirm: async () => 'promote B' });
  assert.equal(s.github.calls.length, calls, 'promote changes files only');
  const after = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  // The installed Macs trust slot A's Sparkle key: each channel's next release is its DMG rotation release.
  assert.deepEqual(after.signing, { active: 'B', staged: null, macRotation: { fromSlot: 'A', fromSparklePublicKey: staged.sparklePublicKey } });
  assert.equal(after.sparklePublicKey, staged.sparkleNextPublicKey);
  assert.equal(after.sparkleNextPublicKey, undefined);
  assert.deepEqual(after.metadataPublicKeys, [staged.metadataPublicKeys[1], staged.metadataPublicKeys[0]], 'the old key stays one more release');
  const component = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  assert.deepEqual(component.publicKeys, [stagedComponent.publicKeys[1], stagedComponent.publicKeys[0]]);
  // No new rotation while the macOS key change is pending; retire clears it, and the next rotate drops the old key.
  await assert.rejects(rotate(s), /macOS key change is still pending/);
  await rotate(s, { retire: true, git: fakeGit({ originTrust: JSON.stringify(after) }), confirm: async () => 'retire A' });
  const retired = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  assert.deepEqual(retired, { ...after, signing: { active: 'B', staged: null } }, 'retire clears only the pending macOS key change');
  await rotate(s);
  const next = JSON.parse(await readFile(join(s.root, 'release-trust.json'), 'utf8'));
  assert.equal(next.metadataPublicKeys.length, 2);
  assert.equal(next.metadataPublicKeys[0], after.metadataPublicKeys[0]);
  assert.ok(!next.metadataPublicKeys.includes(staged.metadataPublicKeys[0]));
  assert.deepEqual(next.signing, { active: 'B', staged: 'A' });
 } finally { await s.cleanup(); }
});

test('rotate --promote refuses when nothing is staged', async () => {
 const s = await setUp();
 try { await assert.rejects(rotate(s, { promote: true, confirm: async () => 'promote B' }), /Nothing is staged/); } finally { await s.cleanup(); }
});

test('rotate --retire deletes exactly the old slot\'s three secrets and only after the promote state is committed', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const stagedTrust = await readFile(join(s.root, 'release-trust.json'), 'utf8');
  await assert.rejects(rotate(s, { retire: true, git: fakeGit({ originTrust: stagedTrust }), confirm: async () => 'retire A' }), /promote it before retiring/);
  await rotate(s, { promote: true, confirm: async () => 'promote B' });
  const promoted = await readFile(join(s.root, 'release-trust.json'), 'utf8');
  await assert.rejects(rotate(s, { retire: true, git: fakeGit({ originTrust: stagedTrust }), confirm: async () => 'retire A' }), /merge the promote PR first/);
  await assert.rejects(rotate(s, { retire: true, git: fakeGit({ originTrust: promoted }), confirm: async () => 'retire B' }), /not confirmed/);
  assert.equal(s.github.state.envSecrets.size, 6);
  await rotate(s, { retire: true, git: fakeGit({ originTrust: promoted }), confirm: async () => 'retire A' });
  assert.deepEqual([...s.github.state.envSecrets.keys()].sort(), [...secretNamesFor('B')].sort());
  const deletes = s.github.calls.filter((call: { argv: string[] }) => call.argv[1] === 'delete').map((call: { argv: string[] }) => call.argv);
  assert.deepEqual(deletes, secretNamesFor('A').map((name: string) => ['secret', 'delete', name, '--env', 'desktop-release', '--repo', 'dilla-chat/dilla']));
 } finally { await s.cleanup(); }
});

// Final review, m5: promote comes at least 60 days after rotate, so rotate's dates would leave under 30 days for the gate.
test('rotate --promote refreshes the sandbox component dates and bumps its generation', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const stagedComponent = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  const later = new Date(NOW.getTime() + 70 * 86400000);
  await rotate(s, { promote: true, confirm: async () => 'promote B', now: later });
  const component = JSON.parse(await readFile(join(s.root, 'linux-sandbox-component.json'), 'utf8'));
  assert.equal(component.generation, stagedComponent.generation + 1);
  assert.equal(component.publishedAt, '2027-01-12T12:00:00Z');
  assert.equal(component.expiresAt, '2027-04-12T12:00:00Z');
  validateConfiguration(component);
 } finally { await s.cleanup(); }
});

// Final review, m6: retiring before a channel's DMG rotation release strands its Macs on the old key.
test('rotate --retire refuses until every channel with a published release has its rotation release out', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const stagedText = await readFile(join(s.root, 'release-trust.json'), 'utf8');
  await rotate(s, { promote: true, confirm: async () => 'promote B' });
  const promoted = await readFile(join(s.root, 'release-trust.json'), 'utf8');
  const STABLE = 'a'.repeat(40), BETA = 'b'.repeat(40);
  const bot = { login: 'github-actions[bot]', type: 'Bot' };
  s.github.state.releases = [
   { tag_name: 'desktop-v1.0.0', draft: false, prerelease: false, immutable: true, author: bot },
   { tag_name: 'desktop-v1.1.0-beta.1', draft: false, prerelease: true, immutable: true, author: bot },
  ];
  s.github.state.tags = { 'desktop-v1.0.0': STABLE, 'desktop-v1.1.0-beta.1': BETA };
  /** origin/main holds the promote; each tagged commit holds the release-trust.json it was built with. */
  const releaseGit = (builtAt: Record<string, string>) => (args: string[]) => {
   if (args[0] === 'merge-base') return '';
   if (args[0] === 'show' && args[1] === 'origin/main:packages/desktop/release-trust.json') return promoted;
   const at = args[0] === 'show' ? /^([a-f0-9]{40}):packages\/desktop\/release-trust\.json$/.exec(args[1]) : null;
   if (at && builtAt[at[1]] !== undefined) return builtAt[at[1]];
   throw new Error('unexpected git ' + args.join(' '));
  };
  const before = await snapshot(s.root);
  // beta has its rotation release out (built with main's new Sparkle key); stable's newest is still built for the old key.
  await assert.rejects(rotate(s, { retire: true, git: releaseGit({ [STABLE]: stagedText, [BETA]: promoted }), confirm: async () => 'retire A' }), /stable rotation release/);
  // A channel whose newest release is not workflow-made cannot be confirmed either.
  s.github.state.releases[0].author = { login: 'octocat', type: 'User' };
  await assert.rejects(rotate(s, { retire: true, git: releaseGit({ [STABLE]: promoted, [BETA]: promoted }), confirm: async () => 'retire A' }), /Cannot confirm the stable rotation release/);
  s.github.state.releases[0].author = bot;
  assert.equal(s.github.state.envSecrets.size, 6, 'no secret is deleted');
  assert.deepEqual(await snapshot(s.root), before, 'no file is written');
  await rotate(s, { retire: true, git: releaseGit({ [STABLE]: promoted, [BETA]: promoted }), confirm: async () => 'retire A' });
  assert.deepEqual([...s.github.state.envSecrets.keys()].sort(), [...secretNamesFor('B')].sort());
 } finally { await s.cleanup(); }
});

test('the real gh adapter never puts a secret in argv and sends it with stdout ignored', async () => {
 const calls: { argv: string[]; stdio: unknown; stdin: Buffer[] }[] = [];
 const { EventEmitter } = await import('node:events');
 const { Writable, PassThrough } = await import('node:stream');
 const spawn = (_command: string, argv: string[], options: { stdio: unknown }) => {
  const call = { argv, stdio: options.stdio, stdin: [] as Buffer[] }; calls.push(call);
  const child = Object.assign(new EventEmitter(), { stdout: new PassThrough(), stderr: new PassThrough(), stdin: new Writable({ write(chunk, _e, done) { call.stdin.push(Buffer.from(chunk)); done(); }, final(done) { setImmediate(() => { child.stdout.end(); child.stderr.end(); child.emit('close', 0); }); done(); } }) });
  return child;
 };
 const gh = createGh({ spawn });
 const secret = Buffer.from('c2VjcmV0LWJ5dGVzLW5vdC1hLXJlYWwta2V5');
 await gh.secretSet('DESKTOP_METADATA_KEY_B', secret);
 assert.deepEqual(calls[0].argv, ['secret', 'set', 'DESKTOP_METADATA_KEY_B', '--env', 'desktop-release', '--repo', 'dilla-chat/dilla']);
 assert.deepEqual(calls[0].stdio, ['pipe', 'ignore', 'pipe']);
 assert.ok(Buffer.concat(calls[0].stdin).equals(secret));
 await assert.rejects(gh.secretSet('OTHER', secret), /Invalid secret/);
 await assert.rejects(gh.secretSet('DESKTOP_METADATA_KEY_A', 'string' as unknown as Buffer), /Invalid secret/);
});

test('no file under the temp roots holds a private key after setup and rotation', async () => {
 const s = await setUp();
 try {
  await rotate(s);
  const forms = s.capture.seen.flatMap(seen => seen.copies.map(copy => copy.toString()));
  for (const entry of await readdir(s.root, { recursive: true, withFileTypes: true })) {
   if (!entry.isFile()) continue;
   const text = await readFile(join(entry.parentPath, entry.name), 'utf8');
   for (const form of forms) assert.ok(!text.includes(form), entry.name);
  }
 } finally { await s.cleanup(); }
});
