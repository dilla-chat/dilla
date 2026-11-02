// Called only by the existing mounted foundation test while its owned FUSE
// image and (in the managed lane) the existing fixture helper remain alive.
import assert from 'node:assert/strict';
import {spawn,spawnSync} from 'node:child_process';
import {mkdir,writeFile,access,mkdtemp,realpath,stat,readFile} from 'node:fs/promises';
import {join,resolve,isAbsolute} from 'node:path';
import {randomBytes} from 'node:crypto';

export async function makeAdmissionRoot(runnerTemp) {
 if(typeof runnerTemp!=='string' || !isAbsolute(runnerTemp) || await realpath(runnerTemp)!==runnerTemp)throw Error('invalid runner temporary directory');
 const info=await stat(runnerTemp);if(!info.isDirectory() || info.uid!==process.getuid())throw Error('unsafe runner temporary directory');
 return mkdtemp(join(runnerTemp,'dilla-sandbox-foundation-'));
}
export async function cleanupAdmissionGroup(child,observedChild) {
 if(!child?.pid)return;
 try {process.kill(-child.pid,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}
 if(child.exitCode===null && child.signalCode===null)await deadline(new Promise((done,reject)=>{child.once('exit',done);child.once('error',reject);}), 'owned main cleanup',5000);
 if(observedChild){for(let i=0;i<100;i++){
  try {const info=await readFile('/proc/'+observedChild+'/stat','utf8');if(/^\d+ \(.*\) Z /.test(info))return;}
  catch(error){if(error.code==='ENOENT')return;throw error;}
  await new Promise(done=>setTimeout(done,50));
 }throw Error('owned child cleanup timed out');}
}

const waitExit = child => new Promise((done,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>done({code,signal}));});
const deadline = (promise,label,ms=15000) => Promise.race([promise,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error(label+' timed out')),ms);timer.unref();})]);
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
   while((i=output.indexOf('\n'))>=0){let report;try{report=JSON.parse(output.slice(0,i));}catch{report=null;}output=output.slice(i+1);if(waiter){const done=waiter;waiter=null;done(report);}else reports.push(report);}
  });
  return {child,exit,nonce,report:()=>deadline(reports.length?Promise.resolve(reports.shift()):new Promise(done=>{waiter=done;}),'admission phase'),stderr:()=>stderr};
 };
 let main,late,descendant,phase='window';
 try {
  assert.equal(exclusive(),0);main=launch(join(root,'admission-profile'));
  const check=async phase=>assert.deepEqual(await main.report(),{format:1,nonce:main.nonce,phase,pid:main.child.pid});
  await check('window');assert.equal(exclusive(),1);assert.equal(publishPending(),1);
  phase='tray';main.child.stdio[4].write('tray\n');
  const tray=await main.report(),candidate=tray?.childPid;
  assert.deepEqual(tray,{format:1,nonce:main.nonce,phase:'tray',pid:main.child.pid,childPid:candidate});
  assert.ok(Number.isSafeInteger(candidate)&&candidate>1);
  const childStatus=await readFile('/proc/'+candidate+'/status','utf8');
  assert.match(childStatus,new RegExp('^PPid:\\s*'+main.child.pid+'$', 'm'));
  descendant=candidate;
  const startTime=(await readFile('/proc/'+descendant+'/stat','utf8')).split(') ')[1].split(' ')[19];
  assert.equal(exclusive(),1);assert.equal(publishPending(),1);
  phase='release';main.child.stdio[4].write('exit\n');
  assert.deepEqual(await deadline(main.exit,'main exit'),{code:0,signal:null});
  // Allow Chromium's own child processes to wind down. A surviving inherited
  // open description keeps EX busy and fails the bounded proof.
  let available=false;for(let i=0;i<50;i++){if(exclusive()===0){available=true;break;}await new Promise(done=>setTimeout(done,100));}
  assert.equal(available,true,'a surviving Chromium descendant retained admission');
  const afterStat=(await readFile('/proc/'+descendant+'/stat','utf8')).split(') ')[1].split(' ');
  assert.notEqual(afterStat[0],'Z');assert.equal(afterStat[19],startTime);
  phase='pending';assert.equal(publishPending(),0);
  const lateProfile=join(root,'admission-late-profile');late=launch(lateProfile);
  assert.deepEqual(await deadline(late.exit,'pending launch'),{code:40,signal:null});
  await assert.rejects(access(lateProfile),{code:'ENOENT'});
  assert.equal(exclusive(),0);
  return {format:1,status:'passed',directElf:true,windowBusy:true,trayBusy:true,pendingRefusedBeforeProfile:true,lockReleasedWithSpawnedChildAlive:true};
 } catch { throw Error('admission proof failed: '+phase); } finally {
  await cleanupAdmissionGroup(main?.child,descendant);
  await cleanupAdmissionGroup(late?.child);
 }
}
