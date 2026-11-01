import { spawn, execFile, type ChildProcess } from 'node:child_process';
import { promisify } from 'node:util';
import { basename, dirname, join, win32, isAbsolute } from 'node:path';
import { readFile, realpath, readdir, stat, readlink } from 'node:fs/promises';
import { readFileSync, readdirSync } from 'node:fs';
import { waitForPreviousProcess } from './handoff.ts';
import { reportWatchdogReady, waitForWatchdogReady, watchdogReadyPid, clearWatchdogReady, type StartupHealth } from './update-health.ts';
import { RELEASE_PUBLIC_KEYS } from './update-keys.ts';
import { quarantineInactiveWindowsProgram } from './windows-stage.ts';
import { commitWindowsUpdate,committedWindowsUpdate } from './windows-transaction.ts';
import { updatedWindowsExecutable,verifyUpdatedWindowsProgram, readInstalledUpdate, readRecoverableInstalledUpdate, readUpdateForRestart, readCompletedUpdateForRestart, readPreparedUpdateForRestart, readRestoredInstalledUpdate, type InstalledUpdatePlan, type UpdateTrust } from './update-plan.ts';
import { createUpdateArming, sendUpdateArm } from './update-arm.ts';
import { createStartupActivation, receiveStartupActivation, type StartupActivation } from './update-activation.ts';
import { runWindowsInstaller, stopWindowsInstaller } from './update-installer.ts';
import { restoreUninstall } from './update-registry.ts';
import { readUpdateJournal, writeUpdateJournal } from './update-journal.ts';
import { activeInstalledUpdate, pendingInstalledUpdate } from './update-state.ts';
import { runInstalledUpdate, recoverInstalledUpdate, type UpdateChild, type WatchdogProcesses } from './update-watchdog.ts';
const NONCE = /^[a-f0-9]{64}$/;
const execute = promisify(execFile);
const sleep = (ms: number) => new Promise<void>(done => setTimeout(done, ms));
export function watchdogNonce(argv: readonly string[]): string | null {
 const values = argv.filter(value => value === '--update-watchdog' || value.startsWith('--update-watchdog='));
 if (!values.length) return null;
 if (values.length !== 1 || !NONCE.test(values[0].slice('--update-watchdog='.length))) throw new Error('Invalid update watchdog role');
 return values[0].slice('--update-watchdog='.length);
}
export function watchdogRecovery(argv: readonly string[]): boolean {
 const flags = argv.filter(value => value.startsWith('--update-recovery'));
 if (!flags.length) return false;
 if (flags.length !== 1 || flags[0] !== '--update-recovery' || !watchdogNonce(argv)) throw new Error('Invalid update recovery role');
 return true;
}
/** Consume before ordinary app initialization or any unrelated subprocess. */
export function takeStartupLaunch(environment: NodeJS.ProcessEnv): { nonce?: string; previousPid?: string; recoveredNonce?: string } {
 const nonce = environment.DILLA_UPDATE_STARTUP_NONCE, previousPid = environment.DILLA_UPDATE_PREVIOUS_PID, recoveredNonce = environment.DILLA_UPDATE_RECOVERED_NONCE;
 delete environment.DILLA_UPDATE_STARTUP_NONCE; delete environment.DILLA_UPDATE_PREVIOUS_PID; delete environment.DILLA_UPDATE_RECOVERED_NONCE;
 if (nonce && recoveredNonce) throw new Error('Conflicting update launch markers');
 if (recoveredNonce !== undefined && !NONCE.test(recoveredNonce)) throw new Error('Invalid restored update nonce');
 if (nonce !== undefined && !NONCE.test(nonce)) throw new Error('Invalid update startup nonce');
 return { ...(nonce ? { nonce } : {}), ...(previousPid ? { previousPid } : {}), ...(recoveredNonce ? { recoveredNonce } : {}) };
}
export function updateLaunchEnvironment(environment: NodeJS.ProcessEnv, nonce?: string): NodeJS.ProcessEnv {
 const result = { ...environment };
 delete result.DILLA_UPDATE_STARTUP_NONCE; delete result.DILLA_UPDATE_PREVIOUS_PID; delete result.DILLA_UPDATE_RECOVERED_NONCE;
 if (nonce) { if (!NONCE.test(nonce)) throw new Error('Invalid update startup nonce'); result.DILLA_UPDATE_STARTUP_NONCE = nonce; }
 return result;
}
function linuxState(pid: number): { state: string; group: number } | null {
 try {
  const stat = readFileSync(`/proc/${pid}/stat`, 'utf8'), fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
  return { state: fields[0], group: Number(fields[2]) };
 } catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT' || (error as NodeJS.ErrnoException).code === 'ESRCH') return null; throw error; }
}
function alive(pid: number, group = false): boolean {
 if (process.platform === 'linux') {
  const running = (state: ReturnType<typeof linuxState>) => state !== null && !['Z', 'X'].includes(state.state);
  if (!group) return running(linuxState(pid));
  return readdirSync('/proc').some(name => /^[0-9]+$/.test(name) && (() => { const state = linuxState(Number(name)); return state?.group === pid && running(state); })());
 }
 try { process.kill(group ? -pid : pid, 0); return true; }
 catch (error) { if ((error as NodeJS.ErrnoException).code === 'ESRCH') return false; throw error; }
}
async function spawned(file: string, args: string[], options: Parameters<typeof spawn>[2]): Promise<ChildProcess> {
 const child = spawn(file, args, options);
 await new Promise<void>((done, reject) => { child.once('error', reject); child.once('spawn', done); });
 return child;
}
async function stopOwnedChild(child: UpdateChild): Promise<void> {
 if (!child.pid || child.pid <= 1 || child.pid === process.pid) throw new Error('Invalid owned update process');
 if (process.platform === 'win32') {
  // An already exited parent has no PID tree for taskkill to identify.
  if (!alive(child.pid)) return;
  await execute(win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/taskkill.exe'), ['/PID', String(child.pid), '/T', '/F'], { windowsHide: true, maxBuffer: 64 * 1024 });
 } else {
  const group = child.groupPid ?? child.pid;
  if (!alive(group, true)) return;
  process.kill(-group, 'SIGTERM');
  const deadline = Date.now() + 2000;
  while (alive(group, true) && Date.now() < deadline) await sleep(50);
  if (alive(group, true)) process.kill(-group, 'SIGKILL');
 }
 const deadline = Date.now() + 5000;
 while (alive(process.platform === 'win32' ? child.pid : child.groupPid ?? child.pid, process.platform !== 'win32')) {
  if (Date.now() >= deadline) throw new Error('The updated app could not be stopped safely');
  await sleep(50);
 }
}
/** Electron/libuv may rewrite /proc cmdline into one process-title string.
 * Match a whole final marker token and exclude Chromium subprocess types. */
export function updateProcessMarker(argv: readonly string[], marker: string): boolean {
 const parts = argv.filter(Boolean);
 if (parts.some(value => /(?:^|\s)--type(?:=|\s)/.test(value))) return false;
 return parts.includes(marker) || (parts.length === 1 && parts[0].endsWith(' ' + marker));
}
/** /proc/exe is a kernel live-inode link, even after AppImage extraction cleanup
 * unlinks its pathname. realpath cannot resolve that still-executing inode. */
async function linuxProcessImage(pid: number): Promise<string> {
 const image = (await readlink(`/proc/${pid}/exe`)).replace(/ \(deleted\)$/, '');
 if (!isAbsolute(image)) throw new Error('Invalid update process image');
 return image;
}
async function ownedProcess(plan: InstalledUpdatePlan, pid: number, role: 'launch' | 'watchdog'): Promise<UpdateChild | null> {
 if (!alive(pid)) return null;
 // Persisted PIDs can be reused. Require the unpredictable launch marker in
 // this process's command line before treating it as an owned update child.
 let argv: string[];
 if (process.platform === 'linux') {
  if ((await stat(`/proc/${pid}`)).uid !== process.getuid!()) throw new Error('Unexpected update process owner');
  argv = (await readFile(`/proc/${pid}/cmdline`, 'utf8')).split('\0');
  if (updateProcessMarker(argv, `--update-${role}=${plan.health.nonce}`)) {
   const expected = role === 'watchdog' ? plan.watchdog : plan.installed, image = await linuxProcessImage(pid), group = linuxState(pid)?.group;
   if (image !== expected && basename(image) !== basename(process.execPath)) throw new Error('Unexpected update process image');
   if (group !== pid && (!group || await linuxProcessImage(group) !== expected)) throw new Error('Unexpected update process launcher');
  }
 }
 else {
  const script = `$p=Get-CimInstance Win32_Process -Filter 'ProcessId = ${pid}'; if($p){@{command=$p.CommandLine;image=$p.ExecutablePath}|ConvertTo-Json -Compress}`;
  const result = await execute(win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile', '-NonInteractive', '-Command', script], { encoding: 'utf8', windowsHide: true, timeout: 30_000, maxBuffer: 64 * 1024 });
  const record = JSON.parse(result.stdout || 'null') as { command?: unknown; image?: unknown } | null;
  if (!record) return null;
  const expected = role === 'watchdog' ? plan.watchdog : plan.windows ? updatedWindowsExecutable(plan) : plan.installed;
  if (typeof record.command !== 'string' || typeof record.image !== 'string' || record.image.toLowerCase() !== expected.toLowerCase()) throw new Error('Unexpected update process image');
  argv = record.command.match(/(?:[^\s"]+|"[^"]*")+/g)?.map(value => value.replace(/^"|"$/g, '')) ?? [];
 }
 if (!updateProcessMarker(argv, `--update-${role}=${plan.health.nonce}`)) { if (role === 'watchdog') return null; throw new Error('The interrupted update PID no longer belongs to this launch'); }
 return { pid, ...(process.platform === 'linux' ? { groupPid: linuxState(pid)?.group } : {}), exitCode: null, signalCode: null };
}
/** AppImage runtimes may keep an outer process while the actual Electron main
 * runs in their detached group. Discover that process independently of reports:
 * exact group of the directly spawned verified AppImage, main executable and
 * unpredictable launch marker. Electron may rewrite argv/environment memory. */
async function actualLinuxMain(launcher: ChildProcess, file: string, marker: string): Promise<UpdateChild> {
 if (process.platform !== 'linux') return launcher;
 const group = launcher.pid;
 if (!group || group <= 1) throw new Error('Invalid update launcher process');
 const deadline = Date.now() + 30_000;
 for (;;) {
  const matches: number[] = [];
  for (const entry of await readdir('/proc')) {
   if (!/^[0-9]+$/.test(entry)) continue;
   const pid = Number(entry), state = linuxState(pid);
   if (state?.group !== group || ['Z', 'X'].includes(state.state)) continue;
   try {
    if ((await stat(`/proc/${pid}`)).uid !== process.getuid!()) continue;
    const argv = (await readFile(`/proc/${pid}/cmdline`, 'utf8')).split('\0');
    if (!updateProcessMarker(argv, marker)) continue;
    if (basename(await linuxProcessImage(pid)) !== basename(process.execPath)) continue;
    // The group is the owned launcher's detached group. If the AppImage
    // runtime keeps a wrapper, independently bind it to that exact file.
    if (pid !== group && await linuxProcessImage(group) !== await realpath(file)) throw new Error('Unexpected AppImage launcher');
    matches.push(pid);
   } catch (error) { if (!['ENOENT', 'ESRCH'].includes((error as NodeJS.ErrnoException).code ?? '')) throw error; }
  }
  if (matches.length > 1) throw new Error('Ambiguous update application process');
  if (matches.length === 1) {
   const pid = matches[0];
   return { pid, groupPid: group, get exitCode() { return launcher.exitCode ?? (alive(pid) ? null : 1); }, get signalCode() { return launcher.signalCode; } };
  }
  if (launcher.exitCode !== null || launcher.signalCode !== null || !alive(group, true)) throw new Error('The update application exited before startup');
  if (Date.now() >= deadline) throw new Error('The update application process did not start');
  await sleep(50);
 }
}
async function discoverOwnedLaunches(plan: InstalledUpdatePlan): Promise<UpdateChild[]> {
 const marker = `--update-launch=${plan.health.nonce}`, children: UpdateChild[] = [];
 if (process.platform === 'linux') {
  for (const entry of await readdir('/proc')) {
   if (!/^[0-9]+$/.test(entry)) continue;
   const pid = Number(entry), state = linuxState(pid);
   if (!state || ['Z', 'X'].includes(state.state)) continue;
   try {
    if ((await stat(`/proc/${pid}`)).uid !== process.getuid!()) continue;
    // Same-user system daemons can prohibit /exe access. Only inspect image
    // identity after the exact unpredictable marker identifies a candidate.
    if (!updateProcessMarker((await readFile(`/proc/${pid}/cmdline`, 'utf8')).split('\0'), marker)) continue;
    if (basename(await linuxProcessImage(pid)) !== basename(process.execPath)) continue;
    if (pid !== state.group && await linuxProcessImage(state.group) !== await realpath(plan.installed)) throw new Error('Unexpected interrupted AppImage launcher');
    children.push({ pid, groupPid: state.group, exitCode: null, signalCode: null });
   } catch (error) { if (!['ENOENT', 'ESRCH'].includes((error as NodeJS.ErrnoException).code ?? '')) throw error; }
  }
 } else {
  const decode = (value: string) => `[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('${Buffer.from(value).toString('base64')}'))`;
  const script = `$image=${decode(plan.windows ? updatedWindowsExecutable(plan) : plan.installed)};$marker=${decode(marker)};ConvertTo-Json -Compress -InputObject @(Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -ieq $image -and $_.CommandLine.Contains($marker)} | ForEach-Object { @{pid=$_.ProcessId;command=$_.CommandLine} })`;
  const output = await execute(win32.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32/WindowsPowerShell/v1.0/powershell.exe'), ['-NoProfile', '-NonInteractive', '-Command', script], { encoding: 'utf8', windowsHide: true, maxBuffer: 1024 * 1024 });
  const rows: unknown = JSON.parse(output.stdout || '[]');
  if (!Array.isArray(rows)) throw new Error('Invalid owned update process discovery');
  for (const row of rows) {
   if (!row || !Number.isSafeInteger(row.pid) || row.pid <= 1 || typeof row.command !== 'string') throw new Error('Invalid owned update process discovery');
   const argv = row.command.match(/(?:[^\s"]+|"[^"]*")+/g)?.map((value: string) => value.replace(/^"|"$/g, '')) ?? [];
   if (updateProcessMarker(argv, marker)) children.push({ pid: row.pid, exitCode: null, signalCode: null });
  }
 }
 return children;
}
export function nativeWatchdogProcesses(plan: InstalledUpdatePlan,publicKeys:readonly string[]=RELEASE_PUBLIC_KEYS): WatchdogProcesses {
 let activation: StartupActivation | undefined;
 return {
  waitParent: pid => waitForPreviousProcess(String(pid)),
  launch: async (file, health?: StartupHealth) => {
   const marker = health ? `--update-launch=${health.nonce}` : null;
   if (health) activation = await createStartupActivation(plan);
   let child: ChildProcess | undefined;
   try {
    child = await spawned(file, marker ? [marker] : [], { detached: true, stdio: 'ignore', cwd: dirname(file), env: { ...updateLaunchEnvironment(process.env, health?.nonce), ...(!health ? plan.windows && file!==plan.installed ? {DILLA_UPDATE_PREVIOUS_PID:String(process.pid)} : { DILLA_UPDATE_RECOVERED_NONCE: plan.health.nonce,DILLA_UPDATE_PREVIOUS_PID:String(process.pid) } : {}), ...(plan.platform === 'linux' ? { APPIMAGE_SILENT_INSTALL: 'true', APPIMAGE: file } : {}) } });
    const application = marker ? await actualLinuxMain(child, file, marker) : child;
    if (health && application.pid) activation!.expect(application.pid);
    child.unref(); return application;
   } catch (error) { await activation?.close(); activation = undefined; if (child) await stopOwnedChild(child); throw error; }
  },
  stop: async child => { await activation?.close(); activation = undefined; await stopOwnedChild(child); },
  activate: async child => {
   if (!activation || !child.pid) throw new Error('Missing startup activation');
   try { await activation.activate(child.pid); } finally { await activation.close(); activation = undefined; }
  },
  recoverChild: pid => ownedProcess(plan, pid, 'launch'),
  recoverLaunches: () => discoverOwnedLaunches(plan),
  restoreWindowsMetadata: async () => {
   if (!plan.uninstall) throw new Error('Missing owned uninstall backup');
   if(!plan.windows)await restoreUninstall(dirname(plan.installed), plan.uninstall);
  },
  ...(plan.windows ? {windowsTarget:()=>verifyUpdatedWindowsProgram(plan),commitWindows:()=>commitWindowsUpdate(plan,publicKeys)} : {}),
  stopInstaller: journal => stopWindowsInstaller(plan, journal),
  installWindows: async file => {
   if (file !== plan.artifactFile) throw new Error('Unexpected authenticated installer path');
   if(plan.windows){const digest=plan.release.artifacts.find(file=>file.platform==='win32'&&file.arch===plan.arch)?.programManifest?.sha512;if(!digest)throw new Error('Missing inactive Windows program digest');await quarantineInactiveWindowsProgram(plan.windows.root,plan.windows.oldSlot,digest,plan.health.nonce);}
   await runWindowsInstaller(plan);
  },
 };
}
export interface PreparedWatchdog { arm(): Promise<void>; cancel(): Promise<void> }
/** Caller retains the single-instance lock. It publishes the active pointer only
 * after this ready acknowledgment, then authenticates arm before actual exit. */
export async function launchUpdateWatchdog(plan: InstalledUpdatePlan, recovery = false): Promise<PreparedWatchdog> {
 await clearWatchdogReady(plan.health);
 const marker = `--update-watchdog=${plan.health.nonce}`;
 const helper = await spawned(plan.watchdog, [...(recovery ? ['--update-recovery'] : []), marker], { detached: true, stdio: 'ignore', cwd: plan.health.directory, env: { ...updateLaunchEnvironment(process.env), DILLA_UPDATE_PREVIOUS_PID: String(process.pid), ...(plan.platform === 'linux' ? { APPIMAGE: plan.watchdog } : {}) } });
 let application: UpdateChild = helper;
 try { application = await actualLinuxMain(helper, plan.watchdog, marker); await waitForWatchdogReady(plan.health, plan.oldVersion, application); helper.unref(); }
 catch (error) { await stopOwnedChild(application); throw error; }
 return { arm: async () => { if (recovery || !application.pid) throw new Error('Recovery cannot authorize installation'); await sendUpdateArm(plan, application.pid); }, cancel: () => stopOwnedChild(application) };
}
/** Restricted helper starts before ordinary locking, windows, tray or account
 * boot. Recovery is a separate safe role and can never arm an installation. */
export async function runUpdateWatchdog(userData: string, nonce: string, trust: UpdateTrust & { executable: string }, launcherPid?: string, recovery = false): Promise<void> {
 const plan = recovery ? await readRecoverableInstalledUpdate(userData, nonce, trust, true) : await readInstalledUpdate(userData, nonce, trust);
 if (plan.platform === 'win32' && !plan.uninstall) throw new Error('Missing owned uninstall backup');
 const processes = nativeWatchdogProcesses(plan,trust.publicKeys);
 if (recovery) {
  const active = await activeInstalledUpdate(plan.userData); if (active?.nonce !== nonce) throw new Error('Recovery belongs to another active transaction');
  await reportWatchdogReady(plan.userData, plan.oldVersion, process.pid, nonce); await waitForPreviousProcess(launcherPid);
  await recoverInstalledUpdate(plan, processes); return;
 }
 if (launcherPid !== String(plan.parentPid) || !alive(plan.parentPid) || (await readUpdateJournal(plan)).phase !== 'prepared') throw new Error('Invalid live update parent or preparation');
 const arming = await createUpdateArming(plan, process.pid, async () => {
  const active = await activeInstalledUpdate(plan.userData), journal = await readUpdateJournal(plan);
  if (active?.nonce !== nonce || active.phase !== 'prepared' || journal.watchdogPid !== process.pid) throw new Error('Update publication was not accepted');
 }, () => alive(plan.parentPid));
 try {
  await writeUpdateJournal(plan, { phase: 'prepared', watchdogPid: process.pid });
  await reportWatchdogReady(plan.userData, plan.oldVersion, process.pid, nonce);
  if (!await arming.wait()) return;
 } finally { await arming.close(); }
 await waitForPreviousProcess(String(plan.parentPid));
 const active = await activeInstalledUpdate(plan.userData), journal = await readUpdateJournal(plan);
 if (active?.nonce !== nonce || active.phase !== 'prepared' || journal.watchdogPid !== process.pid) throw new Error('Armed update authority changed');
 await runInstalledUpdate(plan, processes);
}

/** Resume only a committed interrupted transaction for this exact program. A
 * healthy update child bypasses discovery via its already-consumed launch nonce. */
export function bootstrapNonce(argv:readonly string[]):string|null{
 const roles=argv.filter(value=>value==='--update-bootstrap'||value.startsWith('--update-bootstrap='));
 if(!roles.length)return null;
 if(roles.length!==1||!/^--update-bootstrap=[a-f0-9]{64}$/.test(roles[0])||watchdogNonce(argv))throw new Error('Invalid Windows bootstrap role');return roles[0].slice('--update-bootstrap='.length);
}
/** The immutable launcher invokes only the verified old program. Read-only
 * terminal observation may forward into an already-running app; every recovery
 * spawn/status mutation requires the ordinary single-instance lock first. */
export async function runWindowsBootstrap(userData:string,installed:string,nonce:string,trust:UpdateTrust,acquireLock:()=>boolean,processFactory:(plan:InstalledUpdatePlan)=>WatchdogProcesses=plan=>nativeWatchdogProcesses(plan,trust.publicKeys)):Promise<20|21|22>{
 try{
  const active=await activeInstalledUpdate(userData);if(!active||active.nonce!==nonce)throw new Error('Windows bootstrap active transaction changed');
  if(active.phase==='complete'||active.phase==='failed'){
   const plan=active.phase==='complete'?await readCompletedUpdateForRestart(userData,nonce,{...trust,installed}):await readRestoredInstalledUpdate(userData,nonce,{...trust,installed});
   if(!plan.windows)throw new Error('Missing Windows stable program plan');
   const ready=await watchdogReadyPid(plan.health,plan.oldVersion);if(ready&&await ownedProcess(plan,ready,'watchdog'))return 20;
   const committed=await committedWindowsUpdate(plan,trust.publicKeys);
   if(active.phase==='complete'&&committed)return 22;
   if(active.phase==='failed'){if(committed)throw new Error('Failed Windows update changed active program');return 22;}
   // Complete bytes without a published slot never enabled user/account entry.
   // This is the pre-publication failure window, so retain the old program.
   if(!acquireLock())return 20;
   const processes=processFactory(plan),journal=await readUpdateJournal(plan);
   await processes.stopInstaller?.(journal);
   const launched=await processes.recoverLaunches?.()??[];
   for(const child of launched)await processes.stop(child);
   if(journal.childPid&&!launched.some(child=>child.pid===journal.childPid)){
    if(!processes.recoverChild)throw new Error('Cannot verify the registered pending Windows child');
    const child=await processes.recoverChild(journal.childPid);if(child)await processes.stop(child);
   }
   try{await writeUpdateJournal(plan,{phase:'failed',failure:'interrupted'});return 22;}
   catch{
    // A full volume cannot strand the verified working program. Keep Go's22
    // terminal contract strict: delegate an independently verified recovered
    // old launch instead, which waits for this SIL-owning bootstrap to exit.
    await readRestoredInstalledUpdate(userData,nonce,{...trust,installed});
    if(await committedWindowsUpdate(plan,trust.publicKeys))throw new Error('Windows activation changed during recovery');
    await processes.launch(plan.installed);return 21;
   }
  }
  if(!acquireLock())return 20;
  await resumeInterruptedUpdate(userData,installed,trust);return 21;
 }catch(error){if(error instanceof UpdateStartupHeld)throw error;throw new UpdateStartupHeld('The Windows update bootstrap could not verify a safe committed program');}
}
export class UpdateStartupHeld extends Error { override name = 'UpdateStartupHeld'; }
export async function resumeInterruptedUpdate(userData: string, installed: string, trust: UpdateTrust): Promise<boolean> {
 try { return await observeAndResumeUpdate(userData, installed, trust); }
 catch (error) { if (error instanceof UpdateStartupHeld) throw error; throw new UpdateStartupHeld('The update state could not be verified. Restart Dilla after recovery finishes.'); }
}
async function observeAndResumeUpdate(userData: string, installed: string, trust: UpdateTrust): Promise<boolean> {
 const pending = await activeInstalledUpdate(userData);
 if (pending?.phase === 'prepared') {
  const plan = await readPreparedUpdateForRestart(userData, pending.nonce, { ...trust, installed: await realpath(installed) });
  const journal = await readUpdateJournal(plan), readyPid = await watchdogReadyPid(plan.health, plan.oldVersion);
  // A recovery helper acknowledges readiness before its launcher releases the
  // single-instance lock. Its PID may supersede the initial prepared owner; a
  // stale durable PID must never hide that exact live replacement.
  for (const pid of new Set([journal.watchdogPid, readyPid])) {
   if (pid && await ownedProcess(plan, pid, 'watchdog')) return true;
  }
  // An abandoned preparation is a failed update, never permission to install.
  await launchUpdateWatchdog(plan, true); return true;
 }
 if (pending?.phase === 'complete') {
  const plan = await readCompletedUpdateForRestart(userData, pending.nonce, { ...trust, installed: await realpath(installed) });
  const pid = await watchdogReadyPid(plan.health, plan.oldVersion);
  // A visible complete rename may precede fsync. Never expose another main
  // while its owner can still restore bytes after a failed durable write.
  return !!(pid && await ownedProcess(plan, pid, 'watchdog'));
 }
 if (!pending || !['installing', 'launching', 'recovering'].includes(pending.phase)) return false;
 const plan = await readUpdateForRestart(userData, pending.nonce, { ...trust, installed: await realpath(installed) });
 const pid = await watchdogReadyPid(plan.health, plan.oldVersion);
 if (pid && await ownedProcess(plan, pid, 'watchdog')) return true;
 await launchUpdateWatchdog(plan, true); return true;
}

export async function verifyRestoredLaunch(userData: string, installed: string, nonce: string, trust: UpdateTrust): Promise<InstalledUpdatePlan> {
 return readRestoredInstalledUpdate(userData, nonce, { ...trust, installed: await realpath(installed) });
}
/** Called only after the recovered main owns the ordinary lock, before UI or
 * accounts. The launch marker alone grants no recovery or path authority. */
export async function verifyRestoredWindowsStartup(plan:InstalledUpdatePlan,trust:UpdateTrust):Promise<void>{
 await readRestoredInstalledUpdate(plan.userData,plan.health.nonce,{...trust,installed:plan.installed});
 const ready=await watchdogReadyPid(plan.health,plan.oldVersion);if(ready&&await ownedProcess(plan,ready,'watchdog'))throw new UpdateStartupHeld('Windows recovery is still active');
 const processes=nativeWatchdogProcesses(plan,trust.publicKeys);await processes.stopInstaller?.(await readUpdateJournal(plan));
 if((await processes.recoverLaunches?.()??[]).length)throw new UpdateStartupHeld('A pending Windows child has not stopped');
}


interface RegistrationClock { now(): number; sleep(): Promise<void>; timeoutMs?: number }
/** The new main remains before renderers, single-instance and account/store boot
 * until the watchdog has durably registered this exact launch PID. */
export async function waitForRegisteredStartup(userData: string, installed: string, nonce: string, pid: number, trust: UpdateTrust,
 clock: RegistrationClock = { now: Date.now, sleep: () => sleep(50) }): Promise<InstalledUpdatePlan> {
 if (!Number.isSafeInteger(pid) || pid <= 1) throw new Error('Invalid registered update process');
 const pending = await pendingInstalledUpdate(userData);
 if (!pending || pending.nonce !== nonce) throw new Error('Invalid registered update transaction');
 const plan = await readUpdateForRestart(userData, nonce, { ...trust, installed: await realpath(installed) });
 if (plan.health.version !== trust.version) throw new Error('Invalid registered update version');
 const deadline = clock.now() + (clock.timeoutMs ?? 30_000);
 for (;;) {
  const journal = await readUpdateJournal(plan);
  if (journal.phase !== 'launching' || (journal.childPid !== undefined && journal.childPid !== pid)) throw new Error('The update process was not registered for startup');
  if (journal.childPid === pid) return plan;
  if (clock.now() >= deadline) throw new Error('The update process was not registered before startup timed out');
  await clock.sleep();
 }
}

/** Only an exact durable activation releases user entry into the new app. */
export async function waitForStartupActivation(plan: InstalledUpdatePlan, pid: number, timeoutMs = 90_000): Promise<void> {
 const journal = await readUpdateJournal(plan);
 if (journal.childPid !== pid || !['launching', 'complete'].includes(journal.phase)) throw new Error('Installed update activation was cancelled');
 await receiveStartupActivation(plan, pid, timeoutMs);
}
