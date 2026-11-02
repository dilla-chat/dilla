import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as proof from './support/linux-installed-runtime-proof.mjs';
import { mkdtemp, writeFile, symlink, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
const runtime = version => ({
 format:1,version,mainPid:123,rendererPid:456,start:'12345',uid:1000,
 executable:'/tmp/.mount_owned/dilla-desktop',mount:{point:'/tmp/.mount_owned',type:'fuse.Dilla.AppImage'},
 sandbox:true,contextIsolation:true,nodeIntegration:false,
 facts:{mainUid:[1000,1000,1000,1000],rendererUid:[1000,1000,1000,1000],rendererExecutable:'/tmp/.mount_owned/dilla-desktop',rendererCommand:'dilla-desktop --type=renderer',seccomp:2,noNewPrivs:1,capEff:0,descendant:true},
});
const report = () => ({format:1,platform:'linux',arch:'x64',engine:'AppImageUpdater',from:'1.0.0',to:'1.0.1',profileRetained:true,sandbox:[runtime('1.0.0'),{...runtime('1.0.1'),mainPid:789,rendererPid:987,start:'12346'}]});
test('installed AppImage proof requires initial and upgraded mounted kernel observations', () => {
 assert.equal(typeof proof.verifyInstalledRuntimeProof,'function');
 assert.equal(proof.verifyInstalledRuntimeProof(report()),true);
 for(const change of [
  r=>{delete r.sandbox;},r=>r.sandbox.pop(),r=>{r.sandbox[1].version='1.0.0';},r=>{r.sandbox[1].mainPid=123;},
  r=>{r.platform='win32';},r=>{r.engine='diagnostic';},r=>{r.profileRetained=false;},
 ]){const r=report();change(r);assert.throws(()=>proof.verifyInstalledRuntimeProof(r),/installed AppImage/);}
 for(const change of [
  r=>{r.sandbox=false;},r=>{r.contextIsolation=false;},r=>{r.nodeIntegration=true;},r=>{r.uid=0;},
  r=>{r.mainPid='123';},r=>{r.rendererPid='../1';},r=>{r.start=null;},r=>{r.mount.type='ext4';},
  r=>{r.mount.point='/tmp/other';},r=>{r.executable='/tmp/linux-unpacked/dilla-desktop';},
  r=>{r.facts.seccomp=0;},r=>{r.facts.noNewPrivs=0;},r=>{r.facts.capEff=1;},
  r=>{r.facts.descendant=false;},r=>{r.facts.rendererExecutable='/tmp/other/dilla-desktop';},
  r=>{r.facts.rendererCommand='dilla-desktop --type=gpu-process';},r=>{r.facts.mainUid[1]=0;},
  r=>{r.facts.rendererUid[2]=1001;},r=>{r.facts.rendererUid=[];},
  r=>{r.executable='.mount_owned/dilla-desktop';r.mount.point='.mount_owned';r.facts.rendererExecutable=r.executable;},
 ])for(const index of [0,1]){const r=report();change(r.sandbox[index]);assert.throws(()=>proof.verifyInstalledRuntimeProof(r),/installed AppImage/);}
});
test('installed fixture accepts its libfuse basename subtype and rejects unrelated FUSE mounts', () => {
 const installed = report();
 for (const runtime of installed.sandbox) runtime.mount.type = 'fuse.Dilla.AppImage';
 assert.equal(proof.verifyInstalledRuntimeProof(installed), true);
 for (const type of ['fuse.sshfs', 'fuse.Other.AppImage', 'fuse.AppImage', 'fuse']) for (const index of [0, 1]) {
  const rejected = structuredClone(installed); rejected.sandbox[index].mount.type = type;
  assert.throws(() => proof.verifyInstalledRuntimeProof(rejected), /installed AppImage/);
 }
});
test('installed runtime observer rejects unsafe or mismatched PID identities before proc access', async () => {
 assert.equal(typeof proof.observeInstalledRuntime,'function');
 for(const r of [{mainPid:'../../1',rendererPid:2},{mainPid:2,rendererPid:0},{mainPid:2,rendererPid:NaN},{mainPid:2,rendererPid:2},{mainPid:4,rendererPid:5}])
  await assert.rejects(proof.observeInstalledRuntime({format:1,version:'1.0.0',sandbox:true,contextIsolation:true,nodeIntegration:false,...r},{pid:r.mainPid===4?3:r.mainPid,start:'123',version:'1.0.0'}),/installed AppImage/);
});

test('installed runtime proof reader rejects symlinked, oversized, empty and malformed reports', async () => {
 const root = await mkdtemp(join(tmpdir(),'dilla-sandbox-proof-'));
 try {
  const path = join(root,'proof.json');
  await writeFile(path,JSON.stringify(report()));assert.equal(proof.verifyInstalledRuntimeProof(await proof.readRuntimeReport(path)),true);
  await symlink(path,join(root,'link.json'));await assert.rejects(proof.readRuntimeReport(join(root,'link.json')));
  for(const bytes of ['', 'not json', ' '.repeat(16385)]){await writeFile(path,bytes);await assert.rejects(proof.readRuntimeReport(path));}
  await assert.rejects(proof.readRuntimeReport(root));
 } finally {await rm(root,{recursive:true,force:true});}
});
