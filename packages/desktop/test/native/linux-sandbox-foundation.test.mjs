import { test } from 'node:test';
import assert from 'node:assert/strict';
import { foundationResult, inspectRendererExecutable } from '../support/linux-sandbox-foundation.mjs';

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
 const {join,resolve}=await import('node:path'),{tmpdir}=await import('node:os');
 const {spawn,execFileSync}=await import('node:child_process'),{randomBytes,createHash}=await import('node:crypto');
 const {createRequire}=await import('node:module'),require=createRequire(import.meta.url);
 const {buildFoundationImage,runMountedProbe,FIXTURE_HELPER}=await import('../support/linux-sandbox-foundation.mjs');
 const lane=process.env.DILLA_LINUX_SANDBOX_FOUNDATION_LANE;
 assert.equal(process.platform,'linux');assert.equal(process.arch,'x64');assert.notEqual(process.getuid(),0);
 assert.equal(process.env.GITHUB_ACTIONS,'true');assert.ok(['namespace','managed'].includes(lane));
 const absent=async path=>{try{await lstat(path);return false;}catch(e){if(e.code==='ENOENT')return true;throw e;}};
 assert.equal(await absent('/usr/lib/dilla/appimage'),true,'fresh CI must not have a production helper domain');
 assert.equal(await absent('/usr/lib/dilla-sandbox-foundation-ci'),true,'never adopt a preexisting privileged fixture namespace');
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-sandbox-foundation-')));
 let mounted,helperInstalled=false,mountChild,mountExit;
 const proof={format:1,feasibilityOnly:true,lane,electron:require('electron/package.json').version,observations:[]};
 const policy=async()=>Object.fromEntries(await Promise.all(['/proc/sys/kernel/apparmor_restrict_unprivileged_userns','/proc/sys/kernel/unprivileged_userns_clone','/sys/module/apparmor/parameters/enabled'].map(async path=>[path,await readFile(path,'utf8').then(x=>x.trim(),e=>{if(e.code==='ENOENT')return 'absent';throw e;})])));
 const policyBefore=await policy();
 if(lane==='managed'){assert.equal(policyBefore['/proc/sys/kernel/apparmor_restrict_unprivileged_userns'],'1');assert.equal(policyBefore['/sys/module/apparmor/parameters/enabled'],'Y');}
 try{
  const project=join(root,'project'),out=join(root,'out');await mkdir(project);
  await cp(new URL('../fixtures/linux-sandbox-foundation/',import.meta.url),project,{recursive:true});
  await writeFile(join(project,'package.json'),JSON.stringify({name:'dilla-sandbox-foundation-fixture',version:'1.0.0',main:'main.cjs',description:'Content-free sandbox feasibility fixture',author:'Dilla',license:'AGPL-3.0-or-later'}));
  const image=await buildFoundationImage(project,out),hash=bytes=>createHash('sha512').update(bytes).digest('hex');
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
