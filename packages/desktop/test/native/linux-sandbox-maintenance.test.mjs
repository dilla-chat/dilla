// SOURCE REVIEW REQUIRED before wiring the privileged command manifest into CI.
import test from 'node:test';
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {lstat,mkdtemp,readFile,realpath,rm,mkdir,writeFile} from 'node:fs/promises';
import {isAbsolute,join,resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {MAINTENANCE_ROOT,maintenanceCommands,verifyMaintenanceProof,STAGING_PROGRAM,preserveMaintenanceFailure,maintenanceCommandFailure,captureMaintenanceFailure} from '../support/linux-sandbox-maintenance.mjs';

const repository=resolve(import.meta.dirname,'../../../..');
const fixture=fileURLToPath(new URL('../fixtures/linux-sandbox-maintenance/',import.meta.url));
const safeEnv={PATH:'/usr/local/bin:/usr/bin:/bin',LANG:'C',LC_ALL:'C'};
function identity(){
 const values=[process.env.GITHUB_RUN_ID,process.env.GITHUB_RUN_ATTEMPT,process.env.GITHUB_SHA];
 if(!values.slice(0,2).every(x=>typeof x==='string'&&/^[1-9][0-9]{0,19}$/.test(x))||typeof values[2]!=='string'||!/^[0-9a-f]{40}$/.test(values[2]))throw Error('Invalid CI identity');return values;
}
async function ciBoundary(){
 assert.equal(process.platform,'linux');assert.equal(process.arch,'x64');assert.notEqual(process.getuid(),0);
 assert.equal(process.env.GITHUB_ACTIONS,'true');assert.equal(process.env.RUNNER_ENVIRONMENT,'github-hosted');assert.equal(process.env.DILLA_LINUX_SANDBOX_FOUNDATION_LANE,'managed');
 assert.match(await readFile('/etc/os-release','utf8'),/^VERSION_ID="24\.04"$/m);return identity();
}
async function absent(path){try{await lstat(path);return false;}catch(error){if(error.code==='ENOENT')return true;throw error;}}
function command(program,args,options={}){
 const result=spawnSync(program,args,{cwd:repository,env:process.env,encoding:'utf8',timeout:310_000,maxBuffer:1<<20,...options});
 if(result.error||result.signal||result.status!==0)throw maintenanceCommandFailure(result);return result.stdout;
}
function privileged(args,input){return command('/usr/bin/sudo',args,{env:safeEnv,...(input===undefined?{}:{input})});}
async function policy(){
 const result={};for(const name of ['apparmor_restrict_unprivileged_userns','unprivileged_userns_clone']){
  try{const raw=(await readFile('/proc/sys/kernel/'+name,'utf8')).trim();assert.match(raw,/^[0-9]{1,4}$/);result[name]=raw;}catch(error){if(error.code==='ENOENT')result[name]='absent';else throw error;}
 }
 const enabled=(await readFile('/sys/module/apparmor/parameters/enabled','utf8').catch(error=>{if(error.code==='ENOENT')return 'absent';throw error;})).trim();assert.ok(['Y','N','absent'].includes(enabled));result.apparmor=enabled;return result;
}
export async function cleanupMaintenance(){const job=await ciBoundary();privileged(['/usr/bin/timeout','--signal=TERM','--kill-after=5s','30s','/usr/bin/python3','-I','-S','-c',STAGING_PROGRAM,'cleanup',...job]);}
export async function runMaintenance(t){
 const job=await ciBoundary();assert.equal(await absent(MAINTENANCE_ROOT),true,'never adopt preexisting CI root');for(const path of ['/usr/lib/dilla/appimage','/usr/lib/dilla/native'])assert.equal(await absent(path),true);
 const temp=process.env.RUNNER_TEMP;assert.equal(typeof temp,'string');assert.ok(isAbsolute(temp));assert.equal(await realpath(temp),temp);const parent=await lstat(temp);assert.equal(parent.isDirectory(),true);assert.equal(parent.uid,process.getuid());
 const build=await realpath(await mkdtemp(join(temp,'dilla-maintenance-build-')));const proof={format:1,status:'failed',sourceOnly:false,primaryFailure:false,cleanupFailure:false,stage:'build-credentials'};let failure,commands,attempted=false;
 const before=await policy();
 try{
  for(const generation of [1,2])command('cc',['-std=c11','-Wall','-Wextra','-Werror','-O2','-DDILLA_GENERATION='+generation,join(fixture,'credentials.c'),'-o',join(build,'credential-v'+generation)]);
  proof.stage='build-holder';command('cc',['-std=c11','-Wall','-Wextra','-Werror','-O2',join(fixture,'held-fd.c'),'-o',join(build,'held-fd')]);
  const hash=async name=>createHash('sha512').update(await readFile(join(build,name))).digest('base64');
  const now=Date.now(),published=new Date(now-60_000).toISOString(),expires=new Date(now+86_400_000).toISOString();
  const values={nativeRunID:job[0],nativeAttempt:job[1],nativeHead:job[2],nativePublished:published,nativeExpires:expires,nativeHelper1Hash:await hash('credential-v1'),nativeHelper2Hash:await hash('credential-v2')};
  const flags=Object.entries(values).map(([key,value])=>'-X github.com/jonasthim/dilla/internal/desktopsandbox.'+key+'='+value).join(' ');
  proof.stage='build-go';command('go',['test','-c','-tags=dilla_sandbox_native','-ldflags='+flags,'-o',join(build,'native.test'),'./internal/desktopsandbox'],{env:{...process.env,CGO_ENABLED:'0'}});
  proof.stage='preflight';command(join(build,'native.test'),['-test.run=^TestNativeCompiledConfiguration$']);
  const hashes=await Promise.all(['native.test','credential-v1','credential-v2','held-fd'].map(hash));commands=maintenanceCommands({build,identity:job,hashes});
  attempted=true;const stages=['init','copy-native','copy-v1','copy-v2','copy-observer','verify'];for(const [i,args]of commands.slice(0,-2).entries()){proof.stage=stages[i];const source=i>0&&i<5?['native.test','credential-v1','credential-v2','held-fd'][i-1]:undefined;privileged(args,source?await readFile(join(build,source)):undefined);}
  proof.stage='kernel';const observed=verifyMaintenanceProof(privileged(commands.at(-2)));
  proof.stage='policy';assert.deepEqual(await policy(),before,'host security policy changed');for(const path of ['/usr/lib/dilla/appimage','/usr/lib/dilla/native'])assert.equal(await absent(path),true);
  proof.kernel=observed;proof.status='passed';proof.stage='complete';
 }catch(error){failure=captureMaintenanceFailure(proof,error);}finally{
  failure=await preserveMaintenanceFailure(failure,[async()=>{
   if(attempted){try{await cleanupMaintenance();assert.equal(await absent(MAINTENANCE_ROOT),true);}catch(error){proof.cleanupFailure=true;proof.status='failed';if(error.maintenanceDiagnostic)proof.cleanupDiagnostic=error.maintenanceDiagnostic;throw Error('Maintenance fixture cleanup failed');}}
  },async()=>{
   const records=resolve(import.meta.dirname,'../..','test-results');await mkdir(records,{recursive:true});await writeFile(join(records,'linux-sandbox-maintenance-managed.json'),JSON.stringify(proof,null,2)+'\n');
  },()=>rm(build,{recursive:true,force:true})]);
 }
 if(failure){t?.diagnostic(JSON.stringify(proof));throw failure;}
 t?.diagnostic('Root maintenance kernel proof passed; not power-loss or complete installed-product qualification.');return proof;
}
if(process.argv.slice(2).includes('--cleanup-only')){assert.deepEqual(process.argv.slice(2),['--cleanup-only']);await cleanupMaintenance();}else{
 test('disposable root sandbox maintenance kernel proof',{skip:process.env.DILLA_LINUX_SANDBOX_FOUNDATION_LANE!=='managed',timeout:480_000},runMaintenance);
}
