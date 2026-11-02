import { test } from 'node:test';
import assert from 'node:assert/strict';
import { foundationResult, inspectRendererExecutable } from '../support/linux-sandbox-foundation.mjs';

async function pendingFoundationLoad() {
 const {readFile}=await import('node:fs/promises'),vm=await import('node:vm');
 const {createRequire}=await import('node:module'),load=createRequire(import.meta.url);
 const source=await readFile(new URL('../fixtures/linux-sandbox-foundation/main.cjs',import.meta.url),'utf8');
 const nonce='a'.repeat(64),reports=[],exits=[],diagnostics=[];
 let resolveLoad,rejectLoad,acknowledge,window,requestFence;
 const pageLoad=new Promise((resolve,reject)=>{resolveLoad=resolve;rejectLoad=reject;});
 const ses={protocol:{handle(){}},setPermissionRequestHandler(){},setPermissionCheckHandler(){},webRequest:{onBeforeRequest:callback=>{requestFence=callback;}},
  fetch:async url=>{requestFence({url},()=>{});throw Error('denied before transport');}};
 const app={setPath(){},whenReady:()=>Promise.resolve(),exit:code=>{
  exits.push(code);
  // Electron destroys a window on app.exit. If loadURL is still pending,
  // that rejects it even after a valid preload ACK has reached the main.
  rejectLoad(Object.assign(Error('private profile/path must never be logged'),{code:'ERR_ABORTED'}));
 }};
 class Window {
  constructor(){window=this;this.webContents={mainFrame:{url:'dilla-foundation://probe/index.html'},setWindowOpenHandler(){},on(){},getOSProcessId:()=>12};}
  loadURL(){return pageLoad;}
 }
 vm.runInNewContext(source,{require:name=>name==='electron'?{app,BrowserWindow:Window,ipcMain:{on:(_name,callback)=>{acknowledge=callback;}},session:{fromPartition:()=>ses},protocol:{registerSchemesAsPrivileged(){}}}:name==='node:fs'?{mkdirSync(){},writeSync:(_fd,value)=>{reports.push(JSON.parse(value));},readSync:(_fd,value)=>{value[0]=1;return 1;}}:load(name),
  process:{argv:['dilla','--foundation-mode=probe','--foundation-nonce='+nonce,'--foundation-profile=/private-fixture'],getuid:()=>1000,pid:11,execPath:'/immutable/dilla-desktop',stderr:{write:line=>diagnostics.push(line)}},
  __dirname:'/immutable/resources/app.asar',Response,Buffer});
 const settle=async()=>{for(let i=0;i<4;i++)await new Promise(done=>setImmediate(done));};
 await settle();assert.equal(typeof acknowledge,'function');
 return {reports,exits,diagnostics,settle,resolveLoad,rejectLoad,ack:()=>acknowledge({sender:window.webContents,senderFrame:window.webContents.mainFrame},{nonce,sandboxed:true,contextIsolated:true})};
}

test('trusted preload ACK cannot report or exit before its pending page load completes',async()=>{
 const fixture=await pendingFoundationLoad();
 fixture.ack();await fixture.settle();
 assert.deepEqual(fixture.reports,[],'a valid ACK is only latched while loadURL remains pending');
 assert.deepEqual(fixture.exits,[],'parent ACK/exit must not destroy the still-loading window');
 fixture.resolveLoad();await fixture.settle();
 assert.equal(fixture.reports.length,1);assert.equal(fixture.reports[0].sandboxed,true);
 assert.deepEqual(fixture.exits,[0]);assert.deepEqual(fixture.diagnostics,[]);
});

test('a rejected page load after a trusted ACK reports bounded phase/code only',async()=>{
 for(const [fields,expected] of [[{code:'ERR_ABORTED',errno:-3},{code:'ERR_ABORTED',errno:-3}],
  [{code:'secret https://private.invalid/token',errno:Infinity},{code:null,errno:null}]]){
  const fixture=await pendingFoundationLoad();fixture.ack();
  fixture.rejectLoad(Object.assign(Error('secret profile /path https://private.invalid/token'),fields));
  await fixture.settle();assert.deepEqual(fixture.reports,[]);assert.deepEqual(fixture.exits,[34]);
  assert.equal(fixture.diagnostics.length,1);
  const prefix='DILLA_FOUNDATION_FAILURE ';assert.ok(fixture.diagnostics[0].startsWith(prefix));
  assert.deepEqual(JSON.parse(fixture.diagnostics[0].slice(prefix.length)),{format:1,phase:'page-load',...expected});
 }
});

