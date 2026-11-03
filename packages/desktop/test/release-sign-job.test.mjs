import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createPublicKey, verify, createHash } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm, copyFile, lstat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { spawnSync } from 'node:child_process';
import { loadKeys, signInJob, KEY_VARIABLES } from '../scripts/release-sign-job.mjs';
import { writeDigest, verifyDigest } from '../scripts/candidate-digest.mjs';
import { buildReleaseFixture, isolatedReleaseTools, currentSandboxDates } from './support/release-fixture.mjs';

const RUN_ENV = Object.freeze({ GITHUB_ACTIONS: 'true', GITHUB_REF: 'refs/heads/main', GITHUB_EVENT_NAME: 'workflow_run', MAC_UPDATE: 'zip' });
const der = key => key.export({ type: 'pkcs8', format: 'der' });
const seedOf = key => der(key).subarray(-32);
const rawPublic = key => createPublicKey(key).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
const pem = key => createPublicKey(key).export({ type: 'spki', format: 'pem' }).toString();

/** The three private keys of one slot as the environment carries them. */
function slotEnv(slot, keys) {
 return { [`DESKTOP_METADATA_KEY_${slot}`]: der(keys.metadata).toString('base64'), [`DESKTOP_SPARKLE_KEY_${slot}`]: seedOf(keys.sparkle).toString('base64'), [`DESKTOP_SANDBOX_KEY_${slot}`]: der(keys.sandbox).toString('base64') };
}
const freshKeys = () => ({ metadata: generateKeyPairSync('ed25519').privateKey, sparkle: generateKeyPairSync('ed25519').privateKey, sandbox: generateKeyPairSync('ed25519').privateKey });
/** Every form of a private key that must never be written or printed. */
function secretForms(keys) {
 const forms = [];
 for (const key of [keys.metadata, keys.sandbox]) {
  const bytes = der(key); forms.push(bytes.toString('base64'), bytes.toString('hex'), bytes.toString('base64url'), seedOf(key).toString('base64'), seedOf(key).toString('base64url'), seedOf(key).toString('hex'));
  forms.push(...key.export({ type: 'pkcs8', format: 'pem' }).toString().split('\n').filter(line => line && !line.startsWith('-----')));
 }
 const seed = seedOf(keys.sparkle); forms.push(seed.toString('base64'), seed.toString('base64url'), seed.toString('hex'), keys.sparkle.export({ format: 'jwk' }).d);
 return forms;
}
const secretBuffers = keys => [der(keys.metadata), der(keys.sandbox), seedOf(keys.metadata), seedOf(keys.sandbox), seedOf(keys.sparkle)];

/** A release fixture whose candidate is ready to sign, with isolated tools pinned to slot A and, if staged, slot B. */
async function signingSetup({ staged = false } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-sign-job-'));
 const candidate = join(root, 'candidate'); await mkdir(candidate);
 const fixture = await buildReleaseFixture({ directory: candidate, sandboxDates: currentSandboxDates() });
 await writeFile(join(candidate, 'acceptance.json'), JSON.stringify(fixture.source));
 await writeFile(join(candidate, 'ci-run.json'), JSON.stringify(fixture.run));
 await writeFile(join(candidate, 'ci-jobs.json'), JSON.stringify(fixture.jobs));
 const active = { metadata: fixture.key.privateKey, sparkle: fixture.sparkle.privateKey, sandbox: fixture.sandbox.pair.privateKey };
 const next = staged ? freshKeys() : null;
 const trust = { metadataPublicKeys: [pem(active.metadata), ...(next ? [pem(next.metadata)] : [])], sparklePublicKey: rawPublic(active.sparkle), ...(next ? { sparkleNextPublicKey: rawPublic(next.sparkle) } : {}), signing: { active: 'A', staged: next ? 'B' : null } };
 const sandboxConfig = { ...fixture.sandbox.config, publicKeys: [rawPublic(active.sandbox), ...(next ? [rawPublic(next.sandbox)] : [])] };
 const tools = join(root, 'tools');
 const { verifier, signer } = await isolatedReleaseTools(tools, { trust, sandboxConfig, files: ['scripts/release-sign-job.mjs'] });
 const job = await import(pathToFileURL(join(tools, 'scripts/release-sign-job.mjs')).href);
 const env = { ...RUN_ENV, ...slotEnv('A', active), ...(next ? slotEnv('B', next) : {}) };
 return { root, candidate, output: join(root, 'signed'), fixture, active, next, trust, sandboxConfig, tools, verifier, signer, job, env, cleanup: () => rm(root, { recursive: true, force: true }) };
}
async function filesUnder(directory) {
 const found = [];
 for (const entry of await readdir(directory, { withFileTypes: true, recursive: true })) if (entry.isFile()) found.push(join(entry.parentPath, entry.name));
 return found;
}
function spy(check = () => {}) {
 const calls = [];
 const sign = async options => { calls.push(options); await check(options); return { version: options.version }; };
 sign.calls = calls;
 return sign;
}

