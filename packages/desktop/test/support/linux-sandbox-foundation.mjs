import { createRequire } from 'node:module';
import { spawn, execFileSync } from 'node:child_process';
import { readFile, realpath } from 'node:fs/promises';
import { resolve } from 'node:path';
const require=createRequire(import.meta.url);
export const FIXTURE_HELPER='/usr/lib/dilla-sandbox-foundation-ci/chrome-sandbox';
export const FOUNDATION_TIMEOUT=10_000;
const ackKeys='contextIsolated,deniedRequests,execPath,format,kind,mainPid,mode,nonce,rendererPid,sandboxed';
export function foundationResult({ack,facts,expected,code,signal,stderr='',timedOut=false}) {
 if(!expected || !Number.isSafeInteger(expected.uid) || expected.uid<1 || !Number.isSafeInteger(expected.mainPid) || expected.mainPid<2)return {status:'failed'};
 if(timedOut)return {status:'timed-out'};
 if(ack && typeof ack==='object' && !Array.isArray(ack) && Object.keys(ack).sort().join(',')===ackKeys
   && ack.format===1 && ack.kind==='sandbox-foundation' && ack.mode===expected.mode && ack.nonce===expected.nonce
   && ack.mainPid===expected.mainPid && Number.isSafeInteger(ack.rendererPid) && ack.rendererPid>1
   && ack.execPath===expected.execPath && ack.sandboxed===true && ack.contextIsolated===true && ack.deniedRequests===1
   && code===0 && !signal && facts && facts.mainUid?.every(x=>x===expected.uid) && facts.mainUid.length===4
   && facts.rendererUid?.every(x=>x===expected.uid) && facts.rendererUid.length===4 && expected.uid!==0
   && facts.rendererExecutable===expected.execPath && /(?:^|\s)--type=renderer(?:\s|$)/.test(facts.rendererCommand)
   && facts.seccomp===2 && facts.noNewPrivs===1 && facts.descendant===true && facts.capEff===0)
  return {status:'sandboxed'};
 // This classification is feasibility evidence only, never an elevation API.
 // A normal error/forged string cannot qualify; require Chromium's pinned fatal
 // source/message and a signal death of our directly owned immutable executable.
 if(!ack && ['SIGABRT','SIGTRAP'].includes(signal) && typeof stderr==='string' && stderr.length<=65536
   && /FATAL:.*(?:setuid_sandbox_host\.cc:\d+.*SUID sandbox helper binary was found, but is not configured correctly|zygote_host_impl_linux\.cc:\d+.*No usable sandbox!)/.test(stderr))
  return {status:'sandbox-unavailable'};
 return {status:'failed'};
}
async function status(pid) {
 const text=await readFile('/proc/'+pid+'/status','utf8'), fields=Object.fromEntries(text.split('\n').filter(x=>x.includes(':')).map(x=>{const i=x.indexOf(':');return[x.slice(0,i),x.slice(i+1).trim()];}));
 return {uid:fields.Uid.split(/\s+/).map(Number),ppid:Number(fields.PPid),seccomp:Number(fields.Seccomp),noNewPrivs:Number(fields.NoNewPrivs),capEff:Number.parseInt(fields.CapEff,16)};
}
// Chromium makes a sandboxed renderer nondumpable. CI may read only this
// numeric owned process's executable through a fixed, bounded read-only command;
// it never changes dumpability or any other host security policy.
export async function inspectRendererExecutable(pid, read=realpath, privileged=path=>execFileSync('/usr/bin/sudo',['-n','--','/usr/bin/readlink','--',path],{encoding:'utf8',timeout:2000,maxBuffer:4096}).trimEnd()) {
 if(!Number.isSafeInteger(pid)||pid<2)throw Object.assign(Error('invalid observed process'),{code:'EINVAL'});
 const path='/proc/'+pid+'/exe';
 try{return await read(path);}catch(error){
  if(!['EACCES','EPERM'].includes(error.code)||process.env.GITHUB_ACTIONS!=='true')throw error;
  return privileged(path);
 }
}
async function kernelFacts(mainPid,rendererPid) {
 const [main,renderer,command,mainLabel]=await Promise.all([status(mainPid),status(rendererPid),readFile('/proc/'+rendererPid+'/cmdline','utf8'),readFile('/proc/'+mainPid+'/attr/current','utf8')]);
 let cursor=rendererPid,descendant=false;
 for(let i=0;i<8&&cursor>1;i++){const current=await status(cursor);cursor=current.ppid;if(cursor===mainPid){descendant=true;break;}}
 if(!descendant||!main.uid.every(uid=>uid===process.getuid())||!renderer.uid.every(uid=>uid===process.getuid())||process.getuid()===0)throw Object.assign(Error('foreign observed process'),{code:'EPERM'});
 const rendererExecutable=await inspectRendererExecutable(rendererPid);
 return {mainUid:main.uid,rendererUid:renderer.uid,rendererExecutable,rendererCommand:command.replaceAll('\0',' '),seccomp:renderer.seccomp,noNewPrivs:renderer.noNewPrivs,capEff:renderer.capEff,descendant,mainLabel:mainLabel.trim()};
}
export async function runMountedProbe({mount,image,profile,mode,nonce}) {
 const env={...process.env,APPDIR:mount,APPIMAGE:image};
 for(const key of ['CHROME_DEVEL_SANDBOX','ELECTRON_RUN_AS_NODE','NODE_OPTIONS','APPIMAGE_EXTRACT_AND_RUN','APPIMAGE_EXTRACT_AND_RUN_NO_CLEANUP'])delete env[key];
 const child=spawn(resolve(mount,'AppRun'),['--foundation-mode='+mode,'--foundation-nonce='+nonce,'--foundation-profile='+profile],{env,detached:true,stdio:['ignore','ignore','pipe','pipe','pipe']});
 const expected={nonce,mode,mainPid:child.pid,execPath:resolve(mount,'dilla-desktop'),uid:process.getuid()};
 let stderr='',report='',ack,facts,observationError,invalid=false,timedOut=false;
 const timer=setTimeout(()=>{timedOut=true;try{process.kill(-child.pid,'SIGKILL');}catch{}},FOUNDATION_TIMEOUT);
 child.stderr.on('data',chunk=>{stderr+=chunk.toString();if(stderr.length>65536){invalid=true;try{process.kill(-child.pid,'SIGKILL');}catch{}}});
 const reportDone=new Promise(resolveReport=>{
  let started=false;
  child.stdio[3].on('data',async chunk=>{
   report+=chunk.toString();
   if(report.length>4096||started){invalid=true;try{process.kill(-child.pid,'SIGKILL');}catch{};return;}
   if(!report.endsWith('\n'))return;
   started=true;
   try{ack=JSON.parse(report);if(!Number.isSafeInteger(ack.rendererPid)||ack.rendererPid<2||ack.mainPid!==child.pid)throw Error('invalid owned process');facts=await kernelFacts(child.pid,ack.rendererPid);child.stdio[4].end(Buffer.from([1]));}
   catch(error){observationError={phase:'kernel-observation',code:['EACCES','EPERM','ENOENT','ESRCH','EINVAL','ETIMEDOUT'].includes(error.code)?error.code:'UNKNOWN'};invalid=true;try{process.kill(-child.pid,'SIGKILL');}catch{}}
   finally{resolveReport();}
  });
  child.once('exit',()=>resolveReport());
 });
 try{
  const {code,signal}=await new Promise((resolveExit,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>resolveExit({code,signal}));});
  await reportDone;
  const result=invalid?{status:'failed'}:foundationResult({ack,facts,expected,code,signal,stderr,timedOut});
  return {...result,code,signal,facts,stderr,observationError};
 }finally{clearTimeout(timer);for(const stream of child.stdio)stream?.destroy();}
}
export async function buildFoundationImage(project,out,admissionResources) {
 const {build,Platform,Arch}=require('electron-builder');
 const production=require('../../electron-builder.cjs');
 const safeHook=require('../../scripts/linux-appimage-launcher.cjs');
 const fs=await import('node:fs/promises');
 await build({targets:Platform.LINUX.createTarget(['AppImage'],Arch.x64),publish:'never',projectDir:project,config:{
  appId:'dev.dilla.sandbox.foundation.fixture',productName:'DillaSandboxFoundation',executableName:'dilla-desktop',
  electronVersion:require('electron/package.json').version,electronDist:resolve(import.meta.dirname,'../../../../node_modules/electron/dist'),
  directories:{output:out},files:['main.cjs','preload.cjs','probe.html','admission.cjs','package.json'],asar:true,
  extraResources:admissionResources ? [{from:admissionResources,to:'admission-proof'}] : [],
  electronFuses:production.electronFuses,linux:{target:['AppImage'],category:'Utility',icon:resolve(import.meta.dirname,'../../assets/icon.png')},
  artifactName:'dilla-sandbox-foundation.AppImage',appImage:{executableArgs:[]},
  afterPack:async context=>{await safeHook(context);await fs.rm(resolve(context.appOutDir,'chrome-sandbox'));await fs.symlink(FIXTURE_HELPER,resolve(context.appOutDir,'chrome-sandbox'));}
 }});
 return resolve(out,'dilla-sandbox-foundation.AppImage');
}
