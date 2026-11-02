import { fixtureRuntime } from './support/fixture-update-runtime.ts';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile, writeFile, copyFile } from 'node:fs/promises';
import { basename, join } from 'node:path';
import type { WatchdogProcesses } from '../src/update-watchdog.ts';
const { runInstalledUpdate } = fixtureRuntime.watchdog;
const { readUpdateJournal } = fixtureRuntime.journal;
const { reportStartupHealth } = fixtureRuntime.health;
import { installedFixture } from './support/installed-update.ts';
import type { InstalledUpdatePlan } from '../src/update-plan.ts';

// Exercise the common lifecycle through the native adapter supported by this
// host. Linux still uses actual AppImage replacement/directory durability;
// Windows models its installer through the production process adapter contract.
const lifecyclePlatform = process.platform === 'win32' ? 'win32' : 'linux';
function fixtureInstaller(plan: InstalledUpdatePlan, parentExited: () => boolean, staged: () => void = () => {}): WatchdogProcesses['installWindows'] {
 return async file => {
  assert.equal(plan.platform, 'win32'); assert.equal(file, plan.artifactFile);
  assert.equal(parentExited(), true, 'installer cannot stage before the old parent exits');
  assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
  await copyFile(file, plan.installed); staged();
 };
}

test('native update leaves old bytes installed until parent exit and requires exact child health before completing', async () => installedFixture(async plan => {
 const calls: string[] = []; let parentExited = false;
 const child = { pid: process.pid + 100, exitCode: null, signalCode: null };
 const processes: WatchdogProcesses = {
  waitParent: async pid => { assert.equal(pid, plan.parentPid); assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app'); calls.push('parent-exited'); parentExited = true; },
  launch: async (file, health) => {
   assert.equal(parentExited, true); assert.equal(file, plan.installed); assert.ok(health); assert.equal(await readFile(file, 'utf8'), 'authenticated-new-app');
   calls.push('launch'); await reportStartupHealth(plan.userData, health!.version, child.pid, health!.file, health!.nonce); return child;
  },
  stop: async () => { throw new Error('healthy child must survive'); },
  restoreWindowsMetadata: async () => {}, installWindows: fixtureInstaller(plan, () => parentExited, () => { calls.push('stage'); }),
 };
 let now = 0;
 await runInstalledUpdate(plan, processes, { now: () => now, sleep: async () => { now += 100; } });
 assert.deepEqual(calls, ['parent-exited', ...(lifecyclePlatform === 'win32' ? ['stage'] : []), 'launch']); assert.ok(now >= 1000);
 assert.equal((await readUpdateJournal(plan)).phase, 'complete');
 assert.equal((await readUpdateJournal(plan)).childPid, child.pid);
 assert.equal(await readFile(plan.platform === 'linux' ? plan.installed + '.previous' : join(plan.backup, basename(plan.installed)), 'utf8'), 'working-old-app');
 assert.equal(await readFile(join(plan.userData, 'device-sentinel'), 'utf8'), 'encrypted-device');
}, lifecyclePlatform));

test('a child crash during grace is stopped before native rollback and the old version relaunches with a retryable record', async () => installedFixture(async plan => {
 let parentExited = false;
 const calls: string[] = [], child = { pid: process.pid + 101, exitCode: null as number | null, signalCode: null };
 const processes: WatchdogProcesses = {
  waitParent: async () => { parentExited = true; },
  launch: async (file, health) => {
   assert.equal(parentExited, true);
   if (health) { await reportStartupHealth(plan.userData, health.version, child.pid, health.file, health.nonce); calls.push('new'); return child; }
   assert.equal(await readFile(file, 'utf8'), 'working-old-app'); calls.push('old'); return { ...child, pid: child.pid + 1 };
  },
  stop: async owned => { assert.equal(owned, child); assert.equal(await readFile(plan.installed, 'utf8'), 'authenticated-new-app'); calls.push('stop-owned'); },
  restoreWindowsMetadata: async () => {}, installWindows: fixtureInstaller(plan, () => parentExited),
 };
 let now = 0;
 await runInstalledUpdate(plan, processes, { now: () => now, sleep: async () => { now += 100; if (now >= 500) child.exitCode = 2; } });
 assert.deepEqual(calls, ['new', 'stop-owned', 'old']);
 assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
 assert.deepEqual(await readUpdateJournal(plan), { format: 1, nonce: plan.health.nonce, phase: 'failed', failure: 'startup' });
}, lifecyclePlatform));

test('a missing health report restores the old app while a failed parent handoff never changes installation', async () => installedFixture(async plan => {
 let parentExited = false;
 const child = { pid: process.pid + 101, exitCode: null, signalCode: null };
 const processes: WatchdogProcesses = { waitParent: async () => { throw new Error('parent still running'); }, launch: async () => child, stop: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: fixtureInstaller(plan, () => parentExited) };
 await assert.rejects(runInstalledUpdate(plan, processes), /parent still running/);
 assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
 processes.waitParent = async () => { parentExited = true; };
 let now = 0, old = false, healthLaunches = 0;
 processes.launch = async (_file, health) => { if (!health) old = true; else healthLaunches++; return child; };
 await runInstalledUpdate(plan, processes, { timeoutMs: 500, now: () => now, sleep: async () => { now += 100; } });
 assert.equal(healthLaunches, 1); assert.equal(old, true); assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
}, lifecyclePlatform));

test('a tampered artifact fails before mutation and a failed stop never overwrites a live child', async () => installedFixture(async plan => {
 const child = { pid: process.pid + 101, exitCode: 2, signalCode: null };
 const processes: WatchdogProcesses = { waitParent: async () => {}, launch: async () => child, stop: async () => { throw new Error('cannot stop owned child'); }, restoreWindowsMetadata: async () => {}, installWindows: async () => {} };
 await writeFile(plan.artifactFile, 'tampered');
 await assert.rejects(runInstalledUpdate(plan, processes), /verification/);
 assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
}));

test('a child that cannot be stopped keeps rollback pending and never launches over live program bytes', async () => installedFixture(async plan => {
 const child = { pid: process.pid + 101, exitCode: 2, signalCode: null };
 let launches = 0, parentExited = false;
 const processes: WatchdogProcesses = { waitParent: async () => { parentExited = true; }, launch: async () => { launches++; return child; }, stop: async () => { throw new Error('cannot stop owned child'); }, restoreWindowsMetadata: async () => {}, installWindows: fixtureInstaller(plan, () => parentExited) };
 await assert.rejects(runInstalledUpdate(plan, processes), /cannot stop owned child/);
 assert.equal(launches, 1); assert.equal(await readFile(plan.installed, 'utf8'), 'authenticated-new-app');
 assert.equal((await readUpdateJournal(plan)).phase, 'recovering');
}, lifecyclePlatform));

test('Windows awaits installer completion before direct launch and restores old ASAR, executable and device data on startup failure', async () => installedFixture(async (plan, _trust, root) => {
 const child = { pid: process.pid + 102, exitCode: 2, signalCode: null }, calls: string[] = [];
 const processes: WatchdogProcesses = {
  waitParent: async () => {},
  restoreWindowsMetadata: async () => {}, installWindows: async file => {
   assert.equal(file, plan.artifactFile); calls.push('install');
   await writeFile(plan.installed, 'installed-new-exe'); await writeFile(join(root, 'applications/resources/app.asar'), 'new-asar'); await writeFile(join(root, 'applications/new-runtime.dat'), 'new-only');
  },
  launch: async (file, health) => {
   assert.equal(file, plan.installed);
   if (health) { assert.equal(await readFile(file, 'utf8'), 'installed-new-exe'); calls.push('new'); }
   else { assert.equal(await readFile(file, 'utf8'), 'working-old-app'); calls.push('old'); }
   return child;
  },
  stop: async owned => { assert.equal(owned, child); calls.push('stop'); },
 };
 await runInstalledUpdate(plan, processes);
 assert.deepEqual(calls, ['install', 'new', 'stop', 'old']);
 assert.equal(await readFile(join(root, 'applications/resources/app.asar'), 'utf8'), 'old-asar');
 await assert.rejects(readFile(join(root, 'applications/new-runtime.dat')), { code: 'ENOENT' });
 assert.equal(await readFile(join(plan.userData, 'device-sentinel'), 'utf8'), 'encrypted-device');
 assert.equal((await readUpdateJournal(plan)).failure, 'startup');
}, 'win32'));

test('a failed Windows installer restores a partially overwritten program before relaunching', async () => installedFixture(async plan => {
 const calls: string[] = [];
 await runInstalledUpdate(plan, {
  waitParent: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: async () => { await writeFile(plan.installed, 'partial-NSIS-install'); throw new Error('NSIS exit 2'); },
  stop: async () => { throw new Error('no child exists'); },
  launch: async (file, health) => { assert.equal(health, undefined); assert.equal(await readFile(file, 'utf8'), 'working-old-app'); calls.push('old'); return { pid: process.pid + 100, exitCode: null, signalCode: null }; },
 });
 assert.deepEqual(calls, ['old']); assert.equal((await readUpdateJournal(plan)).failure, 'installer');
}, 'win32'));

test('an interrupted install restores the independently verified old backup without rerunning the artifact', async () => installedFixture(async plan => {
 const { recoverInstalledUpdate } = fixtureRuntime.watchdog;
 const { writeUpdateJournal } = fixtureRuntime.journal;
 await writeFile(plan.installed, 'interrupted-new-app'); await writeUpdateJournal(plan, { phase: 'installing' });
 const calls: string[] = [];
 await recoverInstalledUpdate(plan, {
  waitParent: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: async () => { throw new Error('recovery reran installer'); }, stop: async () => { throw new Error('no owned child'); },
  launch: async file => { assert.equal(await readFile(file, 'utf8'), 'working-old-app'); calls.push('old'); return { pid: process.pid + 100, exitCode: null, signalCode: null }; },
 });
 assert.deepEqual(calls, ['old']); assert.equal((await readUpdateJournal(plan)).failure, 'interrupted');
}));

test('Windows rollback restores the pre-staged old tree even when all further content copies fail ENOSPC', async t => installedFixture(async plan => {
 const { updateFS } = fixtureRuntime.fs;
 t.mock.method(updateFS.promises, 'copyFile', async () => { throw Object.assign(new Error('disk full after NSIS'), { code: 'ENOSPC' }); });
 const processes: WatchdogProcesses = { waitParent: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: async () => { await writeFile(plan.installed, 'partial-new'); throw new Error('NSIS failed'); }, stop: async () => {}, launch: async file => { assert.equal(await readFile(file, 'utf8'), 'working-old-app'); return { pid: process.pid + 100, exitCode: null, signalCode: null }; } };
 try { await runInstalledUpdate(plan, processes); }
 finally { t.mock.restoreAll(); }
 assert.equal(await readFile(plan.installed, 'utf8'), 'working-old-app');
 assert.equal(await readFile(plan.watchdog, 'utf8'), 'working-old-app');
}, 'win32'));

test('a refused Windows registration restore still relaunches the verified old program and keeps a visible retry error', async () => installedFixture(async plan => {
 let relaunched = false;
 await runInstalledUpdate(plan, { waitParent: async () => {}, installWindows: async () => { await writeFile(plan.installed, 'partial-new'); throw new Error('NSIS failed'); }, restoreWindowsMetadata: async () => { throw new Error('owned registration unavailable'); }, stop: async () => {}, launch: async file => { assert.equal(await readFile(file, 'utf8'), 'working-old-app'); relaunched = true; return { pid: process.pid + 100, exitCode: null, signalCode: null }; } });
 assert.equal(relaunched, true); assert.equal((await readUpdateJournal(plan)).failure, 'metadata');
}, 'win32'));

test('Linux interrupted recovery restores a hash-verified on-destination previous AppImage by rename when copies fail ENOSPC', async t => installedFixture(async plan => {
 const { updateFS } = fixtureRuntime.fs, { recoverInstalledUpdate } = fixtureRuntime.watchdog, { writeUpdateJournal } = fixtureRuntime.journal;
 await writeFile(plan.installed + '.previous', 'working-old-app'); await writeFile(plan.installed, 'interrupted-new'); await writeUpdateJournal(plan, { phase: 'installing' });
 t.mock.method(updateFS.promises, 'copyFile', async () => { throw Object.assign(new Error('destination full'), { code: 'ENOSPC' }); });
 let old = false;
 try { await recoverInstalledUpdate(plan, { waitParent: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: async () => { throw new Error('wrong OS'); }, stop: async () => {}, launch: async file => { assert.equal(await readFile(file, 'utf8'), 'working-old-app'); old = true; return { pid: process.pid + 100, exitCode: null, signalCode: null }; } }); }
 finally { t.mock.restoreAll(); }
 assert.equal(old, true); assert.equal(await readFile(plan.backup, 'utf8'), 'working-old-app');
}));

test('recovery discovers and stops an owned launch even if the helper died before recording its PID', async () => installedFixture(async plan => {
 const { writeUpdateJournal } = fixtureRuntime.journal, { recoverInstalledUpdate } = fixtureRuntime.watchdog;
 await writeUpdateJournal(plan, { phase: 'launching' }); await writeFile(plan.installed, 'new-awaiting-registration');
 const child = { pid: process.pid + 100, exitCode: null, signalCode: null }; let stopped = false;
 await recoverInstalledUpdate(plan, { waitParent: async () => {}, restoreWindowsMetadata: async () => {}, installWindows: async () => {},
  recoverLaunches: async () => [child], stop: async value => { assert.equal(value, child); assert.equal(await readFile(plan.installed, 'utf8'), 'new-awaiting-registration'); stopped = true; },
  launch: async file => { assert.equal(stopped, true); assert.equal(await readFile(file, 'utf8'), 'working-old-app'); return child; },
 });
 assert.equal(stopped, true);
}));

test('dropping the activation acknowledgment after durable commit never rolls back a potentially enabled new app', async () => installedFixture(async plan => {
 const child = { pid: process.pid + 100, exitCode: null, signalCode: null };
 let attempted = false, parentExited = false;
 const outcome = await runInstalledUpdate(plan, { waitParent: async () => { parentExited = true; }, restoreWindowsMetadata: async () => {}, installWindows: fixtureInstaller(plan, () => parentExited), stop: async () => { throw new Error('committed app must not be stopped'); },
  activate: async () => { attempted = true; throw new Error('activation ACK dropped'); },
  launch: async (_file, health) => { assert.ok(health); await reportStartupHealth(plan.userData, health.version, child.pid, health.file, health.nonce); return child; },
 }, { now: (() => { let now = 0; return () => (now += 100); })(), sleep: async () => {} });
 assert.equal(attempted, true);
 assert.deepEqual(outcome, { phase: 'complete', durable: true }); assert.equal(await readFile(plan.installed, 'utf8'), 'authenticated-new-app');
 assert.equal((await readUpdateJournal(plan)).phase, 'complete');
}, lifecyclePlatform));

test('Windows inactive staging preserves the old program and completes its journal before publishing the active slot',async()=>installedFixture(async plan=>{
 const calls:string[]=[],child={pid:process.pid+100,exitCode:null,signalCode:null},target=join(plan.health.directory,'new.exe');await writeFile(target,'inactive new app');let now=0;
 const result=await runInstalledUpdate(plan,{waitParent:async()=>{},installWindows:async()=>{calls.push('stage');assert.equal(await readFile(plan.installed,'utf8'),'working-old-app');},windowsTarget:async()=>target,
  launch:async(file,health)=>{assert.equal(file,target);assert.ok(health);calls.push('health');await reportStartupHealth(plan.userData,health.version,child.pid,health.file,health.nonce);return child;},
  commitWindows:async()=>{assert.equal((await readUpdateJournal(plan)).phase,'complete');assert.equal((await readUpdateJournal(plan)).childPid,child.pid);calls.push('slot');},activate:async()=>{calls.push('activate');},
  stop:async()=>assert.fail('healthy committed child must survive'),restoreWindowsMetadata:async()=>assert.fail('inactive install never touches registration'),
 },{now:()=>now,sleep:async()=>{now+=100;}});
 assert.deepEqual(calls,['stage','health','slot','activate']);assert.deepEqual(result,{phase:'complete',durable:true});assert.equal(await readFile(plan.installed,'utf8'),'working-old-app');
},'win32'));

test('an uncertain Windows slot commit stops the disabled child and never rewrites its complete journal or restores old over a visible new commit',async()=>installedFixture(async plan=>{
 const {WindowsCommitUncertain}=fixtureRuntime.windowsSlots;const calls:string[]=[],child={pid:process.pid+100,exitCode:null,signalCode:null};let now=0;
 const result=await runInstalledUpdate(plan,{waitParent:async()=>{},installWindows:async()=>{},launch:async(_file,health)=>{assert.ok(health,'no old relaunch after possible commit');await reportStartupHealth(plan.userData,health.version,child.pid,health.file,health.nonce);return child;},commitWindows:async()=>{throw new WindowsCommitUncertain('uncertain rename');},
  stop:async owned=>{assert.equal(owned,child);calls.push('stop-hidden');},restoreWindowsMetadata:async()=>assert.fail('no postcommit rollback'),activate:async()=>assert.fail('unconfirmed activation remains hidden'),
 },{now:()=>now,sleep:async()=>{now+=100;}});
 assert.deepEqual(result,{phase:'complete',durable:false});assert.deepEqual(calls,['stop-hidden']);assert.equal((await readUpdateJournal(plan)).phase,'complete');
},'win32'));

test('Windows slot pre-rename failure retains the old slot and reopens the working program with retry status',async()=>installedFixture(async plan=>{
 const calls:string[]=[],child={pid:process.pid+100,exitCode:null,signalCode:null};let now=0;
 const result=await runInstalledUpdate(plan,{waitParent:async()=>{},installWindows:async()=>{},launch:async(file,health)=>{if(health){await reportStartupHealth(plan.userData,health.version,child.pid,health.file,health.nonce);}else{assert.equal(file,plan.installed);assert.equal(await readFile(file,'utf8'),'working-old-app');calls.push('old');}return child;},commitWindows:async()=>{throw Object.assign(new Error('slot temp write ENOSPC'),{code:'ENOSPC'});},stop:async()=>{calls.push('stop-new');},restoreWindowsMetadata:async()=>{},activate:async()=>assert.fail('not committed'),
 },{now:()=>now,sleep:async()=>{now+=100;}});
 assert.equal(result.phase,'failed');assert.deepEqual(calls,['stop-new','old']);assert.equal((await readUpdateJournal(plan)).phase,'failed');
},'win32'));
