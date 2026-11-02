import { test } from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, createHash, sign } from 'node:crypto';
import { mkdtemp, mkdir, writeFile, readFile, readdir, copyFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, dirname, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { sandboxReleaseFixture } from './support/linux-sandbox-release-fixture.ts';
// @ts-expect-error publishing tooling is JavaScript
import { appcast, signedAppcast } from '../scripts/feed-format.mjs';
// @ts-expect-error publishing tooling is JavaScript
import { verifyDesktopBundle, TARGETS, publicReleaseRecords, stagePublicReleaseRecords } from '../scripts/verify-release.mjs';
// @ts-expect-error build tooling is JavaScript
import { ACCOUNT_CHECKS } from '../scripts/installed-account-proof.mjs';
// @ts-expect-error build tooling is JavaScript
import { buildArtifactNames, buildProvenanceName } from '../scripts/build-provenance.mjs';

test('publication refuses missing native acceptance even when the generic CI status is green', async () => {
 const sandbox = sandboxReleaseFixture();
 const directory = await mkdtemp(join(tmpdir(), 'dilla-publish-'));
 try {
  const key = generateKeyPairSync('ed25519'), sparkle = generateKeyPairSync('ed25519'), version = '1.2.3';
  const source = { format: 1, ciRun: 10, commit: '1'.repeat(40), head: '2'.repeat(40), tree: '3'.repeat(40), review: { status: 'approved', tree: '3'.repeat(40), sha256: '' }, builds: {} as Record<string, { sha256: string }>, performance: {} as Record<string, { sha256: string }>, installedAccounts: {} as Record<string, { sha256: string }>, notes: { sha256: '' } };
  const notes = Buffer.from('Dilla desktop 1.2.3'); source.notes.sha256 = createHash('sha256').update(notes).digest('hex'); await writeFile(join(directory, 'release-notes.md'), notes);
  const review = Buffer.from('Independent completed-app security review'); source.review.sha256 = createHash('sha256').update(review).digest('hex'); await writeFile(join(directory, 'security-review.md'), review);
  const jobs: { name: string; conclusion: string; steps?: { name: string; conclusion: string }[] }[] = [];
  for (const target of TARGETS) {
   jobs.push({ name: `desktop / desktop-${target}`, conclusion: 'success', steps: ['Record native desktop build provenance', 'Application audio encryption and exclusion', target.startsWith('mac-') ? 'Installed unsigned Sparkle upgrade and signed-feed rejection' : 'Installed upgrade acceptance', ...(target==='win-x64'?['Windows launcher kernel acceptance','Windows installer maintenance acceptance']:[]), ...(target==='linux-x64'?['Linux AppImage sandbox native acceptance']:[])].map(name => ({ name, conclusion: 'success' })) });
   for (let n = 1; n <= 3; n++) jobs.push({ name: `desktop / desktop-client-${target}-${n}`, conclusion: 'success', steps: [{ name: 'Installed account upgrade proof', conclusion: n === 2 ? 'success' : 'skipped' }] });
   jobs.push({ name: `desktop / desktop-voice-${target}`, conclusion: 'success', steps: [{ name: 'Desktop performance comparison', conclusion: 'success' }] });
   const report = Buffer.from(JSON.stringify({ format: 1, target, sourceCommit: source.commit, sourceDirty: false, status: 'measured', ...Object.fromEntries(['startup', 'memory', 'idleCPU', 'interaction', 'callAudio'].map(metric => [metric, [{ browser: 1, desktop: 2 }]])) }));
   await writeFile(join(directory, `performance-${target}.json`), report); source.performance[target] = { sha256: createHash('sha256').update(report).digest('hex') };
   const [platform, arch] = ({ 'linux-x64': ['linux', 'x64'], 'win-x64': ['win32', 'x64'], 'mac-x64': ['darwin', 'x64'], 'mac-arm64': ['darwin', 'arm64'] } as Record<string, string[]>)[target];
   const account = Buffer.from(JSON.stringify({ format: 1, platform, arch, from: '1.0.0', to: '1.0.1', ...Object.fromEntries(ACCOUNT_CHECKS.map((check: string) => [check, true])),
    target, sourceCommit: source.commit, sourceDirty: false, status: 'passed' }));
   await writeFile(join(directory, `installed-account-${target}.json`), account); source.installedAccounts[target] = { sha256: createHash('sha256').update(account).digest('hex') };
  }
  const assets: Array<{name:string;size:number;sha512:string}> = [];
  for (const name of [`dilla-${version}-linux-x64.AppImage`, ...['sandbox','sandbox.json','sandbox.sig'].map(ext => `dilla-${version}-linux-x64.${ext}`), `dilla-${version}-win-x64.exe`, `dilla-${version}-win-x64.manifest.json`, ...['x64', 'arm64'].flatMap(arch => [`dilla-${version}-mac-${arch}.zip`, `dilla-${version}-mac-${arch}.dmg`])]) {
   const bytes = name.endsWith('.sandbox.sig') ? sandbox.signature : name.endsWith('.sandbox.json') ? sandbox.manifest : name.endsWith('.sandbox') ? sandbox.capsule : name.endsWith('.manifest.json') ? Buffer.from(JSON.stringify({format:1,kind:'dilla-desktop-program',version,channel:'stable',platform:'win32',arch:'x64',files:[{path:'dilla-desktop.exe',size:1,sha512:createHash('sha512').update('x').digest('base64')},{path:'resources/app.asar',size:1,sha512:createHash('sha512').update('y').digest('base64')}]})) : Buffer.from(name); await writeFile(join(directory, name), bytes); assets.push({ name, size: bytes.length, sha512: createHash('sha512').update(bytes).digest('base64') });
  }
  for (const target of TARGETS) {
   const record = Buffer.from(JSON.stringify({ format: 1, kind: 'dilla-desktop-build', repository: 'jonasthim/dilla', target, version,
    sourceCommit: source.commit, sourceTree: source.tree, ciRun: source.ciRun, sourceClean: true,
    artifacts: buildArtifactNames(target, version).map((name: string) => assets.find(asset => asset.name === name)), ...(target === 'linux-x64' ? {linuxSandbox:{capsuleSha512:sandbox.hash(sandbox.capsule),manifestSha512:sandbox.hash(sandbox.manifest),embeddedCapsuleSha512:sandbox.hash(sandbox.capsule),embeddedManifestSha512:sandbox.hash(sandbox.manifest),electronSha512:sandbox.hash(Buffer.from('fixture-elf')),asarSha512:sandbox.hash(Buffer.from('fixture-asar'))}} : {}) }));
   await writeFile(join(directory, buildProvenanceName(target)), record); source.builds[target] = { sha256: createHash('sha256').update(record).digest('hex') };
  }
  const release = { format: 1, version, channel: 'stable', publishedAt: '2026-11-01T00:00:00Z', expiresAt: '2026-12-01T00:00:00Z', source, assets, artifacts: assets.filter(asset => !asset.name.endsWith('.dmg') && !asset.name.endsWith('.manifest.json') && !asset.name.includes('.sandbox')).map(({name, ...asset}) => ({ ...asset, platform: name.includes('-linux-') ? 'linux' : name.includes('-win-') ? 'win32' : 'darwin', arch: name.includes('arm64') ? 'arm64' : 'x64', url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/${name}`, ...(name.endsWith('-linux-x64.AppImage') ? {linuxSandbox:{compatibilityEpoch:1,minimumGeneration:1,bundle:{url:`https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-linux-x64.sandbox`,size:sandbox.capsule.length,sha512:sandbox.hash(sandbox.capsule)}}} : {}), ...(name.endsWith('-win-x64.exe') ? {programManifest:{url:`https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-win-x64.manifest.json`,size:assets.find(a=>a.name.endsWith('.manifest.json'))!.size,sha512:assets.find(a=>a.name.endsWith('.manifest.json'))!.sha512}}:{}), ...(name.endsWith('.zip') ? { sparkleSignature: sign(null, Buffer.from(name), sparkle.privateKey).toString('base64') } : {}) })) };
  for (const artifact of release.artifacts.filter(a => a.platform === 'darwin')) await writeFile(join(directory, `appcast-${artifact.arch}.xml`), signedAppcast(appcast(release, artifact, artifact.sparkleSignature), sparkle.privateKey));
  const metadata = Buffer.from(JSON.stringify(release)), signature = sign(null, metadata, key.privateKey).toString('base64');
  const context = { metadata, signature, trust: { metadataPublicKeys: [key.publicKey.export({ type: 'spki', format: 'pem' }).toString()], sparklePublicKey: sparkle.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64') }, directory, run: { id: 10, head_sha: source.head, name: 'ci', status: 'completed', conclusion: 'success', repository: { full_name: 'jonasthim/dilla' }, head_repository: { full_name: 'jonasthim/dilla' } }, jobs, version, now: Date.parse('2026-11-02') };
  // Further verifier invocations below use isolated fixture trust, never production configuration.
  const testTools = join(directory,'fixture-verifier');
  for (const relative of ['scripts/verify-release.mjs','scripts/feed-format.mjs','scripts/installed-account-proof.mjs','scripts/build-provenance.mjs','scripts/linux-sandbox-package.mjs','src/update.ts','src/windows-release.ts','src/windows-manifest.ts','src/update-fs.ts','src/linux-sandbox-release.ts']) { const path=join(testTools,relative); await mkdir(dirname(path),{recursive:true}); await copyFile(resolve(import.meta.dirname,'..',relative),path); }
  await writeFile(join(testTools,'package.json'),JSON.stringify({type:'module'})); await writeFile(join(testTools,'linux-sandbox-component.json'),JSON.stringify(sandbox.config));
  const {verifyDesktopBundle,verifyDesktopSigningCandidate} = await import(pathToFileURL(join(testTools,'scripts/verify-release.mjs')).href);
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
  const resign = (changed: unknown) => { const bytes = Buffer.from(JSON.stringify(changed)); return { ...context, metadata: bytes, signature: sign(null, bytes, key.privateKey).toString('base64') }; };
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
  for (const target of TARGETS) {
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
   const duplicate = structuredClone(jobs); duplicate.find(job => job.name.endsWith(`desktop-client-${target}-1`))!.steps = [{ name: 'Installed account upgrade proof', conclusion: 'success' }];
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: duplicate }), /installed.account.*proof/i);
  }
  // Execute the actual offline signer against a temporary copy of its public
  // tooling/trust. Production keys and repository trust are never modified.
  assert.equal(typeof publicReleaseRecords, 'function', 'publisher must share the complete public proof-record inventory');
  const tools = join(directory, 'isolated-signing-tools'), privateKeys = join(directory, 'private-signing-keys'), candidate = join(directory, 'public-candidate');
  await mkdir(privateKeys, { mode: 0o700 });
  for (const relative of ['scripts/release-feed.mjs', 'scripts/verify-release.mjs', 'scripts/feed-format.mjs', 'scripts/installed-account-proof.mjs', 'scripts/build-provenance.mjs', 'src/update.ts', 'src/windows-release.ts', 'src/windows-manifest.ts', 'src/update-fs.ts', 'src/linux-sandbox-release.ts', 'scripts/linux-sandbox-package.mjs']) {
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
  for (const target of TARGETS) assert.deepEqual(await readFile(join(candidate, buildProvenanceName(target))), await readFile(join(directory, buildProvenanceName(target))));
  // Run the same staging boundary as the publisher: only the records delivered
  // inside the accepted CI installers may establish build/source provenance.
  const downloaded = join(directory, 'independent-ci-download'); await mkdir(downloaded);
  for (const target of TARGETS) await copyFile(join(directory, buildProvenanceName(target)), join(downloaded, buildProvenanceName(target)));
  const trustedPath = join(downloaded, buildProvenanceName(TARGETS[0])), candidatePath = join(candidate, buildProvenanceName(TARGETS[0])), trusted = await readFile(trustedPath);
  await writeFile(candidatePath, JSON.stringify({ ...JSON.parse(trusted.toString()), sourceCommit: 'a'.repeat(40) }));
  await assert.rejects(stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version }), /CI.*build provenance/i);
  assert.deepEqual(await readFile(trustedPath), trusted); assert.equal((await readdir(downloaded)).length, 4, 'reject before staging any candidate records');
  await writeFile(candidatePath, trusted); await rm(trustedPath);
  await assert.rejects(stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version }), /ENOENT|build provenance/i);
  await writeFile(trustedPath, trusted); await stagePublicReleaseRecords({ candidate, directory: downloaded, channel: 'stable', version });
  assert.deepEqual((await readdir(downloaded)).sort(), [...publicRecords, `dilla-${version}-linux-x64.sandbox.sig`].sort());
  for (const name of publicRecords) assert.deepEqual(await readFile(join(downloaded, name)), await readFile(join(candidate, name)));
  const noManifest = JSON.parse(metadata.toString()); delete noManifest.artifacts.find((a: {platform:string})=>a.platform==='win32').programManifest; const missingManifestBytes=Buffer.from(JSON.stringify(noManifest));
  await assert.rejects(verifyDesktopBundle({...context,metadata:missingManifestBytes,signature:sign(null,missingManifestBytes,key.privateKey).toString('base64')}), /manifest/i);
  for (const target of TARGETS) {
   const missingInstalled = structuredClone(jobs);
   const native = missingInstalled.find(job => job.name.endsWith(`desktop-${target}`))!;
   native.steps = native.steps!.filter(step => !step.name.startsWith('Installed '));
   await assert.rejects(verifyDesktopBundle({ ...context, jobs: missingInstalled }), /missing accepted Installed/);
  }
  {const missing=structuredClone(jobs);const linux=missing.find(job=>job.name.endsWith('desktop-linux-x64'))!;linux.steps=linux.steps!.filter(step=>step.name!=='Linux AppImage sandbox native acceptance');await assert.rejects(verifyDesktopBundle({...context,jobs:missing}),/Linux AppImage sandbox native acceptance/);}
  for(const gate of ['Windows launcher kernel acceptance','Windows installer maintenance acceptance']){const missing=structuredClone(jobs);const win=missing.find(job=>job.name.endsWith('desktop-win-x64'))!;win.steps=win.steps!.filter(step=>step.name!==gate);await assert.rejects(verifyDesktopBundle({...context,jobs:missing}),new RegExp(gate));}
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
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: jobs.filter(job => !job.name.endsWith('desktop-client-mac-arm64-3')) }), /Missing Electron/);
  const noAudio = structuredClone(jobs); noAudio[0].steps = noAudio[0].steps!.filter(step => !step.name.includes('audio encryption'));
  await assert.rejects(verifyDesktopBundle({ ...context, jobs: noAudio }), /missing accepted Application audio/);
  const originalFeed = await import('node:fs/promises').then(fs => fs.readFile(join(directory, 'appcast-x64.xml')));
  await writeFile(join(directory, 'appcast-x64.xml'), 'tampered feed');
  await assert.rejects(verifyDesktopBundle(context), /Unsigned Sparkle/);
  await writeFile(join(directory, 'appcast-x64.xml'), originalFeed);
  await writeFile(join(directory, 'release-notes.md'), 'tampered release notes');
  await assert.rejects(verifyDesktopBundle(context), /Release notes/);
  await writeFile(join(directory, 'release-notes.md'), notes);
  await writeFile(join(directory, assets[0].name), 'tampered');
  await assert.rejects(verifyDesktopBundle(context), /artifact digest/);
 } finally { await rm(directory, { recursive: true, force: true }); }
});
