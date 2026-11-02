import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile,mkdtemp,mkdir,writeFile,rm} from 'node:fs/promises';
import {join,resolve} from 'node:path';
import {tmpdir} from 'node:os';
import {createHash} from 'node:crypto';
import {transformAdmissionSources, buildFixtureIdentity, validateFixtureReady} from '../web/support/native-admission-build.ts';
const desktop=resolve(import.meta.dirname,'../../packages/desktop');
const load=async name=>readFile(join(desktop,'src',name),'utf8');
const opts={root:'/private/admission/tree',gate:'/private/admission/gate',uid:1000,gid:1000,sandbox:'/trusted/chrome-sandbox',profile:'/private/admission/profile',instance:'http://127.0.0.1:8481/',nonce:'a'.repeat(64)};
const sha=x=>createHash('sha512').update(x).digest('base64');
test('acceptance transformations reject missing or duplicated production anchors',async()=>{
 const sources={main:await load('main.ts'),admission:await load('linux-admission.ts'),installation:await load('linux-installation.ts')};
 const transformed=transformAdmissionSources(sources,opts);
 assert.ok(transformed.manifest.length>=8);
 for(const [key,anchor] of [['main',"const admission = process.platform"],['admission',"const ADDON ="],['installation',"const ROOT ="]]){
  const modified={...sources,[key]:sources[key].replace(anchor,'missing-anchor')};
  assert.throws(()=>transformAdmissionSources(modified,opts));
  const duplicate={...sources,[key]:sources[key]+'\n'+sources[key].split('\n').find(line=>line.includes(anchor))};
  assert.throws(()=>transformAdmissionSources(duplicate,opts));
 }
});
test('fixture identity hashes actual payload and strict ready binds exact bytes and manager',async t=>{
 const root=await mkdtemp(join(tmpdir(),'dilla-identity-fixture-'));t.after(()=>rm(root,{recursive:true,force:true}));
 for(const [path,contents] of Object.entries({'app/dilla-desktop':'elf','app/resources/app.asar':'asar','app/resources/native/dilla-admission.node':'addon','chrome-sandbox':'helper','package-maintainer':'maintainer'})){
  const file=join(root,path);await mkdir(join(file,'..'),{recursive:true});await writeFile(file,contents);
 }
 const result=await buildFixtureIdentity(root,'0.1.0-beta.1');
 assert.equal(result.identitySha512,sha(result.bytes));assert.equal(result.identity.payload.find(x=>x.path==='app/resources/native/dilla-admission.node').sha512,sha('addon'));
 const ready={format:1,manager:'deb',state:'ready',transaction:'a'.repeat(64),identitySha512:result.identitySha512,alpmHooks:null};
 assert.doesNotThrow(()=>validateFixtureReady(JSON.stringify(ready),result.bytes));
 assert.throws(()=>validateFixtureReady(JSON.stringify({...ready,manager:'rpm'}),result.bytes));
 assert.throws(()=>validateFixtureReady(JSON.stringify(ready),Buffer.from(JSON.stringify(result.identity,null,2))));
 await rm(join(root,'app/resources/native/dilla-admission.node'));await assert.rejects(buildFixtureIdentity(root,'0.1.0-beta.1'));
});

test('transformed actual main and resolver compile with the production gate intact',async()=>{
 const {build}=await import('esbuild');const sources={main:await load('main.ts'),admission:await load('linux-admission.ts'),installation:await load('linux-installation.ts')};
 const transformed=transformAdmissionSources(sources,opts);
 const result=await build({entryPoints:[join(desktop,'src/main.ts')],bundle:true,write:false,platform:'node',format:'cjs',external:['electron','electron-updater','electron-updater/*'],define:{__DILLA_LINUX_INSTALL_KIND__:JSON.stringify('system-package')},plugins:[{name:'fixture-source',setup(plugin){
  for(const [file,contents] of [['main.ts',transformed.main],['linux-admission.ts',transformed.admission],['linux-installation.ts',transformed.installation]])plugin.onLoad({filter:new RegExp('[/\\\\]'+file.replace('.','\\.')+'$')},()=>({contents,loader:'ts',resolveDir:join(desktop,'src')}));
 }}]});
 assert.ok(result.outputFiles[0].contents.length>0);
});

