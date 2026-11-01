import { test } from 'node:test';
import assert from 'node:assert/strict';
import { takeStartupLaunch, updateLaunchEnvironment, watchdogNonce } from '../src/update-runtime.ts';

test('watchdog role accepts one strict nonce and rejects malformed or repeated role arguments', () => {
 const nonce = 'a'.repeat(64);
 assert.equal(watchdogNonce(['app', '--hidden']), null);
 assert.equal(watchdogNonce(['app', `--update-watchdog=${nonce}`]), nonce);
 for (const args of [['app', '--update-watchdog=../../plan'], ['app', '--update-watchdog'], ['app', `--update-watchdog=${nonce}`, `--update-watchdog=${nonce}`]]) assert.throws(() => watchdogNonce(args), /watchdog/);
});

test('launch health environment is consumed immediately and never inherited by a helper or unrelated child', () => {
 const environment = { PATH: '/bin', DILLA_UPDATE_STARTUP_NONCE: 'a'.repeat(64), DILLA_UPDATE_PREVIOUS_PID: '1234' };
 assert.deepEqual(takeStartupLaunch(environment), { nonce: 'a'.repeat(64), previousPid: '1234' });
 assert.deepEqual(environment, { PATH: '/bin' });
 assert.deepEqual(updateLaunchEnvironment(environment), { PATH: '/bin' });
 assert.deepEqual(updateLaunchEnvironment({ PATH: '/bin', DILLA_UPDATE_STARTUP_NONCE: 'stale', DILLA_UPDATE_PREVIOUS_PID: '999' }, 'b'.repeat(64)), { PATH: '/bin', DILLA_UPDATE_STARTUP_NONCE: 'b'.repeat(64) });
});

test('a replacement watchdog cannot inherit an earlier helper acknowledgment', { skip: process.platform === 'win32' }, async () => {
 const { installedFixture } = await import('./support/installed-update.ts');
 const { writeFile, chmod, readFile } = await import('node:fs/promises');
 const { join } = await import('node:path');
 const { launchUpdateWatchdog } = await import('../src/update-runtime.ts');
 await installedFixture(async plan => {
  const ready = join(plan.health.directory, 'watchdog-ready.json');
  await writeFile(ready, JSON.stringify({ nonce: plan.health.nonce, version: plan.oldVersion, pid: process.pid + 100 }), { mode: 0o600 });
  await writeFile(plan.watchdog, `#!${process.execPath}\nconst fs=require('node:fs');setTimeout(()=>fs.writeFileSync(${JSON.stringify(ready)},JSON.stringify({nonce:${JSON.stringify(plan.health.nonce)},version:${JSON.stringify(plan.oldVersion)},pid:process.pid}),{mode:0o600}),200);setInterval(()=>{},1000);`);
  await chmod(plan.watchdog, 0o700);
  let pid: number | undefined;
  try { await launchUpdateWatchdog(plan); pid = JSON.parse(await readFile(ready, 'utf8')).pid; assert.ok(pid && pid !== process.pid + 100); }
  finally { if (pid) process.kill(pid, 'SIGTERM'); }
 });
});

