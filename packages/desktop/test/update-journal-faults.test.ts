import { test } from 'node:test';
import assert from 'node:assert/strict';
import { writeFile, readFile } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installedFixture } from './support/installed-update.ts';
import { fixtureRuntimePath } from './support/fixture-update-runtime.ts';

// The child installs the fault before importing updater modules, so even a
// captured fs.open sees the real open/write/fsync failure boundary. All program
// files, old-tree renames, ticket reads and cleanup remain real filesystem work.
const scenario = `
import fs from 'node:fs/promises';
const options=JSON.parse(await fs.readFile(process.argv[1],'utf8'));
const {plan,phase,fault,result,module}=options;
const target=(plan.platform==='win32'?{installing:1,recovering:2,failed:3,complete:4}:{installing:1,recovering:4,failed:5,complete:4})[phase];
const original=fs.open;let writes=0,injections=0;
fs.open=async function(file,...args){
 const journal=String(file).includes('journal.json.');const index=journal?++writes:0;
 const fail=journal&&index>=target;
 const error=()=>{injections++;return Object.assign(new Error('injected journal '+fault+' ENOSPC'),{code:'ENOSPC'});};
 if(fail&&fault==='open')throw error();
 const handle=await original(file,...args);
 if(fail&&fault==='write')handle.writeFile=async()=>{throw error()};
 if(fail&&fault==='sync')handle.sync=async()=>{throw error()};
 return handle;
};
const {runInstalledUpdate}=await import(module);
const {reportStartupHealth}=await import(options.healthModule);
let oldLaunches=0,stops=0,now=0,error,outcome;
try{
 outcome=await runInstalledUpdate(plan,{
  waitParent:async()=>{},restoreWindowsMetadata:async()=>{},
  installWindows:async()=>{await fs.writeFile(plan.installed,'partial-or-new-NSIS');if(phase!=='complete')throw new Error('NSIS failed')},
  stop:async()=>{stops++},
  launch:async(file,health)=>{if(!health)oldLaunches++;else if(plan.platform==='win32'||phase==='complete')await reportStartupHealth(plan.userData,health.version,process.pid,health.file,health.nonce);return{pid:process.pid,exitCode:null,signalCode:null}}
 },{now:()=>now,sleep:async()=>{now+=100}});
}catch(e){error=String(e)}
await fs.writeFile(result,JSON.stringify({error,outcome,oldLaunches,stops,writes,injections,target,installed:await fs.readFile(plan.installed,'utf8')}));
`;
for (const platform of ['linux', 'win32'] as const) {
for (const phase of ['installing', 'recovering', 'failed', 'complete'] as const) {
 for (const fault of ['open', 'write', 'sync'] as const) {
  test(`${platform} journal ${fault} ENOSPC during ${phase} cannot strand a working program or pretend a durable completion`, {
   skip: platform === 'linux' && process.platform === 'win32' ? 'AppImage replacement requires POSIX directory fsync; Windows NSIS journal faults still run on every host' : false,
  }, async () => installedFixture(async (plan, _trust, root) => {
   const control = join(root, 'fault.json'), result = join(root, 'result.json');
   // URL-sensitive names exercise the child import boundary on every host;
   // each proxy exports the actual module after the child installs its faults.
   const module = join(root, 'watchdog # %.mjs'), healthModule = join(root, 'health # %.mjs');
   await writeFile(module, `export const {runInstalledUpdate}=(await import(${JSON.stringify(pathToFileURL(fixtureRuntimePath).href)})).watchdog;`);
   await writeFile(healthModule, `export const {reportStartupHealth}=(await import(${JSON.stringify(pathToFileURL(fixtureRuntimePath).href)})).health;`);
   await writeFile(control, JSON.stringify({ plan, phase, fault, result, module: pathToFileURL(module).href, healthModule: pathToFileURL(healthModule).href }));
   const child = spawnSync(process.execPath, ['--input-type=module', '--eval', scenario, control], { encoding: 'utf8', timeout: 10000 });
   assert.equal(child.status, 0, child.stderr);
   const observed = JSON.parse(await readFile(result, 'utf8'));
   assert.ok(observed.writes >= observed.target, 'the named journal phase must reach its target write');
   assert.ok(observed.injections > 0, 'the named ENOSPC fault must actually fire before accepting the recovery outcome');
   assert.equal(observed.error, undefined);
   assert.equal(observed.outcome.durable, false);
   assert.equal(observed.installed, 'working-old-app');
   assert.equal(observed.oldLaunches, 1);
   assert.equal(observed.outcome.phase, 'failed');
  }, platform));
 }
}
}