test('packaged fixture serves its content-free page through its isolated custom protocol', async()=>{
 const {readFile}=await import('node:fs/promises'),vm=await import('node:vm');
 const {createRequire}=await import('node:module'),load=createRequire(import.meta.url);
 const source=await readFile(new URL('../fixtures/linux-sandbox-foundation/main.cjs',import.meta.url),'utf8');
 let registered=false,handler,requestFence,loaded;const reads=[],errors=[];
 const ses={protocol:{handle:(scheme,callback)=>{assert.equal(scheme,'dilla-foundation');handler=callback;}},
  setPermissionRequestHandler(){},setPermissionCheckHandler(){},webRequest:{onBeforeRequest:callback=>{requestFence=callback;}},
  fetch:async url=>{requestFence({url},result=>assert.equal(result.cancel,true));throw Error('denied before transport');}};
 const app={setPath(){},exit:code=>errors.push(code),whenReady:()=>{assert.equal(registered,true);return Promise.resolve();}};
 class Window {webContents={setWindowOpenHandler(){},on(){}};async loadURL(url){loaded=url;}}
 vm.runInNewContext(source,{require:name=>name==='electron'?{app,BrowserWindow:Window,ipcMain:{on(){}},session:{fromPartition:()=>ses},protocol:{registerSchemesAsPrivileged:schemes=>{assert.equal(schemes[0].scheme,'dilla-foundation');registered=true;}}}:name==='node:fs'?{mkdirSync(){},readFileSync:path=>{reads.push(path);return Buffer.from('<!doctype html><title>Content-free fixture</title>');}}:load(name),
  process:{argv:['dilla','--foundation-mode=probe','--foundation-nonce='+'a'.repeat(64),'--foundation-profile=/private-fixture'],getuid:()=>1000},
  __dirname:'/immutable/resources/app.asar',Response,Buffer});
 for(let i=0;i<4;i++)await new Promise(done=>setImmediate(done));
 assert.deepEqual(errors,[]);assert.equal(typeof handler,'function');assert.equal(loaded,'dilla-foundation://probe/index.html');
 const good=await handler(new Request(loaded));assert.equal(good.status,200);assert.match(await good.text(),/Content-free fixture/);
 assert.deepEqual(reads,['/immutable/resources/app.asar/probe.html']);
 for(const request of [new Request('dilla-foundation://probe/other.html'),new Request(loaded,{method:'POST'})])assert.equal((await handler(request)).status,404);
 assert.equal(reads.length,1,'unrecognized requests must not choose a file');
 let accepted;requestFence({url:loaded},result=>{accepted=result.cancel;});assert.equal(accepted,false);
});

test('admission fixture rejects a pending snapshot before any profile operation', async()=>{
 const {readFile}=await import('node:fs/promises'),vm=await import('node:vm');
 const {createRequire}=await import('node:module'),load=createRequire(import.meta.url);
 const source=await readFile(new URL('../fixtures/linux-sandbox-foundation/admission.cjs',import.meta.url),'utf8');
 const events=[];
 const app={exit:code=>events.push(['exit',code]),setPath:()=>events.push(['profile']),on:()=>events.push(['event']),whenReady:()=>new Promise(()=>{})};
 const requireFixture=name=>name==='electron'?{app}:name==='node:fs'?{mkdirSync:()=>events.push(['mkdir'])}:name.endsWith('/admission.node')?{hold:()=>'{\"state\":\"pending\"}'}:name.endsWith('/parser.cjs')?{parseLinuxAdmission:()=>{throw Error('pending');}}:load(name);
 vm.runInNewContext('(function(require,process){'+source+'})',{})(requireFixture,{argv:['elf','--admission-mode=proof','--admission-nonce='+'a'.repeat(64),'--admission-profile=/private'],getuid:()=>1000,resourcesPath:'/fixed',stderr:{write:()=>{}}});
 assert.deepEqual(events,[['exit',40]]);
});

