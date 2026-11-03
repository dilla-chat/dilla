import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createPublicKey, sign, verify } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, readdir, rm, copyFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { buildReleaseFixture, isolatedReleaseTools, currentSandboxDates } from './support/release-fixture.mjs';
import { checkMacDiskImage } from '../scripts/check-mac-dmg.mjs';
import { macRotationOf, sparkleFeedKey } from '../scripts/feed-format.mjs';

/*
 * The macOS half of a key rotation. Sparkle's rule (https://sparkle-project.org/documentation/, "Rotating signing
 * keys"): an update may change "either your Apple code signing certificate or your EdDSA keys (but not both)", and
 * with SUVerifyUpdateBeforeExtraction "changing your EdDSA keys can only be done if the update archive is a Developer
 * ID code signed disk image (dmg)". So each channel's first release after `rotate --promote` updates macOS with a
 * Developer-ID-signed, notarized DMG that carries the new SUPublicEDKey, its archive signed by that new key and its
 * appcast by the old key the installed Macs trust (feed-format.mjs). Every other release keeps the zip.
 */

const der = key => key.export({ type: 'pkcs8', format: 'der' });
const raw = key => createPublicKey(key).export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
const pem = key => createPublicKey(key).export({ type: 'spki', format: 'pem' }).toString();
const jwk = key => createPublicKey({ key: { kty: 'OKP', crv: 'Ed25519', x: Buffer.from(key, 'base64').toString('base64url') }, format: 'jwk' });
const slotEnv = (slot, keys) => ({ [`DESKTOP_METADATA_KEY_${slot}`]: der(keys.metadata).toString('base64'), [`DESKTOP_SPARKLE_KEY_${slot}`]: der(keys.sparkle).subarray(-32).toString('base64'), [`DESKTOP_SANDBOX_KEY_${slot}`]: der(keys.sandbox).toString('base64') });
const RUN_ENV = Object.freeze({ GITHUB_ACTIONS: 'true', GITHUB_REF: 'refs/heads/main', GITHUB_EVENT_NAME: 'workflow_run' });

/** Splits a signed appcast into its content and Sparkle 2.9+ signature block. */
function feedParts(feed) {
 const boundary = feed.lastIndexOf(Buffer.from('<!-- sparkle-signatures:\n'));
 const signature = /edSignature: ([A-Za-z0-9+/]{86}==)/.exec(feed.subarray(boundary).toString())[1];
 return { content: feed.subarray(0, boundary), signature: Buffer.from(signature, 'base64') };
}

/**
 * Sparkle 2's acceptance of a regular app update with SURequireSignedFeed and SUVerifyUpdateBeforeExtraction, as the
 * 2.x branch implements it: SUAppcastDriver checks the signed feed under the installed app's key; AppInstaller
 * prevalidates the archive (validateDownloadPathWithFallbackOnCodeSigning with fallback on code signing for a regular
 * app update) under the installed key, falling back to the download's Developer ID signature with the installed
 * app's team (only a disk image carries one); SUUpdateValidator then requires the update to keep an EdDSA key and,
 * after the code-signing fallback, the archive signature to verify under the update's new SUPublicEDKey.
 */
function sparkleAccepts({ installedKey, installedTeam, feed, archive, archiveName, archiveSignature, diskImageTeam, updateKey }) {
 const { content, signature } = feedParts(feed);
 if (!verify(null, content, jwk(installedKey), signature)) return 'the update feed is improperly signed';
 let viaCodeSigning = false;
 if (!verify(null, archive, jwk(installedKey), archiveSignature)) {
  if (!archiveName.endsWith('.dmg') || !diskImageTeam || diskImageTeam !== installedTeam) return 'archive validation before extraction failed';
  viaCodeSigning = true;
 }
 if (!updateKey) return 'the update removes the EdDSA key';
 if (viaCodeSigning && !verify(null, archive, jwk(updateKey), archiveSignature)) return 'the update key does not match the archive signature';
 return 'accepted';
}

