import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, readdir, copyFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { execFileSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { buildReleaseFixture, isolatedReleaseTools, acceptedJobs } from './support/release-fixture.mjs';
// @ts-expect-error publishing tooling is JavaScript
import { appcast, signedAppcast } from '../scripts/feed-format.mjs';
// @ts-expect-error publishing tooling is JavaScript
import { verifyDesktopBundle, TARGETS, publicReleaseRecords, stagePublicReleaseRecords, PUBLIC_ACCEPTANCE_RECORDS, requiredJobInventory, inventoryDigest } from '../scripts/verify-release.mjs';
// @ts-expect-error build tooling is JavaScript
import { ACCOUNT_CHECKS } from '../scripts/installed-account-proof.mjs';
// @ts-expect-error build tooling is JavaScript
import { buildArtifactNames, buildProvenanceName } from '../scripts/build-provenance.mjs';

// @ts-expect-error native recipe tooling is JavaScript
import { aurRecipe } from '../scripts/build-native-linux-packages.mjs';
const buildTargets = [...TARGETS, 'linux-packages-x64'];

test('publication refuses missing native acceptance even when the generic CI status is green', async () => {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-publish-'));
 try {
  const fixture = await buildReleaseFixture({ directory });
  const { sandbox, key, sparkle, version, source, notes, jobs, assets, recipeDirectory, recipeName, release, metadata, context, resign } = fixture;
  assert.equal(release.assets.length, 10, 'seven installable/recipe assets plus three authenticated Linux sandbox assets');
  assert.equal(release.artifacts.length, 3, 'package managers own native-format updates');
  // Further verifier invocations below use isolated fixture trust, never production configuration.
  const testTools = join(directory,'fixture-verifier');
  const { verifier: { verifyDesktopBundle, verifyDesktopSigningCandidate } } = await isolatedReleaseTools(testTools, { trust: context.trust, sandboxConfig: sandbox.config });
  assert.equal((await verifyDesktopBundle(context)).version, version);
  // The fixture acceptance record, metadata, sidecar hashes and signature all
  // bind this malformed ELF; both release paths must reject the ELF itself.
  const oldManifestSize=sandbox.capsule.readUInt32LE(8), oldMaintainerSize=Number(sandbox.capsule.readBigUInt64LE(16));
  const malformedMaintainer=Buffer.from(sandbox.capsule.subarray(32+oldManifestSize,32+oldManifestSize+oldMaintainerSize));
  malformedMaintainer.writeBigUInt64LE(99999n,40); malformedMaintainer.writeUInt16LE(64,58); malformedMaintainer.writeUInt16LE(1,60);
  const malformedDoc=JSON.parse(sandbox.manifest.toString()); malformedDoc.maintainer.sha512=sandbox.hash(malformedMaintainer);
  const malformedManifest=Buffer.from(JSON.stringify(malformedDoc)), malformedHeader=Buffer.from(sandbox.capsule.subarray(0,32)); malformedHeader.writeUInt32LE(malformedManifest.length,8);
  const malformedCapsule=Buffer.concat([malformedHeader,malformedManifest,malformedMaintainer,sandbox.capsule.subarray(32+oldManifestSize+oldMaintainerSize)]);
  const malformedSignature=Buffer.from(sign(null,Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),malformedManifest]),sandbox.pair.privateKey).toString('base64')+'\n');
  const name=`dilla-${version}-linux-x64.sandbox`, proofPath=join(directory,buildProvenanceName('linux-x64')), cleanProof=await readFile(proofPath), badProof=JSON.parse(cleanProof.toString());
  const badRelease=JSON.parse(metadata.toString());
  for (const [suffix,content] of [['',malformedCapsule],['.json',malformedManifest],['.sig',malformedSignature]] as const) {const file=name+suffix; await writeFile(join(directory,file),content); const asset=badRelease.assets.find((x:{name:string})=>x.name===file); asset.size=content.length; asset.sha512=sandbox.hash(content); const proofAsset=badProof.artifacts.find((x:{name:string})=>x.name===file); if(proofAsset){proofAsset.size=content.length;proofAsset.sha512=asset.sha512;}}
  Object.assign(badProof.linuxSandbox,{capsuleSha512:sandbox.hash(malformedCapsule),embeddedCapsuleSha512:sandbox.hash(malformedCapsule),manifestSha512:sandbox.hash(malformedManifest),embeddedManifestSha512:sandbox.hash(malformedManifest)});
  const proofBytes=Buffer.from(JSON.stringify(badProof)); await writeFile(proofPath,proofBytes); badRelease.source.builds['linux-x64'].sha256=createHash('sha256').update(proofBytes).digest('hex');
  Object.assign(badRelease.artifacts.find((x:{platform:string})=>x.platform==='linux').linuxSandbox.bundle,{size:malformedCapsule.length,sha512:sandbox.hash(malformedCapsule)});
  const badMetadata=Buffer.from(JSON.stringify(badRelease)), badContext={...context,metadata:badMetadata,signature:sign(null,badMetadata,key.privateKey).toString('base64')};
  await assert.rejects(verifyDesktopSigningCandidate(badContext),/Invalid Linux sandbox release/); await assert.rejects(verifyDesktopBundle(badContext),/Invalid Linux sandbox release/);
  await writeFile(proofPath,cleanProof); await writeFile(join(directory,name),sandbox.capsule); await writeFile(join(directory,name+'.json'),sandbox.manifest); await writeFile(join(directory,name+'.sig'),sandbox.signature);
  const fixtureConfigPath=join(testTools,'linux-sandbox-component.json');
  await writeFile(fixtureConfigPath,JSON.stringify({...sandbox.config,publicKeys:[key.publicKey.export({type:'spki',format:'der'}).subarray(-32).toString('base64')]}));
  await assert.rejects(verifyDesktopBundle(context),/separate/i); await writeFile(fixtureConfigPath,JSON.stringify(sandbox.config));
  const sidecarPath = join(directory,`dilla-${version}-linux-x64.sandbox.sig`);
  await writeFile(sidecarPath, Buffer.from('wrong')); await assert.rejects(verifyDesktopBundle(context), /digest|sidecar/i); await writeFile(sidecarPath,sandbox.signature);
  const forgedSignature=Buffer.from(sandbox.signature); forgedSignature[0]=forgedSignature[0]===65?66:65;
  await writeFile(sidecarPath,forgedSignature); const forgedRelease=JSON.parse(metadata.toString()); forgedRelease.assets.find((a:{name:string})=>a.name.endsWith('.sandbox.sig')).sha512=sandbox.hash(forgedSignature);
  const forgedBytes=Buffer.from(JSON.stringify(forgedRelease)); await assert.rejects(verifyDesktopBundle({...context,metadata:forgedBytes,signature:sign(null,forgedBytes,key.privateKey).toString('base64')}),/sandbox/i); await writeFile(sidecarPath,sandbox.signature);
  const badDescriptor=JSON.parse(metadata.toString()); badDescriptor.artifacts.find((a:{platform:string})=>a.platform==='linux').linuxSandbox.bundle.sha512=sandbox.hash(sandbox.manifest);
  const badBytes=Buffer.from(JSON.stringify(badDescriptor)); await assert.rejects(verifyDesktopBundle({...context,metadata:badBytes,signature:sign(null,badBytes,key.privateKey).toString('base64')}),/descriptor/i);
  for (const ext of ['deb', 'rpm', 'aur.tar.gz']) {
   const missing = structuredClone(release); missing.assets = missing.assets.filter(asset => !asset.name.endsWith('.' + ext));
   await assert.rejects(verifyDesktopBundle(resign(missing)), /provenance|authenticated/i);
  }
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-linux-packages-x64')) }), /native Linux package producer/i);
  for (const gate of ['Build native Linux release packages', 'Record native Linux package build provenance']) {
   const absent = structuredClone(jobs); const producer = absent.find(job => job.name.endsWith('desktop-linux-packages-x64'))!; producer.steps = producer.steps!.filter(step => step.name !== gate);
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: absent }), /native Linux package producer/i);
  }
  // A source-clean producer and valid hashes are not installed-package qualification.
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.includes('desktop-linux-package-acceptance-')) }), /installed native Linux package acceptance/i);
  for (const format of ['deb', 'rpm', 'aur']) {
   const absent = structuredClone(jobs); absent.find(job => job.name.endsWith(`desktop-linux-package-acceptance-${format}-x64`))!.steps = [];
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: absent }), /installed native Linux package acceptance/i);
  }
  // Even authenticated replacement recipe bytes must pin the accepted DEB.
  const recipePath = join(directory, recipeName), cleanRecipe = await readFile(recipePath);
  await writeFile(join(recipeDirectory, 'PKGBUILD'), aurRecipe(version, 'f'.repeat(64)).PKGBUILD);
  execFileSync('tar', ['--format=ustar', '-czf', recipePath, '-C', recipeDirectory, '.SRCINFO', 'PKGBUILD']);
  const forgedRecipe = await readFile(recipePath), recipeRelease = structuredClone(release), recipeProofPath = join(directory, buildProvenanceName('linux-packages-x64')), cleanRecipeProof = await readFile(recipeProofPath), recipeProof = JSON.parse(cleanRecipeProof.toString());
  const changedAsset = { name: recipeName, size: forgedRecipe.length, sha512: createHash('sha512').update(forgedRecipe).digest('base64') };
  Object.assign(recipeRelease.assets.find(asset => asset.name === recipeName)!, changedAsset); Object.assign(recipeProof.artifacts[2], changedAsset);
  const changedProof = Buffer.from(JSON.stringify(recipeProof)); await writeFile(recipeProofPath, changedProof); recipeRelease.source.builds['linux-packages-x64'].sha256 = createHash('sha256').update(changedProof).digest('hex');
  await assert.rejects(verifyDesktopBundle(resign(recipeRelease)), /recipe/i);
  await writeFile(recipePath, cleanRecipe); await writeFile(recipeProofPath, cleanRecipeProof);
  // PR merge HEAD intentionally differs from API head_sha. The actual native
  // build records, rather than that equality/ancestry, bind accepted source.
  assert.notEqual(source.commit, context.run.head_sha);
  const unrelatedSource = JSON.parse(metadata.toString()); unrelatedSource.source.commit = 'a'.repeat(40); unrelatedSource.source.tree = 'b'.repeat(40); unrelatedSource.source.review.tree = unrelatedSource.source.tree;
  const restoredReports = new Map<string, Buffer>();
  for (const target of TARGETS) for (const kind of ['performance', 'installed-account']) {
   const path = join(directory, `${kind}-${target}.json`), original = await readFile(path); restoredReports.set(path, original);
   const forged = Buffer.from(JSON.stringify({ ...JSON.parse(original.toString()), sourceCommit: unrelatedSource.source.commit })); await writeFile(path, forged);
   unrelatedSource.source[kind === 'performance' ? 'performance' : 'installedAccounts'][target].sha256 = createHash('sha256').update(forged).digest('hex');
  }
  await assert.rejects(verifyDesktopBundle(resign(unrelatedSource)), /build provenance.*source/i);
  for (const [path, original] of restoredReports) await writeFile(path, original);
  for (const target of buildTargets) {
   const missing = JSON.parse(metadata.toString()); delete missing.source.builds[target];
   await assert.rejects(verifyDesktopBundle(resign(missing)), /build provenance/i);
   const path = join(directory, buildProvenanceName(target)), clean = await readFile(path);
   await rm(path); await assert.rejects(verifyDesktopBundle(context), /ENOENT|build provenance/i); await writeFile(path, clean);
   await writeFile(path, Buffer.concat([clean, Buffer.from('tampered')]));
   await assert.rejects(verifyDesktopBundle(context), /build provenance.*digest/i); await writeFile(path, clean);
   for (const fields of [{ sourceCommit: 'f'.repeat(40) }, { sourceTree: 'f'.repeat(40) }, { ciRun: 99 }, { sourceClean: false }, { target: 'other' }, { version: '1.2.4' }, { privateKey: 'never-publish' }, { artifacts: [{ ...JSON.parse(clean.toString()).artifacts[0], sha512: createHash('sha512').update('wrong installer').digest('base64') }] }]) {
    const changed = Buffer.from(JSON.stringify({ ...JSON.parse(clean.toString()), ...fields })), signed = JSON.parse(metadata.toString());
    signed.source.builds[target].sha256 = createHash('sha256').update(changed).digest('hex'); await writeFile(path, changed);
    await assert.rejects(verifyDesktopBundle(resign(signed)), /build provenance/i);
   }
   await writeFile(path, clean);
   for (let index = 0; index < JSON.parse(clean.toString()).artifacts.length; index++) {
    const changed = JSON.parse(clean.toString()), signed = JSON.parse(metadata.toString());
    changed.artifacts[index].sha512 = createHash('sha512').update('wrong native artifact').digest('base64');
    const bytes = Buffer.from(JSON.stringify(changed)); signed.source.builds[target].sha256 = createHash('sha256').update(bytes).digest('hex');
    await writeFile(path, bytes); await assert.rejects(verifyDesktopBundle(resign(signed)), /build provenance.*(artifact mismatch|proof)/i);
   }
   await writeFile(path, clean);
   if (target === 'linux-packages-x64') continue;
   const absent = structuredClone(jobs); absent.find(job => job.name.endsWith(`desktop-${target}`))!.steps = absent.find(job => job.name.endsWith(`desktop-${target}`))!.steps!.filter(step => step.name !== 'Record native desktop build provenance');
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: absent }), /Record native desktop build provenance/);
  }
  for (const target of TARGETS) {
   const missingRecord = JSON.parse(metadata.toString()); delete missingRecord.source.installedAccounts[target];
   await assert.rejects(verifyDesktopBundle(resign(missingRecord)), /installed.account/i);
   const path = join(directory, `installed-account-${target}.json`), clean = await readFile(path);
   await rm(path); await assert.rejects(verifyDesktopBundle(context), /installed.account|ENOENT/i); await writeFile(path, clean);
   await writeFile(path, Buffer.concat([clean, Buffer.from('tampered')]));
   await assert.rejects(verifyDesktopBundle(context), /installed.account.*digest/i); await writeFile(path, clean);
   for (const fields of [{ sourceCommit: 'f'.repeat(40) }, { sourceDirty: true }, { status: 'failed' }, { platform: 'unaccepted' }, { privateKey: 'must-not-be-published' }, ...ACCOUNT_CHECKS.map((check: string) => ({ [check]: false }))]) {
    const changed = Buffer.from(JSON.stringify({ ...JSON.parse(clean.toString()), ...fields })), signed = JSON.parse(metadata.toString());
    signed.source.installedAccounts[target].sha256 = createHash('sha256').update(changed).digest('hex'); await writeFile(path, changed);
    await assert.rejects(verifyDesktopBundle(resign(signed)), /installed.account/i);
   }
   await writeFile(path, clean);
   const clientJobs = (job: {name: string}) => [1, 2, 3].some(shard => job.name.endsWith(`desktop-client-${target}-${shard}`));
   const absent = structuredClone(jobs); for (const job of absent.filter(clientJobs)) job.steps = [];
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: absent }), /installed.account.*proof/i);
   // A second successful proof for the target, from another shard on Linux and from a second upload step elsewhere.
   const duplicate = structuredClone(jobs); duplicate.find(job => job.name.endsWith(`desktop-client-${target}-1`))!.steps = [{ name: 'Installed account upgrade proof', conclusion: 'success' }, ...(target === 'linux-x64' ? [] : [{ name: 'Installed account upgrade proof', conclusion: 'success' }])];
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: duplicate }), /installed.account.*proof/i);
  }
  // Execute the actual offline signer against a temporary copy of its public
  // tooling/trust. Production keys and repository trust are never modified.
  assert.equal(typeof publicReleaseRecords, 'function', 'publisher must share the complete public proof-record inventory');
  const tools = join(directory, 'isolated-signing-tools'), privateKeys = join(directory, 'private-signing-keys'), candidate = join(directory, 'public-candidate');
  await mkdir(privateKeys, { mode: 0o700 });
  for (const relative of ['scripts/release-feed.mjs', 'scripts/verify-release.mjs', 'scripts/feed-format.mjs', 'scripts/installed-account-proof.mjs', 'scripts/build-provenance.mjs','scripts/build-native-linux-packages.mjs','scripts/linux-package-version.mjs', 'src/update.ts',   'src/update-fs.ts', 'src/linux-sandbox-release.ts', 'scripts/linux-sandbox-package.mjs']) {
   const file = join(tools, relative); await mkdir(dirname(file), { recursive: true }); await copyFile(resolve(import.meta.dirname, '..', relative), file);
  }
  await writeFile(join(tools, 'package.json'), JSON.stringify({ type: 'module' }));
  await writeFile(join(tools, 'release-trust.json'), JSON.stringify(context.trust));
  await writeFile(join(tools, 'linux-sandbox-component.json'), JSON.stringify(sandbox.config));
  await writeFile(join(privateKeys, 'linux-sandbox-private.pem'), sandbox.pair.privateKey.export({type:'pkcs8',format:'pem'}), {mode:0o600});
  await writeFile(join(privateKeys, 'metadata-private.pem'), key.privateKey.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600 });
  await writeFile(join(privateKeys, 'sparkle-private.key'), sparkle.privateKey.export({ type: 'pkcs8', format: 'der' }).subarray(-32).toString('base64'), { mode: 0o600 });
  await writeFile(join(directory, 'acceptance.json'), JSON.stringify(source));
  await writeFile(join(directory, 'ci-run.json'), JSON.stringify(context.run)); await writeFile(join(directory, 'ci-jobs.json'), JSON.stringify(jobs));
  const { signDesktopRelease } = await import(pathToFileURL(join(tools, 'scripts/release-feed.mjs')).href);
  const keyPath=join(privateKeys,'linux-sandbox-private.pem'), savedKey=await readFile(keyPath); await rm(keyPath);
  const ciJobsPath=join(directory,'ci-jobs.json'); await writeFile(ciJobsPath,JSON.stringify(jobs.filter(job=>!job.name.endsWith('desktop-linux-x64'))));
  await assert.rejects(signDesktopRelease({ version, artifactsDirectory: directory, keysDirectory: privateKeys, outputDirectory: candidate, now: new Date(context.now) }), /Missing accepted native target/);
  await writeFile(ciJobsPath,JSON.stringify(jobs)); await writeFile(keyPath,savedKey,{mode:0o600});
  await signDesktopRelease({ version, artifactsDirectory: directory, keysDirectory: privateKeys, outputDirectory: candidate, now: new Date(context.now) });
  const publicRecords: string[] = publicReleaseRecords('stable');
  assert.deepEqual((await readdir(candidate)).sort(), [...publicRecords, `dilla-${version}-linux-x64.sandbox.sig`].sort(), 'signing candidate and publisher must consume identical public proof records, without private keys');
  for (const target of TARGETS) assert.deepEqual(await readFile(join(candidate, `installed-account-${target}.json`)), await readFile(join(directory, `installed-account-${target}.json`)));
  for (const target of buildTargets) assert.deepEqual(await readFile(join(candidate, buildProvenanceName(target))), await readFile(join(directory, buildProvenanceName(target))));
  // Run the same staging boundary as the publisher: only the records delivered
  // inside the accepted CI installers may establish build/source provenance.
  const downloaded = join(directory, 'independent-ci-download'); await mkdir(downloaded);
  for (const target of buildTargets) await copyFile(join(directory, buildProvenanceName(target)), join(downloaded, buildProvenanceName(target)));
  const trustedPath = join(downloaded, buildProvenanceName('linux-packages-x64')), candidatePath = join(candidate, buildProvenanceName('linux-packages-x64')), trusted = await readFile(trustedPath);
  await writeFile(candidatePath, JSON.stringify({ ...JSON.parse(trusted.toString()), sourceCommit: 'a'.repeat(40) }));
  await assert.rejects(stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version }), /CI.*build provenance/i);
  assert.deepEqual(await readFile(trustedPath), trusted); assert.equal((await readdir(downloaded)).length, buildTargets.length, 'reject before staging any candidate records');
  await writeFile(candidatePath, trusted); await rm(trustedPath);
  await assert.rejects(stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version }), /ENOENT|build provenance/i);
  await writeFile(trustedPath, trusted); await stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version });
  assert.deepEqual((await readdir(downloaded)).sort(), [...publicRecords, `dilla-${version}-linux-x64.sandbox.sig`].sort());
  for (const name of publicRecords) assert.deepEqual(await readFile(join(downloaded, name)), await readFile(join(candidate, name)));
  for (const target of TARGETS) {
   const missingInstalled = structuredClone(jobs);
   const native = missingInstalled.find(job => job.name.endsWith(`desktop-${target}`))!;
   native.steps = native.steps!.filter(step => !step.name.startsWith('Installed '));
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: missingInstalled }), /missing accepted Installed/);
  }
  {const missing=structuredClone(jobs);const linux=missing.find(job=>job.name.endsWith('desktop-linux-x64'))!;linux.steps=linux.steps!.filter(step=>step.name!=='Linux AppImage sandbox native acceptance');await assert.rejects(verifyDesktopBundle({...context,jobs:missing}),/Linux AppImage sandbox native acceptance/);}
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-voice-win-x64')) }), /Missing Electron voice/);
  const noPerformance = structuredClone(jobs);
  noPerformance.find(job => job.name.endsWith('desktop-voice-linux-x64'))!.steps = [];
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: noPerformance }), /performance comparison/);
  const path = join(directory, 'performance-linux-x64.json');
  const clean = await import('node:fs/promises').then(fs => fs.readFile(path));
  const dirty = Buffer.from(JSON.stringify({ ...JSON.parse(clean.toString()), sourceDirty: true }));
  const dirtyRelease = JSON.parse(metadata.toString());
  dirtyRelease.source.performance['linux-x64'].sha256 = createHash('sha256').update(dirty).digest('hex');
  const dirtyMetadata = Buffer.from(JSON.stringify(dirtyRelease));
  await writeFile(path, dirty);
  await assert.rejects(verifyDesktopBundle({ ...context, metadata: dirtyMetadata, signature: sign(null, dirtyMetadata, key.privateKey).toString('base64') }), /Incomplete measured/);
  await writeFile(path, clean);
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-client-mac-arm64-1')) }), /Missing Electron/);
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-client-linux-x64-3')) }), /Missing Electron/);
  const noAudio = structuredClone(jobs); noAudio[0].steps = noAudio[0].steps!.filter(step => !step.name.includes('audio encryption'));
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: noAudio }), /missing accepted Application audio/);
  const originalFeed = await import('node:fs/promises').then(fs => fs.readFile(join(directory, 'appcast-arm64.xml')));
  await writeFile(join(directory, 'appcast-arm64.xml'), 'tampered feed');
  await assert.rejects(verifyDesktopBundle(context), /Unsigned Sparkle/);
  await writeFile(join(directory, 'appcast-arm64.xml'), originalFeed);
  await writeFile(join(directory, 'release-notes.md'), 'tampered release notes');
  await assert.rejects(verifyDesktopBundle(context), /Release notes/);
  await writeFile(join(directory, 'release-notes.md'), notes);
  await writeFile(join(directory, assets[0].name), 'tampered');
  await assert.rejects(verifyDesktopBundle(context), /artifact digest/);
 } finally { await rm(directory, { recursive: true, force: true }); }
});