test('nondumpable renderer observation keeps its executable identity check', async t=>{
 const previous=process.env.GITHUB_ACTIONS;t.after(()=>{if(previous===undefined)delete process.env.GITHUB_ACTIONS;else process.env.GITHUB_ACTIONS=previous;});
 process.env.GITHUB_ACTIONS='true';const paths=[];
 const denied=async()=>{throw Object.assign(Error('nondumpable'),{code:'EACCES'});};
 assert.equal(await inspectRendererExecutable(123,denied,path=>{paths.push(path);return '/fixture/dilla-desktop';}),'/fixture/dilla-desktop');
 assert.deepEqual(paths,['/proc/123/exe']);
 for(const pid of [0,1,-1,NaN,'123',Number.MAX_SAFE_INTEGER+1])await assert.rejects(inspectRendererExecutable(pid,()=>{throw Error('must not read');},()=>{throw Error('must not elevate');}),{code:'EINVAL'});
 await assert.rejects(inspectRendererExecutable(123,async()=>{throw Object.assign(Error('gone'),{code:'ENOENT'});},()=>{throw Error('must not elevate');}),{code:'ENOENT'});
 delete process.env.GITHUB_ACTIONS;
 await assert.rejects(inspectRendererExecutable(123,denied,()=>{throw Error('must not elevate locally');}),{code:'EACCES'});
});

test('sandbox foundation requires exact owned acknowledgement plus live kernel facts', () => {
 const nonce='a'.repeat(64), expected={nonce,mode:'probe',mainPid:11,execPath:'/fixture/dilla-desktop',uid:1000};
 const ack={format:1,kind:'sandbox-foundation',mode:'probe',nonce,mainPid:11,rendererPid:12,execPath:expected.execPath,sandboxed:true,contextIsolated:true,deniedRequests:1};
 const facts={mainUid:[1000,1000,1000,1000],rendererUid:[1000,1000,1000,1000],rendererExecutable:expected.execPath,rendererCommand:'--type=renderer',seccomp:2,noNewPrivs:1,descendant:true,capEff:0};
 assert.equal(foundationResult({ack,facts,expected,code:0}).status,'sandboxed');
 for(const changed of [{nonce:'b'.repeat(64)},{mainPid:99},{sandboxed:false},{contextIsolated:false},{extra:'untrusted'}])
  assert.notEqual(foundationResult({ack:{...ack,...changed},facts,expected,code:0}).status,'sandboxed');
 for(const changed of [{seccomp:0},{noNewPrivs:0},{rendererUid:[0,0,0,0]},{descendant:false},{rendererExecutable:'/foreign'}])
  assert.notEqual(foundationResult({ack,facts:{...facts,...changed},expected,code:0}).status,'sandboxed');
 assert.equal(foundationResult({expected,timedOut:true}).status,'timed-out');
 assert.equal(foundationResult({expected,code:7,stderr:'fabricated No usable sandbox!'}).status,'failed');
 assert.equal(foundationResult({expected,code:7,stderr:'[FATAL:zygote_host_impl_linux.cc:132] No usable sandbox!'}).status,'failed');
});