test('Linux resolves a wrapper launch to the exact owned application main PID before startup health', { skip: process.platform !== 'linux' }, async () => {
 const { installedFixture } = await import('./support/installed-update.ts');
 const { writeFile, readFile, copyFile, mkdir, unlink } = await import('node:fs/promises');
 const { join } = await import('node:path');
 const { nativeWatchdogProcesses } = await import('../src/update-runtime.ts');
 await installedFixture(async plan => {
  const runtime = join(plan.health.directory, 'runtime'), runtimeNode = join(runtime, 'node');
  await mkdir(runtime); await copyFile(process.execPath, runtimeNode);
  const main = join(plan.health.directory, 'application.cjs'), wrapperPid = join(plan.health.directory, 'wrapper-pid');
  await writeFile(main, `const fs=require('node:fs');fs.writeFileSync(${JSON.stringify(plan.health.file)},JSON.stringify({nonce:process.env.DILLA_UPDATE_STARTUP_NONCE,version:${JSON.stringify(plan.health.version)},pid:process.pid}),{mode:0o600});setInterval(()=>{},1000);`);
  const { execFileSync } = await import('node:child_process');
  const wrapper = join(plan.health.directory, 'wrapper.c');
  await writeFile(wrapper, `#include <unistd.h>\n#include <stdio.h>\n#include <sys/wait.h>\nint main(int argc,char **argv){FILE *f=fopen(${JSON.stringify(wrapperPid)},"w");fprintf(f,"%d",getpid());fclose(f);pid_t child=fork();if(!child){execl(${JSON.stringify(runtimeNode)},${JSON.stringify(runtimeNode)},${JSON.stringify(main)},argv[1],NULL);return 99;}int status;waitpid(child,&status,0);return WIFEXITED(status)?WEXITSTATUS(status):1;}`);
  execFileSync('cc', [wrapper, '-o', plan.installed]);

  const processes = nativeWatchdogProcesses(plan), child = await processes.launch(plan.installed, plan.health);
  try {
   const end = Date.now() + 3000; let report;
   while (Date.now() < end) { try { report = JSON.parse(await readFile(plan.health.file, 'utf8')); break; } catch { await new Promise(done => setTimeout(done, 20)); } }
   assert.ok(report); assert.equal(child.pid, report.pid); assert.notEqual(child.pid, Number(await readFile(wrapperPid, 'utf8')));
   const recovered = await processes.recoverLaunches!();
   assert.ok(recovered.some(found => found.pid === child.pid), 'owned main is discovered despite unreadable other-user system processes');
   await unlink(runtimeNode);
   assert.ok((await processes.recoverLaunches!()).some(found => found.pid === child.pid), 'an unlinked executable is still a live owned process and must be stopped');
  } finally { await processes.stop(child); }
 });
});

test('Linux process-title flattened argv keeps one exact launch marker but cannot match renderer arguments or a nonce prefix', async () => {
 const { updateProcessMarker } = await import('../src/update-runtime.ts');
 const marker = '--update-watchdog=' + 'a'.repeat(64);
 assert.equal(updateProcessMarker(['/tmp/a path/dilla-desktop ' + marker, ''], marker), true);
 assert.equal(updateProcessMarker(['/tmp/dilla-desktop', marker, ''], marker), true);
 for (const argv of [['/tmp/dilla-desktop ' + marker + 'b', ''], ['/tmp/dilla-desktop x' + marker, ''], ['/tmp/dilla-desktop --type=renderer ' + marker, ''], ['/tmp/dilla-desktop', '--type=zygote', marker]]) assert.equal(updateProcessMarker(argv, marker), false);
});

test('a recovered-launch marker is consumed, never inherited, and cannot coexist with a startup health launch', () => {
 const nonce = 'c'.repeat(64), env = { PATH: '/bin', DILLA_UPDATE_RECOVERED_NONCE: nonce };
 assert.deepEqual(takeStartupLaunch(env), { recoveredNonce: nonce }); assert.deepEqual(env, { PATH: '/bin' });
 assert.deepEqual(updateLaunchEnvironment({ PATH: '/bin', DILLA_UPDATE_RECOVERED_NONCE: nonce }), { PATH: '/bin' });
 const conflicting = { DILLA_UPDATE_STARTUP_NONCE: nonce, DILLA_UPDATE_RECOVERED_NONCE: nonce };
 assert.throws(() => takeStartupLaunch(conflicting), /conflicting/i); assert.deepEqual(conflicting, {});
});

test('updated main cannot begin ordinary boot until its exact PID is durably registered', async () => {
 const { installedFixture } = await import('./support/installed-update.ts'), { writeUpdateJournal } = await import('../src/update-journal.ts');
 const { waitForRegisteredStartup } = await import('../src/update-runtime.ts');
 await installedFixture(async (plan, trust) => {
  await (await import('../src/update-state.ts')).activateInstalledUpdate(plan);
  await writeUpdateJournal(plan, { phase: 'launching' });
  let slept = false, now = 0;
  await waitForRegisteredStartup(plan.userData, plan.installed, plan.health.nonce, process.pid, { ...trust, version: plan.health.version }, {
   now: () => now, sleep: async () => { slept = true; now += 100; await writeUpdateJournal(plan, { phase: 'launching', childPid: process.pid }); },
  });
  assert.equal(slept, true);
  await assert.rejects(waitForRegisteredStartup(plan.userData, plan.installed, plan.health.nonce, process.pid + 1, { ...trust, version: plan.health.version }), /registered/);
 });
});