/** A candidate ready to sign, with isolated tools pinned to main's trust after `rotate --promote`: slot B active (the
 * fixture's keys), slot A (fresh keys, the installed Macs' Sparkle key) the pending macOS key change. */
async function rotationSetup({ pending = true } = {}) {
 const root = await mkdtemp(join(tmpdir(), 'dilla-mac-rotation-'));
 const candidate = join(root, 'candidate'); await mkdir(candidate);
 const fixture = await buildReleaseFixture({ directory: candidate, sandboxDates: currentSandboxDates() });
 await writeFile(join(candidate, 'acceptance.json'), JSON.stringify(fixture.source));
 await writeFile(join(candidate, 'ci-run.json'), JSON.stringify(fixture.run));
 await writeFile(join(candidate, 'ci-jobs.json'), JSON.stringify(fixture.jobs));
 const active = { metadata: fixture.key.privateKey, sparkle: fixture.sparkle.privateKey, sandbox: fixture.sandbox.pair.privateKey };
 const old = { metadata: generateKeyPairSync('ed25519').privateKey, sparkle: generateKeyPairSync('ed25519').privateKey, sandbox: generateKeyPairSync('ed25519').privateKey };
 const trust = { metadataPublicKeys: [pem(active.metadata), pem(old.metadata)], sparklePublicKey: raw(active.sparkle),
  signing: { active: 'B', staged: null, ...(pending ? { macRotation: { fromSlot: 'A', fromSparklePublicKey: raw(old.sparkle) } } : {}) } };
 const sandboxConfig = { ...fixture.sandbox.config, publicKeys: [raw(active.sandbox), raw(old.sandbox)] };
 const tools = join(root, 'tools');
 const { verifier, signer } = await isolatedReleaseTools(tools, { trust, sandboxConfig, files: ['scripts/release-sign-job.mjs'] });
 const job = await import(pathToFileURL(join(tools, 'scripts/release-sign-job.mjs')).href);
 const env = { ...RUN_ENV, ...slotEnv('B', active), ...slotEnv('A', old) };
 const output = join(root, 'signed');
 /** The signed output plus the CI files, laid out as the publisher re-verifies them. */
 const published = async () => {
  const directory = join(root, `published-${Math.random()}`); await mkdir(directory);
  for (const asset of fixture.assets) if (!asset.name.endsWith('.sandbox.sig')) await copyFile(join(candidate, asset.name), join(directory, asset.name));
  for (const name of await readdir(output)) await copyFile(join(output, name), join(directory, name));
  const metadata = await readFile(join(directory, `desktop-${fixture.channel}.json`)), signature = await readFile(join(directory, `desktop-${fixture.channel}.json.sig`), 'utf8');
  return { metadata, signature, trust, directory, run: fixture.run, jobs: fixture.jobs, version: fixture.version, now: Date.now() };
 };
 return { root, candidate, output, fixture, active, old, trust, verifier, signer, job, env, published, cleanup: () => rm(root, { recursive: true, force: true }) };
}