test('signInJob refuses to run outside the main-branch workflow_run', async () => {
 const keys = freshKeys();
 for (const overrides of [{ GITHUB_ACTIONS: 'false' }, { GITHUB_REF: 'refs/tags/desktop-v1.2.3' }, { GITHUB_REF: 'refs/heads/feature' }, { GITHUB_EVENT_NAME: 'push' }, { GITHUB_EVENT_NAME: 'pull_request_target' }]) {
  const sign = spy();
  await assert.rejects(signInJob({ env: { ...RUN_ENV, ...slotEnv('A', keys), ...overrides }, version: '1.2.3', candidateDirectory: '/x', outputDirectory: '/y', trust: { signing: { active: 'A', staged: null } }, sign }), /main-branch workflow_run/);
  assert.equal(sign.calls.length, 0);
 }
});

test('signInJob refuses a missing, multi-line or wrong-length key and signs nothing', async () => {
 const keys = freshKeys(), good = slotEnv('A', keys);
 const variants = [];
 for (const name of Object.keys(good)) {
  variants.push({ [name]: undefined }, { [name]: '' }, { [name]: good[name] + '\n' }, { [name]: good[name].slice(0, 32) + '\n' + good[name].slice(32) }, { [name]: good[name].slice(0, -4) },
   { [name]: good[name] + 'AAAA' }, { [name]: good[name].replace(/[A-Za-z]/, '-') }, { [name]: ` ${good[name]}` });
 }
 // Right shape, wrong content: a non-PKCS8 DER and an RSA-free but non-Ed25519 curve are refused too.
 variants.push({ DESKTOP_METADATA_KEY_A: Buffer.alloc(48, 7).toString('base64') });
 variants.push({ DESKTOP_SANDBOX_KEY_A: generateKeyPairSync('x25519').privateKey.export({ type: 'pkcs8', format: 'der' }).toString('base64') });
 for (const change of variants) {
  const env = { ...RUN_ENV, ...good, ...change };
  for (const [key, value] of Object.entries(change)) if (value === undefined) delete env[key];
  const sign = spy();
  await assert.rejects(signInJob({ env, version: '1.2.3', candidateDirectory: '/x', outputDirectory: '/y', trust: { signing: { active: 'A', staged: null } }, sign }), /DESKTOP_(METADATA|SPARKLE|SANDBOX)_KEY_A/, JSON.stringify(Object.keys(change)));
  assert.equal(sign.calls.length, 0);
  for (const name of KEY_VARIABLES) assert.ok(!(name in env), 'key variables are removed even when loading fails');
 }
 for (const trust of [{}, { signing: { active: 'C', staged: null } }, { signing: { active: 'A', staged: 'A' } }, { signing: { active: 'A' } }]) {
  await assert.rejects(signInJob({ env: { ...RUN_ENV, ...good }, version: '1.2.3', candidateDirectory: '/x', outputDirectory: '/y', trust, sign: spy() }), /slot/i);
 }
});