test('actual compile-distinct resolver rejects payload mutation and a missing addon',{skip:process.platform!=='linux'||process.getuid()===0},async t=>{
 const {build}=await import('esbuild'),{createRequire}=await import('node:module');const {chmod,symlink}=await import('node:fs/promises');
 const outer=await mkdtemp(join(tmpdir(),'dilla-resolver-fixture-'));t.after(()=>rm(outer,{recursive:true,force:true}));
 const root=join(outer,'tree');await mkdir(root,{mode:0o755});
 for(const [path,value] of Object.entries({'app/dilla-desktop':'elf','app/resources/app.asar':'asar','app/resources/native/dilla-admission.node':'addon','chrome-sandbox':'helper','package-maintainer':'maintainer'})){
  const file=join(root,path);await mkdir(join(file,'..'),{recursive:true,mode:0o755});await writeFile(file,value);if(['app/dilla-desktop','chrome-sandbox','package-maintainer'].includes(path))await chmod(file,0o755);
 }
 const sandbox=join(outer,'ci-helper');await writeFile(sandbox,'helper');await symlink(sandbox,join(root,'app/chrome-sandbox'));
 const fixture={...opts,root,gate:join(outer,'gate'),uid:process.getuid(),gid:process.getgid(),sandbox,profile:join(outer,'profile')};
 const sources={main:await load('main.ts'),admission:await load('linux-admission.ts'),installation:await load('linux-installation.ts')},transformed=transformAdmissionSources(sources,fixture);
 const result=await buildFixtureIdentity(root,'0.1.0-beta.1');await writeFile(join(root,'package.json'),result.bytes);
 const output=join(outer,'resolver.cjs');await build({entryPoints:[join(desktop,'src/linux-installation.ts')],bundle:true,platform:'node',format:'cjs',outfile:output,plugins:[{name:'fixture-resolver',setup(plugin){plugin.onLoad({filter:/[\\/]linux-installation\.ts$/},()=>({contents:transformed.installation,loader:'ts',resolveDir:join(desktop,'src')}));}}]});
 const {resolveLinuxInstallation}=createRequire(import.meta.url)(output);
 const context={installKind:'system-package',version:'0.1.0-beta.1',channel:'beta',arch:'x64',executable:join(root,'app/dilla-desktop'),resourcesPath:join(root,'app/resources')};
 assert.equal((await resolveLinuxInstallation(context)).identitySha512,result.identitySha512);
 const payload=join(root,'app/resources/app.asar');await writeFile(payload,'ASAR');await assert.rejects(resolveLinuxInstallation(context));await writeFile(payload,'asar');
 const addon=join(root,'app/resources/native/dilla-admission.node');await rm(addon);await assert.rejects(resolveLinuxInstallation(context));
});

