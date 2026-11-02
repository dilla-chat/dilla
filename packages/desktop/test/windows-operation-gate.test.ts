import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, realpath, rm, writeFile, access } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { spawn, execFileSync, spawnSync } from 'node:child_process';
import { randomUUID } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import { createRequire } from 'node:module';
// @ts-expect-error test tooling is JavaScript
import { compileWindowsNSIS } from './support/windows-nsis.mjs';
const require=createRequire(import.meta.url);
const { nsisEscapeString: escape }=require('app-builder-lib/out/targets/nsis/nsisScriptGenerator');
const repository=resolve(import.meta.dirname,'../../..');


function exitOf(child:ReturnType<typeof spawn>):Promise<number|null>{
 if(child.exitCode!==null)return Promise.resolve(child.exitCode);
 return new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',code=>resolve(code));});
}
async function bounded<T>(promise:Promise<T>):Promise<T>{
 let timer:ReturnType<typeof setTimeout>|undefined;
 try{return await Promise.race([promise,new Promise<never>((_,reject)=>{timer=setTimeout(()=>reject(new Error('owned operation fixture timed out')),15000);})]);}
 finally{clearTimeout(timer);}
}

async function compileGate(root:string,appId:string,hold:boolean,crlf:boolean):Promise<string>{
 const source=(await readFile(new URL('../build/windows-installer.nsi',import.meta.url),'utf8')).replaceAll('\r\n','\n');
 const functions=/^!macro GlobalOperationFunctions Prefix\n([\s\S]*?)^!macroend/m.exec(source);
 const free=/^!macro FreeGlobalLocal Variable\n[\s\S]*?^!macroend/m.exec(source);
 assert.ok(functions);assert.ok(free);
 const globals=[...source.matchAll(/^Var (Global\w+)$/gm)].map(match=>match[0]).join('\n');
 const body=functions[1].replaceAll('${Prefix}','');
 return compileWindowsNSIS({directory:root,archive:join(root,'unused.7z'),appId,name:`gate-${hold}-${crlf}.exe`,transform:()=>`
Name "Private Dilla operation gate contract"
RequestExecutionLevel user
SilentInstall silent
!include "LogicLib.nsh"
!define /math DILLA_MUTEX_SA_SIZE \${NSIS_PTR_SIZE} * 3
${globals}
Var OperationMutex
${free[0]}
Function Fail
 Call ReleaseGlobalOperation
 SetErrorLevel 2
 Quit
FunctionEnd
${body}
Section
 Call InitializeOperation
 Call AcquireGlobalOperation
 ${hold?`FileOpen $0 "${escape(join(root,'ready'))}" w
 FileWrite $0 "owned"
 FileClose $0
 StrCpy $1 0
 wait:
 IfFileExists "${escape(join(root,'release'))}" done
 IntOp $1 $1 + 1
 IntCmp $1 400 0 +2 +2
  Call Fail
 Sleep 25
 Goto wait
 done:`:''}
 Call ReleaseGlobalOperation
 SetErrorLevel 0
SectionEnd
`.replaceAll('\n',crlf?'\r\n':'\n')});
}

for(const crlf of [false,true])test(`actual NSIS Global gate compiles from ${crlf?'CRLF':'LF'} source`,async()=>{
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-global-compile-')));
 try{await compileGate(root,'dev.dilla.operation.compile',false,crlf);}finally{await rm(root,{recursive:true,force:true});}
});