test('signInJob signs with the active slot only when no slot is staged', async () => {
 for (const active of ['A', 'B']) {
  const keys = freshKeys(), other = active === 'A' ? 'B' : 'A';
  const env = { ...RUN_ENV, ...slotEnv(active, keys), [`DESKTOP_METADATA_KEY_${other}`]: 'not even base64!' };
  const sign = spy();
  const result = await signInJob({ env, version: '1.2.3', candidateDirectory: '/candidate', outputDirectory: '/signed', trust: { signing: { active, staged: null } }, sign });
  assert.deepEqual(result, { staged: false });
  assert.equal(sign.calls.length, 1);
  const [options] = sign.calls;
  assert.equal(options.keysDirectory, undefined);
  assert.deepEqual(Object.keys(options.keys).sort(), ['metadata', 'sandbox', 'sparkleSeed']);
  assert.equal(pem(options.keys.metadata), pem(keys.metadata));
  assert.equal(rawPublic(options.keys.sandbox), rawPublic(keys.sandbox));
  assert.deepEqual([options.version, options.artifactsDirectory, options.outputDirectory], ['1.2.3', '/candidate', '/signed']);
 }
});

test('signInJob with a staged slot signs with the active keys and writes a rotation proof that verifies under the staged public keys', async () => {
 const s = await signingSetup({ staged: true });
 try {
  const result = await s.job.signInJob({ env: { ...s.env }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust });
  assert.deepEqual(result, { staged: true });
  const channel = s.fixture.channel, metadata = await readFile(join(s.output, `desktop-${channel}.json`));
  const signature = (await readFile(join(s.output, `desktop-${channel}.json.sig`), 'utf8')).trim();
  assert.ok(verify(null, metadata, createPublicKey(s.active.metadata), Buffer.from(signature, 'base64')), 'the release is signed by the active key');
  assert.ok(!verify(null, metadata, createPublicKey(s.next.metadata), Buffer.from(signature, 'base64')));
  const proof = JSON.parse(await readFile(join(s.output, 'rotation-proof.json'), 'utf8'));
  assert.equal(proof.stagedSlot, 'B');
  const manifest = await readFile(join(s.candidate, `dilla-${s.fixture.version}-linux-x64.sandbox.json`));
  const archive = await readFile(join(s.candidate, `dilla-${s.fixture.version}-mac-arm64.zip`));
  const rawKey = raw => createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(raw, 'base64').toString('base64url') }, format: 'jwk' });
  assert.equal(proof.metadata.publicKey, pem(s.next.metadata));
  assert.ok(s.trust.metadataPublicKeys.includes(proof.metadata.publicKey));
  assert.ok(verify(null, metadata, createPublicKey(s.trust.metadataPublicKeys[1]), Buffer.from(proof.metadata.signature, 'base64')));
  assert.equal(proof.metadata.sha256, createHash('sha256').update(metadata).digest('hex'));
  assert.ok(verify(null, Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'), manifest]), rawKey(s.sandboxConfig.publicKeys[1]), Buffer.from(proof.sandbox.signature, 'base64')));
  assert.ok(verify(null, archive, rawKey(s.trust.sparkleNextPublicKey), Buffer.from(proof.sparkle.signature, 'base64')));
  assert.ok(!verify(null, archive, rawKey(s.trust.sparklePublicKey), Buffer.from(proof.sparkle.signature, 'base64')));
  const digest = await writeDigest(s.output, { channel, version: s.fixture.version, staged: true });
  await verifyDigest(s.output, digest, { channel, version: s.fixture.version, staged: true });
 } finally { await s.cleanup(); }
});

test('signInJob refuses a staged slot whose public key is not in the pins', async () => {
 const variants = [
  s => { s.trust.metadataPublicKeys = [s.trust.metadataPublicKeys[0]]; },
  s => { delete s.trust.sparkleNextPublicKey; },
  s => { s.trust.sparkleNextPublicKey = rawPublic(generateKeyPairSync('ed25519').privateKey); },
  s => { s.env = { ...s.env, ...slotEnv('B', { ...s.next, sandbox: generateKeyPairSync('ed25519').privateKey }) }; },
  s => { s.env = { ...s.env, ...slotEnv('B', s.active) }; },
  // The staged Sparkle key must be dedicated: not the staged metadata or sandbox key, nor any pinned one.
  s => { s.next.sparkle = s.next.metadata; s.trust.sparkleNextPublicKey = rawPublic(s.next.metadata); s.env = { ...s.env, ...slotEnv('B', s.next) }; },
  s => { s.next.sparkle = s.next.sandbox; s.trust.sparkleNextPublicKey = rawPublic(s.next.sandbox); s.env = { ...s.env, ...slotEnv('B', s.next) }; },
  s => { s.next.sparkle = s.active.metadata; s.trust.sparkleNextPublicKey = rawPublic(s.active.metadata); s.env = { ...s.env, ...slotEnv('B', s.next) }; },
 ];
 for (const [index, change] of variants.entries()) {
  const s = await signingSetup({ staged: true });
  try {
   change(s);
   const sign = spy();
   await assert.rejects(s.job.signInJob({ env: { ...s.env }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust, sign }), /staged .* pins/i, String(index));
   assert.equal(sign.calls.length, 0, 'nothing is signed');
  } finally { await s.cleanup(); }
 }
});