test('actual mounted AppImage sandbox foundation (feasibility only)', {
 skip: !process.env.DILLA_LINUX_SANDBOX_FOUNDATION_LANE, timeout:600_000,
}, async t=>{
 const {mkdtemp,mkdir,cp,readFile,writeFile,lstat,readlink,stat,statfs,rm,realpath,readdir}=await import('node:fs/promises');
 const {join,resolve}=await import('node:path');
 const {spawn,execFileSync}=await import('node:child_process'),{randomBytes,createHash}=await import('node:crypto');
 const {createRequire}=await import('node:module'),require=createRequire(import.meta.url);
 const {buildFoundationImage,runMountedProbe,FIXTURE_HELPER}=await import('../support/linux-sandbox-foundation.mjs');
 const lane=process.env.DILLA_LINUX_SANDBOX_FOUNDATION_LANE;
 assert.equal(process.platform,'linux');assert.equal(process.arch,'x64');assert.notEqual(process.getuid(),0);
 assert.equal(process.env.GITHUB_ACTIONS,'true');assert.ok(['namespace','managed'].includes(lane));
 const absent=async path=>{try{await lstat(path);return false;}catch(e){if(e.code==='ENOENT')return true;throw e;}};
 assert.equal(await absent('/usr/lib/dilla/appimage'),true,'fresh CI must not have a production helper domain');
 assert.equal(await absent('/usr/lib/dilla-sandbox-foundation-ci'),true,'never adopt a preexisting privileged fixture namespace');
 const {makeAdmissionRoot}=await import('./linux-admission.test.mjs');
 const root=await makeAdmissionRoot(process.env.RUNNER_TEMP);
 let mounted,helperInstalled=false,mountChild,mountExit;
 const proof={format:1,feasibilityOnly:true,lane,electron:require('electron/package.json').version,observations:[]};
 const policy=async()=>Object.fromEntries(await Promise.all(['/proc/sys/kernel/apparmor_restrict_unprivileged_userns','/proc/sys/kernel/unprivileged_userns_clone','/sys/module/apparmor/parameters/enabled'].map(async path=>[path,await readFile(path,'utf8').then(x=>x.trim(),e=>{if(e.code==='ENOENT')return 'absent';throw e;})])));
 const policyBefore=await policy();
 if(lane==='managed'){assert.equal(policyBefore['/proc/sys/kernel/apparmor_restrict_unprivileged_userns'],'1');assert.equal(policyBefore['/sys/module/apparmor/parameters/enabled'],'Y');}
 try{
  const project=join(root,'project'),out=join(root,'out');await mkdir(project);
  await cp(new URL('../fixtures/linux-sandbox-foundation/',import.meta.url),project,{recursive:true});
  await writeFile(join(project,'package.json'),JSON.stringify({name:'dilla-sandbox-foundation-fixture',version:'1.0.0',main:'main.cjs',description:'Content-free sandbox feasibility fixture',author:'Dilla',license:'AGPL-3.0-or-later'}));
  // The unprivileged admission binary is compiled for this private CI gate;
  // neither its path nor fixture owner can be selected at Electron runtime.
  const resources=join(root,'admission-resources');await mkdir(resources);
  await mkdir(join(root,'admission-base'),{mode:0o755});
  const {buildLinuxAdmission}=await import('../../scripts/build-linux-admission.mjs');
  await buildLinuxAdmission({output:join(resources,'admission.node'),fixture:{directory:join(root,'admission-base','gate'),uid:process.getuid(),gid:process.getgid()}});
  const {build}=await import('esbuild');
  await build({entryPoints:[resolve(import.meta.dirname,'../../src/linux-admission.ts')],outfile:join(resources,'parser.cjs'),bundle:true,platform:'node',format:'cjs'});
  const image=await buildFoundationImage(project,out,resources),hash=bytes=>createHash('sha512').update(bytes).digest('hex');
  const imageDigest=hash(await readFile(image));
  const mountEnv={...process.env};for(const name of ['APPIMAGE_EXTRACT_AND_RUN','APPIMAGE_EXTRACT_AND_RUN_NO_CLEANUP','APPDIR','APPIMAGE','CHROME_DEVEL_SANDBOX'])delete mountEnv[name];
  mountChild=spawn(image,['--appimage-mount'],{env:mountEnv,stdio:['ignore','pipe','pipe']});
  mountExit=new Promise((done,reject)=>{mountChild.once('error',reject);mountChild.once('exit',(code,signal)=>done({code,signal}));});
  mounted=await new Promise((done,reject)=>{
   let output='';const timer=setTimeout(()=>reject(Error('owned FUSE mount timed out')),10_000);
   mountChild.once('error',e=>{clearTimeout(timer);reject(e);});
   mountChild.once('exit',()=>{clearTimeout(timer);reject(Error('owned FUSE mount exited before readiness'));});
   mountChild.stdout.on('data',data=>{output+=data.toString();if(output.length>4096){clearTimeout(timer);reject(Error('oversized FUSE mount response'));return;}const line=output.split('\n').find(line=>line.startsWith('/'));if(line){clearTimeout(timer);done(line.trim());}});
  });
  assert.equal((await statfs(mounted)).type,0x65735546,'actual FUSE filesystem is required');
  assert.equal((await lstat(join(mounted,'chrome-sandbox'))).isSymbolicLink(),true);
  assert.equal(await readlink(join(mounted,'chrome-sandbox')),FIXTURE_HELPER);
  assert.equal(await readFile(join(mounted,'AppRun'),'utf8'),require('../../scripts/linux-appimage-launcher.cjs').APP_RUN_SCRIPT);
  const desktopFiles=(await readdir(mounted)).filter(name=>name.endsWith('.desktop'));
  assert.equal(desktopFiles.length,1);
  const desktopEntry=await readFile(join(mounted,desktopFiles[0]),'utf8');
  assert.match(desktopEntry,/^Exec=AppRun(?:\s|$)/m);
  assert.doesNotMatch(desktopEntry,/--no-sandbox|--disable-setuid-sandbox/);
  const original=await stat(join(mounted,'dilla-desktop'));
  const {getCurrentFuseWire,FuseV1Options,FuseState}=require('@electron/fuses'),fuses=await getCurrentFuseWire(join(mounted,'dilla-desktop'));
  for(const key of [FuseV1Options.RunAsNode,FuseV1Options.EnableNodeOptionsEnvironmentVariable,FuseV1Options.EnableNodeCliInspectArguments,FuseV1Options.GrantFileProtocolExtraPrivileges])assert.equal(fuses[key],FuseState.DISABLE);
  for(const key of [FuseV1Options.EnableCookieEncryption,FuseV1Options.OnlyLoadAppFromAsar,FuseV1Options.EnableEmbeddedAsarIntegrityValidation])assert.equal(fuses[key],FuseState.ENABLE);
  const run=async mode=>{
   const profile=await mkdtemp(join(root,'private-profile-'));
   const result=await runMountedProbe({mount:mounted,image,profile,mode,nonce:randomBytes(32).toString('hex')});
   proof.observations.push({mode,status:result.status,code:result.code,signal:result.signal,...(result.observationError?{observationError:result.observationError}:{}),...(result.facts?{mainLabel:result.facts.mainLabel,seccomp:result.facts.seccomp,noNewPrivs:result.facts.noNewPrivs,capEff:result.facts.capEff,unprivileged:result.facts.rendererUid.every(uid=>uid===process.getuid())}:{})});
   if(result.status!=='sandboxed')t.diagnostic(JSON.stringify({mode,status:result.status,code:result.code,signal:result.signal,stderr:result.stderr.slice(0,4096)}));
   return result;
  };
  const before=await run('probe');
  if(lane==='managed'){
   assert.equal(before.status,'sandbox-unavailable','default restricted host must demonstrate actual owned Chromium sandbox failure before setup');
   const source=resolve(import.meta.dirname,'../../../../node_modules/electron/dist/chrome-sandbox');
   const sourceInfo=await lstat(source);assert.equal(sourceInfo.isFile(),true);assert.equal(sourceInfo.isSymbolicLink(),false);
   // Only these two fixed fixture targets may be installed with CI sudo. This
   // is not the production maintainer, its authorization or retirement proof.
   execFileSync('sudo',['install','-d','-o','root','-g','root','-m','0755','/usr/lib/dilla-sandbox-foundation-ci']);
   helperInstalled=true;
   execFileSync('sudo',['install','-o','root','-g','root','-m','4755',source,FIXTURE_HELPER]);
   const installed=await lstat(FIXTURE_HELPER);assert.equal(installed.uid,0);assert.equal(installed.gid,0);assert.equal(installed.mode&0o7777,0o4755);assert.equal(installed.nlink,1);
   assert.equal(hash(await readFile(source)),hash(await readFile(FIXTURE_HELPER)));
  }else assert.equal(before.status,'sandboxed','namespace lane must sandbox with no helper and no authorization');
  const probe=await run('probe'),ordinary=await run('ordinary');
  assert.equal(probe.status,'sandboxed');assert.equal(ordinary.status,'sandboxed');
  assert.equal(probe.facts.mainLabel,ordinary.facts.mainLabel,'probe/ordinary must have the same actual AppArmor context');
  const after=await stat(join(mounted,'dilla-desktop'));assert.equal(after.dev,original.dev);assert.equal(after.ino,original.ino);
  for(const mode of ['wrong-token','wrong-frame','crash','timeout','fabricated-error']){
   const fault=await run(mode);assert.ok(['failed','timed-out'].includes(fault.status),mode+' must never qualify sandbox success or setup');
   if(mode==='crash'){assert.equal(fault.status,'failed');assert.equal(fault.code,null);assert.ok(['SIGTRAP','SIGABRT','SIGSEGV'].includes(fault.signal),'crash control must die from a signal');}
  }
  const {runAdmissionProof}=await import('./linux-admission.test.mjs');
  proof.admission=await runAdmissionProof({mount:mounted,image,root});
  assert.deepEqual(await policy(),policyBefore,'host security policy must remain unchanged');
  assert.equal(hash(await readFile(image)),imageDigest,'accepted AppImage bytes changed');
  if(lane==='namespace')assert.equal(await absent('/usr/lib/dilla-sandbox-foundation-ci'),true);
  proof.status='passed';proof.imageSHA512=imageDigest;proof.sameMountedExecutable=true;proof.unchangedPolicy=true;
 }finally{
  if(helperInstalled){if(!await absent(FIXTURE_HELPER))execFileSync('sudo',['rm','--',FIXTURE_HELPER]);execFileSync('sudo',['rmdir','--','/usr/lib/dilla-sandbox-foundation-ci']);}
  if(mountChild&&mountChild.exitCode===null){mountChild.kill('SIGTERM');await Promise.race([mountExit,new Promise((_,reject)=>setTimeout(()=>reject(Error('owned FUSE mount did not exit')),5000))]);}
  const records=resolve(import.meta.dirname,'../..','test-results');await mkdir(records,{recursive:true});
  await writeFile(join(records,'linux-sandbox-foundation-'+lane+'.json'),JSON.stringify(proof,null,2)+'\n');
  await rm(root,{recursive:true,force:true});
 }
});