test('actual compiled main rejects busy, pending and changed fixture bytes before creating a profile',{skip:process.platform!=='linux'||process.getuid()===0},async t=>{
 const {build}=await import('esbuild'),{spawn,spawnSync}=await import('node:child_process'),{chmod,symlink,access,rename}=await import('node:fs/promises');
 const {buildLinuxAdmission}=await import('../../packages/desktop/scripts/build-linux-admission.mjs');
 const outer=await mkdtemp(join(tmpdir(),'dilla-full-gate-')),base=join(outer,'owned'),root=join(base,'tree'),gate=join(base,'gate'),profile=join(outer,'profile');t.after(()=>rm(outer,{recursive:true,force:true}));
 await mkdir(base,{mode:0o755});await mkdir(root,{mode:0o755});await mkdir(gate,{mode:0o755});await writeFile(join(gate,'lock'),'');
 for(const [path,value] of Object.entries({'app/dilla-desktop':'elf','app/resources/app.asar':'asar','chrome-sandbox':'helper','package-maintainer':'maintainer'})){
  const file=join(root,path);await mkdir(join(file,'..'),{recursive:true,mode:0o755});await writeFile(file,value);if(['app/dilla-desktop','chrome-sandbox','package-maintainer'].includes(path))await chmod(file,0o755);
 }
 const sandbox=join(outer,'ci-helper');await writeFile(sandbox,'helper');await symlink(sandbox,join(root,'app/chrome-sandbox'));
 const addon=join(root,'app/resources/native/dilla-admission.node');await mkdir(join(addon,'..'),{recursive:true,mode:0o755});await buildLinuxAdmission({output:addon,fixture:{directory:gate,uid:process.getuid(),gid:process.getgid()}});
 const fixture={...opts,root,gate,uid:process.getuid(),gid:process.getgid(),sandbox,profile};const sources={main:await load('main.ts'),admission:await load('linux-admission.ts'),installation:await load('linux-installation.ts')},transformed=transformAdmissionSources(sources,fixture);
 const identity=await buildFixtureIdentity(root,'0.1.0-beta.1');await writeFile(join(root,'package.json'),identity.bytes);
 const ready={format:1,manager:'deb',state:'ready',transaction:'a'.repeat(64),identitySha512:identity.identitySha512,alpmHooks:null};
 const compile=async(name,main)=>{const output=join(outer,name);await build({entryPoints:[join(desktop,'src/main.ts')],outfile:output,bundle:true,platform:'node',format:'cjs',external:['electron','electron-updater','electron-updater/*'],define:{__DILLA_LINUX_INSTALL_KIND__:JSON.stringify('system-package')},plugins:[{name:'fixture-main',setup(plugin){for(const [file,contents] of [['main.ts',main],['linux-admission.ts',transformed.admission],['linux-installation.ts',transformed.installation]])plugin.onLoad({filter:new RegExp('[/\\\\]'+file.replace('.','\\.')+'$')},()=>({contents,loader:'ts',resolveDir:join(desktop,'src')}));}}]});return output;};
 const compiled=await compile('main.cjs',transformed.main);
 const source=`const fs=require('node:fs'),vm=require('node:vm'),events=require('node:events');const app=Object.assign(new events.EventEmitter(),{getVersion:()=> '0.1.0-beta.1',isPackaged:true,enableSandbox(){},setName(){},setPath(){},commandLine:{appendSwitch(){}},exit:c=>process.exit(c)});const electron={app,dialog:{showMessageBox:async()=>({response:0})},protocol:{registerSchemesAsPrivileged(){}},nativeTheme:{},ipcMain:{on(){},handle(){}}};const module={exports:{}};vm.runInNewContext(fs.readFileSync(process.argv[1],'utf8'),{module,exports:module.exports,require:n=>n==='electron'?electron:n==='electron-updater'?{}:n.includes('/providers/Provider')?{Provider:class{}}:require(n),process:{platform:'linux',arch:'x64',pid:process.pid,argv:['dilla'],env:{},versions:{},execPath:${JSON.stringify(join(root,'app/dilla-desktop'))},resourcesPath:${JSON.stringify(join(root,'app/resources'))},cwd:()=>process.cwd(),once(){},getBuiltinModule:process.getBuiltinModule},Buffer,TextDecoder,URL,AbortController,Error,setTimeout,clearTimeout,console:{error(){}}});`;
 const refuse=async(gateExpected)=>{const run=spawnSync(process.execPath,['-e',source,compiled],{stdio:['ignore','pipe','pipe','pipe'],timeout:5000});assert.equal(run.status,1);const reports=run.output[3].toString().trim().split('\n').filter(Boolean).map(x=>JSON.parse(x).phase);assert.deepEqual(reports,gateExpected?['gate']:[]);await assert.rejects(access(profile),{code:'ENOENT'});};
 await writeFile(join(gate,'status.json'),JSON.stringify({...ready,state:'pending'}));await refuse(false);
 await writeFile(join(gate,'status.json'),JSON.stringify({...ready,manager:'rpm'}));await refuse(true);
 await writeFile(join(gate,'status.json'),JSON.stringify({...ready,identitySha512:Buffer.alloc(64,1).toString('base64')}));await refuse(true);
 await writeFile(join(gate,'status.json'),JSON.stringify(ready));
 const payload=join(root,'app/resources/app.asar');await writeFile(payload,'ASAR');await refuse(true);await writeFile(payload,'asar');
 const held=addon+'.held';await rename(addon,held);try{await refuse(false);}finally{await rename(held,addon);}
 const addonBytes=await readFile(addon);try{const invalid=Buffer.from(addonBytes);invalid[0]=0;await writeFile(addon,invalid);await refuse(false);}finally{await writeFile(addon,addonBytes);}
 try{await writeFile(join(root,'package.json'),JSON.stringify(identity.identity,null,2));await refuse(true);}finally{await writeFile(join(root,'package.json'),identity.bytes);}
 const gateBypass=await compile('gate-bypass.cjs',transformed.main.replace('? holdLinuxAdmission() : undefined;', '? undefined : undefined;'));
 const identityBypass=await compile('identity-bypass.cjs',transformed.main.replace('if (admission && (installation.installKind', 'if (false && admission && (installation.installKind'));
 const mutationPhases=async program=>{const run=spawnSync(process.execPath,['-e',source,program],{stdio:['ignore','pipe','pipe','pipe'],timeout:5000});assert.equal(run.status,1);const phases=run.output[3].toString().trim().split('\n').filter(Boolean).map(x=>JSON.parse(x).phase);await access(profile);await rm(profile,{recursive:true,force:true});return phases;};
 await writeFile(join(gate,'status.json'),JSON.stringify({...ready,state:'pending'}));assert.deepEqual(await mutationPhases(gateBypass),['gate','verified']);
 await writeFile(join(gate,'status.json'),JSON.stringify({...ready,manager:'rpm'}));assert.deepEqual(await mutationPhases(identityBypass),['gate','verified']);
 await writeFile(join(gate,'status.json'),JSON.stringify(ready));
 const lock=spawn('flock',['--no-fork','--exclusive','--nonblock',join(gate,'lock'),process.execPath,'-e',"process.stdout.write('locked');setInterval(()=>{},1000)"],{stdio:['ignore','pipe','ignore']});
 try{await new Promise(done=>lock.stdout.once('data',done));await refuse(false);assert.deepEqual(await mutationPhases(gateBypass),['gate','verified']);}finally{lock.kill();await new Promise(done=>lock.once('exit',done));}
});
test('pinned helper identity rejects an initial same-size substituted file',async()=>{
 const {assertPinnedElectronHelper}=await import('../web/support/native-admission-build.ts');
 const {readFileSync}=await import('node:fs');const actual=readFileSync(resolve(import.meta.dirname,'../../node_modules/electron/dist/chrome-sandbox'));
 assert.doesNotThrow(()=>assertPinnedElectronHelper(actual));const altered=Buffer.from(actual);altered[0]^=1;assert.throws(()=>assertPinnedElectronHelper(altered));
});
test('production main compilation contains fixed authority without acceptance instrumentation',async()=>{
 const {build}=await import('esbuild');const result=await build({entryPoints:[join(desktop,'src/main.ts')],bundle:true,write:false,platform:'node',format:'cjs',external:['electron','electron-updater','electron-updater/*'],define:{__DILLA_LINUX_INSTALL_KIND__:JSON.stringify('system-package')}});
 const compiled=result.outputFiles[0].text;assert.ok(compiled.includes('/usr/lib/dilla/native'));assert.ok(!compiled.includes('__admissionReport'));assert.ok(!compiled.includes(opts.root));assert.ok(!compiled.includes(opts.nonce));
});
test('owned peer device is identified by the real voice roster helper without a window recorder',async()=>{
 const ts=(await import('typescript')).default,{transform}=await import('esbuild'),vm=await import('node:vm'),{expect}=await import('@playwright/test');
 const voice=await readFile(new URL('../voice/support/voice.ts',import.meta.url),'utf8'),parsed=ts.createSourceFile('voice.ts',voice,ts.ScriptTarget.Latest,true,ts.ScriptKind.TS);
 const declaration=parsed.statements.find(node=>ts.isClassDeclaration(node)&&node.name?.text==='VoicePeer');assert.ok(declaration);
 const compiled=await transform(declaration.getText(parsed),{loader:'ts',format:'cjs'});const module={exports:{}};vm.runInNewContext(compiled.code,{module,exports:module.exports,expect,POLL:{timeout:1000}});const {VoicePeer}=module.exports;
 const {captureNewPeerDevice}=await import('../web/support/native-admission-app.ts');
 const owner='1'.repeat(32),primary='2'.repeat(32),other='3'.repeat(32);let roster=[owner,primary];
 const seen=[];const peer=Object.create(VoicePeer.prototype);peer.setupAnswer={channel_id:'fixture-channel'};peer.sync=async channel=>{seen.push(channel);return {members:roster.length};};peer.devices=async()=>roster;
 const before=await peer.seat(1);roster=[other,primary,owner];assert.equal(await captureNewPeerDevice(peer,before),other);assert.deepEqual(seen,['fixture-channel','fixture-channel']);
 for(const changed of [[other,'4'.repeat(32),owner],[other,other,owner],[other,primary,'bad']]){roster=changed;await assert.rejects(captureNewPeerDevice(peer,before));}
 await assert.rejects(captureNewPeerDevice(peer,[owner,owner]));
});