test('startup activation waits for durable completion bound to this PID and signed version', { skip: !['linux', 'win32'].includes(process.platform) }, async () => {
 const { installedFixture } = await import('./support/installed-update.ts'), { writeUpdateJournal } = await import('../src/update-journal.ts');
 const { waitForStartupActivation } = await import('../src/update-runtime.ts'), { createStartupActivation } = await import('../src/update-activation.ts');
 await installedFixture(async plan => {
  await writeUpdateJournal(plan, { phase: 'launching', childPid: process.pid });
  const gate = await createStartupActivation(plan, 1000); gate.expect(process.pid); let ready = false;
  try {
   const waiting = waitForStartupActivation(plan, process.pid, 1000).then(() => { ready = true; });
   await new Promise(done => setTimeout(done, 30)); assert.equal(ready, false);
   await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid }); await gate.activate(process.pid); await waiting;
   assert.equal(ready, true);
   await assert.rejects(waitForStartupActivation(plan, process.pid + 1), /activation/);
   await writeUpdateJournal(plan, { phase: 'failed', failure: 'startup' });
   await assert.rejects(waitForStartupActivation(plan, process.pid), /activation/);
  } finally { await gate.close(); }
 });
});

test('Linux discovery skips inaccessible noncandidate images but fails closed for an exact marked candidate', { skip: process.platform !== 'linux' }, async () => {
 const { installedFixture } = await import('./support/installed-update.ts'), { spawnSync } = await import('node:child_process'), { resolve, join } = await import('node:path'), { writeFile } = await import('node:fs/promises');
 await installedFixture(async (plan, _trust, root) => {
  const control = join(root, 'process-discovery-control.json'); await writeFile(control, JSON.stringify({ plan, module: resolve(import.meta.dirname, '../src/update-runtime.ts') }), { mode: 0o600 });
  const script = `import fs from 'node:fs/promises';const{plan,module}=JSON.parse(await fs.readFile(process.argv[1],'utf8'));const own='/proc/'+process.pid;const originalRead=fs.readFile;let marked=false,imageReads=0;
fs.readdir=async()=>[String(process.pid)];fs.readFile=async(file,...args)=>String(file)===own+'/cmdline'?(marked?process.execPath+'\\0--update-launch='+plan.health.nonce+'\\0':'protected-daemon\\0'):originalRead(file,...args);
fs.readlink=async()=>{imageReads++;throw Object.assign(new Error('protected process image'),{code:'EACCES'})};
const{syncBuiltinESMExports}=await import('node:module');syncBuiltinESMExports();const{nativeWatchdogProcesses}=await import(module);const processes=nativeWatchdogProcesses(plan);const skipped=await processes.recoverLaunches();const noncandidateReads=imageReads;marked=true;let code;try{await processes.recoverLaunches()}catch(e){code=e.code}process.stdout.write(JSON.stringify({skipped,noncandidateReads,code}));`;
  const result = spawnSync(process.execPath, ['--input-type=module', '--eval', script, control], { encoding: 'utf8', timeout: 10000 });
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(JSON.parse(result.stdout), { skipped: [], noncandidateReads: 0, code: 'EACCES' });
 });
});