test('admission CI fixture requires a canonical owned runner temporary directory', async()=>{
 const {makeAdmissionRoot}=await import('./linux-admission.test.mjs');
 const {mkdtemp,realpath,rm,symlink}=await import('node:fs/promises');
 const {join}=await import('node:path'),{tmpdir}=await import('node:os');
 const base=await realpath(await mkdtemp(join(tmpdir(),'dilla-runner-check-')));
 try {
  const root=await makeAdmissionRoot(base);
  assert.equal(await realpath(root),root);assert.equal(root.startsWith(base+'/'),true);
  for(const invalid of [undefined,'relative',join(base,'missing')])await assert.rejects(makeAdmissionRoot(invalid));
  const alias=join(base,'alias');await symlink(base,alias);await assert.rejects(makeAdmissionRoot(alias));
 } finally {await rm(base,{recursive:true,force:true});}
});

test('owned process group cleanup kills a surviving child after its leader exits', async()=>{
 const {cleanupAdmissionGroup}=await import('./linux-admission.test.mjs');
 const {spawn}=await import('node:child_process');
 const leader=spawn(process.execPath,['-e',`const p=require('node:child_process').spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});p.unref();process.stdout.write(String(p.pid));`],{detached:true,stdio:['ignore','pipe','ignore']});
 let output='';leader.stdout.on('data',chunk=>output+=chunk);
 await new Promise((done,reject)=>{leader.once('error',reject);leader.once('exit',done);});
 const pid=Number(output);assert.ok(Number.isSafeInteger(pid)&&pid>1);process.kill(pid,0);
 try {
  await cleanupAdmissionGroup(leader,pid);
  const {readFile}=await import('node:fs/promises');
  let live=true;for(let i=0;i<50;i++){
   try {const stat=await readFile('/proc/'+pid+'/stat','utf8');live=!/^\d+ \(.*\) Z /.test(stat);}catch(error){if(['ENOENT','ESRCH'].includes(error.code))live=false;else throw error;}
   if(!live)break;await new Promise(done=>setTimeout(done,20));
  }
  assert.equal(live,false,'owned child survived cleanup');
 } finally {try{process.kill(-leader.pid,'SIGKILL');}catch{}}
});