async function attestationFixture() {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-attestation-'));
 const fixture = await buildReleaseFixture({ directory });
 const { verifier } = await isolatedReleaseTools(join(directory, 'fixture-verifier'), { trust: fixture.trust, sandboxConfig: fixture.sandbox.config });
 return { ...fixture, verifier, cleanup: () => rm(directory, { recursive: true, force: true }) };
}
type Changed = { source: { review: Record<string, unknown> & { inventory?: unknown } } };
const changedRelease = (metadata: Buffer): Changed => JSON.parse(metadata.toString());

test('publication refuses an attestation whose inventory digest does not match the run\'s jobs', async () => {
 const f = await attestationFixture();
 try {
  assert.equal((await f.verifier.verifyDesktopBundle(f.context)).version, f.version);
  const wrong = changedRelease(f.metadata); wrong.source.review.inventory = { sha256: 'f'.repeat(64) };
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(wrong)), /attestation/i);
  // A run whose required jobs differ from the attested ones, though every acceptance rule still passes.
  const jobs = structuredClone(f.jobs); jobs.find(job => job.name.endsWith('desktop-client-linux-x64-1'))!.steps![0].conclusion = 'cancelled';
  await assert.rejects(f.verifier.verifyDesktopBundle({ ...f.context, jobs }), /attestation/i);
 } finally { await f.cleanup(); }
});