test('the rotation release updates macOS with a DMG signed by the new key, under an appcast signed by the key the installed Macs trust', async () => {
 const s = await rotationSetup();
 try {
  assert.deepEqual(await s.job.signInJob({ env: { ...s.env, MAC_UPDATE: 'dmg' }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust }), { staged: false });
  const bundle = await s.published();
  const release = await s.verifier.verifyDesktopBundle({ ...bundle, macUpdate: 'dmg' });
  const darwin = release.artifacts.find(artifact => artifact.platform === 'darwin');
  const name = `dilla-${s.fixture.version}-mac-arm64.dmg`;
  assert.equal(darwin.url, `https://github.com/dilla-chat/dilla/releases/download/desktop-v${s.fixture.version}/${name}`);
  assert.deepEqual(release.assets.map(asset => asset.name).filter(item => item.includes('-mac-')).sort(), [name, `dilla-${s.fixture.version}-mac-arm64.zip`], 'both macOS archives stay release assets');
  const archive = await readFile(join(s.candidate, name)), archiveSignature = Buffer.from(darwin.sparkleSignature, 'base64');
  // The archive signature is by the new key, the SUPublicEDKey the rotation build carries (main's sparklePublicKey,
  // electron-builder.cjs; sparkle.test.ts): Sparkle checks it under that key after the Developer ID fallback.
  assert.ok(verify(null, archive, jwk(s.trust.sparklePublicKey), archiveSignature));
  assert.ok(!verify(null, archive, jwk(raw(s.old.sparkle)), archiveSignature));
  // The appcast is signed by the old key: SURequireSignedFeed checks it under the installed app's key.
  const feed = await readFile(join(s.output, 'appcast-arm64.xml'));
  const { content, signature } = feedParts(feed);
  assert.ok(verify(null, content, jwk(raw(s.old.sparkle)), signature));
  assert.ok(!verify(null, content, jwk(s.trust.sparklePublicKey), signature));
  assert.match(content.toString(), new RegExp(`<enclosure url="${darwin.url.replaceAll('.', '\\.')}" sparkle:edSignature="${darwin.sparkleSignature.replaceAll('+', '\\+')}"`));
  assert.equal(sparkleFeedKey(s.trust, 'dmg'), raw(s.old.sparkle));

  // Sparkle, as an installed Mac on the old key runs it: the rotation release installs, with no reinstall.
  const team = 'TEAM123456', update = { installedKey: raw(s.old.sparkle), installedTeam: team, feed, archive, archiveName: name, archiveSignature, diskImageTeam: team, updateKey: s.trust.sparklePublicKey };
  assert.equal(sparkleAccepts(update), 'accepted');
  // The same new-key archive as a zip, or a disk image of another team, or unsigned, is refused: hence the signed DMG.
  assert.equal(sparkleAccepts({ ...update, archiveName: name.replace('.dmg', '.zip'), diskImageTeam: undefined }), 'archive validation before extraction failed');
  assert.equal(sparkleAccepts({ ...update, diskImageTeam: 'OTHERTEAM1' }), 'archive validation before extraction failed');
  assert.equal(sparkleAccepts({ ...update, diskImageTeam: undefined }), 'archive validation before extraction failed');
  // A build that did not carry the new key would be refused after extraction; an appcast signed by the new key never loads.
  assert.equal(sparkleAccepts({ ...update, updateKey: raw(s.old.sparkle) }), 'the update key does not match the archive signature');
  const newKeyFeed = Buffer.concat([content, Buffer.from(`<!-- sparkle-signatures:\nedSignature: ${sign(null, content, s.active.sparkle).toString('base64')}\nlength: ${content.length}\n-->\n`)]);
  assert.equal(sparkleAccepts({ ...update, feed: newKeyFeed }), 'the update feed is improperly signed');
 } finally { await s.cleanup(); }
});

test('the rotation release refuses a zip: the signer, the bundle verifier and the sign job all bind the DMG', async () => {
 const s = await rotationSetup();
 try {
  // A zip release signed while a change is pending (the channel's Macs already moved) verifies as a zip only.
  await s.job.signInJob({ env: { ...s.env, MAC_UPDATE: 'zip' }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust });
  const bundle = await s.published();
  assert.equal((await s.verifier.verifyDesktopBundle({ ...bundle, macUpdate: 'zip' })).artifacts.find(a => a.platform === 'darwin').url.endsWith('.zip'), true);
  await assert.rejects(s.verifier.verifyDesktopBundle({ ...bundle, macUpdate: 'dmg' }), /rotation release must update macOS through its disk image/);
  // Its appcast is signed by the active key, not the old one.
  const { content, signature } = feedParts(await readFile(join(s.output, 'appcast-arm64.xml')));
  assert.ok(verify(null, content, jwk(s.trust.sparklePublicKey), signature));
  // The rotation sign job needs the old slot: without it nothing is signed.
  const withoutOld = Object.fromEntries(Object.entries(s.env).filter(([name]) => !name.endsWith('_A')));
  await assert.rejects(s.job.signInJob({ env: { ...withoutOld, MAC_UPDATE: 'dmg' }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: join(s.root, 'other'), trust: s.trust }), /DESKTOP_METADATA_KEY_A/);
  // An appcast signer that is not the key the rotation starts from is refused.
  await assert.rejects(s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: join(s.root, 'other2'), macUpdate: 'dmg',
   keys: { metadata: s.active.metadata, sparkleSeed: der(s.active.sparkle).subarray(-32), sandbox: s.active.sandbox, feedSparkleSeed: der(generateKeyPairSync('ed25519').privateKey).subarray(-32) } }), /rotation starts from/);
  for (const value of [undefined, '', 'pkg', 'DMG']) {
   await assert.rejects(s.job.signInJob({ env: { ...s.env, MAC_UPDATE: value }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: join(s.root, 'other3'), trust: s.trust }), /zip or dmg/);
  }
 } finally { await s.cleanup(); }
});