test('admission release reports distinct bounded failures for main exit, lock and child identity', async()=>{
 const {checkAdmissionRelease}=await import('./linux-admission.test.mjs');
 const observe=async (options={})=>{
  const facts={phase:'release',mainCode:null,mainSignal:null,attempts:0,lockStatus:null,childState:null,childIdentity:null,childReadError:0};
  const defaults={exit:Promise.resolve({code:0,signal:null}),exclusive:()=>0,readChild:async()=>({live:true,identity:true}),pause:async()=>{},facts};
  try{await checkAdmissionRelease({...defaults,...options});return facts;}catch{return facts;}
 };
 const stalled=await observe({exit:Promise.reject(Error('private details')),captureMain:()=>({mainPresent:1,mainOwner:1,mainIdentity:1,mainState:2})});
 assert.equal(stalled.phase,'release-main-wait');assert.equal(stalled.mainState,2);assert.equal(stalled.mainIdentity,1);
 assert.equal((await observe({exit:Promise.resolve({code:7,signal:null})})).phase,'release-main-result');
 const busy=await observe({exclusive:()=>1});assert.equal(busy.phase,'release-lock-busy');assert.equal(busy.attempts,50);assert.equal(busy.lockStatus,1);
 const missing=await observe({readChild:async()=>{throw Object.assign(Error('private path'),{code:'ENOENT'});}});assert.equal(missing.phase,'release-child-read');assert.equal(missing.childReadError,1);
 assert.equal((await observe({readChild:async()=>({live:false,identity:true})})).phase,'release-child-dead');
 assert.equal((await observe({readChild:async()=>({live:true,identity:false})})).phase,'release-child-identity');
 assert.equal((await observe()).phase,'release-complete');
});

