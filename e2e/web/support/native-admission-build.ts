import {createHash} from 'node:crypto';
import {lstat,readFile,readdir} from 'node:fs/promises';
import {isAbsolute,join,resolve} from 'node:path';
import {parseLinuxAdmission} from '../../../packages/desktop/src/linux-admission.ts';
import {parseNativePackageIdentity} from '../../../packages/desktop/src/linux-installation.ts';
// @ts-ignore The version mapper is shared with release tooling.
import {linuxPackageVersion} from '../../../packages/desktop/scripts/linux-package-version.mjs';

type Sources={main:string;admission:string;installation:string};
type Fixture={root:string;gate:string;uid:number;gid:number;sandbox:string;profile:string;nonce:string;instance:string};
const digest=(bytes:Uint8Array)=>createHash('sha512').update(bytes).digest('base64');
// Electron v45.2.1 Linux x64: helper extracted from the ZIP whose SHA256
// 616efc1ae4784af1a4f3f0e8774e73e7030e7bd1708da9d09bcf543dfa499beb
// matches the Electron project's published release SHASUMS256.txt.
export function assertPinnedElectronHelper(bytes:Uint8Array):void {if(createHash('sha256').update(bytes).digest('hex')!=='f16e2562837856845cfc8ebf4aa7f78b57fdadc6b69130509b1db87ce5d2f869')throw Error('Unqualified Electron sandbox helper');}
function replaceExact(source:string,before:string,after:string,count:number,label:string,manifest:string[]):string {
 const parts=source.split(before);if(parts.length!==count+1)throw new Error('Fixture source boundary changed: '+label);
 manifest.push(label+':'+count);return parts.join(after);
}
/** Compile-only path/owner substitutions. All checks and parsers remain actual source. */
export function transformAdmissionSources(sources:Sources,fixture:Fixture) {
 for(const path of [fixture.root,fixture.gate,fixture.sandbox,fixture.profile])if(!isAbsolute(path)||resolve(path)!==path)throw Error('Invalid fixture path');
 if(!/^http:\/\/127\.0\.0\.1:[0-9]{1,5}\/$/.test(fixture.instance))throw Error('Invalid fixture instance');
 if(!Number.isSafeInteger(fixture.uid)||fixture.uid<1||!Number.isSafeInteger(fixture.gid)||fixture.gid<0||!/^[a-f0-9]{64}$/.test(fixture.nonce))throw Error('Invalid fixture identity');
 const manifest:string[]=[];let {main,admission,installation}=sources;
 const sub=(source:string,before:string,after:string,count:number,label:string)=>replaceExact(source,before,after,count,label,manifest);
 installation=sub(installation,"const ROOT = '/usr/lib/dilla/native';",'const ROOT = '+JSON.stringify(fixture.root)+';',1,'identity-root');
 installation=sub(installation,"['/', '/usr', '/usr/lib', '/usr/lib/dilla', ROOT]",JSON.stringify([fixture.root]),1,'identity-ancestors');
 installation=sub(installation,'stat.uid !== 0 || stat.gid !== 0',`stat.uid !== ${fixture.uid} || stat.gid !== ${fixture.gid}`,1,'identity-owner');
 installation=sub(installation,"path === ROOT + '/chrome-sandbox' ? mode !== 0o4755", "path === ROOT + '/chrome-sandbox' ? mode !== 0o755",1,'fixture-helper-mode');
 installation=sub(installation,"await updateFS.promises.readlink(child) !== ROOT + '/chrome-sandbox'",'await updateFS.promises.readlink(child) !== '+JSON.stringify(fixture.sandbox),1,'adjacent-ci-helper');
 admission=sub(admission,"const ADDON = '/usr/lib/dilla/native/app/resources/native/dilla-admission.node';",'const ADDON = '+JSON.stringify(join(fixture.root,'app/resources/native/dilla-admission.node'))+';',1,'addon-path');
 admission=sub(admission,"['/', '/usr', '/usr/lib', '/usr/lib/dilla', '/usr/lib/dilla/native', '/usr/lib/dilla/native/app', '/usr/lib/dilla/native/app/resources', '/usr/lib/dilla/native/app/resources/native']",JSON.stringify([fixture.root,join(fixture.root,'app'),join(fixture.root,'app/resources'),join(fixture.root,'app/resources/native')]),1,'addon-ancestors');
 admission=sub(admission,'s.uid !== 0 || s.gid !== 0',`s.uid !== ${fixture.uid} || s.gid !== ${fixture.gid}`,2,'addon-owner');
 const binding="if (admission && (installation.installKind !== 'system-package' || admission.manager !== installation.identity.manager\n    || admission.identitySha512 !== installation.identitySha512 || admission.manager === 'alpm'))";
 main=sub(main,binding,binding,1,'unaltered-main-binding');
 const gateLine="const admission = process.platform === 'linux' && LINUX_INSTALL_KIND === 'system-package' ? holdLinuxAdmission() : undefined;";
 const report=`function __admissionReport(phase:string,pid?:number){const fs=process.getBuiltinModule('fs')!;fs.writeSync(3,JSON.stringify({format:1,phase,pid:process.pid,childPid:pid??null,nonce:${JSON.stringify(fixture.nonce)}})+'\\n');}`;
 main=sub(main,gateLine,gateLine+"\n __admissionReport('gate');",1,'unaltered-main-gate');
 main=sub(main,'async function initializeDesktop(): Promise<void> {',report+'\nasync function initializeDesktop(): Promise<void> {',1,'private-phase-reporter');
 const setup=`__admissionReport('verified'); process.once('SIGUSR1',()=>{const win=BrowserWindow.getAllWindows().find(w=>w.webContents.getURL().startsWith(${JSON.stringify(fixture.instance)}));if(!win)throw Error('fixture window missing');win.close();if(!win.isVisible())__admissionReport('hidden');}); process.once('SIGUSR2',()=>{const child=process.getBuiltinModule('child_process')!.spawn('/usr/bin/sleep',['300'],{stdio:'ignore'});child.unref();__admissionReport('child',child.pid);}); app.setName('Dilla'); app.setPath('userData',${JSON.stringify(fixture.profile)}); app.commandLine.appendSwitch('remote-debugging-address','127.0.0.1'); app.commandLine.appendSwitch('remote-debugging-port','0'); app.commandLine.appendSwitch('use-fake-device-for-media-stream'); const fs=process.getBuiltinModule('fs')!;fs.mkdirSync(${JSON.stringify(fixture.profile)},{recursive:true,mode:0o700});try{fs.writeFileSync(${JSON.stringify(join(fixture.profile,'desktop.json'))},${JSON.stringify(JSON.stringify({version:1,instances:[fixture.instance],autostart:false}))},{flag:'wx',mode:0o600});}catch(error:any){if(error.code!=='EEXIST')throw error;} const originalDialog=dialog.showMessageBox.bind(dialog);dialog.showMessageBox=(async(...args:any[])=>{if(args.at(-1)?.title==='Dilla permission')return {response:1,checkboxChecked:false};return (originalDialog as any)(...args);}) as typeof dialog.showMessageBox;`;
 main=sub(main," app.setName('Dilla');\ntry {",' '+setup+'\ntry {',1,'post-verification-profile');
 main=sub(main,"const allowLoopback = !app.isPackaged && process.argv.includes('--allow-loopback');",'const allowLoopback = true;',1,'fixture-loopback');
 main=sub(main,"app.on('second-instance', (_event, argv) => {", "app.on('second-instance', (_event, argv) => { __admissionReport('forward');",1,'forward-observation');
 return {main,admission,installation,manifest};
}
export async function buildFixtureIdentity(root:string,version:string) {
 const files:{path:string;size:number;sha512:string}[]=[];
 async function walk(dir:string,relative=''):Promise<void> {
  for(const name of await readdir(dir)) {
   const path=relative?relative+'/'+name:name;if(path==='package.json')continue;
   const full=join(dir,name),stat=await lstat(full);
   if(stat.isSymbolicLink()){if(path==='app/chrome-sandbox')continue;throw Error('Unexpected fixture symlink');}
   if(stat.isDirectory())await walk(full,path);
   else if(stat.isFile()&&stat.nlink===1){const bytes=await readFile(full);files.push({path,size:bytes.length,sha512:digest(bytes)});}
   else throw Error('Invalid fixture payload');
  }
 }
 await walk(root);files.sort((a,b)=>a.path<b.path?-1:a.path>b.path?1:0);
 for(const required of ['app/dilla-desktop','app/resources/app.asar','app/resources/native/dilla-admission.node','chrome-sandbox','package-maintainer'])if(!files.some(file=>file.path===required&&file.size>0))throw Error('Missing fixture payload');
 const helper=files.find(file=>file.path==='chrome-sandbox')!,mapped=linuxPackageVersion(version,'x64');
 const identity={format:1,installKind:'system-package',manager:'deb',upstreamVersion:version,packageVersion:mapped.deb.version,arch:'x64',channel:mapped.channel,payload:files,helper:{generation:1,compatibilityEpoch:1,sandboxApi:1,size:helper.size,sha512:helper.sha512}};
 const bytes=Buffer.from(JSON.stringify(identity));parseNativePackageIdentity(bytes,{version,arch:'x64',channel:mapped.channel});
 return {identity,bytes,identitySha512:digest(bytes)};
}
export function validateFixtureReady(raw:string,bytes:Uint8Array):void {
 const status=parseLinuxAdmission(raw);const object=JSON.parse(Buffer.from(bytes).toString());
 const identity=parseNativePackageIdentity(bytes,{version:object.upstreamVersion,channel:object.channel,arch:'x64'});
 if(status.manager!==identity.manager || status.identitySha512!==digest(bytes))throw Error('Fixture identity mismatch');
}

