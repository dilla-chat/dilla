import {test} from 'node:test';
import assert from 'node:assert/strict';
import {readFile,writeFile,rm,symlink,readlink,mkdtemp,cp,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {inspectExtracted,inspectAppImage} from '../scripts/inspect-linux-sandbox-package.mjs';

const image=process.env.DILLA_INSPECT_FIXTURE_IMAGE;
const sidecar=process.env.DILLA_INSPECT_FIXTURE_CAPSULE;
const manifest=process.env.DILLA_INSPECT_FIXTURE_MANIFEST;
const extracted=process.env.DILLA_INSPECT_FIXTURE_ROOT;
const active=Boolean(image&&sidecar&&manifest&&extracted);
test('final AppImage inspection derives identical public proof from extracted artifact',{skip:!active},async()=>{
 const proof=await inspectAppImage({image, capsuleFile:sidecar, manifestFile:manifest});
 assert.equal(proof.capsuleSha512,proof.embeddedCapsuleSha512);
 assert.equal(proof.manifestSha512,proof.embeddedManifestSha512);
});
test('final extraction refuses mutated sidecars, ELF, ASAR, launcher, symlink, desktop, and forged or absent proof',{skip:!active},async()=>{
 const root=await mkdtemp(join(tmpdir(),'dilla-inspect-mutation-'));
 try {
  await mkdir(join(root,'resources'));
  const selected=['AppRun','chrome-sandbox','dilla-desktop','dilla-sandbox-launcher','dev.dilla.desktop.desktop','resources/app.asar','resources/linux-sandbox-build.json'];
  for(const name of selected) await cp(join(extracted,name),join(root,name),{recursive:true,force:true,verbatimSymlinks:true});
  const raw=await readFile(sidecar), meta=await readFile(manifest);
  const check=()=>inspectExtracted({directory:root,capsule:raw,manifest:meta});
  await check();
  for (const name of ['AppRun','dilla-desktop','resources/app.asar','dilla-sandbox-launcher','dev.dilla.desktop.desktop','resources/linux-sandbox-build.json']) {
   const path=join(root,name), original=await readFile(path); const changed=name.endsWith('.desktop') ? Buffer.from(original.toString().replace('Exec=AppRun %U','Exec=AppRun --no-sandbox %U')) : Buffer.from(original); if(!name.endsWith('.desktop')) changed[changed.length-1]^=1; await writeFile(path,changed);
   await assert.rejects(check,undefined,name); await writeFile(path,original);
  }
  const original=await readlink(join(root,'chrome-sandbox'));await rm(join(root,'chrome-sandbox'));await symlink('/tmp/wrong',join(root,'chrome-sandbox'));await assert.rejects(check);await rm(join(root,'chrome-sandbox'));await symlink(original,join(root,'chrome-sandbox'));
  const bad=Buffer.from(raw);bad[bad.length-1]^=1;await assert.rejects(inspectExtracted({directory:root,capsule:bad,manifest:meta}));
  const badManifest=Buffer.from(meta);badManifest[badManifest.length-1]^=1;await assert.rejects(inspectExtracted({directory:root,capsule:raw,manifest:badManifest}));
  const {flipFuses,FuseVersion,FuseV1Options}=await import('@electron/fuses');
  const {createHash}=await import('node:crypto'); const elf=join(root,'dilla-desktop'), before=await readFile(elf), proofPath=join(root,'resources/linux-sandbox-build.json'), proof=await readFile(proofPath);
  await flipFuses(elf,{version:FuseVersion.V1,[FuseV1Options.RunAsNode]:true});
  const forged=JSON.parse(proof);forged.electronSha512=createHash('sha512').update(await readFile(elf)).digest('base64');await writeFile(proofPath,JSON.stringify(forged));
  await assert.rejects(check,/fuse/i);await writeFile(elf,before);await writeFile(proofPath,proof);
  await rm(proofPath);await assert.rejects(check);
 } finally {await rm(root,{recursive:true,force:true});}
});
