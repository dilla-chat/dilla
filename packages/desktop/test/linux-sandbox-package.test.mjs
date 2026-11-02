import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { buildCapsule, validateConfiguration, renderLauncher } from '../scripts/linux-sandbox-package.mjs';

const digest = b => createHash('sha512').update(b).digest('base64');
const helper = Buffer.from('fixture helper');
const maintainer = Buffer.from('fixture maintainer');
const key = Buffer.alloc(32, 4).toString('base64');
const config = { goVersion: 'go1.27.3', generation: 1, publishedAt: '2026-11-02T00:00:00Z', expiresAt: '2027-01-02T00:00:00Z', publicKeys: [key], helper: { version: 'fixture', size: helper.length, sha512: digest(helper), sha256: createHash('sha256').update(helper).digest('hex') } };

test('capsule binds exact final component bytes and an empty initial signature', () => {
  const { manifest, capsule, constants } = buildCapsule(config, maintainer, helper);
  assert.deepEqual(JSON.parse(manifest), { format: 1, kind: 'dilla-linux-sandbox', generation: 1, platform: 'linux', arch: 'x64', compatibilityEpoch: 1, sandboxApi: 1, publishedAt: config.publishedAt, expiresAt: config.expiresAt, maintainer: {size: maintainer.length, sha512: digest(maintainer)}, helper: {size: helper.length, sha512: digest(helper)} });
  assert.equal(capsule.subarray(0, 8).toString('binary'), 'DLSBOX1\0');
  assert.equal(capsule.readUInt32LE(8), manifest.length);
  assert.equal(capsule.readUInt32LE(12), 0);
  assert.equal(Number(capsule.readBigUInt64LE(16)), maintainer.length);
  assert.equal(Number(capsule.readBigUInt64LE(24)), helper.length);
  assert.deepEqual(capsule.subarray(32), Buffer.concat([manifest, maintainer, helper]));
  assert.equal(constants.capsuleSha512, digest(capsule));
  assert.equal(constants.maintainerOffset, 32 + manifest.length);
});

test('refuses missing or malformed public configuration and substituted helper', () => {
  for (const invalid of [{...config, publicKeys: []}, {...config, generation: 0}, {...config, expiresAt: '2028-01-02T00:00:00Z'}, {...config, publicKeys: ['bad']}, {...config, extra: true}]) assert.throws(() => validateConfiguration(invalid));
  assert.throws(() => buildCapsule(config, maintainer, Buffer.from('substitution')));
});

 test('launcher overlay freezes capsule identity and dedicated public keys', () => {
  const result = buildCapsule(config, maintainer, helper);
  const source = renderLauncher(config, result, '1.2.3-beta.4');
  assert.match(source, /func compiledCapsule\(\) \(frontend.EmbeddedCapsule, error\)/);
  assert.ok(source.includes(result.constants.capsuleSha512));
  assert.ok(source.includes(result.constants.manifestSha512));
  assert.ok(source.includes(result.constants.maintainerSha512));
  assert.ok(source.includes(key));
  assert.match(source, /frontend.DispatchLaunchVersion\(os.Args\[1:\], capsule, "1\.2\.3-beta\.4"\)/);
  for (const invalid of ['', '1.2.3/evil', '1.2.3\n']) assert.throws(() => renderLauncher(config, result, invalid));
});

test('public maintainer configuration excludes maintainer hash and app identity', async () => {
  const { maintainerConfiguration } = await import('../scripts/linux-sandbox-package.mjs');
  const value = JSON.parse(Buffer.from(maintainerConfiguration(config), 'base64').toString());
  assert.deepEqual(value, { publicKeys: [key], generation: 1, publishedAt: config.publishedAt, expiresAt: config.expiresAt, helper: { size: helper.length, sha512: digest(helper) } });
});

test('build refuses a substituted helper before invoking Go', async () => {
  const { prepareComponents } = await import('../scripts/linux-sandbox-package.mjs');
  await assert.rejects(prepareComponents({ config, helper: Buffer.from('wrong'), outputDirectory: '/nonexistent/never-create' }), /helper/i);
});

test('duplicate public configuration members are rejected including escaped aliases', async () => {
  const { parsePublicConfiguration } = await import('../scripts/linux-sandbox-package.mjs');
  assert.throws(() => parsePublicConfiguration('{"generation":1,"genera\\u0074ion":2}'), /duplicate/i);
  assert.throws(() => parsePublicConfiguration('{"helper":{"size":1,"size":2}}'), /duplicate/i);
});

test('maintenance keys cannot overlap application metadata or Sparkle trust', async () => {
  const { assertDedicatedKeys } = await import('../scripts/linux-sandbox-package.mjs');
  const { createPublicKey, generateKeyPairSync } = await import('node:crypto');
  const pair = generateKeyPairSync('ed25519');
  const pem = pair.publicKey.export({ type: 'spki', format: 'pem' });
  const raw = createPublicKey(pem).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
  assert.throws(() => assertDedicatedKeys({...config, publicKeys: [raw]}, { metadataPublicKeys: [pem], sparklePublicKey: key }), /separate/i);
  assert.throws(() => assertDedicatedKeys(config, { metadataPublicKeys: [pem], sparklePublicKey: key }), /separate/i);
});

test('maintainer output is independent of inherited Go build settings', {skip: process.platform !== 'linux'}, async () => {
  const { mkdtemp, readFile, rm, writeFile } = await import('node:fs/promises');
  const { tmpdir } = await import('node:os');
  const { join, resolve } = await import('node:path');
  const { execFileSync } = await import('node:child_process');
  const root = resolve(import.meta.dirname, '../../..');
  const dir = await mkdtemp(join(tmpdir(), 'dilla-env-test-'));
  try {
    const goenv = join(dir, 'goenv');
    await writeFile(goenv, 'GOAMD64=v3\nGOFLAGS=-tags=tainted\n');
    // This exercises Go environment isolation, not Electron's installed helper.
    // A valid x64 ELF and fixture public configuration keep lightweight Node CI
    // independent of ELECTRON_SKIP_BINARY_DOWNLOAD without skipping the build.
    const { sandboxReleaseFixture } = await import('./support/linux-sandbox-release-fixture.ts');
    const fixture = sandboxReleaseFixture(), production = JSON.parse(await readFile(join(root, 'packages/desktop/linux-sandbox-component.json'), 'utf8'));
    const helperFile = join(dir, 'fixture-chrome-sandbox'), configFile = join(dir, 'fixture-component.json');
    const fixtureHelper = fixture.capsule.subarray(fixture.capsule.length - fixture.config.helper.size);
    await writeFile(helperFile, fixtureHelper); await writeFile(configFile, JSON.stringify({ ...fixture.config, goVersion: production.goVersion }));
    const args = [join(root, 'packages/desktop/scripts/linux-sandbox-package.mjs'), configFile, helperFile];
    for (const [name, overrides] of [['v1', {GOAMD64:'v1', GOENV:'off', GOWORK:'off'}], ['v3', {GOAMD64:'v3', GOENV:goenv, GOFLAGS:'-tags=tainted', GOWORK:join(dir,'missing.work')} ]]) {
      execFileSync(process.execPath, [...args, join(dir, name), '1.0.0'], { cwd: root, env: {...process.env, ...overrides}, stdio: 'pipe' });
    }
    for (const file of ['sandbox-maintainer', 'component.sandbox', 'component.sandbox.json']) assert.deepEqual(await readFile(join(dir, 'v1', file)), await readFile(join(dir, 'v3', file)));
  } finally { await rm(dir, {recursive:true, force:true}); }
});