test('actual NSIS and native Go gates exclude each other and accept the same existing object security',{skip:process.platform!=='win32',timeout:60000},async()=>{
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-global-native-')));
 const appId='dev.dilla.operation.fixture.'+randomUUID().replaceAll('-','');
 const processes:ReturnType<typeof spawn>[]=[];
 try{
  const helper=join(root,'global.test.exe');
  execFileSync('go',['test','-c','-o',helper,'./packages/desktop/native/windows-launcher'],{cwd:repository,env:{...process.env,CGO_ENABLED:'0'},stdio:'pipe',timeout:30000});
  const held=await compileGate(root,appId,true,false),probe=await compileGate(root,appId,false,true);
  const child=spawn(held,['/S'],{stdio:'ignore',windowsHide:true});processes.push(child);
  for(let i=0;i<200;i++){if(await access(join(root,'ready')).then(()=>true,()=>false))break;assert.equal(child.exitCode,null,'NSIS gate exited before ownership');await delay(25);}
  assert.equal(await readFile(join(root,'ready'),'utf8'),'owned');
  const env=(mode:string)=>({...process.env,DILLA_TEST_GLOBAL_MUTEX_MODE:mode,DILLA_TEST_GLOBAL_MUTEX_APP:appId});
  assert.equal(spawnSync(helper,['-test.run=^TestGlobalOperationMutexProcessHelper$'],{env:env('busy'),timeout:10000,windowsHide:true,stdio:'pipe'}).status,0,'Go must see NSIS-held gate as busy, with trusted existing-object security');
  const released=exitOf(child);
  await writeFile(join(root,'release'),'release');
  assert.equal(await bounded(released),0,'NSIS gate release failed');
  assert.equal(spawnSync(helper,['-test.run=^TestGlobalOperationMutexProcessHelper$'],{env:env('acquire'),timeout:10000,windowsHide:true,stdio:'pipe'}).status,0,'Go must acquire after NSIS releases');
  const owner=spawn(helper,['-test.run=^TestGlobalOperationMutexProcessHelper$'],{env:env('abandon'),windowsHide:true,stdio:['pipe','pipe','pipe']});processes.push(owner);
  await bounded(new Promise<void>((resolve,reject)=>{owner.once('error',reject);let body='';owner.stdout!.on('data',chunk=>{body+=chunk.toString();if(body.includes('owned'))resolve();});owner.once('exit',()=>reject(new Error('Go owner exited before ownership')));}));
  assert.equal(spawnSync(probe,['/S'],{timeout:10000,windowsHide:true,stdio:'pipe'}).status,2,'NSIS must authenticate the Go-held object and refuse contention');
  // Keep this exact Go-created object alive after its owner exits; otherwise
  // the next NSIS process could merely create a fresh object with its own ACL.
  const retained=join(root,'retained');
  const literal=(value:string)=>"'"+value.replaceAll("'","''")+"'";
  const script=`$ErrorActionPreference='Stop'; $sid=[Security.Principal.WindowsIdentity]::GetCurrent().User.Value; $gate=[Threading.Mutex]::OpenExisting(${literal('Global\\Dilla.Install.'+appId+'.')}+$sid); try { [IO.File]::WriteAllText(${literal(retained)},'retained'); [void][Console]::ReadLine() } finally { $gate.Dispose() }`;
  const retainer=spawn('powershell.exe',['-NoLogo','-NoProfile','-NonInteractive','-EncodedCommand',Buffer.from(script,'utf16le').toString('base64')],{windowsHide:true,stdio:['pipe','ignore','ignore']});processes.push(retainer);
  for(let i=0;i<200;i++){if(await access(retained).then(()=>true,()=>false))break;assert.equal(retainer.exitCode,null,'existing-object retainer exited early');await delay(25);}
  assert.equal(await readFile(retained,'utf8'),'retained');
  const ownerExited=exitOf(owner);owner.stdin!.end('\n');assert.equal(await bounded(ownerExited),0);
  assert.equal(spawnSync(probe,['/S'],{timeout:10000,windowsHide:true,stdio:'pipe'}).status,0,'NSIS must authenticate and acquire the retained Go-created abandoned object');
  const retainedExit=exitOf(retainer);retainer.stdin!.end('\n');assert.equal(await bounded(retainedExit),0);
 }finally{
  for(const child of processes)if(child.exitCode===null&&child.signalCode===null){const exited=exitOf(child);child.kill();await bounded(exited);}
  await rm(root,{recursive:true,force:true,maxRetries:5,retryDelay:100});
 }
});