test('ordinary startup exits while a verified complete transaction still has its exact live watchdog, even after the registered child died', { skip: process.platform !== 'linux' }, async t => {
 const { installedFixture } = await import('./support/installed-update.ts'), { spawn, spawnSync } = await import('node:child_process'), { readFile, writeFile } = await import('node:fs/promises');
 const { activateInstalledUpdate } = await import('../src/update-state.ts'), { writeUpdateJournal } = await import('../src/update-journal.ts'), { reportWatchdogReady } = await import('../src/update-health.ts');
 const { resumeInterruptedUpdate } = await import('../src/update-runtime.ts');
 await installedFixture(async (plan, trust) => {
  const assertReadHeld = async (fault: string, version: string) => {
   const { join, resolve } = await import('node:path');
   const control = join(plan.health.directory, 'active-read-control.json');
   await writeFile(control, JSON.stringify({ userData: plan.userData, installed: plan.installed, trust: { ...trust, version }, fault, module: resolve(import.meta.dirname, '../src/update-runtime.ts') }), { mode: 0o600 });
   const reading = `import fs from 'node:fs/promises';import{join}from'node:path';const o=JSON.parse(await fs.readFile(process.argv[1],'utf8'));const original=fs.open;fs.open=(file,...args)=>String(file)===o.fault?Promise.reject(Object.assign(new Error('active EIO'),{code:'EIO'})):original(file,...args);const{syncBuiltinESMExports}=await import('node:module');syncBuiltinESMExports();const{resumeInterruptedUpdate}=await import(o.module);let name;try{await resumeInterruptedUpdate(o.userData,o.installed,o.trust)}catch(e){name=e.name}process.stdout.write(JSON.stringify({name}));`;
   const readFault = spawnSync(process.execPath, ['--input-type=module', '--eval', reading, control], { encoding: 'utf8', timeout: 10000 });
   assert.equal(readFault.status, 0, readFault.stderr); assert.deepEqual(JSON.parse(readFault.stdout), { name: 'UpdateStartupHeld' });
  };
  const helper = spawn(plan.watchdog, ['--eval', 'setInterval(()=>{},1000)', '--', '--update-watchdog=' + plan.health.nonce], { detached: true, stdio: 'ignore' });
  await new Promise<void>((done, reject) => { helper.once('spawn', done); helper.once('error', reject); });
  assert.ok(helper.pid);
  try {
   await activateInstalledUpdate(plan); await reportWatchdogReady(plan.userData, plan.oldVersion, helper.pid!, plan.health.nonce);
   assert.equal(await resumeInterruptedUpdate(plan.userData, plan.installed, trust), true, 'prepared owner blocks a second ordinary main before mutation starts');
   await writeUpdateJournal(plan, { phase: 'prepared', watchdogPid: process.pid + 100000 });
   assert.equal(await resumeInterruptedUpdate(plan.userData, plan.installed, trust), true, 'a stale initial journal PID cannot hide the live recovery owner in its authenticated ready record');
   assert.equal(JSON.parse(await readFile(plan.health.directory + '/watchdog-ready.json', 'utf8')).pid, helper.pid, 'the second observer must not clear the first recovery owner acknowledgment');
   await assertReadHeld(plan.health.directory + '/watchdog-ready.json', trust.version);
   await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid + 100000 });
   await writeFile(plan.installed, 'authenticated-new-app');
   assert.equal(await resumeInterruptedUpdate(plan.userData, plan.installed, { ...trust, version: plan.health.version }), true);
   const artifact = await readFile(plan.artifactFile); await writeFile(plan.artifactFile, 'tampered');
   await assert.rejects(resumeInterruptedUpdate(plan.userData, plan.installed, { ...trust, version: plan.health.version }), { name: 'UpdateStartupHeld' });
   await writeFile(plan.artifactFile, artifact);
   await assertReadHeld(plan.userData + '/updates/active.json', plan.health.version);
   await writeUpdateJournal(plan, { phase: 'launching', childPid: process.pid + 100000 });
   await writeFile(plan.artifactFile, 'read fault while live helper may restore');
   await assert.rejects(resumeInterruptedUpdate(plan.userData, plan.installed, { ...trust, version: plan.health.version }), { name: 'UpdateStartupHeld' });
   await writeFile(plan.artifactFile, artifact);
   const fs = (await import('node:fs/promises')).default, original = fs.stat, { syncBuiltinESMExports } = await import('node:module');
   t.mock.method(fs, 'stat', (...args: Parameters<typeof original>) => String(args[0]) === `/proc/${helper.pid}` ? Promise.reject(Object.assign(new Error('cannot inspect owned process'), { code: 'EACCES' })) : original(...args));
   syncBuiltinESMExports();
   try { await assert.rejects(resumeInterruptedUpdate(plan.userData, plan.installed, { ...trust, version: plan.health.version }), { name: 'UpdateStartupHeld' }); }
   finally { t.mock.restoreAll(); syncBuiltinESMExports(); }
   await writeUpdateJournal(plan, { phase: 'complete', childPid: process.pid + 100000 });
  } finally { helper.kill('SIGKILL'); await new Promise<void>(done => helper.once('exit', () => done())); }
  assert.equal(await resumeInterruptedUpdate(plan.userData, plan.installed, { ...trust, version: plan.health.version }), false);
 }, 'linux', await readFile(process.execPath));
});

test('the stable Windows launcher role accepts only one exact bootstrap nonce and cannot combine watchdog authority',async()=>{
 const{bootstrapNonce}=await import('../src/update-runtime.ts'),nonce='a'.repeat(64);
 assert.equal(bootstrapNonce(['app','--hidden']),null);assert.equal(bootstrapNonce(['app','--update-bootstrap='+nonce]),nonce);
 for(const args of [['--update-bootstrap'],['--update-bootstrap=../outside'],['--update-bootstrap='+nonce,'--update-bootstrap='+nonce],['--update-bootstrap='+nonce,'--update-watchdog='+nonce]])assert.throws(()=>bootstrapNonce(args),/bootstrap/);
});
