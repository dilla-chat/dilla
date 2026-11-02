import { test } from 'node:test';
import desktopBuilder from '../../electron-builder.cjs';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { createHash,generateKeyPairSync,sign,randomBytes } from 'node:crypto';
import { spawn,execFileSync } from 'node:child_process';
import { mkdtemp,mkdir,readFile,writeFile,rm,cp,open,readdir,lstat, realpath } from 'node:fs/promises';
import { join,resolve,dirname,win32 } from 'node:path';
import { tmpdir } from 'node:os';
import { setTimeout as delay } from 'node:timers/promises';
import { build } from 'esbuild';
import { requireWindowsEntryCI,runWindowsEntry,assertProtocolVacant,assertOwnedProtocol } from '../support/windows-entry.mjs';
import { windowsFixtureSource,windowsFixtureInstaller } from '../support/windows-update-fixture.mjs';
import { snapshotUninstall } from '../../src/update-registry.ts';
const desktop=resolve(import.meta.dirname,'../..'),repo=resolve(desktop,'../..');
const hash=bytes=>createHash('sha512').update(bytes).digest('base64');
const json=async file=>JSON.parse(await readFile(file,'utf8'));
async function until(read,label,timeout=90000){const deadline=Date.now()+timeout;let last;while(Date.now()<deadline){try{const value=await read();if(value)return value;}catch(error){if(error.terminal)throw error;last=error;}await delay(100);}throw new Error(label+' timed out: '+String(last));}
function alive(pid){try{process.kill(pid,0);return true;}catch(error){if(error.code==='ESRCH')return false;throw error;}}
async function stop(pid,tree=true){if(!pid||!alive(pid))return;if(tree)execFileSync('taskkill',['/PID',String(pid),'/T','/F'],{stdio:'ignore'});else process.kill(pid,'SIGKILL');await until(async()=>!alive(pid),'owned Windows process exits',15000);}