test('publication refuses an attestation for another ci run id', async () => {
 const f = await attestationFixture();
 try {
  const wrong = changedRelease(f.metadata); wrong.source.review.ciRun = 11;
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(wrong)), /attestation/i);
 } finally { await f.cleanup(); }
});

test('publication refuses an attestation for another tree', async () => {
 const f = await attestationFixture();
 try {
  const wrong = changedRelease(f.metadata); wrong.source.review.tree = 'f'.repeat(40);
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(wrong)), /attestation/i);
 } finally { await f.cleanup(); }
});

test('publication refuses the old human review shape', async () => {
 const f = await attestationFixture();
 try {
  const old = changedRelease(f.metadata); old.source.review = { status: 'approved', tree: f.source.tree, sha256: 'a'.repeat(64) };
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(old)), /attestation/i);
  const extra = changedRelease(f.metadata); extra.source.review = { ...extra.source.review, sha256: 'a'.repeat(64) };
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(extra)), /attestation/i);
  const approved = changedRelease(f.metadata); approved.source.review.status = 'approved';
  await assert.rejects(f.verifier.verifyDesktopBundle(f.resign(approved)), /attestation/i);
  assert.ok(!PUBLIC_ACCEPTANCE_RECORDS.includes('security-review.md'), 'no human review file is part of a release');
 } finally { await f.cleanup(); }
});

