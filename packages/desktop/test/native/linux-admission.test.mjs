// Called only by the existing mounted foundation test while its owned FUSE
// image and (in the managed lane) the existing fixture helper remain alive.
import assert from 'node:assert/strict';
import {spawn,spawnSync} from 'node:child_process';
import {mkdir,writeFile,access,mkdtemp,realpath,stat,readFile} from 'node:fs/promises';
import {join,resolve,isAbsolute} from 'node:path';
import {randomBytes} from 'node:crypto';
import {readFileSync} from 'node:fs';

// Closing the final control write also releases a pending fs.read in the child.
export function sendAdmissionExit(stream) { stream.end('exit\n'); }
export async function makeAdmissionRoot(runnerTemp) {
 if(typeof runnerTemp!=='string' || !isAbsolute(runnerTemp) || await realpath(runnerTemp)!==runnerTemp)throw Error('invalid runner temporary directory');
 const info=await stat(runnerTemp);if(!info.isDirectory() || info.uid!==process.getuid())throw Error('unsafe runner temporary directory');
 return mkdtemp(join(runnerTemp,'dilla-sandbox-foundation-'));
}
export async function waitOwnedChildGone(pid,read=readFile) {
 for(let i=0;i<100;i++){
  try {const info=await read('/proc/'+pid+'/stat','utf8');if(/^\d+ \(.*\) Z /.test(info))return;}
  catch(error){if(['ENOENT','ESRCH'].includes(error.code))return;throw error;}
  await new Promise(done=>setTimeout(done,50));
 }throw Error('owned child cleanup timed out');
}
export async function cleanupAdmissionGroup(child,observedChild,diagnostic) {
 if(!child?.pid)return;
 if(diagnostic)diagnostic.stage=1;
 try {process.kill(-child.pid,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}
 if(diagnostic)diagnostic.stage=2;
 if(child.exitCode===null && child.signalCode===null)await deadline(new Promise((done,reject)=>{child.once('exit',done);child.once('error',reject);}), 'owned main cleanup',5000);
 if(diagnostic)diagnostic.stage=3;
 if(observedChild)await waitOwnedChildGone(observedChild);
 if(diagnostic)diagnostic.stage=4;
}

const waitExit = child => new Promise((done,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>done({code,signal}));});
const deadline = (promise,label,ms=15000) => Promise.race([promise,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error(label+' timed out')),ms);timer.unref();})]);
// Inspect only the directly spawned main, after pinning its UID/start time.
function mainProcessFacts(pid,start) {
 try {
  const status=readFileSync('/proc/'+pid+'/status','utf8');
  const uid=/^Uid:\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)\s+([0-9]+)/m.exec(status);
  const owner=uid?.slice(1).every(x=>Number(x)===process.getuid())?1:0;
  const parts=readFileSync('/proc/'+pid+'/stat','utf8').split(') ')[1].split(' ');
  return {mainPresent:1,mainOwner:owner,mainIdentity:parts[19]===start?1:0,mainState:({R:1,S:2,D:3,Z:4,T:5,t:5})[parts[0]]??6,mainReadError:0};
 } catch(error) {return {mainPresent:error?.code==='ENOENT'?0:null,mainOwner:null,mainIdentity:null,mainState:null,mainReadError:error?.code==='ENOENT'?1:error?.code==='EACCES'?2:error?.code==='EPERM'?3:4};}
}
// Inputs are owned test processes and the fixed gate. Expose only bounded
// phase labels and numeric observations, never process paths or stderr.
export async function checkAdmissionRelease({exit,exclusive,readChild,captureMain=()=>({}),pause=()=>new Promise(done=>setTimeout(done,100)),facts}) {
 facts.phase='release-main-wait';let result;
 try{result=await deadline(exit,'main exit');}catch(error){Object.assign(facts,captureMain());throw error;}
 facts.phase='release-main-result';facts.mainCode=Number.isInteger(result.code)?result.code:null;facts.mainSignal=result.signal===null?0:1;
 assert.deepEqual(result,{code:0,signal:null});
 facts.phase='release-lock-probe';
 for(let i=0;i<50;i++){facts.attempts=i+1;facts.lockStatus=exclusive();if(facts.lockStatus===0)break;await pause();}
 facts.phase='release-lock-busy';assert.equal(facts.lockStatus,0);
 facts.phase='release-child-read';let child;
 try{child=await readChild();}catch(error){facts.childReadError=error?.code==='ENOENT'?1:error?.code==='EACCES'?2:error?.code==='EPERM'?3:4;throw error;}
 facts.childState=child.live?1:0;facts.childIdentity=child.identity?1:0;
 facts.phase='release-child-dead';assert.equal(child.live,true);
 facts.phase='release-child-identity';assert.equal(child.identity,true);
 facts.phase='release-complete';
}
// Fixed checkpoints distinguish publication, late exit, profile and lock.
export async function checkAdmissionPending({publish,launch,profile,exclusive,facts}) {
 facts.phase='pending-publish';facts.pendingPublish=publish();
 facts.phase='pending-publish-result';assert.equal(facts.pendingPublish,0);
 facts.phase='pending-launch';const late=launch();
 facts.phase='pending-exit-wait';let result;
 try{result=await deadline(late.exit,'pending launch');}finally{facts.lateAdmissionFlag=late.admissionFailure?.()?1:0;}
 facts.phase='pending-exit-result';facts.lateCode=Number.isInteger(result.code)?result.code:null;facts.lateSignal=result.signal===null?0:1;
 assert.deepEqual(result,{code:40,signal:null});
 facts.phase='pending-profile-read';
 try{await profile();facts.profilePresent=1;}catch(error){
  facts.profileError=error?.code==='ENOENT'?1:error?.code==='EACCES'?2:error?.code==='EPERM'?3:4;
  if(error?.code!=='ENOENT')throw error;facts.profilePresent=0;
 }
 facts.phase='pending-profile-present';assert.equal(facts.profilePresent,0);
 facts.phase='pending-lock-probe';facts.pendingLock=exclusive();
 facts.phase='pending-lock-result';assert.equal(facts.pendingLock,0);
 facts.phase='pending-complete';
}
export async function runAdmissionProof({mount,image,root}) {
 assert.equal(process.env.GITHUB_ACTIONS,'true');
 const gate=join(root,'admission-base','gate'),lock=join(gate,'lock');
 const digest=Buffer.alloc(64).toString('base64');
 const ready={format:1,manager:'deb',state:'ready',transaction:'a'.repeat(64),identitySha512:digest,alpmHooks:null};
 await mkdir(gate,{mode:0o755});await writeFile(lock,'',{mode:0o644});
 const actor=join(root,'admission-status-actor.cjs');
 // Literal gate and status are baked into the private actor. The sole operation
 // runs under flock EX and can only publish the fixed pending record.
 await writeFile(actor,`const fs=require('node:fs');if(process.argv.length!==2)process.exit(2);fs.writeFileSync(${JSON.stringify(join(gate,'status.json'))},${JSON.stringify(JSON.stringify({...ready,state:'pending'}))},{mode:0o644});`);
 await writeFile(join(gate,'status.json'),JSON.stringify(ready),{mode:0o644});
 const runExclusive=command=>{const result=spawnSync('flock',['--exclusive','--nonblock',lock,...command],{timeout:3000});
  if(result.error || result.signal || ![0,1].includes(result.status))throw Error('exclusive actor failed');return result.status;};
 const exclusive=()=>runExclusive(['true']);
 const publishPending=()=>runExclusive([process.execPath,actor]);
 const env={...process.env,APPDIR:mount,APPIMAGE:image};
 for(const name of ['CHROME_DEVEL_SANDBOX','ELECTRON_RUN_AS_NODE','NODE_OPTIONS','APPIMAGE_EXTRACT_AND_RUN','APPIMAGE_EXTRACT_AND_RUN_NO_CLEANUP'])delete env[name];
 const launch=profile=>{
  const nonce=randomBytes(32).toString('hex');
  const child=spawn(resolve(mount,'dilla-desktop'),['--admission-mode=proof','--admission-nonce='+nonce,'--admission-profile='+profile],{env,detached:true,stdio:['ignore','ignore','pipe','pipe','pipe']});
  const exit=waitExit(child);let output='',stderr='',reports=[],waiter;
  child.stderr.on('data',chunk=>{stderr+=chunk.toString();if(stderr.length>4096)try{process.kill(-child.pid,'SIGKILL');}catch{}});
  child.stdio[3].on('data',chunk=>{output+=chunk.toString();if(output.length>4096){try{process.kill(-child.pid,'SIGKILL');}catch{}return;}let i;
   while((i=output.indexOf('\n'))>=0){let report;try{report=JSON.parse(output.slice(0,i));}catch{report=null;}output=output.slice(i+1);if(report && Object.keys(report).sort().join(',')==='format,nonce,phase,pid' && report.format===1 && report.nonce===nonce && report.pid===child.pid && ['exit-command','exit-call'].includes(report.phase)){if(report.phase==='exit-command')facts.controlCommand=1;else facts.controlExit=1;continue;}if(waiter){const done=waiter;waiter=null;done(report);}else reports.push(report);}
  });
  return {child,exit,nonce,report:()=>deadline(reports.length?Promise.resolve(reports.shift()):new Promise(done=>{waiter=done;}),'admission phase'),admissionFailure:()=>stderr.includes('DILLA_ADMISSION_PROOF_FAILURE admission\n')};
 };
 let main,late,descendant,failed=false;const facts={phase:'window',mainCode:null,mainSignal:null,attempts:0,lockStatus:null,childState:null,childIdentity:null,childReadError:0,cleanupError:0,primaryFailure:0,controlCommand:0,controlExit:0,mainPresent:null,mainOwner:null,mainIdentity:null,mainState:null,mainReadError:0,pendingPublish:null,lateCode:null,lateSignal:null,lateAdmissionFlag:0,profilePresent:null,profileError:0,pendingLock:null,cleanupMainStage:0,cleanupMainError:0,cleanupLateStage:0,cleanupLateError:0};
 try {
  assert.equal(exclusive(),0);main=launch(join(root,'admission-profile'));
  const check=async phase=>assert.deepEqual(await main.report(),{format:1,nonce:main.nonce,phase,pid:main.child.pid});
  await check('window');assert.equal(exclusive(),1);assert.equal(publishPending(),1);
  facts.phase='tray';main.child.stdio[4].write('tray\n');
  const tray=await main.report(),candidate=tray?.childPid;
  assert.deepEqual(tray,{format:1,nonce:main.nonce,phase:'tray',pid:main.child.pid,childPid:candidate});
  assert.ok(Number.isSafeInteger(candidate)&&candidate>1);
  const childStatus=await readFile('/proc/'+candidate+'/status','utf8');
  assert.match(childStatus,new RegExp('^PPid:\\s*'+main.child.pid+'$', 'm'));
  descendant=candidate;
  const startTime=(await readFile('/proc/'+descendant+'/stat','utf8')).split(') ')[1].split(' ')[19];
  assert.equal(exclusive(),1);assert.equal(publishPending(),1);
  const mainStart=(await readFile('/proc/'+main.child.pid+'/stat','utf8')).split(') ')[1].split(' ')[19];
  const initialMain=mainProcessFacts(main.child.pid,mainStart);assert.equal(initialMain.mainOwner,1);assert.equal(initialMain.mainIdentity,1);
  facts.phase='release-command';sendAdmissionExit(main.child.stdio[4]);
  await checkAdmissionRelease({exit:main.exit,exclusive,facts,captureMain:()=>mainProcessFacts(main.child.pid,mainStart),readChild:async()=>{
   const afterStat=(await readFile('/proc/'+descendant+'/stat','utf8')).split(') ')[1].split(' ');
   return {live:afterStat[0]!=='Z',identity:afterStat[19]===startTime};
  }});
  const lateProfile=join(root,'admission-late-profile');
  await checkAdmissionPending({publish:publishPending,launch:()=>{late=launch(lateProfile);return late;},profile:()=>access(lateProfile),exclusive,facts});
  return {format:1,status:'passed',directElf:true,windowBusy:true,trayBusy:true,pendingRefusedBeforeProfile:true,lockReleasedWithSpawnedChildAlive:true};
 } catch { failed=true;facts.primaryFailure=1; } finally {
  const mainCleanup={stage:0},lateCleanup={stage:0};
  const errorCode=error=>error?.code==='ESRCH'?1:error?.code==='ENOENT'?2:error?.code==='EACCES'?3:error?.code==='EPERM'?4:5;
  try{await cleanupAdmissionGroup(main?.child,descendant,mainCleanup);}catch(error){facts.cleanupError|=1;facts.cleanupMainError=errorCode(error);}finally{facts.cleanupMainStage=mainCleanup.stage;}
  try{await cleanupAdmissionGroup(late?.child,undefined,lateCleanup);}catch(error){facts.cleanupError|=2;facts.cleanupLateError=errorCode(error);}finally{facts.cleanupLateStage=lateCleanup.stage;}
  if(failed || facts.cleanupError)throw Error('admission proof failed: '+JSON.stringify(facts));
 }
}
