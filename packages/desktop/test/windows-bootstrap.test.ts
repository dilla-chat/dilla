import{test}from'node:test';import assert from'node:assert/strict';import{writeFile,readFile,open,stat}from'node:fs/promises';import{join}from'node:path';
import{readWindowsSlots}from'../src/windows-installation.ts';
import{windowsInstalledFixture}from'./support/windows-installed-update.ts';import{runWindowsBootstrap}from'../src/update-runtime.ts';import{readUpdateJournal}from'../src/update-journal.ts';
for(const fault of ['manifest','program','slot-read'])test('a published Windows commit never falls back to old after '+fault+' failure',{skip:process.platform==='win32'},async t=>windowsInstalledFixture(async({plan,trust,nextProgram,nextManifest,publish})=>{
 await publish();let restore=()=>{};
 if(fault==='manifest')await writeFile(nextManifest,'corrupted published manifest');
 if(fault==='program')await writeFile(join(nextProgram,'resources/app.asar'),'corrupted after account migration');
 if(fault==='slot-read'){
  const target=await stat(join(plan.userData,'updates/windows-active-1.json')),probe=await open(nextManifest,'r'),prototype=Object.getPrototypeOf(probe),original=prototype.read;await probe.close();
  t.mock.method(prototype,'read',async function(this:{stat():Promise<{ino:number;dev:number}>},...args:unknown[]){const value=await this.stat();if(value.ino===target.ino&&value.dev===target.dev)throw Object.assign(new Error('published slot unreadable'),{code:'EIO'});return original.apply(this,args);});restore=()=>t.mock.restoreAll();
 }
 try{if(fault!=='slot-read')await assert.rejects(readWindowsSlots(plan.windows!.root,{publicKeys:trust.publicKeys,channel:trust.channel,arch:trust.arch,initialVersion:plan.windows!.initialVersion,initialManifestSha512:plan.windows!.initialManifestSha512},join(plan.userData,'updates')),/manifest|tree|hash|changed|JSON/i);await assert.rejects(runWindowsBootstrap(plan.userData,plan.installed,plan.health.nonce,trust,()=>true),{name:'UpdateStartupHeld'});}
 finally{restore();}
 assert.equal((await readUpdateJournal(plan)).phase,'complete');assert.ok(await readFile(join(plan.userData,'updates/windows-active-1.json')));
}));

test('durable complete before slot publication delegates the verified old app when failed status writes remain ENOSPC',{skip:process.platform!=='linux'},async t=>windowsInstalledFixture(async({plan,trust,root})=>{
 const{readlink}=await import('node:fs/promises'),probe=await open(join(root,'probe'),'w'),prototype=Object.getPrototypeOf(probe),original=prototype.writeFile;await probe.close();let injected=0;const calls:string[]=[];
 t.mock.method(prototype,'writeFile',async function(this:{fd:number},...args:unknown[]){if((await readlink('/proc/self/fd/'+this.fd)).includes('/journal.json.')){injected++;throw Object.assign(new Error('status volume full'),{code:'ENOSPC'});}return original.apply(this,args);});
 try{
  const code=await runWindowsBootstrap(plan.userData,plan.installed,plan.health.nonce,trust,()=>{calls.push('lock');return true;},()=>({
   waitParent:async()=>{},installWindows:async()=>assert.fail('no reinstall'),restoreWindowsMetadata:async()=>assert.fail('no registration rollback'),stopInstaller:async()=>{calls.push('installer-drained');},recoverLaunches:async()=>[],recoverChild:async()=>null,stop:async()=>assert.fail('no live child'),launch:async file=>{assert.equal(file,plan.installed);calls.push('old');return{pid:process.pid+100,exitCode:null,signalCode:null};},
  }));
  assert.equal(code,21);assert.equal(injected,1);assert.deepEqual(calls,['lock','installer-drained','old']);assert.equal((await readUpdateJournal(plan)).phase,'complete');
 }finally{t.mock.restoreAll();}
}));

test('a recovered nonce cannot select old account code after the new Windows slot committed',{skip:process.platform==='win32'},async()=>windowsInstalledFixture(async({plan,trust,publish})=>{
 await publish();const{verifyRestoredLaunch}=await import('../src/update-runtime.ts');await assert.rejects(verifyRestoredLaunch(plan.userData,plan.installed,plan.health.nonce,trust),/restored Windows slot/);assert.equal((await readUpdateJournal(plan)).phase,'complete');
}));

test('a recovered nonce cannot hide a published newer slot behind a slot read error',{skip:process.platform==='win32'},async t=>windowsInstalledFixture(async({plan,trust,nextManifest,publish})=>{
 await publish();const target=await stat(join(plan.userData,'updates/windows-active-1.json')),probe=await open(nextManifest,'r'),prototype=Object.getPrototypeOf(probe),original=prototype.read;await probe.close();
 t.mock.method(prototype,'read',async function(this:{stat():Promise<{ino:number;dev:number}>},...args:unknown[]){const value=await this.stat();if(value.ino===target.ino&&value.dev===target.dev)throw Object.assign(new Error('published slot EIO'),{code:'EIO'});return original.apply(this,args);});
 const{verifyRestoredLaunch}=await import('../src/update-runtime.ts');try{await assert.rejects(verifyRestoredLaunch(plan.userData,plan.installed,plan.health.nonce,trust));}finally{t.mock.restoreAll();}assert.equal((await readUpdateJournal(plan)).phase,'complete');
}));