test('no key file is ever created', async () => {
 const s = await signingSetup({ staged: true });
 // The signing runs in a child whose TMPDIR and HOME are fresh, so a write to os.tmpdir() or $HOME is seen too.
 const scratch = await mkdtemp(join(tmpdir(), 'dilla-sign-job-scratch-'));
 try {
  const tmp = join(scratch, 'tmp'), home = join(scratch, 'home'); await mkdir(tmp); await mkdir(home);
  await writeFile(join(s.root, 'trust-for-child.json'), JSON.stringify(s.trust));
  const runner = join(s.root, 'run-sign.mjs');
  await writeFile(runner, `const job = await import(${JSON.stringify(pathToFileURL(join(s.tools, 'scripts/release-sign-job.mjs')).href)});
const { readFile } = await import('node:fs/promises');
const trust = JSON.parse(await readFile(${JSON.stringify(join(s.root, 'trust-for-child.json'))}, 'utf8'));
await job.signInJob({ env: process.env, version: ${JSON.stringify(s.fixture.version)}, candidateDirectory: ${JSON.stringify(s.candidate)}, outputDirectory: ${JSON.stringify(s.output)}, trust });
`);
  const before = new Set(await filesUnder(s.root));
  const child = spawnSync(process.execPath, [runner], { env: { PATH: process.env.PATH, TMPDIR: tmp, HOME: home, ...s.env }, encoding: 'utf8', timeout: 120000 });
  assert.equal(child.status, 0, child.stderr);
  assert.deepEqual(await filesUnder(scratch), [], 'nothing is written to TMPDIR or HOME');
  const created = (await filesUnder(s.root)).filter(file => !before.has(file));
  assert.deepEqual(created.map(file => file.slice(s.output.length + 1)).sort(), (await readdir(s.output)).sort(), 'only the signed output is created');
  const forms = [...secretForms(s.active), ...secretForms(s.next)], buffers = [...secretBuffers(s.active), ...secretBuffers(s.next)];
  for (const file of await filesUnder(s.root)) {
   assert.ok(!/private|\.pem$|\.key$/i.test(file.slice(s.root.length)), file);
   const content = await readFile(file), text = content.toString('latin1');
   for (const form of forms) assert.ok(!text.includes(form), `${file} holds a key form`);
   for (const bytes of buffers) assert.equal(content.indexOf(bytes), -1, `${file} holds key bytes`);
  }
 } finally { await s.cleanup(); await rm(scratch, { recursive: true, force: true }); }
});

test('the Sparkle seeds are imported as PKCS8 DER, never turned into a string, and the DER is zeroed', async () => {
 const s = await signingSetup({ staged: true });
 const seeds = [seedOf(s.active.sparkle), seedOf(s.next.sparkle)];
 const holdsSeed = buffer => Buffer.isBuffer(buffer) && seeds.some(seed => buffer.length >= 32 && buffer.indexOf(seed) !== -1);
 const stringified = [], concatenated = [];
 const toString = Buffer.prototype.toString, concat = Buffer.concat;
 Buffer.prototype.toString = function (...args) { if (holdsSeed(this)) stringified.push(args[0]); return toString.apply(this, args); };
 Buffer.concat = function (...args) { const out = concat.apply(Buffer, args); if (holdsSeed(out)) concatenated.push(out); return out; };
 try {
  await s.job.signInJob({ env: { ...s.env }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust });
 } finally { Buffer.prototype.toString = toString; Buffer.concat = concat; await s.cleanup(); }
 assert.deepEqual(stringified, [], 'a seed Buffer was turned into a string');
 assert.ok(concatenated.length >= 2, 'both seeds are imported through a PKCS8 DER Buffer');
 for (const der of concatenated) assert.ok(der.every(byte => byte === 0), 'the PKCS8 DER holding a seed is zeroed');
});