test('requiredJobInventory is sorted, complete and covers every job verifyBundle requires', async () => {
 const f = await attestationFixture();
 try {
  const inventory = requiredJobInventory(f.jobs, f.version);
  const names = inventory.map((job: { name: string }) => job.name);
  assert.deepEqual(names, [...names].sort());
  assert.equal(inventory.length, f.jobs.length, 'every fixture job is a required job');
  assert.deepEqual(inventory.find((job: { name: string }) => job.name === 'desktop / desktop-linux-x64'), f.jobs.find(job => job.name === 'desktop / desktop-linux-x64'));
  for (let index = 0; index < f.jobs.length; index++) {
   const jobs = f.jobs.filter((_, i) => i !== index);
   assert.throws(() => requiredJobInventory(jobs, f.version), Error, f.jobs[index].name);
   await assert.rejects(f.verifier.verifyDesktopBundle({ ...f.context, jobs }), Error, f.jobs[index].name);
   const failed = structuredClone(f.jobs); failed[index].conclusion = 'failure';
   assert.throws(() => requiredJobInventory(failed, f.version), /did not succeed/, f.jobs[index].name);
  }
  const duplicated = [...f.jobs, structuredClone(f.jobs[0])];
  assert.throws(() => requiredJobInventory(duplicated, f.version), /exactly once/i);
 } finally { await f.cleanup(); }
});