test('a release without a pending macOS key change refuses a DMG everywhere', async () => {
 const s = await rotationSetup({ pending: false });
 try {
  assert.equal(macRotationOf(s.trust), null);
  await assert.rejects(s.job.signInJob({ env: { ...s.env, MAC_UPDATE: 'dmg' }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust }), /only allowed for the rotation release/);
  await assert.rejects(s.signer.signDesktopRelease({ version: s.fixture.version, artifactsDirectory: s.candidate, outputDirectory: join(s.root, 'other'), macUpdate: 'dmg',
   keys: { metadata: s.active.metadata, sparkleSeed: der(s.active.sparkle).subarray(-32), sandbox: s.active.sandbox, feedSparkleSeed: der(s.old.sparkle).subarray(-32) } }), /only allowed for the rotation release/);
  await s.job.signInJob({ env: { ...s.env, MAC_UPDATE: 'zip' }, version: s.fixture.version, candidateDirectory: s.candidate, outputDirectory: s.output, trust: s.trust });
  const bundle = await s.published();
  await s.verifier.verifyDesktopBundle(bundle);
  await assert.rejects(s.verifier.verifyDesktopBundle({ ...bundle, macUpdate: 'dmg' }), /only allowed for the rotation release/);
  // Signed metadata that names the DMG as the update (signed by the active keys) is refused as a zip release too.
  const release = JSON.parse(bundle.metadata.toString());
  const darwin = release.artifacts.find(artifact => artifact.platform === 'darwin');
  const dmg = release.assets.find(asset => asset.name.endsWith('-mac-arm64.dmg'));
  Object.assign(darwin, { url: darwin.url.replace(/\.zip$/, '.dmg'), size: dmg.size, sha512: dmg.sha512, sparkleSignature: sign(null, await readFile(join(bundle.directory, dmg.name)), s.active.sparkle).toString('base64') });
  const metadata = Buffer.from(JSON.stringify(release, null, 2) + '\n');
  await assert.rejects(s.verifier.verifyDesktopBundle({ ...bundle, metadata, signature: sign(null, metadata, s.active.metadata).toString('base64') }), /Only the rotation release/);
 } finally { await s.cleanup(); }
});