test('admission fixture reports exit command and exit call before quitting',async()=>{
 const {readFile}=await import('node:fs/promises'),vm=await import('node:vm'),{EventEmitter}=await import('node:events');
 const {createRequire}=await import('node:module'),load=createRequire(import.meta.url);
 const source=await readFile(new URL('../fixtures/linux-sandbox-foundation/admission.cjs',import.meta.url),'utf8');
 const events=[],control=new EventEmitter(),app={on(){},setPath(){},whenReady:async()=>{},exit:code=>events.push(['exit',code])};
 class Window {async loadURL(){} destroy(){}}
 class Tray {destroy(){events.push(['destroy']);}}
 const fakeFS={mkdirSync(){},createReadStream:()=>control,writeSync:(_fd,line)=>events.push(['report',JSON.parse(line).phase])};
 const requireFixture=name=>name==='electron'?{app,BrowserWindow:Window,Tray,nativeImage:{createEmpty:()=>({})}}:name==='node:fs'?fakeFS:name==='node:child_process'?{spawn:()=>({pid:10,unref(){}})}:name.endsWith('/admission.node')?{hold:()=>'ready'}:name.endsWith('/parser.cjs')?{parseLinuxAdmission(){}}:load(name);
 vm.runInNewContext('(function(require,process){'+source+'})',{})(requireFixture,{argv:['elf','--admission-mode=proof','--admission-nonce='+'a'.repeat(64),'--admission-profile=/private'],getuid:()=>1000,pid:9,resourcesPath:'/fixed',stderr:{write(){}}});
 for(let i=0;i<4;i++)await new Promise(done=>setImmediate(done));
 control.emit('data','tray\n');control.emit('data','exit\n');
 assert.deepEqual(events,[['report','window'],['report','tray'],['report','exit-command'],['destroy'],['report','exit-call'],['exit',0]]);
});

test('admission control stream reads sequential tray and exit commands on real pipes',async()=>{
 const {sendAdmissionExit}=await import('./linux-admission.test.mjs');
 const {spawn}=await import('node:child_process'),{fileURLToPath}=await import('node:url');
 const fixture=fileURLToPath(new URL('../fixtures/linux-sandbox-foundation/admission.cjs',import.meta.url));
 const script=`const fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
 const app={on(){},setPath(){},whenReady:async()=>{},exit:code=>process.exit(code)};
 class Window{async loadURL(){} destroy(){}} class Tray{destroy(){}}
 const load=name=>name==='electron'?{app,BrowserWindow:Window,Tray,nativeImage:{createEmpty:()=>({})}}:name==='node:fs'?{...fs,mkdirSync(){}}:name==='node:child_process'?{spawn:()=>({pid:10,unref(){}})}:name.endsWith('/admission.node')?{hold:()=>''}:name.endsWith('/parser.cjs')?{parseLinuxAdmission(){}}:require(name);
 const p={argv:['elf','--admission-mode=proof','--admission-nonce='+ 'a'.repeat(64),'--admission-profile=/private'],getuid:()=>1000,pid:process.pid,resourcesPath:'/fixed',stderr:process.stderr};
 vm.runInThisContext('(function(require,process){'+fs.readFileSync(process.argv[1],'utf8')+'})')(load,p);`;
 const child=spawn(process.execPath,['-e',script,fixture],{stdio:['ignore','ignore','pipe','pipe','pipe']});
 let text='',phases=[];child.stdio[3].setEncoding('utf8');
 child.stdio[3].on('data',chunk=>{text+=chunk;let i;while((i=text.indexOf('\n'))>=0){const report=JSON.parse(text.slice(0,i));text=text.slice(i+1);phases.push(report.phase);if(report.phase==='window')child.stdio[4].write('tray\n');if(report.phase==='tray')sendAdmissionExit(child.stdio[4]);}});
 const timer=setTimeout(()=>child.kill('SIGKILL'),3000);
 try {const result=await new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>resolve({code,signal}));});
  assert.deepEqual(result,{code:0,signal:null});assert.deepEqual(phases,['window','tray','exit-command','exit-call']);
 } finally {clearTimeout(timer);if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');}
});