// A separately compiled CI-owned application uses fixture public keys/transport
// and a disposable CI user. The real dilla association is used only after
// refusing every preexisting mapping; shipping trust, fuses and signing remain intact.
test('Windows stable shell entry survives inactive NSIS interruption, exact health, slot commit faults and explicit retry',{skip:process.platform!=='win32',timeout:1200000},async()=>{
 requireWindowsEntryCI();
 const root=await realpath(await mkdtemp(join(tmpdir(),'dilla-windows-installed-'))),id=randomBytes(6).toString('hex'),product='DillaUpdateAcceptance'+id,appId='dev.dilla.desktop.update-acceptance.'+id,scheme='dilla';
 const profile=join(await realpath(process.env.APPDATA),product),programRoot=join(await realpath(process.env.LOCALAPPDATA),'Programs',product),launcher=join(programRoot,'Dilla.exe'),reports=join(root,'reports'),keys=generateKeyPairSync('ed25519'),key=keys.publicKey.export({type:'spki',format:'pem'}).toString(),log=await open(join(root,'process.log'),'a');
 const sourceNames=['main','updater','update-runtime','update-watchdog','update-journal','update-installer','windows-installation','windows-slots'];
 const source=Object.fromEntries(await Promise.all(sourceNames.map(async name=>[name,await readFile(join(desktop,'src',name+'.ts'),'utf8')]))),productionKeysHash=hash(await readFile(join(desktop,'src/update-keys.ts')));
 let server,descriptor,signature,artifactBytes,manifestBytes,fault='none',currentPid,primaryError,protocolLease=false,installedFixture=false,profileCreated=false;const outcomes={},owned=new Set();
 const entryContext={id,product,programRoot,profile};
 const dispatchEntry=action=>{const result=runWindowsEntry(action,entryContext);if(Number.isSafeInteger(result.launcherPid)&&result.launcherPid>1)owned.add(result.launcherPid);return result;};
 const equalPath=(a,b)=>win32.normalize(a).toLowerCase()===win32.normalize(b).toLowerCase();
 const running=async()=>{const raw=JSON.parse(execFileSync(launcher,['--state'],{encoding:'utf8',timeout:30000}));return raw;};
 const active=async()=>{const pointer=await json(join(profile,'updates/active.json')),directory=join(profile,'updates','startup-'+pointer.nonce);return{directory,journal:await json(join(directory,'journal.json')),helper:await json(join(directory,'watchdog-ready.json'))};};
 const normalLaunch=()=>{const child=spawn(launcher,[],{detached:true,stdio:['ignore',log.fd,log.fd]});child.unref();owned.add(child.pid);return child;};
 const report=async version=>{const value=await json(join(reports,version+'.json'));owned.add(value.pid);return value;};
 const accept=async(version,phase,target=version)=>until(async()=>{const transaction=await active(),value=await report(version);const plan=await json(join(transaction.directory,'plan.json'));return plan.health.version===target&&transaction.journal.phase===phase&&value.state.phase===(phase==='failed'?'error':'idle')?{...transaction,report:value}:null;},'Windows '+phase+' '+version,180000);
 try{
  assertProtocolVacant(runWindowsEntry('preflight',entryContext));await assert.rejects(lstat(programRoot),{code:'ENOENT'});
  await mkdir(profile);profileCreated=true;await mkdir(reports);await writeFile(join(profile,'device-sentinel'),'encrypted-device-data-survives');await writeFile(join(profile,'desktop.json'),JSON.stringify({version:1,instances:['https://desktop-update-offline.invalid/'],autostart:false}));
  server=createServer((req,res)=>{if(req.url==='/release.json'){const bytes=fault==='metadata'?Buffer.from(descriptor.toString().replace('"format":1','"format":2')):descriptor;res.end(bytes);}else if(req.url==='/release.json.sig')res.end(signature);else if(req.url==='/manifest'){const bytes=Buffer.from(manifestBytes);if(fault==='manifest')bytes[0]^=1;res.end(bytes);}else if(req.url==='/artifact'){if(fault==='download'){res.writeHead(200,{'content-length':artifactBytes.length});res.write(artifactBytes.subarray(0,1024));res.destroy();}else{const bytes=Buffer.from(artifactBytes);if(fault==='artifact')bytes[Math.floor(bytes.length/2)]^=1;res.writeHead(200,{'content-length':bytes.length});res.end(bytes);}}else{res.writeHead(404);res.end();}});
  await new Promise((done,reject)=>{server.once('error',reject);server.listen(0,'127.0.0.1',done);});const origin='http://127.0.0.1:'+server.address().port;
  const pack=async(version,stageFault)=>{
   const project=join(root,'project-'+version),out=join(root,'out-'+version),dist=join(project,'dist');await mkdir(dist,{recursive:true});await cp(join(desktop,'dist'),dist,{recursive:true});await rm(join(dist,'client'),{recursive:true,force:true});await cp(join(repo,'packages/web/dist'),join(dist,'client'),{recursive:true});
   await writeFile(join(project,'package.json'),JSON.stringify({name:'dilla-update-acceptance-'+id,version,main:'dist/main.cjs',description:'Installed Windows updater acceptance',author:'Dilla',license:'AGPL-3.0-or-later'}));
   await build({entryPoints:[join(desktop,'src/preload.ts')],outfile:join(dist,'preload.cjs'),bundle:true,platform:'node',target:'es2022',format:'cjs',external:['electron']});
   await build({entryPoints:[join(desktop,'src/main.ts')],outfile:join(dist,'main.cjs'),bundle:true,platform:'node',target:'node24',format:'cjs',external:['electron'],plugins:[{name:'compiled-windows-fixture',setup(plugin){
    plugin.onLoad({filter:/[\\/]src[\\/]update-keys\.ts$/},()=>({contents:`export const RELEASE_PUBLIC_KEYS=${JSON.stringify([key])};`,loader:'ts'}));
    for(const name of sourceNames)plugin.onLoad({filter:new RegExp('[\\\\/]src[\\\\/]'+name+'\\.ts$')},()=>{return{contents:windowsFixtureSource(name,source[name],{appId,product,profile,scheme,origin,reports,version}),loader:'ts',resolveDir:join(desktop,'src')};});
   }}]});
   let installerScript=await readFile(join(desktop,'build/windows-installer.nsi'),'utf8');
   installerScript=windowsFixtureInstaller(installerScript,stageFault,product);
   const scriptFile=join(project,'installer.nsi');await writeFile(scriptFile,installerScript);
   const base={appId,productName:product,executableName:'dilla-desktop', toolsets: desktopBuilder.toolsets,electronVersion:'45.2.1',electronDist:join(repo,'node_modules/electron/dist'),directories:{output:out},files:['dist/**','package.json'],asar:true,publish:{provider:'generic',url:origin},artifactName:`fixture-${version}.\${ext}`,protocols:[{name:'CI-owned Dilla updater fixture',schemes:[scheme]}],electronFuses:{runAsNode:false,enableCookieEncryption:true,enableNodeOptionsEnvironmentVariable:false,enableNodeCliInspectArguments:false,enableEmbeddedAsarIntegrityValidation:true,onlyLoadAppFromAsar:true,grantFileProtocolExtraPrivileges:false},win:{target:'nsis',icon:join(desktop,'assets/icon.png')},nsis:{script:scriptFile,perMachine:false,oneClick:true,allowElevation:false,packElevateHelper:false,preCompressedFileExtensions:[],differentialPackage:false,deleteAppDataOnUninstall:false}};
   const config=join(project,'builder.cjs');await writeFile(config,`const hooks=require(${JSON.stringify(join(desktop,'scripts/windows-builder-hooks.cjs'))}).createWindowsBuilderHooks({metadataPublicKeys:${JSON.stringify([key])}});module.exports={...${JSON.stringify(base)},afterSign:hooks.afterSign,artifactBuildStarted:hooks.artifactBuildStarted};`);
   execFileSync(process.execPath,[join(repo,'node_modules/electron-builder/cli.js'),'--projectDir',project,'--config',config,'--win','--x64','--publish','never'],{cwd:repo,stdio:['ignore',log.fd,log.fd]});
   for(const entry of await readdir(out,{withFileTypes:true}))if(entry.isDirectory())await rm(join(out,entry.name),{recursive:true,force:true,maxRetries:10,retryDelay:100});
   return{file:join(out,`fixture-${version}.exe`),manifest:join(out,`dilla-${version}-win-x64.manifest.json`)};
  };
  const versions={};for(const[v,f]of [['1.0.0'],['1.0.1'],['1.0.2'],['1.0.3'],['1.0.4','once'],['1.0.5','hang'],['1.0.6','hang'],['1.0.7'],['1.0.8'],['1.0.9']])versions[v]=await pack(v,f);
  const release=async version=>{artifactBytes=await readFile(versions[version].file);manifestBytes=await readFile(versions[version].manifest);const now=Date.now();descriptor=Buffer.from(JSON.stringify({format:1,version,channel:'stable',publishedAt:new Date(now-1000).toISOString(),expiresAt:new Date(now+3600000).toISOString(),artifacts:[{platform:'win32',arch:'x64',url:`https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-win-x64.exe`,size:artifactBytes.length,sha512:hash(artifactBytes),programManifest:{url:`https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-win-x64.manifest.json`,size:manifestBytes.length,sha512:hash(manifestBytes)}}]}));signature=sign(null,descriptor,keys.privateKey).toString('base64');};
  assertProtocolVacant(runWindowsEntry('preflight',entryContext));await assert.rejects(lstat(programRoot),{code:'ENOENT'});protocolLease=true;installedFixture=true;
  execFileSync(versions['1.0.0'].file,['/S'],{stdio:['ignore',log.fd,log.fd],timeout:60000});const launcherHash=hash(await readFile(launcher));assert.equal((await running()).version,'1.0.0');
  const exerciseOSEntries=async version=>{
   const proof={},entryFile=join(reports,version+'-entry.json'),manifest=await readFile(versions[version].manifest),expectedExecutable=join(programRoot,'versions','v-'+Buffer.from(hash(manifest),'base64').toString('hex').slice(0,32),'dilla-desktop.exe');
   const waitEntry=async(mode,previousPID)=>until(async()=>{const observed=await json(entryFile);if(observed.pid===previousPID||!observed.activated||!observed.coreReady||!observed.windows.length)return null;
    assert.equal(observed.version,version);assert.equal(equalPath(observed.execPath,expectedExecutable),true);assert.equal(equalPath(observed.profile,profile),true);assert.equal(alive(observed.pid),true);owned.add(observed.pid);
    assert.equal(observed.windows.every(win=>win.enabled),true);
    if(mode==='autostart'){assert.equal(observed.args.includes('--hidden'),true);assert.equal(observed.windows.every(win=>!win.visible),true);}else assert.equal(observed.windows.some(win=>win.visible),true);
    return observed;
   },'actual installed '+mode+' main '+version,60000);
   let previousPID=currentPid;
   if(currentPid)await stop(currentPid);currentPid=undefined;
   await rm(entryFile,{force:true});await rm(join(reports,'autostart-configured.json'),{force:true});await writeFile(join(reports,'enable-autostart'),'fixture person enables production autostart setting');
   const shortcutDispatch=dispatchEntry('shortcut'),shortcut=await waitEntry('shortcut',previousPID);currentPid=shortcut.pid;
   await until(async()=>{const configured=await json(join(reports,'autostart-configured.json'));return configured.pid===shortcut.pid;},'actual production autostart registration');
   const registered=runWindowsEntry('inspect',entryContext);assertOwnedProtocol(registered.protocol,launcher);assert.equal(equalPath(registered.shortcutTarget,launcher),true);assert.equal(registered.shortcutArguments,'');
   proof.shortcut={dispatch:shortcutDispatch,registered,observed:shortcut};
   previousPID=currentPid;await stop(currentPid);currentPid=undefined;await rm(entryFile,{force:true});await rm(join(reports,'declined-entry-invite.json'),{force:true});await rm(join(reports,'unknown-entry-network.jsonl'),{force:true});
   const protocolDispatch=dispatchEntry('protocol'),protocol=await waitEntry('protocol',previousPID);currentPid=protocol.pid;
   const declined=await until(async()=>{const result=await json(join(reports,'declined-entry-invite.json'));return result.pid===protocol.pid?result:null;},'actual URI unknown-instance confirmation');
   assert.equal(protocol.args.includes(protocolDispatch.uri),true);assert.match(declined.message,new RegExp('entry-'+id+'\\.invalid'));assert.equal(declined.response,0);
   assert.deepEqual((await json(join(profile,'desktop.json'))).instances,['https://desktop-update-offline.invalid/']);await assert.rejects(readFile(join(reports,'unknown-entry-network.jsonl')),{code:'ENOENT'});
   proof.protocol={dispatch:protocolDispatch,observed:protocol,declined};
   previousPID=currentPid;await stop(currentPid);currentPid=undefined;await rm(entryFile,{force:true});
   const autostartDispatch=dispatchEntry('autostart'),hidden=await waitEntry('autostart',previousPID);currentPid=hidden.pid;
   assert.equal(autostartDispatch.logonSimulated,false);proof.autostart={dispatch:autostartDispatch,observed:hidden,registeredCommandExecuted:true,osRebootTested:false};
   await stop(currentPid);currentPid=undefined;return proof;
  };
  const retry=async version=>{if(currentPid)await stop(currentPid);currentPid=undefined;await rm(join(reports,version+'.json'),{force:true});await writeFile(join(reports,'retry-next'),'person explicitly retries signed update');normalLaunch();};
  await release('1.0.0');outcomes.initialOSEntries=await exerciseOSEntries('1.0.0');
  await release('1.0.1');
  for(const rejected of ['metadata','manifest','artifact','download']){fault=rejected;normalLaunch();const rejectedReport=await until(async()=>{const value=await report('1.0.0');return value.state.phase==='error'?value:null;},rejected+' rejection');await stop(rejectedReport.pid);await rm(join(reports,'1.0.0.json'),{force:true});assert.equal((await running()).version,'1.0.0');assert.equal(hash(await readFile(launcher)),launcherHash);outcomes[rejected]=rejectedReport;}
  fault='none';normalLaunch();const success=await accept('1.0.1','complete');currentPid=success.report.pid;assert.equal(alive(currentPid),true);assert.equal((await running()).version,'1.0.1');assert.equal(success.journal.childPid,currentPid);const actualHealth=await json(join(success.directory,'healthy.json'));assert.equal(actualHealth.pid,currentPid);assert.equal(actualHealth.version,'1.0.1');assert.equal(actualHealth.nonce,success.journal.nonce);assert.equal(success.report.inheritedHealth,null);assert.equal((await json(join(reports,'1.0.1-startup-wait.json'))).pending,true);await assert.rejects(readFile(join(reports,'1.0.1-precommit-api.jsonl')),{code:'ENOENT'});assert.equal((await until(async()=>json(join(reports,'1.0.1-activated-discovery.json')),'postactivation discovery')).pid,currentPid);outcomes.upgrade=success;
  await until(async()=>!alive(success.helper.pid),'committed update owner exit before OS entry dispatch');outcomes.upgradedOSEntries=await exerciseOSEntries('1.0.1');
  const registry=await snapshotUninstall(programRoot);assert.equal(registry.displayVersion?.data,'1.0.1');
  await release('1.0.2');await retry('1.0.1');const failed=await accept('1.0.1','failed','1.0.2');currentPid=failed.report.pid;assert.equal((await running()).version,'1.0.1');assert.equal(failed.journal.failure,'startup');assert.deepEqual(await snapshotUninstall(programRoot),registry);outcomes.startupRollback=failed;
  await release('1.0.3');await retry('1.0.1');const unregistered=await until(async()=>{const value=await active(),child=await json(join(reports,'unregistered-main.json'));return value.journal.phase==='launching'&&!value.journal.childPid?{...value,child}:null;},'Windows unregistered owned main');owned.add(unregistered.helper.pid);owned.add(unregistered.child.pid);assert.equal(unregistered.child.windows,0);await stop(unregistered.helper.pid,false);assert.equal(alive(unregistered.child.pid),true);normalLaunch();const recovered=await accept('1.0.1','failed','1.0.3');currentPid=recovered.report.pid;assert.equal(alive(unregistered.child.pid),false);assert.equal((await running()).version,'1.0.1');outcomes.interruptedChild=recovered;
  await release('1.0.4');await retry('1.0.1');const partial=await accept('1.0.1','failed','1.0.4');currentPid=partial.report.pid;assert.equal(partial.journal.failure,'installer');assert.equal((await running()).version,'1.0.1');assert.equal(hash(await readFile(launcher)),launcherHash);outcomes.partialInactive=partial;
  await retry('1.0.4');const retried=await accept('1.0.4','complete');currentPid=retried.report.pid;assert.equal((await running()).version,'1.0.4');outcomes.sameArtifactExplicitRetry=retried;assert.equal((await snapshotUninstall(programRoot)).displayVersion?.data,'1.0.4');
  for(const version of ['1.0.5','1.0.6']){await release(version);await retry('1.0.4');const writer=await until(async()=>{const value=await active();if(value.journal.phase!=='installing'||!value.journal.installerPid)return null;const target=join(programRoot,'versions','v-'+Buffer.from(hash(await readFile(versions[version].manifest)),'base64').toString('hex').slice(0,32),'partial-fixture.txt');await readFile(target);return{...value};},'contained NSIS writer');owned.add(writer.helper.pid);owned.add(writer.journal.installerActorPid);owned.add(writer.journal.installerPid);assert.equal(hash(await readFile(launcher)),launcherHash);if(version==='1.0.6'){await stop(writer.helper.pid,false);await until(async()=>!alive(writer.journal.installerPid),'private installer job exits on helper death',30000);normalLaunch();}const failure=await accept('1.0.4','failed',version);currentPid=failure.report.pid;assert.equal((await running()).version,'1.0.4');assert.equal(alive(writer.journal.installerPid),false);assert.equal(hash(await readFile(launcher)),launcherHash);outcomes[version==='1.0.5'?'installerTimeout':'installerParentDeath']=failure;}
  await release('1.0.7');await retry('1.0.4');const slotFull=await accept('1.0.4','failed','1.0.7');currentPid=slotFull.report.pid;assert.equal((await running()).version,'1.0.4');await assert.rejects(readFile(join(reports,'1.0.7-precommit-api.jsonl')),{code:'ENOENT'});outcomes.slotTemporaryEnospc=slotFull;
  // The journal is durable but the one-way slot rename has not happened. Kill
  // only the helper; normal stable launch must recover old without touching stores.
  await release('1.0.8');await retry('1.0.4');const beforeSlot=await until(async()=>{const marker=await json(join(reports,'before-slot-publication.json')),value=await active();return value.journal.phase==='complete'?{...value,marker}:null;},'durable complete before Windows slot rename');owned.add(beforeSlot.helper.pid);owned.add(beforeSlot.journal.childPid);await stop(beforeSlot.helper.pid,false);normalLaunch();await until(async()=>!alive(beforeSlot.journal.childPid),'hidden pending main activation timeout releases SIL',105000);normalLaunch();const oldAfterComplete=await accept('1.0.4','failed','1.0.8');currentPid=oldAfterComplete.report.pid;assert.equal((await running()).version,'1.0.4');await assert.rejects(readFile(join(reports,'1.0.8-precommit-api.jsonl')),{code:'ENOENT'});assert.equal(hash(await readFile(launcher)),launcherHash);outcomes.completeBeforeSlot=oldAfterComplete;
  await release('1.0.9');await retry('1.0.4');const uncertain=await accept('1.0.9','complete');currentPid=uncertain.report.pid;assert.equal((await running()).version,'1.0.9');assert.equal(alive(currentPid),true);assert.equal(hash(await readFile(launcher)),launcherHash);assert.equal((await snapshotUninstall(programRoot)).displayVersion?.data,'1.0.9');outcomes.visibleCommitResultLost=uncertain;
  await until(async()=>!alive(uncertain.helper.pid),'committed final owner exit before OS entry dispatch');outcomes.finalOSEntries=await exerciseOSEntries('1.0.9');
  assert.equal(await readFile(join(profile,'device-sentinel'),'utf8'),'encrypted-device-data-survives');assert.equal(hash(await readFile(join(desktop,'src/update-keys.ts'))),productionKeysHash);
  await mkdir(join(desktop,'test-results'),{recursive:true});await writeFile(join(desktop,'test-results/windows-installed-upgrade.json'),JSON.stringify({platform:'win32',arch:'x64',...outcomes,productionTrustUnchanged:true,osProtectionsUnchanged:true,stableLauncherUnchanged:true},null,2));
 }catch(error){primaryError=error;await mkdir(join(desktop,'test-results'),{recursive:true});await writeFile(join(desktop,'test-results/windows-installed-upgrade-error.json'),JSON.stringify({error:String(error),root,profile,programRoot,outcomes,log:await readFile(join(root,'process.log'),'utf8')},null,2));throw error;}
 finally{for(const pid of owned)await stop(pid).catch(()=>{});if(currentPid)await stop(currentPid).catch(()=>{});try{if(installedFixture)execFileSync(join(programRoot,'Uninstall '+product+'.exe'),['/S'],{stdio:['ignore',log.fd,log.fd],timeout:60000});}catch{}let cleanupError;if(protocolLease){try{const remaining=runWindowsEntry('cleanup',entryContext);assert.equal(remaining.classes,false);assert.equal(remaining.userChoice,false);}catch(error){cleanupError=error;}}if(server)await new Promise(done=>server.close(done));await log.close();if(!process.env.DILLA_KEEP_UPDATE_FIXTURE){await rm(root,{recursive:true,force:true,maxRetries:10,retryDelay:100});if(profileCreated)await rm(profile,{recursive:true,force:true,maxRetries:10,retryDelay:100});}if(cleanupError&&!primaryError)throw cleanupError;}
});