test('the macOS CI disk image check: main\'s key always, and a notarized Developer ID image of the app\'s team while a change is pending', async () => {
 const out = await mkdtemp(join(tmpdir(), 'dilla-dmg-check-'));
 try {
  const version = '1.2.3', key = Buffer.alloc(32, 2).toString('base64');
  await writeFile(join(out, `dilla-${version}-mac-arm64.dmg`), 'dmg'); await writeFile(join(out, `dilla-${version}-mac-arm64.zip`), 'zip');
  const settled = { sparklePublicKey: key, signing: { active: 'B', staged: null } };
  const pending = { ...settled, signing: { ...settled.signing, macRotation: { fromSlot: 'A', fromSparklePublicKey: Buffer.alloc(32, 1).toString('base64') } } };
  const fake = ({ plistKey = key, signed = true, imageTeam = 'TEAM123456', appTeam = 'TEAM123456', notarized = true } = {}) => {
   const calls = [];
   const run = (command, args) => {
    calls.push([command, ...args]);
    if (command === 'plutil') return { SUPublicEDKey: plistKey, SURequireSignedFeed: 'true', SUVerifyUpdateBeforeExtraction: 'true' }[args[1]] + '\n';
    if (command === 'codesign' && args[0] === '-dvv') {
     if (args[1].endsWith('Dilla.app')) return `Authority=Developer ID Application: Example (${appTeam})\nTeamIdentifier=${appTeam}\n`;
     if (!signed) throw new Error('code object is not signed at all');
     return `Authority=Developer ID Application: Example (${imageTeam})\nTeamIdentifier=${imageTeam}\n`;
    }
    if (command === 'spctl') return notarized ? 'accepted\nsource=Notarized Developer ID\n' : 'accepted\nsource=Developer ID\n';
    return '';
   };
   return { calls, run };
  };
  const check = (trust, options, env = { GITHUB_EVENT_NAME: 'push' }, list = () => ['.background', 'Applications', 'Dilla.app']) => { const f = fake(options); return { result: checkMacDiskImage({ out, version, trust, env, run: f.run, log: () => {}, list }), calls: f.calls }; };

  const plain = check(settled);
  assert.deepEqual(plain.result, { rotation: false });
  assert.ok(!plain.calls.some(([command]) => command === 'spctl' || command === 'xcrun'), 'no notarization is required of a zip release');
  assert.throws(() => check(settled, { plistKey: Buffer.alloc(32, 9).toString('base64') }), /SUPublicEDKey/);
  const rotation = check(pending);
  assert.deepEqual(rotation.result, { rotation: true, skipped: false });
  for (const expected of [['codesign', '--verify', '--strict'], ['xcrun', 'stapler', 'validate'], ['spctl', '--assess', '--type']]) assert.ok(rotation.calls.some(call => expected.every((part, i) => call[i] === part)), expected.join(' '));
  assert.ok(rotation.calls.at(-1)[0] === 'spctl' && rotation.calls.some(call => call[0] === 'hdiutil' && call[1] === 'detach'));
  assert.throws(() => check(pending, { signed: false }), /must be Developer ID signed/);
  assert.deepEqual(check(pending, { signed: false }, { GITHUB_EVENT_NAME: 'pull_request' }).result, { rotation: true, skipped: true }, 'a fork pull request has no credentials');
  assert.throws(() => check(pending, { imageTeam: 'OTHERTEAM1' }), /same Developer ID team/);
  assert.throws(() => check(pending, { notarized: false }), /notarized/);
  assert.throws(() => check(settled, {}, undefined, () => ['Applications']), /exactly one \.app .*found: Applications/);
  // An image an earlier step left attached: hdiutil reports its existing mount point, and the check reads that one.
  {
   const seen = [], f = fake({});
   const run = (command, args) => command === 'hdiutil' && args[0] === 'attach'
    ? '<plist><dict><key>mount-point</key>\n\t<string>/Volumes/Dilla 1.2.3</string></dict></plist>' : f.run(command, args);
   checkMacDiskImage({ out, version, trust: settled, env: { GITHUB_EVENT_NAME: 'push' }, run, log: () => {}, list: dir => { seen.push(dir); return ['Dilla.app']; } });
   assert.deepEqual(seen, ['/Volumes/Dilla 1.2.3']);
   assert.ok(f.calls.some(call => call[0] === 'plutil' && call.at(-1) === join('/Volumes/Dilla 1.2.3', 'Dilla.app', 'Contents', 'Info.plist')));
   assert.ok(f.calls.some(call => call[0] === 'hdiutil' && call[1] === 'detach' && call[2] === '/Volumes/Dilla 1.2.3'));
  }
  assert.throws(() => check(settled, {}, undefined, () => ['A.app', 'B.app']), /exactly one \.app/);
  await writeFile(join(out, 'extra.dmg'), 'x');
  assert.throws(() => check(pending), /exactly the disk image/);
 } finally { await rm(out, { recursive: true, force: true }); }
});