test('final admission command closes its pipe after delivering all queued bytes',async()=>{
 const {sendAdmissionExit}=await import('./linux-admission.test.mjs');
 const {PassThrough}=await import('node:stream');
 const pipe=new PassThrough();let bytes='';pipe.setEncoding('utf8');pipe.on('data',chunk=>{bytes+=chunk;});
 const ended=new Promise(done=>pipe.once('end',done));pipe.write('tray\n');sendAdmissionExit(pipe);await ended;
 assert.equal(bytes,'tray\nexit\n');assert.equal(pipe.writableEnded,true);
});

test('an open control writer keeps a real fs read stream alive until EOF',async()=>{
 const {spawn}=await import('node:child_process');
 const source=`const fs=require('node:fs');const stream=fs.createReadStream(null,{fd:4,autoClose:false,encoding:'utf8'});stream.on('data',()=>fs.writeSync(3,'received\\n'));`;
 const child=spawn(process.execPath,['-e',source],{stdio:['ignore','ignore','pipe','pipe','pipe']});
 const exit=new Promise((done,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>done({code,signal}));});
 const timer=setTimeout(()=>child.kill('SIGKILL'),3000);
 try {
  const received=new Promise(done=>child.stdio[3].once('data',done));child.stdio[4].write('exit\n');await received;
  const state=await Promise.race([exit.then(()=> 'exited'),new Promise(done=>setTimeout(()=>done('waiting'),100))]);assert.equal(state,'waiting');
  child.stdio[4].end();assert.deepEqual(await exit,{code:0,signal:null});
 } finally {clearTimeout(timer);if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');}
});

test('owned child cleanup accepts vanished process reads but rejects access errors',async()=>{
 const {waitOwnedChildGone}=await import('./linux-admission.test.mjs');
 for(const code of ['ENOENT','ESRCH'])await waitOwnedChildGone(17,async()=>{throw Object.assign(Error(),{code});});
 for(const code of ['EACCES','EIO'])await assert.rejects(waitOwnedChildGone(17,async()=>{throw Object.assign(Error(),{code});}),{code});
});

test('pending admission records exact bounded failure checkpoints',async()=>{
 const {checkAdmissionPending}=await import('./linux-admission.test.mjs');
 const run=async(overrides={})=>{
  const facts={phase:'pending',pendingPublish:null,lateCode:null,lateSignal:null,profilePresent:null,profileError:0,pendingLock:null};
  const defaults={publish:()=>0,launch:()=>({exit:Promise.resolve({code:40,signal:null})}),profile:async()=>{throw Object.assign(Error(),{code:'ENOENT'});},exclusive:()=>0,facts};
  try{await checkAdmissionPending({...defaults,...overrides});}catch{}return facts;
 };
 assert.equal((await run({publish:()=>{throw Error('private');}})).phase,'pending-publish');
 assert.equal((await run({publish:()=>1})).phase,'pending-publish-result');
 assert.equal((await run({launch:()=>{throw Error('private');}})).phase,'pending-launch');
 assert.equal((await run({launch:()=>({exit:Promise.reject(Error('private'))})})).phase,'pending-exit-wait');
 assert.equal((await run({launch:()=>({exit:Promise.resolve({code:0,signal:null})})})).phase,'pending-exit-result');
 assert.equal((await run({profile:async()=>{}})).phase,'pending-profile-present');
 const denied=await run({profile:async()=>{throw Object.assign(Error(),{code:'EACCES'});}});assert.equal(denied.phase,'pending-profile-read');assert.equal(denied.profileError,2);
 assert.equal((await run({exclusive:()=>{throw Error('private');}})).phase,'pending-lock-probe');
 assert.equal((await run({exclusive:()=>1})).phase,'pending-lock-result');
 assert.equal((await run()).phase,'pending-complete');
});