/** Build only in the dedicated GitHub lane; no production build is modified. */
export async function buildFullClientAdmission(instance:string) {
 const fs=await import('node:fs/promises'),{randomBytes}=await import('node:crypto'),{createRequire}=await import('node:module');
 const {build:bundle}=await import('esbuild');const require=createRequire(import.meta.url);
 if(process.platform!=='linux'||process.arch!=='x64'||process.env.GITHUB_ACTIONS!=='true'||process.getuid?.()===0)throw Error('Fixture requires unprivileged Linux CI');
 const runner=process.env.RUNNER_TEMP;if(!runner||!isAbsolute(runner)||await fs.realpath(runner)!==runner)throw Error('Invalid runner temporary directory');
 const runnerStat=await fs.stat(runner);if(!runnerStat.isDirectory()||runnerStat.uid!==process.getuid!())throw Error('Unsafe runner temporary directory');
 const outer=await fs.mkdtemp(join(runner,'dilla-full-admission-'));
 try {
  const base=join(outer,'owned'),root=join(base,'tree'),gate=join(base,'gate'),profile=join(outer,'profile'),project=join(outer,'project'),out=join(outer,'out');
  await fs.mkdir(base,{mode:0o755});await fs.mkdir(gate,{mode:0o755});await fs.mkdir(project,{mode:0o755});await fs.writeFile(join(gate,'lock'),'');
  const repo=resolve(import.meta.dirname,'../../..'),desktop=join(repo,'packages/desktop');
  if(require('electron/package.json').version!=='45.2.1'||JSON.parse(await fs.readFile(join(desktop,'package.json'),'utf8')).devDependencies.electron!=='45.2.1')throw Error('Unqualified Electron version');
  const sandbox=join(repo,'node_modules/electron/dist/chrome-sandbox'),sandboxInfo=await fs.lstat(sandbox);
  if(!sandboxInfo.isFile()||sandboxInfo.isSymbolicLink()||sandboxInfo.uid!==0||sandboxInfo.gid!==0||(sandboxInfo.mode&0o7777)!==0o4755||sandboxInfo.nlink!==1)throw Error('CI Electron helper is not protected');
  const sandboxBytes=await fs.readFile(sandbox);assertPinnedElectronHelper(sandboxBytes);const sandboxDigest=digest(sandboxBytes),nonce=randomBytes(32).toString('hex');
  const opts={root,gate,uid:process.getuid!(),gid:process.getgid!(),sandbox,profile,nonce,instance};
  const sources=Object.fromEntries(await Promise.all([['main','main.ts'],['admission','linux-admission.ts'],['installation','linux-installation.ts']].map(async([name,file])=>[name,await fs.readFile(join(desktop,'src',file),'utf8')])) ) as Sources;
  const transformed=transformAdmissionSources(sources,opts),dist=join(project,'dist');
  await fs.cp(join(desktop,'dist'),dist,{recursive:true});await fs.rm(join(dist,'client'),{recursive:true,force:true});await fs.cp(join(repo,'packages/web/dist'),join(dist,'client'),{recursive:true});
  const version=JSON.parse(await fs.readFile(join(desktop,'package.json'),'utf8')).version;
  await fs.writeFile(join(project,'package.json'),JSON.stringify({name:'dilla-native-admission-acceptance',version,main:'dist/main.cjs',description:'Native admission acceptance',author:'Dilla',license:'AGPL-3.0-or-later'}));
  await bundle({entryPoints:[join(desktop,'src/main.ts')],outfile:join(dist,'main.cjs'),define:{__DILLA_LINUX_INSTALL_KIND__:JSON.stringify('system-package')},bundle:true,platform:'node',target:'node24',format:'cjs',external:['electron','electron-updater','electron-updater/*'],plugins:[{name:'compiled-admission-fixture',setup(plugin){
   for(const [file,contents] of [['main.ts',transformed.main],['linux-admission.ts',transformed.admission],['linux-installation.ts',transformed.installation]] as const)plugin.onLoad({filter:new RegExp('[/\\\\]'+file.replace('.','\\.')+'$')},()=>({contents,loader:'ts',resolveDir:join(desktop,'src')}));
  }}]});
  const addon=join(outer,'dilla-admission.node');
  // @ts-ignore The compiler is shared with the native fixture suite.
  const {buildLinuxAdmission}=await import('../../../packages/desktop/scripts/build-linux-admission.mjs');
  await buildLinuxAdmission({output:addon,fixture:{directory:gate,uid:opts.uid,gid:opts.gid}});
  const {build,Platform,Arch}=require('electron-builder'),production=require(join(desktop,'electron-builder.cjs'));
  await build({targets:Platform.LINUX.createTarget(['dir'],Arch.x64),projectDir:project,publish:'never',config:{appId:'dev.dilla.desktop.admission.acceptance',productName:'DillaAdmissionAcceptance',executableName:'dilla-desktop',electronVersion:'45.2.1',electronDist:join(repo,'node_modules/electron/dist'),directories:{output:out},files:['dist/**','package.json'],asar:true,electronFuses:production.electronFuses,extraResources:[{from:addon,to:'native/dilla-admission.node'}],linux:{target:['dir'],category:'Network',icon:join(desktop,'assets/icon.png')},afterPack:async(context:any)=>{const adjacent=join(context.appOutDir,'chrome-sandbox');await fs.rm(adjacent,{force:true});await fs.symlink(sandbox,adjacent);}}});
  await fs.mkdir(root,{mode:0o755});await fs.cp(join(out,'linux-unpacked'),join(root,'app'),{recursive:true,verbatimSymlinks:true});
  const {getCurrentFuseWire,FuseV1Options,FuseState}=require('@electron/fuses'),wire=await getCurrentFuseWire(join(root,'app/dilla-desktop'));
  for(const key of [FuseV1Options.RunAsNode,FuseV1Options.EnableNodeOptionsEnvironmentVariable,FuseV1Options.EnableNodeCliInspectArguments,FuseV1Options.GrantFileProtocolExtraPrivileges])if(wire[key]!==FuseState.DISABLE)throw Error('Fixture Electron fuse mismatch');
  for(const key of [FuseV1Options.EnableCookieEncryption,FuseV1Options.OnlyLoadAppFromAsar,FuseV1Options.EnableEmbeddedAsarIntegrityValidation])if(wire[key]!==FuseState.ENABLE)throw Error('Fixture Electron fuse mismatch');
  await fs.copyFile(sandbox,join(root,'chrome-sandbox'));await fs.chmod(join(root,'chrome-sandbox'),0o755);
  await fs.writeFile(join(root,'package-maintainer'),'inert fixture maintainer\n',{mode:0o755});
  const adjacent=join(root,'app/chrome-sandbox');
  const verifySandbox=async()=>{const helperAfter=await fs.lstat(sandbox);
   if(await fs.readlink(adjacent)!==sandbox||!helperAfter.isFile()||helperAfter.isSymbolicLink()||helperAfter.uid!==0||helperAfter.gid!==0||(helperAfter.mode&0o7777)!==0o4755||helperAfter.nlink!==1||helperAfter.dev!==sandboxInfo.dev||helperAfter.ino!==sandboxInfo.ino||digest(await fs.readFile(sandbox))!==sandboxDigest)throw Error('CI helper changed');};
  await verifySandbox();
  const identity=await buildFixtureIdentity(root,version);if(identity.identity.helper.sha512!==sandboxDigest)throw Error('Fixture helper bytes differ');await fs.writeFile(join(root,'package.json'),identity.bytes,{mode:0o644});
  const ready={format:1,manager:'deb',state:'ready',transaction:randomBytes(32).toString('hex'),identitySha512:identity.identitySha512,alpmHooks:null};
  await fs.writeFile(join(gate,'status.json'),JSON.stringify(ready),{mode:0o644});
  return {outer,root,gate,profile,nonce,ready,version,identity,manifest:transformed.manifest,sandboxDigest,verifySandbox,cppFixture:{ancestors:'owned-base-only',filesystems:'production-plus-tmpfs-overlay',uid:opts.uid,gid:opts.gid,flags:['-std=c++17','-O2','-Wall','-Wextra','-Werror','-shared','-fPIC','-pthread','-DNAPI_VERSION=8','-DNODE_GYP_MODULE_NAME=dilla_admission','-I:<node24-headers>','-DDILLA_ADMISSION_FIXTURE=1','-DDILLA_ADMISSION_FIXTURE_DIRECTORY:<gate>','-DDILLA_ADMISSION_FIXTURE_BASE:<owned-base>','-DDILLA_ADMISSION_FIXTURE_UID:<uid>','-DDILLA_ADMISSION_FIXTURE_GID:<gid>']},dispose:()=>fs.rm(outer,{recursive:true,force:true})};
 } catch(error){await fs.rm(outer,{recursive:true,force:true});throw error;}
}