test('process.env no longer holds the key variables while signing', async () => {
 const keys = freshKeys(), staged = freshKeys();
 const saved = Object.fromEntries([...KEY_VARIABLES, ...Object.keys(RUN_ENV)].map(name => [name, process.env[name]]));
 try {
  Object.assign(process.env, RUN_ENV, slotEnv('A', keys), slotEnv('B', staged));
  const sign = spy(() => {
   for (const name of KEY_VARIABLES) assert.ok(!(name in process.env), name + ' still in process.env while signing');
  });
  // The staged slot needs pins this test does not set up, so it signs with the active slot only.
  await signInJob({ env: process.env, version: '1.2.3', candidateDirectory: '/c', outputDirectory: '/o', trust: { signing: { active: 'A', staged: null } }, sign });
  assert.equal(sign.calls.length, 1);
  for (const name of KEY_VARIABLES) assert.ok(!(name in process.env), name);
 } finally {
  for (const [name, value] of Object.entries(saved)) if (value === undefined) delete process.env[name]; else process.env[name] = value;
 }
});

test('all Buffers are zero after success and after a signer throw', async () => {
 const keys = freshKeys();
 const seen = [];
 const ok = spy(options => { seen.push(options.keys.sparkleSeed); assert.ok(options.keys.sparkleSeed.some(byte => byte !== 0), 'the seed is live while signing'); });
 await signInJob({ env: { ...RUN_ENV, ...slotEnv('A', keys) }, version: '1.2.3', candidateDirectory: '/c', outputDirectory: '/o', trust: { signing: { active: 'A', staged: null } }, sign: ok });
 const failing = spy(options => { seen.push(options.keys.sparkleSeed); throw new Error('signer failed'); });
 await assert.rejects(signInJob({ env: { ...RUN_ENV, ...slotEnv('A', keys) }, version: '1.2.3', candidateDirectory: '/c', outputDirectory: '/o', trust: { signing: { active: 'A', staged: null } }, sign: failing }), /signer failed/);
 assert.equal(seen.length, 2);
 for (const seed of seen) { assert.equal(seed.length, 32); assert.ok(seed.every(byte => byte === 0)); }
 const loaded = loadKeys({ ...slotEnv('B', keys) }, 'B');
 assert.ok(loaded.sparkleSeed.some(byte => byte !== 0));
 loaded.zero();
 assert.ok(loaded.sparkleSeed.every(byte => byte === 0));
 // With a staged slot, the active seed handed to the real signer is zeroed afterwards too.
 const s = await signingSetup({ staged: true });
 try {
  const seeds = [], real = s.signer.signDesktopRelease;
  await s.job.signInJob({ env: { ...s.env }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust, sign: async options => { seeds.push(options.keys.sparkleSeed); return real(options); } });
  assert.equal(seeds.length, 1);
  assert.ok(seeds[0].every(byte => byte === 0));
 } finally { await s.cleanup(); }
});

test('an end-to-end sign with generated test keys passes verifyDesktopBundle and candidate-digest verify', async () => {
 const s = await signingSetup();
 try {
  assert.deepEqual(await s.job.signInJob({ env: { ...s.env }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust }), { staged: false });
  const channel = s.fixture.channel, version = s.fixture.version;
  const published = join(s.root, 'published'); await mkdir(published);
  for (const asset of s.fixture.assets) if (!asset.name.endsWith('.sandbox.sig')) await copyFile(join(s.candidate, asset.name), join(published, asset.name));
  for (const name of await readdir(s.output)) await copyFile(join(s.output, name), join(published, name));
  const metadata = await readFile(join(published, `desktop-${channel}.json`)), signature = await readFile(join(published, `desktop-${channel}.json.sig`), 'utf8');
  const release = await s.verifier.verifyDesktopBundle({ metadata, signature, trust: s.trust, directory: published, run: s.fixture.run, jobs: s.fixture.jobs, version, now: Date.now() });
  assert.equal(release.version, version);
  assert.deepEqual(release.source, s.fixture.source);
  const digest = await writeDigest(s.output, { channel, version, staged: false });
  await verifyDigest(s.output, digest, { channel, version, staged: false });
  assert.ok(!(await readdir(s.output)).includes('rotation-proof.json'));
 } finally { await s.cleanup(); }
});