test('requiredJobInventory ignores extra unrelated jobs', () => {
 const jobs = acceptedJobs(), inventory = requiredJobInventory(jobs, '1.2.3');
 const noisy = [{ name: 'go / test', conclusion: 'failure', steps: [{ name: 'x', conclusion: 'failure' }] }, ...structuredClone(jobs), { name: 'desktop / desktop-test-tools-linux-x64', conclusion: 'success', steps: [] }];
 for (const job of noisy) job.steps = [{ name: 'Set up job', conclusion: 'success' }, ...(job.steps ?? []), { name: 'Post checkout', conclusion: 'success' }];
 assert.deepEqual(requiredJobInventory(noisy, '1.2.3'), inventory);
});

test('requiredJobInventory with and without the version argument matches between assemble and verify', async () => {
 const f = await attestationFixture();
 try {
  assert.deepEqual(requiredJobInventory(f.jobs, f.version), requiredJobInventory(f.jobs));
  const expected = createHash('sha256').update(JSON.stringify(requiredJobInventory(f.jobs, f.version))).digest('hex');
  assert.equal(inventoryDigest(f.jobs, f.version), expected);
  assert.equal(inventoryDigest(f.jobs), expected);
  assert.equal(f.source.review.inventory.sha256, expected, 'the fixture attests through the shared helper');
  assert.equal((await f.verifier.verifyDesktopBundle(f.context)).source.review.inventory.sha256, expected);
  assert.throws(() => requiredJobInventory(f.jobs, '1.2'), /version/i);
 } finally { await f.cleanup(); }
});