test('no key material or derived form (PEM line, JWK d, seed) appears in captured stdout or stderr', async () => {
 const s = await signingSetup({ staged: true });
 try {
  const script = join(s.tools, 'scripts/release-sign-job.mjs'), output = join(s.root, 'github-output');
  await writeFile(output, '');
  const forms = [...secretForms(s.active), ...secretForms(s.next)];
  const run = (env, out) => spawnSync(process.execPath, [script, s.fixture.version, s.candidate, out], { encoding: 'utf8', env: { PATH: process.env.PATH, GITHUB_OUTPUT: output, ...env }, timeout: 120000 });
  const success = run(s.env, s.output);
  assert.equal(success.status, 0, success.stderr);
  assert.equal(await readFile(output, 'utf8'), 'staged=true\n');
  const malformed = run({ ...s.env, DESKTOP_SANDBOX_KEY_B: s.env.DESKTOP_SANDBOX_KEY_B.slice(0, 40) }, join(s.root, 'signed-again'));
  assert.notEqual(malformed.status, 0);
  assert.match(malformed.stderr, /DESKTOP_SANDBOX_KEY_B/);
  const outsideRun = run({ ...s.env, GITHUB_REF: 'refs/heads/feature' }, join(s.root, 'signed-third'));
  assert.notEqual(outsideRun.status, 0);
  for (const result of [success, malformed, outsideRun]) for (const stream of [result.stdout, result.stderr]) {
   for (const form of forms) assert.ok(!stream.includes(form), 'a key form was printed');
   assert.ok(!/BEGIN PRIVATE KEY|"d"\s*:/.test(stream));
  }
  for (const value of Object.values(s.env)) if (value.length > 40) assert.ok(!success.stdout.includes(value) && !malformed.stderr.includes(value));
 } finally { await s.cleanup(); }
});

test('signDesktopRelease with keys and with keysDirectory produce identical output', async () => {
 const s = await signingSetup();
 try {
  const now = new Date();
  const viaKeys = join(s.root, 'via-keys'), viaDirectory = join(s.root, 'via-directory'), keysDirectory = join(s.root, 'throwaway-test-keys');
  const seed = Buffer.from(seedOf(s.active.sparkle));
  await s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: viaKeys, now, keys: { metadata: s.active.metadata, sparkleSeed: seed, sandbox: s.active.sandbox } });
  assert.ok(seed.some(byte => byte !== 0), 'the caller owns and zeroes its seed');
  // The legacy keys-directory form, with throwaway test keys only.
  await mkdir(keysDirectory, { mode: 0o700 });
  await writeFile(join(keysDirectory, 'metadata-private.pem'), s.active.metadata.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600 });
  await writeFile(join(keysDirectory, 'sparkle-private.key'), seedOf(s.active.sparkle).toString('base64'), { mode: 0o600 });
  await writeFile(join(keysDirectory, 'linux-sandbox-private.pem'), s.active.sandbox.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600 });
  await s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: viaDirectory, now, keysDirectory });
  await rm(keysDirectory, { recursive: true });
  const names = (await readdir(viaKeys)).sort();
  assert.deepEqual((await readdir(viaDirectory)).sort(), names);
  for (const name of names) assert.deepEqual(await readFile(join(viaKeys, name)), await readFile(join(viaDirectory, name)), name);
  await assert.rejects(s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: join(s.root, 'both'), now, keysDirectory, keys: { metadata: s.active.metadata, sparkleSeed: seed, sandbox: s.active.sandbox } }), /exactly one/i);
  await assert.rejects(s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: join(s.root, 'neither'), now }), /exactly one/i);
  await assert.rejects(s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: join(s.root, 'wrong'), now, keys: { metadata: s.active.sandbox, sparkleSeed: seed, sandbox: s.active.sandbox } }), /Metadata signer/);
  assert.ok(!(await lstat(join(s.root, 'wrong')).catch(() => null)), 'nothing is written for a key that does not match the pins');
 } finally { await s.cleanup(); }
});
